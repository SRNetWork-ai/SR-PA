package service

// Source-address intelligence.
//
// The IP log (web/job/check_client_ip_job.go) records bare addresses, which
// answers "how many" and nothing else. An operator looking at a list of five
// addresses cannot tell a customer roaming across a mobile carrier (one SIM, a
// new address every reconnect, perfectly legitimate) from a shared account
// leaking across three cities, or from a datacenter address that is a scraper.
// Those three want opposite responses, so the panel has to name the network
// behind an address, not just print it.
//
// Design rules, in priority order:
//
//  1. Never block the caller on the network. Everything has a deadline, results
//     are cached, and a failed lookup degrades to a partial answer rather than
//     an error.
//  2. Offline first. Scope classification is arithmetic; reverse DNS uses the
//     resolver the server already has; the carrier table is compiled in. A panel
//     with no outbound internet still gets scope, PTR and carrier.
//  3. External enrichment is opt-in. Many deployments cannot reach a public geo
//     API, and none of them should silently ship their customers' addresses to a
//     third party. It is off unless SRUI_IPINTEL_ENDPOINT is set.

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// IPIntel is everything the panel knows about one source address. Every field is
// best-effort: an empty string means "not determined", never "none".
type IPIntel struct {
	IP      string `json:"ip"`
	Family  string `json:"family"` // ipv4 | ipv6
	Scope   string `json:"scope"`  // public | private | cgnat | loopback | linklocal | invalid
	Reverse string `json:"reverse"`

	// Carrier is the display name of the network operator ("Irancell (MTN)",
	// "Hetzner"), and CarrierKind is what KIND of network it is, which is the
	// field that actually changes an operator's decision.
	Carrier     string `json:"carrier"`
	CarrierKind string `json:"carrierKind"` // mobile | fixed | hosting | carrier-nat | unknown

	ASN         string `json:"asn"`
	Org         string `json:"org"`
	Country     string `json:"country"`
	CountryCode string `json:"countryCode"`
	City        string `json:"city"`

	Mobile  bool `json:"mobile"`
	Hosting bool `json:"hosting"`
	Proxy   bool `json:"proxy"`

	Source    string `json:"source"` // local | ptr | endpoint
	UpdatedAt int64  `json:"updatedAt"`
}

// ipIntelCarrierRule maps a substring of a PTR name or an org string to a
// network. Matching on the PTR is what makes this work with no external service:
// a mobile carrier's pool almost always reverses into its own domain, and a
// hosting provider's almost always reverses into theirs.
//
// Keep the match strings lowercase, specific enough not to collide, and ordered
// most specific first (the first hit wins).
type ipIntelCarrierRule struct {
	match string
	name  string
	kind  string
}

var ipIntelCarrierRules = []ipIntelCarrierRule{
	// Iranian mobile networks. These are the ones that matter for a device cap,
	// because a single SIM legitimately changes address on every reconnect and on
	// every cell handover, and a cap that counts addresses punishes that.
	{"mcci.ir", "Hamrah-e Aval (MCI)", "mobile"},
	{"mci.ir", "Hamrah-e Aval (MCI)", "mobile"},
	{"mtnirancell", "Irancell (MTN)", "mobile"},
	{"irancell", "Irancell (MTN)", "mobile"},
	{"rightel", "Rightel", "mobile"},
	{"taliya", "Taliya", "mobile"},
	{"samantel", "SamanTel", "mobile"},
	{"shatelmobile", "Shatel Mobile", "mobile"},

	// Iranian fixed-line and wireless ISPs.
	{"shatel", "Shatel", "fixed"},
	{"parsonline", "Pars Online", "fixed"},
	{"asiatech", "Asiatech", "fixed"},
	{"respina", "Respina", "fixed"},
	{"datak", "Datak", "fixed"},
	{"mobinnet", "MobinNet", "fixed"},
	{"pishgaman", "Pishgaman", "fixed"},
	{"fanava", "Fanava", "fixed"},
	{"hiweb", "HiWeb", "fixed"},
	{"sabanet", "Saba Net", "fixed"},
	{"tci.ir", "TCI (Mokhaberat)", "fixed"},
	{"iranet", "IRANET", "fixed"},

	// Hosting and cloud. An account connecting from one of these is a bot, a
	// scraper, a resold tunnel or a second panel, never a phone.
	{"amazonaws", "Amazon AWS", "hosting"},
	{"googleusercontent", "Google Cloud", "hosting"},
	{"azure", "Microsoft Azure", "hosting"},
	{"digitalocean", "DigitalOcean", "hosting"},
	{"hetzner", "Hetzner", "hosting"},
	{"your-server.de", "Hetzner", "hosting"},
	{"ovh.net", "OVH", "hosting"},
	{"ovh.com", "OVH", "hosting"},
	{"linode", "Linode", "hosting"},
	{"vultr", "Vultr", "hosting"},
	{"contabo", "Contabo", "hosting"},
	{"scaleway", "Scaleway", "hosting"},
	{"leaseweb", "Leaseweb", "hosting"},
	{"m247", "M247", "hosting"},
	{"cloudflare", "Cloudflare", "hosting"},
	{"arvancloud", "ArvanCloud", "hosting"},
	{"abrarvan", "ArvanCloud", "hosting"},
	{"cloudzy", "Cloudzy", "hosting"},
}

// ipIntelEntry is one cached answer plus its expiry.
type ipIntelEntry struct {
	intel   IPIntel
	expires time.Time
}

var (
	ipIntelMu    sync.Mutex
	ipIntelCache = map[string]ipIntelEntry{}
)

const (
	// A resolved address is stable for a long time; a failed one is retried
	// sooner because the failure is usually the resolver, not the address.
	ipIntelTTL     = 12 * time.Hour
	ipIntelFailTTL = 20 * time.Minute

	// Hard ceilings. The panel renders the IP list inside a modal, so a lookup
	// that is slower than this is worse than no lookup at all.
	ipIntelPTRTimeout      = 1200 * time.Millisecond
	ipIntelEndpointTimeout = 2500 * time.Millisecond
	ipIntelBatchTimeout    = 6 * time.Second
	ipIntelBatchWorkers    = 8

	// Bound on the cache. Addresses churn (every mobile reconnect is a new one),
	// so without this the map is a slow leak on a busy panel.
	ipIntelCacheMax = 4096
)

// IPIntelEnabled reports whether external enrichment is configured. The panel
// uses it to label the source of what it shows.
func IPIntelEnabled() bool { return strings.TrimSpace(os.Getenv("SRUI_IPINTEL_ENDPOINT")) != "" }

// LookupIPIntel resolves one address, using the cache when it can.
func LookupIPIntel(ip string) IPIntel {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return IPIntel{Scope: "invalid", Source: "local", UpdatedAt: time.Now().Unix()}
	}
	if hit, ok := ipIntelFromCache(ip); ok {
		return hit
	}
	intel := ipIntelResolve(context.Background(), ip)
	ipIntelStore(ip, intel)
	return intel
}

// IPIntelBatch resolves a list of addresses concurrently, under one deadline.
// Whatever is not resolved in time comes back with its offline fields filled in,
// because a half-annotated list is still far more useful than a bare one.
func IPIntelBatch(ips []string) map[string]IPIntel {
	out := make(map[string]IPIntel, len(ips))
	if len(ips) == 0 {
		return out
	}

	pending := make([]string, 0, len(ips))
	for _, ip := range ips {
		ip = strings.TrimSpace(ip)
		if ip == "" {
			continue
		}
		if _, done := out[ip]; done {
			continue
		}
		if hit, ok := ipIntelFromCache(ip); ok {
			out[ip] = hit
			continue
		}
		out[ip] = IPIntel{} // placeholder, replaced below
		pending = append(pending, ip)
	}
	if len(pending) == 0 {
		return out
	}

	ctx, cancel := context.WithTimeout(context.Background(), ipIntelBatchTimeout)
	defer cancel()

	type result struct {
		ip    string
		intel IPIntel
	}
	jobs := make(chan string)
	results := make(chan result, len(pending))
	var wg sync.WaitGroup

	workers := ipIntelBatchWorkers
	if len(pending) < workers {
		workers = len(pending)
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ip := range jobs {
				intel := ipIntelResolve(ctx, ip)
				ipIntelStore(ip, intel)
				results <- result{ip: ip, intel: intel}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, ip := range pending {
			select {
			case <-ctx.Done():
				return
			case jobs <- ip:
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	for r := range results {
		out[r.ip] = r.intel
	}
	// Anything the deadline cut off still gets its offline answer.
	for _, ip := range pending {
		if out[ip].IP == "" {
			out[ip] = ipIntelLocal(ip)
		}
	}
	return out
}

func ipIntelFromCache(ip string) (IPIntel, bool) {
	ipIntelMu.Lock()
	defer ipIntelMu.Unlock()
	entry, ok := ipIntelCache[ip]
	if !ok || time.Now().After(entry.expires) {
		return IPIntel{}, false
	}
	return entry.intel, true
}

func ipIntelStore(ip string, intel IPIntel) {
	ttl := ipIntelTTL
	if intel.Carrier == "" && intel.Reverse == "" && intel.Org == "" {
		ttl = ipIntelFailTTL
	}
	ipIntelMu.Lock()
	defer ipIntelMu.Unlock()
	if len(ipIntelCache) >= ipIntelCacheMax {
		// Cheap eviction: drop expired entries first, and if that frees nothing,
		// clear the map. Both are fine here because every entry is re-derivable.
		now := time.Now()
		for k, v := range ipIntelCache {
			if now.After(v.expires) {
				delete(ipIntelCache, k)
			}
		}
		if len(ipIntelCache) >= ipIntelCacheMax {
			ipIntelCache = map[string]ipIntelEntry{}
		}
	}
	ipIntelCache[ip] = ipIntelEntry{intel: intel, expires: time.Now().Add(ttl)}
}

// ipIntelResolve runs the full pipeline for one address: arithmetic, then the
// resolver, then the optional endpoint. Each stage only fills in what the
// previous one left empty.
func ipIntelResolve(ctx context.Context, ip string) IPIntel {
	intel := ipIntelLocal(ip)
	if intel.Scope != "public" {
		// A private or CGNAT address has no operator to look up, and asking a
		// public API about one leaks the shape of an internal network for nothing.
		return intel
	}

	if name := ipIntelReverse(ctx, ip); name != "" {
		intel.Reverse = name
		intel.Source = "ptr"
		if rule, ok := ipIntelMatchCarrier(name); ok {
			intel.Carrier = rule.name
			intel.CarrierKind = rule.kind
		}
	}

	if endpoint := strings.TrimSpace(os.Getenv("SRUI_IPINTEL_ENDPOINT")); endpoint != "" {
		ipIntelFromEndpoint(ctx, endpoint, ip, &intel)
	}

	if intel.CarrierKind == "" {
		intel.CarrierKind = "unknown"
	}
	intel.Mobile = intel.CarrierKind == "mobile"
	if intel.CarrierKind == "hosting" {
		intel.Hosting = true
	}
	intel.UpdatedAt = time.Now().Unix()
	return intel
}

// ipIntelLocal is the part that needs nothing but the address itself.
func ipIntelLocal(ip string) IPIntel {
	intel := IPIntel{IP: ip, Source: "local", UpdatedAt: time.Now().Unix()}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		intel.Scope = "invalid"
		intel.CarrierKind = "unknown"
		return intel
	}
	if parsed.To4() != nil {
		intel.Family = "ipv4"
	} else {
		intel.Family = "ipv6"
	}

	switch {
	case parsed.IsLoopback():
		intel.Scope = "loopback"
	case parsed.IsLinkLocalUnicast() || parsed.IsLinkLocalMulticast():
		intel.Scope = "linklocal"
	case ipIntelIsCGNAT(parsed):
		// 100.64.0.0/10 is carrier-grade NAT: the address belongs to the ISP, not
		// to a subscriber, so two customers can share one and one customer can
		// change theirs mid-session. Naming it is what stops an operator from
		// reading a shared address as account sharing.
		intel.Scope = "cgnat"
		intel.CarrierKind = "carrier-nat"
	case parsed.IsPrivate():
		intel.Scope = "private"
	default:
		intel.Scope = "public"
	}
	if intel.CarrierKind == "" && intel.Scope != "public" {
		intel.CarrierKind = "unknown"
	}
	return intel
}

func ipIntelIsCGNAT(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	return v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127
}

// ipIntelReverse does a PTR lookup under its own deadline and returns the first
// name, trimmed of the trailing dot.
func ipIntelReverse(ctx context.Context, ip string) string {
	lookupCtx, cancel := context.WithTimeout(ctx, ipIntelPTRTimeout)
	defer cancel()
	names, err := net.DefaultResolver.LookupAddr(lookupCtx, ip)
	if err != nil || len(names) == 0 {
		return ""
	}
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(names[0])), ".")
}

func ipIntelMatchCarrier(subject string) (ipIntelCarrierRule, bool) {
	subject = strings.ToLower(subject)
	if subject == "" {
		return ipIntelCarrierRule{}, false
	}
	for _, rule := range ipIntelCarrierRules {
		if strings.Contains(subject, rule.match) {
			return rule, true
		}
	}
	return ipIntelCarrierRule{}, false
}

// ipIntelFromEndpoint queries the operator-configured enrichment service.
//
// SRUI_IPINTEL_ENDPOINT is a URL template containing {ip}. Both of the common
// response shapes are accepted, because the field names differ per service and
// an operator should not have to care:
//
//	ip-api style:  country, countryCode, city, isp, org, as, mobile, proxy, hosting
//	ipinfo style:  country, city, org ("AS12345 Example"), asn
//
// Anything missing is left as the offline pipeline set it.
func ipIntelFromEndpoint(ctx context.Context, endpoint, ip string, intel *IPIntel) {
	url := strings.ReplaceAll(endpoint, "{ip}", ip)
	if !strings.Contains(url, "://") {
		url = "ht" + "tps" + "://" + url
	}

	reqCtx, cancel := context.WithTimeout(ctx, ipIntelEndpointTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return
	}

	get := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := payload[k]; ok {
				if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
			}
		}
		return ""
	}
	flag := func(key string) bool {
		v, ok := payload[key]
		if !ok {
			return false
		}
		b, ok := v.(bool)
		return ok && b
	}

	if v := get("country"); v != "" {
		intel.Country = v
	}
	if v := get("countryCode", "country_code"); v != "" {
		intel.CountryCode = v
	}
	if v := get("city"); v != "" {
		intel.City = v
	}
	if v := get("isp", "org", "organization"); v != "" {
		intel.Org = v
	}
	if v := get("as", "asn"); v != "" {
		intel.ASN = v
	}
	if flag("proxy") {
		intel.Proxy = true
	}
	if flag("hosting") {
		intel.Hosting = true
		if intel.CarrierKind == "" {
			intel.CarrierKind = "hosting"
		}
	}
	if flag("mobile") {
		intel.Mobile = true
		if intel.CarrierKind == "" || intel.CarrierKind == "unknown" {
			intel.CarrierKind = "mobile"
		}
	}

	// The org string is the second chance for the carrier table: a mobile pool
	// with no PTR still says "MTN Irancell" in its org.
	if intel.Carrier == "" {
		if rule, ok := ipIntelMatchCarrier(intel.Org + " " + intel.ASN); ok {
			intel.Carrier = rule.name
			if intel.CarrierKind == "" || intel.CarrierKind == "unknown" {
				intel.CarrierKind = rule.kind
			}
		} else if intel.Org != "" {
			intel.Carrier = intel.Org
		}
	}
	intel.Source = "endpoint"
}
