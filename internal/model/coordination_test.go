package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCoordinationProposalJSONHidesAttemptBinding(t *testing.T) {
	attemptID := "ca_internal-audit-binding"
	encoded, err := json.Marshal(CoordinationProposal{
		ID:        "cp_public",
		AttemptID: &attemptID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "attemptId") ||
		strings.Contains(string(encoded), attemptID) {
		t.Fatalf("coordination attempt binding leaked through proposal JSON: %s", encoded)
	}
}
