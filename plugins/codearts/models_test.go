package main

import "testing"

// Case-only OAuth aliases are ignored by CPA (EqualFold), therefore CodeArts
// publishes the canonical public spelling itself and maps it back before the
// upstream call.
func TestPublicAndUpstreamModelID(t *testing.T) {
	if got := publicModelID("glm-5.3-flash"); got != "GLM-5.3-Flash" {
		t.Errorf("publicModelID = %q", got)
	}
	if got := upstreamModelID("GLM-5.3-Flash"); got != "glm-5.3-flash" {
		t.Errorf("upstreamModelID = %q", got)
	}
	info := modelInfoFor("glm-5.3-flash", "glm-5.3-flash")
	if info.ID != "GLM-5.3-Flash" || info.Name != "glm-5.3-flash" {
		t.Errorf("model info = %+v", info)
	}
}

// TestPublicModelNamesAreCanonicalAndReversed covers the id mapping in both
// directions.
//
// The reverse direction is load-bearing: publishing `DeepSeek-V4.1-Flash` while
// sending it upstream reaches a gateway that only knows
// `deepseek-v4.1-flash`. Doing the rename in the plugin (rather than in the
// host's `oauth-model-alias`) is deliberate — that table cannot reliably hand an
// already-taken name to a second provider, measured 2026-10-01.
func TestPublicModelNamesAreCanonicalAndReversed(t *testing.T) {
	if got := publicModelID("deepseek-v4.1-flash"); got != "DeepSeek-V4.1-Flash" {
		t.Fatalf("publicModelID = %q, want the canonical name", got)
	}
	// Case-insensitive, because the upstream id's casing has drifted before.
	if got := publicModelID("DeepSeek-V4.1-Flash"); got != "DeepSeek-V4.1-Flash" {
		t.Fatalf("publicModelID(canonical) = %q", got)
	}
	if got := upstreamModelID("DeepSeek-V4.1-Flash"); got != "deepseek-v4.1-flash" {
		t.Fatalf("upstreamModelID = %q, want the native id", got)
	}
	if got := publicModelID(BenefitModel); got != "GLM-5.3-Flash" {
		t.Fatalf("the benefit model lost its mapping: %q", got)
	}
	if got := upstreamModelID("GLM-5.3-Flash"); got != BenefitModel {
		t.Fatalf("the benefit model's reverse mapping broke: %q", got)
	}
	// An unknown id passes through untouched in both directions.
	if got := publicModelID("glm-5.2"); got != "glm-5.2" {
		t.Fatalf("unknown id rewritten to %q", got)
	}
	if got := upstreamModelID("GLM-5.2"); got != "GLM-5.2" {
		t.Fatalf("unknown public name rewritten to %q", got)
	}
}
