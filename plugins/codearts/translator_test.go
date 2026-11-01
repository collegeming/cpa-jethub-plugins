package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
)

// The CodeArts gateway reports spent credits as HTTP 400 with error_code
// TM.00001050. CPA treats a bare 400 as a caller request fault and returns it
// to the client without trying another credential, so this classifier pins the
// contract that re-reports it as 402, the status the host reserves for an
// account that can no longer serve requests: the credential cools and the
// scheduler rotates.

const creditExhaustedBody = `{"error_code":"TM.00001050","error_msg":"您的积分已耗尽，请关注活动积分领取活动或者[升级套餐](https://example.com/upgrade)"}`

func TestClassifyUpstreamFailureCreditExhausted(t *testing.T) {
	err := classifyUpstreamFailure(http.StatusBadRequest, []byte(creditExhaustedBody))
	envErr, ok := err.(*abiboot.EnvelopeError)
	if !ok {
		t.Fatalf("classifyUpstreamFailure() = %T %v, want *abiboot.EnvelopeError", err, err)
	}
	if envErr.HTTPStatus != http.StatusPaymentRequired {
		t.Errorf("HTTPStatus = %d, want 402", envErr.HTTPStatus)
	}
	if envErr.Code != "quota_exceeded" {
		t.Errorf("Code = %q, want quota_exceeded", envErr.Code)
	}
	if !strings.Contains(envErr.Message, "积分已耗尽") {
		t.Errorf("Message = %q, want the upstream error_msg", envErr.Message)
	}
}

func TestInspectStreamPayloadCreditExhausted(t *testing.T) {
	err := inspectStreamPayload(creditExhaustedBody)
	envErr, ok := err.(*abiboot.EnvelopeError)
	if !ok {
		t.Fatalf("inspectStreamPayload() = %T %v, want *abiboot.EnvelopeError", err, err)
	}
	if envErr.HTTPStatus != http.StatusPaymentRequired || envErr.Code != "quota_exceeded" {
		t.Errorf("status/code = %d/%q, want 402/quota_exceeded", envErr.HTTPStatus, envErr.Code)
	}
}

// InferHub rejects the OpenAI string form of `stop` — every one of the 239
// string-stop GLM-5.2 requests captured on 2026-11-01 (across ~10 different
// credentials, including after an OAuth refresh) failed inside the stream with
// InferHub.001001005.400 "request param is invalid", while the array form and
// absent stop were admitted. `openai.Request.Stop` is `any`, so the client's
// exact spelling would reach the wire; prepareRequestBody wraps a scalar into
// a one-element array, the form the gateway accepts.

func TestPrepareRequestBodyWrapsStringStopIntoArray(t *testing.T) {
	body := decodeWireBody(t, `{"model":"GLM-5.2","messages":[{"role":"user","content":"hi"}],`+
		`"stop":"</assistant>"}`)
	stop, ok := body["stop"].([]any)
	if !ok {
		t.Fatalf("stop = %#v, want a JSON array, not the string form InferHub rejects", body["stop"])
	}
	if len(stop) != 1 || stop[0] != "</assistant>" {
		t.Fatalf("stop = %#v, want [\"</assistant>\"]", stop)
	}
}

func TestPrepareRequestBodyKeepsArrayStop(t *testing.T) {
	body := decodeWireBody(t, `{"model":"GLM-5.2","messages":[{"role":"user","content":"hi"}],`+
		`"stop":["a","b"]}`)
	stop, ok := body["stop"].([]any)
	if !ok || len(stop) != 2 || stop[0] != "a" || stop[1] != "b" {
		t.Fatalf("stop = %#v, want the caller's [\"a\",\"b\"] untouched", body["stop"])
	}
}

func TestPrepareRequestBodyDropsEmptyAndAbsentStop(t *testing.T) {
	for _, payload := range []string{
		`{"model":"GLM-5.2","messages":[{"role":"user","content":"hi"}]}`,
		// An empty string stop is a no-op; sending "" or [""] would only give
		// upstream another shape to reject.
		`{"model":"GLM-5.2","messages":[{"role":"user","content":"hi"}],"stop":""}`,
	} {
		body := decodeWireBody(t, payload)
		if stop, present := body["stop"]; present {
			t.Fatalf("stop = %#v, want the key absent for payload %s", stop, payload)
		}
	}
}

func TestClassifyUpstreamFailureOtherStatusesUnchanged(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   func(err error) bool
	}{
		{
			name:   "queue error still retries",
			status: http.StatusTooManyRequests,
			body:   `{"error_code":"TM.00001041","error_msg":"queue full"}`,
			want:   func(err error) bool { return errors.Is(err, ErrQueueFull) },
		},
		{
			name:   "credential rejection still sentinel",
			status: http.StatusUnauthorized,
			body:   `{"error_code":"APIG.0301","error_msg":"bad token"}`,
			want:   func(err error) bool { return errors.Is(err, ErrCredentialRejected) },
		},
		{
			name:   "ordinary 400 still generic upstream error",
			status: http.StatusBadRequest,
			body:   `{"error_code":"TM.00009999","error_msg":"bad request"}`,
			want: func(err error) bool {
				envErr, ok := err.(*abiboot.EnvelopeError)
				return ok && envErr.HTTPStatus == http.StatusBadRequest && envErr.Code == "upstream_error"
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := classifyUpstreamFailure(tc.status, []byte(tc.body)); !tc.want(err) {
				t.Errorf("classifyUpstreamFailure(%d) = %v", tc.status, err)
			}
		})
	}
}
