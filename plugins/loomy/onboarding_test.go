package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The key table is the execution order, and the point values must add up to the
// documented 10000 (`loomy-onboarding.ts:46-70`).
func TestOnboardingTable(t *testing.T) {
	want := []struct {
		key    string
		points int
		title  string
	}{
		{"first_message", 500, "发送你的第一条消息"},
		{"pick_skill", 1000, "试试选择一个技能"},
		{"generate_ppt", 1500, "生成第一份 PPT"},
		{"set_schedule", 1000, "设置定时任务"},
		{"install_skill", 1500, "在技能广场安装一个技能"},
		{"configure_remote", 1000, "配置远程控制"},
		{"create_soul", 1500, "创建你的第一个搭子"},
		{"share_soul", 2000, "把搭子分享给朋友"},
	}
	if len(onboardingTasks) != len(want) {
		t.Fatalf("table has %d entries, want %d", len(onboardingTasks), len(want))
	}
	total := 0
	for index, expected := range want {
		task := onboardingTasks[index]
		if task.Key != expected.key || task.Points != expected.points || task.Title != expected.title {
			t.Errorf("entry %d = %#v, want %#v", index, task, expected)
		}
		total += task.Points
	}
	if total != onboardingTotalPoints {
		t.Fatalf("total = %d, want %d", total, onboardingTotalPoints)
	}
	if _, known := onboardingTaskFor("first_message"); !known {
		t.Fatal("first_message must be a known key")
	}
	if _, known := onboardingTaskFor("not_a_task"); known {
		t.Fatal("an unknown key must not be accepted")
	}
}

// The state read must be a GET, must recompute `earned` locally
// (`loomy-onboarding.ts:27-28,94-97`) and must fall back to the known total.
func TestFetchOnboardingStateRecomputesEarned(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if request.Method != http.MethodGet {
			t.Errorf("method = %s, want GET: the status read is read-only", request.Method)
		}
		if got := request.Headers["token"]; len(got) != 1 || got[0] == "" {
			t.Errorf("token header = %#v, want the lowercase session header", got)
		}
		// The server claims 999999 earned, which must be ignored.
		return httpResponse(200, `{"code":"000000","data":{"tasks":{"first_message":true,"pick_skill":true},"earned":999999,"total":0}}`), nil
	}
	fake.install(t)

	state, errState := fetchOnboardingState(testHost(), sampleCredential(t), DefaultConfig())
	if errState != nil {
		t.Fatalf("fetchOnboardingState: %v", errState)
	}
	if state.Earned != 1500 {
		t.Fatalf("earned = %d, want the locally recomputed 1500 (500+1000)", state.Earned)
	}
	if state.Total != onboardingTotalPoints {
		t.Fatalf("total = %d, want the fallback %d for an unusable server value", state.Total, onboardingTotalPoints)
	}
}

// The server total is used when it is usable.
func TestFetchOnboardingStateUsesFiniteServerTotal(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"code":"000000","data":{"tasks":{},"earned":0,"total":12000}}`), nil
	}
	fake.install(t)
	state, errState := fetchOnboardingState(testHost(), sampleCredential(t), DefaultConfig())
	if errState != nil {
		t.Fatalf("fetchOnboardingState: %v", errState)
	}
	if state.Total != 12000 {
		t.Fatalf("total = %d, want the server value 12000", state.Total)
	}
	if state.Earned != 0 {
		t.Fatalf("earned = %d, want 0 for an empty task map", state.Earned)
	}
}

// Unknown keys are rejected LOCALLY, before any request
// (`loomy-onboarding.ts:189-192`).
func TestCompleteOnboardingTaskRejectsUnknownKeysLocally(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		t.Fatalf("an unknown key must not be submitted (%s)", request.URL)
		return nil, nil
	}
	fake.install(t)
	_, errComplete := completeOnboardingTask(testHost(), sampleCredential(t), "not_a_task", DefaultConfig())
	if errComplete == nil || statusOf(errComplete, 0) != http.StatusBadRequest {
		t.Fatalf("error = %v, want a 400 local rejection", errComplete)
	}
	if len(fake.requests) != 0 {
		t.Fatalf("issued %d requests, want 0", len(fake.requests))
	}
}

// The completion body carries ONLY the key (`loomy-onboarding.ts:23-25`), and a
// repeat is success via `alreadyCompleted`.
func TestCompleteOnboardingTaskBodyAndIdempotency(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if strings.TrimSpace(string(request.Body)) != `{"key":"first_message"}` {
			t.Errorf("body = %s, want only the key", request.Body)
		}
		return httpResponse(200, `{"code":"000000","data":{"alreadyCompleted":true,"balance":10000}}`), nil
	}
	fake.install(t)
	already, errComplete := completeOnboardingTask(testHost(), sampleCredential(t), "first_message", DefaultConfig())
	if errComplete != nil {
		t.Fatalf("completeOnboardingTask: %v", errComplete)
	}
	if !already {
		t.Fatal("alreadyCompleted must be reported back so the caller can treat a repeat as success")
	}
}

// claimAll skips tasks that are already true WITHOUT sending a request, posts the
// rest serially, and treats an alreadyCompleted replay as claimed
// (`loomy-onboarding.ts:209-237`).
func TestClaimAllSkipsCompletedAndClaimsTheRest(t *testing.T) {
	fake := newFakeHost()
	completed := map[string]bool{"first_message": true, "pick_skill": true}
	posts := 0
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if request.Method == http.MethodGet {
			body := `{"code":"000000","data":{"tasks":{"first_message":true,"pick_skill":true},"total":10000}}`
			return httpResponse(200, body), nil
		}
		posts++
		var payload struct {
			Key string `json:"key"`
		}
		if errUnmarshal := json.Unmarshal(request.Body, &payload); errUnmarshal != nil {
			t.Fatalf("decode completion body: %v", errUnmarshal)
		}
		if completed[payload.Key] {
			t.Fatalf("a completed task (%s) must not be posted again", payload.Key)
		}
		// Every remaining task answers alreadyCompleted=true: a replay is success.
		return httpResponse(200, `{"code":"000000","data":{"alreadyCompleted":true,"balance":10000}}`), nil
	}
	fake.install(t)

	outcome, errClaim := claimAllOnboarding(testHost(), sampleCredential(t), DefaultConfig())
	if errClaim != nil {
		t.Fatalf("claimAllOnboarding: %v", errClaim)
	}
	if posts != len(onboardingTasks)-2 {
		t.Fatalf("posted %d tasks, want %d", posts, len(onboardingTasks)-2)
	}
	if len(outcome.Skipped) != 2 || outcome.Skipped[0] != "first_message" || outcome.Skipped[1] != "pick_skill" {
		t.Fatalf("skipped = %#v, want the two completed keys in table order", outcome.Skipped)
	}
	if len(outcome.Claimed) != len(onboardingTasks)-2 {
		t.Fatalf("claimed = %d, want %d (a replay counts as claimed)", len(outcome.Claimed), len(onboardingTasks)-2)
	}
	if outcome.Earned != onboardingTotalPoints {
		t.Fatalf("earned = %d, want the full %d once every task counts as claimed", outcome.Earned, onboardingTotalPoints)
	}
	if outcome.Message == "" {
		t.Fatal("an outcome must carry a message")
	}
}

// A terminal `100002` aborts the run so no further doomed request is sent
// (`loomy-onboarding.ts:145-152,205-208`).
func TestClaimAllAbortsOnAuthExpired(t *testing.T) {
	fake := newFakeHost()
	posts := 0
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if request.Method == http.MethodGet {
			return httpResponse(200, `{"code":"000000","data":{"tasks":{},"total":10000}}`), nil
		}
		posts++
		return httpResponse(200, `{"code":"100002","desc":"缺少 token"}`), nil
	}
	fake.install(t)
	_, errClaim := claimAllOnboarding(testHost(), sampleCredential(t), DefaultConfig())
	if errClaim == nil {
		t.Fatal("a dead session must fail the run")
	}
	if statusOf(errClaim, 0) != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", statusOf(errClaim, 0))
	}
	if posts != 1 {
		t.Fatalf("posted %d completions, want exactly 1 before aborting", posts)
	}
}
