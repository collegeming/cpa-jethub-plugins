package abiboot

import (
	"encoding/json"
	"sync"
)

// The cgo layer installed by each plugin's main package registers a single
// function used to reach the host. Everything in the SDK funnels through it.
var (
	hostCallerMu sync.RWMutex
	hostCaller   func(method string, request []byte) ([]byte, error)
)

// SetHostCaller installs the transport used for host callbacks. It is called
// exactly once from cliproxy_plugin_init.
func SetHostCaller(fn func(method string, request []byte) ([]byte, error)) {
	hostCallerMu.Lock()
	defer hostCallerMu.Unlock()
	hostCaller = fn
}

// ClearHostCaller drops the transport during plugin shutdown.
func ClearHostCaller() {
	hostCallerMu.Lock()
	defer hostCallerMu.Unlock()
	hostCaller = nil
}

// hostCallRaw performs one host callback and unwraps the response envelope.
func hostCallRaw(method string, request []byte) (json.RawMessage, error) {
	hostCallerMu.RLock()
	caller := hostCaller
	hostCallerMu.RUnlock()

	if caller == nil {
		return nil, Errorf("host_unavailable", "host callback transport is not initialised")
	}
	raw, err := caller(method, request)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, Errorf("host_empty_response", "host callback %s returned no response", method)
	}
	env, errDecode := decodeEnvelope(raw)
	if errDecode != nil {
		return nil, errDecode
	}
	if !env.OK {
		if env.Error != nil {
			return nil, env.Error
		}
		return nil, Errorf("host_error", "host callback %s failed", method)
	}
	return env.Result, nil
}

// HostCall marshals payload, invokes a host method and returns the raw result.
// It is the generic escape hatch; prefer the typed Host methods.
func HostCall(method string, payload any) (json.RawMessage, error) {
	var request []byte
	if payload != nil {
		encoded, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return nil, Errorf("host_request_encode", "encode %s request: %v", method, errMarshal)
		}
		request = encoded
	}
	return hostCallRaw(method, request)
}

// HostCallInto marshals payload and decodes the host result into out.
func HostCallInto(method string, payload any, out any) error {
	raw, err := HostCall(method, payload)
	if err != nil {
		return err
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if errUnmarshal := json.Unmarshal(raw, out); errUnmarshal != nil {
		return Errorf("host_response_decode", "decode %s response: %v", method, errUnmarshal)
	}
	return nil
}
