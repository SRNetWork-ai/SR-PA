# Nodes and relay

SR-UI can serve traffic from machines other than the one running the panel.
The panel stays the only source of truth for inbounds, clients, quotas and
expiry; a node is a plain Linux box that runs a small agent, runs an Xray
core with a config the panel renders for it, and reports back what it
served.

This page covers how that works end to end, what the panel will and will
not ship to a node, and what to look at when a node misbehaves.

- [Roles](#roles)
- [Joining a node](#joining-a-node)
- [The cycle](#the-cycle)
- [What the panel renders for a node](#what-the-panel-renders-for-a-node)
- [Clients the panel withholds](#clients-the-panel-withholds)
- [Traffic accounting](#traffic-accounting)
- [Multi location and relay chains](#multi-location-and-relay-chains)
- [Operating a node](#operating-a-node)
- [API surface](#api-surface)
- [Troubleshooting](#troubleshooting)
- [Security notes](#security-notes)

## Roles

**edge** - the node terminates client connections. The panel renders a copy
of each inbound you assign to it, so a client config pointed at the node's
address connects there and never touches the master.

**relay** - the node accepts connections and forwards them to the next hop
without terminating them. The hop is another node (`relayViaId`) or the
panel itself, recorded internally as `@master` and resolved from the panel
URL the agent enrolled against. Relay inbounds are plain dokodemo-door
forwarders, so they carry no client list and need no credentials.

A relay in front of an edge node is the usual shape for Iran: a domestic
relay IP that clients reach easily, forwarding to a foreign edge that does
the real work.

## Joining a node

1. In the panel, open **Nodes** and add the node: a name, its address, the
   agent port (62789 by default), the role, and optionally country, city,
   provider and tags. The address is also what another node dials when it
   relays through this one.
2. Assign inbounds to it from the **Inbounds** action on its row. Nothing
   is shipped to a node that is not assigned.
3. Press the token action to mint an enrollment token. It is valid for 30
   minutes and burns on first use. The panel shows the exact command:

   ```
   bash <(curl -fsSL raw.githubusercontent.com/SRNetWork-ai/SR-PA/main/scripts/sr-node-install.sh) --url PANEL_URL --token TOKEN
   ```

   `PANEL_URL` is the full panel address including the secret base path,
   exactly as you use it in a browser.

The installer downloads `sr-node-<arch>` from the rolling `latest` release,
installs an Xray core into `/etc/sr-node/bin` (skip with `--no-core` if the
node already has one), writes a `sr-node` systemd unit, and starts it. The
agent then calls `node/enroll` once, receives a long-lived node token, and
stores it in `/etc/sr-node/agent.json`. The enrollment token is useless
after that.

Useful installer flags: `--dir`, `--bin`, `--version`, `--core`,
`--no-core`, `--interval`, `--insecure`, `--gh-token`, `--dry-run`,
`--uninstall`, `--purge`.

## The cycle

Every tick - 30 seconds by default, 5 seconds minimum - the agent does
three things in this order:

1. **Heartbeat.** `POST node/heartbeat` with agent version, core version,
   the config hash it has applied, uptime, CPU, memory and disk usage, host
   interface totals, the latency of the previous call, and its last error.
   The panel answers with the hash it expects, `inSync`, and its own clock.
2. **Traffic.** `POST node/traffic` with the bytes this node served since
   its last accepted report, per client. Sent before any config work, so
   bytes already served are handed over even if the node is about to be
   reconfigured or has just been emptied.
3. **Sync, only when `inSync` is false.** `GET node/config` returns the
   rendered config, the notes explaining anything left out, and the hash.
   The agent writes `config.json`, tests it with the core, restarts the
   core, and records the hash as applied. A config that fails the test is
   not kept and the node stays on what it had.

A node is shown as online if it has been heard from in the last 95 seconds.

## What the panel renders for a node

A node never receives the panel's own Xray config. It receives a config
built from scratch for that node, containing only:

- A loopback API inbound on `127.0.0.1:62790`, tag `api`, so the agent can
  talk to the core locally.
- A loopback metrics inbound on `127.0.0.1:62791`, tag `metrics`. This is
  what traffic accounting reads; without it a node cannot be billed.
- One inbound per assignment. Edge inbounds are tagged `inbound-<id>` and
  carry the inbound's protocol, settings and stream settings as configured
  in the panel, with `listen` dropped so the node binds every interface.
  The port is the remote port you set for that node, or the inbound's own
  port if you set none. Relay inbounds are tagged `relay-<id>`.
- A freedom outbound `direct` and a blackhole outbound `blocked`.
- Routing that sends the `api` and `metrics` tags to their own inbounds and
  nothing else, with `domainStrategy` of `AsIs`.
- A policy block enabling per-user and system statistics, which is what
  makes the counters the agent reads exist at all.

Everything the renderer refuses to include produces a note, visible with
the config in the panel and in the bundle the agent receives:

| Situation | What happens |
| --- | --- |
| Assigned inbound was deleted | skipped, noted |
| Assigned inbound is disabled in the panel | skipped, noted |
| Protocol is one of the derived ones (L2TP, PPTP, OpenVPN, SSTP, IKEv2, WireGuard client, MTProto, SSH) | skipped, noted - these need services the node does not run |
| Port is zero or out of range | skipped, noted |
| Two assignments want the same port, or one wants 62790 or 62791 | the later one is skipped, noted |
| Relay target does not have that inbound assigned | skipped, noted - forwarding to a port nobody listens on is worse than not forwarding |
| Stream settings name a `certificateFile` | included, noted - that path has to exist on the node too, or the core will refuse it |
| Every client of the inbound is withheld | skipped, noted - see below |

## Clients the panel withholds

Quotas and expiry are enforced on the master by removing a user from the
running core. A node has no such reconciler, so the panel does the same
work while rendering: a client is left out of a node's config when it is
disabled, past its expiry, or over its traffic limit, judged from the
client's own enable flag and its row in `client_traffics`.

Two cases deliberately stay in:

- A **negative expiry time** means a delayed start - the countdown begins
  on first connection, so the account has not expired yet.
- A **total of zero** means unlimited, not exhausted.

If withholding empties an inbound's client list entirely, the whole inbound
is skipped with a note. Xray rejects a client list of zero and would take
every other inbound on that node down with it.

The cost is worth stating plainly: a client dropping out changes the config
hash, so the node restarts its core on the next tick and live connections
there are dropped with it. Serving expired accounts indefinitely is the
worse trade.

## Traffic accounting

Bytes served by a node never pass through the master, so nothing on the
master can count them. The node counts them instead.

The agent scrapes the core's own counters from the metrics inbound
(`127.0.0.1:62791/debug/vars`) and posts the difference since its last
accepted report. The panel merges those deltas through exactly the same
`AddTraffic` path the local collector uses, which means quota, expiry and
the traffic multiplier behave identically wherever the bytes were served.

Three details that matter in practice:

- **Baselines advance only after the panel accepts a report.** A report
  lost to a network blip is re-sent whole on the next tick instead of
  vanishing.
- **A counter that moved backwards means the core restarted**, and the
  current value is taken as the delta. Subtracting a pre-restart baseline
  would silently credit the account every time a config is applied.
- **Inbound-level totals are accepted but not billed.** A relay carries
  bytes that the terminating node also counts; adding both would bill every
  relayed account twice. Per-client numbers are the authority.

A single report claiming more than 16 TiB for one client is rejected as
nonsense rather than written to the database.

Requirements: agent 0.3.0 or newer, and a config rendered by a panel new
enough to include the metrics inbound. An older node keeps working and
keeps serving - it simply reports nothing, and its users are effectively
unmetered until it is updated.

## Multi location and relay chains

Assign the same inbound to several nodes in different countries and each
node serves it on its own address, so one client account can have a config
per location. Give each location its own remote port if you want them to
look distinct, or leave the ports equal and vary only the address.

Chains are allowed: client to relay to edge, or client to relay to relay to
master. The panel refuses cycles and stops at a depth of four hops, because
every hop adds latency and a place for the chain to break.

Per-node public hosts are stored (`publicHost`) but are not yet used when
generating subscription links - that work is still open, and today links
still carry the inbound's configured address.

## Operating a node

The agent is a single static binary with no dependencies beyond the core it
supervises.

```
systemctl status sr-node
journalctl -u sr-node -f -n 200
```

Flags: `-url`, `-token`, `-dir`, `-apply`, `-core`, `-interval`,
`-insecure`, `-once`, `-version`.

- `-dir` holds `agent.json` (enrollment state), `bundle.json` (the last
  bundle received) and `config.json` (what the core is running).
- `-core` points at an Xray binary if it is not in `<dir>/bin`.
- `-apply` runs your own script instead of the built-in core handling. It
  receives `SR_NODE_BUNDLE`, `SR_NODE_CONFIG`, `SR_NODE_HASH`,
  `SR_NODE_ID`, `SR_NODE_NAME` and `SR_NODE_ROLE` in the environment, and
  is what to use if the node runs its core in a container or under another
  supervisor.
- `-once` does one cycle and exits, which is the quickest way to see what
  the agent thinks is wrong.
- `-insecure` skips TLS verification, for a panel on a self-signed
  certificate. Prefer a real certificate.

To update an agent, re-run the installer with `--version latest`. Enrollment
state survives, so no new token is needed.

## API surface

Admin endpoints, under the panel's base path, requiring panel-settings
permission - and super admin for anything that writes:

```
GET  |POST  panel/api/nodes/list
GET         panel/api/nodes/locations
GET         panel/api/nodes/inbounds/:id
POST        panel/api/nodes/save
POST        panel/api/nodes/del/:id
POST        panel/api/nodes/enable/:id
POST        panel/api/nodes/inbounds/:id
POST        panel/api/nodes/token/:id
```

The agent channel, on the same base path, authenticated by the node token
as a bearer - `Authorization: Bearer <token>`, or `X-Node-Token`, or a
`token` query parameter:

```
POST  node/enroll      enrollment token in, node token out
POST  node/heartbeat    state in, expected hash and inSync out
GET   node/config       rendered config, notes and hash
POST  node/traffic      measured per-client deltas in, accepted count out
```

Both `heartbeat` and `traffic` are deliberately forgiving about a body they
cannot parse. Answering 400 would cost that node its accounting or its
registration over a single malformed request, which is a far worse failure
than ignoring one tick.

## Troubleshooting

**`fork/exec bin/xray-linux-amd64: no such file or directory` in the node's
journal.** The node has no core. The panel binary ships one inside itself;
the agent does not.

```
bash <(curl -fsSL raw.githubusercontent.com/SRNetWork-ai/SR-PA/main/scripts/sr-ui-core.sh) --dir /etc/sr-node/bin
systemctl restart sr-node
```

**Node is Online but Out of sync.** The agent is reaching the panel but
cannot apply what it is given. Its last error is on the Nodes page and in
the journal; a config that failed its test is the usual cause, most often a
`certificateFile` path that exists on the master and not on the node.

**Node never appears after install.** The agent dials out to the panel, so
the panel's port has to be reachable from the node - check that first, then
that the URL includes the base path, then that the token was used within 30
minutes.

**Traffic from the node is not counted.** Check the agent is 0.3.0 or newer
(`sr-node -version`), then on the node:

```
curl -s 127.0.0.1:62791/debug/vars | head -c 400
```

Nothing there means the running config predates the metrics inbound - force
a re-sync by saving the node in the panel, which changes its hash.

**The core restarts every tick.** The config hash keeps changing. One
restart after a client expires or an inbound is edited is expected;
continuous restarts mean the core is rejecting the config and the agent is
retrying, so read its journal.

**Clients connect to the master but not to the node.** The node's inbound
ports have to be open in its own firewall and in its provider's, and a
remote port set for that node has to match what the client config dials.

## Security notes

- The node token is a bearer credential with no scope beyond that node.
  Minting a new enrollment token and re-enrolling replaces it.
- The agent channel lives under the panel's secret base path, so it is as
  discoverable as the panel login and no more.
- A node holds the client credentials for the inbounds assigned to it, and
  nothing else - no admin session, no database, no other node's users.
- Pinning a node's TLS fingerprint is supported on the model but not yet
  enforced at enrollment; treat `--insecure` as a temporary measure rather
  than a configuration.
