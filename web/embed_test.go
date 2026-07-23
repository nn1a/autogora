package webui

import (
	"strings"
	"testing"
)

func TestDashboardAssetsAreEmbedded(t *testing.T) {
	for _, name := range []string{"index.html", "app.js", "styles.css"} {
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
