<div align="center">

# SR-UI

**A multi-protocol VPN control panel.** Xray-core plus a full set of native VPN
servers, multi-node relays for multi-location deployments, one account across
many inbounds, and per-account accounting that actually adds up.

English | [فارسی](README_FA.md) | [العربية](README_AR.md) | [Русский](README_RU.md) | [中文](README_ZH.md) | [Türkçe](README_TR.md) | [Español](README_ES.md)

</div>

---

## What SR-UI is

Most panels are an Xray front end. SR-UI is a **control plane**: Xray is one of
the engines it drives, next to WireGuard, AmneziaWG, IKEv2, OpenVPN,
OpenConnect, SSTP, L2TP, PPTP, GRE, MTProto and an in-binary SSH gateway. One
account can live on several of them at once, and the panel keeps billing,
limits and enforcement consistent across all of them.

Three things define it:

- **One account, many inbounds.** An account is not a row inside one inbound. It
  is an identity with a set of memberships, so the same customer can hold a
  VLESS config on a node in Germany, a WireGuard peer at home and an SSH login,
  under one quota, one expiry and one device cap.
- **Multi-node.** The panel host is the master. Remote nodes run a lightweight
  agent, sync their inbound set from the master, and report usage back, so
  multi-location is a deployment choice rather than a second panel.
- **Accounting that is exact.** Every protocol has a metering path chosen to fit
  how it moves bytes: Xray stats API for native protocols, nftables counters per
  tunnel address for the kernel VPNs, and in-process byte counters for the
  userspace relays. Nothing is estimated, and nothing is billed twice.

---

## Protocols

| Family | Protocols | Metering | Notes |
| --- | --- | --- | --- |
| Xray | VLESS, VMess, Trojan, Shadowsocks, SOCKS, HTTP, Dokodemo-door | Xray stats API | REALITY, XTLS, XHTTP, gRPC, WebSocket, TCP, mKCP, HTTPUpgrade |
| Kernel VPN | WireGuard, AmneziaWG, IKEv2 (PSK / EAP-TLS / EAP-MSCHAPv2), L2TP/IPsec, PPTP, GRE | nftables counters | per-client tunnel address, hard enforcement by peer and route removal |
| TLS VPN | OpenVPN, OpenConnect (ocserv), SSTP | nftables counters | RADIUS-backed auth and session control |
| Relay | MTProto, SSH (in-binary Go gateway, TCP plus UDP via udpgw) | in-process counters | no tunnel address, routed through a loopback bridge |

The SSH gateway is worth calling out: it terminates in the panel binary, so it
owns every connection. That gives it both device-limit strategies (reject the
new device, or accept it and evict the oldest) and exact byte accounting with no
external daemon, no kernel module and no nftables rules.

---

## Nodes and multi-location

- **Master.** The panel host itself. It owns every account and all billing and
  cannot be deleted or disabled.
- **Nodes.** Remote servers running the agent. Each node reports status, agent
  version, sync state and its inbound count, and shows up as a group in the
  client editor so you pick exactly which node's inbounds an account gets.
- **Sync.** Inbound definitions flow master to node; usage and session state flow
  node to master, so quota, expiry and device limits are enforced from one place.

A customer's config set can therefore span locations: one subscription link, one
quota, several exit points.

---

## Limits and enforcement

| Limit | Scope | What happens at the limit |
| --- | --- | --- |
| Traffic quota | account | account disabled, live sessions torn down within one tick |
| Expiry | account | same, plus refusal at the next auth |
| Speed limit | account, with "limit after N bytes" | applied live through the speed-limit sidecar, no core restart |
| Device / user limit | inbound, with a per-account override that can only lower it | reject the new device, or accept it and evict the oldest |
| IP limit | account | over-limit source addresses are blocked and logged |

Enforcement is level-triggered, not fire-and-forget: every traffic tick
re-derives the disabled set from the database and re-applies it, so a session
that slipped through the exact tick a quota was crossed is still ended on the
next one.

---

## Operations

- **Admins and roles.** Multiple admins with permission scoping; each admin sees
  only their own inbounds and clients.
- **Resellers.** Credit-based reseller accounts that spend from a balance when
  they create clients.
- **Subscriptions.** Per-account subscription links with the usual client
  formats, plus QR rendering in the panel.
- **LDAP sync.** Optional job that mirrors a directory into panel accounts.
- **Backups, logs, geo files, certificates.** Scheduled jobs for geo data and
  certificate renewal, log rotation, and one-click backup and restore.
- **Live panel.** Traffic, online clients and outbound stats stream over a
  WebSocket, scoped per admin so no admin ever receives another admin's data.

---

## API

The panel exposes an HTTP API covering inbounds, clients, memberships, traffic,
nodes, settings and server actions. It is documented in full, endpoint by
endpoint with request and response bodies, in [api-reference.md](api-reference.md).

---

## Install

### From a release binary

Download the binary for your architecture from the repository releases, then
install the service and the management menu:

```bash
sudo mkdir -p /opt/sr-ui
sudo mv sr-ui-amd64 /opt/sr-ui/
sudo chmod +x /opt/sr-ui/sr-ui-amd64
sudo /opt/sr-ui/sr-ui-amd64 install-menu /usr/bin/sr-ui
sudo /opt/sr-ui/sr-ui-amd64 --systemd
sudo systemctl enable --now sr-ui
```

Then open the panel and sign in. Change the default credentials and the panel
port before exposing it.

### Migrating an existing install

A server already running the upstream panel can be moved over in place. The
migration script stops the old unit, moves the data directory and database,
rewrites the systemd unit and the management command, and starts the panel again
as `sr-ui`:

```bash
sudo bash scripts/srui-server-migrate.sh --dry-run   # show the plan
sudo bash scripts/srui-server-migrate.sh             # apply it
```

After it finishes, open Settings, Panel, and set the service name to `sr-ui` so
the panel restarts itself through the right unit.

### Management menu

```bash
sr-ui
```

The menu covers start, stop and restart, status and logs, port and credential
changes, certificate issuance and renewal, backup and restore, core management
and uninstall.

---

## Build from source

Go 1.24 or newer:

```bash
git clone github.com/SRNetWork-ai/SR-PA.git
cd SR-PA
bash scripts/brand-sr-ui.sh --check    # verify brand strings
go build -trimpath -ldflags "-s -w" -o sr-ui-amd64 -v .
```

The repository also ships `build.sh` for the full release build (embedded assets
and bundled cores) and `deploy.sh` for scripted server deployment. CI builds and
vets every push, so release artifacts come from the workflow rather than a
workstation.

---

## Configuration

Runtime paths and log level are read from the environment. The current build
reads these names:

| Variable | Purpose | Default |
| --- | --- | --- |
| `VPNUI_LOG_LEVEL` | log verbosity (`debug`, `info`, `warning`, `error`) | `info` |
| `VPNUI_DEBUG` | enable debug mode | `false` |
| `VPNUI_BIN_FOLDER` | Xray and helper binaries | `bin` |
| `VPNUI_DB_FOLDER` | database directory | `/etc/x-ui` |
| `VPNUI_LOG_FOLDER` | log directory | `/var/log` |

`SRUI_` aliases are planned; until they land these names stay valid, so an
upgrade never breaks an existing unit file.

---

## Repository layout

```
main.go            entry point, CLI, embedded management script
config/            build-time name, version and path resolution
web/
  controller/      HTTP handlers and API routes
  service/         panel logic: inbounds, accounts, protocols, nodes, limits
  job/             scheduled work: traffic, IP limit, certificates, geo, LDAP
  html/            panel templates
  assets/          panel CSS and JS
xray/              Xray process control, config model and stats API client
database/          models and migrations
sub/               subscription server
scripts/           branding and server migration helpers
test_unit/         integration harness
```

---

## Documentation

The design documents in the repository root are the real reference for how each
subsystem works and why:

| Document | Covers |
| --- | --- |
| [api-reference.md](api-reference.md) | every HTTP endpoint |
| [multi-inbound-client-plan.md](multi-inbound-client-plan.md) | accounts spanning several inbounds |
| [control-plane-framework.md](control-plane-framework.md) | how a protocol is added to the control plane |
| [device-limit-plan.md](device-limit-plan.md) | device caps and eviction strategies |
| [ip-limiter-plan.md](ip-limiter-plan.md) | the IP limiter |
| [speed-limit-plan.md](speed-limit-plan.md) | live speed limiting |
| [reseller-plan.md](reseller-plan.md) | credit-based resellers |
| [wireguard-plan.md](wireguard-plan.md), [amneziawg-plan.md](amneziawg-plan.md), [gre-plan.md](gre-plan.md), [ikev2-plan.md](ikev2-plan.md), [sstp-plan.md](sstp-plan.md), [openconnect-plan.md](openconnect-plan.md), [ssh-plan.md](ssh-plan.md) | one document per protocol |
| [accounts-upgrade-guide.md](accounts-upgrade-guide.md) | the account model migration |

---

## Credits

SR-UI is a fork of **vpn-ui** by Sir-MmD (github.com/Sir-MmD/vpn-ui), which is
itself built on **3x-ui** by MHSanaei (github.com/MHSanaei/3x-ui). The proxy
engine is **Xray-core** by the XTLS project (github.com/XTLS/Xray-core). Routing
data comes from the v2ray-rules-dat project. Thanks to everyone whose work this
builds on.

## License

GPL-3.0. See [LICENSE](LICENSE).

## Disclaimer

This software is provided for lawful use only. You are responsible for complying
with the laws and regulations that apply where you operate it.
