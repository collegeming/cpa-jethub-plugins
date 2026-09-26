// Package authfile holds the one rule every provider plugin follows when it
// tells the host which auth file a credential lives in.
//
// CPA hands a plugin the name of the auth file it is working on: `auth.parse`
// carries it in `FileName`, and `auth.refresh` carries the auth record id,
// which for a file-backed credential IS that file name. A plugin that ignores
// those names and derives one from the credential's fields is guessing, and the
// guess is only as stable as the field it picks — an STS access key rotates on
// every token refresh, and a random suffix changes on every call. The host then
// writes the refreshed credential to the guessed name, which leaves the
// credential the host was refreshing untouched and grows a second entry for the
// same account.
package authfile

import (
	"path"
	"strings"
)

// Name picks the auth file name for one credential record.
//
// candidates are the names the host already knows for the credential, most
// authoritative first: the auth file it parsed, the record id it asked to
// refresh, and that record's `path`/`source` attributes — which name the file
// the credential was read from even when the host passes no id. A candidate is
// accepted when it names a JSON auth file; an absolute path is reduced to the
// file name, because the only names a plugin may write are the ones the host
// accepts for `host.auth.save`. A candidate that is not a JSON file name (a
// runtime auth index, for instance) is skipped: using it would create a file
// that no later load can match to the credential.
//
// derive runs only when no candidate qualifies, which is the brand-new-login
// case where the host genuinely knows no name yet. It must not depend on
// anything that rotates — see the package comment.
func Name(derive func() string, candidates ...string) string {
	for _, candidate := range candidates {
		if name := normalize(candidate); name != "" {
			return name
		}
	}
	if derive == nil {
		return ""
	}
	return derive()
}

// normalize turns one host-supplied candidate into a usable auth file name, or
// returns "" when the candidate cannot be one.
func normalize(candidate string) string {
	name := strings.TrimSpace(candidate)
	if name == "" {
		return ""
	}
	// Auth paths reach a plugin written the way the host's OS writes them. The
	// plugin may run on the other OS, so separators are normalised by hand
	// instead of trusting filepath.
	name = strings.ReplaceAll(name, `\`, "/")
	for _, segment := range strings.Split(name, "/") {
		// A traversal segment would let a credential escape the auth directory
		// on the way back through host.auth.save.
		if segment == ".." {
			return ""
		}
	}
	if strings.HasPrefix(name, "/") || (len(name) > 1 && name[1] == ':') {
		name = path.Base(name)
	}
	if name == "" || name == "." || name == "/" {
		return ""
	}
	if !strings.HasSuffix(strings.ToLower(name), ".json") {
		return ""
	}
	return name
}
