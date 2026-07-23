package processidentity

import (
	"os"
	"testing"
)

func TestCaptureAndInspectCurrentProcess(t *testing.T) {
	identity, err := Capture(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	matching := Inspect(os.Getpid(), &identity)
	if !matching.Alive || !matching.Verified || !matching.Matches {
		t.Fatalf("current process identity did not match: %+v", matching)
	}
	different := identity + "-different"
	mismatch := Inspect(os.Getpid(), &different)
	if !mismatch.Alive || !mismatch.Verified || mismatch.Matches {
		t.Fatalf("different process identity was accepted: %+v", mismatch)
	}
	unverified := Inspect(os.Getpid(), nil)
	if !unverified.Alive || unverified.Verified || unverified.Matches {
		t.Fatalf("missing process identity was treated as verified: %+v", unverified)
	}
}
