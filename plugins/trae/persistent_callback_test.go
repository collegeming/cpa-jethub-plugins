package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/oauthcb"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// getCallbackStatus performs the GET the browser performs against the loopback
// listener. A dial error is the ERR_CONNECTION_REFUSED this contract exists to
// prevent, so it is returned rather than turned into a fatal here.
func getCallbackStatus(rawURL string) (int, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	response, errGet := client.Get(rawURL)
	if errGet != nil {
		return 0, errGet
	}
	defer func() { _ = response.Body.Close() }()
	return response.StatusCode, nil
}

// waitForCallback polls the session the way the panel's auth.login.poll does
// until the named callback arrives; the dispatcher delivers asynchronously.
func waitForCallback(t *testing.T, session *loginSession, wantRefreshToken string) url.Values {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if query, ok := session.takeCallback(); ok && query.Get("refreshToken") == wantRefreshToken {
			return query
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no callback carrying refreshToken=%q reached the session", wantRefreshToken)
	return nil
}

// TestCallbackListenerSurvivesSessionEnd is the production regression guard: the
// portal sends the browser to the published port at a moment when no sign-in is
// in flight (the earlier one timed out, was superseded, or the container
// restarted). The listener must still answer, and the next sign-in must reuse it
// and receive the callback through it.
func TestCallbackListenerSurvivesSessionEnd(t *testing.T) {
	cfg, port := pinnedCallbackConfig(t)
	t.Cleanup(shutdownLoginSessions)

	first, errFirst := startLoginSession(cfg)
	if errFirst != nil {
		t.Fatalf("first startLoginSession: %v", errFirst)
	}
	first.expire("登录会话已超时，请重新发起")

	// The portal's redirect can arrive with nothing in flight — the exact moment
	// the container used to answer ERR_CONNECTION_REFUSED. It must be answered
	// now; with no pending session the captured callback is simply dropped, so the
	// user retries the sign-in.
	orphanURL := first.RedirectURI() + "?refreshToken=" + url.QueryEscape("rt-orphan")
	status, errProbe := getCallbackStatus(orphanURL)
	if errProbe != nil {
		t.Fatalf("callback port %d stopped listening after the session ended: %v", port, errProbe)
	}
	if status != http.StatusOK {
		t.Fatalf("orphan callback status = %d, want the listener's success page", status)
	}

	second, errSecond := startLoginSession(cfg)
	if errSecond != nil {
		t.Fatalf("second startLoginSession: %v", errSecond)
	}
	if second.Port != first.Port {
		t.Fatalf("second session port = %d, want the reused %d", second.Port, first.Port)
	}

	// The portal's redirect, as the browser performs it.
	callbackURL := second.RedirectURI() + "?refreshToken=" + url.QueryEscape("rt-from-browser")
	status, errGet := getCallbackStatus(callbackURL)
	if errGet != nil {
		t.Fatalf("browser callback to %s failed: %v", callbackURL, errGet)
	}
	if status != http.StatusOK {
		t.Fatalf("callback status = %d, want the listener's success page", status)
	}

	query := waitForCallback(t, second, "rt-from-browser")
	if query.Get("refreshToken") != "rt-from-browser" {
		t.Fatalf("callback query = %v, want the browser's parameters", query)
	}
	// The ended session is not revived by a callback that belongs to the new one.
	if _, ok := first.takeCallback(); ok {
		t.Fatal("a callback was stored on a session that had already ended")
	}
}

// TestDeliverRoutesUsableCallback: a usable callback is stored on the pending
// session, which stays pending so the panel's poll performs the exchange.
func TestDeliverRoutesUsableCallback(t *testing.T) {
	cfg, _ := pinnedCallbackConfig(t)
	t.Cleanup(shutdownLoginSessions)

	session, errStart := startLoginSession(cfg)
	if errStart != nil {
		t.Fatalf("startLoginSession: %v", errStart)
	}

	session.deliver(oauthcb.Result{
		Query: url.Values{"refreshToken": {"rt-usable"}, "userInfo": {`{"UserID":"u-1"}`}},
		Path:  CallbackPath,
	})

	query, ok := session.takeCallback()
	if !ok {
		t.Fatal("usable callback was not routed to the pending session")
	}
	if query.Get("refreshToken") != "rt-usable" {
		t.Fatalf("stored callback = %v, want the delivered query", query)
	}
	if status, _, _, finished := session.snapshot(); finished {
		t.Fatalf("status = %v, want the session left pending for the panel's poll", status)
	}
}

// TestDeliverRejectsUnusableCallback: a callback carrying neither refreshToken,
// userJwt.Token nor a usable auth code can never complete a sign-in. This
// plugin's rule is to settle the session with the parse reason, so the panel's
// poll reports why instead of attempting an exchange or spinning forever.
func TestDeliverRejectsUnusableCallback(t *testing.T) {
	cfg, _ := pinnedCallbackConfig(t)
	t.Cleanup(shutdownLoginSessions)

	session, errStart := startLoginSession(cfg)
	if errStart != nil {
		t.Fatalf("startLoginSession: %v", errStart)
	}

	// A PKCE-only callback: the flow this implementation does not support.
	session.deliver(oauthcb.Result{Query: url.Values{"code": {"pkce-only"}}, Path: CallbackPath})

	status, message, _, finished := session.snapshot()
	if !finished {
		t.Fatal("unusable callback left the session pending, want it settled")
	}
	if status != pluginapi.AuthLoginStatusError {
		t.Fatalf("status = %v, want error", status)
	}
	if !strings.Contains(message, "回调无效") {
		t.Fatalf("message = %q, want it to carry the rejection reason", message)
	}

	// The panel's poll must report that reason and never exchange the callback.
	request, errMarshal := json.Marshal(pluginapi.AuthLoginPollRequest{Provider: ProviderKey, State: session.State})
	if errMarshal != nil {
		t.Fatalf("marshal poll request: %v", errMarshal)
	}
	value, errPoll := handleAuthLoginPoll(nil, request)
	if errPoll != nil {
		t.Fatalf("handleAuthLoginPoll: %v", errPoll)
	}
	response, ok := value.(pluginapi.AuthLoginPollResponse)
	if !ok {
		t.Fatalf("poll response = %T", value)
	}
	if response.Status != pluginapi.AuthLoginStatusError || !strings.Contains(response.Message, "回调无效") {
		t.Fatalf("poll response = %v %q, want the rejection reason", response.Status, response.Message)
	}
	if len(response.Auth.StorageJSON) != 0 {
		t.Fatal("an unusable callback produced a credential")
	}
}

// TestDeliverIgnoresCallbackForFinishedSession: the shared listener keeps
// accepting long after a sign-in ends, so late callbacks are normal and must not
// revive a settled session.
func TestDeliverIgnoresCallbackForFinishedSession(t *testing.T) {
	session := &loginSession{ExpiresAt: time.Now().Add(time.Minute)}
	session.expire("登录会话已超时，请重新发起")

	session.deliver(oauthcb.Result{Query: url.Values{"refreshToken": {"rt-late"}}, Path: CallbackPath})

	if _, ok := session.takeCallback(); ok {
		t.Fatal("late callback was stored on a settled session")
	}
	status, message, _, finished := session.snapshot()
	if !finished || status != pluginapi.AuthLoginStatusError || !strings.Contains(message, "超时") {
		t.Fatalf("settled session changed to %v %q (finished=%v)", status, message, finished)
	}
}

// TestRepeatedPinnedSignInsNeverFail: the panel's 重试 can be clicked over and
// over. A pinned port must be handed to every attempt while the shared listener
// stays bound, whether the previous attempt is still pending or already failed.
func TestRepeatedPinnedSignInsNeverFail(t *testing.T) {
	cfg, port := pinnedCallbackConfig(t)
	t.Cleanup(shutdownLoginSessions)

	sessions := make([]*loginSession, 0, 5)
	for attempt := 1; attempt <= 5; attempt++ {
		session, errStart := startLoginSession(cfg)
		if errStart != nil {
			t.Fatalf("sign-in %d: %v", attempt, errStart)
		}
		if session.Port != port {
			t.Fatalf("sign-in %d bound port %d, want the pinned %d", attempt, session.Port, port)
		}
		sessions = append(sessions, session)
		if attempt == 2 {
			// The even attempts end by themselves (a failed exchange), the odd
			// ones stay pending and are superseded by the next retry.
			session.fail("TRAE 登录回调无效：test")
		}
	}

	for index, session := range sessions[:len(sessions)-1] {
		if _, _, _, finished := session.snapshot(); !finished {
			t.Fatalf("attempt %d is still pending, want it settled by the retry", index+1)
		}
	}
	if status, _, _, finished := sessions[len(sessions)-1].snapshot(); finished {
		t.Fatalf("last attempt is settled (%v), want it pending", status)
	}
}

// TestCallbackListenerRebuildsOnSettingsChange: a deployment that republishes
// the callback port must not keep handing out the old one. The cache key covers
// every option that shapes the listener, so a changed port closes the old
// listener and binds the new one.
func TestCallbackListenerRebuildsOnSettingsChange(t *testing.T) {
	firstCfg, firstPort := pinnedCallbackConfig(t)
	secondCfg, secondPort := pinnedCallbackConfig(t)
	// The OS is free to hand the same ephemeral port back; that would make the
	// key identical and the test meaningless.
	for attempt := 0; secondPort == firstPort && attempt < 10; attempt++ {
		secondCfg, secondPort = pinnedCallbackConfig(t)
	}
	if secondPort == firstPort {
		t.Skipf("no second free port available besides %d", firstPort)
	}
	t.Cleanup(shutdownLoginSessions)

	session, errFirst := startLoginSession(firstCfg)
	if errFirst != nil {
		t.Fatalf("first startLoginSession: %v", errFirst)
	}
	if session.Port != firstPort {
		t.Fatalf("session port = %d, want %d", session.Port, firstPort)
	}

	// The operator republishes the callback port; the next sign-in must land on
	// the new one and the replaced listener must release the old one.
	moved, errSecond := startLoginSession(secondCfg)
	if errSecond != nil {
		t.Fatalf("startLoginSession after the port moved: %v", errSecond)
	}
	if moved.Port != secondPort {
		t.Fatalf("session port after the move = %d, want %d", moved.Port, secondPort)
	}
	oldURL := fmt.Sprintf("http://127.0.0.1:%d%s", firstPort, CallbackPath)
	if _, errProbe := getCallbackStatus(oldURL); errProbe == nil {
		t.Fatalf("port %d still answers after the listener moved to %d", firstPort, secondPort)
	}
}

// TestConcurrentPinnedStartsLeaveOnePending: a pinned deployment carries one
// sign-in at a time, so racing starts must not each leave a pending session
// behind. Settling the replaced attempt and registering the new one share one
// critical section, which leaves exactly the last registration pending.
func TestConcurrentPinnedStartsLeaveOnePending(t *testing.T) {
	cfg, port := pinnedCallbackConfig(t)
	t.Cleanup(shutdownLoginSessions)

	const starts = 4
	started := make(chan *loginSession, starts)
	failed := make(chan error, starts)
	for index := 0; index < starts; index++ {
		go func() {
			session, errStart := startLoginSession(cfg)
			if errStart != nil {
				failed <- errStart
				return
			}
			started <- session
		}()
	}

	for index := 0; index < starts; index++ {
		select {
		case errStart := <-failed:
			t.Fatalf("concurrent start on pinned port %d failed: %v", port, errStart)
		case session := <-started:
			if session.Port != port {
				t.Fatalf("session port = %d, want the pinned %d", session.Port, port)
			}
		}
	}

	pending := 0
	loginMu.Lock()
	for _, session := range loginSessions {
		if _, _, _, finished := session.snapshot(); !finished {
			pending++
		}
	}
	loginMu.Unlock()
	if pending != 1 {
		t.Fatalf("pending sessions = %d, want exactly 1", pending)
	}
}
