package main

import (
	"slices"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// One sidebar entry: CPAMP groups menu routes by plugin. Status keeps the
// single Menu so Loomy renders as one nav row; the account pool page is a
// Menu-less resource linked from status (same pattern as login/checkin).

func TestMenuCount(t *testing.T) {
	value, err := handleManagementRegister(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp := value.(pluginapi.ManagementRegistrationResponse)
	menus := 0
	menuPath := ""
	for _, r := range resp.Routes {
		if r.Menu != "" {
			menus++
			menuPath = r.Path
			if r.Path != "/status" {
				t.Fatalf("menu route %s is not /status", r.Path)
			}
		}
	}
	if menus != 1 {
		t.Fatalf("menu routes = %d, want 1 (/status only; /pool is a resource)", menus)
	}
	if menuPath != "/status" {
		t.Fatalf("menu path = %q, want /status", menuPath)
	}
	pages := make([]string, 0, len(resp.Resources))
	for _, resource := range resp.Resources {
		if resource.Menu != "" {
			t.Fatalf("resource route %s carries menu %q, want empty", resource.Path, resource.Menu)
		}
		pages = append(pages, resource.Path)
	}
	if want := []string{"/login", "/checkin", "/onboarding", "/pool"}; !slices.Equal(pages, want) {
		t.Fatalf("resource routes = %v, want %v", pages, want)
	}
}
