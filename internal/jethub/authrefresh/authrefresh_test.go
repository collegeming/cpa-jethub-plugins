package authrefresh

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This file pins the four promises the helper makes to every provider plugin:
//
//  1. an expired credential is renewed once, and the caller is handed the new
//     bytes;
//  2. a valid credential is left alone — the transport is not called at all;
//  3. concurrent callers share one renewal;
//  4. a failure is reported, never retried inside the cooldown, and a terminal
//     failure stops being retried at all — while a transport failure is never
//     dressed up as a dead credential.
//
// Everything is offline: the "vendor" is a closure and the "host" is a fake
// transport installed through abiboot.SetHostCaller.

// testCredential is a stand-in for a provider credential.
type testCredential struct {
	Type         string `json:"type"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    string `json:"expires_at"`
}

// testHost is the fake host transport: it records every host callback the
// helper makes.
type testHost struct {
	mu        sync.Mutex
	httpCalls int
	saved     []savedAuth
	logs      []string
}

type savedAuth struct {
	Name string
	JSON json.RawMessage
}

// install registers the fake transport and returns the per-invocation handle.
func (f *testHost) install(t *testing.T) *abiboot.Host {
	t.Helper()
	abiboot.SetHostCaller(func(method string, request []byte) ([]byte, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch method {
		case pluginabi.MethodHostHTTPDo:
			f.httpCalls++
			return nil, errors.New("unexpected host.http.do: this test expects no vendor call")
		case pluginabi.MethodHostAuthSave:
			var payload struct {
				Name string          `json:"name"`
				JSON json.RawMessage `json:"json"`
			}
			if errDecode := json.Unmarshal(request, &payload); errDecode != nil {
				return nil, errDecode
			}
			f.saved = append(f.saved, savedAuth{Name: payload.Name, JSON: payload.JSON})
			return abiboot.OK(map[string]any{"name": payload.Name, "path": "/auths/" + payload.Name})
		case pluginabi.MethodHostLog:
			var payload struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(request, &payload)
			f.logs = append(f.logs, payload.Message)
			return abiboot.OK(map[string]any{})
		}
		return nil, fmt.Errorf("unexpected host callback %s", method)
	})
	t.Cleanup(abiboot.ClearHostCaller)
	return &abiboot.Host{CallbackID: "cb-1", PluginID: "demo"}
}

func (f *testHost) saveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.saved)
}

// validCredential is what a renewable credential looks like.
func validCredential(expiresAt time.Time) testCredential {
	return testCredential{
		Type:         "demo",
		AccessToken:  "old-token",
		RefreshToken: "refresh-token",
		ExpiresAt:    expiresAt.UTC().Format(time.RFC3339),
	}
}

func encodeCredential(t *testing.T, credential testCredential) json.RawMessage {
	t.Helper()
	raw, errMarshal := json.Marshal(credential)
	if errMarshal != nil {
		t.Fatalf("encode credential: %v", errMarshal)
	}
	return raw
}

func decodeCredential(t *testing.T, storage json.RawMessage) testCredential {
	t.Helper()
	var credential testCredential
	if errUnmarshal := json.Unmarshal(storage, &credential); errUnmarshal != nil {
		t.Fatalf("decode credential %s: %v", storage, errUnmarshal)
	}
	return credential
}

// expiryOfTestCredential is the policy reader every test uses.
func expiryOfTestCredential(storage json.RawMessage) (time.Time, bool) {
	var credential testCredential
	if errUnmarshal := json.Unmarshal(storage, &credential); errUnmarshal != nil {
		return time.Time{}, false
	}
	if credential.ExpiresAt == "" {
		return time.Time{}, false
	}
	parsed, errParse := time.Parse(time.RFC3339, credential.ExpiresAt)
	if errParse != nil {
		return time.Time{}, false
	}
	return parsed, true
}

// newTestRefresher builds the policy under test around one vendor closure.
func newTestRefresher(refresh RefreshFunc, now func() time.Time) *Refresher {
	refresher := New(Policy{
		Provider:    "demo",
		Lead:        time.Hour,
		Cooldown:    5 * time.Minute,
		Expiry:      expiryOfTestCredential,
		Refreshable: func(storage json.RawMessage) bool { return decodeRefreshable(storage) },
		Refresh:     refresh,
	})
	if now != nil {
		refresher.now = now
	}
	return refresher
}

func decodeRefreshable(storage json.RawMessage) bool {
	var credential testCredential
	if errUnmarshal := json.Unmarshal(storage, &credential); errUnmarshal != nil {
		return false
	}
	return credential.RefreshToken != ""
}

// TestEnsureRenewsExpiredCredential is promise 1: the expired credential is
// renewed exactly once, the refreshed bytes are handed back, and they are
// persisted under the name the host knows.
func TestEnsureRenewsExpiredCredential(t *testing.T) {
	base := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	host := &testHost{}
	handle := host.install(t)

	var calls int32
	refresher := newTestRefresher(func(_ *abiboot.Host, request pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
		atomic.AddInt32(&calls, 1)
		old := decodeCredential(t, request.StorageJSON)
		if old.AccessToken != "old-token" {
			t.Errorf("refresh received %q, want the caller's credential", old.AccessToken)
		}
		if request.AuthID != "demo-account.json" {
			t.Errorf("refresh AuthID = %q, want the host-known file name", request.AuthID)
		}
		refreshed := old
		refreshed.AccessToken = "new-token"
		refreshed.ExpiresAt = base.Add(12 * time.Hour).Format(time.RFC3339)
		return pluginapi.AuthRefreshResponse{
			Auth: pluginapi.AuthData{
				ID:          "demo-account.json",
				FileName:    "demo-account.json",
				StorageJSON: encodeCredential(t, refreshed),
			},
		}, nil
	}, func() time.Time { return base })

	expired := encodeCredential(t, validCredential(base.Add(-time.Minute)))
	result, errEnsure := refresher.Ensure(handle, Request{Name: "demo-account.json", StorageJSON: expired})
	if errEnsure != nil {
		t.Fatalf("Ensure returned %v, want success", errEnsure)
	}
	if !result.Refreshed {
		t.Fatal("result.Refreshed = false, want true")
	}
	if !result.Attempted {
		t.Fatal("result.Attempted = false, want true")
	}
	if result.Expired {
		// The refreshed credential expires in 12h: reporting the OLD expiry
		// would tell the page the account is still dead.
		t.Fatal("result.Expired = true after a successful renewal, want false")
	}
	if got := decodeCredential(t, result.Storage).AccessToken; got != "new-token" {
		t.Fatalf("result.Storage access_token = %q, want new-token", got)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}

	host.mu.Lock()
	saved := append([]savedAuth(nil), host.saved...)
	host.mu.Unlock()
	if len(saved) != 1 {
		t.Fatalf("saved files = %d, want 1", len(saved))
	}
	if saved[0].Name != "demo-account.json" {
		t.Fatalf("saved name = %q, want demo-account.json", saved[0].Name)
	}
	if got := decodeCredential(t, saved[0].JSON).AccessToken; got != "new-token" {
		t.Fatalf("saved access_token = %q, want the refreshed one", got)
	}
}

// TestEnsureLeavesValidCredentialAlone is promise 2, and it is the negative
// case the live instance is in today: a credential with hours left must not
// produce a vendor call.
func TestEnsureLeavesValidCredentialAlone(t *testing.T) {
	base := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	host := &testHost{}
	handle := host.install(t)

	var calls int32
	refresher := newTestRefresher(func(_ *abiboot.Host, _ pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
		atomic.AddInt32(&calls, 1)
		return pluginapi.AuthRefreshResponse{}, errors.New("must not be called")
	}, func() time.Time { return base })

	valid := encodeCredential(t, validCredential(base.Add(6*time.Hour)))
	result, errEnsure := refresher.Ensure(handle, Request{Name: "demo-account.json", StorageJSON: valid})
	if errEnsure != nil {
		t.Fatalf("Ensure returned %v, want success", errEnsure)
	}
	if result.Refreshed || result.Attempted {
		t.Fatalf("valid credential was refreshed: %+v", result)
	}
	if string(result.Storage) != string(valid) {
		t.Fatal("valid credential bytes were replaced")
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("refresh calls = %d, want 0", got)
	}
	if got := host.saveCount(); got != 0 {
		t.Fatalf("saved files = %d, want 0", got)
	}
	if host.httpCalls != 0 {
		t.Fatalf("host.http.do calls = %d, want 0", host.httpCalls)
	}
}

// TestEnsureLeavesCredentialWithoutExpiryAlone pins rule: no usable expiry, no
// action — even though the credential looks renewable.
func TestEnsureLeavesCredentialWithoutExpiryAlone(t *testing.T) {
	host := &testHost{}
	handle := host.install(t)

	var calls int32
	refresher := newTestRefresher(func(_ *abiboot.Host, _ pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
		atomic.AddInt32(&calls, 1)
		return pluginapi.AuthRefreshResponse{}, errors.New("must not be called")
	}, nil)

	timeless := encodeCredential(t, testCredential{Type: "demo", AccessToken: "token", RefreshToken: "refresh-token"})
	result, errEnsure := refresher.Ensure(handle, Request{Name: "demo-account.json", StorageJSON: timeless})
	if errEnsure != nil {
		t.Fatalf("Ensure returned %v, want success", errEnsure)
	}
	if result.Attempted || result.Refreshed || result.Expired {
		t.Fatalf("a credential with no expiry was acted on: %+v", result)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("refresh calls = %d, want 0", got)
	}
}

// TestEnsureLeavesUnrefreshableCredentialAlone covers a ticket-flow credential
// that is already expired: there is nothing to renew with, so the vendor is not
// asked, and the caller keeps its own "cannot renew" handling.
func TestEnsureLeavesUnrefreshableCredentialAlone(t *testing.T) {
	base := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	host := &testHost{}
	handle := host.install(t)

	var calls int32
	refresher := newTestRefresher(func(_ *abiboot.Host, _ pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
		atomic.AddInt32(&calls, 1)
		return pluginapi.AuthRefreshResponse{}, errors.New("must not be called")
	}, func() time.Time { return base })

	ticket := encodeCredential(t, testCredential{
		Type:        "demo",
		AccessToken: "ticket-token",
		ExpiresAt:   base.Add(-time.Hour).Format(time.RFC3339),
	})
	result, errEnsure := refresher.Ensure(handle, Request{Name: "demo-account.json", StorageJSON: ticket})
	if errEnsure != nil {
		t.Fatalf("Ensure returned %v, want success", errEnsure)
	}
	if result.Attempted || result.Refreshed {
		t.Fatalf("an unrefreshable credential was refreshed: %+v", result)
	}
	if !result.Expired {
		t.Fatal("result.Expired = false, want true for a past expiry")
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("refresh calls = %d, want 0", got)
	}
}

// TestEnsureWithoutAHostStandsAside pins that a missing host handle — which
// only a unit test can produce — is not turned into a credential failure: the
// provider's own renewal fallback must still get its chance.
func TestEnsureWithoutAHostStandsAside(t *testing.T) {
	base := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

	var calls int32
	refresher := newTestRefresher(func(_ *abiboot.Host, _ pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
		atomic.AddInt32(&calls, 1)
		return pluginapi.AuthRefreshResponse{}, errors.New("must not be called")
	}, func() time.Time { return base })

	expired := encodeCredential(t, validCredential(base.Add(-time.Hour)))
	result, errEnsure := refresher.Ensure(nil, Request{Name: "demo-account.json", StorageJSON: expired})
	if errEnsure != nil {
		t.Fatalf("Ensure returned %v, want the credential to be left alone", errEnsure)
	}
	if result.Attempted || result.Refreshed {
		t.Fatalf("a nil host provoked an attempt: %+v", result)
	}
	if !result.Expired {
		t.Fatal("result.Expired = false, want true for a past expiry")
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("refresh calls = %d, want 0", got)
	}
}

// TestEnsureNoRefreshPathIsANoop pins the Loomy rule: a provider with no
// refresh path never touches the vendor, whatever the expiry says.
func TestEnsureNoRefreshPathIsANoop(t *testing.T) {
	base := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	host := &testHost{}
	handle := host.install(t)

	refresher := New(Policy{Provider: "loomy", Lead: time.Hour, Expiry: expiryOfTestCredential})
	refresher.now = func() time.Time { return base }

	expired := encodeCredential(t, validCredential(base.Add(-time.Hour)))
	result, errEnsure := refresher.Ensure(handle, Request{Name: "loomy-account.json", StorageJSON: expired})
	if errEnsure != nil {
		t.Fatalf("Ensure returned %v, want success", errEnsure)
	}
	if result.Attempted || result.Refreshed || !result.Expired {
		t.Fatalf("probe-only provider acted on the credential: %+v", result)
	}
	if got := host.saveCount(); got != 0 {
		t.Fatalf("saved files = %d, want 0", got)
	}
}

// TestEnsureSharesOneRefreshBetweenConcurrentCallers is promise 3: a page load
// and an inference request arriving together cost the vendor one renewal.
func TestEnsureSharesOneRefreshBetweenConcurrentCallers(t *testing.T) {
	base := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	host := &testHost{}
	handle := host.install(t)

	var calls int32
	release := make(chan struct{})
	refresher := newTestRefresher(func(_ *abiboot.Host, request pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
		atomic.AddInt32(&calls, 1)
		// Hold the single-flight open long enough for every caller to arrive.
		<-release
		refreshed := decodeCredential(t, request.StorageJSON)
		refreshed.AccessToken = "new-token"
		refreshed.ExpiresAt = base.Add(12 * time.Hour).Format(time.RFC3339)
		return pluginapi.AuthRefreshResponse{
			Auth: pluginapi.AuthData{FileName: request.AuthID, StorageJSON: encodeCredential(t, refreshed)},
		}, nil
	}, func() time.Time { return base })

	expired := encodeCredential(t, validCredential(base.Add(-time.Minute)))
	const callers = 8
	results := make([]Result, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			results[index], errs[index] = refresher.Ensure(handle, Request{Name: "demo-account.json", StorageJSON: expired})
		}(i)
	}
	// Give every caller time to reach the single-flight, then let it finish.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("refresh calls = %d, want 1 (single-flight)", got)
	}
	for index := 0; index < callers; index++ {
		if errs[index] != nil {
			t.Fatalf("caller %d returned %v, want success", index, errs[index])
		}
		if got := decodeCredential(t, results[index].Storage).AccessToken; got != "new-token" {
			t.Fatalf("caller %d used %q, want the refreshed token", index, got)
		}
	}
	if got := host.saveCount(); got != 1 {
		t.Fatalf("saved files = %d, want 1", got)
	}
}

// TestEnsureFailureSurfacesAndBacksOff is promise 4's retryable half: the
// failure is reported every time, and the vendor is called once inside the
// cooldown — and a transport failure is never reported as an expired
// credential.
func TestEnsureFailureSurfacesAndBacksOff(t *testing.T) {
	base := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	host := &testHost{}
	handle := host.install(t)

	now := base
	var calls int32
	transportFailure := abiboot.Errorf("refresh_transport", "续期请求失败：dial tcp: connection refused")
	refresher := newTestRefresher(func(_ *abiboot.Host, _ pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
		atomic.AddInt32(&calls, 1)
		return pluginapi.AuthRefreshResponse{}, transportFailure
	}, func() time.Time { return now })

	expired := encodeCredential(t, validCredential(base.Add(-time.Minute)))
	first, errFirst := refresher.Ensure(handle, Request{Name: "demo-account.json", StorageJSON: expired})
	if !errors.Is(errFirst, transportFailure) {
		t.Fatalf("first Ensure error = %v, want the transport failure", errFirst)
	}
	if !errors.Is(first.Err, transportFailure) {
		t.Fatalf("result.Err = %v, want the same failure the caller keeps", first.Err)
	}
	if first.Dead {
		t.Fatal("a transport failure was classified as a dead credential")
	}
	if !first.Expired {
		t.Fatal("result.Expired = false, want true: the credential really is expired")
	}
	if string(first.Storage) != string(expired) {
		t.Fatal("a failed refresh replaced the credential bytes")
	}

	// Inside the cooldown: the recorded failure is reported again, and the
	// vendor is left alone.
	now = base.Add(2 * time.Minute)
	second, errSecond := refresher.Ensure(handle, Request{Name: "demo-account.json", StorageJSON: expired})
	if errSecond == nil || errSecond.Error() != transportFailure.Error() {
		t.Fatalf("second Ensure error = %v, want the recorded failure", errSecond)
	}
	if second.Err == nil {
		t.Fatal("the backed-off result lost the recorded failure")
	}
	if second.Attempted {
		t.Fatal("second Ensure attempted a refresh inside the cooldown")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("refresh calls inside the cooldown = %d, want 1", got)
	}

	// Past the cooldown: one more attempt, which is what makes the failure
	// self-healing rather than permanent.
	now = base.Add(6 * time.Minute)
	if _, errThird := refresher.Ensure(handle, Request{Name: "demo-account.json", StorageJSON: expired}); errThird == nil {
		t.Fatal("third Ensure error = nil, want the failure")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("refresh calls after the cooldown = %d, want 2", got)
	}
}

// TestEnsureTerminalFailureMarksDead is promise 4's terminal half: a 401 stops
// the attempts entirely, keeps reporting the reason, and a re-login (new bytes)
// revives the credential.
func TestEnsureTerminalFailureMarksDead(t *testing.T) {
	base := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	host := &testHost{}
	handle := host.install(t)

	now := base
	var calls int32
	terminal := abiboot.HTTPError("refresh_token_expired", http.StatusUnauthorized, "demo refresh_token 已失效，请重新登录")
	refresher := newTestRefresher(func(_ *abiboot.Host, _ pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
		atomic.AddInt32(&calls, 1)
		return pluginapi.AuthRefreshResponse{}, terminal
	}, func() time.Time { return now })

	expired := encodeCredential(t, validCredential(base.Add(-time.Minute)))
	first, errFirst := refresher.Ensure(handle, Request{Name: "demo-account.json", StorageJSON: expired})
	if errFirst == nil {
		t.Fatal("first Ensure error = nil, want the terminal failure")
	}
	if !first.Dead {
		t.Fatal("result.Dead = false, want true for a 401-style failure")
	}

	// Well past the cooldown, the credential is still not retried: the vendor
	// already said the grant is dead.
	now = base.Add(48 * time.Hour)
	second, errSecond := refresher.Ensure(handle, Request{Name: "demo-account.json", StorageJSON: expired})
	if errSecond == nil {
		t.Fatal("second Ensure error = nil, want the recorded terminal failure")
	}
	if !second.Dead {
		t.Fatal("second Ensure lost the dead verdict")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("refresh calls for a dead credential = %d, want 1", got)
	}

	// A re-login replaces the bytes. The credential is a different one now and
	// must be refreshable again.
	relogged := encodeCredential(t, validCredential(base.Add(-time.Minute)))
	relogged = json.RawMessage(string(relogged[:len(relogged)-1]) + `,"marker":"relogin"}`)
	if _, errThird := refresher.Ensure(handle, Request{Name: "demo-account.json", StorageJSON: relogged}); errThird == nil {
		t.Fatal("third Ensure error = nil, want the refreshed credential attempt to run")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("refresh calls after a re-login = %d, want 2", got)
	}
}

// TestEnsureKeepsTheHostFileName pins the file-name rule: the provider may
// derive any name it likes, but the host's name wins — that is what keeps a
// refresh from minting a second auth file.
func TestEnsureKeepsTheHostFileName(t *testing.T) {
	base := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	host := &testHost{}
	handle := host.install(t)

	refresher := newTestRefresher(func(_ *abiboot.Host, request pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
		refreshed := decodeCredential(t, request.StorageJSON)
		refreshed.AccessToken = "new-token"
		refreshed.ExpiresAt = base.Add(12 * time.Hour).Format(time.RFC3339)
		return pluginapi.AuthRefreshResponse{
			Auth: pluginapi.AuthData{
				// A provider that derived a name from a rotating field.
				ID:          "derived-by-provider.json",
				FileName:    "derived-by-provider.json",
				StorageJSON: encodeCredential(t, refreshed),
			},
		}, nil
	}, func() time.Time { return base })

	expired := encodeCredential(t, validCredential(base.Add(-time.Minute)))
	result, errEnsure := refresher.Ensure(handle, Request{Name: "host-knows-this.json", StorageJSON: expired})
	if errEnsure != nil {
		t.Fatalf("Ensure returned %v, want success", errEnsure)
	}
	if !result.Refreshed {
		t.Fatal("result.Refreshed = false, want true")
	}

	host.mu.Lock()
	saved := append([]savedAuth(nil), host.saved...)
	host.mu.Unlock()
	if len(saved) != 1 {
		t.Fatalf("saved files = %d, want exactly 1", len(saved))
	}
	if saved[0].Name != "host-knows-this.json" {
		t.Fatalf("saved name = %q, want the host-known name", saved[0].Name)
	}
}

// TestEnsureRefusesToSaveWithoutAName pins the other half of that rule: a
// refresh that cannot be attributed to an auth file is not written at all,
// because writing it would mint a file the host does not know.
func TestEnsureRefusesToSaveWithoutAName(t *testing.T) {
	base := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	host := &testHost{}
	handle := host.install(t)

	refresher := newTestRefresher(func(_ *abiboot.Host, request pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
		refreshed := decodeCredential(t, request.StorageJSON)
		refreshed.AccessToken = "new-token"
		return pluginapi.AuthRefreshResponse{Auth: pluginapi.AuthData{StorageJSON: encodeCredential(t, refreshed)}}, nil
	}, func() time.Time { return base })

	expired := encodeCredential(t, validCredential(base.Add(-time.Minute)))
	result, errEnsure := refresher.Ensure(handle, Request{StorageJSON: expired})
	if errEnsure == nil {
		t.Fatal("Ensure error = nil, want a refusal to write an unnamed credential")
	}
	if result.Refreshed {
		t.Fatal("result.Refreshed = true, want false when nothing was persisted")
	}
	if got := host.saveCount(); got != 0 {
		t.Fatalf("saved files = %d, want 0", got)
	}
}

// TestIsTerminal pins the classification the whole scheme rests on: the
// provider's 401/403 is terminal, everything transport-shaped is not.
func TestIsTerminal(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"transport", abiboot.Errorf("refresh_transport", "dial tcp: connection refused"), false},
		{"explicitly retryable", abiboot.RetryableError("transport", "TRAE 续期网络失败"), false},
		{"unauthorized", abiboot.HTTPError("refresh_token_expired", http.StatusUnauthorized, "登录已失效"), true},
		{"forbidden", abiboot.HTTPError("refresh_failed", http.StatusForbidden, "拒绝"), true},
		{"session dead by code", abiboot.Errorf("session_dead", "登录态失效"), true},
		{"server error", abiboot.HTTPError("refresh_failed", http.StatusBadGateway, "502"), false},
		{"too many requests", abiboot.HTTPError("refresh_failed", http.StatusTooManyRequests, "429"), false},
		{"plain error", errors.New("boom"), false},
	}
	for _, testCase := range cases {
		if got := IsTerminal(testCase.err); got != testCase.want {
			t.Errorf("IsTerminal(%s) = %v, want %v", testCase.name, got, testCase.want)
		}
	}
}

// TestHandlerAdaptsAuthRefreshHandler pins that the in-plugin path runs the
// provider's own handler, so no provider has two renewal implementations.
func TestHandlerAdaptsAuthRefreshHandler(t *testing.T) {
	var seen pluginapi.AuthRefreshRequest
	refresh := Handler(func(_ *abiboot.Host, raw json.RawMessage) (any, error) {
		request, errDecode := abiboot.Decode[pluginapi.AuthRefreshRequest](raw)
		if errDecode != nil {
			return nil, errDecode
		}
		seen = request
		return pluginapi.AuthRefreshResponse{Auth: pluginapi.AuthData{FileName: "kept.json", StorageJSON: json.RawMessage(`{"type":"demo"}`)}}, nil
	})

	response, errRefresh := refresh(&abiboot.Host{}, pluginapi.AuthRefreshRequest{
		AuthID:      "kept.json",
		StorageJSON: json.RawMessage(`{"type":"demo","access_token":"x"}`),
	})
	if errRefresh != nil {
		t.Fatalf("refresh returned %v", errRefresh)
	}
	if seen.AuthID != "kept.json" {
		t.Fatalf("handler saw AuthID %q, want kept.json", seen.AuthID)
	}
	if response.Auth.FileName != "kept.json" {
		t.Fatalf("response file name = %q, want kept.json", response.Auth.FileName)
	}

	// A handler that fails must fail the same way it does for the host.
	failing := Handler(func(_ *abiboot.Host, _ json.RawMessage) (any, error) {
		return nil, abiboot.HTTPError("refresh_token_expired", http.StatusUnauthorized, "登录已失效")
	})
	if _, errFail := failing(&abiboot.Host{}, pluginapi.AuthRefreshRequest{}); !IsTerminal(errFail) {
		t.Fatalf("failing handler error = %v, want a terminal classification", errFail)
	}

	// And a handler that answers with something else is a contract error, not a
	// silent success.
	wrong := Handler(func(_ *abiboot.Host, _ json.RawMessage) (any, error) { return "nope", nil })
	if _, errWrong := wrong(&abiboot.Host{}, pluginapi.AuthRefreshRequest{}); errWrong == nil {
		t.Fatal("wrong handler type returned no error")
	}
}
