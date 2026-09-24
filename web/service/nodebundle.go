package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/mhsanaei/3x-ui/v2/logger"
)

// NodeBundleInbound is one inbound as a node is told to serve it.
//
// RemotePort 0 means "whatever port the inbound already uses", left unresolved
// here on purpose: the inbound record is the authority on ports, and copying the
// number into the bundle would freeze a value the inbound page can change a
// minute later, then quietly disagree with it.
type NodeBundleInbound struct {
	InboundId  int `json:"inboundId"`
	RemotePort int `json:"remotePort"`
}

// NodeBundle is everything a node needs: which inbounds it serves, whether it
// egresses itself or forwards to another node, the rendered Xray config, and a
// hash of exactly that.
//
// The assignment and the config are both here because they answer different
// questions. The assignment is what an operator set and what the Nodes page
// shows; the config is what the node actually runs. Keeping the first without
// the second was the earlier design, and it had one fatal property: editing an
// inbound changed nothing the hash could see, so nodes kept serving yesterday's
// clients until somebody touched the assignment.
type NodeBundle struct {
	NodeId          int                 `json:"nodeId"`
	Node            string              `json:"node"`
	Role            string              `json:"role"`
	RelayVia        string              `json:"relayVia"`
	RelayViaAddress string              `json:"relayViaAddress"`
	RelayChain      []string            `json:"relayChain"`
	Inbounds        []NodeBundleInbound `json:"inbounds"`
	Config          json.RawMessage     `json:"config"`
	Notes           []string            `json:"notes,omitempty"`
	Hash            string              `json:"hash"`
}

// Bundle builds a node's work and records its hash as the config the master
// expects to be running.
//
// The hash covers the descriptor and the rendered config and nothing else, so
// it changes exactly when what the node should be doing changes - not when it
// last checked in, not when somebody edited its city. That is what makes the
// in-sync column trustworthy: the master writes the hash it built, the agent
// reports the hash it applied, and the panel only ever compares the two.
func (s *NodeService) Bundle(nodeId int) (*NodeBundle, error) {
	ensureNodeTables()

	n, err := s.Get(nodeId)
	if err != nil {
		return nil, err
	}
	links, err := s.Inbounds(n.Id)
	if err != nil {
		return nil, err
	}
	chain, err := s.Chain(n.Id)
	if err != nil {
		return nil, err
	}

	bundle := &NodeBundle{
		NodeId:     n.Id,
		Node:       n.Name,
		Role:       n.Role,
		RelayChain: chain,
		Inbounds:   make([]NodeBundleInbound, 0, len(links)),
	}

	if n.RelayViaId > 0 {
		// Only the identity of the next hop, not a dialable target: the port a
		// relay forwards to belongs to the inbound it forwards, and inventing one
		// here would be a number two sides could disagree about.
		if via, err := s.Get(n.RelayViaId); err == nil {
			bundle.RelayVia = via.Name
			bundle.RelayViaAddress = via.Address
		}
	}

	for _, link := range links {
		if !link.Enable {
			continue
		}
		bundle.Inbounds = append(bundle.Inbounds, NodeBundleInbound{
			InboundId:  link.InboundId,
			RemotePort: link.RemotePort,
		})
	}
	// Sorted so the hash depends on the assignment and not on row order, which
	// SQLite is under no obligation to keep stable. Without this an unchanged
	// node would drift in and out of sync for no reason.
	sort.SliceStable(bundle.Inbounds, func(i, j int) bool {
		return bundle.Inbounds[i].InboundId < bundle.Inbounds[j].InboundId
	})

	config, notes, err := s.XrayConfig(n.Id)
	if err != nil {
		return nil, err
	}
	bundle.Config = config
	bundle.Notes = notes

	bundle.Hash = nodeBundleHash(bundle)
	if bundle.Hash != "" && bundle.Hash != n.ConfigHash {
		// A read that writes, narrowly: the master only knows what it expects
		// once it has built it. A failure here is logged rather than returned,
		// because the agent asking for its work should still receive it.
		if err := s.MarkConfig(n.Id, bundle.Hash); err != nil {
			logger.Warning("node config hash err:", err)
		}
	}
	return bundle, nil
}

func nodeBundleHash(b *NodeBundle) string {
	flat := *b
	flat.Hash = ""
	// Notes describe the config, they are not part of it. Hashing them would put
	// a node out of sync because an unrelated inbound was disabled somewhere, and
	// send it off to re-apply a config byte-identical to the one it is running.
	flat.Notes = nil
	raw, err := json.Marshal(flat)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
