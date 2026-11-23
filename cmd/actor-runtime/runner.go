package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"time"
	"unicode/utf8"

	"github.com/imprun/worker-platform/internal/apppackage"
)

const (
	maxInputBytes  = 512 << 10
	maxOutputBytes = 64 << 10
)

type appRunner struct{ app apppackage.Installed }

func (a appRunner) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, a.app.Entrypoint, args...)
	cmd.Dir = a.app.Dir
	cmd.Env = []string{"HOME=" + a.app.Dir, "TMPDIR=" + os.TempDir(), "PATH=/usr/local/bin:/usr/bin:/bin"}
	cmd.WaitDelay = time.Second
	return cmd
}

func (a appRunner) routes() http.Handler {
	mux := http.NewServeMux()
	ready := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	mux.HandleFunc("GET /healthz", ready)
	mux.HandleFunc("GET /readyz", ready)
	mux.HandleFunc("POST /invoke", a.serve(false))
	mux.HandleFunc("POST /attempt", a.serve(true))
	return mux
}

func (a appRunner) serve(attempt bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxInputBytes))
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			return
		}
		if err != nil || !utf8.Valid(data) || !json.Valid(data) {
			http.Error(w, "expected a JSON request", http.StatusBadRequest)
			return
		}
		var request apppackage.Invocation
		if attempt && (json.Unmarshal(data, &request) != nil || request.RunID == "" ||
			request.Attempt < 1 || request.Input == nil) {
			http.Error(w, "invalid Job attempt", http.StatusBadRequest)
			return
		}
		if !attempt {
			request.Input = data
		}
		a.invoke(w, r, request)
	}
}

func (a appRunner) invoke(w http.ResponseWriter, r *http.Request, request apppackage.Invocation) {
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	data, err := json.Marshal(request)
	if err != nil {
		http.Error(w, "invalid app input", http.StatusBadRequest)
		return
	}
	cmd := a.command(ctx)
	cmd.Stdin = bytes.NewReader(data)
	var output boundedOutput
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			http.Error(w, "app timed out", http.StatusGatewayTimeout)
		case errors.As(err, &exit) && exit.ExitCode() == apppackage.SessionExpiredExitCode && len(request.SiteSession) > 0:
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusPreconditionRequired)
			_, _ = io.WriteString(w, `{"code":"site_session_expired"}`)
		case errors.As(err, &exit) && exit.ExitCode() == apppackage.InvalidInputExitCode:
			http.Error(w, "invalid app input", http.StatusBadRequest)
		default:
			http.Error(w, "app execution failed", http.StatusBadGateway)
		}
		return
	}
	if !utf8.Valid(output.buffer.Bytes()) || !json.Valid(output.buffer.Bytes()) {
		http.Error(w, "app returned invalid JSON", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(output.buffer.Bytes())
}

type boundedOutput struct{ buffer bytes.Buffer }

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > maxOutputBytes-b.buffer.Len() {
		return 0, errors.New("app output exceeds 64 KiB")
	}
	return b.buffer.Write(p)
}
