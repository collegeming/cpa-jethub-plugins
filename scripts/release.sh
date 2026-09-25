#!/usr/bin/env bash
#
# Package the plugins into the layout the CPA plugin store expects.
#
#   <id>_<version>_<goos>_<goarch>.zip    one per plugin per platform
#   checksums.txt                          sha256sum format
#
# The official store README requires:
#   - the release tag is `v<version>` with a dotted numeric version;
#   - every release carries one zip per supported platform plus one
#     `checksums.txt`;
#   - each zip holds the dynamic library at the zip ROOT, named `<id>.<ext>`
#     (no version suffix and no nested directory), because the CPA installer
#     renames the file to `<id>-v<version><ext>` itself.
#
# This script packages whatever platform the local toolchain can build — by
# default the host platform. Multi-platform releases are produced by
# .github/workflows/release.yml, which has the cross toolchains.
#
# Usage:
#   scripts/release.sh                     # host platform, version from sources
#   VERSION=0.2.0 scripts/release.sh
#   scripts/release.sh --skip-build        # package an existing dist/ tree

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

GO="${GO:-/home/colle/.local/go/bin/go}"
PLUGINS=(codearts codebuddy qoder trae lobsterai)

SKIP_BUILD=0
for argument in "$@"; do
	case "$argument" in
	--skip-build) SKIP_BUILD=1 ;;
	*)
		echo "release.sh: unknown argument '$argument'" >&2
		exit 2
		;;
	esac
done

GOOS="${GOOS:-$("$GO" env GOOS)}"
GOARCH="${GOARCH:-$("$GO" env GOARCH)}"
case "$GOOS" in
darwin) EXT="dylib" ;;
windows) EXT="dll" ;;
*) EXT="so" ;;
esac

# plugin_version mirrors scripts/build.sh: prefer PluginVersion, fall back to
# Version, ignoring test files.
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

if [[ $SKIP_BUILD -eq 0 ]]; then
	echo "==> building ${GOOS}/${GOARCH}"
	GOOS="$GOOS" GOARCH="$GOARCH" GO="$GO" bash scripts/build.sh
fi

RELEASE_DIR="release/${GOOS}/${GOARCH}"
rm -rf "$RELEASE_DIR"
mkdir -p "$RELEASE_DIR"

declare -A ID_VERSION=()
for plugin in "${PLUGINS[@]}"; do
	ID_VERSION["$plugin"]="$(plugin_version "plugins/$plugin")"
done

# A release tag carries one version, so every plugin shipped in it must agree.
if [[ -n "${VERSION:-}" ]]; then
	expected="${VERSION#v}"
else
	expected=""
	for plugin in "${PLUGINS[@]}"; do
		if [[ -z "$expected" ]]; then
			expected="${ID_VERSION[$plugin]}"
		elif [[ "$expected" != "${ID_VERSION[$plugin]}" ]]; then
			echo "release.sh: plugin versions disagree ('$expected' vs '${ID_VERSION[$plugin]}' for $plugin); set VERSION explicitly" >&2
			exit 1
		fi
	done
fi
if [[ ! "$expected" =~ ^[0-9]+(\.[0-9]+)*$ ]]; then
	echo "release.sh: version '$expected' is not a dotted numeric version" >&2
	exit 1
fi

ARTIFACT_DIR="dist/${GOOS}/${GOARCH}"
packaged=0
for plugin in "${PLUGINS[@]}"; do
	source_lib="${ARTIFACT_DIR}/${plugin}-v${ID_VERSION[$plugin]}.${EXT}"
	if [[ ! -f "$source_lib" ]]; then
		echo "==> $plugin: SKIP (no ${source_lib})"
		continue
	fi

	# Stage the library at the zip root under the bare `<id>.<ext>` name the
	# installer requires.
	stage="$(mktemp -d)"
	cp "$source_lib" "${stage}/${plugin}.${EXT}"
	zip_name="${plugin}_${expected}_${GOOS}_${GOARCH}.zip"
	(cd "$stage" && zip -q -X "${OLDPWD}/${RELEASE_DIR}/${zip_name}" "${plugin}.${EXT}")
	rm -rf "$stage"
	echo "==> $plugin: ${RELEASE_DIR}/${zip_name}"
	packaged=$((packaged + 1))
done

if [[ $packaged -eq 0 ]]; then
	echo "release.sh: nothing packaged" >&2
	exit 1
fi

# checksums.txt lives beside the zips in the release, so it only covers them.
(cd "$RELEASE_DIR" && sha256sum ./*.zip | sed 's|\./||' >checksums.txt)

echo
echo "================ release ${expected} (${GOOS}/${GOARCH}) ================"
ls -1 "$RELEASE_DIR" | sed 's/^/  /'
echo
echo "publish:"
echo "  git tag v${expected} && git push origin v${expected}"
echo "  gh release create v${expected} ${RELEASE_DIR}/*.zip ${RELEASE_DIR}/checksums.txt --generate-notes"
