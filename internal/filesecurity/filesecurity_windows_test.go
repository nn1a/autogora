//go:build windows

package filesecurity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsCurrentUserFileSecurityRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "handoff.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RestrictToCurrentUser(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateCurrentUserFile(path, info); err != nil {
		t.Fatal(err)
	}
}
