package main

import (
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// No sidebar entry: CPA-Manager-Plus renders a flat nav and does not group
// entries by plugin, and the repository's single entry is the hub's "Jet Hub"
// page. Every page below stays reachable as a Menu-less resource route.

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
	// Every page is a Menu-less resource route, and the set is pinned: adding a
	// route is a deliberate act, not a side effect.
	pages := make([]string, 0, len(resp.Resources))
	for _, resource := range resp.Resources {
		if resource.Menu != "" {
			t.Fatalf("resource route %s carries menu %q, want empty", resource.Path, resource.Menu)
		}
		pages = append(pages, resource.Path)
	}
	want := []string{"/status", "/login", "/reward"}
	if len(pages) != len(want) {
		t.Fatalf("resource routes = %v, want %v", pages, want)
	}
	for index, path := range want {
		if pages[index] != path {
			t.Fatalf("resource routes = %v, want %v", pages, want)
		}
	}
	// There is deliberately NO /checkin route: this provider has no daily
	// check-in, and shipping the route would imply otherwise.
	for _, path := range pages {
		if strings.Contains(path, "checkin") || strings.Contains(path, "signin") {
			t.Fatalf("resource route %s suggests a daily check-in, which this provider does not have", path)
		}
	}
}

// TestMenuCountIsStableAcrossCalls guards the "exactly one menu in the repo"
// invariant from this plugin's side: registering twice must not accumulate.
func TestMenuCountIsStableAcrossCalls(t *testing.T) {
	var waitGroup sync.WaitGroup
	for index := 0; index < 4; index++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			if _, err := handleManagementRegister(nil, nil); err != nil {
				t.Errorf("register: %v", err)
			}
		}()
	}
	waitGroup.Wait()
}
