package abiboot

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func storageOf(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("storage is not a JSON object: %v (%s)", err, raw)
	}
	return out
}

// The host stores whatever a plugin reports, so a parse response that carries
// only the plugin's own struct would strip the members the host owns; the file's
// priority has to ride along on the way out.
func TestCarryFileMembersKeepsPriorityOnParse(t *testing.T) {
	host := &Host{Incoming: json.RawMessage(`{"priority":6,"weight":2,"access_token":"old"}`)}
	result := carryFileMembers(host, pluginapi.AuthParseResponse{
		Handled: true,
		Auth:    pluginapi.AuthData{StorageJSON: json.RawMessage(`{"access_token":"new"}`)},
		Auths: []pluginapi.AuthData{
			{StorageJSON: json.RawMessage(`{"access_token":"second"}`)},
		},
	})

	response, ok := result.(pluginapi.AuthParseResponse)
	if !ok {
		t.Fatalf("result type = %T, want AuthParseResponse", result)
	}
	members := storageOf(t, response.Auth.StorageJSON)
	if members["priority"] != float64(6) || members["weight"] != float64(2) {
		t.Errorf("auth storage = %s, want priority and weight preserved", response.Auth.StorageJSON)
	}
	if members["access_token"] != "new" {
		t.Errorf("access_token = %v, want the plugin value", members["access_token"])
	}
	expanded := storageOf(t, response.Auths[0].StorageJSON)
	if expanded["priority"] != float64(6) {
		t.Errorf("expanded auth storage = %s, want priority preserved", response.Auths[0].StorageJSON)
	}
}

func TestCarryFileMembersCoversRefreshAndPointerForms(t *testing.T) {
	host := &Host{Incoming: json.RawMessage(`{"priority":4}`)}

	refreshed, ok := carryFileMembers(host, pluginapi.AuthRefreshResponse{
		Auth: pluginapi.AuthData{StorageJSON: json.RawMessage(`{"refresh_token":"new"}`)},
	}).(pluginapi.AuthRefreshResponse)
	if !ok {
		t.Fatal("refresh response type changed")
	}
	if members := storageOf(t, refreshed.Auth.StorageJSON); members["priority"] != float64(4) {
		t.Errorf("refresh storage = %s, want priority preserved", refreshed.Auth.StorageJSON)
	}

	pointer := &pluginapi.AuthParseResponse{
		Auth: pluginapi.AuthData{StorageJSON: json.RawMessage(`{"access_token":"new"}`)},
	}
	carryFileMembers(host, pointer)
	if members := storageOf(t, pointer.Auth.StorageJSON); members["priority"] != float64(4) {
		t.Errorf("pointer storage = %s, want priority preserved", pointer.Auth.StorageJSON)
	}
}

func TestCarryFileMembersLeavesOtherResultsAlone(t *testing.T) {
	host := &Host{Incoming: json.RawMessage(`{"priority":6}`)}

	probe := map[string]any{"provider": "cline"}
	if got := carryFileMembers(host, probe); got == nil {
		t.Fatal("non-auth result was dropped")
	}

	empty := carryFileMembers(&Host{}, pluginapi.AuthParseResponse{
		Auth: pluginapi.AuthData{StorageJSON: json.RawMessage(`{"access_token":"new"}`)},
	}).(pluginapi.AuthParseResponse)
	if string(empty.Auth.StorageJSON) != `{"access_token":"new"}` {
		t.Errorf("storage = %s, want it untouched without an incoming file", empty.Auth.StorageJSON)
	}

	blank := carryFileMembers(host, pluginapi.AuthParseResponse{
		Auth: pluginapi.AuthData{},
	}).(pluginapi.AuthParseResponse)
	if len(blank.Auth.StorageJSON) != 0 {
		t.Errorf("storage = %s, want it left empty", blank.Auth.StorageJSON)
	}
}
