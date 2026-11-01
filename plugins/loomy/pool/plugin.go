package pool

import (
	"net/http"
	"sync/atomic"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The pool manager lives inside the loomy plugin as a sub-package: the main
// plugin owns the one ABI bootstrap and delegates its /pool and /scan
// management routes here. This file keeps the settings slot and the host
// transport the scanner drives.

var settingsSlot atomic.Value // holds Config

func init() {
	settingsSlot.Store(DefaultConfig())
}

// settings returns the live pool configuration.
func settings() Config {
	if value, ok := settingsSlot.Load().(Config); ok {
		return value
	}
	return DefaultConfig()
}

func setSettings(cfg Config) { settingsSlot.Store(cfg) }

// ApplyConfig stores the decoded settings and (re)arms the background sweep.
// The main plugin calls it from its own Configure on every reload.
func ApplyConfig(cfg Config) {
	setSettings(cfg)
	stopScanner()
	startScanner(cfg)
}

// Shutdown stops the background sweep. The main plugin calls it from its own
// Quiesce/Shutdown lifecycle.
func Shutdown() { stopScanner() }

// doer performs one buffered call through the host transport. It is a function
// type for the same reason the codearts pool declares one: the whole
// reconciliation — status fetch, auth list, auth save — can then be exercised
// offline with a scripted transport.
type doer func(method, rawURL string, headers http.Header, body []byte) (*pluginapi.HTTPResponse, error)

// transportFor resolves the transport for one host invocation. Production
// always resolves to hostDo, which routes through `host.http.do`.
var transportFor = hostDo

// hostDo routes a request through the host transport.
func hostDo(h *abiboot.Host) doer {
	return func(method, rawURL string, headers http.Header, body []byte) (*pluginapi.HTTPResponse, error) {
		return h.HTTPDo(abiboot.HTTPDoRequest{
			Method:  method,
			URL:     rawURL,
			Headers: headers,
			Body:    body,
		})
	}
}
