package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
)

// ErrRefreshTokenExpired reports that the refresh token can never be used
// again; the account has to be re-authenticated interactively.
var ErrRefreshTokenExpired = errors.New("codearts: refresh token expired")

// identifierSize is the byte length of a P-256 coordinate.
const identifierSize = 32

// pkcePair holds a PKCE verifier and its SHA-256 challenge.
type pkcePair struct {
	Verifier  string
	Challenge string
}

// newPKCE generates the 48-byte verifier and base64url(SHA-256(verifier))
// challenge Jet-Hub uses. Note the portal expects the literal method name
// "SHA-256", not the RFC 7636 "S256".
func newPKCE() (*pkcePair, error) {
	buf := make([]byte, 48)
	if _, err := rand.Read(buf); err != nil {
		return nil, abiboot.Errorf("pkce_generate", "generate PKCE verifier: %v", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	return &pkcePair{Verifier: verifier, Challenge: base64.RawURLEncoding.EncodeToString(sum[:])}, nil
}

// randomHex returns n random bytes hex-encoded.
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", abiboot.Errorf("random_generate", "generate random bytes: %v", err)
	}
	return hex.EncodeToString(buf), nil
}

// dpopKeyPair is an ephemeral ES256 (P-256) key used to build DPoP proofs.
type dpopKeyPair struct {
	private *ecdsa.PrivateKey
}

// newDpopKeyPair generates a fresh P-256 keypair.
func newDpopKeyPair() (*dpopKeyPair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, abiboot.Errorf("dpop_generate", "generate DPoP key: %v", err)
	}
	return &dpopKeyPair{private: key}, nil
}

// keyPairFromStoredJWK reconstructs the keypair persisted in the credential.
func keyPairFromStoredJWK(jwk DpopPrivateJWK) (*dpopKeyPair, error) {
	if jwk.D == "" || jwk.X == "" || jwk.Y == "" {
		return nil, abiboot.Errorf("dpop_invalid", "stored DPoP key is incomplete")
	}
	dBytes, errD := base64.RawURLEncoding.DecodeString(jwk.D)
	if errD != nil {
		return nil, abiboot.Errorf("dpop_invalid", "decode DPoP private scalar: %v", errD)
	}
	xBytes, errX := base64.RawURLEncoding.DecodeString(jwk.X)
	if errX != nil {
		return nil, abiboot.Errorf("dpop_invalid", "decode DPoP X: %v", errX)
	}
	yBytes, errY := base64.RawURLEncoding.DecodeString(jwk.Y)
	if errY != nil {
		return nil, abiboot.Errorf("dpop_invalid", "decode DPoP Y: %v", errY)
	}
	key := &ecdsa.PrivateKey{
		PublicKey: ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(xBytes), Y: new(big.Int).SetBytes(yBytes)},
		D:         new(big.Int).SetBytes(dBytes),
	}
	if !key.Curve.IsOnCurve(key.PublicKey.X, key.PublicKey.Y) {
		return nil, abiboot.Errorf("dpop_invalid", "stored DPoP public point is not on P-256")
	}
	return &dpopKeyPair{private: key}, nil
}

// publicJWK renders the public half as a JWK.
func (k *dpopKeyPair) publicJWK() DpopPrivateJWK {
	return DpopPrivateJWK{
		Kty: "EC",
		Crv: "P-256",
		X:   base64.RawURLEncoding.EncodeToString(k.private.PublicKey.X.FillBytes(make([]byte, identifierSize))),
		Y:   base64.RawURLEncoding.EncodeToString(k.private.PublicKey.Y.FillBytes(make([]byte, identifierSize))),
	}
}

// privateJWK renders the full keypair for persistence.
func (k *dpopKeyPair) privateJWK() DpopPrivateJWK {
	jwk := k.publicJWK()
	jwk.D = base64.RawURLEncoding.EncodeToString(k.private.D.FillBytes(make([]byte, identifierSize)))
	return jwk
}

// signDPoP builds the ES256 DPoP proof JWS for one request.
func (k *dpopKeyPair) signDPoP(method, htu string) (string, error) {
	jti, errJTI := randomHex(32)
	if errJTI != nil {
		return "", errJTI
	}
	public := k.publicJWK()
	protected := map[string]any{
		"alg": "ES256",
		"typ": "dpop+jwt",
		"jwk": map[string]string{
			"kty": public.Kty,
			"crv": public.Crv,
			"x":   public.X,
			"y":   public.Y,
		},
	}
	protectedJSON, errProtected := json.Marshal(protected)
	if errProtected != nil {
		return "", abiboot.Errorf("dpop_sign", "encode DPoP header: %v", errProtected)
	}
	payload := map[string]any{
		"htm": strings.ToUpper(method),
		"htu": htu,
		"iat": time.Now().Unix(),
		"jti": jti,
	}
	payloadJSON, errPayload := json.Marshal(payload)
	if errPayload != nil {
		return "", abiboot.Errorf("dpop_sign", "encode DPoP payload: %v", errPayload)
	}

	signingInput := base64.RawURLEncoding.EncodeToString(protectedJSON) + "." + base64.RawURLEncoding.EncodeToString(payloadJSON)
	digest := sha256.Sum256([]byte(signingInput))
	r, s, errSign := ecdsa.Sign(rand.Reader, k.private, digest[:])
	if errSign != nil {
		return "", abiboot.Errorf("dpop_sign", "sign DPoP proof: %v", errSign)
	}
	// JWS ES256 signatures are the fixed-width r||s concatenation.
	signature := make([]byte, 64)
	r.FillBytes(signature[:identifierSize])
	s.FillBytes(signature[identifierSize:])
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
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

// requestToken exchanges form values at the STS endpoint with a DPoP proof.
func requestToken(h *abiboot.Host, form url.Values, keyPair *dpopKeyPair) (*tokenResponse, error) {
	proof, errProof := keyPair.signDPoP(http.MethodPost, STSTokenEndpoint)
	if errProof != nil {
		return nil, errProof
	}
	headers := http.Header{
		"DPoP":         []string{proof},
		"Content-Type": []string{"application/x-www-form-urlencoded"},
	}
	response, errDo := h.HTTPDo(abiboot.HTTPDoRequest{
		Method:  http.MethodPost,
		URL:     STSTokenEndpoint,
		Headers: headers,
		Body:    []byte(form.Encode()),
	})
	if errDo != nil {
		return nil, abiboot.Errorf("sts_request", "request STS token: %v", errDo)
	}

	var token tokenResponse
	if len(response.Body) > 0 {
		if errDecode := json.Unmarshal(response.Body, &token); errDecode != nil {
			return nil, abiboot.Errorf("sts_response", "decode STS token response (HTTP %d): %v", response.StatusCode, errDecode)
		}
	}

	if token.Error != "" || token.ErrorCode != "" {
		if isTerminalTokenError(token.Error, token.ErrorCode) {
			return nil, fmt.Errorf("%w: %s %s", ErrRefreshTokenExpired, token.ErrorCode, token.ErrorMsg)
		}
		return nil, abiboot.Errorf("sts_rejected", "STS rejected the request: %s %s %s", token.Error, token.ErrorCode, token.ErrorMsg)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, abiboot.Errorf("sts_status", "STS returned HTTP %d: %s", response.StatusCode, truncate(string(response.Body), 400))
	}
	if token.Credentials == nil || token.Credentials.AccessKeyID == "" {
		return nil, abiboot.Errorf("sts_response", "STS response carries no credentials")
	}
	return &token, nil
}

// isTerminalTokenError reports whether the refresh token is permanently dead.
func isTerminalTokenError(errorName, errorCode string) bool {
	if strings.EqualFold(strings.TrimSpace(errorName), "invalid_grant") {
		return true
	}
	code := strings.TrimSpace(errorCode)
	return strings.Contains(code, "ExpiredRefreshToken") || strings.Contains(code, "InvalidDPoPHeader")
}

// credentialFromToken maps a token response onto the persisted credential.
func credentialFromToken(token *tokenResponse, codeVerifier string, keyPair *dpopKeyPair) *Credential {
	credential := &Credential{
		Type:         ProviderKey,
		CodeVerifier: codeVerifier,
	}
	if token.Credentials != nil {
		credential.AccessKeyID = token.Credentials.AccessKeyID
		credential.SecretAccessKey = token.Credentials.SecretAccessKey
		credential.SecurityToken = token.Credentials.SecurityToken
		credential.ExpiresAt = token.Credentials.Expiration
	}
	credential.RefreshToken = token.RefreshToken
	if keyPair != nil {
		jwk := keyPair.privateJWK()
		credential.DpopPrivateKeyJWK = &jwk
	}
	return credential
}

// exchangeAuthorizationCode trades the portal callback code for credentials.
func exchangeAuthorizationCode(h *abiboot.Host, code, verifier, redirectURI string, keyPair *dpopKeyPair) (*tokenResponse, error) {
	form := url.Values{
		"client_id":     {OAuthClientID},
		"code":          {code},
		"code_verifier": {verifier},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {redirectURI},
	}
	return requestToken(h, form, keyPair)
}

// exchangeRefreshToken renews an expired credential.
func exchangeRefreshToken(h *abiboot.Host, refreshToken, verifier string, keyPair *dpopKeyPair) (*tokenResponse, error) {
	form := url.Values{
		"client_id":     {OAuthClientID},
		"code_verifier": {verifier},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}
	return requestToken(h, form, keyPair)
}

// buildOAuthLoginURL assembles the portal authorize URL for the PKCE flow.
func buildOAuthLoginURL(port int, pkce *pkcePair, ticketID string) string {
	query := url.Values{
		"theme":                 {OAuthTheme},
		"locale":                {OAuthLocale},
		"uri_scheme":            {OAuthClientID},
		"client_id":             {OAuthClientID},
		"port":                  {strconv.Itoa(port)},
		"code_challenge":        {pkce.Challenge},
		"code_challenge_method": {"SHA-256"},
		"ticket_id":             {ticketID},
		"plugin-name":           {LoginPluginName},
		"plugin-version":        {LoginPluginVersion},
	}
	return PortalAuthorizeBase + "?" + query.Encode()
}

// buildLegacyLoginURL assembles the ticket-flow login URL.
func buildLegacyLoginURL(port int, ticketID string) string {
	callback := fmt.Sprintf("http://127.0.0.1:%d%s", port, LegacyCallbackPath)
	redirect := LegacyLoginBase +
		"?IdeaType=jetbrains.&auth_callback_url=" + url.QueryEscape(callback) +
		"&plugin-name=" + LegacyPluginName +
		"&plugin-version=" + LegacyPluginVersion +
		"&ticket_id=" + url.QueryEscape(ticketID)
	return LegacyAuthBase + "?service=" + url.QueryEscape(redirect)
}

// oauthSuccessRedirect is where the browser lands after a successful login.
func oauthSuccessRedirect() string {
	query := url.Values{
		"login_succeed": {"true"},
		"uri_scheme":    {OAuthClientID},
		"locale":        {OAuthLocale},
	}
	return PortalLoginBase + "?" + query.Encode()
}

// truncate shortens s for inclusion in an error message.
func truncate(s string, limit int) string {
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}
