//go:build !windows

package filesecurity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateCurrentUserFileRequiresExactModeAndRejectsSymlink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "handoff.json")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateCurrentUserFile(path, info); err == nil {
		t.Fatal("world-readable file passed security validation")
	}
	if err := RestrictToCurrentUser(path); err != nil {
		t.Fatal(err)
	}
	info, err = os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateCurrentUserFile(path, info); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "handoff-link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	info, err = os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateCurrentUserFile(link, info); err == nil {
		t.Fatal("symbolic link passed security validation")
	}
}
