package main

import (
	"errors"
	"net/http"
	"testing"
)

// statusOf reads the HTTP status a classified failure carries and never invents
// one for an unclassified error.
func TestStatusOf(t *testing.T) {
	classified := credentialError("auth", "dead")
	if got := statusOf(classified, http.StatusBadGateway); got != http.StatusUnauthorized {
		t.Fatalf("statusOf(credential error) = %d, want 401", got)
	}
	if got := statusOf(transportError("boom", "x"), http.StatusOK); got != http.StatusBadGateway {
		t.Fatalf("statusOf(transport error) = %d, want 502", got)
	}
	if got := statusOf(errors.New("plain"), http.StatusTeapot); got != http.StatusTeapot {
		t.Fatalf("statusOf(plain error) = %d, want the fallback 418", got)
	}
	// A classified failure without a status also falls back, so a caller can
	// still choose a sensible code.
	bare := statusError(true, "unclassified", 0, "no status")
	if got := statusOf(bare, http.StatusNotFound); got != http.StatusNotFound {
		t.Fatalf("statusOf(status-less error) = %d, want the fallback 404", got)
	}
	// Wrapping must not defeat the classification.
	wrapped := errors.Join(errors.New("context"), credentialError("auth", "dead"))
	if got := statusOf(wrapped, http.StatusBadGateway); got != http.StatusUnauthorized {
		t.Fatalf("statusOf(wrapped) = %d, want 401", got)
	}
}

// The retry hint travels with the failure so hosts that still read it agree with
// the status.
func TestStatusErrorRetryHint(t *testing.T) {
	if transportError("boom", "x").Retryable != true {
		t.Fatal("a transport failure must be retryable")
	}
	if credentialError("auth", "x").Retryable != false {
		t.Fatal("a dead credential must not be retried")
	}
	if retryable := statusError(false, "quota", http.StatusPaymentRequired, "x").Retryable; retryable {
		t.Fatal("an exhausted quota must not be retried automatically")
	}
}

func TestBoolString(t *testing.T) {
	if boolString(true) != "true" || boolString(false) != "false" {
		t.Fatal("boolString must render the host attribute spelling")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("  hello  ", 10); got != "hello" {
		t.Fatalf("truncate = %q, want the trimmed text", got)
	}
	long := truncate("0123456789", 4)
	if long != "0123…" {
		t.Fatalf("truncate = %q, want the ellipsis suffix", long)
	}
	if got := truncate("", 4); got != "" {
		t.Fatalf("truncate(empty) = %q, want empty", got)
	}
}
