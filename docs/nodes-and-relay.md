# Nodes and relay (multi-location)

SR-UI is single-box by default: the panel writes an Xray config, runs a core
beside itself, and every account terminates on that one address.

A **node** moves the serving side somewhere else while the panel stays the only
place accounts, quotas, limits and links are edited. That is what makes
multi-location possible without running one panel per country and reconciling
them by hand.

This page documents what is implemented today, and says plainly what is not.

## Roles

| Role | Clients connect to | Traffic leaves from |
| --- | --- | --- |
| `edge` | the node | the node |
| `relay` | the node | the node named by `relayViaId` |

`relay` is the "cheap local box in front of the good foreign box" arrangement,
owned by the panel instead of by hand-written configs on two machines that drift
apart. Choosing a relay target implies the relay role; the panel normalizes the
pair rather than storing a node that says `edge` and behaves like a relay.

### Chain rules

- A node may not relay through itself.
- A chain may not contain a loop. The config builder walks the chain, so a loop
  is not a mistake you discover later - it is a walk that never returns, and two
  of your own servers forwarding to each other is a packet storm you pay for by
  the gigabyte.
- A chain may not exceed **4 hops**. Every hop is a real network leg with its own
  latency and its own way to fail.
- Deleting a node detaches anyone relaying through it: those nodes become direct
  egress and the list says so, instead of pointing at a row that no longer
  exists.

## Data model

`database/model/node.go`

### `nodes`

Columns are split by **writer**, deliberately.

- **Operator-set**, written only by the panel: `name`, `address`, `port`,
  `use_tls`, `fingerprint`, `role`, `enable`, `country_code`, `city`,
  `provider`, `tags`, `relay_via_id`, `relay_kind`, `public_host`.
- **Agent-reported**, written only by heartbeats: `status`, `last_seen`,
  `latency_ms`, `agent_version`, `core_version`, `uptime`, `cpu_pct`, `mem_pct`,
  `disk_pct`, `up`, `down`, `applied_hash`, `last_sync_at`, `last_error`.

Mixing the two is how editing a node's city blanks its health, or a heartbeat
quietly reverts an edit. `Save()` therefore writes an explicit column list and
never the whole struct.

`status` has three values and `unknown` is a real one: a node that has never
checked in has a different problem (wrong token, blocked port, agent never
installed) from one that stopped.

### `node_inbounds`

One row per (node, inbound). Assignment is explicit rather than "every node
serves everything": nodes are bought in different places for different reasons,
and pushing every inbound everywhere both wastes ports and publishes protocols
in countries where running them is the reason a server gets blocked.

`remote_port` lets one inbound listen on a different port per node, because a
port that is free (or unblocked) on one machine rarely is on all of them. `0`
means "the port the inbound already uses".

Unassigning **disables** the row instead of deleting it, so a per-node port
survives someone toggling an inbound off and on again.

### `node_enrollments`

Single-use join tokens. A node is created in the panel first and enrolls second,
so the token names the node it may become: a leaked token can only ever be that
one node, only once, and only within 30 minutes.

Tables are migrated lazily, the first time anything touches them, so a panel
that never defines a node never grows them and this feature cannot break the
boot path of existing installs.

## Sync state is a comparison, not a flag

- The master builds a node's assignment descriptor and stores its hash in
  `config_hash`.
- The agent reports the hash it is actually running in `applied_hash`.
- **In sync** means the two are equal and non-empty.

A flag can be set by the side that did not do the work; a comparison cannot. An
empty `config_hash` (nothing built yet, or the assignment just changed) reads as
out of sync rather than as agreement.

The descriptor is sorted before hashing, so the hash depends on the assignment
and not on SQLite row order - otherwise an unchanged node would drift in and out
of sync for no reason.

## Joining a node

1. Create the node in the panel (name, address, agent port, role, placement).
2. Mint a join token: `POST /panel/api/nodes/token/:id`. It is returned **once**;
   only its hash is stored.
3. On the node machine, as root:

```
bash <(curl -fsSL raw.githubusercontent.com/SRNetWork-ai/SR-PA/main/scripts/sr-node-install.sh) --url PANEL_URL --token JOIN_TOKEN
```

`PANEL_URL` must include the panel's **secret path**, because that is where the
agent routes live. A url without it fails with a 404 that the agent translates
into exactly that advice.

Enrolling again replaces the bearer token, which is also how a node is rotated
after a machine is rebuilt or a token is suspected leaked:

```
sr-node -url PANEL_URL -token NEW_JOIN_TOKEN -dir /etc/sr-node -once
systemctl restart sr-node
```

### Why tokens are only ever stored hashed

Both token kinds are stored as SHA-256 on the panel and compared by hash lookup.
A stolen panel database must not also be a working set of node credentials, and
the raw token is never compared against anything.

On the node the bearer token lives in `/etc/sr-node/agent.json`, written 0600 in
a 0700 directory, with the unit running at `UMask=0077`.

### Certificate pinning

`fingerprint` holds the SHA-256 of the agent's TLS certificate. Agents are
reached by address with self-signed certificates, where a valid chain proves
nothing and a pin proves everything - and the control channel carries account
credentials. (Stored today; enforced when the master starts dialing agents.)

## Panel API

Group: `/panel/api/nodes`, behind login and the panel-settings permission.
Reads are settings-level because fleet health is an operations view. **Every
mutation additionally requires super admin**: a node is handed account
credentials and can be pointed at any address on the internet, so creating one is
closer to handing over the panel than to editing an inbound, and it must not be
reachable through a delegated bit.

| Method | Route | Purpose |
| --- | --- | --- |
| GET, POST | `/list` | Nodes with `online`, `inSync`, `inbounds`, `relayVia`, `chain` |
| GET | `/locations` | Enabled nodes grouped by country and city |
| GET | `/inbounds/:id` | One node's inbound assignments |
| POST | `/save` | Create or update (super admin) |
| POST | `/del/:id` | Delete, detaching relays (super admin) |
| POST | `/enable/:id` | Enable or disable (super admin) |
| POST | `/inbounds/:id` | Replace the inbound assignment (super admin) |
| POST | `/token/:id` | Mint a single-use join token (super admin) |

`/save` accepts an explicit allowlist of operator fields only: `id`, `name`,
`address`, `port`, `useTls`, `fingerprint`, `role`, `enable`, `countryCode`,
`city`, `provider`, `tags`, `relayViaId`, `relayKind`, `publicHost`. Binding
straight onto the row would have been shorter and wrong - a form that can reach
`token_hash` is a form that can hand itself a node's credentials, and one that
can reach the reported columns can fake a node's health.

`/inbounds/:id` takes `{"inboundIds":[1,2]}` as JSON, or repeated `inboundIds`
form fields. `/token/:id` returns `token`, `expiresIn`, `host`, `basePath`,
`enrollPath` and `tls` rather than a finished URL: the panel cannot know whether
it is reached over TLS, through a reverse proxy or by bare address, so the
caller with that context assembles the install line.

Node names are restricted to letters, digits, dot, dash and underscore. The name
reaches a systemd unit name, a config filename and a link remark, so it is
restricted at the door rather than escaped in four places later.

## Agent channel

Mounted on the panel's **root** group, not inside `/panel/api`, and therefore
reached at `<base_path>/node/...`. An agent holds no session, so passing it
through the panel's login middleware would 404 every heartbeat; it presents its
bearer token instead, as `Authorization: Bearer <token>`, `X-Node-Token`, or a
`token` query parameter.

The routes still sit behind the panel's secret base path, which is why an agent
is handed a full URL at enrollment and why nothing scanning the internet finds
them.

| Method | Route | Auth | Returns |
| --- | --- | --- | --- |
| POST | `/node/enroll` | join token | `nodeToken`, node identity, `heartbeatSeconds` |
| POST | `/node/heartbeat` | bearer | `configHash`, `inSync`, `serverTime` |
| GET | `/node/config` | bearer | the assignment bundle |

Heartbeat body: `agentVersion`, `coreVersion`, `appliedHash`, `uptime`,
`cpuPct`, `memPct`, `diskPct`, `up`, `down`, `latencyMs`, `error`. Everything in
it is a claim by the far side, so strings are bounded and percentages clamped -
a bad agent reading must not make the dashboard lie.

The heartbeat answers with the hash the node *should* be running, so an agent
learns it is behind on the same call it uses to say it is alive, rather than
polling a second endpoint to find out.

A bad bearer token is refused with **401 and a reason**, not the 404 the panel
API uses to hide itself. The caller is a daemon on the operator's own machine,
and "your token is no longer valid, enroll again" is the difference between a
node that reports its own problem and one that is silently missing for a week.

A node disabled in the panel is refused at authentication, so disabling it
actually cuts the control channel instead of only hiding the row.

### Bundle shape

```json
{
  "nodeId": 3,
  "node": "de-fra-1",
  "role": "relay",
  "relayVia": "nl-ams-1",
  "relayViaAddress": "203.0.113.7",
  "relayChain": ["de-fra-1", "nl-ams-1"],
  "inbounds": [{ "inboundId": 7, "remotePort": 24763 }],
  "hash": "..."
}
```

The bundle is an **assignment**, not a finished Xray config. The panel's inbound
records already own protocol settings, TLS material and client lists;
duplicating them into a per-node snapshot would create a second copy that
drifts. `relayViaAddress` is the next hop's identity, not a dialable target: the
port a relay forwards to belongs to the inbound it forwards.

## The agent

`cmd/sr-node`, shipped as `sr-node-amd64` and `sr-node-arm64` on every release.
It is built with CGO off, so the same static binary runs on musl, on an old
glibc, or in a minimal image without a toolchain.

| Flag | Meaning |
| --- | --- |
| `-url` | panel base url, including the secret path |
| `-token` | join token; enrolls, and re-enrolls when given again |
| `-dir` | state directory (default `/etc/sr-node`) |
| `-apply` | program run after a new config is stored |
| `-core` | xray binary, read only to report its version |
| `-interval` | seconds between heartbeats, overriding the panel |
| `-insecure` | accept a self-signed panel certificate |
| `-once` | one cycle and exit; how the installer proves the channel works |

State lives in two files: `agent.json` (panel url, bearer token, node identity,
applied hash) and `bundle.json` (the last assignment received). Both are written
by rename, never in place, so a crash or a full disk cannot leave half a
credential behind.

Nothing inside a cycle is fatal. A panel that is restarting, a network that
drops for a minute, or a config that fails to apply are all retried on the next
tick; exiting would turn a transient failure into an outage.

### The apply hook

The agent does not yet know how to turn an assignment into a running Xray
config. Rather than pretend, it stores the bundle and runs a hook if one exists:
`-apply PATH`, or an executable `apply.sh` in the state directory.

The hook receives the bundle path as its first argument and in the environment:

| Variable | Value |
| --- | --- |
| `SR_NODE_BUNDLE` | path to the stored bundle |
| `SR_NODE_HASH` | the assignment's hash |
| `SR_NODE_ID` | node id on the panel |
| `SR_NODE_NAME` | node name |
| `SR_NODE_ROLE` | `edge` or `relay` |

**The applied hash only advances when the hook exits 0.** With no hook, the node
keeps reporting out of sync with the reason attached, and the panel shows it.
That is accurate: the assignment arrived, and nothing on that machine is serving
it. A fleet page that says "in sync" while no traffic can flow is worse than one
that admits what it does not know.

## Liveness

Agents beat every 30 seconds and a node stays `online` for **95 seconds** after
its last heartbeat, so two consecutive misses are tolerated. A health column
that flickers on one lost packet is a column operators learn to ignore, which is
worse than not having one. `NodeService.MarkStale()` turns silence into
`offline`.

## Not built yet

Stated plainly so this page is not read as a promise:

- **Config generation.** The panel hands out an assignment; rendering it into
  the node's Xray config, and the built-in apply step that replaces the hook, is
  the next piece.
- **The Nodes page in the panel UI.** The API above is complete and usable with
  a session cookie in the meantime.
- **Per-node traffic attribution.** `up` and `down` are reported totals; folding
  them into per-account accounting comes with the traffic job work.
- **Node-scoped subscription links.** `public_host` is stored and not yet used
  by link generation.
- **Agent certificate pinning.** `fingerprint` is stored but not enforced.
