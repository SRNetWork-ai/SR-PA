package job

import (
	"github.com/mhsanaei/3x-ui/v2/web/service"
	"github.com/mhsanaei/3x-ui/v2/xray"
)

// Relay traffic merge: how the userspace relays' own byte tallies (ssh, mtproto)
// are folded into the Xray records collected in the same tick.
//
// Split out of xray_traffic_job.go so the point where the two sources meet is one
// small, testable file (see relay_traffic_test.go).

// stampSourceInbound names the inbound each relay record's bytes came from, so
// the traffic multiplier bills them at that inbound's rate rather than at
// whichever inbound the account's single client_traffics row happens to name.
// An account missing from the map (or ambiguous, which the map reports as 0) is
// left at 0, which the billing reads as "unknown" and handles by taking the max
// across the account's memberships.
func stampSourceInbound(records []*xray.ClientTraffic, idByEmail map[string]int) []*xray.ClientTraffic {
	if len(records) == 0 || len(idByEmail) == 0 {
		return records
	}
	for _, record := range records {
		if record.InboundId != 0 {
			continue
		}
		record.InboundId = idByEmail[service.AccountKeyOf(record.Email)]
	}
	return records
}

// appendUnrecorded appends each fallback record whose email has no record in
// `existing` yet, and returns the extended slice.
//
// It is the de-duplication guard for a relay whose loopback bridge Xray STILL bills
// as a user stat (mtproto today). Both sources then measure the same transfer, and
// addClientTraffic sums every record for an email, so exactly one of the two may
// reach it.
//
// The predicate is "does this account already have a record this tick", NOT "did it
// move bytes this tick". The two sources flush on different boundaries: Xray can
// report zero for an account whose bytes the relay has already tallied, and report
// those same bytes a tick or two later. Gating on a positive byte count therefore
// admits the relay copy now and the Xray copy afterwards, billing one transfer twice
// (measured: 1.67x on a 100MiB pull).
//
// That stability has a price, and it is why ssh no longer uses this function. An
// account that ALSO holds an Xray-native config (vless, vmess, trojan, ...) has a
// user record in EVERY tick, because GetTraffic emits one per registered counter
// including the zero-valued ones. Its relay bytes were therefore dropped forever:
// traffic climbed on the ssh inbound while the client's own usage stayed flat. The
// answer is to remove the overlap at the source instead of guessing here, which is
// what appendRelayMeasured documents.
func appendUnrecorded(existing, fallback []*xray.ClientTraffic) []*xray.ClientTraffic {
	recorded := make(map[string]bool, len(existing))
	for _, t := range existing {
		recorded[t.Email] = true
	}
	for _, t := range fallback {
		if recorded[t.Email] {
			continue
		}
		// Mark as we go so two relays reporting the same account in one tick contribute
		// once, not twice.
		recorded[t.Email] = true
		existing = append(existing, t)
	}
	return existing
}

// appendRelayMeasured appends every relay record that carries bytes, with no
// de-duplication against this tick's Xray records at all.
//
// It is sound only because the overlap it would otherwise create no longer exists:
// the ssh gateway's loopback socks bridge runs at service.RelayBridgeUserLevel, a
// policy level the panel never enables statsUser* on, so Xray counts those bytes at
// the INBOUND level only (system stats, unchanged) and emits no
// user>>>email>>>traffic counter for them. The gateway's own io.Copy tally is then
// the single source of truth for the account, and it is exact rather than a
// fallback.
//
// Routing is untouched: the socks username still carries the account email, which is
// what a user:[email] rule resolves against. The policy level gates accounting only.
//
// The consequence is the whole point of the change. A client holding an ssh config
// AND a native one now gets both billed, summed per email by addClientTraffic, each
// record already stamped with the inbound its bytes came from so the per-inbound
// breakdown splits them correctly.
func appendRelayMeasured(existing, measured []*xray.ClientTraffic) []*xray.ClientTraffic {
	for _, t := range measured {
		if t == nil {
			continue
		}
		if t.Up == 0 && t.Down == 0 {
			continue
		}
		existing = append(existing, t)
	}
	return existing
}
