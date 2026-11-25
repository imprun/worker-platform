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

func TestAttemptInvokesEcho(t *testing.T) {
	dir := t.TempDir()
	entrypoint := filepath.Join(dir, "echo-app.exe")
	build := exec.CommandContext(t.Context(), "go", "build", "-trimpath", "-o", entrypoint, "../../examples/echo/app")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Echo app: %v\n%s", err, output)
	}
	handler := appRunner{apppackage.Installed{Dir: dir, Entrypoint: entrypoint}}.routes()
	tests := []struct {
		name    string
		request string
		status  int
		body    string
	}{
		{
			name:    "retry skips first attempt delay",
			request: `{"runId":"echo-run","attempt":2,"input":{"message":"hello","waitSeconds":75}}`,
			status:  http.StatusOK,
			body:    `{"message":"hello","version":"v1","runId":"echo-run","attempt":2}`,
		},
		{
			name:    "expired site session",
			request: `{"runId":"echo-run","attempt":1,"input":{"message":"hello"},"siteSession":{"ref":"demo-account","version":1,"state":{"mockCookie":"expired-for-echo"}}}`,
			status:  http.StatusPreconditionRequired,
			body:    `{"code":"site_session_expired"}`,
		},
		{
			name:    "renewed site session",
			request: `{"runId":"echo-run","attempt":2,"input":{"message":"hello"},"siteSession":{"ref":"demo-account","version":2,"state":{"mockCookie":"approved-for-echo"}}}`,
			status:  http.StatusOK,
			body:    `{"message":"hello","version":"v1","runId":"echo-run","attempt":2,"siteSessionVersion":2}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			w := httptest.NewRecorder()
			r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/attempt", strings.NewReader(tt.request))
			handler.ServeHTTP(w, r)
			if w.Code != tt.status || strings.TrimSpace(w.Body.String()) != tt.body {
				t.Fatalf("unexpected Echo response: HTTP %d, body %s", w.Code, w.Body.String())
			}
		})
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

func TestPackageDigestIdentifiesInvokeResponse(t *testing.T) {
	const digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	const reply = `{"hello":"world"}`
	handler := withPackageDigest(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(reply))
	}), digest)
	for _, path := range []string{"/invoke", "/attempt"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(reply))
		r.Header.Set("X-Imprun-Worker-Package-Digest", "임의 헤더")
		handler.ServeHTTP(w, r)
		want := ""
		if path == "/invoke" {
			want = digest
		}
		if got := w.Header().Get("X-Imprun-Worker-Package-Digest"); w.Code != http.StatusOK || got != want || w.Body.String() != reply {
			t.Fatalf("%s의 응답이 예상과 다릅니다: %d %q %s", path, w.Code, got, w.Body)
		}
	}
}
