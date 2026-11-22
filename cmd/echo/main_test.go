package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEchoContract(t *testing.T) {
	h := routes()
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{"GET", "/readyz", "", 200},
		{"POST", "/invoke", `{"message":"hello"}`, 200},
		{"GET", "/invoke", "", 405},
		{"POST", "/invoke", `{"message":null}`, 400},
		{"POST", "/invoke", `{"message":"hello"}{}`, 400},
		{"POST", "/attempt", `{"runId":"test-run","attempt":1,"input":{"message":"hello"}}`, 200},
		{"POST", "/attempt", `{"runId":"test-run","attempt":0,"input":{"message":"hello"}}`, 400},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
		if w.Code != tc.want {
			t.Fatalf("%s %s %q returned %d, want %d", tc.method, tc.path, tc.body, w.Code, tc.want)
		}
		if tc.path == "/invoke" && tc.want == http.StatusOK && strings.TrimSpace(w.Body.String()) != tc.body {
			t.Fatalf("unexpected echo %s", w.Body.String())
		}
	}
}
