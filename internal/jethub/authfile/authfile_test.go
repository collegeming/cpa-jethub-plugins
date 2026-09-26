package authfile

import "testing"

func TestNamePrefersTheNameTheHostAlreadyUses(t *testing.T) {
	derive := func() string { return "codearts-derived.json" }

	tests := []struct {
		name       string
		candidates []string
		want       string
	}{
		{
			// auth.parse: the file the host read the credential from.
			name:       "parse keeps the file name",
			candidates: []string{"codearts-HSTAANV6KVKBXL9FKENJ.json", "/root/.cli-proxy-api/codearts-HSTAANV6KVKBXL9FKENJ.json"},
			want:       "codearts-HSTAANV6KVKBXL9FKENJ.json",
		},
		{
			// auth.refresh: the record id, which is the same file name.
			name:       "refresh keeps the record id",
			candidates: []string{"codearts-HSTAANV6KVKBXL9FKENJ.json", "/root/.cli-proxy-api/codearts-HSTAANV6KVKBXL9FKENJ.json"},
			want:       "codearts-HSTAANV6KVKBXL9FKENJ.json",
		},
		{
			name:       "an empty record id falls back to the path attribute",
			candidates: []string{"", "/root/.cli-proxy-api/codearts-HSTAANV6KVKBXL9FKENJ.json"},
			want:       "codearts-HSTAANV6KVKBXL9FKENJ.json",
		},
		{
			name:       "a Windows path keeps only the file name",
			candidates: []string{`D:\Docker\CLIProxyAPI\auths\codearts-ABC.json`},
			want:       "codearts-ABC.json",
		},
		{
			// Nested auth directories exist: the host walks the auth dir
			// recursively, and a relative id keeps the file where it was.
			name:       "a relative sub-path survives",
			candidates: []string{"sub/codearts-ABC.json"},
			want:       "sub/codearts-ABC.json",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := Name(derive, test.candidates...); got != test.want {
				t.Fatalf("Name(%q) = %q, want %q", test.candidates, got, test.want)
			}
		})
	}
}

// A runtime auth id is not a file name. Accepting it would make the next save
// write a file the loader can never match back to the credential, so the plugin
// has to derive a real name instead.
func TestNameRejectsCandidatesThatAreNotAuthFileNames(t *testing.T) {
	candidates := []string{
		"9c1db01c89adaa0a",
		"codearts-account",
		"../../../etc/passwd.json",
		"sub/../../escape.json",
		"",
		"   ",
	}
	if got := Name(func() string { return "codearts-derived.json" }, candidates...); got != "codearts-derived.json" {
		t.Fatalf("Name(%q) = %q, want the derived name", candidates, got)
	}
}

// Only a brand-new login reaches the derivation, and without one the caller
// must get "" rather than a guess.
func TestNameWithoutCandidatesOrDerivation(t *testing.T) {
	if got := Name(nil, "", "9c1db01c89adaa0a"); got != "" {
		t.Fatalf("Name = %q, want an empty name", got)
	}
}
