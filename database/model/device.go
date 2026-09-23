package model

// DeviceSession is one DEVICE observed on an account, which is not the same
// thing as one address.
//
// The panel used to store addresses only (InboundClientIps), and every question
// an operator actually asks of that list is unanswerable from it:
//
//	"is this account shared?"      five addresses on a mobile carrier is one
//	                               phone reconnecting; two addresses in two
//	                               countries is sharing.
//	"why was my customer cut off?" because a cap counted addresses, and their
//	                               ISP hands out a new one every few hours.
//	"what is connected?"           an address is not a device. A phone, a
//	                               router and a laptop behind one home line are
//	                               one address and three devices.
//
// So the unit of record here is a device identity, and the address is one of its
// attributes rather than its name. DeviceKey holds that identity and is unique
// per account: see the service layer for how it is derived, but the short
// version is strongest-signal-first, from a real client fingerprint down to the
// bare address when nothing better exists.
type DeviceSession struct {
	Id int `json:"id" gorm:"primaryKey;autoIncrement"`

	// One row per (account, device). The unique index is what makes an
	// observation an upsert rather than an append, which matters because
	// observations arrive every tick for as long as a device stays connected.
	ClientEmail string `json:"clientEmail" gorm:"uniqueIndex:idx_device_account_key,priority:1"`
	DeviceKey   string `json:"deviceKey" gorm:"uniqueIndex:idx_device_account_key,priority:2"`

	// Kind says WHICH signal identified this device, so the panel can be honest
	// about confidence instead of presenting a guess as a fact:
	//
	//	fingerprint  the client told us what it is (SSH version banner,
	//	             OpenVPN peer-info, RADIUS calling-station-id). Reliable.
	//	sim          a mobile carrier pool. One subscriber, many addresses.
	//	shared-nat   carrier-grade NAT. May be several subscribers.
	//	address      nothing but the address was available.
	Kind  string `json:"kind"`
	Label string `json:"label"`

	Fingerprint string `json:"fingerprint"`
	Protocol    string `json:"protocol"`
	InboundId   int    `json:"inboundId"`
	NodeName    string `json:"nodeName"`

	Carrier     string `json:"carrier"`
	CarrierKind string `json:"carrierKind"`
	CountryCode string `json:"countryCode"`

	// LastIp is the most recent address, Ips the bounded recent history. The
	// history is what turns "this device changed address" into something visible
	// rather than something an operator has to infer from a list that lost the
	// association.
	LastIp string `json:"lastIp"`
	Ips    string `json:"ips"`

	FirstSeen    int64 `json:"firstSeen"`
	LastSeen     int64 `json:"lastSeen" gorm:"index"`
	Observations int64 `json:"observations"`
}
