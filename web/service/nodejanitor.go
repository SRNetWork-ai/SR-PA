package service

import (
	"sync"
	"time"

	"github.com/mhsanaei/3x-ui/v2/logger"
)

// nodeJanitorInterval is how often silence is converted into offline.
//
// Matched to the agent heartbeat rather than to nodeOnlineWindow: the window
// already tolerates two missed beats, so sweeping on the beat means a node is
// marked offline within one tick of crossing it instead of up to a window late.
const nodeJanitorInterval = 30 * time.Second

var nodeJanitorOnce sync.Once

// StartNodeJanitor begins the background sweep that flips silent nodes to
// offline, and is safe to call from anywhere as often as you like.
//
// Why a goroutine here instead of a cron entry: the panel registers its jobs in
// files this project cannot currently rewrite whole, and inventing a second
// scheduler for one UPDATE would be worse than owning the ticker. The sweep is a
// single indexed statement, so the cost of being wrong about the interval is
// close to nothing.
//
// Why it is not started at boot: the node subsystem deliberately creates its
// tables only when something touches them, so a panel with no nodes stays a
// panel with no node tables. Starting a sweep unconditionally would quietly take
// that promise away, since the sweep itself migrates the tables.
//
// The limit this accepts: if the panel restarts while every agent is already
// dead, nothing sweeps until an operator opens the Nodes page or an agent calls
// in, so a stored status can still read online for a while. That is tolerable
// because the stored column has no reader in that window - the Nodes page
// computes health from last_seen itself, precisely so a stale row cannot claim a
// node is up.
func StartNodeJanitor() {
	nodeJanitorOnce.Do(func() {
		go func() {
			svc := NodeService{}
			// Swept immediately: the most likely stale rows are the ones left
			// behind by the restart that just happened.
			if err := svc.MarkStale(); err != nil {
				logger.Warning("node janitor err:", err)
			}
			ticker := time.NewTicker(nodeJanitorInterval)
			defer ticker.Stop()
			for range ticker.C {
				if err := svc.MarkStale(); err != nil {
					// Logged, never fatal. A failing sweep costs accuracy in a
					// status column; a janitor that can take the panel down with
					// it would cost the whole service.
					logger.Warning("node janitor err:", err)
				}
			}
		}()
	})
}
