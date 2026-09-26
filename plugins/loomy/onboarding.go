package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
)

// Onboarding tasks: eight one-off actions worth 10000 points in total.
//
//	GET  /onboarding/tasks            -> data: { tasks: {<key>: bool}, earned, total }
//	POST /onboarding/tasks/complete   body: { "key": "<key>" }
//	                                  -> data: { alreadyCompleted, balance }
//
// ⚠️ The server validates NO preconditions. Measured 2026-09-26: posting
// `complete` for all eight keys answered
// `{"code":"000000","data":{"alreadyCompleted":false,"balance":N}}` and moved the
// balance 0 -> 10000 without any real conversation, PPT or skill install; the
// completion conditions are judged CLIENT-side (`loomy-onboarding.ts:4-14`,
// `README.md:1415-1421`). The request body carries only `key` — no device
// fingerprint, version or channel (`loomy-onboarding.ts:23-25`).
//
// ⚠️ This is a ONE-OFF capability, independent of the daily grant: it is not part
// of the daily traversal, because running it every day would fire 8 guaranteed
// `alreadyCompleted` requests (`README.md:1465-1469`).
//
// ⚠️ If upstream ever adds real precondition checks, the documented degrade path
// is to replay the actions with the cheapest model (`README.md:1420-1421`).

// onboardingTask is one task of the table, and the table order is the execution
// order (`loomy-onboarding.ts:46-55`).
type onboardingTask struct {
	Key    string
	Points int
	Title  string
}

// onboardingTasks is the ordered key table with its point values and titles
// (`loomy-onboarding.ts:46-67`).
var onboardingTasks = []onboardingTask{
	{Key: "first_message", Points: 500, Title: "发送你的第一条消息"},
	{Key: "pick_skill", Points: 1000, Title: "试试选择一个技能"},
	{Key: "generate_ppt", Points: 1500, Title: "生成第一份 PPT"},
	{Key: "set_schedule", Points: 1000, Title: "设置定时任务"},
	{Key: "install_skill", Points: 1500, Title: "在技能广场安装一个技能"},
	{Key: "configure_remote", Points: 1000, Title: "配置远程控制"},
	{Key: "create_soul", Points: 1500, Title: "创建你的第一个搭子"},
	{Key: "share_soul", Points: 2000, Title: "把搭子分享给朋友"},
}

// onboardingTotalPoints is `LOOMY_ONBOARDING_TOTAL` (`loomy-onboarding.ts:70`).
const onboardingTotalPoints = 10000

// onboardingTaskFor looks a key up in the table.
func onboardingTaskFor(key string) (onboardingTask, bool) {
	trimmed := strings.TrimSpace(key)
	for _, task := range onboardingTasks {
		if task.Key == trimmed {
			return task, true
		}
	}
	return onboardingTask{}, false
}

// onboardingEarned recomputes the earned total from the key table.
//
// The server's `earned` is NOT trusted (`loomy-onboarding.ts:27-28,94-97`).
func onboardingEarned(tasks map[string]bool) int {
	earned := 0
	for _, task := range onboardingTasks {
		if tasks[task.Key] {
			earned += task.Points
		}
	}
	return earned
}

// onboardingState is the read-only view of `GET /onboarding/tasks`.
type onboardingState struct {
	Tasks map[string]bool
	// Earned is recomputed locally, never the server value.
	Earned int
	// Total is the server value when it is usable, else the known 10000
	// (`loomy-onboarding.ts:174`).
	Total int
	// Balance is the reported balance when the server sent one.
	Balance *float64
}

// claim is one completed task.
type claim struct {
	Key    string
	Points int
}

// onboardingOutcome summarises a `claimAll` run (`loomy-onboarding.ts:209-237`).
type onboardingOutcome struct {
	Claimed []claim
	Skipped []string
	Earned  int
	Total   int
	Message string
}

// fetchOnboardingState reads the task state. It stays READ-ONLY: the panel calls
// it on mount, so it must never trigger a completion
// (`jet-hub-rpc.ts:1187-1191`).
func fetchOnboardingState(h *abiboot.Host, credential *Credential, cfg Config) (*onboardingState, error) {
	response, errDo := hostRequest(h, http.MethodGet, APIBase+OnboardingTasksPath, businessHeaders(credential, false), nil, cfg)
	if errDo != nil {
		return nil, transportError("onboarding_transport", "查询 Loomy 任务状态失败：%v", errDo)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, transportError("onboarding_status", "查询 Loomy 任务状态失败（HTTP %d）：%s",
			response.StatusCode, truncate(string(response.Body), 200))
	}
	parsed, errEnvelope := parseEnvelope(response.Body)
	if errEnvelope != nil {
		return nil, errEnvelope
	}
	if !parsed.success() {
		return nil, envelopeFailure("查询 Loomy 任务状态", parsed)
	}
	var data struct {
		Tasks   map[string]bool `json:"tasks"`
		Earned  *float64        `json:"earned"`
		Total   *float64        `json:"total"`
		Balance *float64        `json:"balance"`
	}
	if errDecode := parsed.decodeData(&data); errDecode != nil {
		return nil, errDecode
	}
	state := &onboardingState{Tasks: data.Tasks, Total: onboardingTotalPoints, Balance: data.Balance}
	if state.Tasks == nil {
		state.Tasks = map[string]bool{}
	}
	if data.Total != nil && !math.IsNaN(*data.Total) && !math.IsInf(*data.Total, 0) && *data.Total > 0 {
		state.Total = int(*data.Total)
	}
	// `earned` from the server is ignored on purpose.
	state.Earned = onboardingEarned(state.Tasks)
	return state, nil
}

// completeOnboardingTask posts one completion.
//
// The idempotency key is the response's `alreadyCompleted`, and a repeat counts
// as SUCCESS (`loomy-onboarding.ts:26,181`). The key is validated locally before
// any request is sent (`loomy-onboarding.ts:189-192`).
func completeOnboardingTask(h *abiboot.Host, credential *Credential, key string, cfg Config) (bool, error) {
	task, known := onboardingTaskFor(key)
	if !known {
		return false, statusError(false, "unknown_task", http.StatusBadRequest, "未知的 Loomy 任务 key：%q", key)
	}
	body, errMarshal := json.Marshal(map[string]string{"key": task.Key})
	if errMarshal != nil {
		return false, statusError(false, "encode_task", http.StatusInternalServerError, "encode task body: %v", errMarshal)
	}
	response, errDo := hostRequest(h, http.MethodPost, APIBase+OnboardingCompletePath, businessHeaders(credential, true), body, cfg)
	if errDo != nil {
		return false, transportError("onboarding_complete_transport", "完成任务 %s 失败：%v", task.Key, errDo)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return false, transportError("onboarding_complete_status", "完成任务 %s 失败（HTTP %d）：%s",
			task.Key, response.StatusCode, truncate(string(response.Body), 200))
	}
	parsed, errEnvelope := parseEnvelope(response.Body)
	if errEnvelope != nil {
		return false, errEnvelope
	}
	if !parsed.success() {
		// `100002` here is terminal and aborts the whole run in claimAll; the
		// classification travels through the error code.
		return false, envelopeFailure("完成任务 "+task.Key, parsed)
	}
	var data struct {
		AlreadyCompleted bool `json:"alreadyCompleted"`
	}
	if errDecode := parsed.decodeData(&data); errDecode != nil {
		return false, errDecode
	}
	return data.AlreadyCompleted, nil
}

// claimAllOnboarding walks the key table in order (`loomy-onboarding.ts:209-237`).
//
// Keys already true are SKIPPED without sending a request; the rest are posted
// serially. A server `alreadyCompleted` replay still counts as claimed. A
// `100002` aborts immediately so no further doomed request is sent
// (`loomy-onboarding.ts:145-152,205-208`).
func claimAllOnboarding(h *abiboot.Host, credential *Credential, cfg Config) (onboardingOutcome, error) {
	state, errState := fetchOnboardingState(h, credential, cfg)
	if errState != nil {
		return onboardingOutcome{}, errState
	}
	outcome := onboardingOutcome{Earned: state.Earned, Total: state.Total}
	for _, task := range onboardingTasks {
		if state.Tasks[task.Key] {
			outcome.Skipped = append(outcome.Skipped, task.Key)
			continue
		}
		if _, errComplete := completeOnboardingTask(h, credential, task.Key, cfg); errComplete != nil {
			// A dead session (`100002`) is terminal, so the run stops here
			// instead of firing seven more guaranteed failures; any other failure
			// stops it too, because the state it would build on is unknown.
			return outcome, errComplete
		}
		// An `alreadyCompleted` replay counts as claimed.
		outcome.Claimed = append(outcome.Claimed, claim{Key: task.Key, Points: task.Points})
		outcome.Earned += task.Points
	}
	switch {
	case len(outcome.Claimed) == 0:
		outcome.Message = "所有一次性任务都已完成"
	default:
		outcome.Message = fmt.Sprintf("已完成 %d 个任务，共 %d 积分", len(outcome.Claimed), sumClaimPoints(outcome.Claimed))
	}
	return outcome, nil
}

// sumClaimPoints totals the points of the claimed tasks.
func sumClaimPoints(claims []claim) int {
	total := 0
	for _, item := range claims {
		total += item.Points
	}
	return total
}
