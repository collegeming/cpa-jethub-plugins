package main

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestMenuCount is the repository-wide guard documented in the README: the
// manager's sidebar is flat, so a plugin with more than one Menu route occupies
// more than one row and looks duplicated (`registeredPluginMenus` skips empty
// Menu entries). The repository spends its single entry on the hub plugin, so
// this provider declares none and keeps its pages in Resources with an empty
// Menu.

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
	// The status page must stay mounted on the resource path the hub links to.
	statusMounted := false
	for _, resource := range resp.Resources {
		if resource.Menu != "" {
			t.Fatalf("resource route %s carries menu %q, want empty", resource.Path, resource.Menu)
		}
		if resource.Path == "/status" {
			statusMounted = true
		}
	}
	if !statusMounted {
		t.Fatal("the status page is not a Menu-less resource route, so /v0/resource/plugins/<id>/status would 404")
	}
}
