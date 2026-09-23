package controller

import (
	"strings"

	"github.com/mhsanaei/3x-ui/v2/web/service"

	"github.com/gin-gonic/gin"
)

// Source-address intelligence for the IP-limit views.
//
// The panel already knows WHICH addresses an account was used from: the IP log
// job records them and the client modal lists them. What it could never say is
// WHAT those addresses are, and that is the only thing that makes the list
// actionable. Five addresses on one account is either a customer whose phone
// re-dialled a mobile carrier five times, which is normal and must not be
// punished, or one credential shared with four other people, which is the thing
// a device cap exists to catch. Printing "5" cannot distinguish them; naming the
// network behind each address can.
//
// READ ONLY, and deliberately address-in / address-out: the caller passes the
// addresses it is looking at, and gets annotations back. It does not accept an
// account identifier and go read that account's addresses itself, because that
// would be a second path to an account's data with its own scoping to get wrong.
// The addresses come from the existing per-client route, which already narrows
// them to the caller's own clients, so this route cannot widen what anyone sees.
type IPIntelController struct {
	BaseController
}

func NewIPIntelController(g *gin.RouterGroup) *IPIntelController {
	a := &IPIntelController{}
	a.initRouter(g)
	return a
}

func (a *IPIntelController) initRouter(g *gin.RouterGroup) {
	g.GET("/status", a.status)
	g.POST("/resolve", a.resolve)
}

// ipIntelMaxPerRequest caps one request's work. Each miss can cost a reverse DNS
// lookup and an optional HTTP call, so an unbounded list would let one panel
// request hold resolver capacity for as long as it liked.
const ipIntelMaxPerRequest = 256

// status tells the UI how much detail to expect, so it can label a partial
// answer honestly instead of rendering empty columns that look like a bug.
//
// The offline path (scope, carrier-grade NAT detection, reverse DNS, the
// compiled-in carrier table) is always available. Geo and ASN fields only appear
// when the operator has configured an enrichment endpoint, which many
// deployments cannot or will not do.
func (a *IPIntelController) status(c *gin.Context) {
	jsonObj(c, gin.H{
		"offline":    true,
		"enrichment": service.IPIntelEnabled(),
		"fields": []string{
			"scope", "family", "reverse", "carrier", "carrierKind",
			"asn", "org", "country", "countryCode", "city",
			"mobile", "hosting", "proxy",
		},
	}, nil)
}

type ipIntelResolveRequest struct {
	Ips []string `json:"ips" form:"ips"`
}

// resolve annotates a list of addresses.
//
// An unparsable or empty request is an empty result rather than an error: this
// decorates a view, and a decoration that fails must never take the view with
// it. The same reasoning applies per address inside the service, where an
// address that cannot be resolved comes back with its offline fields filled in.
func (a *IPIntelController) resolve(c *gin.Context) {
	ips := ipIntelRequestedAddresses(c)
	if len(ips) == 0 {
		jsonObj(c, map[string]service.IPIntel{}, nil)
		return
	}
	jsonObj(c, service.IPIntelBatch(ips), nil)
}

// ipIntelRequestedAddresses reads the address list out of whichever shape the
// caller used: a JSON body, repeated form fields, or one comma-separated value.
// The panel's own pages post form bodies while scripts and the documented API
// post JSON, and an endpoint that only accepted one of those would be a
// pointless trap.
func ipIntelRequestedAddresses(c *gin.Context) []string {
	var req ipIntelResolveRequest
	_ = c.ShouldBindJSON(&req)

	raw := make([]string, 0, len(req.Ips)+4)
	raw = append(raw, req.Ips...)
	raw = append(raw, c.PostFormArray("ips")...)
	raw = append(raw, c.PostFormArray("ips[]")...)
	if v := c.PostForm("ips"); v != "" {
		raw = append(raw, strings.Split(v, ",")...)
	}
	if v := c.Query("ips"); v != "" {
		raw = append(raw, strings.Split(v, ",")...)
	}

	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, ip := range raw {
		ip = strings.TrimSpace(ip)
		if ip == "" {
			continue
		}
		if _, dup := seen[ip]; dup {
			continue
		}
		seen[ip] = struct{}{}
		out = append(out, ip)
		if len(out) >= ipIntelMaxPerRequest {
			break
		}
	}
	return out
}
