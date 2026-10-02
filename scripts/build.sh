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

PLUGINS=(codearts cline hub loomy qoder raccoon trae lobsterai zcode minimax atomcode)

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
# The extension has to match what CPA's loader probes (`.so`/`.dylib`/`.dll`,
# internal/pluginhost/platform.go:54) AND what release.sh packages. Omitting the
# windows case here made build.sh write `<id>-v<version>.so` while release.sh
# looked for `.dll`: every packaging run would have SKIPped all 15 plugins.
case "$GOOS" in
darwin) EXT="dylib" ;;
windows) EXT="dll" ;;
*) EXT="so" ;;
esac

OUT_DIR="dist/${GOOS}/${GOARCH}"
mkdir -p "$OUT_DIR"

# Windows links through a generated module-definition file whose LIBRARY line
# carries the OUTPUT file name verbatim (`peCreateExportFile`,
# cmd/link/internal/ld/pe.go). Before Go 1.27 that name is emitted UNQUOTED:
#
#	LIBRARY codearts-v0.1.0.dll        (go1.26.0, go1.26.1)
#	LIBRARY "codearts-v0.1.0.dll"      (go1.27.0+)
#
# and GNU ld's .def parser reads the unquoted form as an expression. The trigger
# is NOT the dashes — it is the DOTTED VERSION NUMBER, which the parser tries to
# read as a float. Measured against binutils 2.47 (and the same shape fails on the
# runner's 2.41), with an otherwise identical EXPORTS body:
#
#	LIBRARY codearts.dll          OK      LIBRARY a-b-c.dll        OK
#	LIBRARY codearts-v0.dll       OK      LIBRARY codearts-v0.1.dll FAIL
#	LIBRARY codearts-v0.1.0.dll   FAIL    LIBRARY v1.2.3.dll       FAIL
#	LIBRARY x0.1.dll              FAIL    LIBRARY x_0.1.dll        FAIL
#
# A dash or an underscore is fine; `digits.digits` anywhere in the name is not.
# The failure is a hard link error, so all 15 plugins failed in every CI run and
# no Windows asset has ever shipped:
#
#	export_file.def:1: syntax error
#	file format not recognized; treating as linker script
#
# Note this is unrelated to the mingw version: it reproduces on both go1.26.x and
# the runner's toolchain. Quoting is the real fix and Go 1.27 does it; until the
# module requires >= 1.27 we link Windows under the DOT-FREE bare plugin id and
# rename to `<id>-v<version>.dll` afterwards. The bare id is exactly the name
# release.sh already stages into the zip, and CPA derives the plugin id from the
# FILE name, never from the internal LIBRARY name — so the rename is safe.
case "$GOOS" in
windows) LINK_STANDIN=1 ;;
*) LINK_STANDIN=0 ;;
esac

# link_target prints the file the compiler should actually write.
#   $1 = plugin id (used verbatim for the Windows stand-in; dots are stripped)
#   $2 = final artifact path
link_target() {
	local id="${1//./_}" final="$2"
	if [[ "$LINK_STANDIN" == "0" ]]; then
		printf '%s' "$final"
		return 0
	fi
	printf '%s/%s.%s' "$(dirname "$final")" "$id" "$EXT"
}

# finalize_link moves the stand-in onto its real name and prints the final path.
# A no-op off Windows.
finalize_link() {
	local wanted="$1" produced="$2"
	if [[ "$produced" == "$wanted" ]]; then
		printf '%s' "$wanted"
		return 0
	fi
	if [[ ! -f "$produced" ]]; then
		echo "build.sh: expected linker output '$produced' is missing" >&2
		return 1
	fi
	mv -f "$produced" "$wanted"
	# The companion header carries the stand-in stem; rename it too or the pair
	# disagrees about the library name.
	local wanted_header="${wanted%.${EXT}}.h" produced_header="${produced%.${EXT}}.h"
	if [[ -f "$produced_header" ]]; then
		mv -f "$produced_header" "$wanted_header"
	fi
	printf '%s' "$wanted"
}

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

# Artifact paths are held in two parallel arrays, not an associative one: macOS
# ships bash 3.2 as /bin/bash, where `declare -A` is not merely unsupported but
# fatal — `declare: -A: invalid option` aborted the darwin release jobs before a
# single plugin was built (CI run 36821676865). The same applies to `${!arr[@]}`
# on an empty array under `set -u`, hence the count guard in artifact_path.
ARTIFACT_IDS=()
ARTIFACT_PATHS=()
ALL_IDS=()

ok=()
failed=()
skipped=()

# record_artifact remembers the output path of one built plugin.
record_artifact() {
	ARTIFACT_IDS+=("$1")
	ARTIFACT_PATHS+=("$2")
}

# artifact_path prints the output path recorded for one plugin id, or fails.
artifact_path() {
	local want="$1" index
	((${#ARTIFACT_IDS[@]} > 0)) || return 1
	for index in "${!ARTIFACT_IDS[@]}"; do
		if [[ "${ARTIFACT_IDS[$index]}" == "$want" ]]; then
			printf '%s' "${ARTIFACT_PATHS[$index]}"
			return 0
		fi
	done
	return 1
}

# build_variant compiles one product-pinned artifact of a shared source tree.
build_variant() {
	local spec="$1" id pkg provider display product version out link_out ldflags
	IFS='|' read -r id pkg provider display product <<<"$spec"

	version="$(plugin_version "$pkg")"
	out="${OUT_DIR}/${id}-v${version}.${EXT}"
	# The display names contain spaces and parentheses, so each -X assignment is
	# quoted for the ldflags parser: it splits like a shell, and a bare space
	# would be read as the start of the next flag.
	ldflags="-X main.ProviderKey=${provider} -X \"main.DisplayName=${display}\" -X main.DefaultProduct=${product}"
	link_out="$(link_target "$id" "$out")"
	echo "==> $id: $GO build -buildmode=c-shared -ldflags \"$ldflags\" -o $link_out $pkg"
	if "$GO" build -buildmode=c-shared -ldflags "$ldflags" -o "$link_out" "$pkg" &&
		out="$(finalize_link "$out" "$link_out")"; then
		ok+=("$id")
		record_artifact "$id" "$out"
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
	link_out="$(link_target "$p" "$out")"
	echo "==> $p: $GO build -buildmode=c-shared -o $link_out $pkg"
	if "$GO" build -buildmode=c-shared -o "$link_out" "$pkg" &&
		out="$(finalize_link "$out" "$link_out")"; then
		ok+=("$p")
		record_artifact "$p" "$out"
	else
		echo "!!! $p: build FAILED" >&2
		failed+=("$p")
	fi
done

echo
echo "================ build summary (${GOOS}/${GOARCH}) ================"
printf '%-11s %-8s %s\n' PLUGIN STATUS ARTIFACT
for p in "${ALL_IDS[@]}"; do
	out="$(artifact_path "$p" || true)"
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
