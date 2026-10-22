package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mightbeanshuu/limitplane/internal/gateway"
)

func TestForwardedForIsIgnoredUnlessTrusted(t *testing.T) {
	var seen string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = gateway.ClientIP(r) })

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.50:41000"
	r.Header.Set("X-Forwarded-For", "198.51.100.7") // somebody else's address

	forwardedFor(false, h).ServeHTTP(httptest.NewRecorder(), r)
	if seen != "203.0.113.50" {
		t.Fatalf("untrusted: identity = %q, want the socket address", seen)
	}

	r.Header.Set("X-Forwarded-For", "198.51.100.7")
	forwardedFor(true, h).ServeHTTP(httptest.NewRecorder(), r)
	if seen != "198.51.100.7" {
		t.Fatalf("trusted: identity = %q, want the forwarded address", seen)
	}
}
