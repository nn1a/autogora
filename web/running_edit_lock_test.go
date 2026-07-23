package webui

import (
	"strings"
	"testing"
)

func TestDashboardLocksTaskEditorWhileRunning(t *testing.T) {
	javascript, err := Files.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(javascript)
	for _, marker := range []string{
		`const editLocked = task.status === "running"`,
		`Execution settings are locked while this task is running.`,
		`Priority remains editable.`,
		`drawerRunningLockedSelectors.forEach`,
		`control.disabled = true`,
		`detail.task.status === "running"`,
		`{ expectedUpdatedAt: state.drawerVersion, priority: Number($("#edit-priority").value) }`,
		`data-terminate-run=`,
		`id="comment-form"`,
	} {
		if !strings.Contains(source, marker) {
			t.Fatalf("running editor lock marker %q is missing", marker)
		}
	}
}
