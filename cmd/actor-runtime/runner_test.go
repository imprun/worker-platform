package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAttemptRequiresInput(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/attempt", strings.NewReader(`{"runId":"test","attempt":1}`))
	appRunner{}.routes().ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing input returned HTTP %d", w.Code)
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
