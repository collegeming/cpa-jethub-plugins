package main

import (
	"slices"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// TestMenuCount guards the ops-page merge: this plugin contributes exactly ONE
// sidebar Menu (/pool, "CodeArts 运维"). Status/login/reg stay Menu-less
// resources; /reg keeps a direct URL for the registrar actions.

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
	// The merged ops page spends exactly ONE sidebar entry (/pool). /reg is a
	// Menu-less resource showing the same combined surface. The hub still owns
	// its own overview entry separately.
	if menus != 1 {
		t.Fatalf("menu routes = %d, want 1 (/pool combined ops)", menus)
	}
	var menuPath, menuName string
	for _, r := range resp.Routes {
		if r.Menu != "" {
			menuPath, menuName = r.Path, r.Menu
		}
	}
	if menuPath != "/pool" {
		t.Fatalf("menu path = %q, want /pool", menuPath)
	}
	if menuName != "CodeArts 运维" {
		t.Fatalf("menu name = %q, want CodeArts 运维", menuName)
	}
	pages := make([]string, 0, len(resp.Resources))
	for _, resource := range resp.Resources {
		if resource.Menu != "" {
			t.Fatalf("resource route %s carries menu %q, want empty", resource.Path, resource.Menu)
		}
		pages = append(pages, resource.Path)
	}
	if want := []string{"/status", "/login", "/reg"}; !slices.Equal(pages, want) {
		t.Fatalf("resource routes = %v, want %v", pages, want)
	}
}
