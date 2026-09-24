#!/usr/bin/env bash
#
# SR-UI installer.
#
#   bash sr-ui-install.sh              interactive install
#   bash sr-ui-install.sh --yes        unattended, credentials generated
#   bash sr-ui-install.sh --dry-run    print every action, change nothing
#   bash sr-ui-install.sh --uninstall  remove the service, keep the database
#
# Design rule: detect, do not assume. The distro, the CPU, a free port, an existing
# panel and the binary's own CLI surface are all discovered at run time.

set -euo pipefail

OWNER="SRNetWork-ai"
REPO="SR-PA"
APP="sr-ui"
DEST="/opt/sr-ui"
BIN="$DEST/sr-ui"
UNIT="/etc/systemd/system/sr-ui.service"
MENU="/usr/bin/sr-ui"
PROTO="https"
# The panel serves plain HTTP until TLS is set up from inside the panel.
SCHEME="http"
API="${PROTO}://api.github.com/repos/${OWNER}/${REPO}"
RAW="${PROTO}://raw.githubusercontent.com/${OWNER}/${REPO}/main"

ASSUME_YES=0
DRY_RUN=0
DO_UNINSTALL=0
PURGE=0
USE_SOURCE=0
SKIP_FIREWALL=0
ALLOW_SWAP=0
REQ_VERSION=""
PANEL_PORT=""
PANEL_USER=""
PANEL_PASS=""
PANEL_PATH=""
ARCH=""
OS_ID=""
OS_FAMILY=""
OS_NAME=""
FETCHED_BIN=""
WORKDIR=""
HELP_TIMEOUT=""
RELEASE_FILE=""
RELEASE_CODE=""
TOKEN="${GITHUB_TOKEN:-${GH_TOKEN:-}}"

C_OK=""; C_WARN=""; C_ERR=""; C_DIM=""; C_OFF=""
if [ -t 2 ]; then
	C_OK=$'\033[32m'; C_WARN=$'\033[33m'; C_ERR=$'\033[31m'; C_DIM=$'\033[2m'; C_OFF=$'\033[0m'
fi

# All logging goes to stderr so functions can return values on stdout.
step() { printf '%s\n' "${C_DIM}==>${C_OFF} $*" >&2; }
info() { printf '%s\n' "${C_OK}[ok]${C_OFF} $*" >&2; }
warn() { printf '%s\n' "${C_WARN}[warn]${C_OFF} $*" >&2; }
die()  { printf '%s\n' "${C_ERR}[error]${C_OFF} $*" >&2; exit 1; }

run() {
	if [ "$DRY_RUN" = "1" ]; then
		printf '%s\n' "${C_DIM}dry-run:${C_OFF} $*" >&2
		return 0
	fi
	"$@"
}

cleanup() {
	if [ -n "$WORKDIR" ] && [ -d "$WORKDIR" ]; then
		rm -rf "$WORKDIR"
	fi
}
trap cleanup EXIT

usage() {
	cat <<'EOF'
SR-UI installer

  -y, --yes          do not ask anything; generate whatever is missing
      --port N       panel port (default: a free random port)
      --username U   panel user (default: generated)
      --password P   panel password (default: generated)
      --path P       panel web base path (default: generated)
      --version TAG  install a specific release tag (default: latest)
      --token TOK    GitHub token, for a private repository or a spent rate limit
                     (GITHUB_TOKEN or GH_TOKEN work too)
      --source       build from source instead of downloading a release
      --swap         allow a temporary swap file if RAM is too small to build
      --no-firewall  do not touch ufw/firewalld
      --dry-run      print what would happen, change nothing
      --uninstall    stop and remove the panel, keep the database
      --purge        with --uninstall, delete the database too
  -h, --help         this text
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
		-y|--yes) ASSUME_YES=1 ;;
		--port) PANEL_PORT="${2:-}"; shift ;;
		--username) PANEL_USER="${2:-}"; shift ;;
		--password) PANEL_PASS="${2:-}"; shift ;;
		--path) PANEL_PATH="${2:-}"; shift ;;
		--version) REQ_VERSION="${2:-}"; shift ;;
		--token) TOKEN="${2:-}"; shift ;;
		--source) USE_SOURCE=1 ;;
		--swap) ALLOW_SWAP=1 ;;
		--no-firewall) SKIP_FIREWALL=1 ;;
		--dry-run) DRY_RUN=1 ;;
		--uninstall) DO_UNINSTALL=1 ;;
		--purge) PURGE=1 ;;
		-h|--help) usage; exit 0 ;;
		*) die "unknown option: $1 (try --help)" ;;
	esac
	shift
done

if [ "$(id -u)" != "0" ]; then
	die "run this as root"
fi

if command -v timeout >/dev/null 2>&1; then
	HELP_TIMEOUT="timeout 10"
fi

detect_os() {
	if [ ! -r /etc/os-release ]; then
		die "/etc/os-release is missing; this system is not supported"
	fi
	# shellcheck disable=SC1091
	. /etc/os-release
	OS_ID="${ID:-unknown}"
	OS_NAME="${PRETTY_NAME:-$OS_ID}"
	case "${OS_ID} ${ID_LIKE:-}" in
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
		*) warn "unrecognised package manager; install these yourself: $*" ;;
	esac
}

detect_arch() {
	case "$(uname -m)" in
		x86_64|amd64) ARCH="amd64" ;;
		aarch64|arm64) ARCH="arm64" ;;
		armv7l|armv7) ARCH="armv7" ;;
		armv6l) ARCH="armv6" ;;
		s390x) ARCH="s390x" ;;
		*) die "unsupported CPU architecture: $(uname -m)" ;;
	esac
}

ensure_deps() {
	local missing=""
	local c
	for c in curl tar; do
		if ! command -v "$c" >/dev/null 2>&1; then
			missing="$missing $c"
		fi
	done
	if [ -n "$missing" ]; then
		step "installing missing tools:$missing"
		# shellcheck disable=SC2086
		pkg_install $missing
	fi
	if ! command -v systemctl >/dev/null 2>&1; then
		die "systemd is required and was not found"
	fi
}

rand_str() {
	local n="${1:-12}"
	LC_ALL=C tr -dc 'a-zA-Z0-9' </dev/urandom 2>/dev/null | head -c "$n" || true
}

port_in_use() {
	local p="$1"
	if command -v ss >/dev/null 2>&1; then
		ss -ltn 2>/dev/null | awk '{print $4}' | grep -qE "[:.]${p}"'$'
	elif command -v netstat >/dev/null 2>&1; then
		netstat -ltn 2>/dev/null | awk '{print $4}' | grep -qE "[:.]${p}"'$'
	else
		return 1
	fi
}

pick_port() {
	local p tries=0
	while [ "$tries" -lt 40 ]; do
		p=$(( (RANDOM % 40000) + 20000 ))
		if ! port_in_use "$p"; then
			printf '%s' "$p"
			return 0
		fi
		tries=$((tries + 1))
	done
	die "could not find a free port; pass --port"
}

existing_panels() {
	local found="" svc
	for svc in vpn-ui x-ui 3x-ui sr-ui; do
		if systemctl list-unit-files 2>/dev/null | grep -qE "^${svc}[.]service"; then
			found="$found $svc"
		fi
	done
	printf '%s' "${found# }"
}

api_curl() {
	if [ -n "$TOKEN" ]; then
		curl -fsSL --max-time 30 -H 'Accept: application/vnd.github+json' -H "Authorization: Bearer $TOKEN" "$@"
	else
		curl -fsSL --max-time 30 -H 'Accept: application/vnd.github+json' "$@"
	fi
}

# Saves the body and prints the HTTP status. -f is deliberately absent: the status
# is the answer, and "missing" and "refused" are different problems to report.
api_status() {
	local out="$1" url="$2" code=""
	if [ -n "$TOKEN" ]; then
		code="$(curl -sSL --max-time 30 -o "$out" -w '%{http_code}' -H 'Accept: application/vnd.github+json' -H "Authorization: Bearer $TOKEN" "$url" 2>/dev/null || true)"
	else
		code="$(curl -sSL --max-time 30 -o "$out" -w '%{http_code}' -H 'Accept: application/vnd.github+json' "$url" 2>/dev/null || true)"
	fi
	printf '%s' "${code:-000}"
}

fetch_release() {
	local url="$API/releases/latest"
	if [ -n "$REQ_VERSION" ]; then
		url="$API/releases/tags/$REQ_VERSION"
	fi
	RELEASE_FILE="$WORKDIR/release.json"
	RELEASE_CODE="$(api_status "$RELEASE_FILE" "$url")"
}

# Every download URL in the release payload, one per line.
release_urls() {
	grep -o '"browser_download_url"[^"]*"[^"]*"' | cut -d'"' -f4
}

asset_url() {
	release_urls | grep -i -- "$ARCH" | grep -viE 'sha256|checksum|[.](asc|sig)$' | head -n 1
}

checksum_url() {
	release_urls | grep -iE 'sha256|checksums' | head -n 1
}

verify_checksum() {
	local file="$1" sums="$2" name want got
	if [ ! -s "$sums" ]; then
		warn "this release publishes no checksum; the download was NOT verified"
		return 0
	fi
	if ! command -v sha256sum >/dev/null 2>&1; then
		warn "sha256sum not available; the download was NOT verified"
		return 0
	fi
	name="$(basename "$file")"
	want="$(grep -F "$name" "$sums" | awk '{print $1}' | head -n 1)"
	if [ -z "$want" ]; then
		warn "no checksum entry for $name; the download was NOT verified"
		return 0
	fi
	got="$(sha256sum "$file" | awk '{print $1}')"
	if [ "$want" != "$got" ]; then
		die "checksum mismatch for $name - refusing to install"
	fi
	info "checksum verified"
}

# These scripts call each other: use the copy next to this one in a checkout,
# otherwise fetch it. Prints the path to use.
helper_script() {
	local name="$1" here="" path=""
	here="$(cd "$(dirname "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd || true)"
	if [ -n "$here" ] && [ -f "$here/$name" ]; then
		printf '%s' "$here/$name"
		return 0
	fi
	path="$WORKDIR/$name"
	if [ ! -s "$path" ]; then
		if ! curl -fsSL --max-time 60 -o "$path" "$RAW/scripts/$name"; then
			return 1
		fi
	fi
	printf '%s' "$path"
}

# A source build needs a Go toolchain, disk space and enough RAM to link a large
# binary, so it lives in its own script that can install and check all three.
build_from_source() {
	local helper="" args=""
	step "fetching the source builder"
	if ! helper="$(helper_script sr-ui-build.sh)"; then
		die "could not fetch scripts/sr-ui-build.sh from ${OWNER}/${REPO}"
	fi
	FETCHED_BIN="$WORKDIR/${APP}"
	args="--out $FETCHED_BIN --repo ${OWNER}/${REPO}"
	if [ -n "$REQ_VERSION" ]; then
		args="$args --ref $REQ_VERSION"
	fi
	if [ -n "$TOKEN" ]; then
		args="$args --token $TOKEN"
	fi
	if [ "$ALLOW_SWAP" = "1" ]; then
		args="$args --swap"
	fi
	if [ "$DRY_RUN" = "1" ]; then
		args="$args --dry-run"
	fi
	# shellcheck disable=SC2086
	if ! bash "$helper" $args >/dev/null; then
		die "the source build failed; run it alone to see everything it says: bash $helper --out /tmp/${APP} --swap"
	fi
}

# The panel forks bin/xray-<goos>-<goarch> from its working directory. A release
# binary unpacks that core out of itself (corebundle); one built from source has
# nothing to unpack, and then the panel runs while every inbound stays down and
# the Reality key buttons, which shell out to the core, fail. The helper is
# idempotent, so ask it either way rather than guess which kind this is.
install_core() {
	local helper="" args="--dir $DEST/bin"
	step "checking the Xray core"
	if ! helper="$(helper_script sr-ui-core.sh)"; then
		warn "could not fetch scripts/sr-ui-core.sh; inbounds stay down until a core is in $DEST/bin"
		return 0
	fi
	if [ "$DRY_RUN" = "1" ]; then
		args="$args --dry-run"
	fi
	if [ -n "$TOKEN" ]; then
		args="$args --token $TOKEN"
	fi
	# shellcheck disable=SC2086
	if ! bash "$helper" $args >/dev/null; then
		warn "no Xray core was installed; retry it alone: bash $helper --dir $DEST/bin"
	fi
}

extract_binary() {
	local file="$1" cand
	case "$file" in
		*.tar.gz|*.tgz) run tar -xzf "$file" -C "$WORKDIR" ;;
		*.zip)
			if ! command -v unzip >/dev/null 2>&1; then
				pkg_install unzip
			fi
			run unzip -q -o "$file" -d "$WORKDIR"
			;;
		*)
			chmod +x "$file"
			FETCHED_BIN="$file"
			return 0
			;;
	esac
	cand="$(find "$WORKDIR" -type f -name '*ui*' 2>/dev/null | grep -viE '[.](tar[.]gz|tgz|zip|txt|json|sha256|asc|sig)$' | head -n 1)"
	if [ -z "$cand" ]; then
		cand="$(find "$WORKDIR" -type f -perm -u+x 2>/dev/null | head -n 1)"
	fi
	if [ -z "$cand" ]; then
		die "no binary found inside the release asset"
	fi
	chmod +x "$cand"
	FETCHED_BIN="$cand"
}

obtain_binary() {
	local url sums_url asset sums
	if [ "$USE_SOURCE" = "1" ]; then
		build_from_source
		return 0
	fi
	step "looking for a release asset for $ARCH"
	fetch_release
	case "$RELEASE_CODE" in
		200) : ;;
		404)
			if [ -n "$REQ_VERSION" ]; then
				warn "${OWNER}/${REPO} has no release tagged ${REQ_VERSION}"
			else
				warn "${OWNER}/${REPO} publishes no release yet - a fork does not inherit the releases of the project it came from, so the panel is built from source instead"
			fi
			build_from_source
			return 0
			;;
		401|403)
			warn "GitHub refused the release request (${RELEASE_CODE}); a private repository or a spent rate limit needs --token or GITHUB_TOKEN"
			build_from_source
			return 0
			;;
		*)
			warn "could not read the release API (status ${RELEASE_CODE})"
			build_from_source
			return 0
			;;
	esac
	url="$(asset_url <"$RELEASE_FILE")"
	if [ -z "$url" ]; then
		warn "the latest release has no asset matching ${ARCH}; building from source instead"
		build_from_source
		return 0
	fi
	asset="$WORKDIR/$(basename "$url")"
	step "downloading $(basename "$url")"
	if ! run api_curl --max-time 900 -o "$asset" "$url"; then
		warn "download failed; falling back to a source build"
		build_from_source
		return 0
	fi
	if [ "$DRY_RUN" = "1" ]; then
		FETCHED_BIN="$asset"
		return 0
	fi
	sums_url="$(checksum_url <"$RELEASE_FILE")"
	sums=""
	if [ -n "$sums_url" ]; then
		sums="$WORKDIR/checksums"
		api_curl --max-time 60 -o "$sums" "$sums_url" 2>/dev/null || sums=""
	fi
	verify_checksum "$asset" "$sums"
	extract_binary "$asset"
}

# The panel's CLI differs between builds, so ask this binary what it supports. A
# build that ignores unknown flags would start serving instead of printing help,
# so this never runs unbounded.
bin_help() {
	if [ -n "$HELP_TIMEOUT" ]; then
		# shellcheck disable=SC2086
		$HELP_TIMEOUT "$@" -h 2>&1 || true
	else
		"$@" -h 2>&1 || true
	fi
}

exec_start_for() {
	local bin="$1"
	if bin_help "$bin" | grep -qE '^[[:space:]]*run([[:space:]]|$)'; then
		printf '%s' "$bin run"
	else
		printf '%s' "$bin"
	fi
}

write_unit() {
	local execline="$1"
	if [ "$DRY_RUN" = "1" ]; then
		step "would write $UNIT with ExecStart=$execline"
		return 0
	fi
	cat >"$UNIT" <<EOF
[Unit]
Description=SR-UI panel
After=network.target nss-lookup.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$DEST
ExecStart=$execline
Restart=on-failure
RestartSec=3
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
EOF
}

# The menu inside the binary resolves the panel through VPNUI_BIN and otherwise
# falls back to the path upstream installs to, so a binary built without the
# rebrand answers "panel binary not found" on a healthy install.
pin_menu_bin() {
	if [ "$DRY_RUN" = "1" ] || [ ! -f "$MENU" ]; then
		return 0
	fi
	if grep -q 'VPNUI_BIN' "$MENU" 2>/dev/null; then
		if ! grep -q "$BIN" "$MENU" 2>/dev/null; then
			sed -i "1a export VPNUI_BIN=$BIN" "$MENU"
			info "menu pinned to $BIN"
		fi
	fi
}

install_menu() {
	if bin_help "$BIN" | grep -q 'install-menu'; then
		if run "$BIN" install-menu "$MENU"; then
			pin_menu_bin
			info "management command installed: $APP"
			return 0
		fi
		warn "the binary refused to install its menu; writing a minimal one"
	fi
	if [ "$DRY_RUN" = "1" ]; then
		step "would write a minimal $MENU"
		return 0
	fi
	cat >"$MENU" <<'EOF'
#!/usr/bin/env bash
case "${1:-status}" in
	start|stop|restart|status) systemctl "$1" sr-ui ;;
	log) journalctl -u sr-ui -f --no-pager ;;
	*) echo "usage: sr-ui {start|stop|restart|status|log}" ;;
esac
EOF
	chmod +x "$MENU"
	info "minimal management command installed: $APP"
}

apply_settings() {
	local help args
	help="$(bin_help "$BIN" setting)"
	args=""
	if printf '%s' "$help" | grep -q -- '-username'; then
		args="$args -username $PANEL_USER -password $PANEL_PASS"
	fi
	if printf '%s' "$help" | grep -q -- '-port'; then
		args="$args -port $PANEL_PORT"
	fi
	if printf '%s' "$help" | grep -q -- '-webBasePath'; then
		args="$args -webBasePath $PANEL_PATH"
	fi
	if [ -z "$args" ]; then
		warn "this build exposes no 'setting' flags; set the port and credentials from the panel menu ($APP)"
		return 0
	fi
	# shellcheck disable=SC2086
	if ! run "$BIN" setting $args >/dev/null 2>&1; then
		warn "applying settings failed; the panel keeps its previous credentials"
		return 0
	fi
	info "panel port, base path and credentials applied"
}

open_firewall() {
	if [ "$SKIP_FIREWALL" = "1" ]; then
		return 0
	fi
	if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q 'Status: active'; then
		run ufw allow "${PANEL_PORT}/tcp" >/dev/null 2>&1 || warn "could not add the ufw rule"
		info "opened ${PANEL_PORT}/tcp in ufw"
	elif command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
		run firewall-cmd --permanent --add-port="${PANEL_PORT}/tcp" >/dev/null 2>&1 || warn "could not add the firewalld rule"
		run firewall-cmd --reload >/dev/null 2>&1 || true
		info "opened ${PANEL_PORT}/tcp in firewalld"
	else
		step "no active firewall detected; nothing to open"
	fi
}

health_check() {
	local i=0
	while [ "$i" -lt 20 ]; do
		if curl -fsS --max-time 3 -o /dev/null "127.0.0.1:${PANEL_PORT}/${PANEL_PATH}/" 2>/dev/null; then
			return 0
		fi
		sleep 1
		i=$((i + 1))
	done
	return 1
}

# ifconfig.me answers over whichever family the server prefers, and an IPv6
# literal is only a URL host inside brackets. Ask for IPv4 first, bracket v6.
public_address() {
	local ip=""
	ip="$(curl -4 -fsS --max-time 5 ifconfig.me 2>/dev/null || true)"
	if [ -z "$ip" ]; then
		ip="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{print $7; exit}')"
	fi
	if [ -z "$ip" ]; then
		ip="$(curl -6 -fsS --max-time 5 ifconfig.me 2>/dev/null || true)"
	fi
	if [ -z "$ip" ]; then
		ip="<server-ip>"
	fi
	case "$ip" in
		*:*) printf '[%s]' "$ip" ;;
		*) printf '%s' "$ip" ;;
	esac
}

do_uninstall() {
	step "removing $APP"
	run systemctl stop "$APP" >/dev/null 2>&1 || true
	run systemctl disable "$APP" >/dev/null 2>&1 || true
	if [ -f "$UNIT" ]; then
		run rm -f "$UNIT"
	fi
	run systemctl daemon-reload >/dev/null 2>&1 || true
	run rm -f "$MENU"
	if [ "$PURGE" = "1" ]; then
		run rm -rf "$DEST"
		warn "$DEST removed, including anything stored in it"
	else
		run rm -f "$BIN"
		info "binary removed; $DEST kept"
	fi
	info "note: a database outside $DEST is untouched, even by --purge"
}

confirm_or_exit() {
	if [ "$ASSUME_YES" = "1" ] || [ "$DRY_RUN" = "1" ]; then
		return 0
	fi
	local reply=""
	printf '%s' "continue? [y/N] " >&2
	read -r reply || true
	case "$reply" in
		y|Y|yes|YES) return 0 ;;
		*) die "aborted" ;;
	esac
}

main() {
	detect_os
	detect_arch

	if [ "$DO_UNINSTALL" = "1" ]; then
		do_uninstall
		exit 0
	fi

	ensure_deps
	WORKDIR="$(mktemp -d)"

	step "system: $OS_NAME ($OS_FAMILY), architecture: $ARCH"

	local others
	others="$(existing_panels)"
	if [ -n "$others" ]; then
		warn "already installed on this server:$others"
		case "$others" in
			*sr-ui*) step "this run will upgrade sr-ui in place and keep its data" ;;
			*) warn "to move an existing panel to the SR-UI name instead, use scripts/srui-server-migrate.sh" ;;
		esac
		confirm_or_exit
	fi

	if [ -z "$PANEL_PORT" ]; then
		PANEL_PORT="$(pick_port)"
	elif port_in_use "$PANEL_PORT"; then
		die "port $PANEL_PORT is already in use"
	fi
	[ -n "$PANEL_USER" ] || PANEL_USER="admin$(rand_str 4)"
	[ -n "$PANEL_PASS" ] || PANEL_PASS="$(rand_str 16)"
	[ -n "$PANEL_PATH" ] || PANEL_PATH="$(rand_str 10)"

	obtain_binary

	step "installing to $DEST"
	run mkdir -p "$DEST"
	if systemctl is-active --quiet "$APP" 2>/dev/null; then
		run systemctl stop "$APP"
	fi
	if [ -f "$BIN" ]; then
		run cp -f "$BIN" "$BIN.bak"
		step "previous binary kept as $BIN.bak"
	fi
	run cp -f "$FETCHED_BIN" "$BIN"
	run chmod +x "$BIN"
	install_core

	local execline
	if [ "$DRY_RUN" = "1" ]; then
		execline="$BIN run"
	else
		execline="$(exec_start_for "$BIN")"
	fi
	write_unit "$execline"
	install_menu

	if [ "$DRY_RUN" != "1" ]; then
		apply_settings
	fi

	run systemctl daemon-reload
	run systemctl enable "$APP" >/dev/null 2>&1 || warn "could not enable the service at boot"
	run systemctl restart "$APP"
	open_firewall

	if [ "$DRY_RUN" = "1" ]; then
		info "dry run finished; nothing was changed"
		exit 0
	fi

	if health_check; then
		info "panel is answering on port $PANEL_PORT"
	else
		warn "the panel did not answer yet - check: journalctl -u $APP -n 50 --no-pager"
	fi

	local addr
	addr="$(public_address)"
	printf '\n'
	printf '%s\n' "  SR-UI installed"
	printf '%s\n' "  ---------------------------------------------"
	printf '%s\n' "  address   ${SCHEME}://${addr}:${PANEL_PORT}/${PANEL_PATH}/"
	printf '%s\n' "  username  ${PANEL_USER}"
	printf '%s\n' "  password  ${PANEL_PASS}"
	printf '%s\n' "  ---------------------------------------------"
	printf '%s\n' "  manage    ${APP}"
	printf '%s\n' "  logs      journalctl -u ${APP} -f"
	printf '\n'
	warn "save the password now; it is not stored anywhere else by this script"
	if [ "$PANEL_PATH" != "" ]; then
		step "the base path is part of the address: without it the panel will not answer"
	fi
}

main "$@"
