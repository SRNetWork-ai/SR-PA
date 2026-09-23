# Address intelligence and the device registry

How SR-UI answers two questions that the address log alone cannot:

- **What is this address?** (`web/service/ipintel.go`)
- **How many devices is this account actually using?** (`web/service/devicesessions.go`)

## Why addresses are not devices

The panel used to store a flat list of source addresses per account and compare its
length to a cap. On the networks these accounts run on, that number is not an
approximation of the device count - it is unrelated to it:

| Situation | Addresses | Devices | What an address cap does |
| --- | --- | --- | --- |
| Phone on a mobile carrier | many per day | 1 | throttles an ordinary customer |
| Several subscribers behind CGNAT | 1 | many | sees nothing |
| Home router with phone, laptop, TV | 1 | 3 | sees nothing |
| Credential resold to 5 cities | 5 | 5 | correct, by luck |

So SR-UI records a device identity and treats the address as one of its attributes.

## Address intelligence

`LookupIPIntel(ip)` and `IPIntelBatch(ips)` return, for each address:

| Field | Meaning |
| --- | --- |
| `family` | `ipv4` / `ipv6` |
| `scope` | `public`, `private`, `cgnat` (100.64.0.0/10), `loopback`, `linklocal`, `invalid` |
| `reverse` | PTR name, when the address has one |
| `carrier`, `carrierKind` | operator name and `mobile` / `fixed` / `hosting` / `carrier-nat` |
| `asn`, `org`, `country`, `countryCode`, `city` | only when enrichment is enabled |
| `mobile`, `hosting`, `proxy` | booleans from the enrichment source |
| `source` | `local`, `ptr`, or `endpoint` - where the answer came from |

**Everything except the ASN/geo row works offline.** Scope is computed locally, PTR uses
the system resolver, and carrier matching uses a built-in table of Iranian mobile and
fixed operators (MCI, Irancell, Rightel, Shatel, ParsOnline, Asiatech, TCI and others)
plus the common hosting networks, so a datacenter address is labelled as one.

Results are cached for 12 hours (20 minutes for failures), every lookup has a hard
deadline, and a batch resolves in parallel. Nothing in the panel blocks on a lookup.

### Optional enrichment

Set `SRUI_IPINTEL_ENDPOINT` to a URL template containing `{ip}` to add ASN, org and geo.
Disabled by default: the panel makes no outbound request about your customers' addresses
unless you configure one. Both ip-api-style and ipinfo-style JSON responses are accepted.

## Device registry

Each sighting is reduced to a **device identity**, strongest signal first:

| Order | Identity | `kind` | Confidence |
| --- | --- | --- | --- |
| 1 | client fingerprint (SSH version banner, OpenVPN peer-info, RADIUS calling-station-id) | `fingerprint` | survives address changes |
| 2 | mobile carrier + protocol | `sim` | one subscriber, unstable address |
| 3 | CGNAT bucket + protocol | `shared-nat` | possibly several subscribers |
| 4 | the address | `address` | nothing better was available |

The `kind` is reported to the UI so the panel can state its confidence instead of
presenting a guess as a fact. Rule 2 is what stops a roaming phone from reading as ten
devices; rule 3 is what stops CGNAT neighbours from being silently merged into one
"confirmed" device.

Per account the registry keeps one row per device with: label, protocol, inbound, node,
carrier, country, last address, a bounded address history (12), first/last seen and an
observation count.

| Limit | Value | Why |
| --- | --- | --- |
| Active window | 30 min | matches the address log, so two screens cannot disagree |
| Retention | 14 days | sharing shows up in days; history beyond that only grows the table |
| Addresses per device | 12 | bounded history |
| Devices per account | 64 | a shared credential must not be able to fill the disk |

Retention is pruned on the hourly access-log rotation. The table is created on first use,
so no migration step is required when upgrading.

### Data sources

| Source | Status | Supplies |
| --- | --- | --- |
| Xray access log scan | shipped | account, address, timestamp |
| SSH gateway | shipped | client version banner, a real per-client fingerprint |
| OpenVPN | planned | `IV_PLAT` / `IV_GUI_VER` peer-info |
| RADIUS (L2TP, PPTP, IKEv2) | planned | calling-station-id, which is a hardware address |

The access log carries no client identity, so those sightings are identified by carrier or
address and labelled accordingly. Only the protocols that genuinely expose a client
identity report a `fingerprint` kind.

## The SSH User Limit counts devices

The SSH gateway is the first place where device identity is **enforced**, not just
recorded, because the handshake hands it a client identity for free.

A device there is `sshDeviceKey`: the sanitised version banner within an address pool
(/24 for IPv4, /64 for IPv6), falling back to the bare address when a client sends no
usable banner - which is exactly the old behaviour, so unidentified clients are not
quietly given a looser limit.

| Case | Before | Now |
| --- | --- | --- |
| Phone re-dials onto a neighbouring carrier address | 2 devices, customer refused | 1 device |
| Laptop and phone behind one home address | 1 device | 2 devices |
| Phone jumps to a different carrier range | 2 devices | 2 devices (known gap) |
| Two people sharing an account from one pool, same client app | 2 devices | 1 device (known gap) |

Both gaps need per-address carrier data at admission time, and that is a lookup the
connection path cannot block on. The banner is client-supplied and forgeable; it is used
only for counting and labelling, never for authentication.

Eviction is per device: when the strategy is `accept`, the oldest device loses **all** of
its sessions, not one of them.

## API

All routes sit under the authenticated panel API and return the panel's standard
`{ success, msg, obj }` envelope.

| Method | Path | Permission | Returns |
| --- | --- | --- | --- |
| GET | `/panel/api/ipintel/status` | accessInbounds | whether enrichment is configured, and the field list |
| POST | `/panel/api/ipintel/resolve` | accessInbounds | intelligence for up to 256 addresses |
| GET, POST | `/panel/api/devices/list/:email` | accessInbounds + client ownership | the account's devices, newest first |
| POST | `/panel/api/devices/forget/:email` | editClient + client ownership | clears that account's device history |

`/ipintel` takes addresses in and gives annotations back; it holds no account data, which
is why it needs no ownership check. The `/devices` routes return one account's data and
therefore carry the same ownership middleware as `clientIps/:email`.

`resolve` accepts `{"ips": ["..."]}`, repeated form fields, or a comma-separated list.

`list` responds with `devices`, `total` (history) and `active` (seen inside the window).
A device cap should be compared against `active`; comparing against `total` is how an
operator disconnects a customer over a phone last seen a week ago.

## Current limits

- Enforcement is device-based **on the SSH gateway only**. For Xray protocols the
  enforced cap is still the per-account address count published to the patched core
  through `speedlimits.json`; the registry is reporting there, not deciding.
- Access-log sightings depend on Xray's access log being enabled; the shipped template
  sets it to `none`.
- Carrier names come from a built-in table plus PTR matching. Without enrichment, an
  operator with no PTR and no table entry is reported as unknown rather than guessed.
- The panel UI does not yet render the registry; the data is reachable through the API
  above.
