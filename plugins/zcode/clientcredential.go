package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// Optional import path: adopt the official client's own login.
//
// This is NOT the primary path — the plugin's own login (login.go) is — but a
// user who already runs the official ZCode client can adopt its credential
// without logging in again. It only READS a file; nothing here ever writes the
// client's store.
//
// ## The encryption, and why every detail is load-bearing
//
// The official client stores `~/.zcode/v2/credentials.json` as a flat object of
// `key -> value`, where an encrypted value is
//
//	enc:v1:<iv>.<authtag>.<ciphertext>
//
// with each of the three segments base64**url** (unpadded), and the key derived as
//
//	key = sha256("zcode-credential-fallback:" + platform + ":" + homedir + ":" + username)
//
// or, when the environment variable `ZCODE_CREDENTIAL_SECRET` is set,
//
//	key = sha256(that value)
//
// The algorithm is AES-256-GCM with a 12-byte IV and a 16-byte tag. A value
// WITHOUT the `enc:v1:` prefix is returned as-is, which is what makes a
// hand-written plaintext entry work too.
//
// ⚠ A deviation in ANY of the four pieces — the prefix string, the platform
// spelling (`win32` / `darwin` / `linux`), the home directory as the client saw
// it, or the `unknown` fallback for an unresolvable user name — produces a GCM
// authentication failure. That is the failure mode to expect when this ever
// stops working on a new machine: the file decrypted fine elsewhere.
//
// Go's standard library covers all of it (`crypto/aes`, `crypto/cipher`,
// `crypto/sha256`, `encoding/base64`), so this needs no dependency.

// Credential-store constants, from `zcode.ts:30-42`.
const (
	credentialPrefix     = "enc:v1:"
	credentialSecretEnv  = "ZCODE_CREDENTIAL_SECRET"
	credentialIVLength   = 12
	credentialTagLength  = 16
	credentialKeyPrefix  = "zcode-credential-fallback"
	credentialUnknownUse = "unknown"
)

// Key fragments: the official key names are long and contain a uuid, so the
// reference matches on a fragment (`zcode.ts:270-276`).
const (
	keyFragmentJWT            = "zcodejwttoken"
	keyFragmentBigModelAccess = "oauth:bigmodel:access_token"
	keyFragmentCodingPlanZai  = "zai-individual-coding-plan"
	keyFragmentCodingPlanBig  = "bigmodel-individual-coding-plan"
	keyFragmentUserInfo       = "oauth:bigmodel:user_info"
)

// clientCredentialFile is the parsed client store: key -> ciphertext-or-plain.
type clientCredentialFile struct {
	Values map[string]string
	Path   string
}

// credentialFileCandidates lists the places the client's store may live, in
// priority order.
//
// The reference stopped scanning the filesystem for this file: the official
// client writes it under a FIXED path — `<dataBaseDir>/.zcode/v2/credentials.json`
// with `dataBaseDir` defaulting to the home directory — so a short, certain list
// is enough.
func credentialFileCandidates(dataDir string) []string {
	out := make([]string, 0, 4)
	push := func(dir string) {
		trimmed := strings.TrimSpace(dir)
		if trimmed == "" {
			return
		}
		candidate := filepath.Join(trimmed, ".zcode", "v2", "credentials.json")
		for _, existing := range out {
			if existing == candidate {
				return
			}
		}
		out = append(out, candidate)
	}

	// ① an explicit data root. The environment variable accepts a `;`-separated
	// list because the official client reads the same one.
	if configured := strings.TrimSpace(dataDir); configured != "" {
		push(configured)
	}
	if fromEnv := strings.TrimSpace(os.Getenv("ZCODE_DATA_BASE_DIR")); fromEnv != "" {
		for _, dir := range strings.Split(fromEnv, ";") {
			push(dir)
		}
	}
	// ② the home directory, which is the official default.
	if home, errHome := os.UserHomeDir(); errHome == nil {
		push(home)
	}
	// ③ the Windows data roots: some install shapes use %APPDATA%\ZCode as their
	// userData directory. Trying both costs one stat each.
	push(os.Getenv("APPDATA"))
	push(os.Getenv("LOCALAPPDATA"))
	return out
}

// resolveCredentialFilePath returns the first existing candidate, or the first
// candidate when none exists (so the error message names a real path).
func resolveCredentialFilePath(dataDir string) string {
	candidates := credentialFileCandidates(dataDir)
	for _, candidate := range candidates {
		if info, errStat := os.Stat(candidate); errStat == nil && info != nil && !info.IsDir() {
			return candidate
		}
	}
	if len(candidates) > 0 {
		return candidates[0]
	}
	return filepath.Join(".zcode", "v2", "credentials.json")
}

// readClientCredentials reads the raw client store.
//
// A missing, unreadable or malformed file yields an error rather than an empty
// map, so the caller can say WHY the import did not happen.
func readClientCredentials(dataDir string) (*clientCredentialFile, error) {
	path := resolveCredentialFilePath(dataDir)
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		return nil, fmt.Errorf("读取官方客户端凭据失败（%s）：%w", path, errRead)
	}
	var parsed map[string]any
	if errUnmarshal := json.Unmarshal(raw, &parsed); errUnmarshal != nil {
		return nil, fmt.Errorf("官方客户端凭据不是 JSON 对象（%s）：%w", path, errUnmarshal)
	}
	values := make(map[string]string, len(parsed))
	for key, value := range parsed {
		if text, ok := value.(string); ok {
			values[key] = text
		}
	}
	return &clientCredentialFile{Values: values, Path: path}, nil
}

// deriveClientCredentialKey reproduces the official key derivation.
//
// ⚠ Do NOT "normalise" anything here. The official secret uses NODE's
// `process.platform` spelling exactly:
//
//	win32 / darwin / linux
//
// Go spells Windows `windows`, so using `runtime.GOOS` directly is WRONG on the one
// platform where almost every captured store comes from: the key differs and GCM
// authentication fails. `credentialPlatform()` performs only this required spelling
// conversion; every other component stays byte-for-byte what the official client
// uses.
func deriveClientCredentialKey() []byte {
	if explicit := os.Getenv(credentialSecretEnv); explicit != "" {
		sum := sha256.Sum256([]byte(explicit))
		return sum[:]
	}
	username := credentialUnknownUse
	if current, errUser := user.Current(); errUser == nil && strings.TrimSpace(current.Username) != "" {
		username = current.Username
	}
	home := ""
	if resolved, errHome := os.UserHomeDir(); errHome == nil {
		home = resolved
	}
	secret := credentialKeyPrefix + ":" + credentialPlatform() + ":" + home + ":" + username
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// credentialPlatform returns Node's `process.platform` spelling, because that exact
// string is part of the official client's key preimage.
func credentialPlatform() string {
	if runtime.GOOS == "windows" {
		return "win32"
	}
	return runtime.GOOS
}

// decryptClientCredentialValue decrypts one stored value.
//
// A value without the `enc:v1:` prefix is returned unchanged, which is the
// official `decrypt()` behaviour and what makes a hand-written plaintext entry
// work as well.
func decryptClientCredentialValue(value string) (string, error) {
	if !strings.HasPrefix(value, credentialPrefix) {
		return value, nil
	}
	segments := strings.Split(strings.TrimPrefix(value, credentialPrefix), ".")
	if len(segments) != 3 {
		return "", errors.New("凭据密文格式非法（应为 iv.tag.data 三段）")
	}
	if segments[0] == "" || segments[1] == "" || segments[2] == "" {
		return "", errors.New("凭据密文格式非法（存在空段）")
	}
	iv, errIV := base64.RawURLEncoding.DecodeString(segments[0])
	if errIV != nil {
		return "", fmt.Errorf("凭据 IV 不是 base64url：%w", errIV)
	}
	tag, errTag := base64.RawURLEncoding.DecodeString(segments[1])
	if errTag != nil {
		return "", fmt.Errorf("凭据 AuthTag 不是 base64url：%w", errTag)
	}
	data, errData := base64.RawURLEncoding.DecodeString(segments[2])
	if errData != nil {
		return "", fmt.Errorf("凭据密文不是 base64url：%w", errData)
	}
	if len(iv) != credentialIVLength {
		return "", fmt.Errorf("凭据 IV 长度非法（%d ≠ %d）", len(iv), credentialIVLength)
	}
	if len(tag) != credentialTagLength {
		return "", fmt.Errorf("凭据 AuthTag 长度非法（%d ≠ %d）", len(tag), credentialTagLength)
	}

	block, errCipher := aes.NewCipher(deriveClientCredentialKey())
	if errCipher != nil {
		return "", fmt.Errorf("初始化 AES 失败：%w", errCipher)
	}
	aead, errAEAD := cipher.NewGCM(block)
	if errAEAD != nil {
		return "", fmt.Errorf("初始化 GCM 失败：%w", errAEAD)
	}
	// Go's Open expects the tag APPENDED to the ciphertext; the client stores it
	// as its own segment, so the two are concatenated here.
	sealed := append(append([]byte(nil), data...), tag...)
	plain, errOpen := aead.Open(nil, iv, sealed, nil)
	if errOpen != nil {
		return "", fmt.Errorf("凭据解密失败（密钥不匹配或密文损坏）：%w", errOpen)
	}
	return string(plain), nil
}

// pickClientCredential finds and decrypts the first value whose key contains a
// fragment.
//
// A single key that cannot be decrypted does not stop the others: an older entry
// may have been written with a different secret.
func pickClientCredential(values map[string]string, fragment string) string {
	for key, value := range values {
		if !strings.Contains(key, fragment) {
			continue
		}
		plain, errDecrypt := decryptClientCredentialValue(value)
		if errDecrypt != nil {
			continue
		}
		if strings.TrimSpace(plain) != "" {
			return strings.TrimSpace(plain)
		}
	}
	return ""
}

// userInfoLabel extracts a display label from the client's user-info blob.
//
// A phone number keeps only its last four digits, matching the masking convention
// the other providers use.
func userInfoLabel(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	var info map[string]any
	if errUnmarshal := json.Unmarshal([]byte(raw), &info); errUnmarshal != nil {
		return ""
	}
	for _, field := range []string{"phone", "mobile", "name", "nickname", "email"} {
		text := stringField(info, field)
		if text == "" {
			continue
		}
		if digitsOnlyPattern.MatchString(text) && len(text) >= 7 {
			return "尾号" + text[len(text)-4:]
		}
		if len(text) > 24 {
			return text[:24]
		}
		return text
	}
	for _, field := range []string{"id", "userId"} {
		text := stringField(info, field)
		if len(text) >= 6 {
			return "id:" + text[len(text)-6:]
		}
	}
	return ""
}

// digitsOnlyPattern recognises a phone-like string.
var digitsOnlyPattern = regexp.MustCompile(`^\d{7,}$`)

// telemetryFileCandidates lists the device-id files, next to the credential file.
func telemetryFileCandidates(dataDir string) []string {
	out := make([]string, 0, 4)
	push := func(dir string) {
		trimmed := strings.TrimSpace(dir)
		if trimmed == "" {
			return
		}
		candidate := filepath.Join(trimmed, ".zcode", "v2", "telemetry-state.json")
		for _, existing := range out {
			if existing == candidate {
				return
			}
		}
		out = append(out, candidate)
	}
	if configured := strings.TrimSpace(dataDir); configured != "" {
		push(configured)
	}
	if fromEnv := strings.TrimSpace(os.Getenv("ZCODE_DATA_BASE_DIR")); fromEnv != "" {
		for _, dir := range strings.Split(fromEnv, ";") {
			push(dir)
		}
	}
	if home, errHome := os.UserHomeDir(); errHome == nil {
		push(home)
	}
	push(os.Getenv("APPDATA"))
	return out
}

// readClientDeviceMid reads the device id the official client recorded.
//
// ⚠ The id is a hard requirement on the billing endpoints — without it they
// answer `400 {"code":3001}` — while its VALUE is not validated. Reading the
// client's own value is therefore a convenience, not a necessity: an import that
// cannot find it still generates one.
func readClientDeviceMid(dataDir string) string {
	for _, path := range telemetryFileCandidates(dataDir) {
		raw, errRead := os.ReadFile(path)
		if errRead != nil {
			continue
		}
		var parsed struct {
			DeviceMid string `json:"deviceMid"`
		}
		if errUnmarshal := json.Unmarshal(raw, &parsed); errUnmarshal != nil {
			continue
		}
		if trimmed := strings.TrimSpace(parsed.DeviceMid); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// importClientCredential builds a Credential from the official client's store.
//
// This is the "adopt an existing login" path. The plugin's own login stays
// primary: if a user logs in through the plugin and the machine also happens to
// hold a stale client store, the plugin credential must win — otherwise a fresh
// login would appear to fail.
func importClientCredential(dataDir string) (*Credential, error) {
	store, errRead := readClientCredentials(dataDir)
	if errRead != nil {
		return nil, errRead
	}
	jwt := pickClientCredential(store.Values, keyFragmentJWT)
	if jwt == "" {
		return nil, credentialError("client_credential_missing",
			"官方客户端凭据（%s）里没有可用的 %s —— "+
				"它可能不存在、解密失败，或使用了不同的 ZCODE_CREDENTIAL_SECRET", store.Path, keyFragmentJWT)
	}
	deviceMid := readClientDeviceMid(dataDir)
	generated := false
	if deviceMid == "" {
		// The value is not validated, so generating one keeps the import usable
		// on a machine whose telemetry file is absent.
		deviceMid = generateDeviceMid()
		generated = true
	}
	userInfo := pickClientCredential(store.Values, keyFragmentUserInfo)
	label := userInfoLabel(userInfo)
	if label == "" {
		label = "设备" + shortMid(deviceMid)
	}
	credential := &Credential{
		ZCodeJWT:              jwt,
		DeviceMid:             deviceMid,
		BigModelAccessToken:   pickClientCredential(store.Values, keyFragmentBigModelAccess),
		BigModelRefreshToken:  "",
		CodingPlanKeyZai:      pickClientCredential(store.Values, keyFragmentCodingPlanZai),
		CodingPlanKeyBigmodel: pickClientCredential(store.Values, keyFragmentCodingPlanBig),
		AccountLabel:          label,
		AppVersion:            detectInstalledAppVersion(),
		Source:                "ide",
	}
	if generated {
		credential.AccountLabel = label + "（设备号由插件生成）"
	}
	credential.normalize()
	return credential, nil
}

// shortMid renders the first eight characters of a device id.
func shortMid(deviceMid string) string {
	if len(deviceMid) <= 8 {
		return deviceMid
	}
	return deviceMid[:8]
}

// detectInstalledAppVersion probes the installed official client's version.
//
// The version only rides a header, so a failed probe must NOT make the provider
// unavailable: the built-in fallback is returned instead.
func detectInstalledAppVersion() string {
	if explicit := strings.TrimSpace(os.Getenv("ZCODE_APP_VERSION")); explicit != "" {
		return explicit
	}
	roots := []string{
		filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "ZCode"),
		filepath.Join(os.Getenv("PROGRAMFILES"), "ZCode"),
		filepath.Join(os.Getenv("PROGRAMFILES(X86)"), "ZCode"),
	}
	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		if _, errStat := os.Stat(filepath.Join(root, "ZCode.exe")); errStat != nil {
			continue
		}
		manifest := filepath.Join(root, ".zcode-install-manifest")
		raw, errRead := os.ReadFile(manifest)
		if errRead != nil {
			continue
		}
		if match := manifestVersionPattern.FindStringSubmatch(string(raw)); len(match) > 1 {
			return match[1]
		}
	}
	return DefaultAppVersion
}

// manifestVersionPattern extracts `"version": "x.y.z"` from the install manifest.
var manifestVersionPattern = regexp.MustCompile(`"version"\s*:\s*"(\d+\.\d+\.\d+)"`)
