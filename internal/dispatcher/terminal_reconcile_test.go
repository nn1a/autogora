package dispatcher

import (
	"testing"

	"github.com/nn1a/autogora/internal/model"
)

func TestPendingGoalCompletionRequestMatchesCommittedResponseLoss(t *testing.T) {
	summary := "Goal accepted after 3 turn(s): acceptance criteria passed"
	request := &model.TerminalRequest{
		Kind:    "complete",
		Summary: &summary,
		Metadata: map[string]any{
			"goalMode":    true,
			"turns":       float64(3),
			"judgeReason": "acceptance criteria passed",
		},
		Artifacts: []string{},
	}
	if !pendingGoalCompletionRequestMatches(
		request,
		summary,
		3,
		"acceptance criteria passed",
	) {
		t.Fatalf("matching committed request was not reconciled: %+v", request)
	}
}

func TestPendingGoalCompletionRequestRejectsDifferentIntent(t *testing.T) {
	summary := "Goal accepted after 3 turn(s): acceptance criteria passed"
	base := model.TerminalRequest{
		Kind:    "complete",
		Summary: &summary,
		Metadata: map[string]any{
			"goalMode":    true,
			"turns":       float64(3),
			"judgeReason": "acceptance criteria passed",
		},
		Artifacts: []string{},
	}
	finalized := "2026-07-24T00:00:00Z"
	tests := []struct {
		name    string
		mutate  func(*model.TerminalRequest)
		turn    int
		reason  string
		summary string
	}{
		{
			name: "finalized",
			mutate: func(request *model.TerminalRequest) {
				request.FinalizedAt = &finalized
			},
			turn: 3, reason: "acceptance criteria passed", summary: summary,
		},
		{
			name: "different turn",
			mutate: func(*model.TerminalRequest) {
			},
			turn: 4, reason: "acceptance criteria passed", summary: summary,
		},
		{
			name: "different reason",
			mutate: func(*model.TerminalRequest) {
			},
			turn: 3, reason: "different judgment", summary: summary,
		},
		{
			name: "different summary",
			mutate: func(*model.TerminalRequest) {
			},
			turn: 3, reason: "acceptance criteria passed", summary: "other",
		},
		{
			name: "unexpected artifact",
			mutate: func(request *model.TerminalRequest) {
				request.Artifacts = []string{"result.json"}
			},
			turn: 3, reason: "acceptance criteria passed", summary: summary,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := base
			request.Metadata = map[string]any{
				"goalMode":    true,
				"turns":       float64(3),
				"judgeReason": "acceptance criteria passed",
			}
			request.Artifacts = append([]string(nil), base.Artifacts...)
			test.mutate(&request)
			if pendingGoalCompletionRequestMatches(
				&request,
				test.summary,
				test.turn,
				test.reason,
			) {
				t.Fatalf("different intent was reconciled: %+v", request)
			}
		})
	}
}
