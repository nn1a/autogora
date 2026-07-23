package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/dispatcher"
)

func dispatchTestApp(t *testing.T) *App {
	t.Helper()
	app := New(&bytes.Buffer{}, &bytes.Buffer{})
	app.Cwd = t.TempDir()
	app.Getenv = func(string) string { return "" }
	return app
}

func TestDispatchPassesExplicitAutopilotForOnceAndWatch(t *testing.T) {
	tests := []struct {
		name     string
		mode     string
		wantOnce bool
	}{
		{name: "once", mode: "--once", wantOnce: true},
		{name: "watch", mode: "--watch", wantOnce: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := dispatchTestApp(t)
			var captured dispatcher.Options
			app.DispatchRunner = func(_ context.Context, options dispatcher.Options) error {
				captured = options
				return nil
			}

			runApp(t, app, "dispatch", test.mode, "--autopilot")

			if !captured.Autopilot {
				t.Fatal("dispatch did not enable Autopilot")
			}
			if captured.Once != test.wantOnce {
				t.Fatalf("Once = %t, want %t", captured.Once, test.wantOnce)
			}
		})
	}
}

func TestDispatchAutopilotRemainsOptIn(t *testing.T) {
	app := dispatchTestApp(t)
	var captured dispatcher.Options
	app.DispatchRunner = func(_ context.Context, options dispatcher.Options) error {
		captured = options
		return nil
	}

	runApp(t, app, "dispatch", "--once")
	if captured.Autopilot {
		t.Fatal("dispatch enabled board automation without --autopilot")
	}

	runApp(t, app, "dispatch", "--watch", "--autopilot=false")
	if captured.Autopilot {
		t.Fatal("dispatch ignored --autopilot=false")
	}
}

func TestDispatchDoesNotExposePreReleaseDaemonAlias(t *testing.T) {
	app := dispatchTestApp(t)
	app.DispatchRunner = func(context.Context, dispatcher.Options) error {
		t.Fatal("removed daemon alias started the dispatcher")
		return nil
	}

	err := app.Run(context.Background(), []string{"daemon", "--force"})
	if err == nil || !strings.Contains(err.Error(), "unknown or not-yet-ported command: daemon") {
		t.Fatalf("removed daemon alias error = %v", err)
	}
}

func TestDispatchRejectsConflictingModes(t *testing.T) {
	app := dispatchTestApp(t)
	app.DispatchRunner = func(context.Context, dispatcher.Options) error {
		t.Fatal("dispatcher ran with conflicting modes")
		return nil
	}

	err := app.Run(context.Background(), []string{"dispatch", "--once", "--watch", "--autopilot"})
	if err == nil || !strings.Contains(err.Error(), "--once and --watch") {
		t.Fatalf("conflicting mode error = %v", err)
	}
}

func TestDispatchHelpExplainsAutopilotBoundary(t *testing.T) {
	for _, args := range [][]string{{"dispatch", "--help"}, {"help", "dispatch"}} {
		app := dispatchTestApp(t)
		output := runApp(t, app, args...)
		if !strings.Contains(output, "--autopilot") ||
			!strings.Contains(output, "Coordinator recovery") ||
			!strings.Contains(output, "does not enable automation by itself") {
			t.Fatalf("dispatch help %v omitted the Autopilot boundary: %s", args, output)
		}
	}
}
