package service

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"strings"

	"github.com/mhsanaei/3x-ui/v2/config"
	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/logger"
	"github.com/mhsanaei/3x-ui/v2/util/json_util"
	"github.com/mhsanaei/3x-ui/v2/xray"
)

// SshService manages the SSH protocol, an in-binary Go SSH gateway (no bundled
// daemon, no kernel module, no nftables, no per-client tunnel IP).
//
// SSH is the second RELAY protocol after MTProto, and shares its shape: a userspace
// terminator with no tunnel, so it deliberately does NOT touch vpnrange.go, gets no
// protocolBase, and stays out of the nft accounting path and BuildVpnEmailToIPMap. A
// client authenticates over SSH (username/password against the panel DB) and its
// forwarded connections egress through Xray via a loopback socks inbound, exactly
// like telemt: routing rides the RFC1929 socks username (the account email), so an
// operator's user:[email] rule resolves per client with no per-client IP.
//
// Where SSH BEATS MTProto: the server owns every net.Conn in-process, so it can
// enumerate live sessions and evict one. That gives it BOTH User-Limit strategies
// (reject and accept-evict-oldest), which the relay-delegated mtproto could not, and
// exact per-account byte accounting from io.Copy (no Prometheus scrape).
//
// All live state (listeners, session table, byte counters) lives in the package-level
// sshMgr singleton, NOT on this struct, because the traffic job drives a zero-value
// SshService copy (like mtprotoCounters / procMgr). The methods here are stateless DB
// helpers and thin delegations to sshMgr.
type SshService struct {
	inboundService InboundService
}

// sshUdpgwPort is the loopback port a client points its badvpn-udpgw at (through the
// SOCKS proxy). A direct-tcpip channel to 127.0.0.1:sshUdpgwPort is not a real TCP
// connection: it carries the udpgw protocol, which the server terminates in-process
// and relays as SOCKS5 UDP ASSOCIATE through Xray. Any other channel is plain TCP.
const sshUdpgwPort = 7300

// RelayBridgeUserLevel is the Xray policy level a relay protocol's loopback socks
// bridge runs at, and it exists for exactly one reason: accounting.
//
// A relay counts its own bytes in-process (io.Copy here, a metrics scrape for
// mtproto) and presents the account email as the socks username so routing can
// resolve user:[email]. Xray copies that username onto the session user, so at the
// default level 0 -- the only level the panel's policy template turns
// statsUserUplink/statsUserDownlink on for -- it ALSO bills those same bytes as a
// user stat. Two measurements of one transfer, and the traffic job then has to throw
// one away.
//
// The guard that did the throwing keyed on the mere PRESENCE of an Xray record for
// the account, because the two sources flush on different boundaries and anything
// smarter double-billed. The cost was silent and severe: an account that also held
// an Xray-native config (vless, vmess, ...) has a user record in every single tick,
// zero-valued or not, so its relay bytes were dropped forever. Traffic climbed on
// the ssh inbound while the client's own usage sat still.
//
// Running the bridge at a level the policy template never names removes the overlap
// at the source. Xray falls back to its built-in default policy for an unknown
// level, and that default has user stats OFF, so the bridge's bytes are counted at
// the INBOUND level only (system stats, untouched: the inbound total stays correct)
// and the relay's own exact tally becomes the single record of per-account usage.
//
// Routing is unaffected. Xray still sets inbound.User.Email from the socks username
// regardless of level, and that is what user:[email] rules resolve against; the
// level gates accounting, nothing else. Timeouts are unaffected too: the template
// sets none for level 0, so both levels use the same session defaults.
//
// Exported because mtproto's bridge must adopt it before its own tally can stop
// going through the presence guard (see appendUnrecorded in web/job).
const RelayBridgeUserLevel = 61

// sshSettings is the SSH slice of an inbound's Settings JSON.
//
// SSH is a relay, so there is NO addressing block (no ipRanges/dns/mtu) and no crypto
// material a client must carry (no psk/keys): the credential is a plain
// username/password. User Limit K and its strategy are inbound-level, like wg-c.
type sshSettings struct {
	// UserLimit is the inbound-level cap on simultaneous devices (distinct client
	// source IPs) per account. Same convention as the other protocols: 0 = no limit
	// (what the UI defaults a new inbound to), else 1..64. An ABSENT value is NOT 0:
	// nil = 1 (a legacy single-device inbound), which is why this is a *int.
	UserLimit         *int   `json:"userLimit"`
	UserLimitStrategy string `json:"userLimitStrategy"` // at the cap: "accept" (default, evict oldest) or "reject" (deny new device)

	// HostKey is the server's persisted ed25519 host key (PEM). The panel mints it on
	// first use and keeps it stable so a client's host-key pin holds across restarts.
	// It is server-managed and never shown in the UI.
	HostKey string `json:"hostKey"`

	// ExternalProxy lists alternate endpoints rendered into the generated client
	// config instead of this server's own address (a relay/CDN in front). Config-only;
	// the SSH server never reads it. Mirrors wg-c.
	ExternalProxy []sshExternalProxy `json:"externalProxy"`

	Clients []sshClient `json:"clients"`
}

// sshExternalProxy is one alternate endpoint for an account's generated config.
type sshExternalProxy struct {
	Dest   string `json:"dest"`
	Port   int    `json:"port"`
	Remark string `json:"remark"`
}

// sshClient is one SSH account. Identity is the username (ID); the password is the
// credential; Email is the routing/accounting identity (matches client_traffics and
// the user:[email] routing rule). A dedicated minimal struct (like wgcClient) so the
// UI's extra string fields do not break json.Unmarshal.
type sshClient struct {
	ID       string `json:"id"`       // SSH login username
	Password string `json:"password"` // SSH password
	Email    string `json:"email"`    // routing + accounting identity
	Enable   bool   `json:"enable"`
	// This account's own device cap. nil = inherit the inbound's User Limit, and it can
	// only LOWER it (resolveUserLimitOverride). SSH has no address pool, so nothing here
	// can collide; the rule is shared so one number means one thing across the panel.
	UserLimitOverride *int `json:"userLimitOverride"`
}

// GetSshInbounds returns every SSH inbound.
func (s *SshService) GetSshInbounds() ([]*model.Inbound, error) {
	db := database.GetDB()
	var inbounds []*model.Inbound
	err := db.Model(model.Inbound{}).Where("protocol = ?", model.SSH).Find(&inbounds).Error
	if err != nil {
		return nil, err
	}
	return inbounds, nil
}

func (s *SshService) parseSettings(inbound *model.Inbound) (*sshSettings, error) {
	settings := &sshSettings{}
	if err := json.Unmarshal([]byte(inbound.Settings), settings); err != nil {
		return nil, err
	}
	return settings, nil
}

// activeClients returns the accounts that are usable (non-empty username, password,
// email, and enabled). The SSH server never authenticates anything else.
func (s *SshService) activeClients(settings *sshSettings) []sshClient {
	out := make([]sshClient, 0, len(settings.Clients))
	for _, c := range settings.Clients {
		if strings.TrimSpace(c.ID) == "" || strings.TrimSpace(c.Password) == "" || strings.TrimSpace(c.Email) == "" {
			continue
		}
		if !c.Enable {
			continue
		}
		out = append(out, c)
	}
	return out
}

// effectiveSshK resolves the inbound User Limit to a device cap. nil -> 1 (legacy
// single device), <=0 -> 0 meaning unlimited, otherwise clamp to 1..64.
func effectiveSshK(u *int) int {
	if u == nil {
		return 1
	}
	if *u <= 0 {
		return 0
	}
	if *u > 64 {
		return 64
	}
	return *u
}

// GetSocksPort is the loopback socks inbound this SSH inbound egresses through,
// following the panel-wide "Xray-side port for inbound N is 12300+N" convention.
func (s *SshService) GetSocksPort(inbound *model.Inbound) int {
	return 12300 + inbound.Id
}

// GetSocksConfig builds the loopback socks inbound the SSH server egresses through.
// It is the mtproto GetSocksConfig with UDP enabled: SSH forwards both TCP (via
// direct-tcpip channels) and UDP (via the in-process udpgw bridge), and the UDP path
// needs the socks inbound to accept UDP ASSOCIATE so Xray routes and accounts UDP
// per client too.
//
// The inbound carries inbound.Tag, so operator per-inbound rules target it. Each
// account rides the socks username: the SSH server presents the authenticated
// account's Email there, Xray copies it to inbound.User.Email, and user:[email] rules
// resolve per client, the same carrier mtproto uses. The password equals the account
// name: this listener is bound to 127.0.0.1 and both ends are generated by this panel,
// so it is an identity assertion between two local processes, not an auth boundary.
//
// userLevel is the one deliberate difference from mtproto's bridge, and it is what
// makes SSH accounting correct: at level 0 Xray would bill this hop as a user stat on
// top of the gateway's own io.Copy tally, one transfer measured twice. At
// RelayBridgeUserLevel it is billed at the inbound level only, so the gateway's tally
// is the single source of truth and can simply be summed with whatever the account's
// native inbounds report. Routing still rides the username, which Xray copies to the
// session user at any level.
func (s *SshService) GetSocksConfig(inbound *model.Inbound) *xray.InboundConfig {
	settings, err := s.parseSettings(inbound)
	if err != nil {
		return nil
	}

	type socksAccount struct {
		User string `json:"user"`
		Pass string `json:"pass"`
	}
	accounts := make([]socksAccount, 0, len(settings.Clients))
	seen := map[string]bool{}
	for _, c := range s.activeClients(settings) {
		if seen[c.Email] {
			continue
		}
		seen[c.Email] = true
		accounts = append(accounts, socksAccount{User: c.Email, Pass: c.Email})
	}
	if len(accounts) == 0 {
		// A socks inbound with no accounts would reject every egress. With no usable
		// account there is nothing to route, so inject nothing.
		return nil
	}

	socksSettings, err := json.Marshal(struct {
		Auth      string         `json:"auth"`
		Accounts  []socksAccount `json:"accounts"`
		UDP       bool           `json:"udp"`
		UserLevel int            `json:"userLevel"`
	}{Auth: "password", Accounts: accounts, UDP: true, UserLevel: RelayBridgeUserLevel})
	if err != nil {
		logger.Warning("SSH: socks settings marshal failed:", err)
		return nil
	}
	sniffing := `{"enabled":true,"destOverride":["tls","http","quic"]}`

	return &xray.InboundConfig{
		Listen:   json_util.RawMessage(`"127.0.0.1"`),
		Port:     s.GetSocksPort(inbound),
		Protocol: "socks",
		Settings: json_util.RawMessage(socksSettings),
		Tag:      inbound.Tag,
		Sniffing: json_util.RawMessage(sniffing),
	}
}

// getDisabledEmails returns accounts the panel has switched off (quota hit, expired,
// or disabled in settings). Identical to the other services.
func (s *SshService) getDisabledEmails() map[string]bool {
	disabled := map[string]bool{}
	db := database.GetDB()
	if db == nil {
		return disabled
	}
	var traffics []*xray.ClientTraffic
	if err := db.Model(xray.ClientTraffic{}).Where("enable = ?", false).Find(&traffics).Error; err != nil {
		return disabled
	}
	for _, t := range traffics {
		disabled[t.Email] = true
	}
	return disabled
}

// ReconcileHostKeys mints an ed25519 host key for any inbound that has none and
// persists it. The UI never sets it; a stable host key is what a client pins, so it
// must survive restarts. Merges into the raw settings so unmodeled UI fields survive
// the round-trip (mirrors mtproto ReconcileSecrets / wg-c ReconcileAllKeys).
func (s *SshService) ReconcileHostKeys() error {
	inbounds, err := s.GetSshInbounds()
	if err != nil {
		return err
	}
	db := database.GetDB()
	for _, inbound := range inbounds {
		settings, err := s.parseSettings(inbound)
		if err != nil {
			continue
		}
		if strings.TrimSpace(settings.HostKey) != "" {
			continue
		}
		hk, err := generateSshHostKey()
		if err != nil {
			return err
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(inbound.Settings), &raw); err != nil {
			continue
		}
		raw["hostKey"] = hk
		out, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		inbound.Settings = string(out)
		if db != nil {
			if err := db.Model(model.Inbound{}).Where("id = ?", inbound.Id).
				Update("settings", inbound.Settings).Error; err != nil {
				logger.Warning("SSH: persisting host key failed:", err)
			}
		}
	}
	return nil
}

// generateSshHostKey returns a fresh ed25519 private key in PKCS#8 PEM form.
func generateSshHostKey() (string, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", err
	}
	block := &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	return string(pem.EncodeToMemory(block)), nil
}

// InitSsh brings the SSH gateway up at panel start.
func (s *SshService) InitSsh() {
	inbounds, err := s.GetSshInbounds()
	if err != nil || len(inbounds) == 0 {
		return
	}
	if err := s.ReconcileHostKeys(); err != nil {
		logger.Warning("SSH: host key reconcile failed:", err)
	}
	if err := s.RestartServices(); err != nil {
		logger.Warning("SSH: failed to start services:", err)
	}
}

// RestartServices reconciles the running in-process listeners with the enabled
// inbounds: it binds a listener for each enabled inbound and closes any that are no
// longer wanted. Unlike the daemon protocols this is not a process restart, just a
// listener reconcile, so live sessions on unchanged inbounds are preserved.
func (s *SshService) RestartServices() error {
	if err := s.ReconcileHostKeys(); err != nil {
		logger.Warning("SSH: host key reconcile failed:", err)
	}
	inbounds, err := s.GetSshInbounds()
	if err != nil {
		return err
	}
	sshMgr.reconcile(s, inbounds)
	return nil
}

// StopServices closes every SSH listener and drops its sessions.
func (s *SshService) StopServices() error {
	sshMgr.stopAll()
	return nil
}

// SetupRouting is a no-op: SSH is a userspace relay with no tunnel, so there are no
// nftables rules or kernel modules to install. Kept for shape parity with siblings.
func (s *SshService) SetupRouting() error { return nil }

// KillDisabledSessions closes the live sessions of any disabled account and trims
// every account to its User Limit, so a settings change that disables an account or
// lowers K takes effect within a tick even without a reconnect. Called each traffic tick.
func (s *SshService) KillDisabledSessions() {
	sshMgr.enforce(s, s.getDisabledEmails())
}

// DisableClients is the edge-triggered analogue; KillDisabledSessions already derives
// the disabled set from the DB, so this just runs it. Present for shape parity.
func (s *SshService) DisableClients(emails []string) {
	if len(emails) == 0 {
		return
	}
	sshMgr.enforce(s, s.getDisabledEmails())
}

// CollectTraffic returns per-account usage deltas since the last call, read straight
// from the in-process byte counters (atomic read-and-reset). No IP, no nft, no scrape:
// the server counted every byte on io.Copy as it flowed.
//
// Since the bridge moved to RelayBridgeUserLevel these deltas are no longer a
// fallback for what Xray might have missed: they are the ONLY per-account record of
// ssh usage, and the traffic job appends them unconditionally.
func (s *SshService) CollectTraffic() []*xray.ClientTraffic {
	return sshMgr.collect()
}

// Available reports whether SSH can run here. It is in-binary Go, so it is always
// available (no bundled binary, no kernel module, no host dependency).
func (s *SshService) Available() bool { return true }

// AnyRunning reports whether any SSH listener is bound (the "core running" signal).
func (s *SshService) AnyRunning() bool { return sshMgr.anyRunning() }

// lookupAccount finds the account for an SSH login on a given inbound, reading the DB
// live so client add/edit/disable needs no restart. It returns the account and true
// only when the username matches, the password matches, the account is enabled, and it
// is not disabled by quota/expiry.
func (s *SshService) lookupAccount(inboundId int, username, password string) (sshClient, bool) {
	inbound, err := s.inboundService.GetInbound(inboundId)
	if err != nil || inbound == nil || !inbound.Enable {
		return sshClient{}, false
	}
	settings, err := s.parseSettings(inbound)
	if err != nil {
		return sshClient{}, false
	}
	disabled := s.getDisabledEmails()
	for _, c := range s.activeClients(settings) {
		if c.ID != username {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(c.Password), []byte(password)) == 1 && !disabled[c.Email] {
			return c, true
		}
		return sshClient{}, false
	}
	return sshClient{}, false
}

// accountLimit returns the device cap that applies to ONE account on this inbound, plus
// the inbound's normalized strategy.
//
// The cap is the inbound's User Limit K with that account's own override folded in, which
// can only lower it (resolveUserLimitOverride). An account with no override, and every
// account on an inbound where nobody has one, resolves to exactly the inbound's K, so this
// is the previous behaviour with one extra lookup.
//
// The STRATEGY stays the inbound's and has no per-account form, the same asymmetry the IP
// Limit draws: how many devices an account may hold is that account's entitlement, but what
// happens AT the cap is the operator's policy for everyone on the inbound.
//
// An unknown account resolves to the inbound's K rather than to a refusal: this is called
// after the SSH handshake has already authenticated the account, so a miss here means the
// settings blob changed under a live session, not that the caller is unauthorized.
func (s *SshService) accountLimit(inboundId int, email string) (int, string) {
	inbound, err := s.inboundService.GetInbound(inboundId)
	if err != nil || inbound == nil {
		return 1, "reject"
	}
	settings, err := s.parseSettings(inbound)
	if err != nil {
		return 1, "reject"
	}
	k := effectiveSshK(settings.UserLimit)
	key := accountKey(email)
	for _, c := range settings.Clients {
		if accountKey(c.Email) == key {
			k = resolveUserLimitOverride(k, c.UserLimitOverride)
			break
		}
	}
	return k, normUserLimitStrategy(settings.UserLimitStrategy)
}

// panelVersion is the running panel version, reported as the SSH core "version".
func sshPanelVersion() string { return config.GetVersion() }
