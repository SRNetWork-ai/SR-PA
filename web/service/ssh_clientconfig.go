package service

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/mhsanaei/3x-ui/v2/database/model"
)

// Client-facing artifacts for the SSH gateway: the share link and the rendered
// per-endpoint configs. Split out of ssh.go, unchanged.

// SshClientConfig is one rendered client artifact for an account/endpoint.
type SshClientConfig struct {
	Remark  string `json:"remark"`  // endpoint remark (external proxy), empty for the default
	Host    string `json:"host"`    // endpoint host the client dials
	Port    int    `json:"port"`    // endpoint port
	Singbox string `json:"singbox"` // a sing-box "ssh" outbound JSON (Hiddify-consumable)
	Plain   string `json:"plain"`   // plaintext host/port/user/pass block
	Link    string `json:"link"`    // an ssh share link (Shadowrocket-importable, QR-friendly)
}

// sshShareLink builds an ssh share link that mobile proxy clients (Shadowrocket)
// can import from a QR scan. The scheme is not publicly documented; Shadowrocket's
// family of schemes base64-encodes the userinfo, so we encode "user:password@host:port"
// (standard base64) and append the label as a URL fragment. Kept as a single source of
// truth so the modal QR and the bulk export render the same link. NOTE: verify against a
// live Shadowrocket import; the encoder is trivial to adjust if the field order differs.
func sshShareLink(user, password, host string, port int, label string) string {
	userinfo := fmt.Sprintf("%s:%s@%s:%s", user, password, host, strconv.Itoa(port))
	link := "ssh" + "://" + base64.StdEncoding.EncodeToString([]byte(userinfo))
	if strings.TrimSpace(label) != "" {
		link += "#" + url.QueryEscape(label)
	}
	return link
}

// RenderClientConfigs returns the client artifacts for the account with the given
// email: a sing-box "ssh" outbound JSON plus a plaintext block, one per endpoint
// (each external proxy, else a single default at the panel-access host).
func (s *SshService) RenderClientConfigs(inbound *model.Inbound, email, endpointHost string) ([]SshClientConfig, error) {
	settings, err := s.parseSettings(inbound)
	if err != nil {
		return nil, err
	}

	type endpointTarget struct {
		host   string
		port   int
		remark string
	}
	var targets []endpointTarget
	for _, ep := range settings.ExternalProxy {
		dest := strings.TrimSpace(ep.Dest)
		if dest == "" {
			continue
		}
		port := ep.Port
		if port <= 0 {
			port = inbound.Port
		}
		targets = append(targets, endpointTarget{host: dest, port: port, remark: strings.TrimSpace(ep.Remark)})
	}
	if len(targets) == 0 {
		// Parity with OpenVPN: an explicit non-wildcard Listen address wins over the
		// panel host, so an operator who pins the inbound to a public IP gets it.
		host := endpointHost
		if l := strings.TrimSpace(inbound.Listen); l != "" && l != "0.0.0.0" {
			host = l
		}
		targets = append(targets, endpointTarget{host: host, port: inbound.Port})
	}

	var acct *sshClient
	for i := range settings.Clients {
		if settings.Clients[i].Email == email {
			acct = &settings.Clients[i]
			break
		}
	}
	if acct == nil {
		return nil, fmt.Errorf("account not found for email %q", email)
	}

	var out []SshClientConfig
	for _, t := range targets {
		singbox, err := json.MarshalIndent(map[string]any{
			"type":        "ssh",
			"tag":         "ssh-out",
			"server":      t.host,
			"server_port": t.port,
			"user":        acct.ID,
			"password":    acct.Password,
		}, "", "  ")
		if err != nil {
			continue
		}
		// The UDPGW line is not decoration: clients that tunnel UDP (HTTP Injector,
		// NapsternetV and friends) ask for a udpgw endpoint, and users read its
		// absence as "this server has no UDP support". It does — handleDirectTCPIP
		// terminates the udpgw protocol in-process on this loopback port — so the
		// address to enter is published alongside the credentials.
		plain := fmt.Sprintf("Host: %s\nPort: %d\nUsername: %s\nPassword: %s\nUDPGW: 127.0.0.1:%d\n",
			t.host, t.port, acct.ID, acct.Password, sshUdpgwPort)
		label := t.remark
		if label == "" {
			label = email
		}
		out = append(out, SshClientConfig{
			Remark:  t.remark,
			Host:    t.host,
			Port:    t.port,
			Singbox: string(singbox),
			Plain:   plain,
			Link:    sshShareLink(acct.ID, acct.Password, t.host, t.port, label),
		})
	}
	return out, nil
}
