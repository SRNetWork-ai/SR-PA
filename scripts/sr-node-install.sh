#!/usr/bin/env bash
#
# SR-UI node agent installer.
#
# Run this on the machine that will serve traffic, NOT on the panel. It installs
# the agent and an Xray core, enrolls against the panel with a single-use join
# token minted there, and leaves a systemd service reporting in every 30 seconds
# and running whatever config the panel hands it.
#
#   bash sr-node-install.sh --url PANEL_URL --token JOIN_TOKEN
#
# PANEL_URL must include the panel's secret path, because that is where the
# agent routes live. The panel hands you the whole line when you mint a token.

set -euo pipefail

OWNER="SRNetWork-ai"
REPO="SR-PA"
PROTO="https"
BIN="/usr/local/bin/sr-node"
DIR="/etc/sr-node"
UNIT="/etc/systemd/system/sr-node.service"
SERVICE="sr-node"
VERSION="latest"
PANEL_URL=""
TOKEN=""
CORE=""
CORE_INSTALL=1
INTERVAL=""
INSECURE=0
PURGE=0
DRY=0
ACTION="install"
GH_TOKEN_VALUE="${GITHUB_TOKEN:-}"

log()  { printf '==> %s\n' "$*"; }
ok()   { printf '[ok] %s\n' "$*"; }
warn() { printf '[warn] %s\n' "$*" >&2; }
die()  { printf '[error] %s\n' "$*" >&2; exit 1; }
run()  { if [ "$DRY" = "1" ]; then printf '    (dry-run) %s\n' "$*"; else "$@"; fi; }

usage() {
  cat <<'EOF'
SR-UI node agent installer

  --url URL        panel base url, including the panel's secret path (required)
  --token TOKEN    single-use join token minted in the panel
  --dir PATH       state directory (default /etc/sr-node)
  --bin PATH       where to install the agent (default /usr/local/bin/sr-node)
  --version TAG    release tag to install (default latest)
  --core PATH      use this xray binary instead of installing one
  --no-core        do not install a core; the node will serve nothing until one exists
  --interval SEC   seconds between heartbeats (default: whatever the panel asks)
  --insecure       accept the panel's certificate without verifying it
  --gh-token TOK   GitHub token, only needed while the repository is private
  --dry-run        print what would happen and change nothing
  --uninstall      stop and remove the agent
  --purge          with --uninstall, also delete the state directory
  -h, --help       this text
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --url) PANEL_URL="${2:-}"; shift 2 ;;
    --token) TOKEN="${2:-}"; shift 2 ;;
    --dir) DIR="${2:-}"; shift 2 ;;
    --bin) BIN="${2:-}"; shift 2 ;;
    --version) VERSION="${2:-}"; shift 2 ;;
    --core) CORE="${2:-}"; shift 2 ;;
    --no-core) CORE_INSTALL=0; shift ;;
    --interval) INTERVAL="${2:-}"; shift 2 ;;
    --insecure) INSECURE=1; shift ;;
    --gh-token) GH_TOKEN_VALUE="${2:-}"; shift 2 ;;
    --dry-run) DRY=1; shift ;;
    --uninstall) ACTION="uninstall"; shift ;;
    --purge) PURGE=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown option: $1 (try --help)" ;;
  esac
done

require_root() {
  [ "$(id -u)" = "0" ] || die "run this as root (sudo)"
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *) die "unsupported architecture: $(uname -m)" ;;
  esac
  log "architecture: $ARCH"
}

download_agent() {
  local asset="sr-node-$ARCH" tmp url
  tmp="$(mktemp)"
  if [ "$VERSION" = "latest" ]; then
    url="$PROTO://github.com/$OWNER/$REPO/releases/latest/download/$asset"
  else
    url="$PROTO://github.com/$OWNER/$REPO/releases/download/$VERSION/$asset"
  fi

  log "downloading $asset ($VERSION)"
  if [ -n "$GH_TOKEN_VALUE" ]; then
    curl -fsSL -H "Authorization: Bearer $GH_TOKEN_VALUE" -o "$tmp" "$url" || { rm -f "$tmp"; return 1; }
  else
    curl -fsSL -o "$tmp" "$url" || { rm -f "$tmp"; return 1; }
  fi

  # A 404 page saved to disk is still a successful download as far as curl is
  # concerned, and a 9KB "binary" fails later with a confusing exec error.
  local size
  size="$(wc -c < "$tmp")"
  if [ "$size" -lt 1000000 ]; then
    rm -f "$tmp"
    warn "what came back is only $size bytes, so it is not the agent"
    return 1
  fi

  run install -m 0755 "$tmp" "$BIN"
  rm -f "$tmp"
  ok "installed $BIN"
}

build_agent() {
  command -v go >/dev/null 2>&1 || return 1
  command -v git >/dev/null 2>&1 || return 1
  local src
  src="$(mktemp -d)"
  log "building the agent from source (no release asset available)"
  git clone --depth 1 "$PROTO://github.com/$OWNER/$REPO.git" "$src/repo" >/dev/null 2>&1 || { rm -rf "$src"; return 1; }
  ( cd "$src/repo" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$src/sr-node" ./cmd/sr-node ) || { rm -rf "$src"; return 1; }
  run install -m 0755 "$src/sr-node" "$BIN"
  rm -rf "$src"
  ok "built and installed $BIN"
}

obtain_agent() {
  download_agent && return 0
  warn "no release asset; trying to build from source"
  build_agent && return 0
  die "could not install the agent: no release asset and no Go toolchain to build one"
}

# The core is the whole reason this machine exists. An agent without one enrolls,
# reports in, shows up green on the fleet page and serves nobody, which is the
# most expensive way for an install to fail: it looks like it worked.
install_core() {
  if [ -n "$CORE" ]; then
    [ -x "$CORE" ] || warn "$CORE is not an executable file; the agent will look elsewhere"
    return 0
  fi
  if [ "$CORE_INSTALL" = "0" ]; then
    warn "skipping the core; this node cannot serve traffic until one is installed"
    return 0
  fi

  local core_dir="$DIR/bin"
  local expected="$core_dir/xray-linux-$ARCH"
  if [ -x "$expected" ]; then
    CORE="$expected"
    ok "core already installed at $CORE"
    return 0
  fi

  log "installing the xray core into $core_dir"
  local script url
  script="$(mktemp)"
  url="$PROTO://raw.githubusercontent.com/$OWNER/$REPO/main/scripts/sr-ui-core.sh"
  if ! curl -fsSL -o "$script" "$url"; then
    rm -f "$script"
    warn "could not fetch the core installer; install an xray binary yourself and re-run with --core PATH"
    return 0
  fi

  local args="--dir $core_dir"
  [ "$DRY" = "1" ] && args="$args --dry-run"
  # shellcheck disable=SC2086
  if bash "$script" $args; then
    ok "core installed"
  else
    warn "the core installer failed; the node will report that it has nothing to run"
  fi
  rm -f "$script"

  if [ -x "$expected" ]; then
    CORE="$expected"
  elif [ -x "$core_dir/xray" ]; then
    CORE="$core_dir/xray"
  fi
}

agent_flags() {
  local flags="-url $PANEL_URL -dir $DIR"
  [ "$INSECURE" = "1" ] && flags="$flags -insecure"
  [ -n "$CORE" ] && flags="$flags -core $CORE"
  [ -n "$INTERVAL" ] && flags="$flags -interval $INTERVAL"
  printf '%s' "$flags"
}

enroll() {
  [ -n "$TOKEN" ] || return 0
  log "enrolling against the panel"
  if [ "$DRY" = "1" ]; then
    printf '    (dry-run) %s %s -token TOKEN -once\n' "$BIN" "$(agent_flags)"
    return 0
  fi
  # Done before the service is enabled, on purpose. A wrong token, a url without
  # the panel's secret path or a blocked port must fail here, while the operator
  # is still watching, instead of turning into a node that is quietly missing.
  # shellcheck disable=SC2046
  "$BIN" $(agent_flags) -token "$TOKEN" -once || die "enrollment failed; nothing was enabled"
  ok "enrolled"
}

write_unit() {
  log "writing $UNIT"
  if [ "$DRY" = "1" ]; then
    printf '    (dry-run) ExecStart=%s %s\n' "$BIN" "$(agent_flags)"
    return 0
  fi
  cat > "$UNIT" <<EOF
[Unit]
Description=SR-UI node agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$BIN $(agent_flags)
Restart=always
RestartSec=5s
# The agent runs the core as its own child, so the core's file descriptors are
# charged to this unit. A busy node exhausts the default long before it
# exhausts anything else.
LimitNOFILE=1048576
# The agent is the only thing that may read its own token.
UMask=0077

[Install]
WantedBy=multi-user.target
EOF
  ok "service file written"
}

start_service() {
  run systemctl daemon-reload
  run systemctl enable "$SERVICE" >/dev/null 2>&1 || true
  run systemctl restart "$SERVICE"
  sleep 2
  if [ "$DRY" = "1" ]; then return 0; fi
  if systemctl is-active --quiet "$SERVICE"; then
    ok "the agent is running"
  else
    warn "the agent is not running; see: journalctl -u $SERVICE -n 50 --no-pager"
  fi
}

do_install() {
  require_root
  [ -n "$PANEL_URL" ] || die "--url is required (the panel url, including its secret path)"
  if [ -z "$TOKEN" ] && [ ! -f "$DIR/agent.json" ]; then
    die "--token is required the first time; mint a join token on the panel's Nodes page"
  fi
  detect_arch
  command -v curl >/dev/null 2>&1 || die "curl is required"
  run mkdir -p "$DIR"
  run chmod 700 "$DIR"
  obtain_agent
  install_core
  enroll
  write_unit
  start_service
  printf '\n'
  ok "node agent installed"
  printf '    state:   %s\n' "$DIR"
  if [ -n "$CORE" ]; then
    printf '    core:    %s\n' "$CORE"
  else
    printf '    core:    none found (install one, then re-run with --core PATH)\n'
  fi
  printf '    logs:    journalctl -u %s -f\n' "$SERVICE"
  printf '    status:  systemctl status %s\n' "$SERVICE"
  printf '\n'
  printf 'The panel decides what this node runs. Assign inbounds to it on the\n'
  printf 'Nodes page and the agent will fetch the config, test it, and start the\n'
  printf 'core on it within a heartbeat. If anything goes wrong the reason shows\n'
  printf 'up on that same page instead of only in this journal.\n'
}

do_uninstall() {
  require_root
  log "removing the node agent"
  run systemctl disable --now "$SERVICE" >/dev/null 2>&1 || true
  run rm -f "$UNIT"
  run systemctl daemon-reload
  run rm -f "$BIN"
  if [ "$PURGE" = "1" ]; then
    run rm -rf "$DIR"
    ok "state removed"
  else
    printf '    state kept at %s (use --purge to delete it)\n' "$DIR"
  fi
  ok "uninstalled"
}

case "$ACTION" in
  install) do_install ;;
  uninstall) do_uninstall ;;
esac
