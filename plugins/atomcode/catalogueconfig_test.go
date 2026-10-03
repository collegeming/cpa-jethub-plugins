package main

import "testing"

// TestModelRefreshFieldIsDeclaredAndParsed pins the new setting end to end: a
// described field the parser ignores is a control that does nothing.
func TestModelRefreshFieldIsDeclaredAndParsed(t *testing.T) {
	declared := false
	for _, field := range ConfigFields() {
		if field.Name == "model_refresh_ms" {
			declared = true
			if field.Type != "integer" {
				t.Errorf("type = %q, want integer", field.Type)
			}
			if len([]rune(field.Description)) < 8 {
				t.Errorf("description too short: %q", field.Description)
			}
		}
	}
	if !declared {
		t.Fatal("model_refresh_ms is not described to the host")
	}
	if got := ConfigFromYAML([]byte("model_refresh_ms: 900000")).ModelRefreshMS; got != 900000 {
		t.Fatalf("model_refresh_ms = %d, want 900000", got)
	}
	if got := DefaultConfig().ModelRefreshMS; got != 0 {
		t.Fatalf("default model_refresh_ms = %d, want 0 (disabled)", got)
	}
}
