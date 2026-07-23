package dashboard

import (
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/boards"
)

func TestAutopilotUpdateRejectsCoordinatorActionLimitAboveAnalyzerBound(t *testing.T) {
	_, err := autopilotUpdate(map[string]any{"coordination": map[string]any{
		"maxActionsPerIncident": boards.MaxCoordinationActionsPerIncident + 1,
	}})
	if err == nil || !strings.Contains(err.Error(), "between 1 and 20") {
		t.Fatalf("unexpected error: %v", err)
	}
	update, err := autopilotUpdate(map[string]any{"coordination": map[string]any{
		"maxActionsPerIncident": boards.MaxCoordinationActionsPerIncident,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if update.Coordination == nil || update.Coordination.MaxActionsPerIncident == nil ||
		*update.Coordination.MaxActionsPerIncident != boards.MaxCoordinationActionsPerIncident {
		t.Fatalf("unexpected update: %#v", update)
	}
}
