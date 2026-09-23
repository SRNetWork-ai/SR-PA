package service

import (
	"strings"
	"testing"
	"time"
)

// deviceTestSession builds a session the way the handshake does. Named apart from
// ssh_test.go's newTestSession, which deliberately builds one WITHOUT an identity.
func deviceTestSession(email, ip, fingerprint string, since time.Time) *sshSession {
	return &sshSession{
		email:       email,
		srcIP:       ip,
		fingerprint: fingerprint,
		devKey:      sshDeviceKey(ip, fingerprint),
		since:       since,
	}
}

func TestSshClientFingerprint(t *testing.T) {
	cases := []struct{ in, want string }{
		{"SSH-2.0-OpenSSH_9.6", "OpenSSH_9.6"},
		{"SSH-1.99-JSCH-0.1.54", "JSCH-0.1.54"},
		{"  SSH-2.0-paramiko_3.4.0  ", "paramiko_3.4.0"},
		{"SSH-2.0-drop\rbear\n", "dropbear"},
		{"", ""},
	}
	for _, c := range cases {
		if got := sshClientFingerprint([]byte(c.in)); got != c.want {
			t.Fatalf("sshClientFingerprint(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// Untrusted input: a client can send a banner of any length.
	long := sshClientFingerprint([]byte("SSH-2.0-" + strings.Repeat("x", 500)))
	if len(long) != 64 {
		t.Fatalf("long banner kept %d chars, want it bounded to 64", len(long))
	}
}

func TestSshIpPoolBucket(t *testing.T) {
	cases := []struct{ in, want string }{
		{"5.113.24.77", "5.113.24.0/24"},
		{"2a01:5ec0:1234:5678:9abc:def0:1:2", "2a01:5ec0:1234:5678::/64"},
		{"not-an-address", "not-an-address"},
	}
	for _, c := range cases {
		if got := sshIpPoolBucket(c.in); got != c.want {
			t.Fatalf("sshIpPoolBucket(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSshDeviceKey(t *testing.T) {
	const app = "MobileApp_2.1"

	// Same phone, same carrier pool, new address: one device.
	if a, b := sshDeviceKey("5.113.24.77", app), sshDeviceKey("5.113.24.190", app); a != b {
		t.Fatalf("re-dial in the same pool produced two devices: %q vs %q", a, b)
	}

	// Same client software, clearly different network: not merged.
	if a, b := sshDeviceKey("5.113.24.77", app), sshDeviceKey("91.98.1.4", app); a == b {
		t.Fatalf("different pools collapsed into one device: %q", a)
	}

	// Two clients behind one address are two devices.
	if a, b := sshDeviceKey("188.34.5.9", app), sshDeviceKey("188.34.5.9", "OpenSSH_9.6"); a == b {
		t.Fatalf("distinct clients behind one address collapsed: %q", a)
	}

	// No banner: the address, exactly as the limit behaved before.
	if got := sshDeviceKey("188.34.5.9", ""); got != "188.34.5.9" {
		t.Fatalf("unidentified client key = %q, want the bare address", got)
	}
}

// A session built outside the handshake has no stored key. If the limit code read the
// field directly, every such session would share the empty key, count as one device,
// and the limit would stop applying.
func TestSessionDeviceKeyFallsBackToAddress(t *testing.T) {
	a := &sshSession{email: "acct@sr", srcIP: "10.0.0.5"}
	b := &sshSession{email: "acct@sr", srcIP: "10.0.0.6"}

	if got := a.deviceKey(); got != "10.0.0.5" {
		t.Fatalf("deviceKey() = %q, want the source address", got)
	}
	if a.deviceKey() == b.deviceKey() {
		t.Fatal("sessions with no stored key collapsed into one device")
	}
}

// The false disconnect this change exists to stop: a phone re-dials, lands on another
// address in the same carrier range while its previous session is still registered, and
// an account limited to one device gets refused.
func TestAdmitTreatsCarrierChurnAsOneDevice(t *testing.T) {
	m := newSshManager()
	now := time.Now()

	first := deviceTestSession("phone@sr", "5.113.24.77", "MobileApp_2.1", now.Add(-2*time.Minute))
	if _, ok := m.admit(first, 1, "reject"); !ok {
		t.Fatal("first session refused")
	}

	redial := deviceTestSession("phone@sr", "5.113.24.190", "MobileApp_2.1", now)
	if _, ok := m.admit(redial, 1, "reject"); !ok {
		t.Fatal("re-dial inside the same carrier pool was refused at k=1")
	}
}

// The other direction: the limit must still count real devices.
func TestAdmitCountsDistinctClientsBehindOneAddress(t *testing.T) {
	m := newSshManager()
	now := time.Now()

	laptop := deviceTestSession("home@sr", "188.34.5.9", "OpenSSH_9.6", now.Add(-2*time.Minute))
	if _, ok := m.admit(laptop, 1, "reject"); !ok {
		t.Fatal("first session refused")
	}

	phone := deviceTestSession("home@sr", "188.34.5.9", "MobileApp_2.1", now)
	if _, ok := m.admit(phone, 1, "reject"); ok {
		t.Fatal("a second distinct client was admitted over k=1")
	}
}

// accept evicts the oldest DEVICE, and all of its sessions go with it.
func TestAdmitAcceptEvictsOldestDevice(t *testing.T) {
	m := newSshManager()
	now := time.Now()

	old := deviceTestSession("mix@sr", "188.34.5.9", "OpenSSH_9.6", now.Add(-10*time.Minute))
	oldSecond := deviceTestSession("mix@sr", "188.34.5.20", "OpenSSH_9.6", now.Add(-9*time.Minute))
	if _, ok := m.admit(old, 2, "accept"); !ok {
		t.Fatal("first session refused")
	}
	if _, ok := m.admit(oldSecond, 2, "accept"); !ok {
		t.Fatal("second session of the same device refused")
	}

	newer := deviceTestSession("mix@sr", "5.113.24.77", "MobileApp_2.1", now.Add(-time.Minute))
	if _, ok := m.admit(newer, 2, "accept"); !ok {
		t.Fatal("second device refused while under the limit")
	}

	third := deviceTestSession("mix@sr", "91.98.1.4", "paramiko_3.4.0", now)
	evicted, ok := m.admit(third, 2, "accept")
	if !ok {
		t.Fatal("accept strategy refused a new device")
	}
	if len(evicted) != 2 {
		t.Fatalf("evicted %d sessions, want both sessions of the oldest device", len(evicted))
	}
	for _, s := range evicted {
		if s.fingerprint != "OpenSSH_9.6" {
			t.Fatalf("evicted the wrong device: %q", s.fingerprint)
		}
	}
}
