const STATUSES = ["triage", "todo", "scheduled", "ready", "running", "blocked", "review", "done", "archived"];
const STATUS_LABELS = {
  triage: "Triage", todo: "To do", scheduled: "Scheduled", ready: "Ready",
  running: "Running", blocked: "Blocked", review: "Review", done: "Done", archived: "Archived",
};
const WORKFLOW_STAGES = [
  { id: "planning", label: "Planning", ariaLabel: "Planning workflow stage", statuses: ["triage", "todo", "scheduled", "ready"] },
  { id: "execution", label: "Execution", ariaLabel: "Execution workflow stage", statuses: ["running", "blocked", "review", "done"] },
  { id: "archive", label: "Archive", ariaLabel: "Archive workflow stage", statuses: ["archived"] },
];
const BOARD_STAGE_FOCUSES = ["all", ...WORKFLOW_STAGES.map((stage) => stage.id)];
const BOARD_VIEW_MODES = ["overview", "compact", "flow", "graph"];
const AUTOMATION_TABS = ["overview", "runs", "recovery", "publishing", "events"];
const AUTOMATION_HELP_LANGUAGES = ["en", "ko"];

const storedTheme = localStorage.getItem("autogora.theme");
const storedStageFocus = localStorage.getItem("autogora.boardStageFocus");
const storedBoardView = localStorage.getItem("autogora.boardView");
const storedAutomationTab = localStorage.getItem("autogora.automationCenterTab");
const storedAutomationHelpLanguage = localStorage.getItem("autogora.automationHelpLanguage");
let activeTheme = ["light", "dark"].includes(storedTheme)
  ? storedTheme
  : (window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light");
document.documentElement.dataset.theme = activeTheme;

const state = {
  boards: [], board: localStorage.getItem("autogora.board") || "default", metadata: null,
  profiles: [], tasks: [], taskWindow: null, stats: null, diagnostics: null, selected: new Set(), drawerTask: null, cursor: 0, socket: null,
  agentConfig: null, agentConfigExists: false, agentPresets: [], detections: [], effectiveAgents: [], supervisor: null, operations: [],
  stageFocus: BOARD_STAGE_FOCUSES.includes(storedStageFocus) ? storedStageFocus : "all",
  boardView: BOARD_VIEW_MODES.includes(storedBoardView) ? storedBoardView : "overview",
  automationTab: AUTOMATION_TABS.includes(storedAutomationTab) ? storedAutomationTab : "overview",
  automationHelpLanguage: AUTOMATION_HELP_LANGUAGES.includes(storedAutomationHelpLanguage)
    ? storedAutomationHelpLanguage
    : (navigator.language.toLowerCase().startsWith("ko") ? "ko" : "en"),
  automationData: null,
  graph: null, graphBoard: "", graphIncludeArchived: false, graphLoading: false, graphError: "",
  graphRequest: 0, graphZoom: 1, graphSignature: "", graphFitPending: false, graphFitMode: true,
  graphShowDependencies: true, graphShowHierarchy: true,
  drawerDirty: false, drawerVersion: null, drawerRequest: 0,
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

function renderMarkdownText(value = "") {
  const source = String(value);
  const links = /(^|[\s(])(https?:\/\/[^\s<]+|mailto:[^\s<]+)/g;
  let cursor = 0;
  let output = "";
  for (const match of source.matchAll(links)) {
    const url = match[2];
    const urlStart = match.index + match[1].length;
    let safe = escapeHtml(source.slice(cursor, urlStart));
    safe = safe.replace(/\*\*([^*\n]+)\*\*/g, "<strong>$1</strong>");
    safe = safe.replace(/__([^_\n]+)__/g, "<strong>$1</strong>");
    const safeURL = escapeHtml(url);
    output += `${safe}<a href="${safeURL}" target="_blank" rel="noopener noreferrer">${safeURL}</a>`;
    cursor = urlStart + url.length;
  }
  let tail = escapeHtml(source.slice(cursor));
  tail = tail.replace(/\*\*([^*\n]+)\*\*/g, "<strong>$1</strong>");
  tail = tail.replace(/__([^_\n]+)__/g, "<strong>$1</strong>");
  return output + tail;
}

function renderInlineMarkdown(value = "") {
  return String(value).split(/(`[^`\n]+`)/g).map((part) => {
    if (part.startsWith("`") && part.endsWith("`")) {
      return `<code>${escapeHtml(part.slice(1, -1))}</code>`;
    }
    return renderMarkdownText(part);
  }).join("");
}

function compactMarkdown(value = "") {
  let inFence = false;
  return String(value).replace(/\r\n?/g, "\n").split("\n").flatMap((rawLine) => {
    const line = rawLine.trim();
    if (/^```/.test(line)) {
      inFence = !inFence;
      return [];
    }
    if (!line) return [];
    if (inFence) return [`<code>${escapeHtml(line)}</code>`];
    const heading = line.match(/^#{1,6}\s+(.+)$/);
    if (heading) return [`<strong>${renderInlineMarkdown(heading[1])}</strong>`];
    const unordered = line.match(/^[-*+]\s+(.+)$/);
    if (unordered) return [`• ${renderInlineMarkdown(unordered[1])}`];
    return [renderInlineMarkdown(line)];
  }).join("<br>");
}

function markdown(value = "", options = {}) {
  if (options.compact) return compactMarkdown(value);
  const lines = String(value).replace(/\r\n?/g, "\n").split("\n");
  const output = [];
  let paragraph = [];
  let list = null;
  let code = null;

  const flushParagraph = () => {
    if (!paragraph.length) return;
    output.push(`<p>${paragraph.map(renderInlineMarkdown).join("<br>")}</p>`);
    paragraph = [];
  };
  const closeList = () => {
    if (!list) return;
    output.push(`</${list}>`);
    list = null;
  };
  const flushCode = () => {
    if (code === null) return;
    output.push(`<pre><code>${escapeHtml(code.join("\n"))}</code></pre>`);
    code = null;
  };

  for (const rawLine of lines) {
    if (code !== null) {
      if (/^\s*```/.test(rawLine)) flushCode();
      else code.push(rawLine);
      continue;
    }
    if (/^\s*```/.test(rawLine)) {
      flushParagraph();
      closeList();
      code = [];
      continue;
    }
    if (!rawLine.trim()) {
      flushParagraph();
      closeList();
      continue;
    }
    const heading = rawLine.match(/^\s*(#{1,6})\s+(.+)$/);
    if (heading) {
      flushParagraph();
      closeList();
      const level = Math.min(6, heading[1].length + 1);
      output.push(`<h${level}>${renderInlineMarkdown(heading[2])}</h${level}>`);
      continue;
    }
    const unordered = rawLine.match(/^\s*[-*+]\s+(.+)$/);
    const ordered = rawLine.match(/^\s*\d+[.)]\s+(.+)$/);
    if (unordered || ordered) {
      flushParagraph();
      const nextList = unordered ? "ul" : "ol";
      if (list !== nextList) {
        closeList();
        list = nextList;
        output.push(`<${list}>`);
      }
      output.push(`<li>${renderInlineMarkdown((unordered || ordered)[1])}</li>`);
      continue;
    }
    const quote = rawLine.match(/^\s*>\s?(.*)$/);
    if (quote) {
      flushParagraph();
      closeList();
      output.push(`<blockquote>${renderInlineMarkdown(quote[1])}</blockquote>`);
      continue;
    }
    closeList();
    paragraph.push(rawLine.trim());
  }
  flushParagraph();
  closeList();
  flushCode();
  return output.join("");
}

function relativeTime(value) {
  const seconds = Math.max(0, Math.floor((Date.now() - Date.parse(value)) / 1000));
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.floor(seconds / 60); if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60); if (hours < 24) return `${hours}h ago`;
  return `${Math.floor(hours / 24)}d ago`;
}

function localDateTimeValue(value = Date.now() + 60 * 60 * 1000) {
  const date = new Date(value);
  const offset = date.getTimezoneOffset() * 60 * 1000;
  return new Date(date.getTime() - offset).toISOString().slice(0, 16);
}

function futureScheduleISO(value) {
  const parsed = new Date(value);
  if (!value || Number.isNaN(parsed.getTime()) || parsed.getTime() <= Date.now()) {
    throw new Error("Choose a valid future schedule time");
  }
  return parsed.toISOString();
}

function boardPathFor(board, path) {
  const separator = path.includes("?") ? "&" : "?";
  return `${path}${separator}board=${encodeURIComponent(board)}`;
}

function boardPath(path) {
  return boardPathFor(state.board, path);
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
let automationLoadGeneration = 0;
let automationLoadController = null;
const automationRecoveryCache = new Map();
let operationalStatusGeneration = 0;
let graphLoadController = null;
let graphResizeObserver = null;
function toast(message, error = false) {
  const element = $("#toast");
  element.textContent = message;
  element.classList.toggle("error", error);
  element.classList.remove("hidden");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => element.classList.add("hidden"), 3500);
}

window.addEventListener("unhandledrejection", (event) => {
  event.preventDefault();
  toast(event.reason?.message || String(event.reason || "Unexpected background error"), true);
});

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
  state.taskWindow = payload.taskWindow || {
    returned: payload.tasks.length, total: payload.tasks.length, truncated: false, limit: payload.tasks.length,
  };
  state.stats = payload.stats;
  state.diagnostics = payload.diagnostics;
  renderFilters();
  renderBoard();
  if (state.boardView === "graph") await loadBoardGraph({ force: true });
}

function workerProfiles() {
  return (state.profiles || []).filter((profile) => !profile.disabled);
}

function profileByName(name) {
  return workerProfiles().find((profile) => profile.name === name);
}

function defaultWorkerProfile() {
  const preferred = state.metadata?.orchestration?.defaultProfile;
  return profileByName(preferred) || workerProfiles()[0] || null;
}

function authoritativeTaskRoute(profileName, assignee, runtime) {
  const customAssignee = String(assignee || "").trim();
  const profile = profileByName(profileName) || profileByName(customAssignee);
  if (profile) {
    return {
      profile, assignee: profile.name, runtime: profile.runtime,
      model: profile.model || "CLI default (unpinned)", provider: profile.provider || "",
    };
  }
  return {
    profile: null, assignee: customAssignee || null, runtime,
    model: runtime === "manual" ? "Manual task" : "CLI default (unpinned)", provider: "",
  };
}

function taskRoutePreview(route) {
  return route.provider ? `${route.model} · ${route.provider}` : route.model;
}

function applyAuthoritativeRouteControls(controls) {
  const route = authoritativeTaskRoute(
    controls.profile.value, controls.assignee.value, controls.runtime.value,
  );
  if (route.profile) {
    controls.profile.value = route.profile.name;
    controls.assignee.value = route.assignee;
    controls.runtime.value = route.runtime;
  }
  controls.model.value = taskRoutePreview(route);
  return route;
}

function switchRouteControlsToCustom(controls) {
  if (profileByName(controls.assignee.value)) controls.assignee.value = "";
  controls.profile.value = "";
  return applyAuthoritativeRouteControls(controls);
}

function taskDialogRouteControls() {
  const form = $("#task-form");
  return {
    profile: form.elements.profile, assignee: form.elements.assignee,
    runtime: form.elements.runtime, model: form.elements.modelPreview,
  };
}

function availableWorkerAgents() {
  const configured = state.agentConfig?.agents || [];
  return configured.filter((agent) => agent.enabled && (agent.roles || []).includes("worker"));
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
  const taskWindow = state.taskWindow || {};
  const windowWarning = taskWindow.truncated
    ? `<span class="metric window-warning" role="status" title="The board snapshot is limited. Use the task list API with targeted filters to inspect tasks outside this window."><strong>${taskWindow.returned} shown</strong><span>of ${taskWindow.total}</span></span>`
    : "";
  $("#stats").innerHTML = `
    <span class="metric"><strong>${state.stats?.total || 0}</strong><span>tasks</span></span>
    <span class="metric"><strong>${state.stats?.byStatus?.running || 0}</strong><span>running</span></span>
    ${windowWarning}
    <button type="button" id="health-details" class="health-chip ${healthy ? "healthy" : "attention"}"><span aria-hidden="true"></span>${healthy ? "Board checks clear" : `Board attention (${diagnosticIssues.length})`}</button>`;
  $("#health-details").addEventListener("click", () => openActivity().catch((error) => toast(error.message, true)));
}

function cardHtml(task) {
  const owner = task.assignee || "Unassigned";
  const route = authoritativeTaskRoute("", task.assignee, task.runtime);
  const progress = task.status !== "done" && task.status !== "archived" && task.subtasksTotal > 0
    ? `<span class="pill" title="Completed subtasks">${task.subtasksDone}/${task.subtasksTotal}</span>` : "";
  const summary = task.body?.trim()
    ? `<div class="card-summary markdown markdown-compact">${markdown(task.body.trim(), { compact: true })}</div>`
    : "";
  return `<article class="card status-${task.status} ${state.selected.has(task.id) ? "selected" : ""}" draggable="true" tabindex="0" data-task="${escapeHtml(task.id)}" data-task-version="${escapeHtml(task.updatedAt)}"
    aria-label="${escapeHtml(`${task.title}, ${STATUS_LABELS[task.status]}, ${owner}, ${route.runtime}`)}">
    <div class="card-top"><input type="checkbox" aria-label="Select ${escapeHtml(task.title)}" ${state.selected.has(task.id) ? "checked" : ""}>
      <span class="status-badge"><span class="status-dot"></span>${STATUS_LABELS[task.status]}</span>
      <span class="mono card-id">${escapeHtml(task.id)}</span>${progress}</div>
    <div class="card-title">${escapeHtml(task.title)}</div>
    ${summary}
    <div class="card-owner ${task.assignee ? "" : "unassigned"}">
      <span class="avatar" aria-hidden="true">${escapeHtml(initials(task.assignee))}</span>
      <span class="owner-copy"><small>Owner</small><strong>${escapeHtml(owner)}</strong></span>
      <span class="runtime-chip" title="Effective worker runtime">${escapeHtml(route.runtime)}</span>
    </div>
    <div class="card-foot">
      ${task.priority ? `<span class="pill priority">P${task.priority}</span>` : ""}
      ${task.tenant ? `<span class="pill">${escapeHtml(task.tenant)}</span>` : ""}
      ${task.commentsCount ? `<span class="card-indicator" title="Comments" aria-label="${task.commentsCount} comment${task.commentsCount === 1 ? "" : "s"}"><span aria-hidden="true">💬</span> ${task.commentsCount}</span>` : ""}${task.relationshipsCount ? `<span class="card-indicator" title="Relationships" aria-label="${task.relationshipsCount} relationship${task.relationshipsCount === 1 ? "" : "s"}"><span aria-hidden="true">↔</span> ${task.relationshipsCount}</span>` : ""}
      ${task.status === "scheduled" ? `<span title="Scheduled time">${task.scheduledAt ? `After ${escapeHtml(new Date(task.scheduledAt).toLocaleString())}` : "On hold · no time"}</span>` : ""}
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

function updateBoardViewControls() {
  $$("[data-stage-focus]").forEach((button) => {
    button.setAttribute("aria-pressed", String(button.dataset.stageFocus === state.stageFocus));
  });
  $$("[data-board-view]").forEach((button) => {
    button.setAttribute("aria-pressed", String(button.dataset.boardView === state.boardView));
  });
  const stage = state.stageFocus === "all"
    ? "All stages"
    : WORKFLOW_STAGES.find((item) => item.id === state.stageFocus)?.label || "All stages";
  const view = state.boardView[0].toUpperCase() + state.boardView.slice(1);
  $("#board-view-summary").textContent = `${stage} · ${view}`;
}

function setStageFocus(value) {
  if (!BOARD_STAGE_FOCUSES.includes(value) || value === state.stageFocus) return;
  state.stageFocus = value;
  localStorage.setItem("autogora.boardStageFocus", value);
  if (value === "archive" && !$("#show-archived").checked) {
    $("#show-archived").checked = true;
    localStorage.setItem("autogora.showArchived", "true");
    updateBoardViewControls();
    loadBoard().catch((error) => toast(error.message, true));
    return;
  }
  renderBoard();
}

function setBoardView(value) {
  if (!BOARD_VIEW_MODES.includes(value) || value === state.boardView) return;
  state.boardView = value;
  localStorage.setItem("autogora.boardView", value);
  renderBoard();
  if (value === "graph") loadBoardGraph({ force: true }).catch((error) => toast(error.message, true));
  else cancelGraphLoad();
}

function bindSegmentedControl(selector, buttonSelector, select) {
  const control = $(selector);
  const buttons = $$(buttonSelector, control);
  control.addEventListener("click", (event) => {
    const button = event.target.closest(buttonSelector);
    if (button) select(button);
  });
  control.addEventListener("keydown", (event) => {
    if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
    const current = buttons.indexOf(document.activeElement);
    if (current < 0) return;
    event.preventDefault();
    const next = event.key === "Home" ? 0 : event.key === "End" ? buttons.length - 1
      : (current + (event.key === "ArrowRight" ? 1 : -1) + buttons.length) % buttons.length;
    buttons[next].focus();
    select(buttons[next]);
  });
}

function cancelGraphLoad() {
  state.graphRequest += 1;
  graphLoadController?.abort();
  graphLoadController = null;
  graphResizeObserver?.disconnect();
  graphResizeObserver = null;
  state.graphLoading = false;
}

function boardGraphSignature(graph) {
  const nodes = (graph.nodes || []).map((node) => `${node.task.id}:${node.phase}`).join("|");
  const dependencies = (graph.dependencies || [])
    .map((edge) => `${edge.prerequisiteId}>${edge.dependentId}`).join("|");
  const hierarchy = (graph.hierarchy || [])
    .map((edge) => `${edge.parentTaskId}>${edge.subtaskId}:${edge.position}`).join("|");
  return `${graph.board}:${graph.includeArchived}:${nodes}::${dependencies}::${hierarchy}`;
}

async function loadBoardGraph(options = {}) {
  if (state.boardView !== "graph") return false;
  const board = state.board;
  const includeArchived = $("#show-archived").checked || state.stageFocus === "archive";
  if (!options.force && state.graph && state.graphBoard === board
      && state.graphIncludeArchived === includeArchived) {
    renderBoardGraph();
    return true;
  }
  const requestID = ++state.graphRequest;
  graphLoadController?.abort();
  const controller = new AbortController();
  graphLoadController = controller;
  state.graphLoading = true;
  state.graphError = "";
  renderBoardGraph();
  try {
    const graph = await api(boardPathFor(
      board,
      `/api/graph?includeArchived=${includeArchived}`,
    ), { signal: controller.signal });
    if (controller.signal.aborted || requestID !== state.graphRequest
        || state.board !== board || state.boardView !== "graph") return false;
    const signature = boardGraphSignature(graph);
    state.graphFitPending = !state.graph || state.graphBoard !== board
      || state.graphIncludeArchived !== includeArchived || state.graphSignature !== signature;
    state.graph = graph;
    state.graphBoard = board;
    state.graphIncludeArchived = includeArchived;
    state.graphSignature = signature;
    return true;
  } catch (error) {
    if (error.name === "AbortError" || controller.signal.aborted) return false;
    if (requestID === state.graphRequest && state.board === board) {
      state.graphError = error.message;
    }
    return false;
  } finally {
    if (requestID === state.graphRequest) {
      state.graphLoading = false;
      graphLoadController = null;
      if (state.boardView === "graph") renderBoardGraph();
    }
  }
}

function graphTaskMatches(task) {
  const search = $("#search").value.trim().toLowerCase();
  const tenant = $("#tenant-filter").value;
  const assignee = $("#assignee-filter").value;
  const boardTask = state.tasks.find((item) => item.id === task.id);
  const searchable = `${task.id}\n${task.title}\n${boardTask?.body || ""}`.toLowerCase();
  const stage = WORKFLOW_STAGES.find((item) => item.id === state.stageFocus);
  return (!search || searchable.includes(search))
    && (!tenant || task.tenant === tenant)
    && (!assignee || task.assignee === assignee)
    && (!stage || stage.statuses.includes(task.status));
}

function graphTitleLines(value, width = 29, limit = 2) {
  const words = String(value || "Untitled task").trim().split(/\s+/).filter(Boolean);
  const lines = [];
  for (const word of words) {
    let remaining = word;
    while (remaining.length > width) {
      if (lines.length >= limit) break;
      lines.push(remaining.slice(0, width));
      remaining = remaining.slice(width);
    }
    if (lines.length >= limit) break;
    if (!remaining) continue;
    const current = lines[lines.length - 1];
    if (current && `${current} ${remaining}`.length <= width) {
      lines[lines.length - 1] = `${current} ${remaining}`;
    } else {
      lines.push(remaining);
    }
  }
  if (!lines.length) lines.push("Untitled task");
  const joined = lines.join(" ");
  if (joined.length < String(value || "").trim().length) {
    lines[lines.length - 1] = `${lines[lines.length - 1].slice(0, width - 1)}…`;
  }
  return lines.slice(0, limit);
}

function boardGraphLayout(graph) {
  const nodeWidth = 224;
  const nodeHeight = 92;
  const columnGap = 24;
  const phaseGap = 92;
  const rowGap = 26;
  const padding = 42;
  const headingHeight = 58;
  const nodes = [...(graph.nodes || [])].sort((left, right) => {
    const leftPhase = left.phase < 0 ? Number.MAX_SAFE_INTEGER : left.phase;
    const rightPhase = right.phase < 0 ? Number.MAX_SAFE_INTEGER : right.phase;
    return leftPhase - rightPhase
      || (left.position || 0) - (right.position || 0)
      || String(left.task.createdAt).localeCompare(String(right.task.createdAt))
      || left.task.id.localeCompare(right.task.id);
  });
  const grouped = new Map();
  for (const node of nodes) {
    const phase = Number.isInteger(node.phase) ? node.phase : -1;
    if (!grouped.has(phase)) grouped.set(phase, []);
    grouped.get(phase).push(node);
  }
  const phases = [...grouped.keys()].sort((left, right) => {
    if (left < 0) return 1;
    if (right < 0) return -1;
    return left - right;
  });
  const rowsPerColumn = Math.min(24, Math.max(8, Math.ceil(Math.sqrt(Math.max(1, nodes.length) * 1.5))));
  const positions = new Map();
  const headings = [];
  let x = padding;
  let maximumRows = 1;
  for (const phase of phases) {
    const phaseNodes = grouped.get(phase);
    const subcolumns = Math.max(1, Math.ceil(phaseNodes.length / rowsPerColumn));
    const phaseWidth = subcolumns * nodeWidth + (subcolumns - 1) * columnGap;
    headings.push({
      phase,
      x,
      width: phaseWidth,
      label: phase < 0 ? "Cycle / unresolved" : `Phase ${phase + 1}`,
      count: phaseNodes.length,
    });
    phaseNodes.forEach((node, index) => {
      const subcolumn = Math.floor(index / rowsPerColumn);
      const row = index % rowsPerColumn;
      positions.set(node.task.id, {
        x: x + subcolumn * (nodeWidth + columnGap),
        y: padding + headingHeight + row * (nodeHeight + rowGap),
        node,
      });
      maximumRows = Math.max(maximumRows, row + 1);
    });
    x += phaseWidth + phaseGap;
  }
  const width = Math.max(720, x - phaseGap + padding);
  const height = Math.max(380, padding * 2 + headingHeight + maximumRows * nodeHeight
    + Math.max(0, maximumRows - 1) * rowGap);
  return {
    nodes,
    positions,
    headings,
    width,
    height,
    nodeWidth,
    nodeHeight,
  };
}

function graphDependencyPath(source, target, layout) {
  const sourceX = source.x + layout.nodeWidth;
  const sourceY = source.y + layout.nodeHeight / 2;
  const targetX = target.x;
  const targetY = target.y + layout.nodeHeight / 2;
  const distance = Math.max(44, Math.abs(targetX - sourceX) / 2);
  return `M ${sourceX} ${sourceY} C ${sourceX + distance} ${sourceY}, ${targetX - distance} ${targetY}, ${targetX} ${targetY}`;
}

function graphHierarchyPath(parent, child, layout) {
  const parentX = parent.x + layout.nodeWidth / 2;
  const parentY = parent.y + layout.nodeHeight;
  const childX = child.x + layout.nodeWidth / 2;
  const childY = child.y;
  const middleY = parentY + (childY - parentY) / 2;
  return `M ${parentX} ${parentY} C ${parentX} ${middleY}, ${childX} ${middleY}, ${childX} ${childY}`;
}

function graphNodeSVG(position, layout) {
  const task = position.node.task;
  const lines = graphTitleLines(task.title);
  const route = `${task.assignee || "Unassigned"} · ${task.runtime || "default"}`;
  const phase = position.node.phase < 0 ? "Cycle or unresolved dependency" : `Phase ${position.node.phase + 1}`;
  const dimmed = graphTaskMatches(task) ? "" : " is-dimmed";
  const blocked = position.node.blockedBy?.length
    ? ` · ${position.node.blockedBy.length} open prerequisite${position.node.blockedBy.length === 1 ? "" : "s"}`
    : "";
  const title = `${task.title} · ${STATUS_LABELS[task.status] || task.status} · ${phase}${blocked}`;
  return `<g class="graph-node status-${escapeHtml(task.status)}${dimmed}" transform="translate(${position.x} ${position.y})"
      role="button" tabindex="0" data-graph-task="${escapeHtml(task.id)}"
      aria-label="${escapeHtml(title)}">
    <title>${escapeHtml(title)}</title>
    <rect width="${layout.nodeWidth}" height="${layout.nodeHeight}" rx="11"></rect>
    <circle class="graph-node-status-dot" cx="14" cy="17" r="4"></circle>
    <text class="graph-node-meta" x="25" y="20">${escapeHtml(`${STATUS_LABELS[task.status] || task.status} · ${task.workflowRole || "worker"}`)}</text>
    <text class="graph-node-id" x="${layout.nodeWidth - 12}" y="20" text-anchor="end">${escapeHtml(task.id)}</text>
    ${lines.map((line, index) => `<text class="graph-node-title" x="13" y="${45 + index * 17}">${escapeHtml(line)}</text>`).join("")}
    <text class="graph-node-route" x="13" y="80">${escapeHtml(route)}</text>
  </g>`;
}

function graphAccessibleList(layout) {
  return `<details class="graph-task-list">
    <summary>Accessible task list · ${layout.nodes.length} shown</summary>
    <div>${layout.nodes.map((node) => {
    const task = node.task;
    const phase = node.phase < 0 ? "cycle or unresolved" : `phase ${node.phase + 1}`;
    return `<button type="button" data-graph-task="${escapeHtml(task.id)}">
        <strong>${escapeHtml(task.title)}</strong>
        <span>${escapeHtml(`${STATUS_LABELS[task.status] || task.status} · ${phase} · ${task.assignee || "Unassigned"} · ${task.runtime || "default"}`)}</span>
      </button>`;
  }).join("")}</div>
  </details>`;
}

function renderBoardGraph() {
  const board = $("#board");
  const graph = state.graphBoard === state.board
    && state.graphIncludeArchived === ($("#show-archived").checked || state.stageFocus === "archive")
    ? state.graph
    : null;
  if (!graph) {
    const message = state.graphError
      ? `<strong>Graph could not be loaded</strong><span>${escapeHtml(state.graphError)}</span><button type="button" data-graph-action="refresh">Retry</button>`
      : `<strong>${state.graphLoading ? "Loading task graph…" : "Task graph is not loaded"}</strong><span>Dependencies and hierarchy will appear here.</span>`;
    board.innerHTML = `<section class="graph-empty" role="${state.graphError ? "alert" : "status"}">${message}</section>`;
    board.querySelector('[data-graph-action="refresh"]')?.addEventListener(
      "click",
      () => loadBoardGraph({ force: true }).catch((error) => toast(error.message, true)),
    );
    return;
  }
  const layout = boardGraphLayout(graph);
  if (!layout.nodes.length) {
    board.innerHTML = `<section class="graph-empty" role="status"><strong>No tasks to diagram</strong><span>Create a task or include archived tasks to populate this graph.</span></section>`;
    return;
  }
  const dependencies = state.graphShowDependencies
    ? (graph.dependencies || []).map((edge) => {
      const source = layout.positions.get(edge.prerequisiteId);
      const target = layout.positions.get(edge.dependentId);
      if (!source || !target) return "";
      const marker = edge.satisfiedAt ? "graph-satisfied-arrow" : "graph-dependency-arrow";
      return `<path class="graph-edge graph-dependency${edge.satisfiedAt ? " is-satisfied" : ""}"
        d="${graphDependencyPath(source, target, layout)}" marker-end="url(#${marker})">
        <title>${escapeHtml(`${source.node.task.title} unlocks ${target.node.task.title}${edge.satisfiedAt ? " · satisfied" : ""}`)}</title>
      </path>`;
    }).join("")
    : "";
  const hierarchy = state.graphShowHierarchy
    ? (graph.hierarchy || []).map((edge) => {
      const parent = layout.positions.get(edge.parentTaskId);
      const child = layout.positions.get(edge.subtaskId);
      if (!parent || !child) return "";
      return `<path class="graph-edge graph-hierarchy" d="${graphHierarchyPath(parent, child, layout)}">
        <title>${escapeHtml(`${child.node.task.title} is a subtask of ${parent.node.task.title}`)}</title>
      </path>`;
    }).join("")
    : "";
  const matching = layout.nodes.filter((node) => graphTaskMatches(node.task)).length;
  const truncated = graph.truncated
    ? `<span class="graph-warning" role="status">Showing ${graph.returnedNodes} of ${graph.totalNodes} tasks · ${graph.omittedNodeCount} omitted by the ${graph.nodeLimit}-node limit</span>`
    : "";
  const contextNote = matching === layout.nodes.length
    ? `${matching} tasks match the current filters`
    : `${matching} of ${layout.nodes.length} tasks match · other nodes stay dimmed for dependency context`;
  const headingY = 39;
  board.innerHTML = `<section class="graph-view" aria-labelledby="graph-title">
    <header class="graph-toolbar">
      <div><span class="eyebrow">Board topology · revision ${graph.graphRevision}</span><h2 id="graph-title">Task dependency graph</h2><p>${escapeHtml(contextNote)}</p></div>
      <div class="graph-actions" aria-label="Graph controls">
        <label><input type="checkbox" data-graph-toggle="dependencies"${state.graphShowDependencies ? " checked" : ""}> Dependencies</label>
        <label><input type="checkbox" data-graph-toggle="hierarchy"${state.graphShowHierarchy ? " checked" : ""}> Hierarchy</label>
        <button type="button" class="ghost compact" data-graph-action="out" aria-label="Zoom out">−</button>
        <button type="button" class="ghost compact" data-graph-action="fit">Fit</button>
        <output class="graph-zoom" data-graph-zoom aria-live="polite">${Math.round(state.graphZoom * 100)}%</output>
        <button type="button" class="ghost compact" data-graph-action="in" aria-label="Zoom in">+</button>
        <button type="button" class="ghost compact" data-graph-action="refresh">Refresh</button>
      </div>
    </header>
    <div class="graph-legend" aria-label="Graph legend">
      <span><i class="dependency"></i> Prerequisite → dependent</span>
      <span><i class="dependency satisfied"></i> Satisfied dependency</span>
      <span><i class="hierarchy"></i> Parent / subtask</span>
      ${truncated}
      ${state.graphLoading ? '<span class="graph-refreshing" role="status">Refreshing…</span>' : ""}
    </div>
    <div class="graph-viewport" tabindex="0" aria-label="Scrollable task graph. Drag the background to pan.">
      <svg class="graph-canvas" width="${layout.width}" height="${layout.height}" viewBox="0 0 ${layout.width} ${layout.height}"
        role="img" aria-labelledby="graph-title graph-description">
        <desc id="graph-description">Tasks are arranged by dependency phase. Solid arrows point from prerequisites to dependents. Dashed lines show parent and subtask hierarchy.</desc>
        <defs>
          <marker id="graph-dependency-arrow" markerWidth="9" markerHeight="7" refX="8" refY="3.5" orient="auto" markerUnits="strokeWidth">
            <path d="M 0 0 L 9 3.5 L 0 7 z"></path>
          </marker>
          <marker id="graph-satisfied-arrow" markerWidth="9" markerHeight="7" refX="8" refY="3.5" orient="auto" markerUnits="strokeWidth">
            <path d="M 0 0 L 9 3.5 L 0 7 z"></path>
          </marker>
        </defs>
        ${layout.headings.map((heading) => `<g class="graph-phase-heading">
          <text x="${heading.x}" y="${headingY}">${escapeHtml(heading.label)}</text>
          <text class="graph-phase-count" x="${heading.x + heading.width}" y="${headingY}" text-anchor="end">${heading.count} ${heading.count === 1 ? "task" : "tasks"}</text>
          <line x1="${heading.x}" x2="${heading.x + heading.width}" y1="${headingY + 12}" y2="${headingY + 12}"></line>
        </g>`).join("")}
        <g class="graph-edges" aria-hidden="true">${hierarchy}${dependencies}</g>
        <g class="graph-nodes">${layout.nodes.map((node) => graphNodeSVG(layout.positions.get(node.task.id), layout)).join("")}</g>
      </svg>
    </div>
    ${graphAccessibleList(layout)}
  </section>`;
  bindBoardGraph(layout);
}

function applyGraphZoom(layout) {
  const canvas = $(".graph-canvas");
  if (!canvas) return;
  canvas.style.width = `${Math.round(layout.width * state.graphZoom)}px`;
  canvas.style.height = `${Math.round(layout.height * state.graphZoom)}px`;
  const output = $("[data-graph-zoom]");
  if (output) output.textContent = `${Math.round(state.graphZoom * 100)}%`;
}

function bindBoardGraph(layout) {
  graphResizeObserver?.disconnect();
  graphResizeObserver = null;
  const viewport = $(".graph-viewport");
  const zoom = (next, keepFit = false) => {
    state.graphFitMode = keepFit;
    state.graphZoom = Math.min(1.8, Math.max(0.18, next));
    applyGraphZoom(layout);
  };
  const fit = (behavior = "auto") => {
    zoom(Math.min(1, (viewport.clientWidth - 24) / layout.width), true);
    viewport.scrollTo({ left: 0, top: 0, behavior });
  };
  $('[data-graph-action="refresh"]')?.addEventListener(
    "click",
    () => loadBoardGraph({ force: true }).catch((error) => toast(error.message, true)),
  );
  $('[data-graph-action="in"]')?.addEventListener("click", () => zoom(state.graphZoom + 0.15));
  $('[data-graph-action="out"]')?.addEventListener("click", () => zoom(state.graphZoom - 0.15));
  $('[data-graph-action="fit"]')?.addEventListener("click", () => fit("smooth"));
  $$("[data-graph-toggle]").forEach((toggle) => toggle.addEventListener("change", () => {
    if (toggle.dataset.graphToggle === "dependencies") state.graphShowDependencies = toggle.checked;
    else state.graphShowHierarchy = toggle.checked;
    renderBoardGraph();
  }));
  $$("[data-graph-task]").forEach((element) => {
    element.addEventListener("click", () => openDrawer(element.dataset.graphTask));
    if (element.tagName.toLowerCase() === "button") return;
    element.addEventListener("keydown", (event) => {
      if (event.key !== "Enter" && event.key !== " ") return;
      event.preventDefault();
      openDrawer(element.dataset.graphTask);
    });
  });
  let pan = null;
  viewport.addEventListener("pointerdown", (event) => {
    if (event.button !== 0 || event.target.closest("[data-graph-task]")) return;
    pan = {
      x: event.clientX,
      y: event.clientY,
      left: viewport.scrollLeft,
      top: viewport.scrollTop,
      pointer: event.pointerId,
    };
    viewport.setPointerCapture(event.pointerId);
    viewport.classList.add("is-panning");
  });
  viewport.addEventListener("pointermove", (event) => {
    if (!pan || event.pointerId !== pan.pointer) return;
    viewport.scrollLeft = pan.left - (event.clientX - pan.x);
    viewport.scrollTop = pan.top - (event.clientY - pan.y);
  });
  const stopPan = (event) => {
    if (!pan || event.pointerId !== pan.pointer) return;
    pan = null;
    viewport.classList.remove("is-panning");
  };
  viewport.addEventListener("pointerup", stopPan);
  viewport.addEventListener("pointercancel", stopPan);
  applyGraphZoom(layout);
  if (state.graphFitPending || state.graphFitMode) {
    const signature = state.graphSignature;
    state.graphFitPending = false;
    requestAnimationFrame(() => {
      if (state.boardView !== "graph" || state.graphSignature !== signature || !viewport.isConnected) return;
      fit();
    });
  }
  if ("ResizeObserver" in window) {
    let previousWidth = viewport.clientWidth;
    graphResizeObserver = new ResizeObserver(() => {
      const width = viewport.clientWidth;
      if (!state.graphFitMode || width === previousWidth) return;
      previousWidth = width;
      fit();
    });
    graphResizeObserver.observe(viewport);
  }
}

function renderBoard() {
  const tasks = filteredTasks();
  const board = $("#board");
  board.dataset.view = state.boardView;
  board.dataset.focus = state.stageFocus;
  if (state.boardView === "graph") {
    updateBoardViewControls();
    renderBoardGraph();
    renderBulk();
    return;
  }
  const showArchived = $("#show-archived").checked || state.stageFocus === "archive";
  const stages = WORKFLOW_STAGES
    .filter((stage) => stage.id !== "archive" || showArchived)
    .filter((stage) => state.stageFocus === "all" || stage.id === state.stageFocus);
  updateBoardViewControls();
  const emptyGuide = state.tasks.length === 0 ? `<section class="empty-guide" aria-label="Get started">
    <div class="empty-guide-intro"><span class="eyebrow">New board</span><h2>Set up a project, then add work</h2><p>Autogora can plan Triage cards, promote dependency-ready tasks, and assign healthy workers after these three checks.</p></div>
    <div class="guide-step"><strong>1 · Coding agents</strong><span>${availableWorkerAgents().length ? `${availableWorkerAgents().length} worker agent${availableWorkerAgents().length === 1 ? "" : "s"} configured.` : "Choose installed CLIs, models, and fallbacks."}</span><button type="button" data-guide="agents">${availableWorkerAgents().length ? "Review agents" : "Set up agents"}</button></div>
    <div class="guide-step"><strong>2 · Project directory</strong><span>${state.metadata?.defaultWorkdir ? escapeHtml(state.metadata.defaultWorkdir) : "Choose the Git repository workers may inspect or change."}</span><button type="button" data-guide="workspace">${state.metadata?.defaultWorkdir ? "Review project" : "Set project path"}</button></div>
    <div class="guide-step"><strong>3 · Add work</strong><span>Import GitHub issues for review or create a task directly.</span><button type="button" data-guide="import" class="primary">Import issues</button><button type="button" data-guide="create" class="ghost">Create task</button></div>
  </section>` : "";
  board.innerHTML = emptyGuide + stages.map((stage) => {
    const stageCount = stage.statuses.reduce((total, status) =>
      total + tasks.filter((task) => task.status === status).length, 0);
    const columns = stage.statuses.map((status) => {
      const cards = tasks.filter((task) => task.status === status);
      return `<section class="column status-${status}" data-status="${status}">
        <header class="column-head"><span class="status-dot"></span><h3>${STATUS_LABELS[status]}</h3><span class="count">${cards.length}</span>${status === "running" ? "" : `<button class="icon-button compact" data-create-status="${status}" aria-label="Create in ${STATUS_LABELS[status]}" title="Create in ${STATUS_LABELS[status]}">+</button>`}</header>
        <div class="column-body" role="region" aria-label="${STATUS_LABELS[status]} tasks" tabindex="0">${renderCardList(cards, status === "running" && $("#lane-profile").checked)}</div>
      </section>`;
    }).join("");
    return `<section class="board-stage" data-stage="${stage.id}" aria-label="${stage.ariaLabel}">
      <header class="board-stage-head"><h2>${stage.label}</h2><span>${stageCount} ${stageCount === 1 ? "task" : "tasks"}</span></header>
      <div class="board-stage-grid">${columns}</div>
    </section>`;
  }).join("");
  $$('[data-guide]').forEach((button) => button.addEventListener("click", () => {
    if (button.dataset.guide === "agents") openAgentSettings().catch((error) => toast(error.message, true));
    if (button.dataset.guide === "workspace") openSettings();
    if (button.dataset.guide === "import") openGitHubImport();
    if (button.dataset.guide === "create") openTaskDialog("triage");
  }));
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
      event.dataTransfer.setData("application/x-autogora-updated-at", card.dataset.taskVersion);
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
      const expectedUpdatedAt = event.dataTransfer.getData("application/x-autogora-updated-at");
      if (taskId) await moveTask(taskId, column.dataset.status, expectedUpdatedAt);
    });
  });
  $$('[data-create-status]').forEach((button) => button.addEventListener("click", () => openTaskDialog(button.dataset.createStatus)));
  const trash = $("#trash-drop");
  trash.ondragover = (event) => { event.preventDefault(); trash.classList.add("drag-over"); };
  trash.ondragleave = () => trash.classList.remove("drag-over");
  trash.ondrop = async (event) => {
    event.preventDefault(); trash.classList.remove("drag-over"); document.body.classList.remove("drag-active");
    const taskId = event.dataTransfer.getData("text/plain");
    const expectedUpdatedAt = event.dataTransfer.getData("application/x-autogora-updated-at");
    if (!taskId || !confirm(`Permanently delete ${taskId}?`)) return;
    try {
      await api(boardPath(`/api/tasks/${taskId}`), {
        method: "DELETE", body: JSON.stringify({ expectedUpdatedAt: expectedUpdatedAt || null }),
      });
      await loadBoard();
    }
    catch (error) { toast(error.message, true); }
  };
}

async function moveTask(taskId, status, expectedUpdatedAt = null) {
  try {
    expectedUpdatedAt ||= state.tasks.find((task) => task.id === taskId)?.updatedAt || null;
    if (status === "running") {
      await api(boardPath("/api/dispatch"), { method: "POST", body: JSON.stringify({ taskId, expectedUpdatedAt }) });
      toast("Task sent to the dispatcher");
      await loadBoard();
      return;
    }
    const body = { status, expectedUpdatedAt };
    if (status === "scheduled") {
      const at = prompt("Run after (local date and time):", localDateTimeValue());
      if (at === null) return;
      body.scheduledAt = futureScheduleISO(at);
    } else body.scheduledAt = null;
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
    if (mutation.status === "scheduled" && !mutation.scheduledAt) {
      const at = prompt("Run selected tasks after (local date and time):", localDateTimeValue());
      if (at === null) return;
      mutation = { ...mutation, scheduledAt: futureScheduleISO(at) };
    } else if (mutation.status) mutation = { ...mutation, scheduledAt: null };
    const ids = [...state.selected];
    const expectedUpdatedAt = Object.fromEntries(ids.map((id) => [
      id, state.tasks.find((task) => task.id === id)?.updatedAt,
    ]).filter(([, version]) => version));
    const result = await api(boardPath("/api/tasks/bulk"), {
      method: "POST", body: JSON.stringify({ ids, mutation: { ...mutation, expectedUpdatedAt } }),
    });
    state.selected = new Set(result.errors.map((item) => item.id));
    const failure = result.errors[0]?.error;
    toast(`${result.ok.length} updated${result.errors.length ? `, ${result.errors.length} failed${failure ? `: ${failure}` : ""}` : ""}`, result.errors.length > 0);
    await loadBoard();
  } catch (error) { toast(error.message, true); }
  finally { $("#bulk-status").value = ""; }
}

function openTaskDialog(status = "todo") {
  const form = $("#task-form");
  form.reset();
  form.elements.status.value = status;
  const profiles = workerProfiles();
  form.elements.profile.innerHTML = `<option value="">Custom assignment</option>${profiles.map((profile) =>
    `<option value="${escapeHtml(profile.name)}">${escapeHtml(profile.name)} · ${escapeHtml(profile.runtime)} · ${escapeHtml(profile.model || "CLI default")}</option>`).join("")}`;
  const preferred = ["todo", "ready", "scheduled"].includes(status) ? defaultWorkerProfile() : null;
  if (preferred) form.elements.profile.value = preferred.name;
  if (status === "scheduled") form.elements.scheduledAt.value = localDateTimeValue();
  updateTaskScheduleVisibility();
  updateTaskModelPreview();
  $("#task-dialog").showModal();
}

function updateTaskScheduleVisibility() {
  const form = $("#task-form");
  const scheduled = form.elements.status.value === "scheduled";
  $("#task-schedule-field").classList.toggle("hidden", !scheduled);
  form.elements.scheduledAt.required = scheduled;
  if (scheduled && !form.elements.scheduledAt.value) form.elements.scheduledAt.value = localDateTimeValue();
}

function updateTaskModelPreview() {
  applyAuthoritativeRouteControls(taskDialogRouteControls());
}

async function openDrawer(taskId, { focus = true, force = false } = {}) {
  if (!force && state.drawerDirty && state.drawerTask === taskId) {
    $("#drawer-refresh").classList.remove("hidden");
    return;
  }
  if (!force && state.drawerDirty && state.drawerTask && state.drawerTask !== taskId
      && !confirm("Discard unsaved task changes?")) return;
  const requestID = ++state.drawerRequest;
  try {
    if (!state.drawerTask) drawerReturnFocus = document.activeElement;
    const detail = await api(boardPath(`/api/tasks/${taskId}`));
    if (requestID !== state.drawerRequest) return;
    if (!force && state.drawerTask === taskId && state.drawerDirty) {
      $("#drawer-refresh").classList.remove("hidden");
      return;
    }
    state.drawerTask = taskId;
    state.drawerDirty = false;
    state.drawerVersion = detail.task.updatedAt;
    $("#drawer-refresh").classList.add("hidden");
    $("#drawer-id").textContent = taskId;
    $("#drawer-status").textContent = STATUS_LABELS[detail.task.status];
    $("#drawer-status").className = `status-chip status-${detail.task.status}`;
    renderDrawer(detail);
    const drawer = $("#drawer");
    if (!drawer.open) drawer.showModal();
    document.body.classList.add("drawer-open");
    drawer.classList.add("open");
    if (focus) $("#drawer-close").focus({ preventScroll: true });
  } catch (error) {
    if (requestID === state.drawerRequest) toast(error.message, true);
  }
}

function closeDrawer() {
  state.drawerRequest++;
  state.drawerTask = null;
  state.drawerDirty = false;
  state.drawerVersion = null;
  const drawer = $("#drawer");
  drawer.classList.remove("open");
  if (drawer.open) drawer.close();
  document.body.classList.remove("drawer-open");
  if (drawerReturnFocus?.isConnected) drawerReturnFocus.focus({ preventScroll: true });
  drawerReturnFocus = null;
}

function taskOptions(excludeId) {
  return state.tasks.filter((task) => task.id !== excludeId && task.status !== "archived")
    .map((task) => `<option value="${escapeHtml(task.id)}">${escapeHtml(task.id)} · ${escapeHtml(task.title)}</option>`).join("");
}

const drawerEditSelectors = [
  "#edit-title", "#edit-profile", "#edit-assignee", "#edit-runtime", "#edit-priority", "#edit-tenant", "#edit-workspace-kind",
  "#edit-workspace", "#edit-branch", "#edit-scheduled-at", "#edit-max-runtime", "#edit-max-retries", "#edit-body", "#edit-skills",
  "#edit-goal-mode", "#edit-goal-turns",
];
const drawerRunningLockedSelectors = drawerEditSelectors.filter((selector) => selector !== "#edit-priority");

function renderDrawer(detail) {
  const task = detail.task;
  const editLocked = task.status === "running";
  const runAgents = new Map((detail.runAgentConfigs || []).map((config) => [config.runId, config]));
  const runRows = detail.runs.slice().reverse().map((run) => {
    const config = runAgents.get(run.id);
    const route = config ? `${config.profile} · ${config.runtime} · ${config.model || "CLI default (unpinned)"}${config.provider ? ` · ${config.provider}` : ""}` : "";
    const provenance = config ? `${String(config.source || "unknown").replaceAll("_", " ")}${config.fallbackFrom ? ` · fallback from ${config.fallbackFrom}` : ""}` : "";
    return `<div class="detail-row">
      ${run.status === "running" ? `<button data-terminate-run="${escapeHtml(run.id)}" class="danger compact">Terminate</button>` : ""}
      ${run.logPath ? `<button data-run-log="${escapeHtml(run.id)}" class="ghost compact">Log tail</button>` : ""}
      <strong>${escapeHtml(run.workerId)}</strong>
      <span class="detail-status">${escapeHtml(run.status)}</span>
      <span class="mono">${escapeHtml(run.id)} · claimed ${relativeTime(run.claimedAt)} · heartbeat ${relativeTime(run.heartbeatAt)} · lease ${escapeHtml(new Date(run.claimExpiresAt).toLocaleString())}</span>
      ${config ? `<div>${escapeHtml(route)}</div><div class="mono">${escapeHtml(provenance)}</div>` : ""}
      ${run.summary ? `<div>${escapeHtml(run.summary)}</div>` : ""}${run.error ? `<div>${escapeHtml(run.error)}</div>` : ""}
    </div>`;
  }).join("");
  const comments = detail.comments.map((comment) => `<article class="detail-row comment-row">
    <header><strong>${escapeHtml(comment.author)}</strong><time class="mono" datetime="${escapeHtml(comment.createdAt)}">${escapeHtml(comment.createdAt)}</time></header>
    <div class="markdown comment-body">${markdown(comment.body)}</div>
  </article>`).join("");
  const attachments = detail.attachments.map((attachment) => `<div class="detail-row">
    <button class="icon-button compact" data-remove-attachment="${escapeHtml(attachment.id)}" aria-label="Remove ${escapeHtml(attachment.name)}" title="Remove attachment">×</button>
    <strong>${escapeHtml(attachment.name)}</strong>
    ${attachment.path ? `<a href="${boardPath(`/api/attachments/${attachment.id}/download?taskId=${task.id}`)}">Download</a>` : `<a href="${escapeHtml(attachment.url)}" target="_blank" rel="noopener noreferrer">Open URL</a>`}
  </div>`).join("");
  const events = detail.events.slice().reverse().slice(0, 30).map((event) => {
    const payload = event.payload && Object.keys(event.payload).length ? JSON.stringify(event.payload, null, 2) : "";
    return `<div class="detail-row"><strong>${escapeHtml(event.kind)}</strong><span class="mono">#${event.id} · ${escapeHtml(event.createdAt)}</span>${payload ? `<div class="event-payload">${escapeHtml(payload)}</div>` : ""}</div>`;
  }).join("");
  const changeSets = (detail.changeSets || []).slice().reverse().map((change) => `<div class="detail-row">
    <strong>${escapeHtml(change.state)} change set · ${escapeHtml(change.id)}</strong>
    <span class="mono">${escapeHtml(change.headCommit)} · ${escapeHtml(change.durableRef)}</span>
    <div>${change.changedFiles?.length ? escapeHtml(change.changedFiles.join(", ")) : "No changed files recorded"}</div>
    <div class="mono">${escapeHtml(change.worktreePath)}</div>
  </div>`).join("");
  const workspaces = (detail.runWorkspaces || []).slice().reverse().map((workspace) => `<div class="detail-row"><strong>${escapeHtml(workspace.kind)} workspace</strong><span class="mono">${escapeHtml(workspace.runId)} · ${escapeHtml(workspace.path)}</span>${workspace.baseCommit ? `<div>Base ${escapeHtml(workspace.baseCommit)}</div>` : ""}</div>`).join("");
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
  const selectedRoute = authoritativeTaskRoute("", task.assignee, task.runtime);
  const selectedProfile = selectedRoute.profile;
  const drawerProfileOptions = `<option value="">Custom assignment</option>${workerProfiles().map((profile) =>
    `<option value="${escapeHtml(profile.name)}"${selectedProfile?.name === profile.name ? " selected" : ""}>${escapeHtml(profile.name)} · ${escapeHtml(profile.runtime)}</option>`).join("")}`;
  const routeModel = taskRoutePreview(selectedRoute);
  $("#drawer-content").innerHTML = `
    <div class="drawer-title-block"><span class="eyebrow">Task</span><h1>${escapeHtml(task.title)}</h1></div>
    <div class="task-context">
      <div class="task-context-owner ${task.assignee ? "" : "unassigned"}">
        <span class="avatar" aria-hidden="true">${escapeHtml(initials(task.assignee))}</span>
        <span><small>Owner</small><strong>${escapeHtml(task.assignee || "Unassigned")}</strong></span>
      </div>
      <div><small>Effective runtime</small><strong>${escapeHtml(selectedRoute.runtime)}</strong></div>
      <div><small>Last updated</small><strong>${relativeTime(task.updatedAt)}</strong></div>
    </div>
    ${task.status === "blocked" ? `<div class="detail-row"><strong>Blocked · ${escapeHtml(task.blockKind || "needs_input")}</strong><div>${escapeHtml(task.blockReason || "No reason recorded")}</div></div>` : ""}
    ${task.result ? `<div class="detail-row"><strong>Result</strong><div class="markdown">${markdown(task.result)}</div></div>` : ""}
    ${editLocked ? '<div class="detail-row" role="note"><strong>Execution settings are locked while this task is running.</strong><div>Priority remains editable. Use comments for durable context, or terminate the run before changing the task specification.</div></div>' : ""}
    <label>Edit title<input id="edit-title" value="${escapeHtml(task.title)}"></label>
    <div class="drawer-grid drawer-routing-grid">
      <label>Board profile<select id="edit-profile">${drawerProfileOptions}</select></label>
      <label>Assignee<input id="edit-assignee" value="${escapeHtml(task.assignee || "")}"></label>
      <label>Runtime<select id="edit-runtime">${["manual", "codex", "claude", "cline", "gemini"].map((item) => `<option ${item === selectedRoute.runtime ? "selected" : ""}>${item}</option>`).join("")}</select></label>
      <label>Current route model<input id="edit-model-preview" value="${escapeHtml(routeModel)}" readonly></label>
      <label>Priority<input id="edit-priority" type="number" value="${task.priority}"></label>
      <label>Tenant<input id="edit-tenant" value="${escapeHtml(task.tenant || "")}"></label>
      <label>Workspace kind<select id="edit-workspace-kind">${["scratch", "dir", "worktree"].map((item) => `<option ${item === task.workspaceKind ? "selected" : ""}>${item}</option>`).join("")}</select></label>
      <label>Workspace path<input id="edit-workspace" value="${escapeHtml(task.workspace || "")}" placeholder="automatic"></label>
      <label>Branch<input id="edit-branch" value="${escapeHtml(task.branch || "")}"></label>
      <label>Run after<input id="edit-scheduled-at" type="datetime-local" value="${task.scheduledAt ? localDateTimeValue(task.scheduledAt) : ""}"></label>
      <label>Max runtime (seconds)<input id="edit-max-runtime" type="number" min="1" value="${task.maxRuntimeSeconds || ""}"></label>
      <label>Max retries<input id="edit-max-retries" type="number" min="1" value="${task.maxRetries || 2}"></label>
    </div>
    <label>Description<textarea id="edit-body" rows="9">${escapeHtml(task.body)}</textarea></label>
    <label>Skills<input id="edit-skills" value="${escapeHtml((task.skills || []).join(", "))}" placeholder="comma-separated"></label>
    <div class="drawer-grid"><label class="inline"><input id="edit-goal-mode" type="checkbox"${task.goalMode ? " checked" : ""}> Goal mode</label><label>Goal max turns<input id="edit-goal-turns" type="number" min="1" value="${task.goalMaxTurns || 20}"></label></div>
    <button id="save-task" class="primary">${editLocked ? "Save priority" : "Save changes"}</button>
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
    <form id="comment-form" class="comment-form">
      <label class="sr-only" for="comment-body">Comment</label>
      <textarea id="comment-body" name="comment" rows="3" required placeholder="Add durable context…"></textarea>
      <button>Comment</button>
    </form>
    <h3>Attachments</h3><div class="detail-list">${attachments || '<small>No attachments</small>'}</div>
    <form id="attachment-form" class="attachment-form"><input type="file" multiple required><button>Upload</button></form>
    <h3>Run history</h3><div class="detail-list">${runRows || '<small>No runs</small>'}</div>
    <h3>Change results</h3><div class="detail-list">${changeSets || '<small>No change sets</small>'}</div>
    <h3>Run workspaces</h3><div class="detail-list">${workspaces || '<small>No prepared workspaces</small>'}</div>
    <h3>Recent events</h3><div class="detail-list">${events}</div>`;
  if (editLocked) {
    drawerRunningLockedSelectors.forEach((selector) => {
      const control = $(selector);
      if (control) control.disabled = true;
    });
  }
  bindDrawer(detail);
}

function bindDrawer(detail) {
  const taskId = detail.task.id;
  const markDirty = () => {
    state.drawerDirty = true;
  };
  drawerEditSelectors.forEach((selector) => $(selector)?.addEventListener("input", markDirty));
  const routeControls = () => ({
    profile: $("#edit-profile"), assignee: $("#edit-assignee"),
    runtime: $("#edit-runtime"), model: $("#edit-model-preview"),
  });
  $("#edit-profile").addEventListener("change", () => {
    const controls = routeControls();
    if (controls.profile.value) applyAuthoritativeRouteControls(controls);
    else switchRouteControlsToCustom(controls);
  });
  $("#edit-assignee").addEventListener("input", () => {
    const controls = routeControls();
    controls.profile.value = "";
    applyAuthoritativeRouteControls(controls);
  });
  $("#edit-runtime").addEventListener("change", () => {
    switchRouteControlsToCustom(routeControls());
  });
  $("#save-task").addEventListener("click", async () => {
    try {
      const scheduleValue = $("#edit-scheduled-at").value;
      if (detail.task.status === "scheduled") futureScheduleISO(scheduleValue);
      const route = applyAuthoritativeRouteControls(routeControls());
      const payload = detail.task.status === "running"
        ? { expectedUpdatedAt: state.drawerVersion, priority: Number($("#edit-priority").value) }
        : {
          expectedUpdatedAt: state.drawerVersion,
          title: $("#edit-title").value, body: $("#edit-body").value,
          assignee: route.assignee, runtime: route.runtime,
          priority: Number($("#edit-priority").value),
          tenant: $("#edit-tenant").value || null, workspaceKind: $("#edit-workspace-kind").value,
          workspace: $("#edit-workspace").value || null, branch: $("#edit-branch").value || null,
          scheduledAt: scheduleValue ? new Date(scheduleValue).toISOString() : null,
          maxRuntimeSeconds: $("#edit-max-runtime").value ? Number($("#edit-max-runtime").value) : null,
          maxRetries: Number($("#edit-max-retries").value) || 2,
          skills: $("#edit-skills").value.split(",").map((item) => item.trim()).filter(Boolean),
          goalMode: $("#edit-goal-mode").checked, goalMaxTurns: Number($("#edit-goal-turns").value) || 20,
        };
      await api(boardPath(`/api/tasks/${taskId}`), { method: "PATCH", body: JSON.stringify(payload) });
      state.drawerDirty = false;
      toast("Task saved"); await loadBoard(); await openDrawer(taskId, { focus: false });
    } catch (error) {
      toast(error.message, true);
      if (error.message.includes("conflict")) $("#drawer-refresh").classList.remove("hidden");
    }
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
    if (termination.pending && termination.signaled) toast("Termination signal sent; the task will be released after the worker exits.");
    else if (termination.pending) toast("Termination recorded; the dispatcher will verify the process and workspace before releasing the task.");
    else toast("Run reclaimed.");
    await openDrawer(taskId); await loadBoard();
  }));
  $$('[data-run-log]', $("#drawer-content")).forEach((button) => button.addEventListener("click", async () => {
    try {
      button.disabled = true;
      const log = await api(boardPath(`/api/tasks/${taskId}/log?runId=${encodeURIComponent(button.dataset.runLog)}&tailBytes=32768`));
      let output = $(".run-log-output", button.closest(".detail-row"));
      if (!output) {
        output = document.createElement("pre");
        output.className = "run-log-output";
        button.closest(".detail-row").append(output);
      }
      output.textContent = log.text || "(log is empty)";
    } catch (error) { toast(error.message, true); }
    finally { button.disabled = false; }
  }));
  $$('[data-remove-attachment]', $("#drawer-content")).forEach((button) => button.addEventListener("click", async () => {
    await api(boardPath(`/api/tasks/${taskId}/attachments/${button.dataset.removeAttachment}`), { method: "DELETE" });
    await openDrawer(taskId);
  }));
  $("#comment-form").addEventListener("submit", async (event) => {
    event.preventDefault(); const input = $("#comment-body", event.currentTarget);
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
  const actionButtons = $$('[data-action]', $("#drawer-content"));
  const expectedUpdatedAt = state.drawerVersion;
  try {
    actionButtons.forEach((button) => { button.disabled = true; });
    if (action === "delete") {
      if (!confirm("Permanently delete this task?")) return;
      await api(boardPath(`/api/tasks/${taskId}`), {
        method: "DELETE", body: JSON.stringify({ expectedUpdatedAt }),
      });
      closeDrawer();
    } else if (action === "complete") {
      const summary = prompt("Completion summary:"); if (!summary) return;
      await api(boardPath(`/api/tasks/${taskId}/complete`), {
        method: "POST", body: JSON.stringify({ summary, expectedUpdatedAt }),
      });
    } else if (action === "block") {
      const reason = prompt("Block reason:"); if (!reason) return;
      await api(boardPath(`/api/tasks/${taskId}/block`), {
        method: "POST", body: JSON.stringify({ reason, kind: "needs_input", expectedUpdatedAt }),
      });
    } else if (action === "specify" || action === "decompose") {
      if (!confirm(`${action} this triage card using the board planner?`)) return;
      await api(boardPath(`/api/tasks/${taskId}/${action}`), {
        method: "POST", body: JSON.stringify({ expectedUpdatedAt }),
      });
    } else if (action === "dispatch") {
      const dispatch = await api(boardPath("/api/dispatch"), {
        method: "POST", body: JSON.stringify({ taskId, expectedUpdatedAt }),
      });
      toast(dispatch.mode === "supervisor" ? "Supervisor is watching this Ready task" : `Dispatcher operation ${dispatch.operation?.id || "started"}`);
    } else if (action === "archive") {
      if (!confirm("Archive this task?")) return;
      await api(boardPath(`/api/tasks/${taskId}/archive`), {
        method: "POST", body: JSON.stringify({ expectedUpdatedAt }),
      });
      closeDrawer();
    } else {
      await api(boardPath(`/api/tasks/${taskId}/${action}`), {
        method: "POST", body: JSON.stringify({ expectedUpdatedAt }),
      });
    }
    await loadBoard(); if (state.drawerTask) await openDrawer(taskId);
  } catch (error) {
    toast(error.message, true);
    if (error.message.includes("conflict")) $("#drawer-refresh").classList.remove("hidden");
  }
  finally { actionButtons.forEach((button) => { if (button.isConnected) button.disabled = false; }); }
}

function parseRoute(value) {
  const [name, runtime = "codex", ...description] = value.trim().split(":");
  if (!name || !["codex", "claude", "cline", "gemini"].includes(runtime)) throw new Error(`Invalid route: ${value}`);
  return { name, runtime, description: description.join(":") };
}

function openGitHubImport() {
  const form = $("#github-form");
  form.reset();
  form.elements.limit.value = "30";
  form.elements.priority.value = "0";
  $("#github-result").classList.add("hidden");
  $("#github-result").innerHTML = "";
  $("#github-dialog").showModal();
}

function githubImportPayload(dryRun) {
  const data = new FormData($("#github-form"));
  return {
    repository: String(data.get("repository") || "").trim(), host: String(data.get("host") || "").trim(),
    state: data.get("state"), labels: String(data.get("labels") || "").trim(), search: String(data.get("search") || "").trim(),
    issues: String(data.get("issues") || "").trim(), limit: Number(data.get("limit")) || 30,
    tenant: String(data.get("tenant") || "").trim() || null, priority: Number(data.get("priority")) || 0, dryRun,
  };
}

function renderGitHubResult(result) {
  const target = $("#github-result");
  const issues = result.issues || [];
  const failures = result.errors || [];
  target.innerHTML = `<div class="import-summary">
    <span class="pill">${escapeHtml(result.status || "success")}</span>
    <span class="pill">${result.fetched || 0} fetched</span>
    <span class="pill">${result.dryRun ? `${result.planned || 0} planned` : `${result.created || 0} created`}</span>
    <span class="pill">${result.existing || 0} existing</span>
    ${result.failed ? `<span class="pill priority">${result.failed} failed</span>` : ""}
  </div>
  <div class="mono">${escapeHtml(result.repository || "Current board repository")}</div>
  <div class="import-issues">${issues.map((issue) => `<div class="import-issue"><strong>#${issue.number}</strong><span>${escapeHtml(issue.title)}</span><span class="mono">${escapeHtml(issue.action)}</span></div>`).join("")}
  ${failures.map((failure) => `<div class="import-issue"><strong>#${failure.number || "?"}</strong><span>${escapeHtml(failure.error)}</span><span class="mono">failed</span></div>`).join("")}</div>`;
  target.classList.remove("hidden");
}

async function runGitHubImport(dryRun) {
  const preview = $("#github-preview");
  const submit = $("#github-submit");
  try {
    preview.disabled = true; submit.disabled = true;
    const result = await api(boardPath("/api/github/import"), { method: "POST", body: JSON.stringify(githubImportPayload(dryRun)) });
    renderGitHubResult(result);
    if (!dryRun) {
      await loadBoard();
      toast(result.failed ? `Import finished with ${result.failed} failure(s)` : `${result.created} issue(s) added to Triage`, result.failed > 0);
    }
  } catch (error) { toast(error.message, true); }
  finally { preview.disabled = false; submit.disabled = false; }
}

async function submitGitHubImport(event) {
  event.preventDefault();
  await runGitHubImport(false);
}

function renderAutomationChip() {
  const chip = $("#automation");
  const supervisor = state.supervisor || {};
  const presentation = supervisorPresentation(supervisor);
  const workers = availableWorkerAgents();
  chip.classList.remove("running", "stopped", "failed");
  if (!state.agentConfigExists || !workers.length) {
    chip.textContent = "Setup incomplete";
    chip.classList.add("stopped");
    chip.title = "Configure at least one enabled worker agent";
  } else if (presentation.status === "running") {
    chip.textContent = `Automation running · ${supervisor.allowWrites ? "write" : "read-only"}`;
    chip.classList.add("running");
    chip.title = `${workers.length} configured worker agent(s) · ${presentation.detail}; click for activity`;
  } else if (presentation.status === "restarting") {
    chip.textContent = `Automation ${presentation.label.toLowerCase()}`;
    chip.classList.add("failed");
    chip.title = `${supervisor.lastError || "Supervisor exited"} · ${presentation.detail}`;
  } else if (presentation.status === "starting") {
    chip.textContent = "Automation starting";
    chip.classList.add("stopped");
    chip.title = presentation.detail;
  } else if (presentation.status === "failed") {
    chip.textContent = "Automation failed";
    chip.classList.add("failed");
    chip.title = supervisor.lastError;
  } else {
    chip.textContent = "Automation stopped";
    chip.classList.add("stopped");
    chip.title = "Click to inspect or start automatic orchestration";
  }
}

async function refreshOperationalStatus() {
  const board = state.board;
  const generation = ++operationalStatusGeneration;
  const requests = await Promise.allSettled([
    api("/api/supervisor"), api(boardPathFor(board, "/api/agents/effective")), api(boardPathFor(board, "/api/operations")),
  ]);
  if (state.board !== board || operationalStatusGeneration !== generation) return;
  if (requests[0].status === "fulfilled") state.supervisor = requests[0].value;
  if (requests[1].status === "fulfilled") state.effectiveAgents = requests[1].value.profiles || [];
  if (requests[2].status === "fulfilled") state.operations = requests[2].value || [];
  renderAutomationChip();
  if ($("#activity-dialog").open) await loadActivity();
}

const AUTOMATION_HELP = {
  en: {
    tabs: {
      overview: "See who handles routine work, exceptional recovery, and publication for this board.",
      runs: "Inspect active workers, agent readiness, dispatch operations, and board checks.",
      recovery: "Review exceptional graph recovery separately from normal task planning.",
      publishing: "Track the handoff from reviewed changes to a branch, pull request, or manual release.",
      events: "Start with a readable event summary and expand raw details only when debugging.",
    },
    roles: {
      Supervisor: "Deterministic host service that keeps board automation running and enforces the global write policy. It does not select a coding-agent model.",
      Dispatcher: "Deterministic host service that assigns Ready tasks to workers and tracks execution. It does not select a coding-agent model for itself.",
      Planner: "Coding-agent role for the normal Triage path: clarify, decompose, and prepare runnable work.",
      Coordinator: "Coding-agent role for exceptional recovery when the task graph stalls, conflicts, or exhausts normal retries.",
      Publisher: "Deterministic host service that moves reviewed finalizer changes to the configured target. It does not select a coding-agent model.",
    },
    boundary: "Supervisor, Dispatcher, and Publisher are deterministic host services with no coding-agent model. Planner and Coordinator invoke the configured coding-agent profile, runtime, model, and provider.",
    recovery: "Planner handles normal Triage work. Coordinator only proposes recovery when the graph needs exceptional intervention.",
    recoveryAssist: "Assist mode waits for a person to approve, reject, or request a new analysis before changing the graph.",
    publishing: "Publication actions use the version currently shown. If another process changes it first, refresh and review the new state.",
  },
  ko: {
    tabs: {
      overview: "이 보드에서 일상 작업, 예외 복구, 배포를 누가 맡는지 확인합니다.",
      runs: "실행 중인 작업자, 에이전트 준비 상태, 디스패치 작업, 보드 점검 결과를 확인합니다.",
      recovery: "일반 작업 계획과 분리해 예외적인 작업 그래프 복구를 검토합니다.",
      publishing: "검토를 마친 변경 사항이 브랜치, 풀 리퀘스트, 수동 배포로 전달되는 과정을 확인합니다.",
      events: "읽기 쉬운 요약을 먼저 보고, 디버깅할 때만 원본 세부 정보를 펼칩니다.",
    },
    roles: {
      Supervisor: "보드 자동화를 유지하고 전역 쓰기 정책을 적용하는 결정론적 호스트 서비스입니다. 코딩 에이전트 모델을 선택하지 않습니다.",
      Dispatcher: "Ready 작업을 작업자에게 배정하고 실행을 추적하는 결정론적 호스트 서비스입니다. 자체 코딩 에이전트 모델을 선택하지 않습니다.",
      Planner: "일반 Triage 흐름에서 요구 사항을 명확히 하고 작업을 나눠 실행할 수 있게 준비하는 코딩 에이전트 역할입니다.",
      Coordinator: "작업 그래프가 멈추거나 충돌하고 일반 재시도를 소진했을 때 예외 복구를 담당하는 코딩 에이전트 역할입니다.",
      Publisher: "검토를 마친 finalizer 변경 사항을 설정한 대상으로 전달하는 결정론적 호스트 서비스입니다. 코딩 에이전트 모델을 선택하지 않습니다.",
    },
    boundary: "Supervisor, Dispatcher, Publisher는 코딩 에이전트 모델 없이 동작하는 결정론적 호스트 서비스입니다. Planner와 Coordinator는 설정한 코딩 에이전트의 프로필, 런타임, 모델, 프로바이더를 사용합니다.",
    recovery: "Planner는 일반 Triage 작업을 처리합니다. Coordinator는 그래프에 예외적인 개입이 필요할 때만 복구안을 제시합니다.",
    recoveryAssist: "Assist 모드에서는 사람이 승인, 거절, 재분석을 선택한 뒤에만 그래프를 변경합니다.",
    publishing: "배포 작업은 현재 화면에 표시된 버전을 기준으로 처리합니다. 다른 프로세스가 먼저 변경했다면 새로 고친 뒤 변경된 상태를 다시 확인하세요.",
  },
};

const COORDINATION_ACTION_LABELS = {
  set_route: "Set route",
  update_priority: "Update priority",
  unblock_task: "Unblock task",
  move_to_triage: "Move to Triage",
  add_dependency: "Add dependency",
  remove_dependency: "Remove dependency",
  create_task: "Create task",
};

function automationHelp(key) {
  return AUTOMATION_HELP[state.automationHelpLanguage]?.[key] || AUTOMATION_HELP.en[key] || "";
}

function structuredValue(value) {
  if (typeof value !== "string") return value;
  try { return JSON.parse(value); } catch (_) { return value; }
}

function rawPayloadText(payload) {
  if (payload == null || payload === "") return "";
  if (typeof payload === "string") return payload;
  try { return JSON.stringify(payload, null, 2); } catch (_) { return String(payload); }
}

function humanizeIdentifier(value = "") {
  return String(value).replace(/([a-z0-9])([A-Z])/g, "$1 $2").replace(/[_-]+/g, " ")
    .replace(/\b\w/g, (character) => character.toUpperCase());
}

function readableValue(value) {
  value = structuredValue(value);
  if (value == null || value === "") return "—";
  if (Array.isArray(value)) return value.map(readableValue).join(", ");
  if (typeof value === "object") {
    return Object.entries(value).slice(0, 6)
      .map(([key, item]) => `${humanizeIdentifier(key)}: ${readableValue(item)}`).join(" · ");
  }
  if (typeof value === "boolean") return value ? "Yes" : "No";
  return String(value);
}

function readableFacts(value) {
  value = structuredValue(value);
  if (!value || typeof value !== "object" || Array.isArray(value)) return "";
  const facts = Object.entries(value).filter(([, item]) => item != null && item !== "").slice(0, 12);
  if (!facts.length) return "";
  return `<dl class="automation-facts">${facts.map(([key, item]) =>
    `<div><dt>${escapeHtml(humanizeIdentifier(key))}</dt><dd>${escapeHtml(readableValue(item))}</dd></div>`).join("")}</dl>`;
}

function automationStatusClass(status = "") {
  if (["running", "healthy", "resolved", "applied", "published", "no_change"].includes(status)) return "is-good";
  if (["failed", "retry_required", "critical", "error", "unhealthy", "missing"].includes(status)) return "is-danger";
  if (["restarting", "manual_completion", "awaiting_approval", "blocked", "warning", "rate_limited", "auth_required"].includes(status)) return "is-attention";
  return "";
}

function automationStatus(status = "unknown") {
  return `<span class="automation-status ${automationStatusClass(status)}">${escapeHtml(humanizeIdentifier(status))}</span>`;
}

function taskTitle(taskID) {
  return state.tasks.find((task) => task.id === taskID)?.title || taskID || "Unknown task";
}

function activityTaskButton(taskID, label) {
  if (!taskID) return "";
  return `<button type="button" class="activity-task-link" data-automation-focus="task:${escapeHtml(taskID)}" data-activity-task="${escapeHtml(taskID)}">${escapeHtml(label || taskTitle(taskID))}</button>`;
}

function supervisorPresentation(supervisor = {}) {
  const restartCount = Number(supervisor.restartCount || 0);
  if (supervisor.running) {
    return { status: "running", label: "Running", detail: restartCount ? `${restartCount} restart${restartCount === 1 ? "" : "s"} since start` : "Stable" };
  }
  if (supervisor.desired && supervisor.nextAttemptAt) {
    const target = new Date(supervisor.nextAttemptAt);
    const validTarget = !Number.isNaN(target.getTime());
    const seconds = validTarget ? Math.max(0, Math.ceil((target.getTime() - Date.now()) / 1000)) : null;
    const remaining = seconds == null ? "scheduled" : seconds < 60 ? `${seconds}s` : `${Math.ceil(seconds / 60)}m`;
    return {
      status: "restarting",
      label: `Restarting · attempt ${restartCount + 1}`,
      detail: `Backoff ${remaining} · next ${validTarget ? target.toLocaleString() : supervisor.nextAttemptAt}`,
    };
  }
  if (supervisor.desired) return { status: "starting", label: "Starting", detail: `Attempt ${restartCount + 1}` };
  if (supervisor.lastError) return { status: "failed", label: "Failed", detail: supervisor.lastError };
  return { status: "stopped", label: "Stopped", detail: "Not requested" };
}

function configuredAgent(config, ids, role) {
  const seen = new Set();
  for (const root of ids || []) {
    const queue = [root];
    while (queue.length) {
      const id = queue.shift();
      if (seen.has(id)) continue;
      seen.add(id);
      const agent = (config.agents || []).find((item) => item.id === id);
      if (!agent) continue;
      queue.push(...(agent.fallbacks || []));
      if (agent.enabled && (agent.roles || []).includes(role)) {
        return { ...agent, fallbackFrom: id === root ? "" : root };
      }
    }
  }
  return null;
}

function plannerRoute(data) {
  const metadata = data.metadata || {};
  const orchestration = metadata.orchestration || {};
  const config = data.effective.config || {};
  const boardModel = String(orchestration.plannerModel || "").trim();
  const boardProvider = String(orchestration.plannerProvider || "").trim();
  if (!boardModel && !boardProvider) {
    const global = configuredAgent(config, config.defaults?.plannerAgents, "planner");
    if (global) return {
      profile: global.id, runtime: global.runtime, model: global.model, provider: global.provider,
      source: global.fallbackFrom ? `Global planner fallback for ${global.fallbackFrom}` : "Global planner default",
      available: true,
    };
  }
  return {
    profile: "board-planner",
    runtime: orchestration.plannerRuntime || "codex",
    model: boardModel,
    provider: boardProvider,
    source: boardModel || boardProvider ? "Board planner override" : "Board planner fallback",
    available: true,
  };
}

function coordinatorRoute(data) {
  const config = data.effective.config || {};
  const override = String(data.coordination.policy?.profile || "").trim();
  const ids = override ? [override] : (config.defaults?.coordinatorAgents || []);
  const selected = configuredAgent(config, ids, "coordinator");
  if (selected) {
    return {
      profile: selected.id, runtime: selected.runtime, model: selected.model,
      provider: selected.provider,
      source: selected.fallbackFrom
        ? `${override ? "Board coordinator" : "Global coordinator"} fallback for ${selected.fallbackFrom}`
        : override ? "Board coordinator override" : "Global coordinator default",
      available: true,
    };
  }
  return {
    profile: override || ids[0] || "Not configured", runtime: "", model: "", provider: "",
    source: override ? "Board coordinator override" : "Global coordinator default", available: false,
  };
}

function codingAgentFacts(route, policyLabel, policyValue, signalLabel, signalValue) {
  return [
    [policyLabel, policyValue],
    ["Profile", `${route.profile} · ${route.source}`],
    ["Runtime", route.runtime || "Unavailable"],
    ["Model", route.model || "CLI default (unpinned)"],
    ["Provider", route.provider || "CLI default"],
    [signalLabel, signalValue],
  ];
}

function publicationRoleState(publications) {
  const failed = publications.filter((item) => item.status === "failed").length;
  const approvals = publications.filter((item) => item.status === "awaiting_approval").length;
  const manual = publications.filter((item) => item.mode === "manual" && item.status === "pending" &&
    (!item.requireApproval || item.approvedAt)).length;
  const publishing = publications.filter((item) => item.status === "publishing").length;
  const parts = [];
  if (failed) parts.push(`${failed} failed · Retry required`);
  if (approvals) parts.push(`${approvals} awaiting approval`);
  if (manual) parts.push(`${manual} ready for manual completion`);
  if (publishing) parts.push(`${publishing} publishing`);
  return {
    status: failed ? "retry_required" : approvals ? "awaiting_approval" : manual ? "manual_completion" : publishing ? "running" : "idle",
    label: parts.join(" · ") || "No pending handoff",
    attention: failed + approvals + manual,
    failed, approvals, manual,
  };
}

function roleCard({ name, kind, status, facts }) {
  const help = automationHelp("roles")?.[name] || AUTOMATION_HELP.en.roles[name];
  return `<article class="automation-role-card">
    <header><div><span class="automation-role-kind ${kind === "Coding agent" ? "is-agent" : ""}">${escapeHtml(kind)}</span><h3>${escapeHtml(name)}</h3></div>${automationStatus(status)}</header>
    <p>${escapeHtml(help)}</p>
    <dl>${facts.map(([label, value]) => `<div><dt>${escapeHtml(label)}</dt><dd>${escapeHtml(value)}</dd></div>`).join("")}</dl>
  </article>`;
}

function automationLoadWarnings(data) {
  if (!data.loadErrors?.length) return "";
  return `<div class="automation-warning" role="status"><strong>Some data could not be loaded.</strong><ul>${data.loadErrors.map((item) =>
    `<li>${escapeHtml(item.label)}: ${escapeHtml(item.message)}</li>`).join("")}</ul></div>`;
}

function renderAutomationOverview(data) {
  const diagnostics = data.inspection.diagnostics || {};
  const issues = diagnostics.issues || [];
  const activeRuns = diagnostics.activeRuns || [];
  const coordination = data.coordination || {};
  const orchestration = data.metadata?.orchestration || {};
  const coordinationPolicy = coordination.policy || orchestration.autopilot?.coordination || {};
  const publicationPolicy = orchestration.autopilot?.publication || {};
  const autopilot = orchestration.autopilot || {};
  const workerAgents = (data.effective.config?.agents || [])
    .filter((agent) => agent.enabled && (agent.roles || []).includes("worker"));
  const publicationState = publicationRoleState(data.publications);
  const coordinationAttention = Number(coordination.activeCount || 0);
  const supervisorAttention = data.supervisor.lastError ? 1 : 0;
  const attention = issues.length + publicationState.attention + coordinationAttention + supervisorAttention;
  const approvals = Number(coordination.awaitingApprovalCount || 0) +
    publicationState.approvals;
  const supervisor = supervisorPresentation(data.supervisor);
  const supervisorPolicy = data.supervisor.allowWrites ? "Workspace writes allowed" : "Read-only";
  const dispatcherStatus = activeRuns.length ? "running" : autopilot.autoExecute ? "ready" : "manual";
  const plannerQueue = Number(diagnostics.stats?.byStatus?.triage || 0);
  const planner = plannerRoute(data);
  const coordinator = coordinatorRoute(data);
  const roles = [
    roleCard({
      name: "Supervisor", kind: "Host service", status: supervisor.status,
      facts: [["Policy", supervisorPolicy], ["State", supervisor.label], ["Restart / backoff", supervisor.detail], ["Attention", String(supervisorAttention)]],
    }),
    roleCard({
      name: "Dispatcher", kind: "Host service", status: dispatcherStatus,
      facts: [["Policy", `AutoExecute ${autopilot.autoExecute ? "on" : "off"}`], ["Execution", "Deterministic host service · no model"], ["Active runs", String(activeRuns.length)]],
    }),
    roleCard({
      name: "Planner", kind: "Coding agent", status: planner.available ? (plannerQueue ? "ready" : "idle") : "missing",
      facts: codingAgentFacts(planner, "Policy", `AutoPlan ${autopilot.autoPlan ? "on" : "off"}`, "Triage queue", String(plannerQueue)),
    }),
    roleCard({
      name: "Coordinator", kind: "Coding agent", status: coordinator.available ? (coordinationAttention ? "warning" : "idle") : "missing",
      facts: codingAgentFacts(coordinator, "Policy", `Mode ${coordinationPolicy.mode || "observe"}`, "Attention", String(coordinationAttention)),
    }),
    roleCard({
      name: "Publisher", kind: "Host service", status: publicationState.status,
      facts: [["Policy", `Mode ${publicationPolicy.mode || "manual"}`], ["Execution", "Deterministic host service · no model"], ["Queue state", publicationState.label], ["Attention", String(publicationState.attention)]],
    }),
  ].join("");
  return `${automationLoadWarnings(data)}
    <section class="activity-section">
      <div class="activity-summary automation-summary">
        <div><small>Board policy</small><strong>${autopilot.enabled ? "Autopilot enabled" : "Manual control"}</strong><span>${autopilot.workspaceWrites && data.supervisor.allowWrites ? "Writes permitted" : "Read-only boundary"}</span></div>
        <div><small>Needs attention</small><strong>${attention}</strong><span>${approvals} approval${approvals === 1 ? "" : "s"}</span></div>
        <div><small>Workers</small><strong>${workerAgents.length || 0} configured</strong><span>${activeRuns.length} active</span></div>
        <div><small>Graph revision</small><strong>${coordination.graphState?.revision ?? "—"}</strong><span>${$("#connection").classList.contains("online") ? "Events connected" : "Events offline"}</span></div>
      </div>
    </section>
    ${data.supervisor.lastError ? `<div class="automation-warning"><strong>Supervisor error</strong><p>${escapeHtml(data.supervisor.lastError)}</p>${data.supervisor.nextAttemptAt ? `<small>Next attempt ${escapeHtml(new Date(data.supervisor.nextAttemptAt).toLocaleString())}</small>` : ""}</div>` : ""}
    <div class="automation-help-callout"><strong>Host services vs coding agents</strong><p>${escapeHtml(automationHelp("boundary"))}</p></div>
    <section class="activity-section"><h2 class="automation-section-title">Roles</h2><div class="automation-role-grid">${roles}</div></section>`;
}

function renderAutomationRuns(data) {
  const diagnostics = data.inspection.diagnostics || {};
  const activeRows = (diagnostics.activeRuns || []).map(({ task, run, agentConfig }) => `<article class="detail-row automation-run">
    <header><div><strong>${escapeHtml(task.title)}</strong>${automationStatus(run.status)}</div>${activityTaskButton(task.id, "Open task")}</header>
    <p>${escapeHtml(agentConfig ? `${agentConfig.profile} · ${agentConfig.runtime} · ${agentConfig.model || "CLI default (unpinned)"}${agentConfig.fallbackFrom ? ` · fallback from ${agentConfig.fallbackFrom}` : ""}` : `${run.workerId} · ${run.runtime}`)}</p>
    <span class="mono">${escapeHtml(run.id)} · heartbeat ${run.heartbeatAt ? relativeTime(run.heartbeatAt) : "unknown"}${run.claimExpiresAt ? ` · lease until ${escapeHtml(new Date(run.claimExpiresAt).toLocaleString())}` : ""}</span>
  </article>`).join("");
  const agentRows = (data.effective.profiles || []).map((profile) => `<article class="detail-row automation-run">
    <header><div><strong>${escapeHtml(profile.name)} · ${escapeHtml(profile.runtime)}</strong>${automationStatus(profile.health?.status || "unknown")}</div></header>
    <p>${escapeHtml(profile.model || "CLI default (unpinned)")}${profile.health?.lastError ? ` · ${escapeHtml(profile.health.lastError)}` : ""}</p>
    <span class="mono">${profile.activeRuns || 0} active${profile.health?.cooldownUntil ? ` · cooldown until ${escapeHtml(new Date(profile.health.cooldownUntil).toLocaleString())}` : ""}</span>
  </article>`).join("");
  const operationRows = data.operations.slice(0, 20).map((operation) => `<article class="detail-row automation-run">
    <header><div><strong>${escapeHtml(humanizeIdentifier(operation.kind))}</strong>${automationStatus(operation.status)}</div>${activityTaskButton(operation.taskId, "Open task")}</header>
    <span class="mono">${escapeHtml(operation.id)} · ${escapeHtml(operation.mode)} · ${operation.allowWrites ? "write" : "read-only"}${operation.startedAt ? ` · ${relativeTime(operation.startedAt)}` : ""}</span>
    ${operation.error ? `<p class="automation-error">${escapeHtml(operation.error)}</p>` : ""}
  </article>`).join("");
  const issueRows = (diagnostics.issues || []).map((issue) => `<article class="detail-row automation-run">
    <header><div><strong>${escapeHtml(humanizeIdentifier(issue.kind))}</strong>${automationStatus("warning")}</div>${activityTaskButton(issue.taskId, "Open task")}</header>
    <p>${escapeHtml(issue.detail)}</p>
  </article>`).join("");
  return `${automationLoadWarnings(data)}
    <section class="activity-section"><h2 class="automation-section-title">Active runs · ${(diagnostics.activeRuns || []).length}</h2><div class="detail-list">${activeRows || '<p class="automation-empty">No workers are running.</p>'}</div></section>
    <section class="activity-section"><h2 class="automation-section-title">Agents · ${(data.effective.profiles || []).length}</h2><div class="detail-list">${agentRows || '<p class="automation-empty">No effective agent profiles.</p>'}</div></section>
    <section class="activity-section"><h2 class="automation-section-title">Dispatch operations</h2><div class="detail-list">${operationRows || '<p class="automation-empty">No WebUI dispatch operations.</p>'}</div></section>
    <section class="activity-section"><h2 class="automation-section-title">Board checks · ${(diagnostics.issues || []).length}</h2><div class="detail-list">${issueRows || '<p class="automation-empty">No workflow, recovery, scheduling, or review items.</p>'}</div></section>`;
}

function coordinationActionDescription(action) {
  const task = action.taskId || action.task?.key || "task";
  switch (action.kind) {
  case "set_route":
    return `${task} → ${action.assignee || "unassigned"} · ${action.runtime || "default runtime"}`;
  case "update_priority":
    return `${task} → priority ${action.priority ?? "unchanged"}`;
  case "unblock_task":
    return `Return ${task} to the runnable queue`;
  case "move_to_triage":
    return `Return ${task} to Triage for normal planning`;
  case "add_dependency":
    return `${action.dependentId || task} waits for ${action.prerequisiteId || "prerequisite"}`;
  case "remove_dependency":
    return `Remove ${action.prerequisiteId || "prerequisite"} from ${action.dependentId || task}`;
  case "create_task":
    return `${action.task?.title || action.task?.key || "New task"}${action.task?.assignee ? ` · ${action.task.assignee}` : ""}`;
  default: {
    const details = Object.entries(action).filter(([key, value]) =>
      !["kind", "reason"].includes(key) && !key.startsWith("expected") && value != null && value !== "");
    return details.length ? details.map(([key, value]) =>
      `${humanizeIdentifier(key)}: ${readableValue(value)}`).join(" · ") : "No additional fields";
  }
  }
}

function coordinationTaskDraft(task = {}) {
  const known = new Set([
    "key", "title", "body", "assignee", "runtime", "workflowRole", "priority",
    "parentTaskId", "prerequisites", "dependents",
  ]);
  const facts = [
    ["Key", task.key || "—"],
    ["Title", task.title || "—"],
    ["Assignee", task.assignee || "Unassigned"],
    ["Runtime", task.runtime || "Default"],
    ["Workflow role", task.workflowRole || "worker"],
    ["Priority", task.priority ?? 0],
    ["Parent task", task.parentTaskId || "None"],
    ["Prerequisites", (task.prerequisites || []).length ? task.prerequisites.join(", ") : "None"],
    ["Dependents", (task.dependents || []).length ? task.dependents.join(", ") : "None"],
  ];
  for (const [key, value] of Object.entries(task)) {
    if (!known.has(key) && value != null && value !== "") facts.push([humanizeIdentifier(key), readableValue(value)]);
  }
  return `<dl class="automation-facts coordination-task-facts">${facts.map(([label, value]) =>
    `<div><dt>${escapeHtml(label)}</dt><dd>${escapeHtml(value)}</dd></div>`).join("")}</dl>
    <div class="coordination-task-body"><strong>Body</strong>${task.body
      ? `<div class="markdown">${markdown(task.body)}</div>`
      : '<p class="automation-empty">No body supplied.</p>'}</div>`;
}

function coordinationActionDetails(action) {
  if (action.kind === "create_task") {
    const versions = action.expectedTaskVersions && Object.keys(action.expectedTaskVersions).length
      ? `<div class="coordination-action-versions"><strong>Expected task versions</strong>${readableFacts(action.expectedTaskVersions)}</div>`
      : "";
    return `${coordinationTaskDraft(action.task || {})}${versions}`;
  }
  const details = Object.fromEntries(Object.entries(action).filter(([key, value]) =>
    !["kind", "reason"].includes(key) && !key.startsWith("expected") && value != null && value !== ""));
  return readableFacts(details);
}

function coordinationActions(actions, proposalID) {
  actions = structuredValue(actions);
  if (!Array.isArray(actions) || !actions.length) return '<p class="automation-empty">No graph changes proposed.</p>';
  return `<ol class="automation-action-list">${actions.map((action, index) => {
    const details = coordinationActionDetails(action);
    return `<li>
      <strong>${index + 1}. ${escapeHtml(COORDINATION_ACTION_LABELS[action.kind] || humanizeIdentifier(action.kind || "Action"))}</strong>
      <span>${escapeHtml(coordinationActionDescription(action))}</span>
      ${action.reason ? `<small>Reason: ${escapeHtml(action.reason)}</small>` : ""}
      ${details ? `<details class="coordination-action-details" data-automation-detail="proposal:${escapeHtml(proposalID)}:action:${index}"><summary>Review full action details</summary>${details}</details>` : ""}
    </li>`;
  }).join("")}</ol>`;
}

function validationErrors(value) {
  value = structuredValue(value);
  if (!Array.isArray(value) || !value.length) return "";
  return `<div class="automation-validation"><strong>Validation</strong><ul>${value.map((item) =>
    `<li>${escapeHtml(readableValue(item))}</li>`).join("")}</ul></div>`;
}

function coordinationProposalCard(proposal, incident, actionsAvailable) {
  const canDecide = actionsAvailable && proposal.status === "awaiting_approval" && incident.status === "awaiting_approval";
  const canReanalyze = actionsAvailable && ["awaiting_approval", "approved"].includes(proposal.status) &&
    incident.status === "awaiting_approval";
  return `<section class="automation-proposal">
    <header><div><strong>${escapeHtml(proposal.summary || "Recovery proposal")}</strong>${automationStatus(proposal.status)}</div><span class="mono">Graph ${proposal.expectedGraphRevision}</span></header>
    <p class="automation-rationale">${escapeHtml(proposal.rationale || "No rationale supplied.")}</p>
    <div class="automation-agent-line">Coordinator: ${escapeHtml(proposal.coordinatorAgent || "unknown")} · ${escapeHtml(proposal.coordinatorModel || "CLI default")}${proposal.coordinatorProvider ? ` · ${escapeHtml(proposal.coordinatorProvider)}` : ""}</div>
    <h4>Proposed actions</h4>${coordinationActions(proposal.actions, proposal.id)}
    ${validationErrors(proposal.validationErrors)}
    ${(canDecide || canReanalyze) ? `<div class="automation-actions">
      ${canDecide ? `<button type="button" class="primary" data-automation-focus="coordination:approve:${escapeHtml(proposal.id)}" data-coordination-action="approve" data-proposal-id="${escapeHtml(proposal.id)}" data-proposal-version="${escapeHtml(proposal.updatedAt)}">Approve</button><button type="button" class="danger" data-automation-focus="coordination:reject:${escapeHtml(proposal.id)}" data-coordination-action="reject" data-proposal-id="${escapeHtml(proposal.id)}" data-proposal-version="${escapeHtml(proposal.updatedAt)}">Reject</button>` : ""}
      ${canReanalyze ? `<button type="button" data-automation-focus="coordination:retry:${escapeHtml(proposal.id)}" data-coordination-action="retry" data-proposal-id="${escapeHtml(proposal.id)}" data-proposal-version="${escapeHtml(proposal.updatedAt)}">Reanalyze</button>` : ""}
    </div>` : ""}
  </section>`;
}

function coordinationIncidentCard(entry, actionsAvailable) {
  const incident = entry.incident;
  const proposals = entry.proposals || [];
  return `<article class="automation-record">
    <header class="automation-record-header"><div><h3>${escapeHtml(incident.summary || humanizeIdentifier(incident.trigger))}</h3><div class="automation-statuses">${automationStatus(incident.severity)}${automationStatus(incident.status)}</div></div>${activityTaskButton(incident.taskId || incident.rootTaskId, "Open task")}</header>
    <dl class="automation-record-meta"><div><dt>Trigger</dt><dd>${escapeHtml(humanizeIdentifier(incident.trigger))}</dd></div><div><dt>Graph</dt><dd>${incident.graphRevision}</dd></div><div><dt>Updated</dt><dd>${incident.updatedAt ? escapeHtml(relativeTime(incident.updatedAt)) : "—"}</dd></div></dl>
    ${readableFacts(incident.details)}
    ${entry.loadError ? `<div class="automation-warning"><strong>Proposal details unavailable</strong><p>${escapeHtml(entry.loadError)}</p></div>` : proposals.map((proposal) => coordinationProposalCard(proposal, incident, actionsAvailable)).join("")}
    ${!entry.loadError && !proposals.length ? '<p class="automation-empty">No proposal has been recorded for this incident.</p>' : ""}
    ${actionsAvailable && incident.status === "open" ? `<div class="automation-actions"><button type="button" class="danger" data-automation-focus="coordination:dismiss:${escapeHtml(incident.id)}" data-coordination-action="dismiss" data-incident-id="${escapeHtml(incident.id)}" data-incident-version="${escapeHtml(incident.updatedAt)}">Dismiss incident</button></div>` : ""}
  </article>`;
}

function renderAutomationRecovery(data) {
  const policy = data.coordination.policy || {};
  let records = '<p class="automation-empty">No coordination incidents.</p>';
  if (data.recoveryLoading) records = '<div class="detail-row">Loading recovery proposals…</div>';
  else if (data.recoveryLoaded && data.recoveryDetails.length) records = data.recoveryDetails
    .map((entry) => coordinationIncidentCard(entry, data.actionsAvailable.recovery)).join("");
  else if (!data.recoveryLoaded && (data.coordination.incidents || []).length) records = '<div class="detail-row">Loading recovery proposals…</div>';
  return `${automationLoadWarnings(data)}
    <div class="automation-help-callout"><strong>Planner ≠ Coordinator</strong><p>${escapeHtml(automationHelp("recovery"))}</p></div>
    ${policy.mode === "assist" ? `<div class="automation-help-callout is-attention"><strong>Assist mode</strong><p>${escapeHtml(automationHelp("recoveryAssist"))}</p></div>` : ""}
    <section class="activity-section">
      <div class="activity-summary automation-summary">
        <div><small>Policy</small><strong>${escapeHtml(humanizeIdentifier(policy.mode || "observe"))}</strong><span>${escapeHtml(policy.profile || "Default coordinator")}</span></div>
        <div><small>Active incidents</small><strong>${Number(data.coordination.activeCount || 0)}</strong><span>${Number(data.coordination.awaitingApprovalCount || 0)} awaiting approval</span></div>
        <div><small>Graph revision</small><strong>${data.coordination.graphState?.revision ?? "—"}</strong><span>Latest board state</span></div>
      </div>
    </section>
    <section class="activity-section"><h2 class="automation-section-title">Incidents</h2><div class="automation-record-list">${records}</div></section>`;
}

function safeExternalURL(value) {
  if (!value) return null;
  try {
    const parsed = new URL(value);
    return ["http:", "https:"].includes(parsed.protocol) ? parsed.href : null;
  } catch (_) { return null; }
}

function publicationCard(item, actionsAvailable) {
  const link = safeExternalURL(item.url);
  const canReject = actionsAvailable && ["awaiting_approval", "pending", "failed"].includes(item.status);
  const canComplete = actionsAvailable && item.mode === "manual" && item.status === "pending" &&
    (!item.requireApproval || item.approvedAt);
  return `<article class="automation-record publication-record">
    <header class="automation-record-header"><div><h3>${escapeHtml(taskTitle(item.taskId))}</h3><div class="automation-statuses">${automationStatus(item.status)}${automationStatus(item.mode)}</div></div>${activityTaskButton(item.taskId, "Open task")}</header>
    <dl class="automation-record-meta publication-meta">
      <div><dt>Status</dt><dd>${escapeHtml(humanizeIdentifier(item.status))}</dd></div>
      <div><dt>Mode</dt><dd>${escapeHtml(humanizeIdentifier(item.mode))}</dd></div>
      <div><dt>Task</dt><dd class="mono">${escapeHtml(item.taskId)}</dd></div>
      <div><dt>Branch</dt><dd>${escapeHtml(item.targetBranch || "—")}</dd></div>
      <div><dt>Remote</dt><dd>${escapeHtml(item.remote || "—")}</dd></div>
      <div><dt>URL</dt><dd>${link ? `<a data-automation-focus="publication:url:${escapeHtml(item.id)}" href="${escapeHtml(link)}" target="_blank" rel="noopener noreferrer">${escapeHtml(item.url)}</a>` : escapeHtml(item.url || "—")}</dd></div>
    </dl>
    ${item.error ? `<div class="automation-error"><strong>Error</strong><p>${escapeHtml(item.error)}</p></div>` : ""}
    ${actionsAvailable && (item.status === "awaiting_approval" || canReject || item.status === "failed" || canComplete) ? `<div class="automation-actions">
      ${actionsAvailable && item.status === "awaiting_approval" ? `<button type="button" class="primary" data-automation-focus="publication:approve:${escapeHtml(item.id)}" data-publication-action="approve" data-publication-id="${escapeHtml(item.id)}" data-publication-version="${escapeHtml(item.updatedAt)}">Approve</button>` : ""}
      ${canReject ? `<button type="button" class="danger" data-automation-focus="publication:reject:${escapeHtml(item.id)}" data-publication-action="reject" data-publication-id="${escapeHtml(item.id)}" data-publication-version="${escapeHtml(item.updatedAt)}">Reject</button>` : ""}
      ${actionsAvailable && item.status === "failed" ? `<button type="button" data-automation-focus="publication:retry:${escapeHtml(item.id)}" data-publication-action="retry" data-publication-id="${escapeHtml(item.id)}" data-publication-version="${escapeHtml(item.updatedAt)}">Retry</button>` : ""}
      ${canComplete ? `<button type="button" class="primary" data-automation-focus="publication:complete:${escapeHtml(item.id)}" data-publication-action="complete" data-publication-id="${escapeHtml(item.id)}" data-publication-version="${escapeHtml(item.updatedAt)}">Complete manually</button>` : ""}
    </div>` : ""}
  </article>`;
}

function renderAutomationPublishing(data) {
  const policy = data.metadata?.orchestration?.autopilot?.publication || {};
  const publicationState = publicationRoleState(data.publications);
  return `${automationLoadWarnings(data)}
    <div class="automation-help-callout"><strong>Publishing</strong><p>${escapeHtml(automationHelp("publishing"))}</p></div>
    <section class="activity-section">
      <div class="activity-summary automation-summary">
        <div><small>Policy</small><strong>${escapeHtml(humanizeIdentifier(policy.mode || "manual"))}</strong><span>${policy.requireApproval ? "Approval required" : "No approval gate"}</span></div>
        <div><small>Target</small><strong>${escapeHtml(policy.targetBranch || "main")}</strong><span>${escapeHtml(policy.remote || "origin")}</span></div>
        <div><small>Needs attention</small><strong>${publicationState.attention}</strong><span>${publicationState.failed} failed · ${publicationState.approvals} approval · ${publicationState.manual} manual</span></div>
      </div>
    </section>
    <section class="activity-section"><h2 class="automation-section-title">Publication handoffs</h2><div class="automation-record-list">${data.publications.map((item) => publicationCard(item, data.actionsAvailable.publishing)).join("") || '<p class="automation-empty">No publication handoffs yet.</p>'}</div></section>`;
}

function eventSummary(payload) {
  const value = structuredValue(payload);
  if (value == null || value === "") return "No additional summary.";
  if (typeof value !== "object") return String(value);
  const preferred = ["summary", "message", "reason", "error", "status", "action", "mode", "result", "url"];
  const parts = preferred.filter((key) => value[key] != null && value[key] !== "")
    .slice(0, 4).map((key) => `${humanizeIdentifier(key)}: ${readableValue(value[key])}`);
  if (parts.length) return parts.join(" · ");
  const first = Object.entries(value).slice(0, 4);
  return first.length ? first.map(([key, item]) => `${humanizeIdentifier(key)}: ${readableValue(item)}`).join(" · ") : "No additional summary.";
}

function renderAutomationEvents(data) {
  const events = data.inspection.recentEvents || [];
  const rows = events.map((event) => {
    const raw = rawPayloadText(event.payload);
    return `<article class="automation-record event-record">
      <header class="automation-record-header"><div><h3>${escapeHtml(humanizeIdentifier(event.kind))}</h3><span class="mono">#${event.id}${event.createdAt ? ` · ${relativeTime(event.createdAt)}` : ""}${event.runId ? ` · ${escapeHtml(event.runId)}` : ""}</span></div>${activityTaskButton(event.taskId, "Open task")}</header>
      <p class="event-summary">${escapeHtml(eventSummary(event.payload))}</p>
      ${raw ? `<details class="event-details" data-automation-detail="event:${escapeHtml(event.id)}"><summary>Raw details</summary><pre class="event-payload">${escapeHtml(raw)}</pre></details>` : ""}
    </article>`;
  }).join("");
  return `${automationLoadWarnings(data)}
    <section class="activity-section"><h2 class="automation-section-title">Recent durable events</h2><div class="automation-record-list">${rows || '<p class="automation-empty">No task events yet.</p>'}</div></section>`;
}

function updateAutomationShell() {
  const language = state.automationHelpLanguage;
  const help = AUTOMATION_HELP[language] || AUTOMATION_HELP.en;
  $("#automation-center-help").textContent = help.tabs[state.automationTab];
  const languageButton = $("#automation-help-language");
  languageButton.textContent = `Help · ${language.toUpperCase()}`;
  languageButton.setAttribute("aria-label", `Switch help text to ${language === "en" ? "Korean" : "English"}`);
  $$("[data-activity-tab]", $("#automation-tabs")).forEach((button) => {
    const selected = button.dataset.activityTab === state.automationTab;
    button.setAttribute("aria-selected", String(selected));
    button.tabIndex = selected ? 0 : -1;
  });
  $("#activity-content").setAttribute("aria-labelledby", `automation-tab-${state.automationTab}`);
}

function captureAutomationViewState() {
  const content = $("#activity-content");
  const active = document.activeElement;
  const focusTarget = active?.closest?.("[data-automation-focus]");
  const focusedDetails = active?.closest?.("details[data-automation-detail]");
  return {
    tab: state.automationTab,
    openDetails: $$("details[data-automation-detail]", content)
      .filter((details) => details.open).map((details) => details.dataset.automationDetail),
    focusKey: focusTarget?.dataset.automationFocus || "",
    focusDetailsKey: active?.tagName === "SUMMARY" ? focusedDetails?.dataset.automationDetail || "" : "",
    focusInContent: Boolean(active && content.contains(active)),
    dialogScrollTop: $("#activity-dialog").scrollTop,
  };
}

function restoreAutomationViewState(view) {
  if (!view) return;
  const content = $("#activity-content");
  const expanded = new Set(view.openDetails || []);
  $$("details[data-automation-detail]", content).forEach((details) => {
    details.open = expanded.has(details.dataset.automationDetail);
  });
  if (view.tab === state.automationTab && view.focusInContent) {
    let target = $$("[data-automation-focus]", content)
      .find((element) => element.dataset.automationFocus === view.focusKey);
    if (!target && view.focusDetailsKey) {
      const details = $$("details[data-automation-detail]", content)
        .find((element) => element.dataset.automationDetail === view.focusDetailsKey);
      target = details && $("summary", details);
    }
    (target || content).focus({ preventScroll: true });
  }
  $("#activity-dialog").scrollTop = view.dialogScrollTop || 0;
}

function setAutomationBusy(busy) {
  const content = $("#activity-content");
  content.setAttribute("aria-busy", String(busy));
  $("#activity-refresh").disabled = busy;
  if (busy) {
    $$("[data-coordination-action], [data-publication-action]", content)
      .forEach((button) => { button.disabled = true; });
  }
}

function cancelAutomationLoad() {
  automationLoadGeneration++;
  automationLoadController?.abort();
  automationLoadController = null;
}

function currentAutomationRequest(board, generation, signal) {
  return !signal?.aborted && state.board === board && automationLoadGeneration === generation;
}

function renderAutomationCenter(options = {}) {
  const view = options.viewState || captureAutomationViewState();
  updateAutomationShell();
  if (!state.automationData) return;
  const renderers = {
    overview: renderAutomationOverview,
    runs: renderAutomationRuns,
    recovery: renderAutomationRecovery,
    publishing: renderAutomationPublishing,
    events: renderAutomationEvents,
  };
  const content = $("#activity-content");
  content.dataset.board = state.automationData.board;
  content.innerHTML = renderers[state.automationTab](state.automationData);
  restoreAutomationViewState(view);
}

function recoveryCacheKey(board, incident) {
  return JSON.stringify([board, incident.id, incident.updatedAt]);
}

function pruneRecoveryCache(board, incidents) {
  const valid = new Set(incidents.map((incident) => recoveryCacheKey(board, incident)));
  for (const key of automationRecoveryCache.keys()) {
    let cachedBoard = "";
    try { [cachedBoard] = JSON.parse(key); } catch (_) { automationRecoveryCache.delete(key); continue; }
    if (cachedBoard === board && !valid.has(key)) automationRecoveryCache.delete(key);
  }
}

async function ensureRecoveryDetails(data = state.automationData, options = {}) {
  if (!data || data.recoveryLoaded || data.recoveryLoading) return;
  const { board, loadGeneration: generation, requestSignal: signal } = data;
  if (!currentAutomationRequest(board, generation, signal) || state.automationData !== data) return;
  const view = options.viewState || captureAutomationViewState();
  data.recoveryLoading = true;
  const incidents = (data.coordination.incidents || []).slice(0, 20);
  pruneRecoveryCache(board, incidents);
  if (options.renderLoading !== false) {
    renderAutomationCenter({ viewState: view });
    setAutomationBusy(true);
  }
  const details = new Array(incidents.length);
  const missing = [];
  incidents.forEach((incident, index) => {
    const cached = automationRecoveryCache.get(recoveryCacheKey(board, incident));
    if (cached) details[index] = cached;
    else missing.push({ incident, index });
  });
  const results = await Promise.allSettled(missing.map(({ incident }) =>
    api(boardPathFor(board, `/api/coordination/incidents/${encodeURIComponent(incident.id)}`), { signal })));
  if (!currentAutomationRequest(board, generation, signal) || state.automationData !== data) return;
  results.forEach((result, resultIndex) => {
    const { incident, index } = missing[resultIndex];
    if (result.status === "fulfilled") {
      details[index] = result.value;
      automationRecoveryCache.set(recoveryCacheKey(board, incident), result.value);
    } else {
      details[index] = {
        incident, proposals: [],
        loadError: result.reason?.name === "AbortError" ? "Request canceled" : result.reason?.message || "Request failed",
      };
    }
  });
  data.recoveryDetails = details;
  data.recoveryLoaded = true;
  data.recoveryLoading = false;
  if (options.renderResult !== false) {
    renderAutomationCenter({ viewState: view });
    setAutomationBusy(false);
  }
}

function setAutomationTab(value, options = {}) {
  if (!AUTOMATION_TABS.includes(value)) return;
  state.automationTab = value;
  if (options.persist !== false) localStorage.setItem("autogora.automationCenterTab", value);
  renderAutomationCenter();
  if (options.focus) $(`#automation-tab-${value}`)?.focus();
  if (value === "recovery") ensureRecoveryDetails().catch((error) => toast(error.message, true));
}

function toggleAutomationHelpLanguage() {
  state.automationHelpLanguage = state.automationHelpLanguage === "en" ? "ko" : "en";
  localStorage.setItem("autogora.automationHelpLanguage", state.automationHelpLanguage);
  renderAutomationCenter();
}

async function loadActivity() {
  const board = state.board;
  const generation = ++automationLoadGeneration;
  automationLoadController?.abort();
  const controller = new AbortController();
  automationLoadController = controller;
  const { signal } = controller;
  setAutomationBusy(true);
  try {
    const requests = await Promise.allSettled([
      api(boardPathFor(board, "/api/inspect"), { signal }),
      api("/api/supervisor", { signal }),
      api(boardPathFor(board, "/api/agents/effective"), { signal }),
      api(boardPathFor(board, "/api/operations"), { signal }),
      api(boardPathFor(board, "/api/coordination"), { signal }),
      api(boardPathFor(board, "/api/publications?limit=100"), { signal }),
    ]);
    if (!currentAutomationRequest(board, generation, signal)) return false;
    const previous = state.automationData?.board === board ? state.automationData : null;
    const fallback = [
      previous?.inspection || { diagnostics: {}, recentEvents: [] },
      state.supervisor || {},
      previous?.effective || { profiles: state.effectiveAgents || [], config: state.agentConfig || {}, metadata: state.metadata },
      previous?.operations || state.operations || [],
      previous?.coordination || { policy: state.metadata?.orchestration?.autopilot?.coordination || {}, incidents: [], activeCount: 0, awaitingApprovalCount: 0 },
      previous?.publications || [],
    ];
    const labels = ["Board inspection", "Supervisor", "Agents", "Operations", "Recovery", "Publishing"];
    const values = requests.map((request, index) => request.status === "fulfilled" ? request.value : fallback[index]);
    const loadErrors = requests.flatMap((request, index) => request.status === "rejected" && request.reason?.name !== "AbortError"
      ? [{ label: labels[index], message: request.reason?.message || "Request failed" }] : []);
    const [inspection, supervisor, effective, operations, coordination, publications] = values;
    const data = {
      board, loadGeneration: generation, requestSignal: signal,
      metadata: effective.metadata || previous?.metadata || state.metadata,
      inspection, supervisor, effective, operations: operations || [], coordination,
      publications: publications || [], loadErrors, recoveryDetails: [], recoveryLoaded: false,
      recoveryLoading: false,
      actionsAvailable: {
        recovery: requests[4].status === "fulfilled",
        publishing: requests[5].status === "fulfilled",
      },
    };
    state.supervisor = supervisor;
    state.effectiveAgents = effective.profiles || [];
    state.operations = operations || [];
    state.automationData = data;
    renderAutomationChip();
    if (state.automationTab === "recovery") {
      await ensureRecoveryDetails(data, { renderLoading: false, renderResult: false });
      if (!currentAutomationRequest(board, generation, signal) || state.automationData !== data) return false;
    }
    renderAutomationCenter({ viewState: captureAutomationViewState() });
    return true;
  } finally {
    if (currentAutomationRequest(board, generation, signal)) setAutomationBusy(false);
  }
}

function automationMutationMessage(error) {
  return /conflict|changed|stale|revision|updated/i.test(error.message)
    ? "The state changed before this action completed. Data was refreshed; review the current state and try again."
    : error.message;
}

async function runAutomationMutation(button, action, successMessage) {
  button.disabled = true;
  try {
    await action();
    toast(successMessage);
    await loadActivity();
  } catch (error) {
    toast(automationMutationMessage(error), true);
    await loadActivity().catch(() => {});
  } finally {
    if (button.isConnected) button.disabled = false;
  }
}

async function mutateCoordination(button) {
  const action = button.dataset.coordinationAction;
  const data = state.automationData;
  if (!data || data.board !== state.board || !data.actionsAvailable.recovery) {
    toast("Recovery data is stale. Refresh before acting.", true);
    return;
  }
  if (action === "dismiss") {
    const entry = data.recoveryDetails.find((item) => item.incident.id === button.dataset.incidentId);
    if (entry && button.dataset.incidentVersion !== entry.incident.updatedAt) {
      toast("This incident changed. Refresh before dismissing it.", true);
      await loadActivity();
      return;
    }
    if (!entry || !confirm("Dismiss this open incident?")) return;
    await runAutomationMutation(button, () => api(boardPathFor(data.board, `/api/coordination/incidents/${encodeURIComponent(entry.incident.id)}/dismiss`), {
      method: "POST", body: JSON.stringify({ expectedGraphRevision: entry.incident.graphRevision }),
    }), "Incident dismissed");
    return;
  }
  const proposal = data.recoveryDetails.flatMap((item) => item.proposals || [])
    .find((item) => item.id === button.dataset.proposalId);
  if (!proposal) return;
  if (button.dataset.proposalVersion !== proposal.updatedAt) {
    toast("This recovery proposal changed. Refresh before acting.", true);
    await loadActivity();
    return;
  }
  const prompts = {
    approve: "Approve and apply this recovery proposal?",
    reject: "Reject this recovery proposal?",
    retry: "Supersede this proposal and request a new analysis?",
  };
  if (!confirm(prompts[action])) return;
  await runAutomationMutation(button, () => api(boardPathFor(data.board, `/api/coordination/proposals/${encodeURIComponent(proposal.id)}/${action}`), {
    method: "POST",
    body: JSON.stringify({
      expectedUpdatedAt: proposal.updatedAt,
      expectedGraphRevision: proposal.expectedGraphRevision,
    }),
  }), {
    approve: "Recovery proposal approved",
    reject: "Recovery proposal rejected",
    retry: "Recovery reanalysis scheduled",
  }[action]);
}

async function mutatePublication(button) {
  const action = button.dataset.publicationAction;
  const data = state.automationData;
  if (!data || data.board !== state.board || !data.actionsAvailable.publishing) {
    toast("Publishing data is stale. Refresh before acting.", true);
    return;
  }
  const item = data.publications.find((value) => value.id === button.dataset.publicationId);
  if (!item) return;
  if (button.dataset.publicationVersion !== item.updatedAt) {
    toast("This publication changed. Refresh before acting.", true);
    await loadActivity();
    return;
  }
  const body = { expectedUpdatedAt: item.updatedAt };
  if (action === "reject") {
    const reason = prompt("Reason for rejection:");
    if (reason == null || !reason.trim()) return;
    body.reason = reason.trim();
  } else if (action === "complete") {
    const url = prompt("Published URL (optional):", item.url || "");
    if (url == null) return;
    body.url = url.trim() || null;
  } else if (!confirm(action === "approve"
    ? "Approve this publication handoff?"
    : "Retry this failed publication?")) return;
  await runAutomationMutation(button, () => api(boardPathFor(data.board, `/api/publications/${encodeURIComponent(item.id)}/${action}`), {
    method: "POST", body: JSON.stringify(body),
  }), {
    approve: "Publication approved",
    reject: "Publication rejected",
    retry: "Publication retry scheduled",
    complete: "Manual publication completed",
  }[action]);
}

async function openActivity() {
  updateAutomationShell();
  if (!$("#activity-dialog").open) $("#activity-dialog").showModal();
  $("#activity-content").innerHTML = '<div class="detail-row">Loading Automation Center…</div>';
  await loadActivity();
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
    defaults: { workerAgents: [], plannerAgents: [], coordinatorAgents: [], judgeAgents: [] },
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
      ${["worker", "planner", "coordinator", "judge"].map((role) => `<label class="inline"><input data-agent-role="${role}" type="checkbox"${roles.has(role) ? " checked" : ""}> ${role}</label>`).join("")}
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
  form.elements.coordinatorAgents.value = (config.defaults?.coordinatorAgents || []).join(", ");
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
      coordinatorAgents: commaIDs(form.elements.coordinatorAgents.value),
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
      roles: ["worker", "planner", "coordinator", "judge"], fallbacks: [],
    });
    if (enabled) added.push(detection.id);
  }
  result.defaults ||= { workerAgents: [], plannerAgents: [], coordinatorAgents: [], judgeAgents: [] };
  for (const [key, role] of [["workerAgents", "worker"], ["plannerAgents", "planner"], ["coordinatorAgents", "coordinator"], ["judgeAgents", "judge"]]) {
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

function renderAgentPresets() {
  const select = $("#agent-preset");
  select.innerHTML = state.agentPresets.map((preset) =>
    `<option value="${escapeHtml(preset.id)}">${escapeHtml(preset.id)}</option>`).join("");
  if (!select.value && state.agentPresets.length) select.value = state.agentPresets[0].id;
  updateAgentPresetDescription();
}

function updateAgentPresetDescription() {
  const preset = state.agentPresets.find((item) => item.id === $("#agent-preset").value);
  $("#agent-preset-description").textContent = preset?.description
    || "Choose a common unpinned agent configuration, then review it before saving.";
}

function recommendedAgentPreset() {
  const available = new Set(state.detections
    .filter((item) => ["installed", "version_unavailable"].includes(item.state))
    .map((item) => item.runtime));
  if (available.has("codex") && available.has("claude")) return "codex-claude";
  if (available.has("codex")) return "codex";
  if (available.has("claude")) return "claude";
  return state.agentPresets.some((preset) => preset.id === "codex-claude") ? "codex-claude" : state.agentPresets[0]?.id;
}

async function previewAgentPreset({ automatic = false } = {}) {
  const select = $("#agent-preset");
  const button = $("#apply-agent-preset");
  if (!select.value) return;
  try {
    button.disabled = true;
    button.textContent = "Preparing…";
    const result = await api("/api/agents/presets", {
      method: "POST",
      body: JSON.stringify({
        id: select.value,
        replace: $("#agent-preset-replace").checked,
        config: readAgentEditor(),
      }),
    });
    state.detections = result.detections || [];
    renderAgentEditor(result.config);
    $("#agent-preset-description").textContent = `${result.preset.description} Review the generated agents, then choose Save and apply.`;
    if (!automatic) toast(`Loaded ${result.preset.id} preset for review`);
  } catch (error) {
    toast(error.message, true);
  } finally {
    button.disabled = false;
    button.textContent = "Use preset";
  }
}

async function loadAgentConfiguration() {
  const [configuration, supervisor, presetCatalog] = await Promise.all([
    api("/api/config"), api("/api/supervisor"), api("/api/agents/presets"),
  ]);
  state.agentConfig = configuration.config;
  state.agentConfigExists = configuration.exists;
  state.agentPresets = presetCatalog.presets || [];
  state.supervisor = supervisor;
  $("#agents-config-path").textContent = configuration.path;
  renderAgentPresets();
  renderAutomationChip();
  return configuration;
}

function renderSupervisorStatus() {
  const status = state.supervisor || {};
  const statusElement = $("#supervisor-status");
  const toggle = $("#supervisor-toggle");
  if (status.running) {
    statusElement.textContent = `Running · ${status.maxWorkers || 1} worker${status.maxWorkers === 1 ? "" : "s"} · ${status.allowWrites ? "workspace writes allowed" : "read-only workers"}`;
    toggle.textContent = "Stop";
  } else if (status.desired && status.nextAttemptAt) {
    statusElement.textContent = `Restarting · attempt ${Number(status.restartCount || 0) + 1} at ${new Date(status.nextAttemptAt).toLocaleTimeString()}${status.lastError ? ` · ${status.lastError}` : ""}`;
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
  renderAgentPresets();
  renderAgentEditor(state.agentConfig);
  renderSupervisorStatus();
  const dialog = $("#agents-dialog");
  if (!dialog.open) dialog.showModal();
  try {
    const effective = await api(boardPath("/api/agents/effective"));
    state.effectiveAgents = effective.profiles || [];
  } catch (error) { state.effectiveAgents = []; }
  await detectAgents(false);
  const recommended = recommendedAgentPreset();
  if (recommended) {
    $("#agent-preset").value = recommended;
    updateAgentPresetDescription();
  }
  if (firstRun && !(state.agentConfig.agents || []).length && recommended) {
    await previewAgentPreset({ automatic: true });
  }
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
    renderAutomationChip();
    $("#agents-dialog").close();
    await loadBoard();
    toast(state.supervisor.running ? "Agent settings saved; orchestration is running" : "Agent settings saved; automation is stopped");
  } catch (error) { toast(error.message, true); }
  finally { button.disabled = false; button.textContent = "Save and apply"; }
}

async function toggleSupervisor() {
  const button = $("#supervisor-toggle");
  if (!state.agentConfigExists) return;
  try {
    button.disabled = true;
    const action = state.supervisor?.running || state.supervisor?.desired ? "stop" : "start";
    state.supervisor = await api(`/api/supervisor/${action}`, { method: "POST", body: "{}" });
    renderSupervisorStatus();
    renderAutomationChip();
  } catch (error) { toast(error.message, true); }
  finally { button.disabled = false; }
}

function removeAgentReferences(id) {
  if (!id) return;
  const form = $("#agents-form");
  for (const name of ["workerAgents", "plannerAgents", "coordinatorAgents", "judgeAgents"]) {
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
  socket.addEventListener("open", () => { $("#connection").textContent = "events connected"; $("#connection").classList.add("online"); });
  socket.addEventListener("message", (message) => {
    const payload = JSON.parse(message.data);
    if (payload.cursor) state.cursor = payload.cursor;
    scheduleRefresh();
  });
  socket.addEventListener("error", () => {
    $("#connection").textContent = "events offline"; $("#connection").classList.remove("online");
  });
}

let refreshTimer;
function scheduleRefresh() {
  clearTimeout(refreshTimer);
  refreshTimer = setTimeout(async () => {
    await loadBoard();
    if (state.drawerTask && state.drawerDirty) {
      $("#drawer-refresh").classList.remove("hidden");
    } else if (state.drawerTask) {
      await openDrawer(state.drawerTask, { focus: false }).catch(() => closeDrawer());
    }
  }, 180);
}

function initializeSelects() {
  const mutableStatuses = STATUSES.filter((status) => status !== "running");
  const options = mutableStatuses.map((status) => `<option value="${status}">${status}</option>`).join("");
  $("#task-form [name=status]").innerHTML = options;
  $("#bulk-status").innerHTML = `<option value="">Move to…</option>${options}`;
  $("#show-archived").checked = localStorage.getItem("autogora.showArchived") === "true" || state.stageFocus === "archive";
  $("#lane-profile").checked = localStorage.getItem("autogora.laneByProfile") === "true";
}

function bindGlobalActions() {
  $$('[data-close-dialog]').forEach((button) => button.addEventListener("click", () => button.closest("dialog").close()));
  bindSegmentedControl('[data-board-control="focus"]', "[data-stage-focus]", (button) => setStageFocus(button.dataset.stageFocus));
  bindSegmentedControl('[data-board-control="view"]', "[data-board-view]", (button) => setBoardView(button.dataset.boardView));
  $("#board-select").addEventListener("change", async (event) => {
    cancelAutomationLoad();
    cancelGraphLoad();
    state.board = event.target.value; state.cursor = 0; state.selected.clear(); localStorage.setItem("autogora.board", state.board);
    await loadBoard(); connectEvents();
  });
  ["#search", "#tenant-filter", "#assignee-filter"].forEach((selector) => $(selector).addEventListener("input", renderBoard));
  $("#lane-profile").addEventListener("change", () => { localStorage.setItem("autogora.laneByProfile", $("#lane-profile").checked); renderBoard(); });
  $("#show-archived").addEventListener("change", () => {
    const checked = $("#show-archived").checked;
    localStorage.setItem("autogora.showArchived", checked);
    if (!checked && state.stageFocus === "archive") {
      state.stageFocus = "all";
      localStorage.setItem("autogora.boardStageFocus", state.stageFocus);
      updateBoardViewControls();
    }
    loadBoard();
  });
  $("#drawer-close").addEventListener("click", closeDrawer);
  $("#drawer").addEventListener("cancel", (event) => { event.preventDefault(); closeDrawer(); });
  $("#drawer-refresh").addEventListener("click", () => state.drawerTask && openDrawer(state.drawerTask, { focus: false, force: true }));
  document.addEventListener("keydown", (event) => { if (event.key === "Escape" && state.drawerTask) closeDrawer(); });
  $("#bulk-clear").addEventListener("click", () => { state.selected.clear(); renderBoard(); });
  $("#bulk-status").addEventListener("change", (event) => { if (event.target.value) bulkMutation({ status: event.target.value }); });
  $("#bulk-assign").addEventListener("click", () => bulkMutation({ assignee: $("#bulk-assignee").value || null }));
  $("#bulk-archive").addEventListener("click", () => confirm("Archive selected tasks?") && bulkMutation({ archive: true }));
  $("#bulk-delete").addEventListener("click", () => confirm("Permanently delete selected tasks?") && bulkMutation({ delete: true }));
  $("#new-board").addEventListener("click", () => { $("#board-form").reset(); $("#board-dialog").showModal(); });
  $("#new-swarm").addEventListener("click", () => { $("#swarm-form").reset(); $("#swarm-dialog").showModal(); });
  $("#nudge").addEventListener("click", async () => {
    const result = await api(boardPath("/api/dispatch"), { method: "POST", body: "{}" });
    toast(result.mode === "supervisor" ? "Supervisor is already watching this board" : `Dispatcher operation ${result.operation?.id || "started"}`);
    await refreshOperationalStatus();
  });
  $("#theme-toggle").addEventListener("click", () => setTheme(activeTheme === "dark" ? "light" : "dark"));
  $("#board-settings").addEventListener("click", openSettings);
  $("#agent-settings").addEventListener("click", () => openAgentSettings().catch((error) => toast(error.message, true)));
  $("#import-issues").addEventListener("click", openGitHubImport);
  $("#automation").addEventListener("click", () => openActivity().catch((error) => toast(error.message, true)));
  $("#activity-refresh").addEventListener("click", () => loadActivity().catch((error) => toast(error.message, true)));
  $("#activity-dialog").addEventListener("close", () => {
    cancelAutomationLoad();
    setAutomationBusy(false);
  });
  $("#automation-help-language").addEventListener("click", toggleAutomationHelpLanguage);
  $("#automation-tabs").addEventListener("click", (event) => {
    const tab = event.target.closest("[data-activity-tab]");
    if (tab) setAutomationTab(tab.dataset.activityTab);
  });
  $("#automation-tabs").addEventListener("keydown", (event) => {
    if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
    event.preventDefault();
    const current = AUTOMATION_TABS.indexOf(state.automationTab);
    const next = event.key === "Home" ? 0
      : event.key === "End" ? AUTOMATION_TABS.length - 1
        : (current + (event.key === "ArrowRight" ? 1 : -1) + AUTOMATION_TABS.length) % AUTOMATION_TABS.length;
    setAutomationTab(AUTOMATION_TABS[next], { focus: true });
  });
  $("#activity-content").addEventListener("click", (event) => {
    if (state.automationData?.board !== state.board) {
      toast("This Automation Center view is stale. Refresh before acting.", true);
      return;
    }
    const taskButton = event.target.closest("[data-activity-task]");
    if (taskButton) {
      $("#activity-dialog").close();
      openDrawer(taskButton.dataset.activityTask);
      return;
    }
    const coordinationButton = event.target.closest("[data-coordination-action]");
    if (coordinationButton) {
      mutateCoordination(coordinationButton).catch((error) => toast(error.message, true));
      return;
    }
    const publicationButton = event.target.closest("[data-publication-action]");
    if (publicationButton) mutatePublication(publicationButton).catch((error) => toast(error.message, true));
  });
  $("#manage-agents").addEventListener("click", () => {
    $("#settings-dialog").close();
    openAgentSettings().catch((error) => toast(error.message, true));
  });
  $("#task-form").addEventListener("submit", submitTask);
  $("#task-form [name=profile]").addEventListener("change", () => {
    const controls = taskDialogRouteControls();
    if (controls.profile.value) applyAuthoritativeRouteControls(controls);
    else switchRouteControlsToCustom(controls);
  });
  $("#task-form [name=assignee]").addEventListener("input", () => {
    const controls = taskDialogRouteControls();
    controls.profile.value = "";
    applyAuthoritativeRouteControls(controls);
  });
  $("#task-form [name=runtime]").addEventListener("change", () => {
    switchRouteControlsToCustom(taskDialogRouteControls());
  });
  $("#task-form [name=status]").addEventListener("change", updateTaskScheduleVisibility);
  $("#github-form").addEventListener("submit", submitGitHubImport);
  $("#github-preview").addEventListener("click", () => runGitHubImport(true));
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
  $("#agent-preset").addEventListener("change", updateAgentPresetDescription);
  $("#apply-agent-preset").addEventListener("click", () => previewAgentPreset());
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
    const scheduledAt = data.get("status") === "scheduled" ? futureScheduleISO(data.get("scheduledAt")) : null;
    const route = authoritativeTaskRoute(data.get("profile"), data.get("assignee"), data.get("runtime"));
    await api(boardPath("/api/tasks"), { method: "POST", body: JSON.stringify({
      title: data.get("title"), body: data.get("body"), status: data.get("status"),
      assignee: route.assignee, runtime: route.runtime, priority: Number(data.get("priority")),
      tenant: data.get("tenant") || null, workspaceKind: data.get("workspaceKind"), workspace: data.get("workspace") || null,
      branch: data.get("branch") || null, maxRuntimeSeconds: data.get("maxRuntimeSeconds") ? Number(data.get("maxRuntimeSeconds")) : null,
      maxRetries: Number(data.get("maxRetries")) || 2, scheduledAt,
      skills: String(data.get("skills") || "").split(",").map((item) => item.trim()).filter(Boolean),
      goalMode: data.get("goalMode") === "on", goalMaxTurns: Number(data.get("goalMaxTurns")) || 20,
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
    cancelGraphLoad();
    state.board = board.slug; state.cursor = 0; localStorage.setItem("autogora.board", state.board);
    $("#board-dialog").close(); await loadBoards(); await loadBoard(); connectEvents();
  } catch (error) { toast(error.message, true); }
}

function openSettings() {
  const form = $("#settings-form"); const metadata = state.metadata; const settings = metadata.orchestration;
  const autopilot = settings.autopilot || {}; const coordination = autopilot.coordination || {}; const publication = autopilot.publication || {};
  form.elements.name.value = metadata.name; form.elements.description.value = metadata.description;
  form.elements.color.value = /^#[0-9a-f]{6}$/i.test(metadata.color) ? metadata.color : "#5b7cff";
  form.elements.defaultWorkdir.value = metadata.defaultWorkdir || ""; form.elements.autoDecompose.checked = settings.autoDecompose;
  form.elements.autoPromoteChildren.checked = settings.autoPromoteChildren;
  form.elements.plannerRuntime.value = settings.plannerRuntime; form.elements.autoDecomposePerTick.value = settings.autoDecomposePerTick;
  form.elements.plannerModel.value = settings.plannerModel || ""; form.elements.plannerProvider.value = settings.plannerProvider || "";
  form.elements.defaultProfile.value = settings.defaultProfile || ""; form.elements.finalizerProfile.value = settings.finalizerProfile || "";
  form.elements.autopilotEnabled.checked = Boolean(autopilot.enabled); form.elements.autoPlan.checked = Boolean(autopilot.autoPlan);
  form.elements.autoExecute.checked = Boolean(autopilot.autoExecute); form.elements.workspaceWrites.checked = Boolean(autopilot.workspaceWrites);
  form.elements.reviewGate.checked = Boolean(autopilot.reviewGate); form.elements.coordinatorMode.value = coordination.mode || "observe";
  form.elements.coordinatorProfile.value = coordination.profile || ""; form.elements.publicationMode.value = publication.mode || "manual";
  form.elements.publicationTargetBranch.value = publication.targetBranch || "main"; form.elements.publicationRemote.value = publication.remote || "origin";
  form.elements.publicationApproval.checked = Boolean(publication.requireApproval);
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
        finalizerProfile: data.get("finalizerProfile") || null, profiles,
        autopilot: {
          enabled: data.get("autopilotEnabled") === "on", autoPlan: data.get("autoPlan") === "on",
          autoExecute: data.get("autoExecute") === "on", workspaceWrites: data.get("workspaceWrites") === "on",
          reviewGate: data.get("reviewGate") === "on",
          coordination: { mode: data.get("coordinatorMode"), profile: data.get("coordinatorProfile") || null },
          publication: { mode: data.get("publicationMode"), targetBranch: data.get("publicationTargetBranch"), remote: data.get("publicationRemote"), requireApproval: data.get("publicationApproval") === "on" },
        } },
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
    await loadBoards();
    const configuration = await loadAgentConfiguration();
    await loadBoard(); connectEvents(); await refreshOperationalStatus();
    if (!configuration.exists && sessionStorage.getItem("autogora.agentSetupDeferred") !== "true") {
      await openAgentSettings({ firstRun: true });
    }
    window.setInterval(() => refreshOperationalStatus().catch(() => {}), 5000);
  }
  catch (error) { toast(error.message, true); }
}

main();
