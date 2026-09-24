package service

import (
	"strings"

	"github.com/mhsanaei/3x-ui/v2/logger"
	"github.com/mhsanaei/3x-ui/v2/xray"
)

// Traffic served by a node is measured on that node and sent here.
//
// The master cannot measure it. Its own Xray never sees those connections, and
// reaching into a node's API from here would mean exposing that API to the
// internet on every edge box - a credentialed control channel opened outward,
// to save a request the agent is already making inward every thirty seconds.
//
// So the agent reads its local counters and posts the difference since it last
// succeeded. Deltas rather than totals, because a node can be rebuilt, its core
// restarted and its counters reset at any moment, and an absolute figure would
// have to be reconciled against a history nobody keeps.

// nodeTrafficSanityBytes is the most one account may claim in a single report.
//
// Not a limit on real usage: it is roughly a month of saturated gigabit, from
// one account, in the seconds between two heartbeats. A number above it means a
// counter was misread or a report was forged, and merging it would flip an
// account to over-quota forever, which is a harder thing to undo than a missing
// tick of accounting.
const nodeTrafficSanityBytes int64 = 1 << 44

// NodeUserTraffic is one account's bytes since the node's last accepted report.
type NodeUserTraffic struct {
	Email string `json:"email" form:"email"`
	Up    int64  `json:"up" form:"up"`
	Down  int64  `json:"down" form:"down"`
}

// NodeTagTraffic is one inbound tag's bytes on that node, kept for the fleet
// view and deliberately not billed. See IngestTraffic.
type NodeTagTraffic struct {
	Tag  string `json:"tag" form:"tag"`
	Up   int64  `json:"up" form:"up"`
	Down int64  `json:"down" form:"down"`
}

// NodeTrafficReport is one agent's measurement of one interval.
type NodeTrafficReport struct {
	Users    []NodeUserTraffic `json:"users" form:"users"`
	Inbounds []NodeTagTraffic  `json:"inbounds" form:"inbounds"`
}

// IngestTraffic merges a node's measurement into the panel's accounting.
//
// Only per-account bytes are billed. Inbound totals from a node would double
// count on purpose-built chains: a relay forwards the same bytes the node at
// the end of the chain also sees, so an inbound sum across a fleet counts every
// relayed byte once per hop. The per-account counter has no such problem -
// only the node that terminates the connection knows which account it belongs
// to, so exactly one node in any chain reports it.
//
// The records are marked CoreCounted because that is what they are: Xray user
// stats, carrying no inbound of their own. That flag is what lets the panel's
// attribution place them against an account's memberships instead of guessing,
// and it is the same path the master's own core records take.
func (s *NodeService) IngestTraffic(nodeId int, report NodeTrafficReport) (int, error) {
	ensureNodeTables()

	n, err := s.Get(nodeId)
	if err != nil {
		return 0, err
	}
	if n == nil {
		return 0, nil
	}

	records := make([]*xray.ClientTraffic, 0, len(report.Users))
	for _, user := range report.Users {
		email := strings.TrimSpace(user.Email)
		if email == "" {
			continue
		}
		up, down := user.Up, user.Down
		if up < 0 {
			up = 0
		}
		if down < 0 {
			down = 0
		}
		if up == 0 && down == 0 {
			continue
		}
		if up > nodeTrafficSanityBytes || down > nodeTrafficSanityBytes {
			logger.Warning("node", n.Name, "reported an impossible amount of traffic for", email, "- ignored")
			continue
		}
		records = append(records, &xray.ClientTraffic{
			Email:       email,
			Up:          up,
			Down:        down,
			CoreCounted: true,
		})
	}

	if len(records) == 0 {
		return 0, nil
	}

	// The same entry point the local collector uses, on purpose. Quota, expiry,
	// the traffic multiplier and the depletion sweep all live behind it, so an
	// account behaves identically whether its bytes were moved by the panel's
	// own Xray or by a node on another continent.
	inbounds := InboundService{}
	if err, _, _, _, _ := inbounds.AddTraffic(nil, records); err != nil {
		return 0, err
	}
	logger.Debug("node traffic merged from", n.Name, "for", len(records), "accounts")
	return len(records), nil
}
