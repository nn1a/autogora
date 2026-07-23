package mcpserver

import (
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/boards"
)

func TestAutopilotBoardUpdateRejectsCoordinatorActionLimitAboveAnalyzerBound(t *testing.T) {
	tooMany := boards.MaxCoordinationActionsPerIncident + 1
	_, err := autopilotBoardUpdate(&boardAutopilotInput{Coordination: &boardCoordinationInput{
		MaxActionsPerIncident: &tooMany,
	}})
	if err == nil || !strings.Contains(err.Error(), "between 1 and 20") {
		t.Fatalf("unexpected error: %v", err)
	}
	maximum := boards.MaxCoordinationActionsPerIncident
	update, err := autopilotBoardUpdate(&boardAutopilotInput{Coordination: &boardCoordinationInput{
		MaxActionsPerIncident: &maximum,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if update.Coordination == nil || update.Coordination.MaxActionsPerIncident == nil ||
		*update.Coordination.MaxActionsPerIncident != maximum {
		t.Fatalf("unexpected update: %#v", update)
	}
}
