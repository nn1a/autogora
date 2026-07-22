package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionCommand(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := run([]string{"version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("version returned %d: %s", code, stderr.String())
	}
	if got := stdout.String(); !strings.HasPrefix(got, "taskcircuit ") {
		t.Fatalf("unexpected version output %q", got)
	}
}
