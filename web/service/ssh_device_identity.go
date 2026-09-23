package service

// Device identity for the SSH gateway.
//
// The SSH handshake is one of the few places in this panel where the client tells us
// what it actually IS: RFC 4253 requires a version banner, and real clients put
// something meaningful in it ("OpenSSH_9.6", "JSCH-0.1.54", "paramiko_3.4.0", and the
// mobile tunnelling apps their own names). That string survives the thing that breaks
// address-based counting - a changing address - which makes it a far better answer to
// "how many devices is this account using" than the address ever was.
//
// It is still client-supplied and therefore trivially forgeable. That is fine for what
// it is used for here: counting a customer's own devices and labelling them for the
// operator. It is NOT used for authentication or authorisation anywhere.

import (
	"net"
	"strings"
	"time"
)

// sshClientFingerprint reduces a version banner to a fingerprint.
//
// The transport prefix is dropped because every client sends it and it distinguishes
// nothing. The result is sanitised and bounded: this is untrusted input that ends up in
// a database column and on an operator's screen.
func sshClientFingerprint(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	s = strings.TrimPrefix(s, "SSH-2.0-")
	s = strings.TrimPrefix(s, "SSH-1.99-")

	cleaned := make([]rune, 0, len(s))
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		cleaned = append(cleaned, r)
	}
	s = strings.TrimSpace(string(cleaned))
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

// sshDeviceKey is what the User Limit counts.
//
// With a fingerprint, a device is that client within an address POOL rather than at one
// exact address. The pool is what absorbs mobile churn: a phone that re-dials and lands
// on a neighbouring address in the same carrier range stays one device, where before it
// became a second one and pushed the account over its limit. Two different clients
// behind one home NAT now count as two, which is also what an operator means by
// "devices".
//
// Without a fingerprint this is the bare address, exactly as before. A fallback that
// silently merged unknown clients would loosen the limit for the clients least able to
// be identified.
//
// The honest limitation: a phone that jumps between carrier pools still reads as two
// devices, and two people sharing an account from one pool with the same client software
// read as one. Closing either gap needs per-address carrier data at admission time,
// which is a lookup this path cannot afford to block on.
func sshDeviceKey(srcIP, fingerprint string) string {
	if fingerprint == "" {
		return srcIP
	}
	return fingerprint + "@" + sshIpPoolBucket(srcIP)
}

// sshIpPoolBucket collapses an address into the block its provider hands out from:
// /24 for IPv4, /64 for IPv6 (one subscriber's delegated prefix).
func sshIpPoolBucket(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ip
	}
	if v4 := parsed.To4(); v4 != nil {
		block := make(net.IP, 4)
		copy(block, v4)
		block[3] = 0
		return block.String() + "/24"
	}
	v6 := parsed.To16()
	if v6 == nil {
		return ip
	}
	block := make(net.IP, 16)
	copy(block[:8], v6[:8])
	return block.String() + "/64"
}

// sshObserveDevice reports an authenticated session to the device registry.
//
// Asynchronous on purpose. The registry may resolve the address (PTR, optionally an
// enrichment endpoint) and writes a row; none of that belongs on a path a user is
// waiting on, and none of it is allowed to fail a connection.
func sshObserveDevice(email, srcIP, fingerprint string, inboundId int) {
	if email == "" {
		return
	}
	obs := DeviceObservation{
		Email:       email,
		IP:          srcIP,
		Protocol:    "ssh",
		InboundId:   inboundId,
		Fingerprint: fingerprint,
		Seen:        time.Now().Unix(),
	}
	go ObserveDevice(obs)
}
