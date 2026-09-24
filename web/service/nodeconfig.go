package service

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/mhsanaei/3x-ui/v2/database/model"
)

// A node runs a stock Xray and nothing else. Everything it does is decided
// here, on the master, and handed over as a finished config.
//
// The alternative - shipping the assignment and letting the agent build the
// config - was tried first and is why this file exists. Two sides generating
// configs from the same rules is two implementations to keep in step, and the
// one on the node is the one nobody can see when it drifts. Rendering here also
// makes the sync question answerable: the master hashes what it rendered, the
// agent reports the hash it applied, and anything other than equality is a
// problem the Nodes page can show.

const (
	// nodeApiPort is where a node's Xray exposes its gRPC API, bound to loopback.
	// Fixed rather than configurable: only the agent on that host ever dials it,
	// and a per-node setting would be one more thing to get wrong for no gain.
	nodeApiPort = 62790

	// NodeMetricsPort is where the same Xray publishes its counters as expvar
	// JSON, also on loopback.
	//
	// The gRPC API above can already report traffic, and on the panel's own host
	// that is exactly how it is read. It is the wrong tool on a node: a gRPC
	// client means importing the core packages, and importing those into the
	// agent means shipping the 160 MB embedded core to every edge box to move a
	// few counters. One HTTP GET and encoding/json cost nothing.
	NodeMetricsPort = 62791

	// NodeMasterAddress stands in for "the panel itself" as a relay's egress.
	//
	// The master genuinely cannot know the address a node reaches it on - that is
	// a question about DNS, NAT and which interface the node uses - but the agent
	// knows it exactly, because it is already talking to the panel. So a relay
	// with no next hop gets this token and the agent substitutes its own panel
	// host. This is the common case, not an exotic one: a cheap local box in
	// front of the server that already works.
	NodeMasterAddress = "@master"
)

var (
	nodeLogConfig       = json.RawMessage(`{"loglevel":"warning"}`)
	nodeStatsConfig     = json.RawMessage(`{}`)
	nodeApiConfig       = json.RawMessage(`{"tag":"api","services":["HandlerService","StatsService","LoggerService"]}`)
	nodeApiSettings     = json.RawMessage(`{"address":"127.0.0.1"}`)
	nodeMetricsConfig   = json.RawMessage(`{"tag":"metrics"}`)
	nodeOutboundsConfig = json.RawMessage(`[{"protocol":"freedom","tag":"direct"},{"protocol":"blackhole","tag":"blocked"}]`)
	nodeRoutingConfig   = json.RawMessage(`{"domainStrategy":"AsIs","rules":[{"type":"field","inboundTag":["api"],"outboundTag":"api"},{"type":"field","inboundTag":["metrics"],"outboundTag":"metrics"}]}`)

	// Per-user counters are on from the first byte a node forwards. Turning them
	// on later would mean a restart, and a restart means dropping every live
	// connection to start counting traffic that was already flowing.
	nodePolicyConfig = json.RawMessage(`{"levels":{"0":{"statsUserUplink":true,"statsUserDownlink":true}},"system":{"statsInboundUplink":true,"statsInboundDownlink":true,"statsOutboundUplink":true,"statsOutboundDownlink":true}}`)
)

type nodeXrayInbound struct {
	Listen         string          `json:"listen,omitempty"`
	Port           int             `json:"port"`
	Protocol       string          `json:"protocol"`
	Settings       json.RawMessage `json:"settings,omitempty"`
	StreamSettings json.RawMessage `json:"streamSettings,omitempty"`
	Tag            string          `json:"tag"`
}

type nodeXrayConfig struct {
	Log       json.RawMessage   `json:"log"`
	API       json.RawMessage   `json:"api"`
	Metrics   json.RawMessage   `json:"metrics"`
	Stats     json.RawMessage   `json:"stats"`
	Policy    json.RawMessage   `json:"policy"`
	Inbounds  []nodeXrayInbound `json:"inbounds"`
	Outbounds json.RawMessage   `json:"outbounds"`
	Routing   json.RawMessage   `json:"routing"`
}

type nodeForwardSettings struct {
	Address        string `json:"address"`
	Port           int    `json:"port"`
	Network        string `json:"network"`
	FollowRedirect bool   `json:"followRedirect"`
}

// XrayConfig renders the config for one node, plus the list of things the
// operator should know about it.
//
// The notes are not logging. Every inbound this refuses to put on a node is an
// inbound somebody assigned and expects to see working, so the reason travels
// with the config to the Nodes page instead of dying in a log file: "disabled
// in the panel", "not assigned to the next hop", "the panel runs that protocol
// itself". A silent omission here looks exactly like a broken node.
func (s *NodeService) XrayConfig(nodeId int) (json.RawMessage, []string, error) {
	ensureNodeTables()

	n, err := s.Get(nodeId)
	if err != nil {
		return nil, nil, err
	}
	links, err := s.Inbounds(n.Id)
	if err != nil {
		return nil, nil, err
	}

	notes := make([]string, 0, 4)

	// Where traffic goes when this node is not the end of the line.
	forwarding := false
	hopAddress := ""
	hopPorts := map[int]int{}
	hopAssigned := map[int]bool{}

	if n.RelayViaId > 0 {
		via, viaErr := s.Get(n.RelayViaId)
		switch {
		case viaErr != nil || via == nil:
			notes = append(notes, "the node this one relays through no longer exists; pick a new next hop")
		case strings.TrimSpace(via.Address) == "":
			notes = append(notes, "next hop "+via.Name+" has no address, so there is nowhere to forward to")
		default:
			forwarding = true
			hopAddress = strings.TrimSpace(via.Address)
			viaLinks, linkErr := s.Inbounds(via.Id)
			if linkErr != nil {
				return nil, nil, linkErr
			}
			for _, link := range viaLinks {
				if !link.Enable {
					continue
				}
				hopAssigned[link.InboundId] = true
				hopPorts[link.InboundId] = link.RemotePort
			}
		}
	} else if n.Role == model.NodeRoleRelay {
		// A relay with no next hop relays to the panel's own server, which is the
		// whole shape of "local box in front, real server behind".
		forwarding = true
		hopAddress = NodeMasterAddress
	}

	cfg := nodeXrayConfig{
		Log:       nodeLogConfig,
		API:       nodeApiConfig,
		Metrics:   nodeMetricsConfig,
		Stats:     nodeStatsConfig,
		Policy:    nodePolicyConfig,
		Outbounds: nodeOutboundsConfig,
		Routing:   nodeRoutingConfig,
		Inbounds: []nodeXrayInbound{{
			Listen:   "127.0.0.1",
			Port:     nodeApiPort,
			Protocol: "dokodemo-door",
			Settings: nodeApiSettings,
			Tag:      "api",
		}, {
			Listen:   "127.0.0.1",
			Port:     NodeMetricsPort,
			Protocol: "dokodemo-door",
			Settings: nodeApiSettings,
			Tag:      "metrics",
		}},
	}

	ids := make([]int, 0, len(links))
	localPort := map[int]int{}
	for _, link := range links {
		if !link.Enable {
			continue
		}
		ids = append(ids, link.InboundId)
		localPort[link.InboundId] = link.RemotePort
	}
	// Sorted so the rendered config, and therefore its hash, depends on the
	// assignment rather than on the order SQLite felt like returning rows in.
	sort.Ints(ids)

	inbounds := InboundService{}

	// Seeded with the agent's own two sockets. They are bound to 127.0.0.1 while
	// an inbound binds every interface, and the kernel refuses the second bind
	// either way - which Xray reports by refusing the config outright, taking
	// every other inbound on the node down with it.
	taken := map[int]int{nodeApiPort: 0, NodeMetricsPort: 0}

	for _, id := range ids {
		inbound, inboundErr := inbounds.GetInbound(id)
		if inboundErr != nil || inbound == nil {
			notes = append(notes, "inbound #"+strconv.Itoa(id)+" is assigned to this node but no longer exists")
			continue
		}
		if !inbound.Enable {
			notes = append(notes, inboundLabel(inbound)+" is disabled in the panel, so the node does not open it")
			continue
		}
		// L2TP, OpenVPN, SSH and the rest are daemons the panel runs and wires up
		// itself; their Xray presence is a side effect, not the service. A node has
		// none of that machinery, and pretending otherwise would produce a config
		// that starts cleanly and serves nobody.
		if hasDerivedXrayInbound(inbound.Protocol) {
			notes = append(notes, inboundLabel(inbound)+" runs on a daemon the panel manages itself, which a node cannot host yet")
			continue
		}

		port := localPort[id]
		if port <= 0 {
			// 0 means "same port as the panel uses", resolved here and never stored,
			// so editing the inbound's port moves the node with it.
			port = inbound.Port
		}
		if port < 1 || port > 65535 {
			notes = append(notes, inboundLabel(inbound)+" has no usable port for this node")
			continue
		}
		if other, clash := taken[port]; clash {
			// Xray refuses the entire config over one duplicate port, so a clash has
			// to cost one inbound rather than the whole node.
			if other == 0 {
				notes = append(notes, inboundLabel(inbound)+" wants port "+strconv.Itoa(port)+", which the node agent reserves for itself; give it a different port for this node")
			} else {
				notes = append(notes, inboundLabel(inbound)+" wants port "+strconv.Itoa(port)+", which inbound #"+strconv.Itoa(other)+" already uses on this node")
			}
			continue
		}

		if forwarding {
			target := inbound.Port
			if hopAddress != NodeMasterAddress {
				if !hopAssigned[id] {
					notes = append(notes, inboundLabel(inbound)+" is not assigned to the next hop, so there is nothing there to forward it to")
					continue
				}
				if hop := hopPorts[id]; hop > 0 {
					target = hop
				}
			}
			forward, marshalErr := json.Marshal(nodeForwardSettings{
				Address:        hopAddress,
				Port:           target,
				Network:        "tcp,udp",
				FollowRedirect: false,
			})
			if marshalErr != nil {
				return nil, notes, marshalErr
			}
			// A relay moves bytes and terminates nothing: no TLS, no Reality keys, no
			// client list. The handshake belongs to the node at the end of the chain,
			// which is the point - a seized relay gives up nothing but a destination.
			cfg.Inbounds = append(cfg.Inbounds, nodeXrayInbound{
				Port:     port,
				Protocol: "dokodemo-door",
				Settings: json.RawMessage(forward),
				Tag:      "relay-" + strconv.Itoa(id),
			})
			taken[port] = id
			continue
		}

		stream := string(inbound.StreamSettings)
		if strings.Contains(stream, "certificateFile") {
			// Paths, not contents. The node will start and then fail every handshake.
			notes = append(notes, inboundLabel(inbound)+" names certificate files by path; copy them to the node at the same paths, or switch the inbound to inline certificates")
		}

		// Listen is deliberately dropped: an address that pins the inbound to one
		// interface on the panel's host is, on a different host, either meaningless
		// or an address that does not exist there.
		cfg.Inbounds = append(cfg.Inbounds, nodeXrayInbound{
			Port:           port,
			Protocol:       string(inbound.Protocol),
			Settings:       nodeRawJson(string(inbound.Settings)),
			StreamSettings: nodeRawJson(stream),
			Tag:            "inbound-" + strconv.Itoa(id),
		})
		taken[port] = id
	}

	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, notes, err
	}
	return json.RawMessage(raw), notes, nil
}

// nodeRawJson passes stored JSON through untouched, and drops anything that is
// not JSON. Embedding a malformed blob would produce a config file Xray refuses
// in full, taking every other inbound on the node down with it.
func nodeRawJson(value string) json.RawMessage {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	if !json.Valid([]byte(trimmed)) {
		return nil
	}
	return json.RawMessage(trimmed)
}
