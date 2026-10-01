# SR-UI panel API

The panel's own HTTP API is the one the web UI uses. There is no separate,
reduced "public API": anything the panel can do, a program can do, with the same
permission checks applied to both.

Two ways to authenticate:

| | Used by | Credential |
|---|---|---|
| Session | The web UI | signed cookie, 6h |
| API token | Scripts, billing systems, monitors, other panels | bearer token |

---

## 1. The one thing to understand about API tokens

**A token is not a set of rights. It is a narrowed copy of the admin who minted
it, re-derived on every single request.**

Everything else in this document follows from that sentence:

- A token's effective rights are `owner's current rights` intersected with
  `the token's scope`. Not the rights the owner had when the token was created.
- Narrow an admin's permissions and **their tokens narrow with them, instantly** -
  no rotation, no cleanup list, nothing left holding rights the account lost.
- Disable or delete the admin and **their tokens stop working**. There is no such
  thing here as a credential that outlives the person responsible for it.
- Super admin is **not** inherited. It must be asked for explicitly at mint time
  (`superAdmin: true`), and only a super admin may ask. Without it, a token for
  the panel owner is limited to exactly the scopes written on it.
- **Reseller accounts cannot mint tokens at all.** A reseller's rights come from
  their role, not from a stored permission mask, so a scope written on such a
  token would be stored, displayed, and enforce nothing. A limit that cannot be
  enforced is refused instead of offered.

The panel stores only a SHA-256 hash of each token plus a 4+4 character hint
(`a1b2..9f8e`) to tell them apart in a list. **The token itself is shown exactly
once**, in the response that creates it. It cannot be recovered - only replaced.

---

## 2. Sending a token

All API routes live **under the panel's secret base path**, the same random
segment in the URL you log in with. A token alone is not enough to find them.

```sh
SCHEME=https                      # or http, if you have not set up TLS yet
HOST=panel.example.com:2053
BASE=$SCHEME://$HOST/yourSecretPath
TOKEN=paste-the-token-you-were-shown-once

curl -sS -H "Authorization: Bearer $TOKEN" "$BASE/panel/api/nodes/list"
```

Three transports are accepted, in this order:

1. `Authorization: Bearer <token>` - **use this one.**
2. `X-API-Token: <token>` - for clients that reserve `Authorization`.
3. `?apiToken=<token>` - a concession for tools that cannot set headers. It lands
   in access logs, proxy logs and shell history. Avoid it where you have a
   choice.

### Reading the response

Every endpoint answers with the same envelope:

```json
{ "success": true, "msg": "", "obj": { } }
```

| Status | Meaning |
|---|---|
| `200` + `success: true` | done |
| `200` + `success: false` | understood and refused - usually a permission the token does not hold. `msg` says which. |
| `401` | the token itself was rejected: unknown, disabled, expired, or its admin is gone. `msg` says which. |
| `404` | **no credential was presented at all.** The panel hides its API from unauthenticated callers rather than confirming an endpoint exists. A 404 where you expected 401 almost always means your header never arrived. |

---

## 3. Scopes

A scope is one of the panel's permission slugs - the same list the **Admins**
page edits. There is no second vocabulary invented for the API.

| Slug | Grants |
|---|---|
| `accessInbounds` | read inbounds, clients, devices, address intelligence |
| `createInbound` / `editInbound` / `deleteInbound` | inbound lifecycle |
| `createClient` / `editClient` / `deleteClient` | account lifecycle |
| `bulkOperation` | the bulk client routes |
| `accessCoreSettings` | core settings |
| `accessXraySettings` | Xray config, routing, outbounds, custom geo |
| `accessPanelSettings` | panel settings, **and the Nodes fleet view** |
| `manageResellers` | the resellers subsystem |
| `accessOverview` / `manageOverview` | the host overview page, and acting on it |

Fetch the live list instead of hardcoding it:

```sh
curl -sS -H "Authorization: Bearer $TOKEN" "$BASE/panel/api/tokens/scopes"
```

Some routes ask for **super admin** rather than a slug: creating or deleting
nodes, DB export/import, the panel update, the systemd unit. Those need a token
minted with `superAdmin: true`, by a super admin. Issue them rarely and give them
an expiry.

---

## 4. Managing tokens

These routes are **super admin only**, and are **refused to token-authenticated
callers** even when the token holds super admin. A credential that can mint
credentials escapes its own scope in one call, so minting requires a real panel
login.

| Route | Method | Does |
|---|---|---|
| `/panel/api/tokens/list` | GET, POST | every token: name, hint, owner, scopes, expiry, last use |
| `/panel/api/tokens/scopes` | GET | the slug vocabulary |
| `/panel/api/tokens/mint` | POST | issue a token, returned once |
| `/panel/api/tokens/enable/:id` | POST | `{"enable": false}` - switch off, keep the record |
| `/panel/api/tokens/del/:id` | POST | delete permanently |

### Minting

```json
{
  "name": "billing sync",
  "scopes": ["accessInbounds", "createClient", "editClient"],
  "superAdmin": false,
  "ttlDays": 90
}
```

- `name` is required. It is how you recognise a credential a year later, when
  deciding whether revoking it will break something.
- `ttlDays` omitted or `0` means **never expires**. Allowed, because some
  integrations genuinely outlive any date you would pick - but prefer an expiry.
- The owner is **the admin making the request**. There is no field for choosing
  someone else: that would be an endpoint for minting other people's credentials,
  and the audit trail would name the wrong person.

The reply carries the token once:

```json
{
  "success": true,
  "obj": {
    "id": 3,
    "name": "billing sync",
    "hint": "a1b2..9f8e",
    "token": "…64 hex characters…",
    "header": "Authorization: Bearer …",
    "scopes": ["accessInbounds", "createClient", "editClient"]
  }
}
```

### Revoking

Prefer `enable/:id` with `{"enable": false}` during an incident. Deleting a token
also deletes the record of when it was last used and from which address - which
is exactly the evidence you want while working out what the leaked credential
touched. Delete it afterwards.

Every token row carries `lastUsedAt` and `lastUsedFrom`. These are written at
most once a minute per token: a monitor polling every five seconds would
otherwise write to the shared panel database on every single call, and a minute
of imprecision in an audit column is worth far less than that.

---

## 5. Endpoint map

All paths are relative to `$BASE`. Permission shown is what a token must hold.

### Nodes - `accessPanelSettings`, mutations need super admin

| Route | Method | Does |
|---|---|---|
| `/panel/api/nodes/list` | GET, POST | the fleet: status, agent and core version, load, traffic, sync state, last error |
| `/panel/api/nodes/locations` | GET | nodes grouped by country and city, for multi-location views |
| `/panel/api/nodes/inbounds/:id` | GET | which inbounds a node serves |
| `/panel/api/nodes/preview/:id` | GET, POST | the exact Xray config the panel would hand this node, with its hash. Read-only, and the fastest way to answer "why is this node serving that" |
| `/panel/api/nodes/save` | POST | create or update a node |
| `/panel/api/nodes/inbounds/:id` | POST | assign inbounds to a node |
| `/panel/api/nodes/token/:id` | POST | mint a 30-minute enrollment token for an agent |
| `/panel/api/nodes/enable/:id` | POST | enable or disable a node |
| `/panel/api/nodes/del/:id` | POST | delete a node |

See [nodes-and-relay.md](nodes-and-relay.md) for the subsystem itself.

### Accounts and inbounds - `accessInbounds` to read

| Route | Does |
|---|---|
| `/panel/api/inbounds/...` | the full inbound and client surface the Inbounds page uses |
| `/panel/api/clients/...` | the account-centric read model behind the Clients page |
| `/panel/api/devices/list/:email` | devices seen for one account - additionally scoped to accounts the caller owns |
| `/panel/api/ipintel/resolve` | annotate source addresses: network, carrier, country |

See [ip-and-device-intelligence.md](ip-and-device-intelligence.md).

### Host and core

| Route | Permission |
|---|---|
| `/panel/api/server/...` | per-route; host status, core control, logs |
| `/panel/api/custom-geo/...` | `accessXraySettings` or `manageOverview` |
| `/panel/api/backuptotgbot` | `manageOverview` - **mails the whole database**, including password hashes, to a Telegram chat |

---

## 6. Not an API token: the node agent channel

`POST <base>/node/enroll`, `POST <base>/node/heartbeat`, `GET <base>/node/config`
and `POST <base>/node/traffic` are the node agents' own channel. They sit outside
`/panel/api` and use **node tokens**, issued per node by `nodes/token/:id`.

A node token authenticates *a machine* and reaches only those four routes. An API
token authenticates *on behalf of an admin*. They are not interchangeable, and
neither one can be used in the other's place.

---

## 7. Operating advice

- **One token per integration**, named after it. Shared tokens cannot be revoked
  without an outage nobody can predict the shape of.
- **Mint the smallest scope that works.** A billing sync usually needs
  `accessInbounds` + `createClient` + `editClient` and nothing else.
- **Give a token an owner who should have those rights**, not the super admin
  account. The intersection is the safety net; do not start it wide.
- **Set `ttlDays`** on anything you can plausibly rotate.
- Tokens cross the network in a header. Put TLS in front of the panel before
  issuing any - over plain HTTP a bearer token is readable by every hop.
- A `404` from an API call means no credential arrived. Check the header name
  before suspecting the token.
