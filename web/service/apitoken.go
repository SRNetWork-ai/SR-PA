package service

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/logger"
)

// apiTokenBytes is the entropy behind a token. Long, because nobody types these
// and because it is the only secret between the internet and an admin's rights.
const apiTokenBytes = 32

// apiTokenTouchWindow is how stale "last used" is allowed to get.
//
// Without it every authenticated request writes a row, which turns a monitoring
// probe polling every five seconds into a steady stream of writes to the same
// page of a SQLite file that the whole panel shares. A minute of imprecision in
// an audit column is worth incomparably more than that.
const apiTokenTouchWindow int64 = 60

// apiTokenMaxName bounds the label. It reaches a list and nothing else.
const apiTokenMaxName = 64

var apiTokenTablesOnce sync.Once

// ensureApiTokenTables migrates the table on first use, matching how the node
// subsystem does it: a panel that never mints a token never grows the table, and
// a mistake in this migration cannot break the boot path of an install that does
// not use the feature.
func ensureApiTokenTables() {
	apiTokenTablesOnce.Do(func() {
		db := database.GetDB()
		if db == nil {
			return
		}
		if err := db.AutoMigrate(&model.ApiToken{}); err != nil {
			logger.Warning("api token table migrate err:", err)
		}
	})
}

// ApiTokenService owns programmatic credentials for the panel API.
type ApiTokenService struct{}

// ApiTokenRow is one token as a list shows it: the stored row plus the two
// things the row cannot answer on its own.
type ApiTokenRow struct {
	model.ApiToken
	Scopes  []string `json:"scopes"`
	Expired bool     `json:"expired"`
}

// List returns every token, newest first. No secret material is included; the
// hash is dropped by the model's own json tag.
func (s *ApiTokenService) List() ([]ApiTokenRow, error) {
	ensureApiTokenTables()
	var tokens []model.ApiToken
	if err := database.GetDB().Model(model.ApiToken{}).Find(&tokens).Error; err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	rows := make([]ApiTokenRow, 0, len(tokens))
	for _, t := range tokens {
		rows = append(rows, ApiTokenRow{
			ApiToken: t,
			Scopes:   t.Permissions.Slugs(),
			Expired:  t.ExpiresAt > 0 && t.ExpiresAt < now,
		})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].CreatedAt > rows[j].CreatedAt })
	return rows, nil
}

// Mint issues a token for one admin and returns the clear value exactly once.
//
// The owner is passed in rather than looked up by id so that the caller's own
// session is the authority on who is minting. An endpoint that accepted a user
// id here would be an endpoint for minting other people's credentials.
func (s *ApiTokenService) Mint(owner *model.User, name string, scopes []string, superAdmin bool, ttlDays int, from string) (*model.ApiToken, string, error) {
	ensureApiTokenTables()
	if owner == nil || !owner.Enable {
		return nil, "", errors.New("no admin to issue this token for")
	}
	// Refused rather than approximated. Can() derives a reseller's rights from the
	// role and ignores their stored mask, so a scope written onto a reseller's
	// token would be accepted, stored, displayed - and enforce nothing.
	if owner.IsReseller {
		return nil, "", errors.New("reseller accounts cannot issue API tokens")
	}

	label := apiTokenTrim(name, apiTokenMaxName)
	if label == "" {
		return nil, "", errors.New("a token needs a name, so it can be recognised later and revoked")
	}

	mask := model.PermissionsFromSlugs(scopes)
	if !owner.IsSuperAdmin {
		// Cannot exceed its owner, now or ever: this is also recomputed on every
		// request, so this clamp is for honest storage rather than enforcement.
		mask &= owner.Permissions
		if superAdmin {
			return nil, "", errors.New("only a super admin can issue a super admin token")
		}
	}
	if mask == 0 && !superAdmin {
		return nil, "", errors.New("this token would grant nothing; pick at least one scope")
	}

	clear, err := apiTokenNew()
	if err != nil {
		return nil, "", err
	}
	now := time.Now()
	tok := &model.ApiToken{
		Name:         label,
		TokenHash:    apiTokenHash(clear),
		TokenHint:    apiTokenHint(clear),
		UserId:       owner.Id,
		Username:     owner.Username,
		Permissions:  mask,
		SuperAdmin:   superAdmin,
		Enable:       true,
		CreatedAt:    now.Unix(),
		CreatedBy:    apiTokenTrim(from, 64),
		LastUsedFrom: "",
	}
	if ttlDays > 0 {
		tok.ExpiresAt = now.AddDate(0, 0, ttlDays).Unix()
	}
	if err := database.GetDB().Create(tok).Error; err != nil {
		return nil, "", err
	}
	logger.Info("api token issued:", tok.Name, "for", owner.Username)
	return tok, clear, nil
}

// Authenticate resolves a presented token into the user the request should run
// as, which is a NARROWED COPY of the owner rather than the owner themselves.
//
// This is the whole design. The panel decides everything through User.Can and
// User.IsSuperAdmin, so handing those two fields a scoped copy makes every
// existing gate - permissions, super admin, per-inbound and per-client ownership
// - apply to tokens without a single one of them being taught what a token is.
// Duplicating that logic for a second credential type is how the two copies end
// up disagreeing, and the one that disagrees in the permissive direction is the
// one nobody notices.
//
// The copy is never written back to the database.
func (s *ApiTokenService) Authenticate(raw string, from string) (*model.ApiToken, *model.User, error) {
	ensureApiTokenTables()
	value := strings.TrimSpace(raw)
	if len(value) < 32 {
		return nil, nil, errors.New("invalid API token")
	}

	db := database.GetDB()
	tok := &model.ApiToken{}
	err := db.Model(model.ApiToken{}).Where("token_hash = ?", apiTokenHash(value)).First(tok).Error
	if database.IsNotFound(err) {
		return nil, nil, errors.New("invalid API token")
	}
	if err != nil {
		return nil, nil, err
	}
	if !tok.Enable {
		return nil, nil, errors.New("this API token has been disabled")
	}
	now := time.Now().Unix()
	if tok.ExpiresAt > 0 && tok.ExpiresAt < now {
		return nil, nil, errors.New("this API token has expired")
	}

	owner := &model.User{}
	err = db.Model(model.User{}).Where("id = ?", tok.UserId).First(owner).Error
	if database.IsNotFound(err) {
		// The admin is gone. The token dies with them rather than becoming an
		// ownerless credential with a stored scope and nobody responsible for it.
		return nil, nil, errors.New("the admin this token belongs to no longer exists")
	}
	if err != nil {
		return nil, nil, err
	}
	if !owner.Enable {
		return nil, nil, errors.New("the admin this token belongs to is disabled")
	}
	if owner.IsReseller {
		// Possible if the account was converted after the token was issued. Refused
		// for the same reason minting is: the scope on this row would not be
		// enforceable against a role-derived mask.
		return nil, nil, errors.New("this token belongs to a reseller account and is no longer valid")
	}

	effective := *owner
	mask := tok.Permissions
	if !owner.IsSuperAdmin {
		// Recomputed per request, which is what makes demoting an admin take effect
		// on their tokens at once instead of at their next rotation.
		mask &= owner.Permissions
	}
	effective.Permissions = mask
	// A super admin's own mask is usually empty, because Can() never reads it for
	// them. So this bit is not a restriction on top of the mask - for tokens it is
	// the difference between a credential scoped to a few slugs and one that can
	// replace the panel binary.
	effective.IsSuperAdmin = owner.IsSuperAdmin && tok.SuperAdmin

	if !effective.IsSuperAdmin && effective.Permissions == 0 {
		return nil, nil, errors.New("this token no longer grants anything; its admin's permissions were narrowed")
	}

	s.touch(tok, from, now)
	return tok, &effective, nil
}

// SetEnable turns a token off without destroying the record of it, which is what
// you want during an incident: revoking by deletion also deletes the evidence of
// when the credential was last used and by whom.
func (s *ApiTokenService) SetEnable(id int, enable bool) error {
	ensureApiTokenTables()
	if id <= 0 {
		return errors.New("no token id given")
	}
	return database.GetDB().Model(model.ApiToken{}).Where("id = ?", id).
		Updates(map[string]any{"enable": enable}).Error
}

// Delete removes a token permanently.
func (s *ApiTokenService) Delete(id int) error {
	ensureApiTokenTables()
	if id <= 0 {
		return errors.New("no token id given")
	}
	tok := &model.ApiToken{}
	err := database.GetDB().Model(model.ApiToken{}).Where("id = ?", id).First(tok).Error
	if database.IsNotFound(err) {
		return fmt.Errorf("no API token with id %d", id)
	}
	if err != nil {
		return err
	}
	logger.Info("api token revoked:", tok.Name)
	return database.GetDB().Where("id = ?", id).Delete(&model.ApiToken{}).Error
}

// touch records use, at most once per apiTokenTouchWindow. Failures are logged
// and swallowed: losing an audit timestamp must never cost a caller their
// request.
func (s *ApiTokenService) touch(tok *model.ApiToken, from string, now int64) {
	if tok.LastUsedAt > 0 && now-tok.LastUsedAt < apiTokenTouchWindow {
		return
	}
	err := database.GetDB().Model(model.ApiToken{}).Where("id = ?", tok.Id).Updates(map[string]any{
		"last_used_at":   now,
		"last_used_from": apiTokenTrim(from, 64),
	}).Error
	if err != nil {
		logger.Warning("api token touch err:", err)
	}
}

// apiTokenNew returns a fresh credential, hex so it survives a shell, a systemd
// unit, a YAML file and a copy-paste unchanged.
func apiTokenNew() (string, error) {
	buf := make([]byte, apiTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func apiTokenHash(value string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(value)))
	return hex.EncodeToString(sum[:])
}

func apiTokenHint(value string) string {
	v := strings.TrimSpace(value)
	if len(v) <= 8 {
		return ""
	}
	return v[:4] + ".." + v[len(v)-4:]
}

func apiTokenTrim(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) > max {
		return s[:max]
	}
	return s
}
