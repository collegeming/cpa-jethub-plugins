package abiboot

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// saveAuthFixture records what SaveAuth actually sent to the host.
type saveAuthFixture struct {
	saved json.RawMessage
	// file is the auth file the host is holding, keyed by auth index.
	file json.RawMessage
	// listCalls counts host.auth.list invocations.
	listCalls int
	// getCalls counts host.auth.get invocations.
	getCalls int
}

func (f *saveAuthFixture) install(t *testing.T) {
	t.Helper()
	SetHostCaller(func(method string, request []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			f.listCalls++
			return OK(map[string]any{"files": []map[string]any{{
				"name": "cline-a.json", "id": "cline-a.json", "auth_index": "idx-1",
				"provider": "cline", "type": "cline", "status": "active",
			}}})
		case pluginabi.MethodHostAuthGet:
			f.getCalls++
			return OK(map[string]any{"auth_index": "idx-1", "name": "cline-a.json", "json": f.file})
		case pluginabi.MethodHostAuthSave:
			var payload struct {
				Name string          `json:"name"`
				JSON json.RawMessage `json:"json"`
			}
			if errDecode := json.Unmarshal(request, &payload); errDecode != nil {
				return nil, errDecode
			}
			f.saved = payload.JSON
			return OK(map[string]any{"name": payload.Name, "path": "/auths/" + payload.Name})
		}
		return nil, Errorf("unexpected_method", "unexpected host method %s", method)
	})
	t.Cleanup(func() {
		ClearHostCaller()
	})
}

// A callback that carried the auth file must carry its host-owned members
// forward: `priority` drives credential selection, and dropping it silently
// resets the credential's routing tier.
func TestSaveAuthCarriesIncomingHostMembersForward(t *testing.T) {
	fixture := &saveAuthFixture{}
	fixture.install(t)

	h := &Host{Incoming: json.RawMessage(`{"priority":6,"access_token":"old"}`)}
	if _, errSave := h.SaveAuth("cline-a.json", json.RawMessage(`{"access_token":"new"}`)); errSave != nil {
		t.Fatalf("SaveAuth: %v", errSave)
	}
	var saved map[string]any
	if errUnmarshal := json.Unmarshal(fixture.saved, &saved); errUnmarshal != nil {
		t.Fatalf("decode saved: %v", errUnmarshal)
	}
	if saved["priority"] != float64(6) {
		t.Errorf("priority = %v, want 6 (saved=%s)", saved["priority"], fixture.saved)
	}
}

// A login poll's payload carries no auth file, so Incoming is empty. Without a
// fallback the host-owned members are erased from the file the host is holding,
// which is the "re-login silently resets the routing tier" failure.
func TestSaveAuthRecoversHostMembersWhenIncomingIsEmpty(t *testing.T) {
	fixture := &saveAuthFixture{file: json.RawMessage(`{"priority":6,"weight":3,"access_token":"old"}`)}
	fixture.install(t)

	h := &Host{}
	if _, errSave := h.SaveAuth("cline-a.json", json.RawMessage(`{"access_token":"new"}`)); errSave != nil {
		t.Fatalf("SaveAuth: %v", errSave)
	}
	var saved map[string]any
	if errUnmarshal := json.Unmarshal(fixture.saved, &saved); errUnmarshal != nil {
		t.Fatalf("decode saved: %v", errUnmarshal)
	}
	if saved["priority"] != float64(6) {
		t.Errorf("priority = %v, want 6 — host-owned member was erased (saved=%s)", saved["priority"], fixture.saved)
	}
	if saved["weight"] != float64(3) {
		t.Errorf("weight = %v, want 3 — host-owned member was erased (saved=%s)", saved["weight"], fixture.saved)
	}
	if saved["access_token"] != "new" {
		t.Errorf("access_token = %v, want new — the plugin's own write must still win", saved["access_token"])
	}
}

// The recovery is best effort: an unknown file (a first login) simply has
// nothing to preserve, and a lookup failure must not block the save.
func TestSaveAuthProceedsWhenNoStoredFileExists(t *testing.T) {
	fixture := &saveAuthFixture{}
	fixture.install(t)

	h := &Host{}
	if _, errSave := h.SaveAuth("brand-new.json", json.RawMessage(`{"access_token":"new"}`)); errSave != nil {
		t.Fatalf("SaveAuth: %v", errSave)
	}
	var saved map[string]any
	if errUnmarshal := json.Unmarshal(fixture.saved, &saved); errUnmarshal != nil {
		t.Fatalf("decode saved: %v", errUnmarshal)
	}
	if saved["access_token"] != "new" {
		t.Errorf("saved = %s, want the plugin's own members", fixture.saved)
	}
}

// An entry with no backing file must not be mistaken for the target: the empty
// path's filepath.Base is ".", and matching on it would let a runtime-only
// credential's members leak into a file-backed save.
func TestStoredAuthFileIgnoresEntriesWithoutAPath(t *testing.T) {
	entry := pluginapi.HostAuthFileEntry{Name: "runtime-only", ID: "runtime-only"}
	if authEntryMatchesName(entry, ".") {
		t.Error("an entry without a path matched \".\"")
	}
	if !authEntryMatchesName(entry, "runtime-only") {
		t.Error("the entry's own name must still match")
	}
	withPath := pluginapi.HostAuthFileEntry{Name: "other", ID: "other", Path: "/auths/acct.json"}
	if !authEntryMatchesName(withPath, "acct.json") {
		t.Error("the file name of a backing path must match")
	}
}
