package main

import (
	"net/http"
	"testing"
	"time"
)

func TestParseClientVersion(t *testing.T) {
	tests := []struct {
		name  string
		input any
		want  string
		ok    bool
	}{
		{name: "date shaped", input: "2026.9.4", want: "2026.9.4", ok: true},
		{name: "trimmed", input: " 2026.9.4 ", want: "2026.9.4", ok: true},
		{name: "major only", input: "2026", want: "2026", ok: true},
		{name: "prerelease suffix", input: "2026.9.4-beta.1", want: "2026.9.4-beta.1", ok: true},
		{name: "not a string", input: 2026.9, ok: false},
		{name: "null", input: nil, ok: false},
		{name: "empty", input: "", ok: false},
		{name: "html error page", input: "<html>500</html>", ok: false},
		{name: "trailing dot", input: "2026.9.", ok: false},
		{name: "leading v", input: "v2026.9.4", ok: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := parseClientVersion(test.input)
			if ok != test.ok || got != test.want {
				t.Fatalf("parseClientVersion(%v) = %q,%v want %q,%v", test.input, got, ok, test.want, test.ok)
			}
		})
	}
}

func TestParseClientVersionFromUpdate(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
		ok   bool
	}{
		{
			name: "real shape",
			body: `{"data":{"value":{"version":"2026.9.4","date":"2026-09-04"}},"code":0,"msg":"OK"}`,
			want: "2026.9.4",
			ok:   true,
		},
		{
			name: "business envelope shape is not accepted",
			body: `{"code":0,"msg":"OK","data":{"version":"2026.9.4"}}`,
			ok:   false,
		},
		{name: "missing value", body: `{"data":{},"code":0}`, ok: false},
		{name: "bad version", body: `{"data":{"value":{"version":"nope"}},"code":0}`, ok: false},
		{name: "not json", body: `<html></html>`, ok: false},
		{name: "empty", body: ``, ok: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := parseClientVersionFromUpdate([]byte(test.body))
			if ok != test.ok || got != test.want {
				t.Fatalf("parseClientVersionFromUpdate(%s) = %q,%v want %q,%v", test.body, got, ok, test.want, test.ok)
			}
		})
	}
}

func TestClientVersionResolver(t *testing.T) {
	t.Run("remote then cache", func(t *testing.T) {
		fake := newFakeHost().on(httpRoute{
			Method: http.MethodGet,
			Match:  "api-overmind",
			Body:   `{"data":{"value":{"version":"2026.10.1"}},"code":0}`,
		})
		host := installFakeHost(t, fake)
		now := time.Now()
		version, source := versionResolver.resolve(host, DefaultConfig(), now)
		if version != "2026.10.1" || source != "remote" {
			t.Fatalf("first resolve = %q,%q want 2026.10.1,remote", version, source)
		}
		cached, source := versionResolver.resolve(host, DefaultConfig(), now.Add(time.Minute))
		if cached != "2026.10.1" || source != "cache" {
			t.Fatalf("second resolve = %q,%q want cached", cached, source)
		}
		if len(fake.requestsFor("api-overmind")) != 1 {
			t.Fatalf("cache miss: %d remote calls", len(fake.requestsFor("api-overmind")))
		}
	})

	t.Run("config override wins and is not cached", func(t *testing.T) {
		fake := newFakeHost()
		host := installFakeHost(t, fake)
		cfg := DefaultConfig()
		cfg.ClientVersionOverride = "2020.1.1"
		version, source := versionResolver.resolve(host, cfg, time.Now())
		if version != "2020.1.1" || source != "config" {
			t.Fatalf("override = %q,%q", version, source)
		}
		if len(fake.requestsFor("api-overmind")) != 0 {
			t.Fatal("config override must not hit the network")
		}
	})

	t.Run("failure falls back and is not cached", func(t *testing.T) {
		fake := newFakeHost().on(httpRoute{Method: http.MethodGet, Match: "api-overmind", Status: 500, Body: "boom"})
		host := installFakeHost(t, fake)
		version, source := versionResolver.resolve(host, DefaultConfig(), time.Now())
		if version != FallbackClientVersion || source != "fallback" {
			t.Fatalf("fallback = %q,%q", version, source)
		}
		if versionResolver.cached != "" {
			t.Fatal("a fallback value must not be cached")
		}
	})

	t.Run("nil host falls back", func(t *testing.T) {
		version, source := versionResolver.resolve(nil, DefaultConfig(), time.Now())
		if version != FallbackClientVersion || source != "fallback" {
			t.Fatalf("nil host = %q,%q", version, source)
		}
	})

	t.Run("transport error falls back", func(t *testing.T) {
		fake := newFakeHost().on(httpRoute{Method: http.MethodGet, Match: "api-overmind", Err: errFakeTransport})
		host := installFakeHost(t, fake)
		version, source := versionResolver.resolve(host, DefaultConfig(), time.Now())
		if version != FallbackClientVersion || source != "fallback" {
			t.Fatalf("transport error = %q,%q", version, source)
		}
	})
}
