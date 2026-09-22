package job

import (
	"github.com/mhsanaei/3x-ui/v2/logger"
	"github.com/mhsanaei/3x-ui/v2/web/service"
	"github.com/mhsanaei/3x-ui/v2/web/service/rbridge"
	"github.com/mhsanaei/3x-ui/v2/web/websocket"
	"github.com/mhsanaei/3x-ui/v2/xray"
)

// XrayTrafficJob collects and processes traffic statistics from Xray, updating the database and optionally informing external APIs.
type XrayTrafficJob struct {
	adminService    service.AdminService
	settingService  service.SettingService
	xrayService     service.XrayService
	inboundService  service.InboundService
	outboundService service.OutboundService
	l2tpService     service.L2tpService
	pptpService     service.PptpService
	openvpnService  service.OpenVpnService
	ocservService   service.OcservService
	sstpService     service.SstpService
	ikev2Service    service.Ikev2Service
	wgcService      service.WgcService
	awgService      service.AwgService
	greService      service.GreService
	mtprotoService  service.MtprotoService
	sshService      service.SshService
	nftService      service.NftService
	radiusService   *service.RadiusService
	sweeper         *rbridge.Sweeper
}

// NewXrayTrafficJob creates a new traffic collection job instance.
func NewXrayTrafficJob(rs *service.RadiusService) *XrayTrafficJob {
	j := &XrayTrafficJob{radiusService: rs}
	// Wire the shared RADIUS server into THIS job's own l2tp/pptp/openvpn service
	// structs. They are zero-value copies (not the web server's wired instances), so
	// without this their radiusService is nil and every DisableClients / KillDisabled-
	// Sessions call silently no-ops (see the `s.radiusService != nil` guards) — which
	// is why over-quota and disabled l2tp/pptp tunnels were never torn down live and
	// only got refused at the next reconnect. The secret is irrelevant to the kill
	// paths (they read the in-memory session map), so pass empty; getRadiusSecret
	// falls back to the DB if anything else needs it.
	j.l2tpService.SetRadius(rs, "")
	j.pptpService.SetRadius(rs, "")
	j.openvpnService.SetRadius(rs, "")
	j.ocservService.SetRadius(rs, "")
	j.sstpService.SetRadius(rs, "")
	j.ikev2Service.SetRadius(rs, "")
	// The rbridge Sweeper drives the non-RADIUS (sweep-reconciled) protocols each tick: it polls
	// their live tunnels, enforces quota/disable + User Limit, and writes their sessions +
	// nft accounting through the RADIUS service (the rbridge.Sink). ikev2 psk/eap-tls is the
	// first such adapter; future non-RADIUS protocols (e.g. WireGuard) register here too.
	j.sweeper = rbridge.New(rs)
	j.sweeper.Register(&j.ikev2Service)
	j.sweeper.Register(&j.wgcService)
	j.sweeper.Register(&j.awgService)
	j.sweeper.Register(&j.greService)
	return j
}

// Run collects traffic statistics from Xray and updates the database, triggering restart if needed.
func (j *XrayTrafficJob) Run() {
	var traffics []*xray.Traffic
	var clientTraffics []*xray.ClientTraffic

	if j.xrayService.IsXrayRunning() {
		var err error
		traffics, clientTraffics, err = j.xrayService.GetXrayTraffic()
		if err != nil {
			traffics = nil
			clientTraffics = nil
		}
	}

	// Collect L2TP, PPTP, and OpenVPN per-client traffic from nftables counters (atomic read+reset)
	// Session maps (IP→email) come from the embedded RADIUS server
	// This runs regardless of Xray status — VPN traffic is independent
	// Non-RADIUS protocols (ikev2 psk/eap-tls, ...) authenticate locally with no RADIUS
	// round-trip, so the rbridge Sweeper reconciles their live tunnels into the session store +
	// nft counters BEFORE reading sessions below, so this tick bills their traffic and enforces
	// their quota + User Limit.
	// WireGuard: reconcile the kernel peer set to DB state FIRST (before the sweep polls),
	// so the interface holds exactly the enabled, non-disabled devices. Because a removed
	// peer cannot complete a handshake, this makes disable/quota enforcement HARD (a
	// disabled account cannot reconnect at all) and re-adds re-enabled accounts within one
	// tick — no eventual-eviction window. Cheap: a few in-process wgctrl/netlink calls.
	if err := j.wgcService.GenerateAllConfigs(); err != nil {
		logger.Debug("wgc: peer reconcile failed:", err)
	}
	// AmneziaWG: identical hard-enforcement reconcile to wg-c, before the sweep polls.
	if err := j.awgService.GenerateAllConfigs(); err != nil {
		logger.Debug("awg: peer reconcile failed:", err)
	}
	// GRE: same contract, and it matters MORE here than for the WireGuard family. GRE has no
	// handshake to refuse, so this reconcile IS the enforcement: it deletes a disabled
	// account's point-to-point device and withdraws its route and learned neighbour entry,
	// leaving it with no reverse path at all.
	if err := j.greService.GenerateAllConfigs(); err != nil {
		logger.Debug("gre: peer reconcile failed:", err)
	}
	// IKEv2 psk/eap-tls: same hard-enforcement contract, for the same reason. Those two
	// modes authenticate locally at charon (no RADIUS round-trip that could re-check the
	// account), so the sweep below only terminates the SA and the client re-dials into the
	// next tick's gap -- disabled, but still browsing. This drops the disabled account's
	// swanctl connection so the eviction sticks, and restores it within a tick when the
	// account comes back. Cheap in the steady state: it reloads only when the admissible
	// set changed. eap-mschapv2 is untouched (its RADIUS re-auth already enforces).
	j.ikev2Service.ReconcileDisabled()

	if j.sweeper != nil {
		j.sweeper.Tick()
	}

	vpnSessions := map[string]map[string]string{
		"l2tp":        j.radiusService.GetSessions("l2tp"),
		"pptp":        j.radiusService.GetSessions("pptp"),
		"openvpn":     j.radiusService.GetSessions("openvpn"),
		"openconnect": j.radiusService.GetSessions("openconnect"),
		"sstp":        j.radiusService.GetSessions("sstp"),
		"ikev2":       j.radiusService.GetSessions("ikev2"),
		"wg-c":        j.radiusService.GetSessions("wg-c"),
		"awg":         j.radiusService.GetSessions("awg"),
		"gre":         j.radiusService.GetSessions("gre"),
	}
	// Which inbound each tunnel address belongs to, so every collected record names
	// the SOURCE of its bytes. Two things read it: the traffic multiplier, which
	// must bill at the rate of the inbound the traffic actually came from, and the
	// per-inbound breakdown, which files the bytes under that inbound's membership.
	//
	// It comes from the SESSION, which knows: the address was handed out of one
	// inbound's pool, and the daemon that authenticated it named its inbound (or the
	// panel resolved it from the address). It used to come from
	// SingleInboundIdByEmail, which answers by scanning settings for the account and
	// deliberately returns 0 when two inbounds of the protocol serve it -- so a
	// multi-inbound account lost its multiplier entirely, and there was nothing to
	// attribute its usage with either.
	//
	// SingleInboundIdByEmail is kept as the fallback for an address the session store
	// cannot place (a panel restarted mid-session, before the first interim re-adds
	// it), where its one answer is better than none.
	vpnInbounds := make(map[string]map[string]int, len(vpnSessions))
	for protocol, ipToEmail := range vpnSessions {
		if len(ipToEmail) == 0 {
			continue
		}
		byIP := j.radiusService.GetSessionInbounds(protocol)
		if byIP == nil {
			byIP = map[string]int{}
		}
		var idByEmail map[string]int
		for ip, email := range ipToEmail {
			if byIP[ip] != 0 {
				continue
			}
			if idByEmail == nil {
				idByEmail = j.inboundService.SingleInboundIdByEmail(protocol)
			}
			if id := idByEmail[service.AccountKeyOf(email)]; id != 0 {
				byIP[ip] = id
			}
		}
		vpnInbounds[protocol] = byIP
	}
	clientTraffics = append(clientTraffics, j.nftService.CollectAndResetTraffic(vpnSessions, vpnInbounds)...)

	// MTProto and SSH are userspace relays: no client ever gets a tunnel IP, so neither
	// can use the nft per-IP path above. Each keeps its own per-account byte counters
	// (telemt's Prometheus metrics; the SSH gateway's io.Copy tallies).
	//
	// Both egress through a paired Xray socks inbound whose USERNAME is the account
	// email, and Xray copies that username onto the session user, so historically Xray
	// billed the SAME bytes a second time as a user stat and one of the two copies had
	// to be dropped. appendUnrecorded still does that for mtproto: it drops the relay
	// copy for any account Xray already has a record for this tick.
	//
	// SSH no longer needs that guess, and must not make it. Its bridge runs at
	// service.RelayBridgeUserLevel, a policy level the panel never enables statsUser*
	// on, so Xray counts ssh bytes at the inbound level only and emits no user counter
	// for them. The gateway's own tally is exact and is the only record of them, so it
	// is appended unconditionally.
	//
	// That is the fix for a real accounting hole: the presence predicate meant any
	// account that ALSO held an Xray-native config had a user record in every tick
	// (GetTraffic emits one per registered counter, including zero-valued ones), so its
	// ssh bytes were dropped forever. The inbound's counter climbed while the client's
	// own usage never moved.
	//
	// Both relays name their own inbound: telemt scrapes per inbound and the SSH
	// gateway counts per listener, so each record arrives already stamped. The stamp
	// below is the fallback for a record that still carries none, and it skips any
	// that does (see stampSourceInbound). SingleInboundIdByEmail returns 0 when two
	// inbounds of the protocol serve the account, which falls back to the max
	// multiplier across memberships and contributes nothing to the breakdown.
	clientTraffics = appendUnrecorded(clientTraffics, stampSourceInbound(
		j.mtprotoService.CollectTraffic(), j.inboundService.SingleInboundIdByEmail("mtproto")))
	clientTraffics = appendRelayMeasured(clientTraffics, stampSourceInbound(
		j.sshService.CollectTraffic(), j.inboundService.SingleInboundIdByEmail("ssh")))

	// Level-triggered enforcement: disconnect any STILL-connected client that is no
	// longer allowed (quota/expiry hit, or disabled in settings). The edge-triggered
	// DisableClients calls below only fire on the exact tick a quota is first crossed;
	// if that one-shot kill missed, the live VPN tunnel would keep running until the
	// user reconnects (auth refuses them only then) — the reported "used their 1GB but
	// kept going" bug. This sweep re-derives the disabled set from the DB each run, so
	// it is idempotent and cheap when nothing is disabled. Runs before the early-return
	// so an idle-but-over-quota session is still torn down.
	j.l2tpService.KillDisabledSessions()
	j.pptpService.KillDisabledSessions()
	j.openvpnService.KillDisabledSessions()
	j.ocservService.KillDisabledSessions()
	j.sstpService.KillDisabledSessions()
	j.ikev2Service.KillDisabledSessions()
	// MTProto: re-renders [access.user_enabled] from client_traffics. telemt's config
	// watcher applies it and cancels the disabled accounts' live sessions itself, so
	// there is no separate kill path and no daemon restart.
	j.mtprotoService.KillDisabledSessions()
	// SSH: close the live sessions of any disabled account and trim every account to
	// its User Limit. The server owns every net.Conn, so this is an in-process close.
	j.sshService.KillDisabledSessions()

	// Skip DB update if no traffic to process
	if len(traffics) == 0 && len(clientTraffics) == 0 {
		return
	}

	err, needRestart0, l2tpDisabledEmails, pptpDisabledEmails, ovpnDisabledEmails := j.inboundService.AddTraffic(traffics, clientTraffics)
	if err != nil {
		logger.Warning("add inbound traffic failed:", err)
	}
	// Republish the speed limit sidecar now that AddTraffic has committed: this is the
	// only point in the tick where every protocol's bytes have landed, so it is the
	// first moment a "Limit After" threshold crossing is visible. It is deliberately
	// independent of the needRestart flags below and never sets one: nothing in the
	// xray.Config graph changed, and a restart per threshold crossing (which is a
	// continuous event as users consume data) is the exact thing the sidecar exists to
	// avoid. Cheap on a no-op tick: it skips the write when the bytes are unchanged.
	// The idle-tick early return above correctly skips this, since no bytes means no
	// threshold can have crossed.
	service.WriteSpeedLimits()
	// AddTraffic is where THIS tick's bytes land and a crossed quota flips the account
	// to disabled, so the sweep above (which ran before it) could only ever see the
	// PREVIOUS tick's verdict: an account that blew its quota kept relaying for one
	// more full interval. Re-run it here so the same tick that detects the overage
	// also ends the session, halving worst-case overshoot. Idempotent and cheap: it
	// re-renders the config and generateServerConfig now skips writing when the bytes
	// are unchanged, so a no-op tick costs a read and no reload.
	j.mtprotoService.KillDisabledSessions()
	// Enforce limits on L2TP clients (kill sessions, regenerate chap-secrets)
	if len(l2tpDisabledEmails) > 0 {
		j.l2tpService.DisableClients(l2tpDisabledEmails)
	}
	// Enforce limits on PPTP clients
	if len(pptpDisabledEmails) > 0 {
		j.pptpService.DisableClients(pptpDisabledEmails)
	}
	// Enforce limits on OpenVPN clients
	if len(ovpnDisabledEmails) > 0 {
		j.openvpnService.DisableClients(ovpnDisabledEmails)
	}
	err, needRestart1 := j.outboundService.AddTraffic(traffics, clientTraffics)
	if err != nil {
		logger.Warning("add outbound traffic failed:", err)
	}
	if ExternalTrafficInformEnable, err := j.settingService.GetExternalTrafficInformEnable(); ExternalTrafficInformEnable {
		j.informTrafficToExternalAPI(traffics, clientTraffics)
	} else if err != nil {
		logger.Warning("get ExternalTrafficInformEnable failed:", err)
	}
	if needRestart0 || needRestart1 {
		j.xrayService.SetToNeedRestart()
	}

	// If no frontend client is connected, skip all WebSocket broadcasting routines,
	// including expensive DB queries for online clients and JSON marshaling.
	if !websocket.HasClients() {
		return
	}

	// Update online clients list and map
	onlineClients := j.inboundService.GetOnlineClients()
	// And WHICH inbound each of them was seen on. The pages count their online
	// tallies per inbound from this; without it in the push they would recompute
	// from whatever membership set the last REST fetch left behind, and a session
	// that moved between two of an account's inbounds would go on lighting the one
	// it left until the next full refresh.
	onlineMemberships := j.inboundService.GetOnlineMemberships()
	lastOnlineMap, err := j.inboundService.GetClientsLastOnline()
	if err != nil {
		logger.Warning("get clients last online failed:", err)
		lastOnlineMap = make(map[string]int64)
	}

	// Traffic names clients, so it is per-admin data and cannot go out panel-wide:
	// broadcasting it whole put every admin's client emails and usage in every other
	// admin's browser. Each connected admin gets only their own slice.
	j.broadcastTrafficScoped(traffics, clientTraffics, onlineClients, onlineMemberships, lastOnlineMap)

	// Inbounds are per-admin, so this job cannot push a payload: it has no single
	// correct audience. It used to broadcast GetAllInbounds() to every browser every
	// tick, which handed each admin every other admin's inbounds and overwrote their
	// table with them. Sending the lightweight invalidate instead makes each browser
	// re-fetch through /panel/api/inbounds/list, which is scoped to the caller and
	// permission-gated. The frontend already debounces this signal.
	websocket.BroadcastInvalidate(websocket.MessageTypeInbounds)

	updatedOutbounds, err := j.outboundService.GetOutboundsTraffic()
	if err != nil {
		logger.Warning("get all outbounds for websocket failed:", err)
	}

	if updatedOutbounds != nil {
		websocket.BroadcastOutbounds(updatedOutbounds)
	}
}
