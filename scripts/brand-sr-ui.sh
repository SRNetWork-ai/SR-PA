#!/usr/bin/env bash
#
# scripts/brand-sr-ui.sh
#
# Idempotent rebrand of THIS SOURCE TREE, from the upstream "vpn-ui" identity
# to SR-UI. It exists because the identity is spread over files that are far
# too large to hand-edit reliably (main.go 123K, vpn-ui.sh 50K, deploy.sh 27K,
# index.html 176K, core.html 80K, settings.html 70K, login.html 45K), and a
# hand pass over those would be a guessing game. A rule table is auditable;
# a 400-line diff over a 176K template is not.
#
# What it renames:
#
#   command       /usr/bin/vpn-ui   ->  /usr/bin/sr-ui
#   menu script   vpn-ui.sh         ->  sr-ui.sh      (and the //go:embed line)
#   systemd unit  vpn-ui.service    ->  sr-ui.service
#   binary        vpn-ui-amd64      ->  sr-ui-amd64
#   display name  VPN-UI            ->  SR-UI
#   identifiers   VpnUI / vpnui     ->  SrUI / srui
#
# What it deliberately LEAVES ALONE, so that an existing box keeps its data
# when it is updated in place:
#
#   /opt/vpn-ui  /usr/local/vpn-ui  /etc/vpn-ui  /var/log/vpn-ui
#   vpn-ui.db  and  config.dbBaseName
#   VPNUI_* environment variables
#   the upstream slug Sir-MmD/vpn-ui  (attribution; this fork is GPL-3.0 too)
#
# Those are data paths, not branding. Renaming them is a migration, not a
# rebrand, and it belongs in scripts/srui-server-migrate.sh where it can be
# done with the panel stopped.
#
# Usage:
#   bash scripts/brand-sr-ui.sh           apply to the working tree
#   bash scripts/brand-sr-ui.sh --check   list what is still unbranded, exit 1
#   SR_REPO=owner/name bash scripts/brand-sr-ui.sh
#
# Safe to run twice: every rule is a rename from a token that no longer
# exists after the first pass.

set -euo pipefail

MODE="apply"
if [ "${1:-}" = "--check" ]; then
	MODE="check"
elif [ -n "${1:-}" ]; then
	echo "usage: $0 [--check]" >&2
	exit 2
fi

# Release source for deploy.sh. This fork, not upstream: deploy.sh pulls the
# panel binary from "$REPO"'s latest release, and pointing a rebranded install
# at upstream's asset would silently un-rebrand the box on the next update.
SR_REPO="${SR_REPO:-SRNetWork-ai/SR-PA}"

cd "$(dirname "$0")/.."
SELF="scripts/brand-sr-ui.sh"

HAVE_GIT=0
if command -v git >/dev/null 2>&1 && git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	HAVE_GIT=1
fi

# git ls-files keeps us off .git, build output and anything ignored. The
# find fallback is for a tarball checkout.
list_files() {
	if [ "$HAVE_GIT" = "1" ]; then
		git ls-files -z
	else
		find . -type f -not -path './.git/*' -print0
	fi
}

normalise() {
	case "$1" in
		./*) printf '%s' "${1#./}" ;;
		*) printf '%s' "$1" ;;
	esac
}

# ---------------------------------------------------------------- check mode

if [ "$MODE" = "check" ]; then
	left=0
	while IFS= read -r -d '' raw; do
		f="$(normalise "$raw")"
		if [ "$f" = "$SELF" ]; then continue; fi
		if [ ! -f "$f" ]; then continue; fi
		if ! grep -Iq . "$f" 2>/dev/null; then continue; fi
		n=$(grep -c -E 'VPN-UI|VpnUI|vpnui|/usr/bin/vpn-ui|vpn-ui\.service|vpn-ui\.sh|vpn-ui-amd64' "$f" 2>/dev/null || true)
		if [ "${n:-0}" -gt 0 ]; then
			printf '%6s  %s\n' "$n" "$f"
			left=$((left + n))
		fi
	done < <(list_files)
	if [ "$left" -gt 0 ]; then
		echo ""
		echo "$left unbranded occurrence(s). Run: bash $SELF"
		exit 1
	fi
	echo "tree is branded as SR-UI"
	exit 0
fi

# ---------------------------------------------------------------- apply mode

PROG="$(mktemp)"
trap 'rm -f "$PROG"' EXIT

# Order matters. The protect/restore sentinels run first and last so that the
# broad final rule (bare "vpn-ui" -> "sr-ui", which catches prose, comments and
# default service names) cannot reach a data path or the upstream slug.
cat >"$PROG" <<'SEDPROG'
s|/opt/vpn-ui|@@SRKEEP1@@|g
s|/usr/local/vpn-ui|@@SRKEEP2@@|g
s|/etc/vpn-ui|@@SRKEEP3@@|g
s|/var/log/vpn-ui|@@SRKEEP4@@|g
s|vpn-ui\.db|@@SRKEEP5@@|g
s|Sir-MmD/vpn-ui|@@SRKEEP6@@|g
s|dbBaseName = "vpn-ui"|@@SRKEEP7@@|g
s|VPNUI_|@@SRKEEP8@@|g
s|vpn-ui\.sh|sr-ui.sh|g
s|vpn-ui-amd64|sr-ui-amd64|g
s|vpn-ui-arm64|sr-ui-arm64|g
s|vpn-ui-armv7|sr-ui-armv7|g
s|vpn-ui-s390x|sr-ui-s390x|g
s|vpn-ui\.service|sr-ui.service|g
s|/usr/bin/vpn-ui|/usr/bin/sr-ui|g
s|/usr/local/bin/vpn-ui|/usr/local/bin/sr-ui|g
s|VPN-UI|SR-UI|g
s|VPN_UI|SR_UI|g
s|VpnUI|SrUI|g
s|VpnUi|SrUi|g
s|vpnui|srui|g
s|vpn-ui|sr-ui|g
s|@@SRKEEP1@@|/opt/vpn-ui|g
s|@@SRKEEP2@@|/usr/local/vpn-ui|g
s|@@SRKEEP3@@|/etc/vpn-ui|g
s|@@SRKEEP4@@|/var/log/vpn-ui|g
s|@@SRKEEP5@@|vpn-ui.db|g
s|@@SRKEEP6@@|Sir-MmD/vpn-ui|g
s|@@SRKEEP7@@|dbBaseName = "vpn-ui"|g
s|@@SRKEEP8@@|VPNUI_|g
SEDPROG

changed=0
scanned=0

while IFS= read -r -d '' raw; do
	f="$(normalise "$raw")"
	if [ "$f" = "$SELF" ]; then continue; fi
	if [ ! -f "$f" ]; then continue; fi
	# grep -I is the binary test: skip logos, fonts, the vendored assets.
	if ! grep -Iq . "$f" 2>/dev/null; then continue; fi
	scanned=$((scanned + 1))
	if ! grep -q -E 'vpn-ui|VPN-UI|VPN_UI|VpnUI|VpnUi|vpnui' "$f" 2>/dev/null; then continue; fi
	before="$(cksum <"$f")"
	sed -i -f "$PROG" "$f"
	after="$(cksum <"$f")"
	if [ "$before" != "$after" ]; then
		changed=$((changed + 1))
		echo "branded  $f"
	fi
done < <(list_files)

# Paths that carry the old name themselves. Done after the content pass so the
# //go:embed line in main.go already points at sr-ui.sh when the file lands
# under its new name.
while IFS= read -r -d '' raw; do
	f="$(normalise "$raw")"
	if [ ! -e "$f" ]; then continue; fi
	base="$(basename "$f")"
	case "$base" in
		*vpn-ui*) : ;;
		*) continue ;;
	esac
	dir="$(dirname "$f")"
	newbase="${base//vpn-ui/sr-ui}"
	if [ "$dir" = "." ]; then
		new="$newbase"
	else
		new="$dir/$newbase"
	fi
	if [ "$new" = "$f" ]; then continue; fi
	if [ "$HAVE_GIT" = "1" ]; then
		git mv -f "$f" "$new"
	else
		mv -f "$f" "$new"
	fi
	echo "renamed  $f -> $new"
	changed=$((changed + 1))
done < <(list_files)

# deploy.sh pulls the release asset from $REPO. Upstream's releases are still
# branded vpn-ui, so a rebranded box has to fetch from this fork instead.
if [ -f deploy.sh ]; then
	if grep -q -E '^REPO="' deploy.sh; then
		sed -i -E "s|^REPO=\"[^\"]*\"|REPO=\"$SR_REPO\"|" deploy.sh
		echo "repo     deploy.sh REPO -> $SR_REPO"
	fi
fi

# The panel name the binary logs and the UI shows.
printf 'SR-UI\n' >config/name

echo ""
echo "scanned $scanned text file(s), changed $changed"
echo ""
echo "next:"
echo "  bash scripts/brand-sr-ui.sh --check"
echo "  go build -o sr-ui-amd64 -v ."
echo "  git add -A && git commit -m 'SR-UI rebrand'"
