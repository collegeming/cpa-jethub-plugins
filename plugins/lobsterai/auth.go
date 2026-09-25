package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Credential is the JSON persisted as the CPA auth file for one LobsterAI
// account. The field names deliberately stay snake_case so auth material remains
// interchangeable with Jet-Hub's `LobsteraiCredential`
// (lobsterai.ts:62-114).
//
// The three identity fields uuid / first_keyfrom / latest_keyfrom are the
// biggest structural difference from the CodeBuddy credential: the refresh
// request is not "refreshToken only", it also carries the keyfrom payload, so
// losing them means the account can never be renewed again and the user must
// log in from scratch.
type Credential struct {
	Type string `json:"type,omitempty"`
	// AccessToken is the `Authorization: Bearer` value.
	AccessToken string `json:"access_token"`
	// RefreshToken may legitimately be empty; Refreshable() decides.
	RefreshToken string `json:"refresh_token"`
	// ExpiresAt is a millisecond timestamp kept as a string, matching the
	// BuddyCredential storage convention (seconds / ISO are also accepted on
	// read).
	ExpiresAt string `json:"expires_at,omitempty"`
	// UID is the account identity used for deduplication.
	UID string `json:"uid,omitempty"`
	// UserID is the Youdao yid / account userId sent in keyfrom payloads.
	UserID string `json:"user_id,omitempty"`
	// Nickname is display-only.
	Nickname string `json:"nickname,omitempty"`
	// UUID is the installation uuid generated at login and echoed on refresh.
	UUID string `json:"uuid,omitempty"`
	// FirstKeyfrom is the first-login timestamp in milliseconds.
	FirstKeyfrom string `json:"first_keyfrom,omitempty"`
	// LatestKeyfrom is the latest-activity timestamp. It deliberately stays at
	// its login-time value forever: the reference Go implementation never
	// updates it, so updating it here would diverge from the only
	// production-verified behaviour (lobsterai.ts:378-410).
	LatestKeyfrom string `json:"latest_keyfrom,omitempty"`
}

var (
	// unsafeFileNameChars keeps generated auth file names portable.
	unsafeFileNameChars = regexp.MustCompile(`[^A-Za-z0-9._@-]+`)
	// refreshLead is how long before expiry a credential is considered due.
	refreshLead = time.Hour
)

// ParseCredential decodes and validates an auth-file payload.
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, abiboot.HTTPError("invalid_credential", http.StatusUnauthorized, "empty LobsterAI credential")
	}
	var credential Credential
	if errUnmarshal := decodeJSON(raw, &credential); errUnmarshal != nil {
		return nil, abiboot.HTTPError("invalid_credential", http.StatusUnauthorized, "decode LobsterAI credential: %v", errUnmarshal)
	}
	if strings.TrimSpace(credential.AccessToken) == "" {
		return nil, abiboot.HTTPError("invalid_credential", http.StatusUnauthorized, "LobsterAI credential is missing access_token")
	}
	return &credential, nil
}

// Encode serialises the credential for storage in the CPA auth file.
func (c *Credential) Encode() (json.RawMessage, error) {
	if c.Type == "" {
		c.Type = ProviderKey
	}
	raw, errMarshal := json.Marshal(c)
	if errMarshal != nil {
		return nil, abiboot.Errorf("encode_credential", "encode LobsterAI credential: %v", errMarshal)
	}
	return json.RawMessage(raw), nil
}

// Refreshable reports whether the credential carries a silent-renewal token.
func (c *Credential) Refreshable() bool {
	return strings.TrimSpace(c.RefreshToken) != ""
}

// Expiry resolves the credential expiry, preferring expires_at and falling back
// to the access-token JWT `exp` claim (lobsterai.ts:195-206). ok=false means
// "unknown", which must not be treated as expired.
func (c *Credential) Expiry() (time.Time, bool) {
	raw := strings.TrimSpace(c.ExpiresAt)
	if raw != "" {
		if isDigits(raw) {
			value, errParse := strconv.ParseInt(raw, 10, 64)
			if errParse == nil {
				if value > 1_000_000_000_000 {
					return time.UnixMilli(value), true
				}
				return time.Unix(value, 0), true
			}
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05Z", "2006-01-02 15:04:05"} {
			if parsed, errParse := time.Parse(layout, raw); errParse == nil {
				return parsed, true
			}
		}
	}
	if expiresAt, ok := jwtExpiresAt(c.AccessToken); ok {
		return expiresAt, true
	}
	return time.Time{}, false
}

// Expired reports whether the credential is past (or within skew of) expiry.
// An unparseable expiry is not expired — matching the Rust and Go references
// (lobsterai.ts:208-212).
func (c *Credential) Expired(skew time.Duration) bool {
	expiresAt, ok := c.Expiry()
	if !ok {
		return false
	}
	return time.Now().Add(skew).After(expiresAt)
}

// jwtExpiresAt decodes the `exp` claim of a JWT without verifying it. The
// access token is an HS512 JWT whose exp is the authoritative expiry
// (lobsterai.ts:191-193).
func jwtExpiresAt(token string) (time.Time, bool) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, errDecode := base64.RawURLEncoding.DecodeString(parts[1])
	if errDecode != nil {
		// Some encoders emit padded base64url; accept that too.
		payload, errDecode = base64.URLEncoding.DecodeString(parts[1])
		if errDecode != nil {
			return time.Time{}, false
		}
	}
	var claims map[string]any
	if errUnmarshal := decodeJSON(payload, &claims); errUnmarshal != nil {
		return time.Time{}, false
	}
	exp, found := readNumberField(claims, "exp")
	if !found || exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(exp), 0), true
}

func isDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

// keyfromBody builds the identity payload shared by exchange, refresh and the
// model listing (lobsterai.ts:234-250). uuid / userId are omitted rather than
// sent as empty strings: the reference Go implementation only adds them when
// non-empty, and an empty value may be rejected as invalid.
func keyfromBody(credential *Credential, clientVersion string) map[string]any {
	body := map[string]any{
		"firstKeyfrom":  credential.FirstKeyfrom,
		"latestKeyfrom": credential.LatestKeyfrom,
		"version":       clientVersion,
	}
	if strings.TrimSpace(credential.UUID) != "" {
		body["uuid"] = credential.UUID
	}
	if strings.TrimSpace(credential.UserID) != "" {
		body["userId"] = credential.UserID
	}
	return body
}

// refreshBody is the keyfrom payload plus refreshToken (lobsterai.ts:262-270).
func refreshBody(credential *Credential, clientVersion string) map[string]any {
	body := keyfromBody(credential, clientVersion)
	body["refreshToken"] = credential.RefreshToken
	return body
}

// tokenPayload is the token/user part of an exchange or refresh response
// (lobsterai.ts:274-310).
type tokenPayload struct {
	AccessToken   string
	RefreshToken  string
	ExpiresIn     float64
	HasExpiresIn  bool
	UserID        string // user.id
	YID           string // user.yid
	AccountUserID string // user.userId
	Nickname      string
}

// parseTokenPayload reads the token payload out of an envelope `data` object.
func parseTokenPayload(data map[string]any) tokenPayload {
	user := asRecord(data["user"])
	payload := tokenPayload{
		AccessToken:   readStringField(data, "accessToken"),
		RefreshToken:  readStringField(data, "refreshToken"),
		UserID:        readStringField(user, "id"),
		YID:           readStringField(user, "yid"),
		AccountUserID: readStringField(user, "userId"),
		Nickname:      readStringField(user, "nickname"),
	}
	if expiresIn, found := readNumberField(data, "expiresIn"); found {
		payload.ExpiresIn = expiresIn
		payload.HasExpiresIn = true
	}
	return payload
}

// resolveUID resolves the stable account id with the reference implementation's
// four-level fallback (lobsterai.ts:312-335):
//
//	user.id -> user.userId -> user.yid -> sha256(accessToken) hex[:16]
//
// The hash fallback guarantees a stable id even when every server-side user
// field is empty, which matters because the account pool deduplicates by id.
// Deliberately no JWT `sub` step: the reference Go implementation has none, and
// inserting one would give the same account two different ids.
func resolveUID(payload tokenPayload) string {
	for _, candidate := range []string{payload.UserID, payload.AccountUserID, payload.YID} {
		if strings.TrimSpace(candidate) != "" {
			return candidate
		}
	}
	sum := sha256.Sum256([]byte(payload.AccessToken))
	return hex.EncodeToString(sum[:])[:16]
}

// loginSessionState is the login-time state that must survive into the
// credential (lobsterai-oauth.ts:68-85).
type loginSessionState struct {
	UUID          string
	FirstKeyfrom  string
	LatestKeyfrom string
}

// buildCredential assembles a persistable credential from a token payload
// (lobsterai.ts:337-376). expires_at preference:
//  1. expiresIn (relative seconds) measured from now — the reference uses
//     time.Now(), not the JWT iat, unlike the Buddy side;
//  2. otherwise the access-token JWT exp;
//  3. otherwise empty, and Expiry() reports "unknown" rather than "expired".
func buildCredential(payload tokenPayload, session loginSessionState, now time.Time) *Credential {
	expiresAt := ""
	if payload.HasExpiresIn && payload.ExpiresIn > 0 {
		expiresAt = strconv.FormatInt(now.Add(time.Duration(payload.ExpiresIn*float64(time.Second))).UnixMilli(), 10)
	} else if exp, ok := jwtExpiresAt(payload.AccessToken); ok {
		expiresAt = strconv.FormatInt(exp.UnixMilli(), 10)
	}

	userID := payload.AccountUserID
	if strings.TrimSpace(userID) == "" {
		userID = payload.YID
	}
	return &Credential{
		Type:          ProviderKey,
		AccessToken:   payload.AccessToken,
		RefreshToken:  payload.RefreshToken,
		ExpiresAt:     expiresAt,
		UID:           resolveUID(payload),
		UserID:        userID,
		Nickname:      payload.Nickname,
		UUID:          session.UUID,
		FirstKeyfrom:  session.FirstKeyfrom,
		LatestKeyfrom: session.LatestKeyfrom,
	}
}

// applyRefresh merges a refresh response into the stored credential. Every
// identity field is carried over: the refresh response only carries tokens, and
// latest_keyfrom deliberately keeps its original value
// (lobsterai.ts:378-410).
func applyRefresh(previous *Credential, payload tokenPayload, now time.Time) *Credential {
	refreshed := *previous
	refreshed.Type = ProviderKey
	refreshed.AccessToken = payload.AccessToken
	// A refresh response may omit refreshToken; never overwrite it with "".
	if strings.TrimSpace(payload.RefreshToken) != "" {
		refreshed.RefreshToken = payload.RefreshToken
	}
	if payload.HasExpiresIn && payload.ExpiresIn > 0 {
		refreshed.ExpiresAt = strconv.FormatInt(now.Add(time.Duration(payload.ExpiresIn*float64(time.Second))).UnixMilli(), 10)
	} else if exp, ok := jwtExpiresAt(payload.AccessToken); ok {
		refreshed.ExpiresAt = strconv.FormatInt(exp.UnixMilli(), 10)
	} else if refreshed.ExpiresAt == "" {
		refreshed.ExpiresAt = previous.ExpiresAt
	}
	return &refreshed
}

// authDataFor converts a credential into the host-facing AuthData record.
func authDataFor(credential *Credential, fileName string) (pluginapi.AuthData, error) {
	storage, errEncode := credential.Encode()
	if errEncode != nil {
		return pluginapi.AuthData{}, errEncode
	}
	if strings.TrimSpace(fileName) == "" {
		fileName = defaultAuthFileName(credential)
	}
	label := strings.TrimSpace(credential.Nickname)
	if label == "" {
		label = strings.TrimSpace(credential.UID)
	}
	if label == "" {
		label = "LobsterAI 账号"
	}
	prefix := strings.TrimSpace(credential.UID)
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	expiresAt := ""
	nextRefresh := time.Time{}
	if expiry, ok := credential.Expiry(); ok {
		expiresAt = expiry.UTC().Format(time.RFC3339)
		nextRefresh = expiry.Add(-refreshLead)
	}
	return pluginapi.AuthData{
		Provider:    ProviderKey,
		ID:          fileName,
		FileName:    fileName,
		Label:       label,
		Prefix:      prefix,
		StorageJSON: storage,
		Metadata: map[string]any{
			"expires_at":     expiresAt,
			"refreshable":    credential.Refreshable(),
			"uid":            credential.UID,
			"latest_keyfrom": credential.LatestKeyfrom,
		},
		// NOTE: do NOT use the attribute name `api_key` here. The host treats a
		// non-empty `api_key` attribute as proof that the credential is an API
		// key (see sdk/cliproxy/auth/classification.go AuthKind), which makes
		// OAuthModelAliasChannel return "" and oauthExcludedModels return nil.
		// Model aliases and `oauth-excluded-models` would then be silently
		// ignored for every LobsterAI account. The UID is an account identity,
		// not an API key, so it is published under a neutral name.
		Attributes: map[string]string{
			"uid":         credential.UID,
			"account":     label,
			"credential":  ProviderKey,
			"refreshable": boolString(credential.Refreshable()),
		},
		NextRefreshAfter: nextRefresh,
	}, nil
}

// defaultAuthFileName derives a stable auth-file name for an account, matching
// the reference layout `lobsterai-{uid}.json`.
func defaultAuthFileName(credential *Credential) string {
	identity := strings.TrimSpace(credential.UID)
	if identity == "" {
		identity = strings.TrimSpace(credential.UserID)
	}
	if identity == "" {
		identity = strings.TrimSpace(credential.Nickname)
	}
	identity = unsafeFileNameChars.ReplaceAllString(identity, "-")
	identity = strings.Trim(identity, "-")
	if identity == "" {
		identity = "account"
	}
	if len(identity) > 64 {
		identity = identity[:64]
	}
	return ProviderKey + "-" + identity + ".json"
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// handleAuthIdentifier advertises the provider key this plugin owns.
func handleAuthIdentifier(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return abiboot.IdentifierReply(ProviderKey), nil
}

// handleAuthParse recognises an auth file already present in the auth
// directory. Anything that is not a LobsterAI credential is left to the host so
// other providers get a chance to claim it.
func handleAuthParse(_ *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.AuthParseRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	if request.Provider != "" && request.Provider != ProviderKey {
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	credential, errParse := ParseCredential(request.RawJSON)
	if errParse != nil {
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	auth, errAuth := authDataFor(credential, request.FileName)
	if errAuth != nil {
		return nil, errAuth
	}
	return pluginapi.AuthParseResponse{Handled: true, Auth: auth}, nil
}

// handleAuthRefresh renews a credential through POST /api/auth/refresh.
func handleAuthRefresh(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.AuthRefreshRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	credential, errParse := ParseCredential(request.StorageJSON)
	if errParse != nil {
		return nil, errParse
	}
	refreshed, errRenew := renewCredential(h, credential, settings())
	if errRenew != nil {
		return nil, errRenew
	}
	auth, errAuth := authDataFor(refreshed, request.AuthID)
	if errAuth != nil {
		return nil, errAuth
	}
	nextRefresh := time.Time{}
	if expiry, ok := refreshed.Expiry(); ok {
		nextRefresh = expiry.Add(-refreshLead)
	}
	return pluginapi.AuthRefreshResponse{Auth: auth, NextRefreshAfter: nextRefresh}, nil
}

// renewCredential performs one silent renewal and returns the merged
// credential. It is shared by auth.refresh and by the executor's single
// post-401 retry, so both paths classify terminal failures identically.
//
// Terminal classification is more precise than the reference Go bridge: only
// HTTP 401/403, the session-dead markers, or a code:0 response without an
// accessToken end the account's life; everything else stays retryable
// (lobsterai-auth.ts:417-464).
func renewCredential(h *abiboot.Host, credential *Credential, cfg Config) (*Credential, error) {
	if !credential.Refreshable() {
		return nil, abiboot.HTTPError("not_refreshable", http.StatusUnauthorized,
			"该 LobsterAI 账号缺少 refresh_token，请重新登录")
	}
	clientVersion := resolveClientVersion(h, cfg)
	body, errMarshal := json.Marshal(refreshBody(credential, clientVersion))
	if errMarshal != nil {
		return nil, abiboot.Errorf("encode_refresh", "encode LobsterAI refresh body: %v", errMarshal)
	}
	if h == nil {
		return nil, abiboot.Errorf("refresh_transport", "LobsterAI 续期网络失败：host transport 不可用")
	}
	response, errDo := h.HTTPDo(abiboot.HTTPDoRequest{
		Method:  http.MethodPost,
		URL:     APIBase + RefreshPath,
		Headers: anonymousHeaders(),
		Body:    body,
	})
	if errDo != nil {
		return nil, abiboot.Errorf("refresh_transport", "LobsterAI 续期网络失败：%v", errDo)
	}

	envelope := parseEnvelope(response.Body)
	if !envelope.OK {
		kind := classifyLobsteraiError(response.StatusCode, string(response.Body))
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden || isTerminalError(kind) {
			return nil, abiboot.HTTPError("refresh_token_expired", http.StatusUnauthorized,
				"LobsterAI refresh_token 已失效，请重新登录：%s", envelope.Message)
		}
		return nil, abiboot.HTTPError("refresh_failed", refreshFailureStatus(response.StatusCode),
			"LobsterAI 续期失败：%s", envelope.Message)
	}

	payload := parseTokenPayload(envelope.Data)
	if strings.TrimSpace(payload.AccessToken) == "" {
		// code:0 without an accessToken means the credential can no longer be
		// renewed; the reference implementation also treats it as terminal.
		return nil, abiboot.HTTPError("refresh_token_expired", http.StatusUnauthorized,
			"LobsterAI 续期响应缺少 accessToken，请重新登录")
	}
	return applyRefresh(credential, payload, time.Now()), nil
}

// refreshFailureStatus keeps a retryable upstream failure visible as the
// original class of error instead of flattening everything into 500.
func refreshFailureStatus(status int) int {
	switch {
	case status == http.StatusTooManyRequests:
		return http.StatusTooManyRequests
	case status >= 500:
		return http.StatusBadGateway
	case status >= 400:
		return status
	default:
		return http.StatusBadGateway
	}
}
