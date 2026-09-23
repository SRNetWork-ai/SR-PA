#!/usr/bin/env bash
#
# SR-UI source builder.
#
#   bash sr-ui-build.sh                    build into ./sr-ui
#   bash sr-ui-build.sh --out /tmp/sr-ui   build to a chosen path
#   bash sr-ui-build.sh --ref v1.2.3       build a tag or branch
#   bash sr-ui-build.sh --swap             add temporary swap on a small server
#
# The installer calls this when no release asset exists for the running
# architecture, which is the normal case for a fork: forks do not inherit the
# releases of the project they came from. It also runs perfectly well on its own.
#
# Logs go to stderr. The only thing on stdout is the path of the finished binary.

set -euo pipefail

OWNER="SRNetWork-ai"
REPO="SR-PA"
APP="sr-ui"
PROTO="https"
GO_MIN_MAJOR=1
GO_MIN_MINOR=21
GO_FALLBACK="go1.23.6"

REF="main"
OUT=""
TOKEN="${GITHUB_TOKEN:-${GH_TOKEN:-}}"
ALLOW_SWAP=0
DRY_RUN=0
KEEP_SRC=0
WORKDIR=""
SWAPFILE=""
OS_FAMILY=""
OS_NAME=""
GOARCH=""

C_OK=""; C_WARN=""; C_ERR=""; C_DIM=""; C_OFF=""
if [ -t 2 ]; then
	C_OK=$'\033[32m'; C_WARN=$'\033[33m'; C_ERR=$'\033[31m'; C_DIM=$'\033[2m'; C_OFF=$'\033[0m'
fi

step() { printf '%s\n' "${C_DIM}==>${C_OFF} $*" >&2; }
info() { printf '%s\n' "${C_OK}[ok]${C_OFF} $*" >&2; }
warn() { printf '%s\n' "${C_WARN}[warn]${C_OFF} $*" >&2; }
die()  { printf '%s\n' "${C_ERR}[error]${C_OFF} $*" >&2; exit 1; }

cleanup() {
	if [ -n "$SWAPFILE" ] && [ -f "$SWAPFILE" ]; then
		swapoff "$SWAPFILE" >/dev/null 2>&1 || true
		rm -f "$SWAPFILE"
	fi
	if [ -n "$WORKDIR" ] && [ -d "$WORKDIR" ] && [ "$KEEP_SRC" != "1" ]; then
		rm -rf "$WORKDIR"
	fi
}
trap cleanup EXIT

usage() {
	cat <<'EOF'
SR-UI source builder

      --out PATH    where to write the finished binary (default: ./sr-ui)
      --ref REF     branch, tag or commit to build (default: main)
      --repo O/R    build another fork (default: SRNetWork-ai/SR-PA)
      --token TOK   GitHub token, for a private repository
      --swap        create a temporary swap file when RAM is too small
      --keep-src    keep the cloned source instead of deleting it
      --dry-run     print what would happen, change nothing
  -h, --help        this text
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
		--out) OUT="${2:-}"; shift ;;
		--ref) REF="${2:-}"; shift ;;
		--repo)
			case "${2:-}" in
				*/*) OWNER="${2%%/*}"; REPO="${2##*/}" ;;
				*) die "--repo expects OWNER/NAME" ;;
			esac
			shift
			;;
		--token) TOKEN="${2:-}"; shift ;;
		--swap) ALLOW_SWAP=1 ;;
		--keep-src) KEEP_SRC=1 ;;
		--dry-run) DRY_RUN=1 ;;
		-h|--help) usage; exit 0 ;;
		*) die "unknown option: $1 (try --help)" ;;
	esac
	shift
done

if [ -z "$OUT" ]; then
	OUT="$PWD/$APP"
fi

run() {
	if [ "$DRY_RUN" = "1" ]; then
		printf '%s\n' "${C_DIM}dry-run:${C_OFF} $*" >&2
		return 0
	fi
	"$@"
}

detect_os() {
	if [ ! -r /etc/os-release ]; then
		die "/etc/os-release is missing; this system is not supported"
	fi
	# shellcheck disable=SC1091
	. /etc/os-release
	OS_NAME="${PRETTY_NAME:-${ID:-unknown}}"
	case "${ID:-} ${ID_LIKE:-}" in
		*debian*|*ubuntu*) OS_FAMILY="debian" ;;
		*rhel*|*fedora*|*centos*|*almalinux*|*rocky*) OS_FAMILY="rhel" ;;
		*arch*) OS_FAMILY="arch" ;;
		*suse*) OS_FAMILY="suse" ;;
		*) OS_FAMILY="" ;;
	esac
}

pkg_install() {
	case "$OS_FAMILY" in
		debian)
			run env DEBIAN_FRONTEND=noninteractive apt-get update -qq
			run env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$@"
			;;
		rhel)
			if command -v dnf >/dev/null 2>&1; then
				run dnf install -y -q "$@"
			else
				run yum install -y -q "$@"
			fi
			;;
		arch) run pacman -Sy --noconfirm "$@" ;;
		suse) run zypper --non-interactive install "$@" ;;
		*) warn "unrecognised package manager; install these yourself: $*"; return 1 ;;
	esac
}

# Go names some architectures differently from uname, and 32 bit arm builds are
# armv6l in Go's own download names.
detect_arch() {
	case "$(uname -m)" in
		x86_64|amd64) GOARCH="amd64" ;;
		aarch64|arm64) GOARCH="arm64" ;;
		armv7l|armv7|armv6l) GOARCH="armv6l" ;;
		s390x) GOARCH="s390x" ;;
		riscv64) GOARCH="riscv64" ;;
		*) die "unsupported CPU architecture: $(uname -m)" ;;
	esac
}

ensure_deps() {
	local missing="" c
	for c in curl tar git gcc make; do
		if ! command -v "$c" >/dev/null 2>&1; then
			missing="$missing $c"
		fi
	done
	if [ -n "$missing" ]; then
		step "installing build tools:$missing"
		# shellcheck disable=SC2086
		pkg_install $missing || warn "some build tools could not be installed:$missing"
	fi
	for c in curl tar git; do
		if ! command -v "$c" >/dev/null 2>&1; then
			die "$c is required and could not be installed"
		fi
	done
}

go_version_ok() {
	local v major minor
	command -v go >/dev/null 2>&1 || return 1
	v="$(go env GOVERSION 2>/dev/null || true)"
	if [ -z "$v" ]; then
		v="$(go version 2>/dev/null | awk '{print $3}')"
	fi
	v="${v#go}"
	major="${v%%.*}"
	v="${v#*.}"
	minor="${v%%.*}"
	case "${major}${minor}" in
		''|*[!0-9]*) return 1 ;;
	esac
	if [ "$major" -gt "$GO_MIN_MAJOR" ]; then
		return 0
	fi
	if [ "$major" -eq "$GO_MIN_MAJOR" ] && [ "$minor" -ge "$GO_MIN_MINOR" ]; then
		return 0
	fi
	return 1
}

go_label() {
	go env GOVERSION 2>/dev/null || printf 'go'
}

install_go_from_distro() {
	case "$OS_FAMILY" in
		debian) pkg_install golang-go ;;
		rhel) pkg_install golang ;;
		arch|suse) pkg_install go ;;
		*) return 1 ;;
	esac
	hash -r 2>/dev/null || true
	go_version_ok
}

install_go_official() {
	local ver tgz url
	ver="$(curl -fsSL --max-time 20 "${PROTO}://go.dev/VERSION?m=text" 2>/dev/null | head -n 1 || true)"
	case "$ver" in
		go[0-9]*) : ;;
		*)
			ver="$GO_FALLBACK"
			warn "go.dev did not answer which version is current; using $ver"
			;;
	esac
	tgz="${WORKDIR}/${ver}.linux-${GOARCH}.tar.gz"
	url="${PROTO}://dl.google.com/go/${ver}.linux-${GOARCH}.tar.gz"
	step "downloading ${ver} for ${GOARCH}"
	if ! curl -fsSL --max-time 900 -o "$tgz" "$url"; then
		warn "could not download $ver for $GOARCH"
		return 1
	fi
	if [ -d /usr/local/go ]; then
		warn "replacing the existing /usr/local/go"
		rm -rf /usr/local/go
	fi
	if ! tar -C /usr/local -xzf "$tgz"; then
		warn "could not unpack $ver"
		return 1
	fi
	rm -f "$tgz"
	PATH="/usr/local/go/bin:$PATH"
	export PATH
	if [ -d /etc/profile.d ]; then
		printf 'export PATH=/usr/local/go/bin:$PATH\n' >/etc/profile.d/go.sh 2>/dev/null || true
	fi
	hash -r 2>/dev/null || true
	go_version_ok
}

ensure_go() {
	if go_version_ok; then
		info "$(go_label) is already installed"
		return 0
	fi
	if [ "$DRY_RUN" = "1" ]; then
		step "would install Go: distro package first, dl.google.com as a fallback"
		return 0
	fi
	if [ "$(id -u)" != "0" ]; then
		die "Go needs to be installed and that needs root; run this with sudo"
	fi
	if command -v go >/dev/null 2>&1; then
		warn "the installed $(go_label) is older than ${GO_MIN_MAJOR}.${GO_MIN_MINOR}; installing a current one"
	else
		step "Go is not installed; installing it now"
	fi
	if install_go_from_distro; then
		info "$(go_label) installed from the distro packages"
		return 0
	fi
	if install_go_official; then
		info "$(go_label) installed in /usr/local/go"
		return 0
	fi
	die "could not install Go automatically; install Go ${GO_MIN_MAJOR}.${GO_MIN_MINOR} or newer and run this again"
}

free_mb() {
	df -Pm "$1" 2>/dev/null | awk 'NR == 2 {print $4}'
}

meminfo_mb() {
	awk -v k="$1" '$1 == k":" {printf "%d", $2 / 1024; exit}' /proc/meminfo 2>/dev/null
}

ensure_space() {
	local have
	have="$(free_mb "$WORKDIR")"
	case "${have:-x}" in
		''|*[!0-9]*) warn "could not read the free disk space"; return 0 ;;
	esac
	step "free disk space: ${have} MB"
	if [ "$have" -lt 4096 ]; then
		warn "this build wants roughly 4 GB free; ${have} MB may not be enough"
	fi
}

# The link step of a binary this size is what runs out of memory on a 1 GB server,
# and the kernel kills it with a message that explains nothing. Say so first.
ensure_memory() {
	local ram swap total file="/swapfile.sr-ui" size=2048
	ram="$(meminfo_mb MemTotal)"
	swap="$(meminfo_mb SwapTotal)"
	case "${ram:-x}" in
		''|*[!0-9]*) return 0 ;;
	esac
	case "${swap:-x}" in
		''|*[!0-9]*) swap=0 ;;
	esac
	total=$((ram + swap))
	step "memory: ${ram} MB RAM, ${swap} MB swap"
	if [ "$total" -ge 2048 ]; then
		return 0
	fi
	if [ "$ALLOW_SWAP" != "1" ]; then
		warn "the link step usually needs about 2 GB; with ${total} MB it can be killed. Re-run with --swap to add a temporary swap file"
		return 0
	fi
	if [ "$DRY_RUN" = "1" ]; then
		step "would add a temporary ${size} MB swap file"
		return 0
	fi
	step "adding a temporary ${size} MB swap file"
	if ! fallocate -l "${size}M" "$file" 2>/dev/null; then
		if ! dd if=/dev/zero of="$file" bs=1M count="$size" 2>/dev/null; then
			warn "could not create a swap file; continuing without it"
			rm -f "$file"
			return 0
		fi
	fi
	chmod 600 "$file"
	if mkswap "$file" >/dev/null 2>&1 && swapon "$file" >/dev/null 2>&1; then
		SWAPFILE="$file"
		info "temporary swap is active and is removed when this script exits"
	else
		warn "swap could not be enabled; continuing without it"
		rm -f "$file"
	fi
}

clone_source() {
	local url="${PROTO}://github.com/${OWNER}/${REPO}.git"
	if [ -n "$TOKEN" ]; then
		url="${PROTO}://x-access-token:${TOKEN}@github.com/${OWNER}/${REPO}.git"
	fi
	step "cloning ${OWNER}/${REPO} at ${REF}"
	if run git clone --depth 1 --branch "$REF" "$url" "$WORKDIR/src" 2>/dev/null; then
		return 0
	fi
	# --branch only accepts a branch or a tag, so a commit id lands here.
	warn "shallow clone of ${REF} did not work; trying a full clone"
	if ! run git clone "$url" "$WORKDIR/src"; then
		if [ -z "$TOKEN" ]; then
			die "clone failed; if ${OWNER}/${REPO} is private, pass --token or set GITHUB_TOKEN"
		fi
		die "clone failed even with a token; check that it can read ${OWNER}/${REPO}"
	fi
	run git -C "$WORKDIR/src" checkout --quiet "$REF"
}

build_source() {
	local gomod="$WORKDIR/src/go.mod" want=""
	if [ -f "$gomod" ]; then
		want="$(awk '$1 == "toolchain" {print $2; exit}' "$gomod")"
		if [ -z "$want" ]; then
			want="$(awk '$1 == "go" {print "go" $2; exit}' "$gomod")"
		fi
		if [ -n "$want" ]; then
			step "go.mod asks for ${want}; GOTOOLCHAIN=auto fetches it if the installed Go is older"
		fi
	fi
	if [ "$DRY_RUN" = "1" ]; then
		step "would run: go build -trimpath -ldflags '-s -w' -o $OUT ."
		return 0
	fi
	mkdir -p "$(dirname "$OUT")"
	step "building; on a small server this takes several minutes"
	(
		cd "$WORKDIR/src"
		export GOTOOLCHAIN="${GOTOOLCHAIN:-auto}"
		go build -trimpath -ldflags "-s -w" -o "$OUT" .
	)
	if [ ! -s "$OUT" ]; then
		die "the build reported success but produced no binary"
	fi
	chmod +x "$OUT"
}

main() {
	detect_os
	detect_arch
	step "system: ${OS_NAME} (${OS_FAMILY:-unknown family}), Go architecture: ${GOARCH}"
	ensure_deps
	WORKDIR="$(mktemp -d)"
	ensure_space
	ensure_go
	ensure_memory
	clone_source
	build_source
	if [ "$KEEP_SRC" = "1" ]; then
		info "source kept at $WORKDIR/src"
	fi
	if [ "$DRY_RUN" = "1" ]; then
		info "dry run finished; nothing was changed"
		printf '%s\n' "$OUT"
		exit 0
	fi
	info "built $(du -h "$OUT" 2>/dev/null | awk '{print $1}') binary at $OUT"
	printf '%s\n' "$OUT"
}

main "$@"
