package main

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// One sidebar entry, no more: CPA-Manager-Plus renders a flat nav and does not
// group entries by plugin, so a second Menu route looks like a duplicate.
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
			t.Fatalf("resource %s carries Menu %q; sub-pages must have an empty Menu", resource.Path, resource.Menu)
		}
	}
}
