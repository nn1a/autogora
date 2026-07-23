package webui

import (
	"strings"
	"testing"
)

func TestTaskFormsRespectAuthoritativeConfiguredProfiles(t *testing.T) {
	javascript := dashboardAsset(t, "app.js")
	for _, marker := range []string{
		`function authoritativeTaskRoute(profileName, assignee, runtime)`,
		`const profile = profileByName(profileName) || profileByName(customAssignee)`,
		`assignee: profile.name, runtime: profile.runtime`,
		`model: profile.model || "CLI default (unpinned)"`,
		`function switchRouteControlsToCustom(controls)`,
		`if (profileByName(controls.assignee.value)) controls.assignee.value = ""`,
		`const route = authoritativeTaskRoute(data.get("profile"), data.get("assignee"), data.get("runtime"))`,
		`assignee: route.assignee, runtime: route.runtime`,
		`const selectedRoute = authoritativeTaskRoute("", task.assignee, task.runtime)`,
		`<option ${item === selectedRoute.runtime ? "selected" : ""}>`,
		`const route = applyAuthoritativeRouteControls(routeControls())`,
	} {
		if !strings.Contains(javascript, marker) {
			t.Fatalf("authoritative task route marker %q is missing", marker)
		}
	}
	if strings.Contains(javascript,
		`profile.name === (task.assignee || "") && profile.runtime === task.runtime`) {
		t.Fatal("drawer still treats a configured assignee with stale task runtime as a custom route")
	}
}

func TestCustomRouteChangesCannotRetainAConfiguredProfileID(t *testing.T) {
	javascript := dashboardAsset(t, "app.js")
	for _, marker := range []string{
		`if (controls.profile.value) applyAuthoritativeRouteControls(controls)`,
		`else switchRouteControlsToCustom(controls)`,
		`controls.profile.value = ""`,
		`switchRouteControlsToCustom(taskDialogRouteControls())`,
		`switchRouteControlsToCustom(routeControls())`,
		`title="Effective worker runtime"`,
	} {
		if !strings.Contains(javascript, marker) {
			t.Fatalf("custom task route marker %q is missing", marker)
		}
	}
}
