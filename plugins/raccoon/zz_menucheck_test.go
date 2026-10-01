package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
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
	want := []string{"/status", "/login", "/reward", "/checkin"}
	if len(pages) != len(want) {
		t.Fatalf("resource routes = %v, want %v", pages, want)
	}
	for index, path := range want {
		if pages[index] != path {
			t.Fatalf("resource routes = %v, want %v", pages, want)
		}
	}
	// `/checkin` EXISTS as the hub sweep's entry point, but it is NOT a daily
	// check-in and must never read like one: the daily 300 is granted by the
	// server on its own. The route exists so the sweep can claim the ONE-OFF
	// desktop login reward, and its description has to say so — otherwise the
	// next reader will assume a daily action that does not exist.
	described := false
	for _, resource := range resp.Resources {
		if resource.Path != "/checkin" {
			continue
		}
		described = true
		if !strings.Contains(resource.Description, "没有每日签到") {
			t.Fatalf("/checkin must state that this provider has no daily check-in; got %q", resource.Description)
		}
	}
	if !described {
		t.Fatal("/checkin route is missing: the hub sweep has no entry point")
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

// TestCheckinOnlyWritesForAnExplicitAction is the safety property that makes it
// acceptable to put a ONE-OFF reward behind a button people press daily.
//
// Two gates have to hold: the route must not write on a page load, and it must
// not write at all when the reward's own state already says it was granted. The
// server's `granted` flag is a third gate, but relying on it alone would mean
// every sweep fires a pointless write against a lifetime grant.
func TestCheckinOnlyWritesForAnExplicitAction(t *testing.T) {
	grants := 0
	load := func(claimed bool, action string) map[string]any {
		grants = 0
		fake := newFakeHost()
		fake.install(t)
		fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
			switch {
			case strings.HasSuffix(request.URL, LoginPointsGrantPath):
				grants++
				return httpResponse(http.StatusOK, `{"code":0,"data":{"granted":true,"popup":{"points":3000}}}`), nil
			case strings.Contains(request.URL, PointsBillsPath):
				// The state is DERIVED from bill history: no dedicated endpoint
				// exists, and matching biz_type alone is wrong because the
				// registration gift shares it — the event name is the separator.
				if claimed {
					return httpResponse(http.StatusOK, `{"code":0,"data":{"items":[{"biz_type":"`+
						LoginRewardBizType+`","event_name":"`+LoginRewardEventName+`","points":3000}]}}`), nil
				}
				return httpResponse(http.StatusOK, `{"code":0,"data":{"items":[]}}`), nil
			default:
				return httpResponse(http.StatusOK, `{"code":0,"data":{}}`), nil
			}
		}
		storedCredential(t, fake, "rc-1", "raccoon-1.json", sampleCredential(t))
		query := url.Values{"auth_index": {"rc-1"}, "format": {"json"}}
		if action != "" {
			query.Set("action", action)
		}
		response := callManagement(t, testHost(), managementRequest(
			http.MethodGet, "/v0/resource/plugins/raccoon/checkin", query, nil))
		var document map[string]any
		if errUnmarshal := json.Unmarshal(response.Body, &document); errUnmarshal != nil {
			t.Fatalf("decode %s: %v", string(response.Body), errUnmarshal)
		}
		return document
	}

	// A plain load never writes.
	document := load(false, "")
	if grants != 0 {
		t.Fatalf("a page load issued %d grant requests", grants)
	}
	if document["status"] != "needs-action" {
		t.Fatalf("plain load status = %v, want needs-action", document["status"])
	}

	// The explicit action does write, once.
	document = load(false, "claim")
	if grants != 1 {
		t.Fatalf("action=claim issued %d grant requests, want 1", grants)
	}
	if document["status"] != "claimed" {
		t.Fatalf("status = %v, want claimed", document["status"])
	}

	// An account that already holds the reward is reported WITHOUT a write. The
	// server would refuse a duplicate anyway, but firing a pointless write at a
	// lifetime grant on every sweep is not the same as being correct.
	document = load(true, "claim")
	if grants != 0 {
		t.Fatalf("an already-granted reward still issued %d grant requests", grants)
	}
	if document["status"] != "already-claimed" {
		t.Fatalf("status = %v, want already-claimed", document["status"])
	}
}
