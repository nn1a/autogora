package cli

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDashboardCancellationClosesEventStreamAndReturnsSuccess(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("AUTOGORA_CONFIG", filepath.Join(directory, "agents.json"))
	stdoutReader, stdoutWriter := io.Pipe()
	defer stdoutReader.Close()
	defer stdoutWriter.Close()
	stderr := &bytes.Buffer{}
	app := New(stdoutWriter, stderr)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- app.Run(ctx, []string{
			"dashboard",
			"--port", "0",
			"--token", testDashboardToken,
			"--db", filepath.Join(directory, "autogora.db"),
		})
	}()

	line, err := bufio.NewReader(stdoutReader).ReadString('\n')
	if err != nil {
		cancel()
		t.Fatalf("read dashboard URL: %v", err)
	}
	dashboardURL := strings.TrimSpace(line)
	parsed, err := url.Parse(dashboardURL)
	if err != nil {
		cancel()
		t.Fatalf("parse dashboard URL %q: %v", dashboardURL, err)
	}
	streamURL := parsed.Scheme + "://" + parsed.Host + "/api/events/stream?token=" + url.QueryEscape(testDashboardToken)
	stream, err := http.Get(streamURL)
	if err != nil {
		cancel()
		t.Fatalf("open dashboard event stream: %v", err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("event stream status = %d", stream.StatusCode)
	}

	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("dashboard cancellation returned an error: %v (stderr=%s)", err, stderr.String())
		}
	case <-time.After(time.Second):
		t.Fatal("dashboard did not stop promptly after cancellation")
	}
}

const testDashboardToken = "test-cli-dashboard-token-32-characters"
