const STATUSES = ["triage", "todo", "scheduled", "ready", "running", "blocked", "review", "done", "archived"];
const STATUS_LABELS = {
  triage: "Triage", todo: "To do", scheduled: "Scheduled", ready: "Ready",
  running: "Running", blocked: "Blocked", review: "Review", done: "Done", archived: "Archived",
};
const COLORS = {
  triage: "#a98cff", todo: "#8791a3", scheduled: "#e7b65b", ready: "#5e91ff",
  running: "#36c9b0", blocked: "#ff6978", review: "#de84ff", done: "#55d38b", archived: "#667085",
};
const WORKFLOW_STAGES = [
  { id: "planning", label: "Planning", ariaLabel: "Planning workflow stage", statuses: ["triage", "todo", "scheduled", "ready"] },
  { id: "execution", label: "Execution", ariaLabel: "Execution workflow stage", statuses: ["running", "blocked", "review", "done"] },
  { id: "archive", label: "Archive", ariaLabel: "Archive workflow stage", statuses: ["archived"] },
];

const storedTheme = localStorage.getItem("autogora.theme");
let activeTheme = ["light", "dark"].includes(storedTheme)
  ? storedTheme
  : (window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light");
document.documentElement.dataset.theme = activeTheme;

const state = {
  boards: [], board: localStorage.getItem("autogora.board") || "default", metadata: null,
  profiles: [], tasks: [], stats: null, diagnostics: null, selected: new Set(), drawerTask: null, cursor: 0, socket: null,
  agentConfig: null, agentConfigExists: false, detections: [], effectiveAgents: [], supervisor: null,
};

const $ = (selector, root = document) => root.querySelector(selector);
const $$ = (selector, root = document) => [...root.querySelectorAll(selector)];
const escapeHtml = (value = "") => String(value).replace(/[&<>'"]/g, (char) => ({
  "&": "&amp;", "<": "&lt;", ">": "&gt;", "'": "&#39;", '"': "&quot;",
})[char]);

function initials(value) {
  const words = String(value || "?").trim().split(/[\s._-]+/).filter(Boolean);
  return words.slice(0, 2).map((word) => word[0]).join("").toUpperCase() || "?";
}

function setTheme(theme, persist = true) {
  activeTheme = theme;
  document.documentElement.dataset.theme = theme;
  if (persist) localStorage.setItem("autogora.theme", theme);
  const target = theme === "dark" ? "light" : "dark";
  const button = $("#theme-toggle");
  if (button) {
    $(".theme-icon", button).textContent = target === "light" ? "☀" : "☾";
    $(".theme-label", button).textContent = target === "light" ? "Light" : "Dark";
    button.setAttribute("aria-label", `Switch to ${target} theme`);
    button.title = `Switch to ${target} theme`;
  }
  document.querySelector('meta[name="theme-color"]')?.setAttribute(
    "content",
    theme === "dark" ? "#0b0e14" : "#f4f6fa",
  );
}

function markdown(value = "") {
  let safe = escapeHtml(value);
  safe = safe.replace(/```([\s\S]*?)```/g, (_, code) => `<pre>${code.trim()}</pre>`);
  safe = safe.replace(/`([^`]+)`/g, "<code>$1</code>");
  safe = safe.replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>");
  safe = safe.replace(/(^|\s)(https?:\/\/[^\s<]+|mailto:[^\s<]+)/g, (_, prefix, url) =>
    `${prefix}<a href="${url}" target="_blank" rel="noopener noreferrer">${url}</a>`);
  safe = safe.split("\n").map((line) => {
    if (line.startsWith("### ")) return `<h4>${line.slice(4)}</h4>`;
    if (line.startsWith("## ")) return `<h3>${line.slice(3)}</h3>`;
    if (line.startsWith("# ")) return `<h2>${line.slice(2)}</h2>`;
    if (line.startsWith("- ")) return `<div>• ${line.slice(2)}</div>`;
    return line || "<br>";
  }).join("\n");
  return safe;
}

function relativeTime(value) {
  const seconds = Math.max(0, Math.floor((Date.now() - Date.parse(value)) / 1000));
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.floor(seconds / 60); if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60); if (hours < 24) return `${hours}h ago`;
  return `${Math.floor(hours / 24)}d ago`;
}

function boardPath(path) {
  const separator = path.includes("?") ? "&" : "?";
  return `${path}${separator}board=${encodeURIComponent(state.board)}`;
}

async function api(path, options = {}) {
  const response = await fetch(path, {
    credentials: "same-origin",
    ...options,
    headers: options.body && !(options.body instanceof Blob)
      ? { "content-type": "application/json", ...(options.headers || {}) }
      : options.headers,
  });
  const text = await response.text();
  const value = text ? JSON.parse(text) : null;
  if (!response.ok) throw new Error(value?.error || `HTTP ${response.status}`);
  return value;
}

let toastTimer;
let drawerReturnFocus = null;
function toast(message, error = false) {
  const element = $("#toast");
  element.textContent = message;
  element.classList.toggle("error", error);
  element.classList.remove("hidden");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => element.classList.add("hidden"), 3500);
}

async function loadBoards() {
  const payload = await api("/api/boards");
  state.boards = payload.boards;
  if (!state.boards.some((board) => board.slug === state.board)) state.board = payload.current || "default";
  const select = $("#board-select");
  select.innerHTML = state.boards.map((board) =>
    `<option value="${escapeHtml(board.slug)}">${escapeHtml(board.icon ? `${board.icon} ` : "")}${escapeHtml(board.name)}</option>`,
  ).join("");
  select.value = state.board;
}

async function loadBoard() {
  const includeArchived = $("#show-archived").checked;
  const payload = await api(boardPath(`/api/board?includeArchived=${includeArchived}`));
  state.metadata = payload.board;
  state.profiles = payload.profiles || payload.board?.orchestration?.profiles || [];
  state.tasks = payload.tasks;
  state.stats = payload.stats;
  state.diagnostics = payload.diagnostics;
  renderFilters();
  renderBoard();
}

function workerProfiles() {
  return (state.profiles || []).filter((profile) => !profile.disabled);
}

function profileByName(name) {
  return workerProfiles().find((profile) => profile.name === name);
}

function filteredTasks() {
  const search = $("#search").value.trim().toLowerCase();
  const tenant = $("#tenant-filter").value;
  const assignee = $("#assignee-filter").value;
  return state.tasks.filter((task) =>
    (!search || `${task.title}\n${task.body}`.toLowerCase().includes(search)) &&
    (!tenant || task.tenant === tenant) && (!assignee || task.assignee === assignee));
}

function renderFilters() {
  const tenant = $("#tenant-filter").value;
  const assignee = $("#assignee-filter").value;
  const tenants = [...new Set(state.tasks.map((task) => task.tenant).filter(Boolean))].sort();
  const assignees = [...new Set(state.tasks.map((task) => task.assignee).filter(Boolean))].sort();
  $("#tenant-filter").innerHTML = `<option value="">All tenants</option>${tenants.map((item) => `<option>${escapeHtml(item)}</option>`).join("")}`;
  $("#assignee-filter").innerHTML = `<option value="">All assignees</option>${assignees.map((item) => `<option>${escapeHtml(item)}</option>`).join("")}`;
  $("#tenant-filter").value = tenants.includes(tenant) ? tenant : "";
  $("#assignee-filter").value = assignees.includes(assignee) ? assignee : "";
  const diagnosticIssues = state.diagnostics?.issues || [];
  const healthy = diagnosticIssues.length === 0;
  $("#stats").innerHTML = `
    <span class="metric"><strong>${state.stats?.total || 0}</strong><span>tasks</span></span>
    <span class="metric"><strong>${state.stats?.byStatus?.running || 0}</strong><span>running</span></span>
    <button type="button" id="health-details" class="health-chip ${healthy ? "healthy" : "attention"}"><span aria-hidden="true"></span>${healthy ? "Healthy" : `Needs attention (${diagnosticIssues.length})`}</button>`;
  $("#health-details").addEventListener("click", () => {
    if (healthy) return;
    alert(diagnosticIssues.slice(0, 20).map((issue) => `${issue.kind} · ${issue.taskId}\n${issue.detail}`).join("\n\n")
      + (diagnosticIssues.length > 20 ? `\n\n… ${diagnosticIssues.length - 20} more issue(s)` : ""));
  });
}

function cardHtml(task) {
  const owner = task.assignee || "Unassigned";
  const progress = task.status !== "done" && task.status !== "archived" && task.subtasksTotal > 0
    ? `<span class="pill" title="Completed subtasks">${task.subtasksDone}/${task.subtasksTotal}</span>` : "";
  const summary = task.body?.trim()
    ? `<div class="card-summary">${escapeHtml(task.body.trim())}</div>`
    : "";
  return `<article class="card ${state.selected.has(task.id) ? "selected" : ""}" draggable="true" tabindex="0" data-task="${escapeHtml(task.id)}"
    style="--status-color:${COLORS[task.status]}" aria-label="${escapeHtml(`${task.title}, ${STATUS_LABELS[task.status]}, ${owner}, ${task.runtime}`)}">
    <div class="card-top"><input type="checkbox" aria-label="Select ${escapeHtml(task.title)}" ${state.selected.has(task.id) ? "checked" : ""}>
      <span class="status-badge"><span class="status-dot"></span>${STATUS_LABELS[task.status]}</span>
      <span class="mono card-id">${escapeHtml(task.id)}</span>${progress}</div>
    <div class="card-title">${escapeHtml(task.title)}</div>
    ${summary}
    <div class="card-owner ${task.assignee ? "" : "unassigned"}">
      <span class="avatar" aria-hidden="true">${escapeHtml(initials(task.assignee))}</span>
      <span class="owner-copy"><small>Owner</small><strong>${escapeHtml(owner)}</strong></span>
      <span class="runtime-chip" title="Worker runtime">${escapeHtml(task.runtime)}</span>
    </div>
    <div class="card-foot">
      ${task.priority ? `<span class="pill priority">P${task.priority}</span>` : ""}
      ${task.tenant ? `<span class="pill">${escapeHtml(task.tenant)}</span>` : ""}
      ${task.commentsCount ? `<span title="Comments">💬 ${task.commentsCount}</span>` : ""}${task.relationshipsCount ? `<span title="Relationships">↔ ${task.relationshipsCount}</span>` : ""}
      <span class="updated">Updated ${relativeTime(task.updatedAt)}</span>
    </div>
  </article>`;
}

function renderCardList(tasks, lanes) {
  if (!lanes) return `<div class="card-list">${tasks.map(cardHtml).join("")}</div>`;
  const groups = new Map();
  for (const task of tasks) {
    const key = task.assignee || "unassigned";
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key).push(task);
  }
  return [...groups].map(([assignee, items]) =>
    `<div class="lane"><div class="lane-title">${escapeHtml(assignee)}</div><div class="card-list">${items.map(cardHtml).join("")}</div></div>`,
  ).join("");
}

function renderBoard() {
  const tasks = filteredTasks();
  const stages = WORKFLOW_STAGES.filter((stage) => stage.id !== "archive" || $("#show-archived").checked);
  $("#board").innerHTML = stages.map((stage) => {
    const stageCount = stage.statuses.reduce((total, status) =>
      total + tasks.filter((task) => task.status === status).length, 0);
    const columns = stage.statuses.map((status) => {
      const cards = tasks.filter((task) => task.status === status);
      return `<section class="column" data-status="${status}" style="--status-color:${COLORS[status]}">
        <header class="column-head"><span class="status-dot"></span><h3>${STATUS_LABELS[status]}</h3><span class="count">${cards.length}</span>${status === "running" ? "" : `<button class="icon-button compact" data-create-status="${status}" aria-label="Create in ${STATUS_LABELS[status]}" title="Create in ${STATUS_LABELS[status]}">+</button>`}</header>
        <div class="column-body" role="region" aria-label="${STATUS_LABELS[status]} tasks" tabindex="0">${renderCardList(cards, status === "running" && $("#lane-profile").checked)}</div>
      </section>`;
    }).join("");
    return `<section class="board-stage" data-stage="${stage.id}" aria-label="${stage.ariaLabel}">
      <header class="board-stage-head"><h2>${stage.label}</h2><span>${stageCount} ${stageCount === 1 ? "task" : "tasks"}</span></header>
      <div class="board-stage-grid">${columns}</div>
    </section>`;
  }).join("");
  bindCards();
  renderBulk();
}

function bindCards() {
  $$(".card").forEach((card) => {
    const taskId = card.dataset.task;
    card.addEventListener("click", (event) => {
      if (event.target.matches("input")) {
        event.stopPropagation();
        if (event.target.checked) state.selected.add(taskId); else state.selected.delete(taskId);
        renderBoard();
      } else openDrawer(taskId);
    });
    card.addEventListener("keydown", (event) => {
      if (event.target.matches("input") || (event.key !== "Enter" && event.key !== " ")) return;
      event.preventDefault();
      openDrawer(taskId);
    });
    card.addEventListener("dragstart", (event) => {
      event.dataTransfer.setData("text/plain", taskId);
      card.classList.add("dragging");
      document.body.classList.add("drag-active");
    });
    card.addEventListener("dragend", () => { card.classList.remove("dragging"); document.body.classList.remove("drag-active"); });
  });
  $$(".column").forEach((column) => {
    column.addEventListener("dragover", (event) => { event.preventDefault(); column.classList.add("drag-over"); });
    column.addEventListener("dragleave", () => column.classList.remove("drag-over"));
    column.addEventListener("drop", async (event) => {
      event.preventDefault(); column.classList.remove("drag-over");
      const taskId = event.dataTransfer.getData("text/plain");
      if (taskId) await moveTask(taskId, column.dataset.status);
    });
  });
  $$('[data-create-status]').forEach((button) => button.addEventListener("click", () => openTaskDialog(button.dataset.createStatus)));
  const trash = $("#trash-drop");
  trash.ondragover = (event) => { event.preventDefault(); trash.classList.add("drag-over"); };
  trash.ondragleave = () => trash.classList.remove("drag-over");
  trash.ondrop = async (event) => {
    event.preventDefault(); trash.classList.remove("drag-over"); document.body.classList.remove("drag-active");
    const taskId = event.dataTransfer.getData("text/plain");
    if (!taskId || !confirm(`Permanently delete ${taskId}?`)) return;
    try { await api(boardPath(`/api/tasks/${taskId}`), { method: "DELETE" }); await loadBoard(); }
    catch (error) { toast(error.message, true); }
  };
}

async function moveTask(taskId, status) {
  try {
    if (status === "running") {
      await api(boardPath("/api/dispatch"), { method: "POST", body: JSON.stringify({ taskId }) });
      toast("Task sent to the dispatcher");
      await loadBoard();
      return;
    }
    const body = { status };
    if (status === "done") body.summary = prompt("Completion summary:", "Completed from dashboard") || "Completed from dashboard";
    if (status === "blocked") body.reason = prompt("Block reason:");
    if (status === "blocked" && !body.reason) return;
    if (status === "archived" && !confirm("Archive this task?")) return;
    await api(boardPath(`/api/tasks/${taskId}`), { method: "PATCH", body: JSON.stringify(body) });
    await loadBoard();
  } catch (error) { toast(error.message, true); }
}

function renderBulk() {
  const bar = $("#bulk-bar");
  bar.classList.toggle("hidden", state.selected.size === 0);
  $("#bulk-count").textContent = state.selected.size;
}

async function bulkMutation(mutation) {
  if (!state.selected.size) return;
  try {
    const result = await api(boardPath("/api/tasks/bulk"), {
      method: "POST", body: JSON.stringify({ ids: [...state.selected], mutation }),
    });
    state.selected.clear();
    toast(`${result.ok.length} updated${result.errors.length ? `, ${result.errors.length} failed` : ""}`, result.errors.length > 0);
    await loadBoard();
  } catch (error) { toast(error.message, true); }
}

function openTaskDialog(status = "todo") {
  const form = $("#task-form");
  form.reset();
  form.elements.status.value = status;
  const profiles = workerProfiles();
  form.elements.profile.innerHTML = `<option value="">Custom assignment</option>${profiles.map((profile) =>
    `<option value="${escapeHtml(profile.name)}">${escapeHtml(profile.name)} · ${escapeHtml(profile.runtime)} · ${escapeHtml(profile.model || "CLI default")}</option>`).join("")}`;
  updateTaskModelPreview();
  $("#task-dialog").showModal();
}

function updateTaskModelPreview() {
  const form = $("#task-form");
  const profile = profileByName(form.elements.profile.value);
  if (profile) {
    form.elements.assignee.value = profile.name;
    form.elements.runtime.value = profile.runtime;
    form.elements.modelPreview.value = profile.model || "CLI default (unpinned)";
    return;
  }
  form.elements.modelPreview.value = form.elements.runtime.value === "manual" ? "Manual task" : "CLI default (unpinned)";
}

async function openDrawer(taskId) {
  try {
    if (!state.drawerTask) drawerReturnFocus = document.activeElement;
    const detail = await api(boardPath(`/api/tasks/${taskId}`));
    state.drawerTask = taskId;
    $("#drawer-id").textContent = taskId;
    $("#drawer-status").textContent = STATUS_LABELS[detail.task.status];
    $("#drawer-status").style.setProperty("--status-color", COLORS[detail.task.status]);
    $("#drawer").classList.add("open");
    $("#drawer").setAttribute("aria-hidden", "false");
    $("#scrim").classList.remove("hidden");
    renderDrawer(detail);
    $("#drawer-close").focus({ preventScroll: true });
  } catch (error) { toast(error.message, true); }
}

function closeDrawer() {
  state.drawerTask = null;
  $("#drawer").classList.remove("open");
  $("#drawer").setAttribute("aria-hidden", "true");
  $("#scrim").classList.add("hidden");
  if (drawerReturnFocus?.isConnected) drawerReturnFocus.focus({ preventScroll: true });
  drawerReturnFocus = null;
}

function taskOptions(excludeId) {
  return state.tasks.filter((task) => task.id !== excludeId && task.status !== "archived")
    .map((task) => `<option value="${escapeHtml(task.id)}">${escapeHtml(task.id)} · ${escapeHtml(task.title)}</option>`).join("");
}

function renderDrawer(detail) {
  const task = detail.task;
  const runAgents = new Map((detail.runAgentConfigs || []).map((config) => [config.runId, config]));
  const runRows = detail.runs.slice().reverse().map((run) => {
    const config = runAgents.get(run.id);
    const route = config ? `${config.profile} · ${config.runtime} · ${config.model || "CLI default (unpinned)"}${config.provider ? ` · ${config.provider}` : ""}` : "";
    const provenance = config ? `${String(config.source || "unknown").replaceAll("_", " ")}${config.fallbackFrom ? ` · fallback from ${config.fallbackFrom}` : ""}` : "";
    return `<div class="detail-row">
      ${run.status === "running" ? `<button data-terminate-run="${escapeHtml(run.id)}" class="danger compact">Terminate</button>` : ""}
      <strong>${escapeHtml(run.workerId)}</strong>
      <span class="detail-status">${escapeHtml(run.status)}</span>
      <span class="mono">${escapeHtml(run.id)} · ${relativeTime(run.claimedAt)}</span>
      ${config ? `<div>${escapeHtml(route)}</div><div class="mono">${escapeHtml(provenance)}</div>` : ""}
      ${run.summary ? `<div>${escapeHtml(run.summary)}</div>` : ""}${run.error ? `<div>${escapeHtml(run.error)}</div>` : ""}
    </div>`;
  }).join("");
  const comments = detail.comments.map((comment) => `<div class="detail-row"><strong>${escapeHtml(comment.author)}</strong>${markdown(comment.body)}<div class="mono">${escapeHtml(comment.createdAt)}</div></div>`).join("");
  const attachments = detail.attachments.map((attachment) => `<div class="detail-row">
    <button class="icon-button compact" data-remove-attachment="${escapeHtml(attachment.id)}" aria-label="Remove ${escapeHtml(attachment.name)}" title="Remove attachment">×</button>
    <strong>${escapeHtml(attachment.name)}</strong>
    ${attachment.path ? `<a href="${boardPath(`/api/attachments/${attachment.id}/download?taskId=${task.id}`)}">Download</a>` : `<a href="${escapeHtml(attachment.url)}" target="_blank" rel="noopener noreferrer">Open URL</a>`}
  </div>`).join("");
  const events = detail.events.slice().reverse().slice(0, 30).map((event) => `<div class="detail-row"><strong>${escapeHtml(event.kind)}</strong><span class="mono">#${event.id} · ${escapeHtml(event.createdAt)}</span></div>`).join("");
  const dependency = (item) => `<div class="detail-row" data-open-task="${escapeHtml(item.id)}"><strong>${escapeHtml(item.title)}</strong><span class="mono">${escapeHtml(item.id)} · ${escapeHtml(item.status)}</span></div>`;
  const graph = detail.relationshipGraph;
  const focusNode = graph.nodes.find((node) => node.task.id === task.id);
  const rootNode = graph.nodes.find((node) => node.task.id === graph.rootTaskId);
  const graphDisplayNodes = [focusNode, rootNode, ...graph.nodes]
    .filter(Boolean)
    .filter((node, index, values) => values.findIndex((candidate) => candidate.task.id === node.task.id) === index)
    .slice(0, 100);
  const graphRows = graphDisplayNodes.map((node) => `<div class="detail-row" data-open-task="${escapeHtml(node.task.id)}">
    <strong>${node.task.id === task.id ? "Current · " : ""}${escapeHtml(node.task.title)}</strong>
    <span class="detail-status">Phase ${node.phase >= 0 ? node.phase + 1 : "?"} · ${escapeHtml(node.task.status)}</span>
    <span class="mono">${escapeHtml(node.task.id)}${node.parentTaskId ? ` · subtask of ${escapeHtml(node.parentTaskId)}` : node.task.id === graph.rootTaskId ? " · hierarchy root" : ""}</span>
    <div>Requires: ${node.blockedBy.length > 0 ? escapeHtml(node.blockedBy.join(", ")) : "all prerequisites complete or none"}</div>
  </div>`).join("");
  const uiOmittedNodeCount = Math.max(0, graph.totalConnectedNodes - graphDisplayNodes.length);
  const graphLimitNotice = uiOmittedNodeCount > 0
    ? `<div class="detail-row"><strong>Bounded graph view</strong><div>Showing ${graphDisplayNodes.length} of ${graph.totalConnectedNodes} connected tasks. ${uiOmittedNodeCount} distant nodes are omitted from the drawer without blocking worker context.</div></div>`
    : "";
  const parentTask = detail.parentTask
    ? `<div class="detail-row" data-open-task="${escapeHtml(detail.parentTask.id)}"><button type="button" class="icon-button compact" data-remove-parent-task="${escapeHtml(detail.parentTask.id)}" aria-label="Remove parent task">×</button><strong>${escapeHtml(detail.parentTask.title)}</strong><span class="mono">${escapeHtml(detail.parentTask.id)}</span></div>`
    : "<small>No parent task</small>";
  const subtasks = detail.subtasks.map((subtask) => `<div class="detail-row" data-open-task="${escapeHtml(subtask.id)}"><button type="button" class="icon-button compact" data-remove-subtask="${escapeHtml(subtask.id)}" aria-label="Remove subtask">×</button><strong>${escapeHtml(subtask.title)}</strong><span class="mono">${escapeHtml(subtask.id)} · ${escapeHtml(subtask.status)}</span></div>`).join("");
  const selectedProfile = workerProfiles().find((profile) => profile.name === (task.assignee || "") && profile.runtime === task.runtime);
  const drawerProfileOptions = `<option value="">Custom assignment</option>${workerProfiles().map((profile) =>
    `<option value="${escapeHtml(profile.name)}"${selectedProfile?.name === profile.name ? " selected" : ""}>${escapeHtml(profile.name)} · ${escapeHtml(profile.runtime)}</option>`).join("")}`;
  const routeModel = selectedProfile?.model || (task.runtime === "manual" ? "Manual task" : "CLI default (unpinned)");
  $("#drawer-content").innerHTML = `
    <div class="drawer-title-block"><span class="eyebrow">Task</span><h1>${escapeHtml(task.title)}</h1></div>
    <div class="task-context">
      <div class="task-context-owner ${task.assignee ? "" : "unassigned"}">
        <span class="avatar" aria-hidden="true">${escapeHtml(initials(task.assignee))}</span>
        <span><small>Owner</small><strong>${escapeHtml(task.assignee || "Unassigned")}</strong></span>
      </div>
      <div><small>Runtime</small><strong>${escapeHtml(task.runtime)}</strong></div>
      <div><small>Last updated</small><strong>${relativeTime(task.updatedAt)}</strong></div>
    </div>
    <label>Edit title<input id="edit-title" value="${escapeHtml(task.title)}"></label>
    <div class="drawer-grid drawer-routing-grid">
      <label>Board profile<select id="edit-profile">${drawerProfileOptions}</select></label>
      <label>Assignee<input id="edit-assignee" value="${escapeHtml(task.assignee || "")}"></label>
      <label>Runtime<select id="edit-runtime">${["manual", "codex", "claude", "cline", "gemini"].map((item) => `<option ${item === task.runtime ? "selected" : ""}>${item}</option>`).join("")}</select></label>
      <label>Current route model<input id="edit-model-preview" value="${escapeHtml(routeModel)}" readonly></label>
      <label>Priority<input id="edit-priority" type="number" value="${task.priority}"></label>
    </div>
    <label>Description<textarea id="edit-body" rows="9">${escapeHtml(task.body)}</textarea></label>
    <button id="save-task" class="primary">Save changes</button>
    <div class="action-row">
      ${task.status === "triage" ? '<button data-action="specify">Specify</button><button data-action="decompose">Decompose</button>' : ""}
      ${task.status === "blocked" ? '<button data-action="unblock">Unblock</button>' : ""}
      ${task.status === "ready" ? '<button data-action="dispatch" class="primary">Run task</button>' : ""}
      ${["todo", "scheduled", "blocked", "triage", "review"].includes(task.status) ? '<button data-action="promote">Promote</button>' : ""}
      ${!["running", "done", "archived"].includes(task.status) ? '<button data-action="complete">Complete</button><button data-action="block">Block</button>' : ""}
      ${!["running", "archived"].includes(task.status) ? '<button data-action="archive">Archive</button>' : ""}
      ${task.status !== "running" ? '<button data-action="delete" class="danger">Delete</button>' : '<span class="action-note">Terminate the active run below before completing, blocking, archiving, or deleting.</span>'}
    </div>
    <h3>Rendered description</h3><div class="markdown">${markdown(task.body || "(empty)")}</div>
    <h3>Execution order</h3>
    <div class="detail-row"><strong>Phase ${focusNode?.phase >= 0 ? focusNode.phase + 1 : "?"} of ${graph.totalPhases}</strong><span class="mono">Hierarchy root · ${escapeHtml(graph.rootTaskId)}</span><div>Claims are allowed only after every direct prerequisite handoff is satisfied.</div></div>
    <div class="detail-list">${graphLimitNotice}${graphRows}</div>
    <h3>Task hierarchy</h3>
    <small>Hierarchy records parent/subtask ownership. It does not control execution order.</small>
    <div class="detail-list">${parentTask}</div>
    <form id="set-parent-task" class="link-form"><select required><option value="">Set parent task…</option>${taskOptions(task.id)}</select><button>Set</button></form>
    <div class="detail-list">${subtasks || '<small>No subtasks</small>'}</div>
    <form id="add-subtask" class="link-form"><select required><option value="">Add subtask…</option>${taskOptions(task.id)}</select><button>Add</button></form>
    <h3>Execution dependencies</h3>
    <small>Prerequisites must reach Done before this task can be claimed.</small>
    <h4>Prerequisites</h4>
    <div class="detail-list">${detail.parents.map(dependency).join("") || '<small>No parents</small>'}</div>
    <form id="add-parent" class="link-form"><select required><option value="">Add prerequisite…</option>${taskOptions(task.id)}</select><button>Add</button></form>
    <h4>Dependents</h4>
    <div class="detail-list">${detail.children.map(dependency).join("") || '<small>No children</small>'}</div>
    <form id="add-child" class="link-form"><select required><option value="">Add dependent…</option>${taskOptions(task.id)}</select><button>Add</button></form>
    <h3>Comments</h3><div class="detail-list">${comments || '<small>No comments</small>'}</div>
    <form id="comment-form" class="comment-form"><input required placeholder="Add durable context…"><button>Comment</button></form>
    <h3>Attachments</h3><div class="detail-list">${attachments || '<small>No attachments</small>'}</div>
    <form id="attachment-form" class="attachment-form"><input type="file" multiple required><button>Upload</button></form>
    <h3>Run history</h3><div class="detail-list">${runRows || '<small>No runs</small>'}</div>
    <h3>Recent events</h3><div class="detail-list">${events}</div>`;
  bindDrawer(detail);
}

function bindDrawer(detail) {
  const taskId = detail.task.id;
  const updateRoutePreview = () => {
    const profile = profileByName($("#edit-profile").value);
    if (profile) {
      $("#edit-assignee").value = profile.name;
      $("#edit-runtime").value = profile.runtime;
      $("#edit-model-preview").value = profile.model || "CLI default (unpinned)";
      return;
    }
    $("#edit-model-preview").value = $("#edit-runtime").value === "manual" ? "Manual task" : "CLI default (unpinned)";
  };
  $("#edit-profile").addEventListener("change", updateRoutePreview);
  $("#edit-assignee").addEventListener("input", () => { $("#edit-profile").value = ""; updateRoutePreview(); });
  $("#edit-runtime").addEventListener("change", () => { $("#edit-profile").value = ""; updateRoutePreview(); });
  $("#save-task").addEventListener("click", async () => {
    try {
      await api(boardPath(`/api/tasks/${taskId}`), { method: "PATCH", body: JSON.stringify({
        title: $("#edit-title").value, body: $("#edit-body").value,
        assignee: $("#edit-assignee").value || null, runtime: $("#edit-runtime").value,
        priority: Number($("#edit-priority").value),
      }) });
      toast("Task saved"); await loadBoard(); await openDrawer(taskId);
    } catch (error) { toast(error.message, true); }
  });
  $$('[data-action]', $("#drawer-content")).forEach((button) => button.addEventListener("click", () => drawerAction(taskId, button.dataset.action)));
  $$('[data-open-task]', $("#drawer-content")).forEach((row) => row.addEventListener("click", () => openDrawer(row.dataset.openTask)));
  $$('[data-remove-parent-task]', $("#drawer-content")).forEach((button) => button.addEventListener("click", async (event) => {
    event.stopPropagation();
    await api(boardPath(`/api/hierarchy?parentTaskId=${encodeURIComponent(button.dataset.removeParentTask)}&subtaskId=${encodeURIComponent(taskId)}`), { method: "DELETE" });
    await openDrawer(taskId); await loadBoard();
  }));
  $$('[data-remove-subtask]', $("#drawer-content")).forEach((button) => button.addEventListener("click", async (event) => {
    event.stopPropagation();
    await api(boardPath(`/api/hierarchy?parentTaskId=${encodeURIComponent(taskId)}&subtaskId=${encodeURIComponent(button.dataset.removeSubtask)}`), { method: "DELETE" });
    await openDrawer(taskId); await loadBoard();
  }));
  $$('[data-terminate-run]', $("#drawer-content")).forEach((button) => button.addEventListener("click", async () => {
    if (!confirm("Terminate this active run and release its task?")) return;
    const termination = await api(boardPath(`/api/runs/${button.dataset.terminateRun}/terminate`), { method: "POST", body: JSON.stringify({ reason: "Terminated by dashboard user" }) });
    if (termination.pending) toast("Termination signal sent; the task will be released after the worker exits.");
    await openDrawer(taskId); await loadBoard();
  }));
  $$('[data-remove-attachment]', $("#drawer-content")).forEach((button) => button.addEventListener("click", async () => {
    await api(boardPath(`/api/tasks/${taskId}/attachments/${button.dataset.removeAttachment}`), { method: "DELETE" });
    await openDrawer(taskId);
  }));
  $("#comment-form").addEventListener("submit", async (event) => {
    event.preventDefault(); const input = $("input", event.currentTarget);
    await api(boardPath(`/api/tasks/${taskId}/comments`), { method: "POST", body: JSON.stringify({ body: input.value, author: "dashboard" }) });
    await openDrawer(taskId);
  });
  $("#attachment-form").addEventListener("submit", async (event) => {
    event.preventDefault(); const files = [...$("input", event.currentTarget).files]; if (!files.length) return;
    for (const file of files) {
      await api(boardPath(`/api/tasks/${taskId}/attachments?name=${encodeURIComponent(file.name)}`), {
        method: "POST", body: file, headers: { "content-type": file.type || "application/octet-stream" },
      });
    }
    await openDrawer(taskId);
  });
  const link = (formId, parent) => $(formId).addEventListener("submit", async (event) => {
    event.preventDefault(); const selected = $("select", event.currentTarget).value; if (!selected) return;
    await api(boardPath("/api/links"), { method: "POST", body: JSON.stringify(parent ? { parentId: selected, childId: taskId } : { parentId: taskId, childId: selected }) });
    await openDrawer(taskId); await loadBoard();
  });
  link("#add-parent", true); link("#add-child", false);
  const hierarchy = (formId, selectedIsParent) => $(formId).addEventListener("submit", async (event) => {
    event.preventDefault(); const selected = $("select", event.currentTarget).value; if (!selected) return;
    await api(boardPath("/api/hierarchy"), {
      method: "POST",
      body: JSON.stringify(selectedIsParent
        ? { parentTaskId: selected, subtaskId: taskId }
        : { parentTaskId: taskId, subtaskId: selected }),
    });
    await openDrawer(taskId); await loadBoard();
  });
  hierarchy("#set-parent-task", true); hierarchy("#add-subtask", false);
}

async function drawerAction(taskId, action) {
  try {
    if (action === "delete") {
      if (!confirm("Permanently delete this task?")) return;
      await api(boardPath(`/api/tasks/${taskId}`), { method: "DELETE" }); closeDrawer();
    } else if (action === "complete") {
      const summary = prompt("Completion summary:"); if (!summary) return;
      await api(boardPath(`/api/tasks/${taskId}/complete`), { method: "POST", body: JSON.stringify({ summary }) });
    } else if (action === "block") {
      const reason = prompt("Block reason:"); if (!reason) return;
      await api(boardPath(`/api/tasks/${taskId}/block`), { method: "POST", body: JSON.stringify({ reason, kind: "needs_input" }) });
    } else if (action === "specify" || action === "decompose") {
      if (!confirm(`${action} this triage card using the board planner?`)) return;
      await api(boardPath(`/api/tasks/${taskId}/${action}`), { method: "POST", body: "{}" });
    } else if (action === "dispatch") {
      await api(boardPath("/api/dispatch"), { method: "POST", body: JSON.stringify({ taskId }) });
      toast("Task sent to the dispatcher");
    } else if (action === "archive") {
      if (!confirm("Archive this task?")) return;
      await api(boardPath(`/api/tasks/${taskId}/archive`), { method: "POST", body: "{}" }); closeDrawer();
    } else {
      await api(boardPath(`/api/tasks/${taskId}/${action}`), { method: "POST", body: "{}" });
    }
    await loadBoard(); if (state.drawerTask) await openDrawer(taskId);
  } catch (error) { toast(error.message, true); }
}

function parseRoute(value) {
  const [name, runtime = "codex", ...description] = value.trim().split(":");
  if (!name || !["codex", "claude", "cline", "gemini"].includes(runtime)) throw new Error(`Invalid route: ${value}`);
  return { name, runtime, description: description.join(":") };
}

function profileEditorRow(profile = {}) {
  const runtime = ["codex", "claude", "cline", "gemini"].includes(profile.runtime) ? profile.runtime : "codex";
  const options = ["codex", "claude", "cline", "gemini"].map((value) =>
    `<option value="${value}"${value === runtime ? " selected" : ""}>${value}</option>`).join("");
  return `<article class="profile-row">
    <div class="profile-row-head">
      <label>Name<input data-profile="name" value="${escapeHtml(profile.name || "")}" placeholder="implementer" required></label>
      <label>Runtime<select data-profile="runtime">${options}</select></label>
      <label>Model<input data-profile="model" value="${escapeHtml(profile.model || "")}" placeholder="CLI default"></label>
      <label>Provider<input data-profile="provider" value="${escapeHtml(profile.provider || "")}" placeholder="Cline only"></label>
      <button type="button" class="ghost profile-remove" aria-label="Remove profile">Remove</button>
    </div>
    <label>Description<textarea data-profile="description" rows="2">${escapeHtml(profile.description || "")}</textarea></label>
    <div class="profile-row-options">
      <label class="inline"><input data-profile="enabled" type="checkbox"${profile.disabled ? "" : " checked"}> Enabled</label>
      <label>Max running<input data-profile="maxConcurrent" type="number" min="0" value="${Number(profile.maxConcurrent) || 0}"></label>
      <label>Priority<input data-profile="priority" type="number" value="${Number(profile.priority) || 0}"></label>
      <label>Fallback profiles<input data-profile="fallbacks" value="${escapeHtml((profile.fallbacks || []).join(", "))}" placeholder="claude-backup, local-cline"></label>
    </div>
  </article>`;
}

function renderProfileEditor(profiles = []) {
  $("#profile-list").innerHTML = profiles.map(profileEditorRow).join("");
}

function readProfileEditor() {
  return $$(".profile-row", $("#profile-list")).map((row) => {
    const get = (name) => $(`[data-profile=${name}]`, row);
    const name = get("name").value.trim();
    if (!name) throw new Error("Every worker profile needs a name");
    return {
      name, runtime: get("runtime").value, model: get("model").value.trim(), provider: get("provider").value.trim(),
      description: get("description").value.trim(), disabled: !get("enabled").checked,
      maxConcurrent: Number(get("maxConcurrent").value) || 0, priority: Number(get("priority").value) || 0,
      fallbacks: get("fallbacks").value.split(",").map((value) => value.trim()).filter(Boolean),
    };
  });
}

function commaIDs(value) {
  return [...new Set(String(value || "").split(",")
    .map((item) => item.trim().toLowerCase()).filter(Boolean))];
}

function blankAgentConfig() {
  return {
    schemaVersion: 1,
    supervisor: { autoStart: false, maxWorkers: 1, allowWrites: false },
    defaults: { workerAgents: [], plannerAgents: [], judgeAgents: [] },
    agents: [],
  };
}

function detectionForAgent(agent) {
  return state.detections.find((item) => item.id === agent.id)
    || state.detections.find((item) => item.runtime === agent.runtime && item.executable === agent.command)
    || state.detections.find((item) => item.runtime === agent.runtime);
}

function effectiveAgentFor(agent) {
  return state.effectiveAgents.find((item) => item.name === agent.id);
}

function agentEditorRow(agent = {}) {
  const runtime = ["codex", "claude", "cline", "gemini"].includes(agent.runtime) ? agent.runtime : "codex";
  const runtimeOptions = ["codex", "claude", "cline", "gemini"].map((value) =>
    `<option value="${value}"${value === runtime ? " selected" : ""}>${value}</option>`).join("");
  const roles = new Set(agent.roles || ["worker"]);
  const detection = detectionForAgent({ ...agent, runtime });
  const effective = effectiveAgentFor(agent);
  const stateName = detection?.state || "configured";
  const stateClass = ["installed", "version_unavailable"].includes(stateName) ? stateName : "";
  const health = effective?.health?.status && effective.health.status !== "unknown"
    ? ` · ${effective.health.status}${effective.activeRuns ? ` · ${effective.activeRuns} active` : ""}` : "";
  const detectionNote = detection
    ? `${detection.version || detection.message || "No version details"}${health}`
    : `Configured command; standard PATH detection has not matched it${health}`;
  return `<article class="agent-row" data-original-id="${escapeHtml(agent.id || "")}">
    <div class="agent-row-head">
      <label class="inline agent-enabled"><input data-agent="enabled" type="checkbox"${agent.enabled ? " checked" : ""}> Enabled</label>
      <div class="agent-row-title"><strong>${escapeHtml(agent.id || "New agent")}</strong><span class="agent-state ${stateClass}">${escapeHtml(stateName.replaceAll("_", " "))}</span></div>
      <button type="button" class="ghost compact agent-row-remove">Remove</button>
    </div>
    <div class="agent-fields">
      <label>Agent ID<input data-agent="id" value="${escapeHtml(agent.id || "")}" placeholder="codex-primary" required pattern="[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?"></label>
      <label>Runtime<select data-agent="runtime">${runtimeOptions}</select></label>
      <label>Command<input data-agent="command" value="${escapeHtml(agent.command || runtime)}" required></label>
      <label>Model<input data-agent="model" value="${escapeHtml(agent.model || "")}" placeholder="CLI default (unpinned)"></label>
    </div>
    <div class="agent-fields secondary">
      <label>Provider<input data-agent="provider" value="${escapeHtml(agent.provider || "")}" placeholder="optional"></label>
      <label>Maximum concurrent<input data-agent="maxConcurrent" type="number" min="1" max="64" value="${Math.max(1, Number(agent.maxConcurrent) || 1)}"></label>
      <label>Fallback agent IDs<input data-agent="fallbacks" value="${escapeHtml((agent.fallbacks || []).join(", "))}" placeholder="claude-backup"></label>
    </div>
    <div class="agent-roles"><span>Roles</span>
      ${["worker", "planner", "judge"].map((role) => `<label class="inline"><input data-agent-role="${role}" type="checkbox"${roles.has(role) ? " checked" : ""}> ${role}</label>`).join("")}
    </div>
    <div class="agent-detection-note">${escapeHtml(detectionNote)}</div>
  </article>`;
}

function renderAgentEditor(config = state.agentConfig || blankAgentConfig()) {
  const form = $("#agents-form");
  form.elements.autoStart.checked = Boolean(config.supervisor?.autoStart);
  form.elements.maxWorkers.value = Math.max(1, Number(config.supervisor?.maxWorkers) || 1);
  form.elements.allowWrites.checked = Boolean(config.supervisor?.allowWrites);
  form.elements.workerAgents.value = (config.defaults?.workerAgents || []).join(", ");
  form.elements.plannerAgents.value = (config.defaults?.plannerAgents || []).join(", ");
  form.elements.judgeAgents.value = (config.defaults?.judgeAgents || []).join(", ");
  $("#agent-list").innerHTML = (config.agents || []).map(agentEditorRow).join("")
    || '<div class="detail-row"><strong>No coding agents configured</strong><div>Detect installed CLIs or add one manually.</div></div>';
  const installed = state.detections.filter((item) => item.state === "installed").length;
  const uncertain = state.detections.filter((item) => item.state === "version_unavailable").length;
  const missing = state.detections.filter((item) => item.state === "missing").length;
  $("#agent-detection-summary").textContent = state.detections.length
    ? `${installed} installed, ${uncertain} need verification, ${missing} not found on PATH.`
    : "No detection results yet.";
}

function readAgentEditor() {
  const form = $("#agents-form");
  const agents = $$(".agent-row", $("#agent-list")).map((row) => {
    const field = (name) => $(`[data-agent=${name}]`, row);
    const roles = $$('[data-agent-role]:checked', row).map((input) => input.dataset.agentRole);
    const id = field("id").value.trim().toLowerCase();
    if (!id) throw new Error("Every coding agent needs an ID");
    if (!roles.length) throw new Error(`Agent ${id} needs at least one role`);
    return {
      id, runtime: field("runtime").value, command: field("command").value.trim(),
      model: field("model").value.trim(), provider: field("provider").value.trim(),
      enabled: field("enabled").checked, maxConcurrent: Number(field("maxConcurrent").value) || 1,
      roles, fallbacks: commaIDs(field("fallbacks").value),
    };
  });
  const seen = new Set();
  for (const agent of agents) {
    if (seen.has(agent.id)) throw new Error(`Agent ID ${agent.id} is duplicated`);
    seen.add(agent.id);
  }
  return {
    schemaVersion: 1,
    supervisor: {
      autoStart: form.elements.autoStart.checked,
      maxWorkers: Number(form.elements.maxWorkers.value) || 1,
      allowWrites: form.elements.allowWrites.checked,
    },
    defaults: {
      workerAgents: commaIDs(form.elements.workerAgents.value),
      plannerAgents: commaIDs(form.elements.plannerAgents.value),
      judgeAgents: commaIDs(form.elements.judgeAgents.value),
    },
    agents,
  };
}

function suggestedAgentConfig(config) {
  const result = JSON.parse(JSON.stringify(config || blankAgentConfig()));
  result.agents ||= [];
  const added = [];
  for (const detection of state.detections) {
    if (detection.state === "missing") continue;
    const alreadyConfigured = result.agents.some((agent) => agent.id === detection.id
      || (agent.runtime === detection.runtime && detection.executable && agent.command === detection.executable));
    if (alreadyConfigured) continue;
    const enabled = detection.state === "installed";
    result.agents.push({
      id: detection.id, runtime: detection.runtime, command: detection.executable || detection.runtime,
      model: "", provider: "", enabled, maxConcurrent: 1,
      roles: ["worker", "planner", "judge"], fallbacks: [],
    });
    if (enabled) added.push(detection.id);
  }
  result.defaults ||= { workerAgents: [], plannerAgents: [], judgeAgents: [] };
  for (const [key, role] of [["workerAgents", "worker"], ["plannerAgents", "planner"], ["judgeAgents", "judge"]]) {
    if (!(result.defaults[key] || []).length) {
      result.defaults[key] = result.agents.filter((agent) => agent.enabled && agent.roles.includes(role)).map((agent) => agent.id);
    }
  }
  const eligibleWorkers = result.agents.filter((agent) => agent.enabled && agent.roles.includes("worker")).map((agent) => agent.id);
  for (const id of added) {
    const index = eligibleWorkers.indexOf(id);
    const agent = result.agents.find((item) => item.id === id);
    if (agent && agent.roles.includes("worker") && !(agent.fallbacks || []).length) {
      agent.fallbacks = eligibleWorkers.slice(index + 1);
    }
  }
  return { config: result, added };
}

async function loadAgentConfiguration() {
  const [configuration, supervisor] = await Promise.all([api("/api/config"), api("/api/supervisor")]);
  state.agentConfig = configuration.config;
  state.agentConfigExists = configuration.exists;
  state.supervisor = supervisor;
  $("#agents-config-path").textContent = configuration.path;
  return configuration;
}

function renderSupervisorStatus() {
  const status = state.supervisor || {};
  const statusElement = $("#supervisor-status");
  const toggle = $("#supervisor-toggle");
  if (status.running) {
    statusElement.textContent = `Running · ${status.maxWorkers || 1} worker${status.maxWorkers === 1 ? "" : "s"} · ${status.allowWrites ? "workspace writes allowed" : "read-only workers"}`;
    toggle.textContent = "Stop";
  } else {
    statusElement.textContent = status.lastError ? `Stopped · ${status.lastError}` : "Stopped";
    toggle.textContent = "Start";
  }
  toggle.disabled = !state.agentConfigExists;
}

async function detectAgents(addSuggestions = true) {
  const button = $("#detect-agents");
  button.disabled = true;
  button.textContent = "Detecting…";
  try {
    const result = await api("/api/agents/detect", { method: "POST", body: "{}" });
    state.detections = result.agents || [];
    if (addSuggestions) {
      const suggested = suggestedAgentConfig(readAgentEditor());
      renderAgentEditor(suggested.config);
      if (suggested.added.length) toast(`Added ${suggested.added.join(", ")} as detected suggestions`);
    } else {
      renderAgentEditor(readAgentEditor());
    }
  } catch (error) { toast(error.message, true); }
  finally { button.disabled = false; button.textContent = "Detect CLIs"; }
}

async function openAgentSettings({ firstRun = false } = {}) {
  if (!state.agentConfig) await loadAgentConfiguration();
  if (firstRun) state.agentConfig.supervisor.autoStart = true;
  renderAgentEditor(state.agentConfig);
  renderSupervisorStatus();
  const dialog = $("#agents-dialog");
  if (!dialog.open) dialog.showModal();
  try {
    const effective = await api(boardPath("/api/agents/effective"));
    state.effectiveAgents = effective.profiles || [];
  } catch (error) { state.effectiveAgents = []; }
  await detectAgents(true);
}

async function submitAgentSettings(event) {
  event.preventDefault();
  const button = $("#agents-submit");
  try {
    const config = readAgentEditor();
    button.disabled = true; button.textContent = "Applying…";
    const saved = await api("/api/config", { method: "PUT", body: JSON.stringify(config) });
    state.agentConfig = saved.config; state.agentConfigExists = saved.exists;
    state.supervisor = await api("/api/supervisor");
    renderSupervisorStatus();
    $("#agents-dialog").close();
    await loadBoard();
    toast(config.supervisor.autoStart ? "Agent settings saved; orchestration is running" : "Agent settings saved");
  } catch (error) { toast(error.message, true); }
  finally { button.disabled = false; button.textContent = "Save and apply"; }
}

async function toggleSupervisor() {
  const button = $("#supervisor-toggle");
  if (!state.agentConfigExists) return;
  try {
    button.disabled = true;
    const action = state.supervisor?.running ? "stop" : "start";
    state.supervisor = await api(`/api/supervisor/${action}`, { method: "POST", body: "{}" });
    renderSupervisorStatus();
  } catch (error) { toast(error.message, true); }
  finally { button.disabled = false; }
}

function removeAgentReferences(id) {
  if (!id) return;
  const form = $("#agents-form");
  for (const name of ["workerAgents", "plannerAgents", "judgeAgents"]) {
    form.elements[name].value = commaIDs(form.elements[name].value).filter((value) => value !== id).join(", ");
  }
  $$('[data-agent=fallbacks]', $("#agent-list")).forEach((input) => {
    input.value = commaIDs(input.value).filter((value) => value !== id).join(", ");
  });
}

function connectEvents() {
  state.socket?.close();
  const socket = new EventSource(`/api/events/stream?board=${encodeURIComponent(state.board)}&since=${state.cursor}`);
  state.socket = socket;
  socket.addEventListener("open", () => { $("#connection").textContent = "live"; $("#connection").classList.add("online"); });
  socket.addEventListener("message", (message) => {
    const payload = JSON.parse(message.data);
    if (payload.cursor) state.cursor = payload.cursor;
    scheduleRefresh();
  });
  socket.addEventListener("error", () => {
    $("#connection").textContent = "offline"; $("#connection").classList.remove("online");
  });
}

let refreshTimer;
function scheduleRefresh() {
  clearTimeout(refreshTimer);
  refreshTimer = setTimeout(async () => {
    await loadBoard();
    if (state.drawerTask) await openDrawer(state.drawerTask).catch(() => closeDrawer());
  }, 180);
}

function initializeSelects() {
  const mutableStatuses = STATUSES.filter((status) => status !== "running");
  const options = mutableStatuses.map((status) => `<option value="${status}">${status}</option>`).join("");
  $("#task-form [name=status]").innerHTML = options;
  $("#bulk-status").innerHTML = `<option value="">Move to…</option>${options}`;
  $("#show-archived").checked = localStorage.getItem("autogora.showArchived") === "true";
  $("#lane-profile").checked = localStorage.getItem("autogora.laneByProfile") === "true";
}

function bindGlobalActions() {
  $$('[data-close-dialog]').forEach((button) => button.addEventListener("click", () => button.closest("dialog").close()));
  $("#board-select").addEventListener("change", async (event) => {
    state.board = event.target.value; state.cursor = 0; state.selected.clear(); localStorage.setItem("autogora.board", state.board);
    await loadBoard(); connectEvents();
  });
  ["#search", "#tenant-filter", "#assignee-filter"].forEach((selector) => $(selector).addEventListener("input", renderBoard));
  $("#lane-profile").addEventListener("change", () => { localStorage.setItem("autogora.laneByProfile", $("#lane-profile").checked); renderBoard(); });
  $("#show-archived").addEventListener("change", () => { localStorage.setItem("autogora.showArchived", $("#show-archived").checked); loadBoard(); });
  $("#drawer-close").addEventListener("click", closeDrawer); $("#scrim").addEventListener("click", closeDrawer);
  document.addEventListener("keydown", (event) => { if (event.key === "Escape" && state.drawerTask) closeDrawer(); });
  $("#bulk-clear").addEventListener("click", () => { state.selected.clear(); renderBoard(); });
  $("#bulk-status").addEventListener("change", (event) => { if (event.target.value) bulkMutation({ status: event.target.value }); });
  $("#bulk-assign").addEventListener("click", () => bulkMutation({ assignee: $("#bulk-assignee").value || null }));
  $("#bulk-archive").addEventListener("click", () => confirm("Archive selected tasks?") && bulkMutation({ archive: true }));
  $("#bulk-delete").addEventListener("click", () => confirm("Permanently delete selected tasks?") && bulkMutation({ delete: true }));
  $("#new-board").addEventListener("click", () => { $("#board-form").reset(); $("#board-dialog").showModal(); });
  $("#new-swarm").addEventListener("click", () => { $("#swarm-form").reset(); $("#swarm-dialog").showModal(); });
  $("#nudge").addEventListener("click", async () => { await api(boardPath("/api/dispatch"), { method: "POST", body: "{}" }); toast("Dispatcher pass started"); });
  $("#theme-toggle").addEventListener("click", () => setTheme(activeTheme === "dark" ? "light" : "dark"));
  $("#board-settings").addEventListener("click", openSettings);
  $("#agent-settings").addEventListener("click", () => openAgentSettings().catch((error) => toast(error.message, true)));
  $("#manage-agents").addEventListener("click", () => {
    $("#settings-dialog").close();
    openAgentSettings().catch((error) => toast(error.message, true));
  });
  $("#task-form").addEventListener("submit", submitTask);
  $("#task-form [name=profile]").addEventListener("change", updateTaskModelPreview);
  $("#task-form [name=assignee]").addEventListener("input", () => {
    $("#task-form [name=profile]").value = ""; updateTaskModelPreview();
  });
  $("#task-form [name=runtime]").addEventListener("change", () => {
    $("#task-form [name=profile]").value = ""; updateTaskModelPreview();
  });
  $("#board-form").addEventListener("submit", submitBoard);
  $("#settings-form").addEventListener("submit", submitSettings);
  $("#auto-describe-profiles").addEventListener("click", autoDescribeProfiles);
  $("#add-profile").addEventListener("click", () => {
    $("#profile-list").insertAdjacentHTML("beforeend", profileEditorRow());
    $(".profile-row:last-child [data-profile=name]", $("#profile-list"))?.focus();
  });
  $("#profile-list").addEventListener("click", (event) => {
    if (event.target.closest(".profile-remove")) event.target.closest(".profile-row").remove();
  });
  $("#agents-form").addEventListener("submit", submitAgentSettings);
  $("#detect-agents").addEventListener("click", () => detectAgents(true));
  $("#add-agent").addEventListener("click", () => {
    const empty = $("#agent-list .detail-row"); if (empty) empty.remove();
    $("#agent-list").insertAdjacentHTML("beforeend", agentEditorRow({ enabled: false, roles: ["worker"], maxConcurrent: 1 }));
    $(".agent-row:last-child [data-agent=id]", $("#agent-list"))?.focus();
  });
  $("#agent-list").addEventListener("click", (event) => {
    const remove = event.target.closest(".agent-row-remove");
    if (!remove) return;
    const row = remove.closest(".agent-row");
    removeAgentReferences($("[data-agent=id]", row).value.trim().toLowerCase());
    row.remove();
    if (!$(".agent-row", $("#agent-list"))) renderAgentEditor({ ...readAgentEditor(), agents: [] });
  });
  $("#agent-list").addEventListener("input", (event) => {
    if (!event.target.matches("[data-agent=id]")) return;
    $(".agent-row-title strong", event.target.closest(".agent-row")).textContent = event.target.value.trim() || "New agent";
  });
  $("#agents-later").addEventListener("click", () => {
    sessionStorage.setItem("autogora.agentSetupDeferred", "true"); $("#agents-dialog").close();
  });
  $("#supervisor-toggle").addEventListener("click", toggleSupervisor);
  $("#swarm-form").addEventListener("submit", submitSwarm);
  $("#archive-board").addEventListener("click", archiveBoard);
}

async function submitTask(event) {
  event.preventDefault(); const data = new FormData(event.currentTarget);
  try {
    await api(boardPath("/api/tasks"), { method: "POST", body: JSON.stringify({
      title: data.get("title"), body: data.get("body"), status: data.get("status"),
      assignee: data.get("assignee") || null, runtime: data.get("runtime"), priority: Number(data.get("priority")),
      tenant: data.get("tenant") || null, workspaceKind: data.get("workspaceKind"), workspace: data.get("workspace") || null,
      skills: String(data.get("skills") || "").split(",").map((item) => item.trim()).filter(Boolean), goalMode: data.get("goalMode") === "on",
    }) });
    $("#task-dialog").close(); await loadBoard();
  } catch (error) { toast(error.message, true); }
}

async function submitBoard(event) {
  event.preventDefault(); const data = new FormData(event.currentTarget);
  try {
    const board = await api("/api/boards", { method: "POST", body: JSON.stringify({
      slug: data.get("slug"), name: data.get("name"), description: data.get("description"), icon: data.get("icon"),
      defaultWorkdir: data.get("defaultWorkdir") || null, switch: true,
    }) });
    state.board = board.slug; state.cursor = 0; localStorage.setItem("autogora.board", state.board);
    $("#board-dialog").close(); await loadBoards(); await loadBoard(); connectEvents();
  } catch (error) { toast(error.message, true); }
}

function openSettings() {
  const form = $("#settings-form"); const metadata = state.metadata; const settings = metadata.orchestration;
  form.elements.name.value = metadata.name; form.elements.description.value = metadata.description;
  form.elements.color.value = /^#[0-9a-f]{6}$/i.test(metadata.color) ? metadata.color : "#5b7cff";
  form.elements.defaultWorkdir.value = metadata.defaultWorkdir || ""; form.elements.autoDecompose.checked = settings.autoDecompose;
  form.elements.autoPromoteChildren.checked = settings.autoPromoteChildren;
  form.elements.plannerRuntime.value = settings.plannerRuntime; form.elements.autoDecomposePerTick.value = settings.autoDecomposePerTick;
  form.elements.plannerModel.value = settings.plannerModel || ""; form.elements.plannerProvider.value = settings.plannerProvider || "";
  form.elements.defaultProfile.value = settings.defaultProfile || ""; form.elements.orchestratorProfile.value = settings.orchestratorProfile || "";
  renderProfileEditor(settings.profiles || []);
  $("#archive-board").classList.toggle("hidden", state.board === "default");
  $("#settings-dialog").showModal();
}

async function submitSettings(event) {
  event.preventDefault(); const data = new FormData(event.currentTarget);
  try {
    const profiles = readProfileEditor();
    await api(`/api/boards/${encodeURIComponent(state.board)}`, { method: "PATCH", body: JSON.stringify({
      name: data.get("name"), description: data.get("description"), color: data.get("color"), defaultWorkdir: data.get("defaultWorkdir") || null,
      orchestration: { autoDecompose: data.get("autoDecompose") === "on", autoPromoteChildren: data.get("autoPromoteChildren") === "on", plannerRuntime: data.get("plannerRuntime"),
        plannerModel: data.get("plannerModel"), plannerProvider: data.get("plannerProvider"),
        autoDecomposePerTick: Number(data.get("autoDecomposePerTick")), defaultProfile: data.get("defaultProfile") || null,
        orchestratorProfile: data.get("orchestratorProfile") || null, profiles },
    }) });
    $("#settings-dialog").close(); await loadBoards(); await loadBoard();
  } catch (error) { toast(error.message, true); }
}

async function autoDescribeProfiles() {
  const button = $("#auto-describe-profiles");
  try {
    const profiles = readProfileEditor();
    const blank = profiles.filter((profile) => !profile.description?.trim());
    if (!blank.length) { toast("Every configured profile already has a description"); return; }
    button.disabled = true; button.textContent = `Describing 0/${blank.length}…`;
    for (let index = 0; index < blank.length; index += 1) {
      const profile = blank[index];
      button.textContent = `Describing ${index + 1}/${blank.length}…`;
      const described = await api(boardPath(`/api/profiles/${encodeURIComponent(profile.name)}/describe-auto`), {
        method: "POST", body: JSON.stringify({ runtime: profile.runtime }),
      });
      profile.description = described.description;
    }
    renderProfileEditor(profiles);
    await loadBoard(); toast(`${blank.length} profile description${blank.length === 1 ? "" : "s"} generated`);
  } catch (error) { toast(error.message, true); }
  finally { button.disabled = false; button.textContent = "Auto-describe blank profiles"; }
}

async function submitSwarm(event) {
  event.preventDefault(); const data = new FormData(event.currentTarget);
  try {
    const workers = String(data.get("workers")).split(",").map(parseRoute);
    await api(boardPath("/api/orchestration/swarm"), { method: "POST", body: JSON.stringify({
      goal: data.get("goal"), workers, verifier: parseRoute(String(data.get("verifier"))), synthesizer: parseRoute(String(data.get("synthesizer"))),
    }) });
    $("#swarm-dialog").close(); await loadBoard(); toast("Swarm graph created");
  } catch (error) { toast(error.message, true); }
}

async function archiveBoard(event) {
  event.preventDefault();
  if (state.board === "default" || !confirm(`Archive board ${state.board}?`)) return;
  try {
    await api(`/api/boards/${encodeURIComponent(state.board)}`, { method: "DELETE" });
    state.board = "default"; localStorage.setItem("autogora.board", state.board); $("#settings-dialog").close();
    await loadBoards(); await loadBoard(); connectEvents();
  } catch (error) { toast(error.message, true); }
}

async function main() {
  setTheme(activeTheme, false); initializeSelects(); bindGlobalActions();
  try {
    await loadBoards(); await loadBoard(); connectEvents();
    const configuration = await loadAgentConfiguration();
    if (!configuration.exists && sessionStorage.getItem("autogora.agentSetupDeferred") !== "true") {
      await openAgentSettings({ firstRun: true });
    }
  }
  catch (error) { toast(error.message, true); }
}

main();
