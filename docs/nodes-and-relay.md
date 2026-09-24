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

## Enrollment flow

1. Create the node in the panel (name, address, agent port, role, placement).
2. Mint a join token: `POST /panel/api/nodes/token/:id`. It is returned **once**;
   only its hash is stored.
3. On the node machine, the agent posts the token to `POST <base>/node/enroll`
   and receives its long-lived bearer token.
4. The agent posts `POST <base>/node/heartbeat` every 30 seconds and pulls
   `GET <base>/node/config` whenever the hash it is told about differs from the
   one it applied.

Enrolling again replaces the bearer token, which is also how a node is rotated
after a machine is rebuilt or a token is suspected leaked.

### Why tokens are only ever stored hashed

Both token kinds are stored as SHA-256 and compared by hash lookup. A stolen
panel database must not also be a working set of node credentials, and the raw
token is never compared against anything.

### Certificate pinning

`fingerprint` holds the SHA-256 of the agent's TLS certificate. Agents are
reached by address with self-signed certificates, where a valid chain proves
nothing and a pin proves everything - and the control channel carries account
credentials.

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

## Liveness

Agents beat every 30 seconds and a node stays `online` for **95 seconds** after
its last heartbeat, so two consecutive misses are tolerated. A health column
that flickers on one lost packet is a column operators learn to ignore, which is
worse than not having one. `NodeService.MarkStale()` turns silence into
`offline`.

## Not built yet

Stated plainly so this page is not read as a promise:

- **Per-inbound config generation on the node.** The bundle carries the
  assignment; turning it into the node's Xray config and applying it is the next
  piece.
- **The agent binary and its installer.** The channel it will speak is fixed by
  this page.
- **The Nodes page in the panel UI.** The API above is complete and usable with
  a session cookie in the meantime.
- **Per-node traffic attribution.** `up` and `down` are reported totals; folding
  them into per-account accounting comes with the traffic job work.
- **Node-scoped subscription links.** `public_host` is stored and not yet used
  by link generation.
