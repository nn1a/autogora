package dispatcher

import (
	"testing"

	"github.com/nn1a/autogora/internal/boards"
)

func TestAutoDecomposeEnabledUsesOnePlannerAndObserverPolicy(t *testing.T) {
	base := boards.Metadata{}
	base.Orchestration.AutoDecompose = true
	base.Orchestration.Autopilot.Enabled = true
	base.Orchestration.Autopilot.AutoPlan = true
	cases := []struct {
		name     string
		metadata boards.Metadata
		options  Options
		want     bool
	}{
		{name: "board enabled", metadata: base, want: true},
		{
			name: "board disabled",
			metadata: func() boards.Metadata {
				value := base
				value.Orchestration.AutoDecompose = false
				return value
			}(),
		},
		{
			name:     "Autopilot planning enabled",
			metadata: base,
			options:  Options{Autopilot: true},
			want:     true,
		},
		{
			name: "Autopilot globally disabled",
			metadata: func() boards.Metadata {
				value := base
				value.Orchestration.Autopilot.Enabled = false
				return value
			}(),
			options: Options{Autopilot: true},
		},
		{
			name: "Autopilot AutoPlan disabled",
			metadata: func() boards.Metadata {
				value := base
				value.Orchestration.Autopilot.AutoPlan = false
				return value
			}(),
			options: Options{Autopilot: true},
		},
		{
			name:     "explicit disable wins",
			metadata: base,
			options:  Options{Autopilot: true, AutoDecompose: boolValue(false)},
		},
		{
			name: "explicit enable wins",
			metadata: func() boards.Metadata {
				value := base
				value.Orchestration.AutoDecompose = false
				value.Orchestration.Autopilot.Enabled = false
				value.Orchestration.Autopilot.AutoPlan = false
				return value
			}(),
			options: Options{Autopilot: true, AutoDecompose: boolValue(true)},
			want:    true,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := autoDecomposeEnabled(test.metadata, test.options); got != test.want {
				t.Fatalf("autoDecomposeEnabled() = %t, want %t", got, test.want)
			}
		})
	}
}
