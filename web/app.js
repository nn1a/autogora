const STATUSES = ["triage", "todo", "scheduled", "ready", "running", "blocked", "review", "done", "archived"];
const COLORS = {
  triage: "#a98cff", todo: "#8791a3", scheduled: "#e7b65b", ready: "#5e91ff",
  running: "#36c9b0", blocked: "#ff6978", review: "#de84ff", done: "#55d38b", archived: "#667085",
};

const state = {
  boards: [], board: localStorage.getItem("kanban.board") || "default", metadata: null,
  tasks: [], stats: null, selected: new Set(), drawerTask: null, cursor: 0, socket: null,
};

const $ = (selector, root = document) => root.querySelector(selector);
const $$ = (selector, root = document) => [...root.querySelectorAll(selector)];
const escapeHtml = (value = "") => String(value).replace(/[&<>'"]/g, (char) => ({
  "&": "&amp;", "<": "&lt;", ">": "&gt;", "'": "&#39;", '"': "&quot;",
})[char]);

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
  state.tasks = payload.tasks;
  state.stats = payload.stats;
  renderFilters();
  renderBoard();
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
  $("#stats").textContent = `${state.stats?.total || 0} tasks · ${state.stats?.byStatus?.running || 0} running`;
}

function cardHtml(task) {
  const progress = task.status !== "done" && task.status !== "archived" && task.childrenDone !== undefined
    ? `<span class="pill">${task.childrenDone}/${task.childrenTotal}</span>` : "";
  return `<article class="card ${state.selected.has(task.id) ? "selected" : ""}" draggable="true" data-task="${escapeHtml(task.id)}">
    <div class="card-top"><input type="checkbox" aria-label="Select ${escapeHtml(task.title)}" ${state.selected.has(task.id) ? "checked" : ""}><span class="mono">${escapeHtml(task.id)}</span>${progress}</div>
    <div class="card-title">${escapeHtml(task.title)}</div>
    <div class="card-meta">
      ${task.priority ? `<span class="pill priority">P${task.priority}</span>` : ""}
      ${task.tenant ? `<span class="pill">${escapeHtml(task.tenant)}</span>` : ""}
      <span>${escapeHtml(task.assignee || "unassigned")}</span><span>·</span><span>${escapeHtml(task.runtime)}</span>
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
  const statuses = $("#show-archived").checked ? STATUSES : STATUSES.filter((status) => status !== "archived");
  $("#board").innerHTML = statuses.map((status) => {
    const cards = tasks.filter((task) => task.status === status);
    return `<section class="column" data-status="${status}" style="--status-color:${COLORS[status]}">
      <header class="column-head"><span class="status-dot"></span><h2>${status}</h2><span class="count">${cards.length}</span><button data-create-status="${status}" aria-label="Create in ${status}">+</button></header>
      ${renderCardList(cards, status === "running" && $("#lane-profile").checked)}
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
    card.addEventListener("dragstart", (event) => {
      event.dataTransfer.setData("text/plain", taskId);
      card.classList.add("dragging");
    });
    card.addEventListener("dragend", () => card.classList.remove("dragging"));
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
}

async function moveTask(taskId, status) {
  if (status === "running") {
    toast("Tasks enter Running only when claimed by the dispatcher", true);
    return;
  }
  try {
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
  $("#task-dialog").showModal();
}

async function openDrawer(taskId) {
  try {
    const detail = await api(boardPath(`/api/tasks/${taskId}`));
    state.drawerTask = taskId;
    $("#drawer-id").textContent = taskId;
    $("#drawer-status").textContent = detail.task.status;
    $("#drawer").classList.add("open");
    $("#drawer").setAttribute("aria-hidden", "false");
    $("#scrim").classList.remove("hidden");
    renderDrawer(detail);
  } catch (error) { toast(error.message, true); }
}

function closeDrawer() {
  state.drawerTask = null;
  $("#drawer").classList.remove("open");
  $("#drawer").setAttribute("aria-hidden", "true");
  $("#scrim").classList.add("hidden");
}

function taskOptions(excludeId) {
  return state.tasks.filter((task) => task.id !== excludeId && task.status !== "archived")
    .map((task) => `<option value="${escapeHtml(task.id)}">${escapeHtml(task.id)} · ${escapeHtml(task.title)}</option>`).join("");
}

function renderDrawer(detail) {
  const task = detail.task;
  const runRows = detail.runs.slice().reverse().map((run) => `<div class="detail-row">
    ${run.status === "running" ? `<button data-terminate-run="${escapeHtml(run.id)}" class="danger">Terminate</button>` : ""}
    <strong>${escapeHtml(run.status)} · ${escapeHtml(run.workerId)}</strong>
    <span class="mono">${escapeHtml(run.id)} · ${escapeHtml(run.claimedAt)}</span>
    ${run.summary ? `<div>${escapeHtml(run.summary)}</div>` : ""}${run.error ? `<div>${escapeHtml(run.error)}</div>` : ""}
  </div>`).join("");
  const comments = detail.comments.map((comment) => `<div class="detail-row"><strong>${escapeHtml(comment.author)}</strong>${markdown(comment.body)}<div class="mono">${escapeHtml(comment.createdAt)}</div></div>`).join("");
  const attachments = detail.attachments.map((attachment) => `<div class="detail-row">
    <button data-remove-attachment="${escapeHtml(attachment.id)}" aria-label="Remove">×</button>
    <strong>${escapeHtml(attachment.name)}</strong>
    ${attachment.path ? `<a href="${boardPath(`/api/attachments/${attachment.id}/download?taskId=${task.id}`)}">Download</a>` : `<a href="${escapeHtml(attachment.url)}" target="_blank" rel="noopener noreferrer">Open URL</a>`}
  </div>`).join("");
  const events = detail.events.slice().reverse().slice(0, 30).map((event) => `<div class="detail-row"><strong>${escapeHtml(event.kind)}</strong><span class="mono">#${event.id} · ${escapeHtml(event.createdAt)}</span></div>`).join("");
  const dependency = (item) => `<div class="detail-row" data-open-task="${escapeHtml(item.id)}"><strong>${escapeHtml(item.title)}</strong><span class="mono">${escapeHtml(item.id)} · ${escapeHtml(item.status)}</span></div>`;
  $("#drawer-content").innerHTML = `
    <label>Title<input id="edit-title" value="${escapeHtml(task.title)}"></label>
    <div class="drawer-grid">
      <label>Assignee<input id="edit-assignee" value="${escapeHtml(task.assignee || "")}"></label>
      <label>Runtime<select id="edit-runtime">${["manual", "codex", "claude"].map((item) => `<option ${item === task.runtime ? "selected" : ""}>${item}</option>`).join("")}</select></label>
      <label>Priority<input id="edit-priority" type="number" value="${task.priority}"></label>
    </div>
    <label>Description<textarea id="edit-body" rows="9">${escapeHtml(task.body)}</textarea></label>
    <button id="save-task" class="primary">Save changes</button>
    <div class="action-row">
      ${task.status === "triage" ? '<button data-action="specify">Specify</button><button data-action="decompose">Decompose</button>' : ""}
      ${task.status === "blocked" ? '<button data-action="unblock">Unblock</button>' : ""}
      ${["todo", "scheduled", "blocked", "triage", "review"].includes(task.status) ? '<button data-action="promote">Promote</button>' : ""}
      ${task.status !== "done" && task.status !== "archived" ? '<button data-action="complete">Complete</button><button data-action="block">Block</button>' : ""}
      ${task.status !== "archived" ? '<button data-action="archive">Archive</button>' : ""}
      <button data-action="delete" class="danger">Delete</button>
    </div>
    <h3>Rendered description</h3><div class="markdown">${markdown(task.body || "(empty)")}</div>
    <h3>Dependencies</h3>
    <div class="detail-list">${detail.parents.map(dependency).join("") || '<small>No parents</small>'}</div>
    <form id="add-parent" class="link-form"><select required><option value="">Add parent…</option>${taskOptions(task.id)}</select><button>Add</button></form>
    <div class="detail-list">${detail.children.map(dependency).join("") || '<small>No children</small>'}</div>
    <form id="add-child" class="link-form"><select required><option value="">Add child…</option>${taskOptions(task.id)}</select><button>Add</button></form>
    <h3>Comments</h3><div class="detail-list">${comments || '<small>No comments</small>'}</div>
    <form id="comment-form" class="comment-form"><input required placeholder="Add durable context…"><button>Comment</button></form>
    <h3>Attachments</h3><div class="detail-list">${attachments || '<small>No attachments</small>'}</div>
    <form id="attachment-form" class="attachment-form"><input type="file" required><button>Upload</button></form>
    <h3>Run history</h3><div class="detail-list">${runRows || '<small>No runs</small>'}</div>
    <h3>Recent events</h3><div class="detail-list">${events}</div>`;
  bindDrawer(detail);
}

function bindDrawer(detail) {
  const taskId = detail.task.id;
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
  $$('[data-terminate-run]', $("#drawer-content")).forEach((button) => button.addEventListener("click", async () => {
    if (!confirm("Terminate this active run and release its task?")) return;
    await api(boardPath(`/api/runs/${button.dataset.terminateRun}/terminate`), { method: "POST", body: JSON.stringify({ reason: "Terminated by dashboard user" }) });
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
    event.preventDefault(); const file = $("input", event.currentTarget).files[0]; if (!file) return;
    await api(boardPath(`/api/tasks/${taskId}/attachments?name=${encodeURIComponent(file.name)}`), {
      method: "POST", body: file, headers: { "content-type": file.type || "application/octet-stream" },
    });
    await openDrawer(taskId);
  });
  const link = (formId, parent) => $(formId).addEventListener("submit", async (event) => {
    event.preventDefault(); const selected = $("select", event.currentTarget).value; if (!selected) return;
    await api(boardPath("/api/links"), { method: "POST", body: JSON.stringify(parent ? { parentId: selected, childId: taskId } : { parentId: taskId, childId: selected }) });
    await openDrawer(taskId); await loadBoard();
  });
  link("#add-parent", true); link("#add-child", false);
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
  if (!name || !["codex", "claude"].includes(runtime)) throw new Error(`Invalid route: ${value}`);
  return { name, runtime, description: description.join(":") };
}

function connectEvents() {
  state.socket?.close();
  const protocol = location.protocol === "https:" ? "wss:" : "ws:";
  const socket = new WebSocket(`${protocol}//${location.host}/api/events/ws?board=${encodeURIComponent(state.board)}&since=${state.cursor}`);
  state.socket = socket;
  socket.addEventListener("open", () => { $("#connection").textContent = "live"; $("#connection").classList.add("online"); });
  socket.addEventListener("message", (message) => {
    const payload = JSON.parse(message.data);
    if (payload.cursor) state.cursor = payload.cursor;
    scheduleRefresh();
  });
  socket.addEventListener("close", () => {
    $("#connection").textContent = "offline"; $("#connection").classList.remove("online");
    if (state.socket === socket) setTimeout(connectEvents, 1200);
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
}

function bindGlobalActions() {
  $$('[data-close-dialog]').forEach((button) => button.addEventListener("click", () => button.closest("dialog").close()));
  $("#board-select").addEventListener("change", async (event) => {
    state.board = event.target.value; state.cursor = 0; state.selected.clear(); localStorage.setItem("kanban.board", state.board);
    await loadBoard(); connectEvents();
  });
  ["#search", "#tenant-filter", "#assignee-filter", "#lane-profile"].forEach((selector) => $(selector).addEventListener("input", renderBoard));
  $("#show-archived").addEventListener("change", loadBoard);
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
  $("#board-settings").addEventListener("click", openSettings);
  $("#task-form").addEventListener("submit", submitTask);
  $("#board-form").addEventListener("submit", submitBoard);
  $("#settings-form").addEventListener("submit", submitSettings);
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
      slug: data.get("slug"), name: data.get("name"), description: data.get("description"),
      defaultWorkdir: data.get("defaultWorkdir") || null, switch: true,
    }) });
    state.board = board.slug; state.cursor = 0; localStorage.setItem("kanban.board", state.board);
    $("#board-dialog").close(); await loadBoards(); await loadBoard(); connectEvents();
  } catch (error) { toast(error.message, true); }
}

function openSettings() {
  const form = $("#settings-form"); const metadata = state.metadata; const settings = metadata.orchestration;
  form.elements.name.value = metadata.name; form.elements.description.value = metadata.description;
  form.elements.color.value = /^#[0-9a-f]{6}$/i.test(metadata.color) ? metadata.color : "#5b7cff";
  form.elements.defaultWorkdir.value = metadata.defaultWorkdir || ""; form.elements.autoDecompose.checked = settings.autoDecompose;
  form.elements.plannerRuntime.value = settings.plannerRuntime; form.elements.autoDecomposePerTick.value = settings.autoDecomposePerTick;
  form.elements.defaultProfile.value = settings.defaultProfile || ""; form.elements.orchestratorProfile.value = settings.orchestratorProfile || "";
  form.elements.profiles.value = settings.profiles.map((profile) => `${profile.name}:${profile.runtime}:${profile.description || ""}`).join("\n");
  $("#archive-board").classList.toggle("hidden", state.board === "default");
  $("#settings-dialog").showModal();
}

async function submitSettings(event) {
  event.preventDefault(); const data = new FormData(event.currentTarget);
  try {
    const profiles = String(data.get("profiles") || "").split("\n").map((line) => line.trim()).filter(Boolean).map(parseRoute);
    await api(`/api/boards/${encodeURIComponent(state.board)}`, { method: "PATCH", body: JSON.stringify({
      name: data.get("name"), description: data.get("description"), color: data.get("color"), defaultWorkdir: data.get("defaultWorkdir") || null,
      orchestration: { autoDecompose: data.get("autoDecompose") === "on", plannerRuntime: data.get("plannerRuntime"),
        autoDecomposePerTick: Number(data.get("autoDecomposePerTick")), defaultProfile: data.get("defaultProfile") || null,
        orchestratorProfile: data.get("orchestratorProfile") || null, profiles },
    }) });
    $("#settings-dialog").close(); await loadBoards(); await loadBoard();
  } catch (error) { toast(error.message, true); }
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
    state.board = "default"; localStorage.setItem("kanban.board", state.board); $("#settings-dialog").close();
    await loadBoards(); await loadBoard(); connectEvents();
  } catch (error) { toast(error.message, true); }
}

async function main() {
  initializeSelects(); bindGlobalActions();
  try { await loadBoards(); await loadBoard(); connectEvents(); }
  catch (error) { toast(error.message, true); }
}

main();
