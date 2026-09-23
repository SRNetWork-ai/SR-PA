#!/usr/bin/env bash
#
# SR-UI core installer: puts the Xray core and its geo data where the panel
# looks for them.
#
#   bash sr-ui-core.sh                     install into /opt/sr-ui/bin
#   bash sr-ui-core.sh --dir /path/bin     install somewhere else
#   bash sr-ui-core.sh --version v25.8.3   pin a specific core release
#   bash sr-ui-core.sh --force             replace a core that is already there
#
# The panel forks "bin/xray-<goos>-<goarch>" relative to its working directory.
# A release build carries that core inside the executable (see the corebundle
# package) and unpacks it on every start. A build from source carries nothing,
# so the panel comes up perfectly while the core never does, and the Xray log
# repeats "fork/exec bin/xray-linux-amd64: no such file or directory" with every
# inbound down. corebundle falls back to whatever core is already on disk, and
# this script is what puts it there.
#
# Logs go to stderr. The only thing on stdout is the path of the installed core.

set -euo pipefail

DIR="/opt/sr-ui/bin"
# The panel pins a patched Xray fork (it fixes the Shadowsocks per-user method
# fallback). Prefer it when it publishes a build for this CPU, and fall back to
# upstream Xray, which is the same core without that one patch.
REPOS="Sir-MmD/Xray-core XTLS/Xray-core"
REQ_REPO=""
VERSION=""
WANT_GEO=1
FORCE=0
DRY_RUN=0
PROTO="https"
TOKEN="${GITHUB_TOKEN:-${GH_TOKEN:-}}"
WORKDIR=""
GOARCH=""
ASSET_RE=""
CORE_NAME=""

C_OK=""; C_WARN=""; C_ERR=""; C_DIM=""; C_OFF=""
if [ -t 2 ]; then
	C_OK=$'\033[32m'; C_WARN=$'\033[33m'; C_ERR=$'\033[31m'; C_DIM=$'\033[2m'; C_OFF=$'\033[0m'
fi

step() { printf '%s\n' "${C_DIM}==>${C_OFF} $*" >&2; }
info() { printf '%s\n' "${C_OK}[ok]${C_OFF} $*" >&2; }
warn() { printf '%s\n' "${C_WARN}[warn]${C_OFF} $*" >&2; }
die()  { printf '%s\n' "${C_ERR}[error]${C_OFF} $*" >&2; exit 1; }

cleanup() {
	if [ -n "$WORKDIR" ] && [ -d "$WORKDIR" ]; then
		rm -rf "$WORKDIR"
	fi
}
trap cleanup EXIT

usage() {
	cat <<'EOF'
SR-UI core installer

      --dir PATH      where the panel looks for its core (default: /opt/sr-ui/bin)
      --version TAG   core release to install (default: the latest)
      --repo O/N      take the core from one specific repository
      --no-geo        do not install geoip.dat / geosite.dat
      --force         replace a core, and geo files, already on disk
      --token TOK     GitHub token, for a spent rate limit
      --dry-run       print what would happen, change nothing
  -h, --help          this text
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
		--dir) DIR="${2:-}"; shift ;;
		--version) VERSION="${2:-}"; shift ;;
		--repo) REQ_REPO="${2:-}"; shift ;;
		--no-geo) WANT_GEO=0 ;;
		--force) FORCE=1 ;;
		--token) TOKEN="${2:-}"; shift ;;
		--dry-run) DRY_RUN=1 ;;
		-h|--help) usage; exit 0 ;;
		*) die "unknown option: $1 (try --help)" ;;
	esac
	shift
done

# Three different names for one CPU: uname's, Go's (the panel builds the file
# name from runtime.GOARCH) and the release asset's, which follows neither.
detect_arch() {
	case "$(uname -m)" in
		x86_64|amd64) GOARCH="amd64"; ASSET_RE='linux-64[.]zip' ;;
		aarch64|arm64) GOARCH="arm64"; ASSET_RE='linux-arm64-v8a[.]zip' ;;
		armv7l|armv7) GOARCH="arm"; ASSET_RE='linux-arm32-v7a[.]zip' ;;
		armv6l) GOARCH="arm"; ASSET_RE='linux-arm32-v6[.]zip' ;;
		s390x) GOARCH="s390x"; ASSET_RE='linux-s390x[.]zip' ;;
		riscv64) GOARCH="riscv64"; ASSET_RE='linux-riscv64[.]zip' ;;
		*) die "unsupported CPU architecture: $(uname -m)" ;;
	esac
	CORE_NAME="xray-linux-${GOARCH}"
}

api_curl() {
	if [ -n "$TOKEN" ]; then
		curl -fsSL --max-time 30 -H 'Accept: application/vnd.github+json' -H "Authorization: Bearer $TOKEN" "$@"
	else
		curl -fsSL --max-time 30 -H 'Accept: application/vnd.github+json' "$@"
	fi
}

release_json() {
	local repo="$1" url="${PROTO}://api.github.com/repos/${repo}/releases/latest"
	if [ -n "$VERSION" ]; then
		url="${PROTO}://api.github.com/repos/${repo}/releases/tags/${VERSION}"
	fi
	api_curl "$url" 2>/dev/null || true
}

asset_url() {
	grep -o '"browser_download_url"[^"]*"[^"]*"' | cut -d'"' -f4 | grep -iE "$ASSET_RE" | head -n 1
}

find_asset() {
	local repo json url
	# shellcheck disable=SC2086
	for repo in $REPOS; do
		step "asking ${repo} for a core build for ${GOARCH}" >&2
		json="$(release_json "$repo")"
		if [ -z "$json" ]; then
			warn "${repo}: no readable release"
			continue
		fi
		url="$(printf '%s' "$json" | asset_url || true)"
		if [ -n "$url" ]; then
			printf '%s' "$url"
			return 0
		fi
		warn "${repo}: this release has nothing for ${GOARCH}"
	done
	return 1
}

# Xray publishes a .dgst next to each asset. Rather than depend on its exact
# labels, take the first 64 hex characters on a line that mentions 256.
verify_asset() {
	local file="$1" url="$2" dgst want got
	dgst="$WORKDIR/dgst"
	if ! curl -fsSL --max-time 60 -o "$dgst" "${url}.dgst" 2>/dev/null; then
		warn "no checksum published next to this asset; the download was NOT verified"
		return 0
	fi
	want="$(grep -i '256' "$dgst" 2>/dev/null | grep -ioE '[0-9a-f]{64}' | head -n 1 || true)"
	if [ -z "$want" ]; then
		warn "the checksum file names no sha256; the download was NOT verified"
		return 0
	fi
	if ! command -v sha256sum >/dev/null 2>&1; then
		warn "sha256sum is not available; the download was NOT verified"
		return 0
	fi
	got="$(sha256sum "$file" | awk '{print $1}')"
	if [ "$want" != "$got" ]; then
		die "checksum mismatch - refusing to install this core"
	fi
	info "checksum verified"
}

extract_zip() {
	local zip="$1" dest="$2"
	mkdir -p "$dest"
	if command -v unzip >/dev/null 2>&1; then
		unzip -q -o "$zip" -d "$dest"
		return 0
	fi
	if command -v python3 >/dev/null 2>&1; then
		python3 -c 'import sys, zipfile; zipfile.ZipFile(sys.argv[1]).extractall(sys.argv[2])' "$zip" "$dest"
		return 0
	fi
	return 1
}

# A rename swaps the directory entry for a fresh inode. Writing into the file
# instead would fail with ETXTBSY when the running panel has that core mapped.
place_file() {
	local src="$1" dest="$2" mode="$3"
	mv -f "$src" "${dest}.new"
	chmod "$mode" "${dest}.new"
	mv -f "${dest}.new" "$dest"
}

install_geo() {
	local name src
	if [ "$WANT_GEO" != "1" ]; then
		return 0
	fi
	for name in geoip.dat geosite.dat; do
		src="$(find "$WORKDIR/x" -type f -name "$name" 2>/dev/null | head -n 1)"
		if [ -z "$src" ]; then
			continue
		fi
		# The dashboard can update these, so an existing file is somebody's
		# decision, not leftovers.
		if [ -s "$DIR/$name" ] && [ "$FORCE" != "1" ]; then
			info "$name is already installed; keeping it"
			continue
		fi
		place_file "$src" "$DIR/$name" 0644
		info "installed $name"
	done
}

core_version() {
	local bin="$1"
	if command -v timeout >/dev/null 2>&1; then
		timeout 10 "$bin" version 2>/dev/null | head -n 1 || true
	else
		"$bin" version 2>/dev/null | head -n 1 || true
	fi
}

main() {
	detect_arch
	if [ -n "$REQ_REPO" ]; then
		REPOS="$REQ_REPO"
	fi
	local dest="$DIR/$CORE_NAME"
	if [ -s "$dest" ] && [ "$FORCE" != "1" ]; then
		info "a core is already installed at $dest (--force replaces it)"
		printf '%s\n' "$dest"
		return 0
	fi
	if [ "$(id -u)" != "0" ] && [ "$DRY_RUN" != "1" ]; then
		die "run this as root; it writes into $DIR"
	fi
	if ! command -v curl >/dev/null 2>&1; then
		die "curl is required and was not found"
	fi
	WORKDIR="$(mktemp -d)"
	local url
	if ! url="$(find_asset)"; then
		die "no core release for ${GOARCH} in: ${REPOS}"
	fi
	if [ "$DRY_RUN" = "1" ]; then
		step "would install $(basename "$url") as ${dest}"
		printf '%s\n' "$dest"
		return 0
	fi
	local zip="$WORKDIR/$(basename "$url")"
	step "downloading $(basename "$url")"
	if ! curl -fsSL --max-time 600 -o "$zip" "$url"; then
		die "could not download the core from ${url}"
	fi
	verify_asset "$zip" "$url"
	if ! extract_zip "$zip" "$WORKDIR/x"; then
		die "could not unpack the core: install unzip (or python3) and run this again"
	fi
	local src
	src="$(find "$WORKDIR/x" -type f -name xray 2>/dev/null | head -n 1)"
	if [ -z "$src" ]; then
		src="$(find "$WORKDIR/x" -maxdepth 2 -type f -perm -u+x 2>/dev/null | head -n 1)"
	fi
	if [ -z "$src" ]; then
		die "the archive contains no core binary"
	fi
	mkdir -p "$DIR"
	place_file "$src" "$dest" 0755
	install_geo
	local ver
	ver="$(core_version "$dest")"
	if [ -n "$ver" ]; then
		info "core installed: ${ver}"
	else
		warn "the core is in place but did not answer 'version'; check it by hand: $dest version"
	fi
	step "restart the panel so it picks the core up: systemctl restart sr-ui"
	printf '%s\n' "$dest"
}

main "$@"
