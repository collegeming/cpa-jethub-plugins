package reg

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	stdjar "net/http/cookiejar"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The registrar lives inside the codearts plugin as a sub-package: the merged
// plugin owns the one ABI bootstrap and delegates its /reg and /register
// management routes here. This file keeps the settings slot and the direct
// HTTP transport the registration chain drives.

var settingsSlot atomic.Value // holds Config

func init() {
	settingsSlot.Store(DefaultConfig())
}

// settings returns the live registrar configuration.
func settings() Config {
	if value, ok := settingsSlot.Load().(Config); ok {
		return value
	}
	return DefaultConfig()
}

func setSettings(cfg Config) { settingsSlot.Store(cfg) }

// SetConfig stores the decoded registrar settings. The merged plugin calls it
// from its own Configure on every reload.
func SetConfig(cfg Config) { setSettings(cfg) }

// doer performs one buffered call through the transport. Declared as a
// function type so the whole chain — SMS pool, registration, login, OAuth —
// can be exercised offline with a scripted transport.
type doer func(method, rawURL string, headers http.Header, body []byte) (*pluginapi.HTTPResponse, error)

// sessionTransport bundles the request doer with a cookie lookup — the
// CodeArts portal API wants a `cftk` header mirroring the session cookie of
// the same name, which only the transport's jar can see.
type sessionTransport struct {
	do     doer
	cookie func(rawURL, name string) string
}

// transportFor resolves the transport for one job run. Production resolves to
// directTransport — a dedicated net/http client — because the CAS/OAuth chain
// needs per-hop redirect and cookie visibility that host.http.do cannot
// provide: the host client follows redirects internally (swallowing
// intermediate Set-Cookie headers like SSOTGC) and strips Cookie as a
// credential header on cross-origin hops.
var transportFor = func(*abiboot.Host) sessionTransport { return directTransport() }

// directTransport builds a sessionTransport on its own *http.Client: redirects
// are never followed (the caller walks Location/JS hops itself) and the
// standard cookiejar provides RFC domain/path scoping across the whole run.
func directTransport() sessionTransport {
	jar, _ := stdjar.New(nil)
	client := &http.Client{
		Jar:     jar,
		Timeout: 30 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return sessionTransport{
		do: func(method, rawURL string, headers http.Header, body []byte) (*pluginapi.HTTPResponse, error) {
			request, errNew := http.NewRequest(method, rawURL, bytes.NewReader(body))
			if errNew != nil {
				return nil, fmt.Errorf("build request %s: %v", rawURL, errNew)
			}
			if headers == nil {
				headers = http.Header{}
			}
			request.Header = headers.Clone()
			response, errDo := client.Do(request)
			if errDo != nil {
				return nil, fmt.Errorf("%s %s: %v", method, hostPath(rawURL), errDo)
			}
			defer func() { _ = response.Body.Close() }()
			payload, errRead := io.ReadAll(response.Body)
			if errRead != nil {
				return nil, fmt.Errorf("read %s response: %v", hostPath(rawURL), errRead)
			}
			return &pluginapi.HTTPResponse{
				StatusCode: response.StatusCode,
				Headers:    response.Header.Clone(),
				Body:       payload,
			}, nil
		},
		cookie: func(rawURL, name string) string {
			if jar == nil {
				return ""
			}
			parsed, errParse := url.Parse(rawURL)
			if errParse != nil {
				return ""
			}
			// Several *cftk cookies stack on huaweicloud.com (account_cftk,
			// devcloud…tcftk, …). The portal's own CSRF token ends in "tcftk";
			// prefer it, then fall back to any match.
			fallback := ""
			for _, c := range jar.Cookies(parsed) {
				if strings.Contains(c.Name, name) {
					if strings.HasSuffix(c.Name, "tcftk") {
						return c.Value
					}
					if fallback == "" {
						fallback = c.Value
					}
				}
			}
			return fallback
		},
	}
}
