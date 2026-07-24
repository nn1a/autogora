package processguard

import (
	"errors"
	"runtime"
	"strings"
	"testing"
)

func TestAutomaticMutationContainmentMatchesTeardownProof(t *testing.T) {
	available := AutomaticMutationContainmentAvailable()
	if available != TeardownProofAvailable() {
		t.Fatalf(
			"automatic mutation containment = %t, teardown proof = %t",
			available,
			TeardownProofAvailable(),
		)
	}
	if available != (runtime.GOOS == "linux") {
		t.Fatalf(
			"automatic mutation containment = %t on %s, want Linux only",
			available,
			runtime.GOOS,
		)
	}
	if available {
		if reason := AutomaticMutationContainmentUnsupportedReason(); reason != "" {
			t.Fatalf("supported platform returned unsupported reason %q", reason)
		}
		if err := RequireAutomaticMutationContainment("test mutation"); err != nil {
			t.Fatalf("supported mutation rejected: %v", err)
		}
		return
	}

	reason := AutomaticMutationContainmentUnsupportedReason()
	if strings.TrimSpace(reason) == "" {
		t.Fatal("unsupported platform returned no capability reason")
	}
	err := RequireAutomaticMutationContainment("test mutation")
	if !errors.Is(err, ErrAutomaticMutationContainmentUnavailable) {
		t.Fatalf("capability error = %v", err)
	}
	var typed *AutomaticMutationContainmentError
	if !errors.As(err, &typed) || typed.Operation != "test mutation" {
		t.Fatalf("typed capability error = %#v", typed)
	}
}
