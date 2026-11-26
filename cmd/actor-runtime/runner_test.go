package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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

func TestInvokeRecordsAndReturnsJSON(t *testing.T) {
	dir := t.TempDir()
	entrypoint := filepath.Join(dir, "json-echo.exe")
	build := exec.CommandContext(t.Context(), "go", "build", "-trimpath", "-o", entrypoint, "../../examples/echo/json-app")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build JSON Echo: %v\n%s", err, output)
	}
	runner := appRunner{apppackage.Installed{Dir: dir, Entrypoint: entrypoint}}
	if err := runner.command(t.Context(), "--check").Run(); err != nil {
		t.Fatalf("check JSON Echo journal: %v", err)
	}
	server := httptest.NewServer(runner.routes())
	defer server.Close()
	invoke := func(body string) (int, string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/invoke", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, string(data)
	}

	tests := []struct{ input, want string }{
		{input: `{"hello":"world"}`, want: `{"hello":"world"}`},
		{
			input: " {\n  \"number\": 9007199254740993, \"items\": [true, null, {\"name\": \"에코\"}]\n}\n",
			want:  `{"number":9007199254740993,"items":[true,null,{"name":"에코"}]}`,
		},
		{input: `"hello"`, want: `"hello"`},
		{input: `null`, want: `null`},
	}
	var journal strings.Builder
	for i, tt := range tests {
		status, body := invoke(tt.input)
		if status != http.StatusOK || body != tt.want {
			t.Fatalf("unexpected echo: HTTP %d, body %s", status, body)
		}
		if i == 0 {
			t.Logf("POST /invoke %s", tt.input)
			t.Logf("HTTP %d %s", status, body)
		}
		journal.WriteString(tt.want + "\n")
	}
	for _, invalid := range []string{`{"missing":}`, `"` + strings.Repeat("a", maxOutputBytes) + `"`} {
		if status, _ := invoke(invalid); status != http.StatusBadRequest {
			t.Fatalf("invalid or oversized input returned HTTP %d", status)
		}
	}
	path := filepath.Join(dir, "requests.jsonl")
	data, err := os.ReadFile(path)
	if err != nil || string(data) != journal.String() {
		t.Fatalf("unexpected request journal: %q, error %v", data, err)
	}

	firstRecord, _, _ := strings.Cut(string(data), "\n")
	t.Logf("requests.jsonl %s", firstRecord)

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if status, _ := invoke(`{"message":"cannot record"}`); status != http.StatusBadGateway {
		t.Fatalf("recording failure returned HTTP %d", status)
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
