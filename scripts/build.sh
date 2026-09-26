#!/usr/bin/env bash
#
# Build every CPA native plugin as a Go c-shared library.
#
# Usage:
#   scripts/build.sh                       # host GOOS/GOARCH
#   GO=/usr/local/go/bin/go scripts/build.sh
#   GOOS=linux GOARCH=arm64 scripts/build.sh
#
# Output (install-ready layout):
#   dist/<goos>/<goarch>/<plugin>-v<version>.so      (linux)
#   dist/<goos>/<goarch>/<plugin>-v<version>.dylib   (darwin)
#
# The file name matters: CPA derives the plugin ID from the file name stem,
# stripping only a trailing `-v<version>` when both halves are valid
# (internal/pluginhost/platform.go). Naming an artifact `codearts_linux_amd64.so`
# would register it as plugin ID `codearts_linux_amd64`, breaking
# `plugins.configs.<id>` and `auth.identifier`. Hence `<id>-v<version>.<ext>`.
# The produced tree can be copied straight into the CPA plugins directory:
#
#   cp -r dist/linux/amd64/. /path/to/cpa/plugins/linux/amd64/
#
# A plugin that is still being written (or has no `package main` yet) is
# reported as FAILED/SKIPPED; the remaining plugins are still built.

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

GO="${GO:-$(command -v go 2>/dev/null || echo /home/colle/.local/go/bin/go)}"
if [[ ! -x "$GO" ]]; then
	echo "build.sh: go binary not found at '$GO' (override with GO=/path/to/go)" >&2
	exit 127
fi

# c-shared always needs cgo. The remaining values match the documented build
# environment; each is overridable from the caller's environment.
export CGO_ENABLED=1
export GOFLAGS="${GOFLAGS:--mod=mod}"
export GOTOOLCHAIN="${GOTOOLCHAIN:-local}"
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
export GOSUMDB="${GOSUMDB:-off}"

PLUGINS=(codearts qoder trae lobsterai)

# Variants: one source tree, several CPA plugins.
#
# CPA derives a plugin's ID from the artifact file name and lets a plugin
# register exactly ONE provider key, which is what names its auth files, its
# model prefix and its executor route. The Tencent family therefore cannot be
# "both China and international at once" from a single artifact: it has to be
# one artifact per product, each pinned at build time.
#
# Fields: artifact-id | package | provider key | display name | default product
# The default product is a config value from plugins/codebuddy/config.go.
VARIANTS=(
	"codebuddy|./plugins/codebuddy|codebuddy|CodeBuddy|codebuddy"
	"codebuddy-intl|./plugins/codebuddy|codebuddy-intl|CodeBuddy (国际版)|codebuddy-intl"
	"workbuddy-cn|./plugins/codebuddy|workbuddy-cn|WorkBuddy (国内版)|workbuddy-cn"
	"workbuddy|./plugins/codebuddy|workbuddy|WorkBuddy (国际版)|workbuddy"
)

GOOS="${GOOS:-$("$GO" env GOOS)}"
GOARCH="${GOARCH:-$("$GO" env GOARCH)}"
case "$GOOS" in
darwin) EXT="dylib" ;;
*) EXT="so" ;;
esac

OUT_DIR="dist/${GOOS}/${GOARCH}"
mkdir -p "$OUT_DIR"

# plugin_version reads the plugin's declared version so the artifact name carries
# the same version the plugin reports in `plugin.register`. Both declaration
# styles are accepted: a standalone `const PluginVersion = "x"` and an entry in a
# `const (...)` block. Test files are ignored so a fixture cannot shadow it.
plugin_version() {
	local dir="$1" files=() file version=""
	for file in "$dir"/*.go; do
		[[ "$file" == *_test.go ]] && continue
		files+=("$file")
	done
	if ((${#files[@]} > 0)); then
		version="$(grep -rhoE '^[[:space:]]*(const[[:space:]]+)?PluginVersion[[:space:]]*=[[:space:]]*"[^"]+"' "${files[@]}" 2>/dev/null |
			head -n1 | sed -E 's/.*"([^"]+)".*/\1/')"
		if [[ -z "$version" ]]; then
			version="$(grep -rhoE '^[[:space:]]*(const[[:space:]]+)?Version[[:space:]]*=[[:space:]]*"[^"]+"' "${files[@]}" 2>/dev/null |
				head -n1 | sed -E 's/.*"([^"]+)".*/\1/')"
		fi
	fi
	printf '%s' "${version:-0.0.0}"
}

declare -A ARTIFACT=()
ALL_IDS=()

ok=()
failed=()
skipped=()

# build_variant compiles one product-pinned artifact of a shared source tree.
build_variant() {
	local spec="$1" id pkg provider display product version out ldflags
	IFS='|' read -r id pkg provider display product <<<"$spec"

	version="$(plugin_version "$pkg")"
	out="${OUT_DIR}/${id}-v${version}.${EXT}"
	# The display names contain spaces and parentheses, so each -X assignment is
	# quoted for the ldflags parser: it splits like a shell, and a bare space
	# would be read as the start of the next flag.
	ldflags="-X main.ProviderKey=${provider} -X \"main.DisplayName=${display}\" -X main.DefaultProduct=${product}"
	echo "==> $id: $GO build -buildmode=c-shared -ldflags \"$ldflags\" -o $out $pkg"
	if "$GO" build -buildmode=c-shared -ldflags "$ldflags" -o "$out" "$pkg"; then
		ok+=("$id")
		ARTIFACT["$id"]="$out"
	else
		echo "!!! $id: build FAILED" >&2
		failed+=("$id")
	fi
}

for spec in "${VARIANTS[@]}"; do
	id="${spec%%|*}"
	ALL_IDS+=("$id")
	build_variant "$spec"
done

for p in "${PLUGINS[@]}"; do
	ALL_IDS+=("$p")
	pkg="./plugins/$p"
	if [[ ! -d "$pkg" ]]; then
		echo "==> $p: SKIP (no directory $pkg)"
		skipped+=("$p")
		continue
	fi

	# -buildmode=c-shared needs a `package main` directory. Prefer the plugin
	# directory itself; otherwise fall back to a nested main package so a
	# plugin that keeps its library code at the top level still builds.
	if ! grep -qs '^package main$' "$pkg"/*.go 2>/dev/null; then
		nested="$(grep -rls --include='*.go' '^package main$' "$pkg" 2>/dev/null | head -n1 || true)"
		if [[ -n "$nested" ]]; then
			pkg="./$(dirname "$nested")"
		else
			echo "==> $p: SKIP (no package main under plugins/$p)"
			skipped+=("$p")
			continue
		fi
	fi

	version="$(plugin_version "$pkg")"
	out="${OUT_DIR}/${p}-v${version}.${EXT}"
	echo "==> $p: $GO build -buildmode=c-shared -o $out $pkg"
	if "$GO" build -buildmode=c-shared -o "$out" "$pkg"; then
		ok+=("$p")
		ARTIFACT["$p"]="$out"
	else
		echo "!!! $p: build FAILED" >&2
		failed+=("$p")
	fi
done

echo
echo "================ build summary (${GOOS}/${GOARCH}) ================"
printf '%-11s %-8s %s\n' PLUGIN STATUS ARTIFACT
for p in "${ALL_IDS[@]}"; do
	out="${ARTIFACT[$p]:-}"
	if printf '%s\n' "${ok[@]:-}" | grep -qx "$p" && [[ -n "$out" && -f "$out" ]]; then
		printf '%-11s %-8s %s (%s)\n' "$p" OK "$out" "$(du -h "$out" | cut -f1)"
	elif printf '%s\n' "${failed[@]:-}" | grep -qx "$p"; then
		printf '%-11s %-8s %s\n' "$p" FAILED "-"
	else
		printf '%-11s %-8s %s\n' "$p" SKIPPED "-"
	fi
done

if ((${#failed[@]} > 0)); then
	echo
	echo "build.sh: ${#failed[@]} plugin(s) failed: ${failed[*]}" >&2
	exit 1
fi

echo
echo "build.sh: $((${#ok[@]})) built, $((${#skipped[@]})) skipped, 0 failed"
echo "install: cp -r ${OUT_DIR}/. /path/to/cpa/plugins/${GOOS}/${GOARCH}/"
