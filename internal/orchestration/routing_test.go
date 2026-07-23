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
	fallback, finalizer := SelectProfileRoutes(profiles, &implementer, &reviewer, model.RuntimeCodex)
	if fallback.Name != "implementer" || finalizer.Name != "reviewer" || finalizer.Runtime != model.RuntimeGemini {
		t.Fatalf("selected routes = %#v %#v", fallback, finalizer)
	}
}

func TestSelectProfileRoutesSuppliesRunnableEmptyBoardFallback(t *testing.T) {
	fallback, finalizer := SelectProfileRoutes(nil, nil, nil, model.RuntimeCline)
	if fallback.Name != "cline-worker" || fallback.Runtime != model.RuntimeCline || finalizer.Name != fallback.Name || finalizer.Runtime != fallback.Runtime {
		t.Fatalf("fallback routes = %#v %#v", fallback, finalizer)
	}
}

func TestSelectProfileRoutesSkipsDisabledAndUsesPriority(t *testing.T) {
	disabled := ProfileRoute{Name: "disabled", Runtime: model.RuntimeCodex, Disabled: true, Priority: 100}
	backup := ProfileRoute{Name: "backup", Runtime: model.RuntimeClaude, Priority: 5}
	primary := ProfileRoute{Name: "primary", Runtime: model.RuntimeGemini, Priority: 10}
	profiles := ResolveProfileRoutes(nil, []ProfileRoute{disabled, backup, primary})
	fallback, finalizer := SelectProfileRoutes(profiles, nil, nil, model.RuntimeCodex)
	if fallback.Name != "primary" || finalizer.Name != "primary" || profiles[0].Name != "disabled" || profiles[1].Name != "primary" {
		t.Fatalf("unexpected profile selection: profiles=%#v fallback=%#v finalizer=%#v", profiles, fallback, finalizer)
	}
}
