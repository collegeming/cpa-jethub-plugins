package catalog

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// fakeHost installs a host transport that holds one auth file and records
// every save, so a test can observe exactly what was published.
type fakeHost struct {
	file   json.RawMessage
	saved  map[string]json.RawMessage
	gets   int
	saves  int
	listOK bool
	// listEmpty models the install → configure → login order: host.auth.list
	// answers with no credentials until an account exists.
	listEmpty bool
	mu        sync.Mutex
}

// saveCount reads the save counter under the fixture lock.
func (f *fakeHost) saveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.saves
}

func (f *fakeHost) install(t *testing.T) {
	t.Helper()
	f.saved = map[string]json.RawMessage{}
	if f.file == nil {
		f.file = json.RawMessage(`{"priority":6,"weight":2,"access_token":"A"}`)
	}
	abiboot.SetHostCaller(func(method string, request []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			f.mu.Lock()
			empty := f.listEmpty
			f.mu.Unlock()
			if !f.listOK && empty {
				return nil, errors.New("list unavailable")
			}
			if empty {
				return abiboot.OK(map[string]any{"files": []map[string]any{}})
			}
			return abiboot.OK(map[string]any{"files": []map[string]any{{
				"name": "acct.json", "id": "acct.json", "auth_index": "idx-9",
				"provider": "demo", "type": "demo", "status": "active",
			}}})
		case pluginabi.MethodHostAuthGet:
			f.gets++
			return abiboot.OK(map[string]any{"auth_index": "idx-9", "name": "acct.json", "json": f.file})
		case pluginabi.MethodHostAuthSave:
			var payload struct {
				Name string          `json:"name"`
				JSON json.RawMessage `json:"json"`
			}
			if errDecode := json.Unmarshal(request, &payload); errDecode != nil {
				return nil, errDecode
			}
			f.mu.Lock()
			f.saves++
			f.saved[payload.Name] = payload.JSON
			f.mu.Unlock()
			return abiboot.OK(map[string]any{"name": payload.Name, "path": "/auths/" + payload.Name})
		}
		return nil, abiboot.Errorf("unexpected", "unexpected method %s", method)
	})
	t.Cleanup(abiboot.ClearHostCaller)
}

// The publish must leave a timestamp, because the host deduplicates auth-file
// events by content hash: a rewrite that did not change the member would be
// discarded and the catalogue change would stay invisible.
func TestPublishWritesAChangingMember(t *testing.T) {
	fixture := &fakeHost{listOK: true}
	fixture.install(t)
	h := &abiboot.Host{}

	if errPublish := Publish(h, "acct.json"); errPublish != nil {
		t.Fatalf("Publish: %v", errPublish)
	}
	first := string(fixture.saved["acct.json"])
	if first == "" {
		t.Fatal("nothing was saved")
	}
	var members map[string]any
	if errUnmarshal := json.Unmarshal(fixture.saved["acct.json"], &members); errUnmarshal != nil {
		t.Fatalf("decode saved: %v", errUnmarshal)
	}
	if _, ok := members[PublishKey]; !ok {
		t.Errorf("%s missing from the published file: %s", PublishKey, first)
	}

	// A second publish must differ, or the host would ignore it.
	time.Sleep(2 * time.Millisecond)
	if errPublish := Publish(h, "acct.json"); errPublish != nil {
		t.Fatalf("second Publish: %v", errPublish)
	}
	if string(fixture.saved["acct.json"]) == first {
		t.Error("the second publish produced identical bytes; the host would deduplicate it")
	}
}

// The publish must not erase the host-owned routing members that share the file.
func TestPublishKeepsHostOwnedMembers(t *testing.T) {
	fixture := &fakeHost{listOK: true}
	fixture.install(t)

	if errPublish := Publish(&abiboot.Host{}, "acct.json"); errPublish != nil {
		t.Fatalf("Publish: %v", errPublish)
	}
	var members map[string]any
	if errUnmarshal := json.Unmarshal(fixture.saved["acct.json"], &members); errUnmarshal != nil {
		t.Fatalf("decode saved: %v", errUnmarshal)
	}
	if members["priority"] != float64(6) {
		t.Errorf("priority = %v, want 6 (saved=%s)", members["priority"], fixture.saved["acct.json"])
	}
	if members["weight"] != float64(2) {
		t.Errorf("weight = %v, want 2 (saved=%s)", members["weight"], fixture.saved["acct.json"])
	}
	if members["access_token"] != "A" {
		t.Errorf("access_token = %v, want A", members["access_token"])
	}
}

// A publish that cannot resolve the auth index must still work: the host accepts
// the file name too, and a lookup failure must not block the refresh.
func TestPublishSurvivesAnUnresolvableIndex(t *testing.T) {
	fixture := &fakeHost{listOK: false}
	fixture.install(t)

	if errPublish := Publish(&abiboot.Host{}, "acct.json"); errPublish != nil {
		t.Fatalf("Publish: %v", errPublish)
	}
	if fixture.saves != 1 {
		t.Errorf("saves = %d, want 1", fixture.saves)
	}
}

// Run refreshes first and publishes second. Publishing first would have the host
// re-register the catalogue the refresh is about to replace.
func TestRunRefreshesThenPublishes(t *testing.T) {
	fixture := &fakeHost{listOK: true}
	fixture.install(t)

	var order []string
	result := Run(Request{
		Host:     &abiboot.Host{},
		Provider: "demo",
		AuthName: "acct.json",
		Refresh: func() (Outcome, error) {
			order = append(order, "refresh")
			return Outcome{Models: 7, Changed: true}, nil
		},
	})
	order = append(order, "after")

	if len(order) != 2 || order[0] != "refresh" {
		t.Fatalf("order = %v, want the refresh first", order)
	}
	if result.Err != nil {
		t.Fatalf("Run: %v", result.Err)
	}
	if result.Models != 7 || !result.Changed || !result.Published {
		t.Errorf("result = %+v, want models=7 changed published", result)
	}
	if fixture.saves != 1 {
		t.Errorf("saves = %d, want 1", fixture.saves)
	}
}

// A failed refresh must not publish: the host would re-register the unchanged
// catalogue, and the operator would see "published" for a refresh that failed.
func TestRunDoesNotPublishAfterAFailedRefresh(t *testing.T) {
	fixture := &fakeHost{listOK: true}
	fixture.install(t)

	result := Run(Request{
		Host:     &abiboot.Host{},
		Provider: "demo",
		AuthName: "acct.json",
		Refresh:  func() (Outcome, error) { return Outcome{}, errors.New("vendor down") },
	})
	if result.Err == nil {
		t.Fatal("want a refresh error")
	}
	if result.Published || fixture.saves != 0 {
		t.Errorf("published after a failed refresh: %+v, saves=%d", result, fixture.saves)
	}
}

// An empty auth name refreshes the cache only. This is the automatic path, which
// must never rewrite an auth file the host is also writing.
func TestRunWithoutAuthNameSkipsPublishing(t *testing.T) {
	fixture := &fakeHost{listOK: true}
	fixture.install(t)

	result := Run(Request{
		Host:     &abiboot.Host{},
		Provider: "demo",
		Refresh:  func() (Outcome, error) { return Outcome{Models: 3}, nil },
	})
	if result.Err != nil {
		t.Fatalf("Run: %v", result.Err)
	}
	if result.Published || fixture.saves != 0 {
		t.Errorf("an automatic refresh published: %+v", result)
	}
}

func TestParseInterval(t *testing.T) {
	cases := []struct {
		name string
		raw  any
		want time.Duration
	}{
		{"nil is disabled", nil, 0},
		{"zero is disabled", 0, 0},
		{"negative is disabled", -1, 0},
		{"milliseconds", 60000, time.Minute},
		{"duration string", "30m", 30 * time.Minute},
		{"numeric string", "90000", 90 * time.Second},
		{"blank string", "  ", 0},
		{"garbage string", "soon", 0},
		{"float", float64(30000), 30 * time.Second},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := ParseInterval(testCase.raw); got != testCase.want {
				t.Errorf("ParseInterval(%v) = %v, want %v", testCase.raw, got, testCase.want)
			}
		})
	}
}

func TestSchedulerDisabledByDefault(t *testing.T) {
	scheduler := NewScheduler(0)
	if scheduler.Enabled() {
		t.Error("a zero interval must not enable the scheduler")
	}
	// Start on a disabled scheduler must be harmless.
	scheduler.Start(Request{})
	scheduler.Stop()
}

func TestSchedulerRunsAndStops(t *testing.T) {
	fixture := &fakeHost{listOK: true}
	fixture.install(t)

	calls := make(chan struct{}, 8)
	scheduler := NewScheduler(10 * time.Millisecond)
	scheduler.Start(Request{
		Provider: "demo",
		Refresh: func() (Outcome, error) {
			calls <- struct{}{}
			return Outcome{Models: 1}, nil
		},
		// No AuthName: the automatic path must not rewrite an auth file.
	})
	defer scheduler.Stop()

	select {
	case <-calls:
	case <-time.After(2 * time.Second):
		t.Fatal("the scheduler never fired")
	}
	// A second Start must not add a loop.
	scheduler.Start(Request{})
	scheduler.Stop()
	scheduler.Stop() // idempotent

	if fixture.saves != 0 {
		t.Errorf("the automatic path published %d times, want 0", fixture.saves)
	}
	interval, runs, _, _ := scheduler.Status()
	if interval != 10*time.Millisecond || runs == 0 {
		t.Errorf("status = (%v, %d), want the interval and at least one run", interval, runs)
	}
}

// Concurrent refreshes must not interleave: two accounts refreshing at once
// would fetch the same vendor catalogue twice and race on the same cache.
func TestRunSerialisesConcurrentRefreshes(t *testing.T) {
	fixture := &fakeHost{listOK: true}
	fixture.install(t)

	var mu sync.Mutex
	active, peak := 0, 0
	var wait sync.WaitGroup
	for index := 0; index < 8; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			Run(Request{
				Provider: "demo",
				Refresh: func() (Outcome, error) {
					mu.Lock()
					active++
					if active > peak {
						peak = active
					}
					mu.Unlock()
					time.Sleep(2 * time.Millisecond)
					mu.Lock()
					active--
					mu.Unlock()
					return Outcome{Models: 1}, nil
				},
			})
		}()
	}
	wait.Wait()
	if peak != 1 {
		t.Errorf("peak concurrent refreshes = %d, want 1", peak)
	}
}

// The automatic path must reach the host registry, or `GET /v1/models` stays
// stale forever: the catalogue a client sees is the host's, not this cache.
func TestPublishOnChangePublishesWhenTheCatalogueMoved(t *testing.T) {
	fixture := &fakeHost{listOK: true}
	fixture.install(t)

	result := Run(Request{
		Host:            &abiboot.Host{},
		Provider:        "demo",
		PublishOnChange: true,
		Refresh:         func() (Outcome, error) { return Outcome{Models: 9, Changed: true}, nil },
	})
	if result.Err != nil {
		t.Fatalf("Run: %v", result.Err)
	}
	if !result.Published {
		t.Errorf("a moved catalogue was not published: %+v", result)
	}
	if fixture.saves != 1 {
		t.Errorf("saves = %d, want 1", fixture.saves)
	}
	if fixture.saved["acct.json"] == nil {
		t.Errorf("published through the wrong file: %v", fixture.saved)
	}
}

// A stable catalogue must cost nothing: publishing every tick would rewrite an
// auth file the host also writes when it renews a token.
func TestPublishOnChangeStaysSilentWhenNothingMoved(t *testing.T) {
	fixture := &fakeHost{listOK: true}
	fixture.install(t)

	result := Run(Request{
		Host:            &abiboot.Host{},
		Provider:        "demo",
		PublishOnChange: true,
		Refresh:         func() (Outcome, error) { return Outcome{Models: 9, Changed: false}, nil },
	})
	if result.Err != nil {
		t.Fatalf("Run: %v", result.Err)
	}
	if result.Published || fixture.saves != 0 {
		t.Errorf("an unchanged catalogue wrote the auth file: %+v, saves=%d", result, fixture.saves)
	}
	if result.PublishSkipped == "" {
		t.Error("a skipped publish must say why, or the page reads as an unexplained no-op")
	}
}

// Without PublishOnChange and without an explicit name, Run stays cache-only —
// the behaviour the per-plugin tests assert for a plain tick.
func TestRunWithoutAnyPublishRequestStaysCacheOnly(t *testing.T) {
	fixture := &fakeHost{listOK: true}
	fixture.install(t)

	result := Run(Request{
		Host:     &abiboot.Host{},
		Provider: "demo",
		Refresh:  func() (Outcome, error) { return Outcome{Models: 9, Changed: true}, nil },
	})
	if result.Published || fixture.saves != 0 {
		t.Errorf("Run published without being asked to: %+v", result)
	}
}

// An explicit auth name publishes even when nothing moved: an operator pressing
// the button expects the host to re-register regardless.
func TestExplicitAuthNamePublishesAnUnchangedCatalogue(t *testing.T) {
	fixture := &fakeHost{listOK: true}
	fixture.install(t)

	result := Run(Request{
		Host:     &abiboot.Host{},
		Provider: "demo",
		AuthName: "acct.json",
		Refresh:  func() (Outcome, error) { return Outcome{Models: 9, Changed: false}, nil },
	})
	if !result.Published {
		t.Errorf("the manual path skipped publishing: %+v", result)
	}
	if fixture.saves != 1 {
		t.Errorf("saves = %d, want 1", fixture.saves)
	}
}

// An automatic publish with no account to write through must say so rather than
// reporting success.
func TestPublishOnChangeReportsWhenNoAccountExists(t *testing.T) {
	fixture := &fakeHost{listOK: true}
	fixture.install(t)

	result := Run(Request{
		Host:            &abiboot.Host{},
		Provider:        "absent-provider",
		PublishOnChange: true,
		Refresh:         func() (Outcome, error) { return Outcome{Models: 9, Changed: true}, nil },
	})
	if result.Published || fixture.saves != 0 {
		t.Errorf("published with no matching account: %+v", result)
	}
	if result.PublishSkipped == "" {
		t.Error("the skip must be explained")
	}
}

// The tick path end to end, including the bare host handle a background
// goroutine uses. The resolution of the publish target must happen at TICK time,
// not at Start time: Configure runs before any account exists, so a name
// captured then would be empty forever and the automatic publish would silently
// never fire.
func TestSchedulerTickPublishesOnChangeAndStaysSilentOtherwise(t *testing.T) {
	fixture := &fakeHost{listOK: true}
	fixture.install(t)

	// The account appears only after the scheduler is already running, which is
	// what the install → configure → login order looks like in production.
	fixture.listEmpty = true

	changed := make(chan bool, 8)
	scheduler := NewScheduler(10 * time.Millisecond)
	scheduler.Start(Request{
		Host:            &abiboot.Host{},
		Provider:        "demo",
		PublishOnChange: true,
		Refresh: func() (Outcome, error) {
			// No signal means "the catalogue did not move": a tick before the
			// move must stay silent, so the default has to be false.
			moved := false
			select {
			case moved = <-changed:
			default:
			}
			return Outcome{Models: 4, Changed: moved}, nil
		},
	})
	defer scheduler.Stop()

	// First ticks report no change → nothing may be written.
	time.Sleep(60 * time.Millisecond)
	if count := fixture.saveCount(); count != 0 {
		t.Fatalf("an unchanged catalogue wrote the auth file %d times", count)
	}

	// The account now exists, and the catalogue moves.
	fixture.mu.Lock()
	fixture.listEmpty = false
	fixture.mu.Unlock()
	changed <- true

	deadline := time.After(3 * time.Second)
	for fixture.saveCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("a moved catalogue never reached the host")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if saves := fixture.saveCount(); saves != 1 {
		t.Errorf("saves = %d, want exactly 1 for one change", saves)
	}
}
