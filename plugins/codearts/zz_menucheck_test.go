package main

import (
	"slices"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestMenuCount guards the repository-wide rule documented in the README: the
// manager's sidebar is flat and does not group entries by plugin, and the ONE
// entry this repository spends is the hub's "Jet Hub" page. A Menu here would add
// a second nav item for this provider, so this plugin must declare NONE — its
// status page stays reachable as a Menu-less resource route, which is what the
// hub links to.

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
	if menus != 0 {
		t.Fatalf("menu routes = %d, want zero: the hub plugin owns the repository's only sidebar entry", menus)
	}
	// Every page is a Menu-less resource route, and the set is pinned: the
	// 新建账号 affordance is a LINK to /login, so adding a second account must
	// not have added a route. The repository-wide sidebar count therefore stays
	// at the hub's single entry — this plugin contributes none of it.
	pages := make([]string, 0, len(resp.Resources))
	for _, resource := range resp.Resources {
		if resource.Menu != "" {
			t.Fatalf("resource route %s carries menu %q, want empty", resource.Path, resource.Menu)
		}
		pages = append(pages, resource.Path)
	}
	if want := []string{"/status", "/login"}; !slices.Equal(pages, want) {
		t.Fatalf("resource routes = %v, want %v: /status is what the hub links to, /login is what it and 新建账号 open", pages, want)
	}
}
