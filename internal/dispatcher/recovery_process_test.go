package dispatcher

import (
	"testing"

	"github.com/nn1a/autogora/internal/processidentity"
)

func TestManagedWorkerTreeAlivePreservesMismatchedPIDGroup(t *testing.T) {
	tests := []struct {
		name        string
		state       processidentity.State
		descendants bool
		want        bool
	}{
		{
			name: "verified worker leader",
			state: processidentity.State{
				Alive: true, Verified: true, Matches: true,
			},
			want: true,
		},
		{
			name: "unverified occupied pid",
			state: processidentity.State{
				Alive: true,
			},
			want: true,
		},
		{
			name: "reused pid with original group descendants",
			state: processidentity.State{
				Alive: true, Verified: true, Matches: false,
			},
			descendants: true,
			want:        true,
		},
		{
			name: "reused pid without original group",
			state: processidentity.State{
				Alive: true, Verified: true, Matches: false,
			},
			want: false,
		},
		{
			name:        "dead leader with descendants",
			descendants: true,
			want:        true,
		},
		{
			name: "fully stopped",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := managedWorkerTreeAlive(
				test.state,
				test.descendants,
			); got != test.want {
				t.Fatalf("managedWorkerTreeAlive() = %v, want %v", got, test.want)
			}
		})
	}
}
