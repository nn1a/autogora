package webui

import (
	"strings"
	"testing"
)

func TestAutomationCenterUsesAccessibleTaskOrientedTabs(t *testing.T) {
	html := dashboardAsset(t, "index.html")
	for _, marker := range []string{
		`<h2>Automation Center</h2>`,
		`id="automation-help-language"`,
		`role="tablist" aria-label="Automation Center sections"`,
		`role="tab" aria-controls="activity-content"`,
		`data-activity-tab="overview"`,
		`data-activity-tab="runs"`,
		`data-activity-tab="recovery"`,
		`data-activity-tab="publishing"`,
		`data-activity-tab="events"`,
		`role="tabpanel" aria-labelledby="automation-tab-overview"`,
	} {
		if !strings.Contains(html, marker) {
			t.Fatalf("Automation Center marker %q is missing", marker)
		}
	}
	if strings.Contains(html, "Automation & activity") {
		t.Fatal("the old linear activity heading is still present")
	}
}

func TestAutomationCenterExplainsRolesAndPersistsHelpLanguage(t *testing.T) {
	javascript := dashboardAsset(t, "app.js")
	for _, marker := range []string{
		`const AUTOMATION_HELP =`,
		`Planner: "Coding-agent role for the normal Triage path`,
		`Coordinator: "Coding-agent role for exceptional recovery`,
		`Supervisor, Dispatcher, and Publisher are deterministic host services with no coding-agent model.`,
		`Supervisor, Dispatcher, Publisher는 코딩 에이전트 모델 없이 동작하는 결정론적 호스트 서비스입니다.`,
		`Planner는 일반 Triage 작업을 처리합니다.`,
		`Coordinator는 그래프에 예외적인 개입이 필요할 때만 복구안을 제시합니다.`,
		`localStorage.getItem("autogora.automationHelpLanguage")`,
		`localStorage.setItem("autogora.automationHelpLanguage"`,
		`localStorage.getItem("autogora.automationCenterTab")`,
		`localStorage.setItem("autogora.automationCenterTab"`,
		`["ArrowLeft", "ArrowRight", "Home", "End"]`,
		`button.setAttribute("aria-selected"`,
	} {
		if !strings.Contains(javascript, marker) {
			t.Fatalf("Automation Center help marker %q is missing", marker)
		}
	}
}

func TestAutomationCenterUsesPublicCASRecoveryAndPublicationActions(t *testing.T) {
	javascript := dashboardAsset(t, "app.js")
	for _, marker := range []string{
		`api(boardPathFor(board, "/api/coordination"), { signal })`,
		"`/api/coordination/incidents/${encodeURIComponent(incident.id)}`",
		"`/api/coordination/proposals/${encodeURIComponent(proposal.id)}/${action}`",
		"`/api/coordination/incidents/${encodeURIComponent(entry.incident.id)}/dismiss`",
		`expectedUpdatedAt: proposal.updatedAt`,
		`expectedGraphRevision: proposal.expectedGraphRevision`,
		`expectedGraphRevision: entry.incident.graphRevision`,
		`api(boardPathFor(board, "/api/publications?limit=100"), { signal })`,
		"`/api/publications/${encodeURIComponent(item.id)}/${action}`",
		`const body = { expectedUpdatedAt: item.updatedAt }`,
		`button.dataset.proposalVersion !== proposal.updatedAt`,
		`button.dataset.publicationVersion !== item.updatedAt`,
		`Data was refreshed; review the current state and try again.`,
	} {
		if !strings.Contains(javascript, marker) {
			t.Fatalf("Automation Center mutation marker %q is missing", marker)
		}
	}
	if strings.Contains(javascript, "claim"+"Token") {
		t.Fatal("the WebUI must not read or submit internal lease credentials")
	}
}

func TestAutomationCenterRendersReadableRecoveryAndCollapsedEventDetails(t *testing.T) {
	javascript := dashboardAsset(t, "app.js")
	for _, marker := range []string{
		`function coordinationActionDescription(action)`,
		`<ol class="automation-action-list">`,
		`<p class="automation-rationale">`,
		`<details class="event-details" data-automation-detail="event:`,
		`<summary>Raw details</summary>`,
		`<p class="event-summary">`,
		`function safeExternalURL(value)`,
		`data-publication-action="complete"`,
	} {
		if !strings.Contains(javascript, marker) {
			t.Fatalf("readable Automation Center marker %q is missing", marker)
		}
	}
	if strings.Contains(javascript, `<details class="event-details" open`) {
		t.Fatal("raw event details are expanded by default")
	}
}

func TestAutomationCenterShowsCompleteSafeCreateTaskProposal(t *testing.T) {
	javascript := dashboardAsset(t, "app.js")
	for _, marker := range []string{
		`function coordinationTaskDraft(task = {})`,
		`["Key", task.key || "—"]`,
		`["Title", task.title || "—"]`,
		`["Assignee", task.assignee || "Unassigned"]`,
		`["Runtime", task.runtime || "Default"]`,
		`["Workflow role", task.workflowRole || "worker"]`,
		`["Priority", task.priority ?? 0]`,
		`["Parent task", task.parentTaskId || "None"]`,
		`["Prerequisites", (task.prerequisites || []).length`,
		`["Dependents", (task.dependents || []).length`,
		`<div class="markdown">${markdown(task.body)}</div>`,
		`<summary>Review full action details</summary>`,
		`data-automation-detail="proposal:`,
	} {
		if !strings.Contains(javascript, marker) {
			t.Fatalf("complete create_task review marker %q is missing", marker)
		}
	}
}

func TestAutomationCenterShowsHostAndCodingAgentConfiguration(t *testing.T) {
	javascript := dashboardAsset(t, "app.js")
	for _, marker := range []string{
		`kind: "Host service"`,
		`kind: "Coding agent"`,
		`function plannerRoute(data)`,
		`config.defaults?.plannerAgents`,
		`function coordinatorRoute(data)`,
		`data.coordination.policy?.profile`,
		`config.defaults?.coordinatorAgents`,
		`["Model", route.model || "CLI default (unpinned)"]`,
		`["Provider", route.provider || "CLI default"]`,
		`function supervisorPresentation(supervisor = {})`,
		`Restarting · attempt ${restartCount + 1}`,
		`function publicationRoleState(publications)`,
		`Retry required`,
		`ready for manual completion`,
	} {
		if !strings.Contains(javascript, marker) {
			t.Fatalf("role configuration marker %q is missing", marker)
		}
	}
}

func TestAutomationCenterRejectsStaleLoadsAndCachesRecoveryDetails(t *testing.T) {
	javascript := dashboardAsset(t, "app.js")
	for _, marker := range []string{
		`let automationLoadGeneration = 0`,
		`let automationLoadController = null`,
		`const automationRecoveryCache = new Map()`,
		`const board = state.board`,
		`const generation = ++automationLoadGeneration`,
		`automationLoadController?.abort()`,
		`const controller = new AbortController()`,
		`currentAutomationRequest(board, generation, signal)`,
		`state.automationData?.board === board`,
		`function recoveryCacheKey(board, incident)`,
		`automationRecoveryCache.get(recoveryCacheKey(board, incident))`,
		`state.automationData !== data`,
	} {
		if !strings.Contains(javascript, marker) {
			t.Fatalf("stale load isolation marker %q is missing", marker)
		}
	}
}

func TestAutomationCenterPreservesExpandedDetailsAndFocus(t *testing.T) {
	javascript := dashboardAsset(t, "app.js")
	for _, marker := range []string{
		`function captureAutomationViewState()`,
		`details[data-automation-detail]`,
		`focusKey: focusTarget?.dataset.automationFocus`,
		`focusDetailsKey: active?.tagName === "SUMMARY"`,
		`function restoreAutomationViewState(view)`,
		`details.open = expanded.has(details.dataset.automationDetail)`,
		`target = details && $("summary", details)`,
		`(target || content).focus({ preventScroll: true })`,
		`data-automation-focus="coordination:approve:`,
		`data-automation-focus="publication:retry:`,
		`setAutomationBusy(true)`,
	} {
		if !strings.Contains(javascript, marker) {
			t.Fatalf("Automation Center view preservation marker %q is missing", marker)
		}
	}
}

func TestAutomationCenterIsResponsiveWithoutDialogOverflow(t *testing.T) {
	styles := dashboardAsset(t, "styles.css")
	for _, marker := range []string{
		`dialog.dialog-automation { width: min(1040px, calc(100vw - 32px)); overflow-x: hidden; }`,
		`.automation-tabs {`,
		`grid-template-columns: repeat(5, minmax(0, 1fr));`,
		`.automation-role-grid {`,
		`repeat(auto-fit, minmax(min(260px, 100%), 1fr))`,
		`.event-payload {`,
		`word-break: break-word`,
		`.automation-tabs { grid-template-columns: repeat(2, minmax(0, 1fr)); }`,
	} {
		if !strings.Contains(styles, marker) {
			t.Fatalf("responsive Automation Center marker %q is missing", marker)
		}
	}
}
