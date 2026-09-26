package main

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestMenuCount is the repository-wide guard documented in the README: the
// manager's sidebar is flat, so a plugin with more than one Menu route occupies
// more than one row and looks duplicated (`registeredPluginMenus` skips empty
// Menu entries). Sub-pages therefore live in Resources with an empty Menu.
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
		t.Fatalf("menu routes = %d, want exactly 1", menus)
	}
	for _, resource := range resp.Resources {
		if resource.Menu != "" {
			t.Fatalf("resource route %s must not carry a Menu, got %q", resource.Path, resource.Menu)
		}
	}
}
