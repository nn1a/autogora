package webui

import (
	"strings"
	"testing"
)

func dashboardAsset(t *testing.T, name string) string {
	t.Helper()
	contents, err := Files.ReadFile(name)
	if err != nil {
		t.Fatalf("read embedded %s: %v", name, err)
	}
	return string(contents)
}

func TestDashboardPersistsAccessibleBoardViews(t *testing.T) {
	html := dashboardAsset(t, "index.html")
	javascript := dashboardAsset(t, "app.js")
	styles := dashboardAsset(t, "styles.css")

	for _, marker := range []string{
		`class="board-view-controls" aria-label="Board view controls"`,
		`role="group" aria-labelledby="stage-focus-label"`,
		`aria-describedby="stage-focus-help"`,
		`data-stage-focus="planning"`,
		`data-stage-focus="execution"`,
		`data-stage-focus="archive"`,
		`role="group" aria-labelledby="board-view-label"`,
		`aria-describedby="board-view-help"`,
		`data-board-view="overview"`,
		`data-board-view="compact"`,
		`data-board-view="flow"`,
		`id="board-view-summary"`,
		`title="Responsive status grids with full task cards"`,
		`title="Denser status grids with condensed task cards"`,
		`title="Separate stage panels that emphasize workflow order"`,
		`title="Dependency and hierarchy diagram for the current board"`,
		`Focus shows one workflow stage at a time`,
		`Flow separates workflow stages, and Graph diagrams task dependencies and hierarchy`,
	} {
		if !strings.Contains(html, marker) {
			t.Fatalf("board view control marker %q is missing", marker)
		}
	}
	for _, marker := range []string{
		`localStorage.getItem("autogora.boardStageFocus")`,
		`localStorage.getItem("autogora.boardView")`,
		`localStorage.setItem("autogora.boardStageFocus", value)`,
		`localStorage.setItem("autogora.boardView", value)`,
		`board.dataset.view = state.boardView`,
		`board.dataset.focus = state.stageFocus`,
		`state.stageFocus === "all" || stage.id === state.stageFocus`,
		`["ArrowLeft", "ArrowRight", "Home", "End"]`,
		`button.setAttribute("aria-pressed"`,
	} {
		if !strings.Contains(javascript, marker) {
			t.Fatalf("board view behavior marker %q is missing", marker)
		}
	}
	for _, marker := range []string{
		`.board[data-view="flow"][data-focus="all"]`,
		`repeat(auto-fit, minmax(min(360px, 100%), 1fr))`,
		`.board[data-view="compact"] .card-summary { display: none; }`,
		`.segmented-control button[aria-pressed="true"]`,
		`overflow-x: hidden`,
	} {
		if !strings.Contains(styles, marker) {
			t.Fatalf("board view style marker %q is missing", marker)
		}
	}
}

func TestDashboardColumnsUsePageScrollByDefault(t *testing.T) {
	styles := dashboardAsset(t, "styles.css")
	start := strings.Index(styles, ".column-body {")
	if start < 0 {
		t.Fatal("column body rule is missing")
	}
	end := strings.Index(styles[start:], "}")
	if end < 0 {
		t.Fatal("column body rule is not closed")
	}
	rule := styles[start : start+end]
	if !strings.Contains(rule, "overflow: visible") {
		t.Fatalf("column body does not use page scrolling: %s", rule)
	}
	for _, obsolete := range []string{
		"28vh",
		"overflow-y: auto",
		"scrollbar-gutter",
		"overscroll-behavior",
	} {
		if strings.Contains(rule, obsolete) {
			t.Fatalf("column body still uses internal scrolling marker %q: %s", obsolete, rule)
		}
	}
}

func TestDashboardDoesNotMaskPageOverflowGlobally(t *testing.T) {
	styles := dashboardAsset(t, "styles.css")
	start := strings.Index(styles, "\nbody {")
	if start < 0 {
		t.Fatal("body rule is missing")
	}
	end := strings.Index(styles[start:], "}")
	if end < 0 {
		t.Fatal("body rule is not closed")
	}
	rule := styles[start : start+end]
	if strings.Contains(rule, "overflow-x") || strings.Contains(rule, "overflow:") {
		t.Fatalf("global body rule masks layout overflow: %s", rule)
	}
	if !strings.Contains(styles, ".board {") ||
		!strings.Contains(styles, "max-width: 100%") ||
		!strings.Contains(styles, "overflow-x: hidden") {
		t.Fatal("board does not bound its own responsive layout")
	}
}

func TestDashboardExplainsSwarmBoundary(t *testing.T) {
	html := dashboardAsset(t, "index.html")
	for _, marker := range []string{
		`id="new-swarm"`,
		`aria-describedby="swarm-button-help"`,
		`Advanced DAG template: parallel workers, verifier, and synthesizer`,
		`class="swarm-help" aria-label="About Swarm"`,
		`parallel worker tasks followed by verifier and synthesizer dependencies`,
		`separate from normal Autopilot execution and Coordinator recovery`,
	} {
		if !strings.Contains(html, marker) {
			t.Fatalf("Swarm help marker %q is missing", marker)
		}
	}
}
