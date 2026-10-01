package model

// ApiToken is a credential for a program rather than a person: a script, a
// billing system, another panel, a monitoring probe.
//
// It deliberately holds no rights of its own. Every token belongs to an admin
// and is resolved against that admin's live row on each request, so the question
// "what may this token do" always has the same answer as "what may its owner do,
// narrowed by its scope". Tokens that carry independent rights are the ones that
// are still working months after the person they were issued for was removed.
type ApiToken struct {
	Id   int    `json:"id" gorm:"primaryKey;autoIncrement"`
	Name string `json:"name"`

	// Only the hash is stored, so the clear token exists exactly once: in the
	// response to the call that minted it. The hint is the few characters needed
	// to tell two tokens apart in a list, and is useless on its own.
	TokenHash string `json:"-" gorm:"index"`
	TokenHint string `json:"tokenHint"`

	// Owner. Username is denormalised for the list view: an admin can be renamed
	// or deleted, and a token list that cannot say who created a credential is a
	// token list nobody dares revoke from.
	UserId   int    `json:"userId" gorm:"index"`
	Username string `json:"username"`

	// Scope, in the same bitmask the Admins page edits. Intersected with the
	// owner's mask at request time rather than at mint time, so narrowing an
	// admin narrows their tokens immediately instead of leaving a credential
	// behind that remembers rights the account no longer has.
	Permissions Permission `json:"-"`

	// SuperAdmin must be asked for explicitly. It is the only way a token reaches
	// the escalation-class routes, and it is refused unless the owner is a super
	// admin at the moment of the request.
	SuperAdmin bool `json:"superAdmin"`

	Enable bool `json:"enable"`

	// ExpiresAt is a unix second, or 0 for a token that never expires. Zero is
	// allowed because some integrations genuinely outlive any date you would pick,
	// and a credential that dies unannounced at 3am is its own kind of outage.
	ExpiresAt int64 `json:"expiresAt"`

	LastUsedAt   int64  `json:"lastUsedAt"`
	LastUsedFrom string `json:"lastUsedFrom"`

	CreatedAt int64  `json:"createdAt"`
	CreatedBy string `json:"createdBy"`
}
