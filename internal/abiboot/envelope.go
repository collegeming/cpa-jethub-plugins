// Package abiboot implements the CPA native plugin JSON-envelope contract.
//
// A CPA plugin is a native dynamic library that exports cliproxy_plugin_init
// together with a single call/free/shutdown function table. Every exchange
// between host and plugin is a JSON envelope:
//
//	{"ok":true,"result":{...}}
//	{"ok":false,"error":{"code":"...","message":"..."}}
//
// This package owns the Go side of that contract: envelope construction,
// capability registration, method dispatch and the typed host callbacks
// (host.http.do, host.http.* stream operations, host.auth.*, host.log).
// The cgo glue that exports the four C symbols lives in each plugin's main
// package because //export is only honoured in package main.
package abiboot

import (
	"encoding/json"
	"fmt"
)

// Envelope is the framing used for every host <-> plugin exchange.
type Envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *EnvelopeError  `json:"error,omitempty"`
}

// EnvelopeError mirrors pluginabi.Error. HTTPStatus is surfaced to the client
// when the failing method is an executor call.
type EnvelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

func (e *EnvelopeError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// Errorf builds a plain (non-HTTP) plugin error.
func Errorf(code, format string, args ...any) *EnvelopeError {
	return &EnvelopeError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// HTTPError builds an error that carries an HTTP status code through to the
// CPA client (used by executor methods).
func HTTPError(code string, status int, format string, args ...any) *EnvelopeError {
	return &EnvelopeError{Code: code, Message: fmt.Sprintf(format, args...), HTTPStatus: status}
}

// RetryableError marks a failure as retryable so the host may re-dispatch it.
func RetryableError(code string, format string, args ...any) *EnvelopeError {
	return &EnvelopeError{Code: code, Message: fmt.Sprintf(format, args...), Retryable: true}
}

// OK wraps a method result in a success envelope.
func OK(v any) ([]byte, error) {
	var raw json.RawMessage
	if v != nil {
		encoded, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("marshal plugin result: %w", err)
		}
		raw = encoded
	} else {
		raw = json.RawMessage("{}")
	}
	return json.Marshal(Envelope{OK: true, Result: raw})
}

// encodeError wraps an error in a failure envelope, preserving HTTP status and
// retryability when the error is an *EnvelopeError.
func encodeError(err error) []byte {
	envErr := &EnvelopeError{Code: "plugin_error", Message: err.Error()}
	if typed, ok := err.(*EnvelopeError); ok && typed != nil {
		envErr = typed
	}
	raw, marshalErr := json.Marshal(Envelope{OK: false, Error: envErr})
	if marshalErr != nil {
		// Last-resort literal; must never fail to produce a reply.
		return []byte(`{"ok":false,"error":{"code":"plugin_error","message":"plugin failed to encode error"}}`)
	}
	return raw
}

// rawEnvelope is used by the transport layer to inspect host replies.
func decodeEnvelope(raw []byte) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return Envelope{}, fmt.Errorf("decode envelope: %w", err)
	}
	return env, nil
}
