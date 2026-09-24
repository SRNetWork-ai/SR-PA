package model

// Node is one machine other than the panel's own host that serves this panel's
// inbounds.
//
// The panel is single-box by default: it writes an Xray config, runs a core
// beside itself, and every account terminates on that one address. A node moves
// the serving side elsewhere while the panel stays the only place accounts,
// quotas, limits and links are edited. That is what makes multi-location
// possible without running a second panel per country and reconciling them by
// hand.
//
// Two roles, and they are not the same thing:
//
//	edge   clients connect here and traffic leaves from here.
//	relay  clients connect here and traffic is forwarded to RelayViaId, and
//	       leaves from there. This is the "cheap local box in front of the good
//	       foreign box" arrangement, owned by the panel instead of by
//	       hand-written configs on two machines that drift apart.
//
// Columns are split by WRITER, deliberately. Everything an operator sets
// (Address, Role, RelayViaId, placement) is written only by the panel, and
// everything an agent reports (Status, LastSeen, versions, counters,
// AppliedHash) is written only by heartbeats. Mixing the two is how editing a
// node's city blanks its health, or a heartbeat quietly reverts an edit.
type Node struct {
	Id   int    `json:"id" gorm:"primaryKey;autoIncrement"`
	Name string `json:"name" gorm:"uniqueIndex"`

	// Where the master reaches the agent. An IPv6 literal is stored bare and
	// bracketed only when a URL is built, so it still compares equal to what the
	// agent reports and to what the operator typed.
	Address string `json:"address"`
	Port    int    `json:"port"`
	UseTLS  bool   `json:"useTls" gorm:"column:use_tls"`

	// Fingerprint is the SHA-256 of the agent's TLS certificate, pinned when the
	// node enrolls. The control channel carries account credentials, and agents
	// are reached by address with self-signed certificates, where a valid chain
	// proves nothing and a pin proves everything.
	Fingerprint string `json:"fingerprint"`

	// TokenHash is the SHA-256 of the bearer token the agent presents. The token
	// itself is shown once, at enrollment, and never stored: a stolen panel
	// database must not also be a working set of node credentials.
	TokenHash string `json:"-" gorm:"column:token_hash;index"`
	TokenHint string `json:"tokenHint" gorm:"column:token_hint"`

	Role   string `json:"role"`
	Enable bool   `json:"enable"`

	// Placement. Country and city are what the location picker groups by, and
	// what a subscription can be filtered on, so they are columns rather than
	// something parsed back out of the node's name.
	CountryCode string `json:"countryCode"`
	City        string `json:"city"`
	Provider    string `json:"provider"`
	Tags        string `json:"tags"`

	// RelayViaId is the node this one forwards to; 0 means it egresses itself.
	RelayViaId int    `json:"relayViaId" gorm:"column:relay_via_id"`
	RelayKind  string `json:"relayKind" gorm:"column:relay_kind"`

	// PublicHost is what goes into client links for inbounds served here, when
	// the address clients should use is not the address the panel uses: a domain
	// pointed at this node, a CDN name, a clean-IP front.
	PublicHost string `json:"publicHost"`

	// Reported by the agent, never by the operator.
	Status       string  `json:"status"`
	LastSeen     int64   `json:"lastSeen" gorm:"index"`
	LatencyMs    int     `json:"latencyMs" gorm:"column:latency_ms"`
	AgentVersion string  `json:"agentVersion"`
	CoreVersion  string  `json:"coreVersion"`
	Uptime       int64   `json:"uptime"`
	CpuPct       float64 `json:"cpuPct" gorm:"column:cpu_pct"`
	MemPct       float64 `json:"memPct" gorm:"column:mem_pct"`
	DiskPct      float64 `json:"diskPct" gorm:"column:disk_pct"`
	Up           int64   `json:"up"`
	Down         int64   `json:"down"`

	// ConfigHash is what the master last built for this node; AppliedHash is what
	// the agent says it is running. Equal means in sync. That is a comparison
	// rather than a flag on purpose: a flag can be set by the side that did not
	// do the work.
	ConfigHash  string `json:"configHash"`
	AppliedHash string `json:"appliedHash"`
	LastSyncAt  int64  `json:"lastSyncAt"`
	LastError   string `json:"lastError"`

	CreatedAt int64 `json:"createdAt"`
	UpdatedAt int64 `json:"updatedAt"`
}

// Node roles.
const (
	NodeRoleEdge  = "edge"
	NodeRoleRelay = "relay"
)

// Node health as the panel reports it. "unknown" is a real state and not a
// synonym for offline: a node that has never checked in has a different problem
// (wrong token, blocked port, agent never installed) from one that stopped.
const (
	NodeStatusUnknown = "unknown"
	NodeStatusOnline  = "online"
	NodeStatusOffline = "offline"
)

// NodeInbound assigns one inbound to one node.
//
// Assignment is explicit rather than "every node serves everything". Nodes are
// bought in different places for different reasons, and pushing every inbound
// everywhere both wastes ports and publishes protocols in countries where
// running them is the reason a server gets blocked.
type NodeInbound struct {
	Id        int `json:"id" gorm:"primaryKey;autoIncrement"`
	NodeId    int `json:"nodeId" gorm:"uniqueIndex:idx_node_inbound,priority:1"`
	InboundId int `json:"inboundId" gorm:"uniqueIndex:idx_node_inbound,priority:2;index"`

	// RemotePort lets one inbound listen on a different port per node, because a
	// port that is free (or unblocked) on one machine rarely is on all of them.
	// 0 means "the same port the inbound already uses".
	RemotePort int  `json:"remotePort"`
	Enable     bool `json:"enable"`

	AppliedHash string `json:"appliedHash"`
	LastSyncAt  int64  `json:"lastSyncAt"`
}

// NodeEnrollment is a single-use join token.
//
// A node is created in the panel first and enrolls second, so the token names
// the node it may become: a leaked token can then only ever be that one node,
// only once, and only before it expires. Only the hash is stored, for the same
// reason the node's bearer token is only hashed.
type NodeEnrollment struct {
	Id        int    `json:"id" gorm:"primaryKey;autoIncrement"`
	TokenHash string `json:"-" gorm:"column:token_hash;uniqueIndex"`
	TokenHint string `json:"tokenHint" gorm:"column:token_hint"`
	NodeId    int    `json:"nodeId" gorm:"index"`
	NodeName  string `json:"nodeName"`
	CreatedAt int64  `json:"createdAt"`
	CreatedBy string `json:"createdBy"`
	ExpiresAt int64  `json:"expiresAt"`
	UsedAt    int64  `json:"usedAt"`
	UsedFrom  string `json:"usedFrom"`
}
