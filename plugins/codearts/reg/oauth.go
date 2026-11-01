package reg

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// credentialFile is the on-disk auth document the codearts plugin parses.
// Field names match the provider's Credential struct so host.auth.save can
// register the file without the provider ever seeing this plugin.
type credentialFile struct {
	Type              string         `json:"type"`
	AccessKeyID       string         `json:"access_key_id"`
	SecretAccessKey   string         `json:"secret_access_key"`
	SecurityToken     string         `json:"security_token"`
	ExpiresAt         string         `json:"expires_at"`
	DomainID          string         `json:"domain_id,omitempty"`
	UserID            string         `json:"user_id,omitempty"`
	UserName          string         `json:"user_name,omitempty"`
	RefreshToken      string         `json:"refresh_token,omitempty"`
	CodeVerifier      string         `json:"code_verifier,omitempty"`
	DpopPrivateKeyJWK *dpopStoredJWK `json:"dpop_private_key_jwk,omitempty"`
	// Password is the console login password generated at registration —
	// required to re-authenticate the account if its refresh token dies.
	Password string `json:"password,omitempty"`
	// InviteCode is this account's own INVITE_USER referral code, minted via
	// ops/invite/generate. Later registrations draw from it randomly so the
	// pool chains referral credit back onto itself.
	InviteCode string `json:"invite_code,omitempty"`
}

// dpopStoredJWK mirrors the codearts plugin's persisted EC P-256 JWK shape.
type dpopStoredJWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	D   string `json:"d,omitempty"`
}

// pkcePair holds a PKCE verifier and its SHA-256 challenge.
type pkcePair struct {
	Verifier  string
	Challenge string
}

func newPKCE() (*pkcePair, error) {
	buf := make([]byte, 48)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("generate PKCE verifier: %v", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	return &pkcePair{Verifier: verifier, Challenge: base64.RawURLEncoding.EncodeToString(sum[:])}, nil
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// dpopKeyPair is an ephemeral ES256 (P-256) key used to build DPoP proofs.
type dpopKeyPair struct {
	private *ecdsa.PrivateKey
}

func newDpopKeyPair() (*dpopKeyPair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate DPoP key: %v", err)
	}
	return &dpopKeyPair{private: key}, nil
}

const dpopCoordSize = 32

func (k *dpopKeyPair) publicJWK() dpopStoredJWK {
	return dpopStoredJWK{
		Kty: "EC",
		Crv: "P-256",
		X:   base64.RawURLEncoding.EncodeToString(k.private.PublicKey.X.FillBytes(make([]byte, dpopCoordSize))),
		Y:   base64.RawURLEncoding.EncodeToString(k.private.PublicKey.Y.FillBytes(make([]byte, dpopCoordSize))),
	}
}

func (k *dpopKeyPair) privateJWK() dpopStoredJWK {
	jwk := k.publicJWK()
	jwk.D = base64.RawURLEncoding.EncodeToString(k.private.D.FillBytes(make([]byte, dpopCoordSize)))
	return jwk
}

// signDPoP builds the ES256 DPoP proof JWS for one request.
func (k *dpopKeyPair) signDPoP(method, htu string) (string, error) {
	jti, errJTI := randomHex(32)
	if errJTI != nil {
		return "", errJTI
	}
	public := k.publicJWK()
	protectedJSON, errProtected := json.Marshal(map[string]any{
		"alg": "ES256",
		"typ": "dpop+jwt",
		"jwk": map[string]string{"kty": public.Kty, "crv": public.Crv, "x": public.X, "y": public.Y},
	})
	if errProtected != nil {
		return "", fmt.Errorf("encode DPoP header: %v", errProtected)
	}
	payloadJSON, errPayload := json.Marshal(map[string]any{
		"htm": strings.ToUpper(method),
		"htu": htu,
		"iat": time.Now().Unix(),
		"jti": jti,
	})
	if errPayload != nil {
		return "", fmt.Errorf("encode DPoP payload: %v", errPayload)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(protectedJSON) + "." + base64.RawURLEncoding.EncodeToString(payloadJSON)
	digest := sha256.Sum256([]byte(signingInput))
	r, s, errSign := ecdsa.Sign(rand.Reader, k.private, digest[:])
	if errSign != nil {
		return "", fmt.Errorf("sign DPoP proof: %v", errSign)
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:dpopCoordSize])
	s.FillBytes(signature[dpopCoordSize:])
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// redirectURI is what the authorize endpoint hands the code to; it is
// intercepted, never connected to. The port only matters to upstream.
func redirectURI(port int) string {
	return "http://127.0.0.1:" + strconv.Itoa(port) + OAuthRedirectPath
}

// buildOAuthAuthorizeURL assembles the real grant endpoint. Verified live:
// the portal/authorize page is only a consent shell — the actual OAuth grant
// runs at authui/v1/oauth2/authorize on the CAS domain, which needs nothing
// but the SSO session and a PKCE challenge.
func buildOAuthAuthorizeURL(pkce *pkcePair, state string, port int) string {
	query := url.Values{
		"client_id":             {OAuthClientID},
		"code_challenge_method": {"SHA-256"},
		"code_challenge":        {pkce.Challenge},
		"state":                 {state},
		"redirect_uri":          {redirectURI(port)},
		"response_type":         {"code"},
		"scope":                 {"openid"},
	}
	return OAuthAuthorizeURL + "?" + query.Encode()
}

// buildThirdLoginURL assembles the register-ticket redemption URL. The SPA's
// useAutoLogin.js redeems account_info.ticket here with idp=CHNIDP; the 302
// chain that follows plants the auth-domain SSO session.
func buildThirdLoginURL(ticket string) string {
	query := url.Values{
		"idp":     {"CHNIDP"},
		"service": {RedeemServiceURL},
		"ticket":  {ticket},
	}
	return ThirdLoginURL + "?" + query.Encode()
}

// buildCasLoginURL assembles the quick_login ticket redemption URL. The SPA's
// SelectAccountContent calls handleCasLogin after getTicket("quick_login").
func buildCasLoginURL(ticket string) string {
	query := url.Values{
		"ticket":  {ticket},
		"service": {RedeemServiceURL},
	}
	return CasLoginURL + "?" + query.Encode()
}

// callbackCodePattern finds `code=` on the loopback callback Location.
var callbackCodePattern = regexp.MustCompile(`[?&]code=([^&\s]+)`)
var callbackStatePattern = regexp.MustCompile(`[?&]state=([^&\s]+)`)

// followChain performs GET hops until the chain lands on the loopback
// callback (returns the authorization code and the callback URL it came on),
// runs out of redirects, or an HTTP/body error forces a stop. JS-based
// navigation inside returned pages is followed too: the portal and CAS both
// use script shells. Cookies ride inside the doer's own jar.
func followChain(d doer, start string, log func(string)) (code string, callback string, err error) {
	current := start
	for hop := 0; hop < 12; hop++ {
		response, errDo := d(http.MethodGet, current, http.Header{
			"User-Agent": {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"},
			"Accept":     {"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"},
		}, nil)
		if errDo != nil {
			return "", current, fmt.Errorf("hop %d: %v", hop, errDo)
		}
		location := firstHeader(response, "Location")
		log(fmt.Sprintf("授权链第 %d 跳: HTTP %d %s -> %s", hop, response.StatusCode,
			truncate(hostPath(current), 90), truncate(location, 90)))
		if location != "" {
			if match := callbackCodePattern.FindStringSubmatch(location); match != nil && strings.HasPrefix(location, "http://127.0.0.1") {
				return match[1], location, nil
			}
			next, errResolve := url.Parse(location)
			if errResolve != nil {
				return "", current, fmt.Errorf("hop %d: bad Location %q", hop, truncate(location, 160))
			}
			base, _ := url.Parse(current)
			current = base.ResolveReference(next).String()
			continue
		}
		body := string(response.Body)
		if match := callbackCodePattern.FindStringSubmatch(body); match != nil && strings.Contains(body, "127.0.0.1") {
			return match[1], body, nil
		}
		if target := jsNavigationTarget(body, current); target != "" {
			next, errResolve := url.Parse(target)
			if errResolve == nil {
				base, _ := url.Parse(current)
				current = base.ResolveReference(next).String()
				continue
			}
		}
		return "", current, fmt.Errorf("chain ended at %s (HTTP %d): %s", current, response.StatusCode, truncate(body, 200))
	}
	return "", current, fmt.Errorf("redirect chain exceeded hop limit")
}

// firstHeader returns the first value of a response header case-insensitively.
func firstHeader(response *pluginapi.HTTPResponse, name string) string {
	for key, values := range response.Headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

// jsNavigationTarget extracts the navigation target from the script shells
// Huawei uses between hops. Two shapes exist: a quoted literal
// (window.location.replace('X'), location.href='X', meta refresh url=X), and
// the CAS gate which builds the login URL dynamically as
// 'authui/login?service=' + encodeURIComponent(href) — the latter is resolved
// against the page's own URL.
var jsNavPatterns = []*regexp.Regexp{
	regexp.MustCompile(`location\.replace\s*\(\s*'([^']+)'`),
	regexp.MustCompile(`location\.replace\s*\(\s*"([^"]+)"`),
	regexp.MustCompile(`window\.location\.href\s*=\s*'([^']+)'`),
	regexp.MustCompile(`window\.location\.href\s*=\s*"([^"]+)"`),
	regexp.MustCompile(`location\.href\s*=\s*'([^']+)'`),
	regexp.MustCompile(`url='([^']+)'`),
	regexp.MustCompile(`url=([^"'\s>]+)"?`),
}

// casGatePattern marks the script shell that bounces an unauthenticated hit
// into the CAS login page with the current URL as the service.
var casGatePattern = regexp.MustCompile(`authui/login\?service=['"]?\s*\+\s*encodeURIComponent\s*\(\s*href\s*\)`)

func jsNavigationTarget(body, current string) string {
	if casGatePattern.MatchString(body) {
		return AuthUIBase + "/authui/login?service=" + url.QueryEscape(current)
	}
	for _, pattern := range jsNavPatterns {
		if match := pattern.FindStringSubmatch(body); match != nil {
			target := strings.TrimSpace(match[1])
			if strings.HasPrefix(target, "http") || strings.HasPrefix(target, "/") {
				return target
			}
		}
	}
	return ""
}

// rideRedirects follows GET hops purely to carry the session forward — each
// 3xx Location or JS-shell navigation is followed and the doer's jar absorbs
// its cookies; the ride ends quietly on the first non-navigating response.
// Used for ticket redemption, where the only thing that matters is the SSO
// cookie the hops plant.
func rideRedirects(d doer, start string, log func(string)) {
	current := start
	for hop := 0; hop < 10; hop++ {
		response, errDo := d(http.MethodGet, current, http.Header{
			"User-Agent": {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"},
			"Accept":     {"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"},
		}, nil)
		if errDo != nil {
			log(fmt.Sprintf("会话接力第 %d 跳请求失败: %v", hop, errDo))
			return
		}
		location := firstHeader(response, "Location")
		log(fmt.Sprintf("会话接力第 %d 跳: HTTP %d %s -> %s", hop, response.StatusCode,
			truncate(hostPath(current), 90), truncate(location, 90)))
		if location == "" {
			if target := jsNavigationTarget(string(response.Body), current); target != "" {
				next, errResolve := url.Parse(target)
				if errResolve == nil {
					base, _ := url.Parse(current)
					current = base.ResolveReference(next).String()
					continue
				}
			}
			return
		}
		next, errResolve := url.Parse(location)
		if errResolve != nil {
			return
		}
		base, _ := url.Parse(current)
		current = base.ResolveReference(next).String()
	}
}

// hostPath strips a URL to host+path for compact log lines.
func hostPath(rawURL string) string {
	parsed, errParse := url.Parse(rawURL)
	if errParse != nil {
		return rawURL
	}
	return parsed.Host + parsed.EscapedPath()
}

// redeemTicket trades an auto-login ticket for the auth-domain SSO session.
// Register tickets redeem at thirdLogin (idp=CHNIDP); quick_login tickets from
// the shared registerui path redeem at casLogin — matching useAutoLogin.js.
func redeemTicket(d doer, ticket string, viaCAS bool, log func(string)) {
	if ticket == "" {
		log("响应未返回 ticket，跳过后续会话建立")
		return
	}
	if viaCAS {
		log("兑换 quick_login ticket（casLogin）建立 SSO 会话")
		rideRedirects(d, buildCasLoginURL(ticket), log)
		return
	}
	log("兑换注册 ticket（thirdLogin）建立 SSO 会话")
	rideRedirects(d, buildThirdLoginURL(ticket), log)
}

// obtainCredential runs the post-register half: redeem the register ticket
// for an SSO session, activate the free tier, run the referral chain, hit the
// OAuth2 authorize endpoint for the code, then exchange at STS. No second SMS
// is needed — the registration ticket is the session credential.
func obtainCredential(t sessionTransport, info *accountInfo, host *abiboot.Host, cfg Config, log func(string)) (*credentialFile, error) {
	pkce, errPKCE := newPKCE()
	if errPKCE != nil {
		return nil, errPKCE
	}
	keyPair, errKey := newDpopKeyPair()
	if errKey != nil {
		return nil, errKey
	}
	state, errState := randomHex(18)
	if errState != nil {
		return nil, errState
	}
	port := 18080

	redeemTicket(t.do, info.Ticket, info.LoginViaCAS, log)
	plantPortalSession(t.do, pkce.Challenge, port, log)

	// Fresh accounts must claim the free trial once or the credit APIs and
	// the model catalogue stay behind TM.00001005. A failed activation still
	// leaves a valid credential, so it degrades to a warning.
	if errActivate := activatePackage(t, log); errActivate != nil {
		log("套餐激活未完成（账号稍后可用巡检重试）: " + errActivate.Error())
	}

	// Referral chain: bind a pool member's code (they earn the INVITE_USER
	// credit), then mint this account's own code for the next run.
	myInviteCode := runInviteChain(t, host, cfg, log)

	code, lastURL, errRide := followChain(t.do, buildOAuthAuthorizeURL(pkce, state, port), log)
	if errRide != nil {
		return nil, fmt.Errorf("oauth 授权未取到 code: %v", errRide)
	}
	if match := callbackStatePattern.FindStringSubmatch(lastURL); match != nil && match[1] != state {
		return nil, fmt.Errorf("oauth 授权 state 回显不一致")
	}
	log("拿到授权 code，换取 STS 凭证")

	credential, errExchange := exchangeCode(t.do, code, pkce, redirectURI(port), keyPair)
	if errExchange != nil {
		return nil, errExchange
	}
	credential.DomainID = info.DomainID
	credential.UserID = info.UserID
	credential.UserName = info.DomainName
	credential.Password = info.Password
	credential.InviteCode = myInviteCode
	return credential, nil
}

// tokenResponse is the STS OAuth2 response body.
type tokenResponse struct {
	Credentials *struct {
		AccessKeyID     string `json:"access_key_id"`
		SecretAccessKey string `json:"secret_access_key"`
		SecurityToken   string `json:"security_token"`
		Expiration      string `json:"expiration"`
	} `json:"credentials"`
	RefreshToken string `json:"refresh_token"`
	Error        string `json:"error"`
	ErrorCode    string `json:"error_code"`
	ErrorMsg     string `json:"error_msg"`
}

// exchangeCode trades the authorization code for tokens at STS.
func exchangeCode(d doer, code string, pkce *pkcePair, redirect string, keyPair *dpopKeyPair) (*credentialFile, error) {
	proof, errProof := keyPair.signDPoP(http.MethodPost, STSTokenEndpoint)
	if errProof != nil {
		return nil, errProof
	}
	form := url.Values{
		"client_id":     {OAuthClientID},
		"code":          {code},
		"code_verifier": {pkce.Verifier},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {redirect},
	}
	response, errDo := d(http.MethodPost, STSTokenEndpoint, http.Header{
		"DPoP":         {proof},
		"Content-Type": {"application/x-www-form-urlencoded"},
	}, []byte(form.Encode()))
	if errDo != nil {
		return nil, fmt.Errorf("STS 请求失败: %v", errDo)
	}
	var token tokenResponse
	if len(response.Body) > 0 {
		if errDecode := json.Unmarshal(response.Body, &token); errDecode != nil {
			return nil, fmt.Errorf("STS 响应解析失败 (HTTP %d): %v", response.StatusCode, errDecode)
		}
	}
	if token.Error != "" || token.ErrorCode != "" {
		return nil, fmt.Errorf("STS 拒绝: %s %s %s", token.Error, token.ErrorCode, truncate(token.ErrorMsg, 160))
	}
	if token.Credentials == nil || token.Credentials.AccessKeyID == "" {
		return nil, fmt.Errorf("STS 未返回凭证 (HTTP %d): %s", response.StatusCode, truncate(string(response.Body), 200))
	}
	jwk := keyPair.privateJWK()
	return &credentialFile{
		Type:              "codearts",
		AccessKeyID:       token.Credentials.AccessKeyID,
		SecretAccessKey:   token.Credentials.SecretAccessKey,
		SecurityToken:     token.Credentials.SecurityToken,
		ExpiresAt:         token.Credentials.Expiration,
		RefreshToken:      token.RefreshToken,
		CodeVerifier:      pkce.Verifier,
		DpopPrivateKeyJWK: &jwk,
	}, nil
}
