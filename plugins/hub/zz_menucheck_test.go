package main

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestMenuCount guards the one rule this repository has been burned by: the
// manager renders ONE sidebar entry per menu route and does not group them by
// plugin, so a second non-empty Menu looks like a duplicated sidebar entry.
//
// This plugin owns the repository's single entry, which is why every provider
// plugin's TestMenuCount asserts ZERO: the panel is one "Jet Hub" row, and the
// provider pages it links to are Menu-less resource routes.
func TestMenuCount(t *testing.T) {
	value, err := handleManagementRegister(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp := value.(pluginapi.ManagementRegistrationResponse)
	menus := 0
	for _, r := range resp.Routes {
		if r.Menu != "" {
			menus++
		}
	}
	if menus != 1 {
		t.Fatalf("menu routes = %d, want exactly 1 (the repository's only sidebar entry)", menus)
	}
	if resp.Routes[0].Method != http.MethodGet || resp.Routes[0].Path != "/status" || resp.Routes[0].Menu != MenuLabel {
		t.Fatalf("menu route = %s %s %q, want GET /status with menu %q", resp.Routes[0].Method, resp.Routes[0].Path, resp.Routes[0].Menu, MenuLabel)
	}
	if MenuLabel != "Jet Hub" {
		t.Fatalf("MenuLabel = %q, want the sidebar entry to read \"Jet Hub\"", MenuLabel)
	}
	// Every other declared page must be a Menu-less resource route.
	for _, resource := range resp.Resources {
		if resource.Menu != "" {
			t.Fatalf("resource route %s carries menu %q, want empty", resource.Path, resource.Menu)
		}
	}
}
