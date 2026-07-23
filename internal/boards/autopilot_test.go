package boards

import "testing"

func TestNormalizeAutopilotBoundsCoordinationBudgets(t *testing.T) {
	policy := normalizeAutopilot(AutopilotSettings{Coordination: CoordinationSettings{
		Mode: CoordinationModeAuto, IdleSeconds: MinCoordinationIdleSeconds,
		MaxCallsPerHour:       MaxCoordinationCallsPerHour + 1,
		MaxActionsPerIncident: MaxCoordinationActionsPerIncident + 1,
	}})
	if policy.Coordination.MaxCallsPerHour != MaxCoordinationCallsPerHour {
		t.Fatalf("max calls per hour = %d, want %d", policy.Coordination.MaxCallsPerHour, MaxCoordinationCallsPerHour)
	}
	if policy.Coordination.MaxActionsPerIncident != MaxCoordinationActionsPerIncident {
		t.Fatalf("max actions per incident = %d, want %d", policy.Coordination.MaxActionsPerIncident, MaxCoordinationActionsPerIncident)
	}
}
