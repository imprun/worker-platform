package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	address := flag.String("listen", ":80", "listen address")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	server := &http.Server{
		Addr: *address, Handler: routes(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second,
	}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	case <-ctx.Done():
		shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := server.Shutdown(shutdown); err != nil {
			log.Print(err)
		}
	}
}

func routes() http.Handler {
	mux := http.NewServeMux()
	probe := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	mux.HandleFunc("GET /healthz", probe)
	mux.HandleFunc("GET /readyz", probe)
	mux.HandleFunc("POST /invoke", echo)
	mux.HandleFunc("POST /attempt", echoAttempt)
	return mux
}

type echoPayload struct {
	Message *string `json:"message"`
}

func echo(w http.ResponseWriter, r *http.Request) {
	var payload echoPayload
	if !decodeEcho(w, r, 8*1024, &payload) || payload.Message == nil {
		http.Error(w, "expected one JSON object with a string message (8 KiB max)", http.StatusBadRequest)
		return
	}
	writeEcho(w, payload)
}

func echoAttempt(w http.ResponseWriter, r *http.Request) {
	var job struct {
		RunID   string           `json:"runId"`
		Attempt int              `json:"attempt"`
		Input   echoPayload      `json:"input"`
		Session *mockSiteSession `json:"siteSession,omitempty"`
	}
	if !decodeEcho(w, r, (64+16)*1024+256, &job) || job.RunID == "" || job.Attempt < 1 || job.Input.Message == nil {
		http.Error(w, "expected a Job attempt with a string message", http.StatusBadRequest)
		return
	}
	if job.Session == nil {
		writeEcho(w, job.Input)
		return
	}
	echoWithMockSession(w, job.Input, *job.Session)
}

type mockSiteSession struct {
	Ref     string `json:"ref"`
	Version int64  `json:"version"`
	State   struct {
		MockCookie string `json:"mockCookie"`
	} `json:"state"`
}

func echoWithMockSession(w http.ResponseWriter, payload echoPayload, session mockSiteSession) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if session.State.MockCookie != "approved-for-echo" {
		w.WriteHeader(http.StatusPreconditionRequired)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": "site_session_expired"})
		return
	}
	_ = json.NewEncoder(w).Encode(struct {
		echoPayload
		SiteSessionVersion int64 `json:"siteSessionVersion"`
	}{payload, session.Version})
}

func decodeEcho(w http.ResponseWriter, r *http.Request, limit int64, value any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	decoder.DisallowUnknownFields()
	return decoder.Decode(value) == nil && decoder.Decode(new(any)) == io.EOF
}

func writeEcho(w http.ResponseWriter, payload echoPayload) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(payload)
}
