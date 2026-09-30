package main

import (
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Qoder business-error parsing: quota exhaustion and model queueing.
//
// Ported from `src/model-queue.ts`. The encrypted endpoint reports a failure as
// a business `code` inside the frame body, and two of those codes must NOT be
// flattened into a generic upstream failure, because the host reacts to them
// differently:
//
//	110    `Billing daily count exceeded` — the day's quota is gone. Waiting
//	       cannot help, so the host must rotate the credential instead of
//	       retrying (upstream `2c1af59`: classified as `SERVER` it burned five
//	       backoffs, ~15.5s, on a deterministic failure).
//	10605  model queued — temporarily blocked. The server states how long to
//	       wait, and that delay is honoured (upstream `daf9fb1`).
//
// ⚠️ **The nesting depth of an error body must not be assumed.** The measured
// queue frame is
//
//	{"code":403,"message":"{\"code\":\"10605\",\"message\":\"{\\\"isQueued\\\":…}\"}"}
//
// i.e. the business code sits one JSON-STRING layer below the frame's own
// `code`, whose 403 is a transport status and not a business code at all.
// Reading only the top level therefore classifies nothing — upstream shipped
// that bug three times (`daf9fb1`: 「修了两次仍失败」). `parseBusinessError`
// walks every layer instead, so the reader never has to know the depth.

// Business codes (`model-queue.ts:36`, `:73`).
const (
	// QueueBusinessCode is the client's `mRA` → `model_queued`.
	QueueBusinessCode = "10605"
	// BillingBusinessCode is the quota code. It is NOT hardcoded in the client
	// bundle — the server sends it — which is why a text fallback exists
	// (`model-queue.ts:104-108`).
	BillingBusinessCode = "110"
	// QueueMaxDelayMS caps a single queue wait (`model-queue.ts:138`): below the
	// cap the server's own value is used, at or above it the wait is clamped, so
	// one request never blocks for the 30s the server may ask for.
	QueueMaxDelayMS = 10_000
	// businessMaxDepth bounds the unwrapping walk. Real bodies nest one or two
	// layers; the bound only exists so a self-referential body cannot spin.
	businessMaxDepth = 8
)

// billingMessagePattern is `looksLikeBillingError` (`model-queue.ts:128-130`)
// plus the Chinese spellings of the same condition.
//
// ⚠️ The patterns stay NARROW on purpose. `model-queue.ts:120-126` records why:
// generic words such as `balance` or a bare `quota` also occur in perfectly
// healthy text, so matching them would turn an ordinary upstream failure into a
// non-retryable 402. Every alternative below therefore names the exhaustion
// itself (`quota exceeded`, `余额不足`, …) rather than the noun alone.
var billingMessagePattern = regexp.MustCompile(
	`(?i)billing[\s_]*daily[\s_]*count[\s_]*exceeded|daily[\s_]*count[\s_]*exceeded|billing_error|` +
		`quota[\s_]*(?:exceeded|exhausted|exceed|used[\s_]*up)|insufficient[\s_]+(?:quota|balance|credits)|` +
		`out[\s_]+of[\s_]+credits|` +
		`余额不足|额度(?:已)?(?:耗尽|用完|不足)|次数(?:已)?(?:用完|耗尽|不足)`)

// looksLikeBillingError is `looksLikeBillingError` (`model-queue.ts:128-130`).
func looksLikeBillingError(text string) bool {
	return billingMessagePattern.MatchString(text)
}

// queueSleep waits out a server-supplied queue delay before the failure is
// surfaced. It is a variable so a test can observe the computed delay instead of
// sleeping it — no test in this package waits for a real queue.
var queueSleep = func(delay time.Duration) { time.Sleep(delay) }

// queueState mirrors `QueueInfo` (`model-queue.ts:144-155`), trimmed to the
// fields the delay computation and the marker test read. A nil pointer means the
// key was absent at EVERY layer, which is what separates "no queue" from
// "isQueued: false" — the latter is a real, momentary queue
// (`model-queue.ts:230-235`).
type queueState struct {
	IsQueued         *bool
	ServiceAvailable *bool
	// The three delay encodings, in the client's priority order (`kJa()` / `EV()`
	// / `IRA()`).
	RetryAfterMillis      *float64
	RetryAfterMillisCamel *float64
	RetryAfterSeconds     *float64
}

// marksQueue reports whether the state carries a queue marker.
func (q *queueState) marksQueue() bool {
	return q != nil && (q.IsQueued != nil || q.ServiceAvailable != nil)
}

// delayMS is `queueDelayMs` (`model-queue.ts:248-255`): `retry_after_ms` →
// `retryAfterMs` → `retryAfterSeconds × 1000`, then clamped to QueueMaxDelayMS.
//
// ⚠️ A negative value yields NO delay rather than zero. Upstream discards such a
// value outright (`W7c()`), because treating it as 0 turns the retry into a busy
// loop that burns every attempt instantly.
func (q *queueState) delayMS() (int, bool) {
	if q == nil {
		return 0, false
	}
	raw, ok := firstNumber(q.RetryAfterMillis, q.RetryAfterMillisCamel)
	if !ok {
		if seconds, hasSeconds := firstNumber(q.RetryAfterSeconds); hasSeconds {
			raw, ok = seconds*1000, true
		}
	}
	if !ok || raw < 0 {
		return 0, false
	}
	delay := int(raw)
	if delay > QueueMaxDelayMS {
		delay = QueueMaxDelayMS
	}
	return delay, true
}

// businessError is one error body after every nested layer has been unwrapped.
type businessError struct {
	// Raw is the body as received, used when no layer carries readable text.
	Raw string
	// Code is the business code of the deepest layer that carries one. It is the
	// one RENDERED: the outermost code is frequently just a transport status
	// (403) or a wrapper, while the business code sits deeper.
	Code string
	// Codes holds every layer's code, in outer-to-inner order. Classification
	// reads this set, not Code: upstream matches a business code at ANY depth
	// (`parseQueueError`'s `nodes.some(…)`, `model-queue.ts:225-227`), so
	// requiring the deepest one specifically would re-introduce a
	// depth-dependent miss.
	Codes []string
	// Message is the deepest human-readable text. A layer whose `message` is
	// itself JSON is kept only as a fallback rendering — the real text is one
	// layer further down.
	Message string
	// Type is the `type` field, e.g. `model_error`.
	Type string
	// Queue holds the queue markers and delay candidates found at any layer.
	Queue *queueState
	// Depth counts the layers that were successfully parsed (>= 1).
	Depth int
	// codeDepth / messageDepth track which layer the fields above came from.
	codeDepth    int
	messageDepth int
}

// text renders the `message (code/type)` suffix the sources produce
// (`openai-compat.ts:650-654`: `[String(data.code), data.type].join('/')`).
func (e businessError) text() string {
	message := e.Message
	if message == "" {
		message = e.Raw
	}
	detail := e.Code
	if e.Type != "" {
		if detail == "" {
			detail = e.Type
		} else {
			detail += "/" + e.Type
		}
	}
	if detail == "" {
		return message
	}
	return message + " (" + detail + ")"
}

// hasCode reports a business-code hit at ANY depth. Codes are normalised to their
// string form while parsing, so one comparison covers the number and the string
// spelling (`isBillingBusinessCode` / `isQueueBusinessCode`,
// `model-queue.ts:76-78`, `:163-165`).
func (e businessError) hasCode(want string) bool {
	return slices.Contains(e.Codes, want)
}

// businessKind is what a business-error body means for the host.
type businessKind int

const (
	// businessUnknown is every code this plugin has no specific handling for;
	// those keep the generic upstream classification.
	businessUnknown businessKind = iota
	// businessQuota is the day's quota being gone — deterministic.
	businessQuota
	// businessQueue is a temporary queue wait.
	businessQueue
)

// classifyBusinessError decides the kind of a parsed body.
//
// ⚠️ Queue is tested FIRST, exactly as `openai-compat.ts:628-648` does. The
// historical bug was the opposite mistake — testing the wrapper's code and
// concluding "not a queue" — which is why no test here is gated on the
// outermost code.
func classifyBusinessError(parsed businessError) businessKind {
	switch {
	case parsed.hasCode(QueueBusinessCode) || parsed.Queue.marksQueue():
		return businessQueue
	case parsed.hasCode(BillingBusinessCode) || looksLikeBillingError(parsed.text()):
		return businessQuota
	default:
		return businessUnknown
	}
}

// parseBusinessError unwraps an error body layer by layer.
//
// ok is false when the body is not a JSON object at all (the `[FAIL]node:…
// msg:…` text frame, for instance); the caller then keeps its existing generic
// handling.
func parseBusinessError(raw string) (businessError, bool) {
	parsed := businessError{Raw: strings.TrimSpace(raw), codeDepth: -1, messageDepth: -1}
	if parsed.Raw == "" || !walkBusiness(parsed.Raw, 0, &parsed) {
		return businessError{}, false
	}
	return parsed, true
}

// walkBusiness records one layer and descends into the keys upstream walks
// (`data` / `result` / `message` / `body`; strings are decoded as JSON —
// `model-queue.ts:207-219`), so a JSON-string-wrapped body is penetrated too.
func walkBusiness(raw string, depth int, out *businessError) bool {
	node := decodeJSONObject(raw)
	if node == nil {
		return false
	}
	if depth+1 > out.Depth {
		out.Depth = depth + 1
	}

	if code := businessCodeOf(node); code != "" {
		// Every layer's code is recorded; the deepest one is also the one shown.
		out.Codes = append(out.Codes, code)
		if depth > out.codeDepth {
			out.Code, out.codeDepth = code, depth
		}
	}
	if text, ok := node["message"].(string); ok && strings.TrimSpace(text) != "" && depth > out.messageDepth {
		// A `message` that is itself JSON is recorded as a rendering fallback:
		// it still describes the failure better than the layer above it, and a
		// deeper layer with real text replaces it (the depth comparison).
		out.Message, out.messageDepth = text, depth
	}
	if value, ok := node["type"].(string); ok && value != "" && out.Type == "" {
		out.Type = value
	}
	mergeQueueFields(out, node)

	if depth+1 >= businessMaxDepth {
		return true
	}
	for _, key := range []string{"data", "result", "message", "body"} {
		switch value := node[key].(type) {
		case map[string]any:
			if encoded, errMarshal := json.Marshal(value); errMarshal == nil {
				walkBusiness(string(encoded), depth+1, out)
			}
		case string:
			walkBusiness(value, depth+1, out)
		}
	}
	return true
}

// businessCodeOf normalises a layer's `code`, accepting both the number and the
// string spelling — `110` and `"110"` are the same business code.
func businessCodeOf(node map[string]any) string {
	value, present := node["code"]
	if !present {
		return ""
	}
	if number, ok := numberValue(value); ok {
		return strconv.FormatFloat(number, 'f', -1, 64)
	}
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}

// mergeQueueFields folds one layer's queue markers and delay candidates into the
// accumulated state. A field already seen is never overwritten: the outermost
// layer is the one that chose to publish it.
func mergeQueueFields(out *businessError, node map[string]any) {
	if out.Queue == nil {
		out.Queue = &queueState{}
	}
	queue := out.Queue
	if value, ok := node["isQueued"].(bool); ok && queue.IsQueued == nil {
		queue.IsQueued = &value
	}
	if value, ok := node["serviceAvailable"].(bool); ok && queue.ServiceAvailable == nil {
		queue.ServiceAvailable = &value
	}
	for _, field := range []struct {
		key    string
		target **float64
	}{
		{"retry_after_ms", &queue.RetryAfterMillis},
		{"retryAfterMs", &queue.RetryAfterMillisCamel},
		{"retryAfterSeconds", &queue.RetryAfterSeconds},
	} {
		if *field.target != nil {
			continue
		}
		if value, ok := numberValue(node[field.key]); ok {
			copied := value
			*field.target = &copied
		}
	}
}

// decodeJSONObject parses one JSON object layer; nil for anything else.
func decodeJSONObject(raw string) map[string]any {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed[0] != '{' {
		return nil
	}
	var node map[string]any
	if errUnmarshal := json.Unmarshal([]byte(trimmed), &node); errUnmarshal != nil {
		return nil
	}
	return node
}

// numberValue reads a JSON number, ignoring anything else. Booleans and strings
// are deliberately NOT coerced (upstream `RE()` accepts finite numbers only).
func numberValue(value any) (float64, bool) {
	number, ok := value.(float64)
	if !ok {
		return 0, false
	}
	return number, true
}

// firstNumber returns the first present value among the candidates.
func firstNumber(candidates ...*float64) (float64, bool) {
	for _, candidate := range candidates {
		if candidate != nil {
			return *candidate, true
		}
	}
	return 0, false
}
