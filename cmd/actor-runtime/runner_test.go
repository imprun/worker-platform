package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/imprun/worker-platform/internal/apppackage"
)

func TestAttemptRequiresInput(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/attempt", strings.NewReader(`{"runId":"test","attempt":1}`))
	appRunner{}.routes().ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing input returned HTTP %d", w.Code)
	}
}

func TestAttemptInvokesEchoWithRunAndAttempt(t *testing.T) {
	dir := t.TempDir()
	entrypoint := filepath.Join(dir, "echo-app.exe")
	build := exec.CommandContext(t.Context(), "go", "build", "-trimpath", "-o", entrypoint, "../../examples/echo/app")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Echo app: %v\n%s", err, output)
	}
	handler := appRunner{apppackage.Installed{Dir: dir, Entrypoint: entrypoint}}.routes()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// The delay in the input is for the first Attempt. A retry must immediately process the same input.
	w := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/attempt",
		strings.NewReader(`{"runId":"echo-run","attempt":2,"input":{"message":"hello","waitSeconds":75}}`))
	handler.ServeHTTP(w, r)
	const want = `{"message":"hello","version":"v1","runId":"echo-run","attempt":2}`
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != want {
		t.Fatalf("unexpected Echo retry: HTTP %d, response %s", w.Code, w.Body.String())
	}
}

func TestAppOutputBoundAppliesToCopiedStreams(t *testing.T) {
	var output boundedOutput
	// Wrapping the reader here exercises the writer-side io.Copy dispatch.
	reader := struct{ io.Reader }{strings.NewReader(strings.Repeat("a", maxOutputBytes+1))}
	if _, err := io.Copy(&output, reader); err == nil || output.buffer.Len() > maxOutputBytes {
		t.Fatalf("output limit did not apply: bytes=%d, error=%v", output.buffer.Len(), err)
	}
}
