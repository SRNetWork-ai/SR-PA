package service

// The device registry.
//
// This exists because "how many devices is this account using" and "how many
// addresses has this account connected from" are different questions, and the
// panel could only answer the second one. On the networks these accounts are
// actually used on, the second answer is not an approximation of the first, it
// is unrelated to it:
//
//   - A phone on a mobile carrier gets a new address on every reconnect, every
//     cell handover and every idle timeout. One device, a dozen addresses a day.
//     A cap that counts addresses throttles the most ordinary customer there is.
//   - Carrier-grade NAT (100.64/10) puts many subscribers behind one address.
//     One address, many devices, and the cap sees nothing.
//   - A home line with a router serves a phone, a laptop and a TV from one
//     address. Again one address, three devices.
//
// So each observation is reduced to a DEVICE IDENTITY, strongest signal first,
// and the row is keyed on that. When a protocol can tell us what the client is
// (SSH sends a version banner, OpenVPN sends peer-info, RADIUS carries a
// calling-station-id) that fingerprint IS the identity and the address is just an
// attribute. When nothing is available the address is used, and the row says so,
// because presenting a fallback as a fingerprint would be worse than the old
// behaviour, not better.

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/logger"
)

// DeviceObservation is one sighting of a device, from whichever subsystem saw
// it. Everything except Email and IP is optional: a caller that knows more
// produces a better identity, and a caller that knows only the address still
// produces a usable row.
type DeviceObservation struct {
	Email       string
	IP          string
	Protocol    string
	InboundId   int
	NodeName    string
	Fingerprint string
	Seen        int64
}

// DeviceRow is one device as the panel renders it, with the live address
// annotation attached.
type DeviceRow struct {
	DeviceKey    string   `json:"deviceKey"`
	Kind         string   `json:"kind"`
	Label        string   `json:"label"`
	Fingerprint  string   `json:"fingerprint"`
	Protocol     string   `json:"protocol"`
	InboundId    int      `json:"inboundId"`
	NodeName     string   `json:"nodeName"`
	Carrier      string   `json:"carrier"`
	CarrierKind  string   `json:"carrierKind"`
	CountryCode  string   `json:"countryCode"`
	LastIp       string   `json:"lastIp"`
	Ips          []string `json:"ips"`
	FirstSeen    int64    `json:"firstSeen"`
	LastSeen     int64    `json:"lastSeen"`
	Observations int64    `json:"observations"`
	Active       bool     `json:"active"`
	Intel        IPIntel  `json:"intel"`
}

const (
	// A device stops counting as present this long after its last sighting. It is
	// deliberately close to the IP log's own retention: both are answering "is
	// this thing still here", and two different answers on one screen is a bug
	// report waiting to happen.
	deviceActiveWindow = int64(30 * 60)

	// Rows older than this are pruned. Device history is for spotting sharing,
	// which shows up within days; keeping it forever only grows the table.
	deviceRetention = int64(14 * 24 * 60 * 60)

	// Per-device address history, newest last.
	deviceMaxIps = 12

	// Ceiling per account. A shared credential can otherwise write an unbounded
	// number of rows, which is a disk-filling vector from an untrusted party.
	deviceMaxPerAccount = 64
)

var (
	deviceMigrateMu   sync.Mutex
	deviceMigrateDone bool
	deviceWriteMu     sync.Mutex
)

// deviceStoreReady migrates the table on first use and reports whether the
// registry can be used at all.
//
// Lazy and retryable on purpose: this subsystem is optional telemetry, so it
// must never be a reason the panel fails to start, and a database that was not
// ready at the first observation must not disable it permanently.
func deviceStoreReady() bool {
	deviceMigrateMu.Lock()
	defer deviceMigrateMu.Unlock()
	if deviceMigrateDone {
		return true
	}
	db := database.GetDB()
	if db == nil {
		return false
	}
	if err := db.AutoMigrate(&model.DeviceSession{}); err != nil {
		logger.Warning("device registry: migrate failed:", err)
		return false
	}
	deviceMigrateDone = true
	return true
}

// ObserveDevice records one sighting.
func ObserveDevice(obs DeviceObservation) {
	ObserveDevices([]DeviceObservation{obs})
}

// ObserveDevices records a batch, resolving every distinct address once.
//
// Batching is not just an optimisation here: address resolution can touch the
// network, and a caller with fifty sightings must not turn that into fifty
// serial lookups while holding up a scheduled job.
func ObserveDevices(observations []DeviceObservation) {
	if len(observations) == 0 || !deviceStoreReady() {
		return
	}

	addresses := make([]string, 0, len(observations))
	for _, obs := range observations {
		if strings.TrimSpace(obs.IP) != "" {
			addresses = append(addresses, obs.IP)
		}
	}
	intelByIp := IPIntelBatch(addresses)

	now := time.Now().Unix()
	touched := make(map[string]struct{}, len(observations))

	for _, obs := range observations {
		obs.Email = strings.TrimSpace(obs.Email)
		obs.IP = strings.TrimSpace(obs.IP)
		if obs.Email == "" {
			continue
		}
		if obs.Seen <= 0 {
			obs.Seen = now
		}
		intel := intelByIp[obs.IP]
		if err := deviceUpsert(obs, intel); err != nil {
			logger.Warning("device registry: upsert failed:", err)
			continue
		}
		touched[obs.Email] = struct{}{}
	}

	for email := range touched {
		deviceEnforceAccountCap(email)
	}
}

// deviceUpsert folds one sighting into the account's row for that device.
func deviceUpsert(obs DeviceObservation, intel IPIntel) error {
	key, kind := deviceIdentity(obs, intel)
	label := deviceLabel(obs, intel, kind)

	deviceWriteMu.Lock()
	defer deviceWriteMu.Unlock()

	db := database.GetDB()
	row := &model.DeviceSession{}
	err := db.Model(model.DeviceSession{}).
		Where("client_email = ? and device_key = ?", obs.Email, key).
		First(row).Error
	if err != nil {
		row = &model.DeviceSession{
			ClientEmail: obs.Email,
			DeviceKey:   key,
			FirstSeen:   obs.Seen,
		}
	}

	row.Kind = kind
	row.Label = label
	if fp := deviceNormalizeFingerprint(obs.Fingerprint); fp != "" {
		row.Fingerprint = fp
	}
	if obs.Protocol != "" {
		row.Protocol = strings.ToLower(obs.Protocol)
	}
	if obs.InboundId > 0 {
		row.InboundId = obs.InboundId
	}
	if obs.NodeName != "" {
		row.NodeName = obs.NodeName
	}
	if intel.Carrier != "" {
		row.Carrier = intel.Carrier
	}
	if intel.CarrierKind != "" {
		row.CarrierKind = intel.CarrierKind
	}
	if intel.CountryCode != "" {
		row.CountryCode = intel.CountryCode
	}
	if obs.IP != "" {
		row.LastIp = obs.IP
		row.Ips = deviceMergeIps(row.Ips, obs.IP)
	}
	if row.FirstSeen == 0 || obs.Seen < row.FirstSeen {
		row.FirstSeen = obs.Seen
	}
	if obs.Seen > row.LastSeen {
		row.LastSeen = obs.Seen
	}
	row.Observations++

	return db.Save(row).Error
}

// deviceIdentity reduces a sighting to a stable identity, strongest signal
// first. The ORDER is the whole design:
//
//  1. A client fingerprint is the only signal that survives an address change,
//     so it wins whenever a protocol provides one.
//  2. A mobile carrier pool means one subscriber whose address is not stable,
//     so the carrier plus protocol is a better identity than any single address
//     it had. This is what stops a roaming phone from reading as many devices.
//  3. Carrier-grade NAT is the opposite risk, so it gets its own bucket and its
//     own kind, and is never presented as a single confirmed device.
//  4. Otherwise the address is all there is.
func deviceIdentity(obs DeviceObservation, intel IPIntel) (string, string) {
	proto := strings.ToLower(strings.TrimSpace(obs.Protocol))
	if proto == "" {
		proto = "any"
	}

	if fp := deviceNormalizeFingerprint(obs.Fingerprint); fp != "" {
		return "fp:" + fp, "fingerprint"
	}
	if intel.CarrierKind == "mobile" && intel.Carrier != "" {
		return "sim:" + strings.ToLower(intel.Carrier) + ":" + proto, "sim"
	}
	if intel.Scope == "cgnat" {
		bucket := intel.Carrier
		if bucket == "" {
			bucket = deviceReverseSuffix(intel.Reverse)
		}
		if bucket == "" {
			bucket = obs.IP
		}
		return "nat:" + strings.ToLower(bucket) + ":" + proto, "shared-nat"
	}
	if obs.IP == "" {
		return "unknown:" + proto, "address"
	}
	return "ip:" + obs.IP, "address"
}

// deviceLabel is what the operator reads. It never invents detail: with no
// fingerprint and no carrier it is the address, which is exactly as much as is
// known.
func deviceLabel(obs DeviceObservation, intel IPIntel, kind string) string {
	parts := make([]string, 0, 3)
	if fp := deviceNormalizeFingerprint(obs.Fingerprint); fp != "" {
		parts = append(parts, fp)
	}
	switch {
	case intel.Carrier != "":
		parts = append(parts, intel.Carrier)
	case intel.Org != "":
		parts = append(parts, intel.Org)
	}
	if kind == "shared-nat" {
		parts = append(parts, "shared NAT")
	}
	if len(parts) == 0 {
		if obs.IP != "" {
			return obs.IP
		}
		return "unknown device"
	}
	label := strings.Join(parts, " - ")
	if obs.InboundId > 0 && kind == "address" {
		label += " (inbound " + strconv.Itoa(obs.InboundId) + ")"
	}
	return label
}

// deviceNormalizeFingerprint trims the noise out of a client-supplied
// identifier. It is client-supplied, so it is also untrusted: bounded length,
// single line, no control characters.
func deviceNormalizeFingerprint(fp string) string {
	fp = strings.TrimSpace(fp)
	if fp == "" {
		return ""
	}
	fp = strings.ReplaceAll(fp, "\n", " ")
	fp = strings.ReplaceAll(fp, "\r", " ")
	fp = strings.ReplaceAll(fp, "\t", " ")
	fp = strings.Join(strings.Fields(fp), " ")
	if len(fp) > 96 {
		fp = fp[:96]
	}
	return fp
}

// deviceReverseSuffix keeps the registrable-looking tail of a PTR name, so two
// addresses in one provider's pool bucket together.
func deviceReverseSuffix(reverse string) string {
	reverse = strings.TrimSpace(strings.ToLower(reverse))
	if reverse == "" {
		return ""
	}
	labels := strings.Split(reverse, ".")
	if len(labels) <= 2 {
		return reverse
	}
	return strings.Join(labels[len(labels)-2:], ".")
}

// deviceMergeIps appends an address to the bounded history, newest last, without
// duplicating the entry it already ends with.
func deviceMergeIps(stored, ip string) string {
	ips := deviceParseIps(stored)
	filtered := make([]string, 0, len(ips)+1)
	for _, existing := range ips {
		if existing != ip {
			filtered = append(filtered, existing)
		}
	}
	filtered = append(filtered, ip)
	if len(filtered) > deviceMaxIps {
		filtered = filtered[len(filtered)-deviceMaxIps:]
	}
	encoded, err := json.Marshal(filtered)
	if err != nil {
		return stored
	}
	return string(encoded)
}

func deviceParseIps(stored string) []string {
	if strings.TrimSpace(stored) == "" {
		return nil
	}
	var ips []string
	if err := json.Unmarshal([]byte(stored), &ips); err != nil {
		return nil
	}
	return ips
}

// deviceEnforceAccountCap drops the least recently seen rows past the ceiling.
func deviceEnforceAccountCap(email string) {
	db := database.GetDB()
	var count int64
	if err := db.Model(model.DeviceSession{}).Where("client_email = ?", email).Count(&count).Error; err != nil {
		return
	}
	if count <= deviceMaxPerAccount {
		return
	}
	var stale []model.DeviceSession
	if err := db.Model(model.DeviceSession{}).
		Where("client_email = ?", email).
		Order("last_seen asc").
		Limit(int(count - deviceMaxPerAccount)).
		Find(&stale).Error; err != nil {
		return
	}
	for _, row := range stale {
		db.Delete(model.DeviceSession{}, row.Id)
	}
}

// ListAccountDevices returns one account's devices, most recently seen first,
// each annotated with what is currently known about its address.
func ListAccountDevices(email string) ([]DeviceRow, error) {
	email = strings.TrimSpace(email)
	if email == "" || !deviceStoreReady() {
		return []DeviceRow{}, nil
	}

	var stored []model.DeviceSession
	err := database.GetDB().Model(model.DeviceSession{}).
		Where("client_email = ?", email).
		Find(&stored).Error
	if err != nil {
		return nil, err
	}

	addresses := make([]string, 0, len(stored))
	for _, row := range stored {
		if row.LastIp != "" {
			addresses = append(addresses, row.LastIp)
		}
	}
	intelByIp := IPIntelBatch(addresses)

	cutoff := time.Now().Unix() - deviceActiveWindow
	rows := make([]DeviceRow, 0, len(stored))
	for _, row := range stored {
		rows = append(rows, DeviceRow{
			DeviceKey:    row.DeviceKey,
			Kind:         row.Kind,
			Label:        row.Label,
			Fingerprint:  row.Fingerprint,
			Protocol:     row.Protocol,
			InboundId:    row.InboundId,
			NodeName:     row.NodeName,
			Carrier:      row.Carrier,
			CarrierKind:  row.CarrierKind,
			CountryCode:  row.CountryCode,
			LastIp:       row.LastIp,
			Ips:          deviceParseIps(row.Ips),
			FirstSeen:    row.FirstSeen,
			LastSeen:     row.LastSeen,
			Observations: row.Observations,
			Active:       row.LastSeen >= cutoff,
			Intel:        intelByIp[row.LastIp],
		})
	}
	sort.Slice(rows, func(a, b int) bool {
		if rows[a].LastSeen != rows[b].LastSeen {
			return rows[a].LastSeen > rows[b].LastSeen
		}
		return rows[a].DeviceKey < rows[b].DeviceKey
	})
	return rows, nil
}

// CountAccountDevices counts devices seen inside the window, which is the number
// a device cap should be compared against.
//
// Rows of kind shared-nat count as ONE, not as the number of subscribers that
// might be behind them, because over-counting here would disconnect a paying
// customer on evidence the panel does not have.
func CountAccountDevices(email string, window int64) int {
	if !deviceStoreReady() {
		return 0
	}
	if window <= 0 {
		window = deviceActiveWindow
	}
	var count int64
	err := database.GetDB().Model(model.DeviceSession{}).
		Where("client_email = ? and last_seen >= ?", strings.TrimSpace(email), time.Now().Unix()-window).
		Count(&count).Error
	if err != nil {
		return 0
	}
	return int(count)
}

// ForgetAccountDevices clears an account's device history.
func ForgetAccountDevices(email string) error {
	if !deviceStoreReady() {
		return nil
	}
	return database.GetDB().
		Where("client_email = ?", strings.TrimSpace(email)).
		Delete(model.DeviceSession{}).Error
}

// PruneDeviceSessions drops rows past the retention window.
func PruneDeviceSessions() {
	if !deviceStoreReady() {
		return
	}
	cutoff := time.Now().Unix() - deviceRetention
	if err := database.GetDB().
		Where("last_seen < ?", cutoff).
		Delete(model.DeviceSession{}).Error; err != nil {
		logger.Warning("device registry: prune failed:", err)
	}
}
