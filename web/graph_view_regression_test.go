package webui

import (
	"strings"
	"testing"
)

func cssRule(t *testing.T, styles, selector string) string {
	t.Helper()
	start := strings.Index(styles, selector+" {")
	if start < 0 {
		t.Fatalf("CSS rule %q is missing", selector)
	}
	end := strings.Index(styles[start:], "}")
	if end < 0 {
		t.Fatalf("CSS rule %q is not closed", selector)
	}
	return styles[start : start+end]
}

func TestDashboardGraphViewPreservesBoardAndRelationshipSemantics(t *testing.T) {
	html := dashboardAsset(t, "index.html")
	javascript := dashboardAsset(t, "app.js")

	for _, marker := range []string{
		`data-board-view="graph"`,
		`title="Dependency and hierarchy diagram for the current board"`,
		`Graph diagrams task dependencies and hierarchy`,
	} {
		if !strings.Contains(html, marker) {
			t.Fatalf("graph view control marker %q is missing", marker)
		}
	}
	for _, marker := range []string{
		`const BOARD_VIEW_MODES = ["overview", "compact", "flow", "graph"]`,
		`const board = state.board`,
		`/api/graph?includeArchived=${includeArchived}`,
		`state.board !== board || state.boardView !== "graph"`,
		`requestID !== state.graphRequest`,
		`loadBoardGraph({ force: true })`,
		`boardGraphSignature(graph)`,
		`edge.prerequisiteId`,
		`edge.dependentId`,
		`edge.parentTaskId`,
		`edge.subtaskId`,
	} {
		if !strings.Contains(javascript, marker) {
			t.Fatalf("graph data contract marker %q is missing", marker)
		}
	}
}

func TestDashboardGraphViewIsNativeAccessibleAndBounded(t *testing.T) {
	html := dashboardAsset(t, "index.html")
	javascript := dashboardAsset(t, "app.js")
	styles := dashboardAsset(t, "styles.css")

	for _, marker := range []string{
		`<svg class="graph-canvas"`,
		`marker id="graph-dependency-arrow"`,
		`marker id="graph-satisfied-arrow"`,
		`class="graph-edge graph-hierarchy"`,
		`role="button" tabindex="0" data-graph-task=`,
		`<details class="graph-task-list">`,
		`other nodes stay dimmed for dependency context`,
		`graph.omittedNodeCount`,
		`Drag the background to pan`,
		`requestAnimationFrame(() =>`,
	} {
		if !strings.Contains(javascript, marker) {
			t.Fatalf("native graph behavior marker %q is missing", marker)
		}
	}
	if strings.Contains(html, `<script src="http`) {
		t.Fatal("graph view must not add a network-loaded JavaScript dependency")
	}

	boardRule := cssRule(t, styles, `.board[data-view="graph"]`)
	if !strings.Contains(boardRule, "overflow: hidden") {
		t.Fatalf("graph board does not contain page-level overflow: %s", boardRule)
	}
	viewportRule := cssRule(t, styles, ".graph-viewport")
	for _, marker := range []string{"max-width: 100%", "overflow: auto", "overscroll-behavior: contain"} {
		if !strings.Contains(viewportRule, marker) {
			t.Fatalf("graph viewport is missing %q: %s", marker, viewportRule)
		}
	}
}
