#!/usr/bin/env bash
#
# scripts/srui-server-migrate.sh
#
# Run this ON THE SERVER, as root, on a box that already runs the panel under
# its old name. It makes SR-UI the only way in:
#
#   /usr/bin/vpn-ui       ->  /usr/bin/sr-ui      (old command deleted)
#   vpn-ui.service        ->  sr-ui.service       (enabled and started again)
#   menu text  VPN-UI     ->  SR-UI
#
# It does NOT move data. /opt/vpn-ui, the vpn-ui.db beside the binary,
# /var/log/vpn-ui and the binary's own filename stay exactly where they are,
# because a rename there is a migration with something to lose, and this
# script is only about the name you type. Nothing here is destructive apart
# from the old command and the old unit file, and both are re-creatable by
# re-running the panel's own installer.
#
# Usage:
#   bash scripts/srui-server-migrate.sh
#   bash scripts/srui-server-migrate.sh --dry-run        show the plan only
#   bash scripts/srui-server-migrate.sh --keep-old       keep vpn-ui as an alias
#   bash scripts/srui-server-migrate.sh --no-unit-rename keep vpn-ui.service

set -euo pipefail

DRY=0
KEEP_OLD=0
RENAME_UNIT=1

while [ $# -gt 0 ]; do
	case "$1" in
		--dry-run) DRY=1 ;;
		--keep-old) KEEP_OLD=1 ;;
		--no-unit-rename) RENAME_UNIT=0 ;;
		-h | --help)
			sed -n '3,26p' "$0"
			exit 0
			;;
		*)
			echo "unknown option: $1" >&2
			exit 2
			;;
	esac
	shift
done

if [ "$(id -u)" != "0" ]; then
	echo "run as root (sudo bash $0)" >&2
	exit 1
fi

OLD_CMD="/usr/bin/vpn-ui"
NEW_CMD="/usr/bin/sr-ui"
UNIT_DIR="/etc/systemd/system"
OLD_UNIT="vpn-ui.service"
NEW_UNIT="sr-ui.service"

say() { printf '  %s\n' "$*"; }
step() { printf '\n== %s\n' "$*"; }

run() {
	if [ "$DRY" = "1" ]; then
		printf '  would run: %s\n' "$*"
		return 0
	fi
	"$@"
}

# ------------------------------------------------------------- find the panel

find_binary() {
	local unit out p
	for unit in "$NEW_UNIT" "$OLD_UNIT"; do
		if systemctl cat "$unit" >/dev/null 2>&1; then
			out="$(systemctl show -p ExecStart --value "$unit" 2>/dev/null || true)"
			p="$(printf '%s' "$out" | sed -n 's/.*path=\([^ ;]*\).*/\1/p' | head -n1)"
			if [ -n "$p" ] && [ -x "$p" ]; then
				printf '%s' "$p"
				return 0
			fi
		fi
	done
	for p in /opt/vpn-ui/vpn-ui-amd64 /opt/vpn-ui/vpn-ui-arm64 /opt/vpn-ui/vpn-ui \
		/usr/local/vpn-ui/vpn-ui-amd64 /usr/local/vpn-ui/vpn-ui \
		/opt/sr-ui/sr-ui-amd64 /usr/local/x-ui/x-ui; do
		if [ -x "$p" ]; then
			printf '%s' "$p"
			return 0
		fi
	done
	return 1
}

step "locating the panel binary"
BIN=""
if BIN="$(find_binary)"; then
	say "binary   $BIN"
else
	say "binary   not found (continuing: the menu script alone can be renamed)"
fi

WAS_ACTIVE=0
if systemctl is-active --quiet "$OLD_UNIT" 2>/dev/null; then
	WAS_ACTIVE=1
	say "unit     $OLD_UNIT is running"
elif systemctl is-active --quiet "$NEW_UNIT" 2>/dev/null; then
	say "unit     $NEW_UNIT is already running"
else
	say "unit     panel is not running right now"
fi

# ------------------------------------------------------------- the sr-ui menu

step "installing the sr-ui command"

if [ -n "$BIN" ] && [ "$DRY" = "0" ]; then
	# The menu script ships inside the binary, so this is the copy that
	# matches the running release. install-menu takes the target path.
	if "$BIN" install-menu "$NEW_CMD" >/dev/null 2>&1; then
		say "wrote    $NEW_CMD (from the binary's embedded menu)"
	elif [ -f "$OLD_CMD" ]; then
		cp -a "$OLD_CMD" "$NEW_CMD"
		say "wrote    $NEW_CMD (copied from $OLD_CMD)"
	else
		echo "could not produce $NEW_CMD: no embedded menu and no $OLD_CMD" >&2
		exit 1
	fi
elif [ "$DRY" = "1" ]; then
	say "would write $NEW_CMD"
elif [ -f "$OLD_CMD" ]; then
	cp -a "$OLD_CMD" "$NEW_CMD"
	say "wrote    $NEW_CMD (copied from $OLD_CMD)"
else
	echo "nothing to migrate: neither a panel binary nor $OLD_CMD exists" >&2
	exit 1
fi

# -------------------------------------------------------- brand the menu copy

step "rebranding $NEW_CMD"

PROG="$(mktemp)"
trap 'rm -f "$PROG"' EXIT

# Same discipline as scripts/brand-sr-ui.sh: protect everything that names a
# file on disk, rewrite everything that names the product, restore. The
# binary keeps its filename here, so vpn-ui-amd64 is protected too.
{
	echo 's|/opt/vpn-ui|@@K1@@|g'
	echo 's|/usr/local/vpn-ui|@@K2@@|g'
	echo 's|/etc/vpn-ui|@@K3@@|g'
	echo 's|/var/log/vpn-ui|@@K4@@|g'
	echo 's|vpn-ui\.db|@@K5@@|g'
	echo 's|Sir-MmD/vpn-ui|@@K6@@|g'
	echo 's|vpn-ui-amd64|@@K7@@|g'
	echo 's|vpn-ui-arm64|@@K8@@|g'
	echo 's|VPNUI_|@@K9@@|g'
	if [ "$RENAME_UNIT" = "1" ]; then
		echo 's|vpn-ui\.service|sr-ui.service|g'
	else
		echo 's|vpn-ui\.service|@@K10@@|g'
	fi
	echo 's|/usr/bin/vpn-ui|/usr/bin/sr-ui|g'
	echo 's|VPN-UI|SR-UI|g'
	echo 's|VpnUI|SrUI|g'
	echo 's|vpnui|srui|g'
	if [ "$RENAME_UNIT" = "1" ]; then
		# Bare occurrences are mostly systemctl arguments, which only follow
		# the unit when the unit is actually being renamed.
		echo 's|vpn-ui|sr-ui|g'
	fi
	echo 's|@@K1@@|/opt/vpn-ui|g'
	echo 's|@@K2@@|/usr/local/vpn-ui|g'
	echo 's|@@K3@@|/etc/vpn-ui|g'
	echo 's|@@K4@@|/var/log/vpn-ui|g'
	echo 's|@@K5@@|vpn-ui.db|g'
	echo 's|@@K6@@|Sir-MmD/vpn-ui|g'
	echo 's|@@K7@@|vpn-ui-amd64|g'
	echo 's|@@K8@@|vpn-ui-arm64|g'
	echo 's|@@K9@@|VPNUI_|g'
	echo 's|@@K10@@|vpn-ui.service|g'
} >"$PROG"

if [ "$DRY" = "1" ]; then
	say "would rewrite the menu text in $NEW_CMD"
else
	sed -i -f "$PROG" "$NEW_CMD"
	chmod 0755 "$NEW_CMD"
	say "rewrote  product name, command path$([ "$RENAME_UNIT" = "1" ] && printf ' and unit name' || printf '')"
fi

# --------------------------------------------------------------- systemd unit

if [ "$RENAME_UNIT" = "1" ]; then
	step "renaming the systemd unit"
	if [ -f "$UNIT_DIR/$NEW_UNIT" ]; then
		say "exists   $UNIT_DIR/$NEW_UNIT (left as is)"
		run systemctl enable "$NEW_UNIT" >/dev/null 2>&1 || true
	elif [ -f "$UNIT_DIR/$OLD_UNIT" ]; then
		run systemctl stop "$OLD_UNIT" || true
		run systemctl disable "$OLD_UNIT" >/dev/null 2>&1 || true
		run cp -a "$UNIT_DIR/$OLD_UNIT" "$UNIT_DIR/$NEW_UNIT"
		if [ "$DRY" = "0" ]; then
			sed -i -E 's|^Description=.*|Description=SR-UI panel|' "$UNIT_DIR/$NEW_UNIT"
		fi
		run rm -f "$UNIT_DIR/$OLD_UNIT"
		run systemctl daemon-reload
		run systemctl enable "$NEW_UNIT" >/dev/null 2>&1 || true
		say "unit     $OLD_UNIT -> $NEW_UNIT"
	else
		say "unit     no $OLD_UNIT on this host, nothing to rename"
	fi

	if [ "$WAS_ACTIVE" = "1" ] || systemctl is-enabled --quiet "$NEW_UNIT" 2>/dev/null; then
		run systemctl restart "$NEW_UNIT" || run systemctl start "$NEW_UNIT" || true
	fi
fi

# ----------------------------------------------------------- retire vpn-ui

step "retiring the old command"

if [ -e "$OLD_CMD" ]; then
	if [ "$KEEP_OLD" = "1" ]; then
		run rm -f "$OLD_CMD"
		run ln -s "$NEW_CMD" "$OLD_CMD"
		say "kept     $OLD_CMD as a symlink to $NEW_CMD"
	else
		run rm -f "$OLD_CMD"
		say "removed  $OLD_CMD"
	fi
else
	say "already  no $OLD_CMD on this host"
fi

hash -r 2>/dev/null || true

# ---------------------------------------------------------------------- report

step "result"

if [ "$DRY" = "1" ]; then
	say "dry run: nothing was changed"
	exit 0
fi

if [ -x "$NEW_CMD" ]; then
	say "command  sr-ui"
else
	say "command  MISSING - $NEW_CMD was not created"
fi

if [ "$RENAME_UNIT" = "1" ]; then
	state="$(systemctl is-active "$NEW_UNIT" 2>/dev/null || true)"
	say "service  $NEW_UNIT is ${state:-unknown}"
	say "logs     journalctl -u sr-ui -e"
fi

cat <<'EOF'

  Two things the panel itself still remembers:

  1. Settings -> Panel -> Service: if the service name box still says
     vpn-ui, set it to sr-ui, otherwise the panel restarts the wrong unit
     from its own UI.

  2. An in-place update run from the OLD binary re-creates /usr/bin/vpn-ui,
     because that path is compiled into it. The permanent fix is to build
     from this fork with scripts/brand-sr-ui.sh applied, which compiles the
     sr-ui path in instead.
EOF
