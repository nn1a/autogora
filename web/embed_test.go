package webui

import (
	"strings"
	"testing"
)

func TestDashboardAssetsAreEmbedded(t *testing.T) {
	for _, name := range []string{"index.html", "app.js", "styles.css", "favicon.svg"} {
		contents, err := Files.ReadFile(name)
		if err != nil {
			t.Fatalf("read embedded %s: %v", name, err)
		}
		if len(contents) == 0 {
			t.Fatalf("embedded %s is empty", name)
		}
	}
}

func TestDashboardIncludesAgentOnboardingAndEffectiveRoutes(t *testing.T) {
	html, err := Files.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	javascript, err := Files.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{`id="agents-dialog"`, `id="agent-settings"`, `name="allowWrites"`} {
		if !strings.Contains(string(html), marker) {
			t.Fatalf("agent setup marker %q is missing", marker)
		}
	}
	for _, marker := range []string{"/api/agents/detect", "/api/agents/effective", "payload.profiles", "fallback from"} {
		if !strings.Contains(string(javascript), marker) {
			t.Fatalf("agent routing marker %q is missing", marker)
		}
	}
	if strings.Contains(string(javascript), `"kanban.`) {
		t.Fatal("pre-release dashboard still uses the old kanban storage namespace")
	}
}

func TestDashboardGroupsResponsiveWorkflowStages(t *testing.T) {
	javascript, err := Files.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	styles, err := Files.ReadFile("styles.css")
	if err != nil {
		t.Fatal(err)
	}

	for _, marker := range []string{
		`const WORKFLOW_STAGES`,
		`Planning workflow stage`,
		`Execution workflow stage`,
		`Archive workflow stage`,
		`class="board-stage-grid"`,
		`class="column-body" role="region"`,
		`data-status="${status}"`,
		`data-create-status="${status}"`,
	} {
		if !strings.Contains(string(javascript), marker) {
			t.Fatalf("workflow stage marker %q is missing", marker)
		}
	}

	for _, marker := range []string{
		`@media (max-width: 1700px)`,
		`grid-template-columns: repeat(8, minmax(0, 1fr))`,
		`grid-template-columns: repeat(4, minmax(0, 1fr))`,
		`@media (max-width: 1200px)`,
		`grid-template-columns: repeat(2, minmax(0, 1fr))`,
		`@media (max-width: 600px)`,
		`grid-template-columns: minmax(0, 1fr)`,
		`max-height: clamp(180px, 28vh, 320px)`,
		`overflow-x: hidden`,
		`overflow-wrap: anywhere`,
	} {
		if !strings.Contains(string(styles), marker) {
			t.Fatalf("responsive board marker %q is missing", marker)
		}
	}

	for _, obsolete := range []string{`grid-auto-flow: column`, `grid-auto-columns:`, `scroll-snap-type: x`} {
		if strings.Contains(string(styles), obsolete) {
			t.Fatalf("horizontal board layout marker %q is still present", obsolete)
		}
	}
}

func TestDashboardStatusColorsRespectStrictStylePolicy(t *testing.T) {
	html, err := Files.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	javascript, err := Files.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	styles, err := Files.ReadFile("styles.css")
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(html), "style=") || strings.Contains(string(javascript), "style=") || strings.Contains(string(javascript), ".style.setProperty") {
		t.Fatal("dashboard uses inline styles that the Content-Security-Policy blocks")
	}
	for _, marker := range []string{"status-${task.status}", "status-${status}", ".status-triage", ".status-running", ".status-done"} {
		if !strings.Contains(string(javascript)+string(styles), marker) {
			t.Fatalf("CSP-safe status marker %q is missing", marker)
		}
	}
}

func TestDashboardCarriesTaskVersionsThroughInteractiveMutations(t *testing.T) {
	javascript, err := Files.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{
		`data-task-version="${escapeHtml(task.updatedAt)}"`,
		`application/x-autogora-updated-at`,
		`const expectedUpdatedAt = state.drawerVersion`,
		`mutation: { ...mutation, expectedUpdatedAt }`,
		`state.selected = new Set(result.errors.map`,
		`taskWindow.truncated`,
		`${taskWindow.returned} shown`,
	} {
		if !strings.Contains(string(javascript), marker) {
			t.Fatalf("optimistic Web mutation marker %q is missing", marker)
		}
	}
}
