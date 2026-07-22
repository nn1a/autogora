package orchestration

import (
	"testing"

	"github.com/nn1a/autogora/internal/model"
)

func TestResolveAndSelectProfileRoutes(t *testing.T) {
	reviewer, implementer := "reviewer", "implementer"
	tasks := []model.Task{
		{Assignee: &reviewer, Runtime: model.RuntimeCodex},
		{Assignee: &implementer, Runtime: model.RuntimeClaude},
	}
	profiles := ResolveProfileRoutes(tasks, []ProfileRoute{{Name: "reviewer", Runtime: model.RuntimeGemini, Description: "checks work"}})
	if len(profiles) != 2 || profiles[0].Runtime != model.RuntimeGemini || profiles[0].Description == "" || profiles[1].Name != "implementer" {
		t.Fatalf("resolved profiles = %#v", profiles)
	}
	fallback, orchestrator := SelectProfileRoutes(profiles, &implementer, &reviewer, model.RuntimeCodex)
	if fallback.Name != "implementer" || orchestrator.Name != "reviewer" || orchestrator.Runtime != model.RuntimeGemini {
		t.Fatalf("selected routes = %#v %#v", fallback, orchestrator)
	}
}

func TestSelectProfileRoutesSuppliesRunnableEmptyBoardFallback(t *testing.T) {
	fallback, orchestrator := SelectProfileRoutes(nil, nil, nil, model.RuntimeCline)
	if fallback.Name != "cline-worker" || fallback.Runtime != model.RuntimeCline || orchestrator != fallback {
		t.Fatalf("fallback routes = %#v %#v", fallback, orchestrator)
	}
}
