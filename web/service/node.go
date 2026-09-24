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

	"gorm.io/gorm"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/logger"
)

// nodeOnlineWindow is how long a node stays online after its last heartbeat.
// Agents beat every 30 seconds, so this tolerates two consecutive misses: a
// health column that flickers on one lost packet is a column operators learn to
// ignore, which is worse than not having one.
const nodeOnlineWindow = 95 * time.Second

// nodeEnrollTTL bounds a join token's life. It is short on purpose: the token is
// pasted into an install command on another machine a minute or two after it is
// minted, and anything longer is a live credential sitting in a scrollback
// buffer or a chat message.
const nodeEnrollTTL = 30 * time.Minute

// nodeMaxChain caps relay hops. Every hop is a real network leg with its own
// latency and its own way to fail; a chain this long is already a design
// mistake, and the cap is here so that a mistake cannot become an infinite walk
// in the config builder.
const nodeMaxChain = 4

// DefaultNodeAgentPort is where the node agent listens unless told otherwise.
const DefaultNodeAgentPort = 62789

// nodeTokenBytes is the entropy behind both token kinds. 32 bytes because these
// are bearer credentials for a machine that will be handed account data, and
// nobody has to type them.
const nodeTokenBytes = 32

// ensureNodeTables migrates the node tables the first time anything touches
// them.
//
// Lazily, rather than in database/db.go, so that a panel that never defines a
// node never grows the tables, and so this feature cannot break the boot path of
// every existing install if its migration is wrong.
var nodeTablesOnce sync.Once

func ensureNodeTables() {
	nodeTablesOnce.Do(func() {
		db := database.GetDB()
		if db == nil {
			return
		}
		if err := db.AutoMigrate(&model.Node{}, &model.NodeInbound{}, &model.NodeEnrollment{}); err != nil {
			logger.Warning("node tables migrate err:", err)
		}
	})
}

// NodeService owns nodes: the machines that serve this panel's inbounds from
// somewhere else.
type NodeService struct{}

// NodeRow is one node as the panel lists it: the stored row plus the three
// things a list has to answer and a row alone cannot (is it actually up, is it
// running what we built, and where does its traffic finally leave).
type NodeRow struct {
	model.Node
	Online   bool     `json:"online"`
	InSync   bool     `json:"inSync"`
	Inbounds int      `json:"inbounds"`
	RelayVia string   `json:"relayVia"`
	Chain    []string `json:"chain"`
}

// NodeLocation is one place traffic can leave from, for the location picker.
type NodeLocation struct {
	CountryCode string `json:"countryCode"`
	City        string `json:"city"`
	Nodes       int    `json:"nodes"`
	Online      int    `json:"online"`
}

// NodeHeartbeat is what an agent reports each tick. Everything in it is a claim
// by the far side, so every field is bounded or clamped before it is stored.
type NodeHeartbeat struct {
	AgentVersion string  `json:"agentVersion"`
	CoreVersion  string  `json:"coreVersion"`
	AppliedHash  string  `json:"appliedHash"`
	Uptime       int64   `json:"uptime"`
	CpuPct       float64 `json:"cpuPct"`
	MemPct       float64 `json:"memPct"`
	DiskPct      float64 `json:"diskPct"`
	Up           int64   `json:"up"`
	Down         int64   `json:"down"`
	LatencyMs    int     `json:"latencyMs"`
	Error        string  `json:"error"`
}

// List returns every node, name-ordered, annotated for the Nodes page.
func (s *NodeService) List() ([]NodeRow, error) {
	ensureNodeTables()
	db := database.GetDB()

	var nodes []model.Node
	if err := db.Model(model.Node{}).Find(&nodes).Error; err != nil {
		return nil, err
	}

	counts := map[int]int{}
	var links []model.NodeInbound
	if err := db.Model(model.NodeInbound{}).Find(&links).Error; err == nil {
		for _, link := range links {
			if link.Enable {
				counts[link.NodeId]++
			}
		}
	}

	byId := make(map[int]model.Node, len(nodes))
	for _, n := range nodes {
		byId[n.Id] = n
	}

	rows := make([]NodeRow, 0, len(nodes))
	for _, n := range nodes {
		row := NodeRow{Node: n}
		row.Online = nodeIsOnline(n)
		// An empty ConfigHash means nothing has been built for this node yet, and
		// two empty strings must not read as agreement.
		row.InSync = n.ConfigHash != "" && n.ConfigHash == n.AppliedHash
		row.Inbounds = counts[n.Id]
		row.Chain = nodeChainNames(byId, n)
		if via, ok := byId[n.RelayViaId]; ok {
			row.RelayVia = via.Name
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return strings.ToLower(rows[i].Name) < strings.ToLower(rows[j].Name)
	})
	return rows, nil
}

// Get loads one node by id.
func (s *NodeService) Get(id int) (*model.Node, error) {
	ensureNodeTables()
	if id <= 0 {
		return nil, errors.New("no node id given")
	}
	n := &model.Node{}
	err := database.GetDB().Model(model.Node{}).Where("id = ?", id).First(n).Error
	if database.IsNotFound(err) {
		return nil, fmt.Errorf("no node with id %d", id)
	}
	if err != nil {
		return nil, err
	}
	return n, nil
}

// GetByName loads one node by name, which is what the agent's config file and
// the install command carry.
func (s *NodeService) GetByName(name string) (*model.Node, error) {
	ensureNodeTables()
	n := &model.Node{}
	err := database.GetDB().Model(model.Node{}).Where("name = ?", strings.TrimSpace(name)).First(n).Error
	if database.IsNotFound(err) {
		return nil, fmt.Errorf("no node named %s", name)
	}
	if err != nil {
		return nil, err
	}
	return n, nil
}

// Save creates or updates a node from operator input.
//
// On update it writes an explicit column list rather than the whole struct. The
// agent-reported columns are deliberately absent: a form submitted from a page
// that was opened five minutes ago still carries the health values from then,
// and saving the struct would roll the node's status back to whatever the
// browser last saw.
func (s *NodeService) Save(n *model.Node) error {
	ensureNodeTables()
	if n == nil {
		return errors.New("no node given")
	}

	name, err := nodeCheckName(n.Name)
	if err != nil {
		return err
	}
	n.Name = name

	// An address may be typed with brackets by someone copying an IPv6 URL;
	// stored bare so it compares equal to what the agent reports.
	n.Address = strings.Trim(strings.TrimSpace(n.Address), "[]")
	if n.Address == "" {
		return errors.New("a node needs an address the panel can reach it on")
	}
	if n.Port <= 0 {
		n.Port = DefaultNodeAgentPort
	}
	if n.Port > 65535 {
		return errors.New("node port is out of range")
	}

	n.Role = nodeCheckRole(n.Role)
	if n.Role == model.NodeRoleRelay && n.RelayViaId <= 0 {
		return errors.New("a relay node needs a node to relay through")
	}
	if n.RelayViaId > 0 && n.Role != model.NodeRoleRelay {
		// Choosing a relay target IS choosing the relay role. Silently keeping
		// the pair inconsistent would leave a node that says edge and behaves
		// like a relay.
		n.Role = model.NodeRoleRelay
	}
	if err := s.validateChain(n.Id, n.RelayViaId); err != nil {
		return err
	}

	n.CountryCode = strings.ToUpper(nodeTrim(n.CountryCode, 2))
	n.City = nodeTrim(n.City, 48)
	n.Provider = nodeTrim(n.Provider, 48)
	n.Tags = nodeTrim(n.Tags, 128)
	n.PublicHost = nodeTrim(n.PublicHost, 253)
	n.RelayKind = nodeTrim(n.RelayKind, 24)
	n.Fingerprint = strings.ToLower(strings.ReplaceAll(nodeTrim(n.Fingerprint, 95), ":", ""))

	db := database.GetDB()
	now := time.Now().Unix()

	// Checked here as well as by the unique index so the panel can say which name
	// collided instead of surfacing a driver error to the operator.
	var clash int64
	q := db.Model(model.Node{}).Where("name = ?", n.Name)
	if n.Id > 0 {
		q = q.Where("id <> ?", n.Id)
	}
	if err := q.Count(&clash).Error; err != nil {
		return err
	}
	if clash > 0 {
		return fmt.Errorf("a node named %s already exists", n.Name)
	}

	if n.Id <= 0 {
		n.Status = model.NodeStatusUnknown
		n.TokenHash = ""
		n.TokenHint = ""
		n.ConfigHash = ""
		n.AppliedHash = ""
		n.CreatedAt = now
		n.UpdatedAt = now
		return db.Create(n).Error
	}

	if _, err := s.Get(n.Id); err != nil {
		return err
	}
	return db.Model(model.Node{}).Where("id = ?", n.Id).Updates(map[string]any{
		"name":         n.Name,
		"address":      n.Address,
		"port":         n.Port,
		"use_tls":      n.UseTLS,
		"fingerprint":  n.Fingerprint,
		"role":         n.Role,
		"enable":       n.Enable,
		"country_code": n.CountryCode,
		"city":         n.City,
		"provider":     n.Provider,
		"tags":         n.Tags,
		"relay_via_id": n.RelayViaId,
		"relay_kind":   n.RelayKind,
		"public_host":  n.PublicHost,
		"updated_at":   now,
	}).Error
}

// SetEnable turns a node on or off without touching anything else.
func (s *NodeService) SetEnable(id int, enable bool) error {
	ensureNodeTables()
	if _, err := s.Get(id); err != nil {
		return err
	}
	return database.GetDB().Model(model.Node{}).Where("id = ?", id).
		Updates(map[string]any{"enable": enable, "updated_at": time.Now().Unix()}).Error
}

// Delete removes a node, its inbound assignments and its unused join tokens.
func (s *NodeService) Delete(id int) error {
	ensureNodeTables()
	if _, err := s.Get(id); err != nil {
		return err
	}
	now := time.Now().Unix()
	return database.GetDB().Transaction(func(tx *gorm.DB) error {
		// A deleted node must not stay somebody's relay target: the chain walk
		// would step into a row that no longer exists and the config builder
		// would emit an outbound pointing nowhere. Detaching is the honest
		// outcome - those nodes become direct egress and the list says so.
		if err := tx.Model(model.Node{}).Where("relay_via_id = ?", id).
			Updates(map[string]any{"relay_via_id": 0, "role": model.NodeRoleEdge, "config_hash": "", "updated_at": now}).Error; err != nil {
			return err
		}
		if err := tx.Where("node_id = ?", id).Delete(&model.NodeInbound{}).Error; err != nil {
			return err
		}
		if err := tx.Where("node_id = ?", id).Delete(&model.NodeEnrollment{}).Error; err != nil {
			return err
		}
		return tx.Where("id = ?", id).Delete(&model.Node{}).Error
	})
}

// Inbounds returns the inbound assignments for one node.
func (s *NodeService) Inbounds(nodeId int) ([]model.NodeInbound, error) {
	ensureNodeTables()
	var links []model.NodeInbound
	err := database.GetDB().Model(model.NodeInbound{}).
		Where("node_id = ?", nodeId).Find(&links).Error
	return links, err
}

// SetInbounds replaces which inbounds a node serves.
//
// Unassigning disables the row instead of deleting it, so a per-node RemotePort
// survives someone toggling an inbound off and on again. That port is often the
// only value on the row an operator had to discover by hand.
func (s *NodeService) SetInbounds(nodeId int, inboundIds []int) error {
	ensureNodeTables()
	if _, err := s.Get(nodeId); err != nil {
		return err
	}
	wanted := make(map[int]bool, len(inboundIds))
	for _, id := range inboundIds {
		if id > 0 {
			wanted[id] = true
		}
	}
	now := time.Now().Unix()
	return database.GetDB().Transaction(func(tx *gorm.DB) error {
		var have []model.NodeInbound
		if err := tx.Model(model.NodeInbound{}).Where("node_id = ?", nodeId).Find(&have).Error; err != nil {
			return err
		}
		seen := make(map[int]bool, len(have))
		for _, link := range have {
			seen[link.InboundId] = true
			if link.Enable == wanted[link.InboundId] {
				continue
			}
			if err := tx.Model(model.NodeInbound{}).Where("id = ?", link.Id).
				Updates(map[string]any{"enable": wanted[link.InboundId]}).Error; err != nil {
				return err
			}
		}
		for id := range wanted {
			if seen[id] {
				continue
			}
			if err := tx.Create(&model.NodeInbound{NodeId: nodeId, InboundId: id, Enable: true}).Error; err != nil {
				return err
			}
		}
		// What the node is running no longer matches what it should run, and an
		// empty ConfigHash is how that is said: the next build sets it, and until
		// then the list shows the node as out of sync rather than claiming
		// agreement it cannot have.
		return tx.Model(model.Node{}).Where("id = ?", nodeId).
			Updates(map[string]any{"config_hash": "", "updated_at": now}).Error
	})
}

// NodesForInbound returns the enabled nodes serving one inbound, which is what
// link generation and subscriptions need: one inbound becomes one entry per
// location.
func (s *NodeService) NodesForInbound(inboundId int) ([]model.Node, error) {
	ensureNodeTables()
	db := database.GetDB()
	var links []model.NodeInbound
	if err := db.Model(model.NodeInbound{}).
		Where("inbound_id = ? AND enable = ?", inboundId, true).Find(&links).Error; err != nil {
		return nil, err
	}
	if len(links) == 0 {
		return nil, nil
	}
	ids := make([]int, 0, len(links))
	for _, link := range links {
		ids = append(ids, link.NodeId)
	}
	var nodes []model.Node
	err := db.Model(model.Node{}).Where("id IN ? AND enable = ?", ids, true).Find(&nodes).Error
	return nodes, err
}

// Locations groups enabled nodes by where they are, for a multi-location picker.
func (s *NodeService) Locations() ([]NodeLocation, error) {
	ensureNodeTables()
	var nodes []model.Node
	if err := database.GetDB().Model(model.Node{}).Where("enable = ?", true).Find(&nodes).Error; err != nil {
		return nil, err
	}
	index := map[string]*NodeLocation{}
	for _, n := range nodes {
		key := n.CountryCode + "/" + n.City
		loc, ok := index[key]
		if !ok {
			loc = &NodeLocation{CountryCode: n.CountryCode, City: n.City}
			index[key] = loc
		}
		loc.Nodes++
		if nodeIsOnline(n) {
			loc.Online++
		}
	}
	out := make([]NodeLocation, 0, len(index))
	for _, loc := range index {
		out = append(out, *loc)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CountryCode != out[j].CountryCode {
			return out[i].CountryCode < out[j].CountryCode
		}
		return out[i].City < out[j].City
	})
	return out, nil
}

// MintEnrollment issues a single-use join token for one node and returns it in
// clear exactly once. Only its hash is kept, so this is the only moment the
// panel can show it.
func (s *NodeService) MintEnrollment(nodeId int, by string) (string, error) {
	ensureNodeTables()
	n, err := s.Get(nodeId)
	if err != nil {
		return "", err
	}
	token, err := nodeNewToken()
	if err != nil {
		return "", err
	}
	now := time.Now()
	rec := &model.NodeEnrollment{
		TokenHash: nodeHashToken(token),
		TokenHint: nodeTokenHint(token),
		NodeId:    n.Id,
		NodeName:  n.Name,
		CreatedAt: now.Unix(),
		CreatedBy: nodeTrim(by, 48),
		ExpiresAt: now.Add(nodeEnrollTTL).Unix(),
	}
	if err := database.GetDB().Create(rec).Error; err != nil {
		return "", err
	}
	return token, nil
}

// ConsumeEnrollment turns a join token into the node's long-lived bearer token,
// once. Enrolling again replaces the previous bearer token, which is also how a
// node is rotated after a machine is rebuilt or a token is suspected leaked.
func (s *NodeService) ConsumeEnrollment(token string, from string) (*model.Node, string, error) {
	ensureNodeTables()
	db := database.GetDB()

	rec := &model.NodeEnrollment{}
	err := db.Model(model.NodeEnrollment{}).Where("token_hash = ?", nodeHashToken(token)).First(rec).Error
	if database.IsNotFound(err) {
		return nil, "", errors.New("unknown enrollment token")
	}
	if err != nil {
		return nil, "", err
	}
	now := time.Now().Unix()
	if rec.UsedAt > 0 {
		return nil, "", errors.New("this enrollment token was already used")
	}
	if rec.ExpiresAt > 0 && rec.ExpiresAt < now {
		return nil, "", errors.New("this enrollment token has expired")
	}
	n, err := s.Get(rec.NodeId)
	if err != nil {
		return nil, "", err
	}
	bearer, err := nodeNewToken()
	if err != nil {
		return nil, "", err
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(model.Node{}).Where("id = ?", n.Id).Updates(map[string]any{
			"token_hash": nodeHashToken(bearer),
			"token_hint": nodeTokenHint(bearer),
			"updated_at": now,
		}).Error; err != nil {
			return err
		}
		return tx.Model(model.NodeEnrollment{}).Where("id = ?", rec.Id).Updates(map[string]any{
			"used_at":   now,
			"used_from": nodeTrim(from, 64),
		}).Error
	})
	if err != nil {
		return nil, "", err
	}
	n.TokenHint = nodeTokenHint(bearer)
	return n, bearer, nil
}

// Authenticate resolves an agent's bearer token to its node.
//
// The lookup is by hash, so the stored value is never a usable credential and
// the comparison never touches the raw token. A disabled node is refused here
// rather than at the route, so disabling one in the panel actually cuts its
// control channel instead of only hiding it.
func (s *NodeService) Authenticate(token string) (*model.Node, error) {
	ensureNodeTables()
	t := strings.TrimSpace(token)
	if len(t) < 32 {
		return nil, errors.New("invalid node token")
	}
	n := &model.Node{}
	err := database.GetDB().Model(model.Node{}).Where("token_hash = ?", nodeHashToken(t)).First(n).Error
	if database.IsNotFound(err) {
		return nil, errors.New("invalid node token")
	}
	if err != nil {
		return nil, err
	}
	if !n.Enable {
		return nil, errors.New("this node is disabled")
	}
	return n, nil
}

// ApplyHeartbeat stores one agent report. Counters are stored as reported
// totals; percentages are clamped because a bad agent reading must not make the
// dashboard lie.
func (s *NodeService) ApplyHeartbeat(nodeId int, hb NodeHeartbeat) error {
	ensureNodeTables()
	now := time.Now().Unix()
	updates := map[string]any{
		"status":        model.NodeStatusOnline,
		"last_seen":     now,
		"agent_version": nodeTrim(hb.AgentVersion, 32),
		"core_version":  nodeTrim(hb.CoreVersion, 32),
		"uptime":        hb.Uptime,
		"cpu_pct":       nodeClampPct(hb.CpuPct),
		"mem_pct":       nodeClampPct(hb.MemPct),
		"disk_pct":      nodeClampPct(hb.DiskPct),
		"up":            hb.Up,
		"down":          hb.Down,
		"latency_ms":    hb.LatencyMs,
		"last_error":    nodeTrim(hb.Error, 240),
	}
	if hash := nodeTrim(hb.AppliedHash, 64); hash != "" {
		updates["applied_hash"] = hash
		updates["last_sync_at"] = now
	}
	return database.GetDB().Model(model.Node{}).Where("id = ?", nodeId).Updates(updates).Error
}

// MarkConfig records the config the master built for a node. Called by the
// config builder, and the other half of the in-sync comparison.
func (s *NodeService) MarkConfig(nodeId int, hash string) error {
	ensureNodeTables()
	return database.GetDB().Model(model.Node{}).Where("id = ?", nodeId).Updates(map[string]any{
		"config_hash": nodeTrim(hash, 64),
		"updated_at":  time.Now().Unix(),
	}).Error
}

// MarkStale turns silence into offline. Meant to be called from the traffic job,
// because a node that stops reporting stops being usable and the panel should
// say so before a customer does.
func (s *NodeService) MarkStale() error {
	ensureNodeTables()
	cutoff := time.Now().Add(-nodeOnlineWindow).Unix()
	return database.GetDB().Model(model.Node{}).
		Where("status = ? AND last_seen < ?", model.NodeStatusOnline, cutoff).
		Updates(map[string]any{"status": model.NodeStatusOffline}).Error
}

// Chain returns the node names traffic passes through, entry first and egress
// last.
func (s *NodeService) Chain(id int) ([]string, error) {
	ensureNodeTables()
	var nodes []model.Node
	if err := database.GetDB().Model(model.Node{}).Find(&nodes).Error; err != nil {
		return nil, err
	}
	byId := make(map[int]model.Node, len(nodes))
	for _, n := range nodes {
		byId[n.Id] = n
	}
	start, ok := byId[id]
	if !ok {
		return nil, fmt.Errorf("no node with id %d", id)
	}
	return nodeChainNames(byId, start), nil
}

// validateChain refuses a relay target that would loop or run deeper than
// nodeMaxChain.
//
// A loop is not a mistake that shows up later: the config builder walks the
// chain, so it would never return, and two of your own servers forwarding to
// each other is a packet storm you pay for by the gigabyte.
func (s *NodeService) validateChain(id int, via int) error {
	if via <= 0 {
		return nil
	}
	if via == id {
		return errors.New("a node cannot relay through itself")
	}
	seen := map[int]bool{}
	if id > 0 {
		seen[id] = true
	}
	hops := 0
	for cur := via; cur > 0; {
		if seen[cur] {
			return errors.New("that relay target would form a loop")
		}
		seen[cur] = true
		hops++
		if hops >= nodeMaxChain {
			return fmt.Errorf("a relay chain may not be longer than %d hops", nodeMaxChain)
		}
		next, err := s.Get(cur)
		if err != nil {
			return err
		}
		cur = next.RelayViaId
	}
	return nil
}

// nodeChainNames walks a relay chain over an already-loaded map, so a list of
// nodes costs one query rather than one per hop. The visited guard is not
// paranoia: rows predating validateChain, or edited straight in the database,
// can still contain a cycle, and this function must not be the thing that hangs
// the page.
func nodeChainNames(byId map[int]model.Node, start model.Node) []string {
	names := []string{start.Name}
	seen := map[int]bool{start.Id: true}
	for cur := start.RelayViaId; cur > 0; {
		if seen[cur] {
			names = append(names, "(loop)")
			break
		}
		next, ok := byId[cur]
		if !ok {
			names = append(names, "(missing)")
			break
		}
		seen[cur] = true
		names = append(names, next.Name)
		if len(names) > nodeMaxChain+1 {
			break
		}
		cur = next.RelayViaId
	}
	return names
}

func nodeIsOnline(n model.Node) bool {
	if n.LastSeen <= 0 {
		return false
	}
	return time.Now().Unix()-n.LastSeen <= int64(nodeOnlineWindow/time.Second)
}

// nodeNewToken returns a fresh random credential, hex so it survives being
// pasted through a shell, a systemd unit and a config file unchanged.
func nodeNewToken() (string, error) {
	buf := make([]byte, nodeTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func nodeHashToken(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

// nodeTokenHint is enough to recognise a token in a list and useless to anyone
// who has only the hint.
func nodeTokenHint(token string) string {
	t := strings.TrimSpace(token)
	if len(t) <= 8 {
		return ""
	}
	return t[:4] + ".." + t[len(t)-4:]
}

func nodeCheckName(name string) (string, error) {
	n := strings.TrimSpace(name)
	if n == "" {
		return "", errors.New("a node needs a name")
	}
	if len(n) > 48 {
		return "", errors.New("that node name is too long")
	}
	// The name reaches a systemd unit name, a config filename and a link remark,
	// so it is restricted at the door rather than escaped in four places later.
	for _, r := range n {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return "", fmt.Errorf("a node name may not contain %q; use letters, digits, dot, dash or underscore", string(r))
		}
	}
	return n, nil
}

func nodeCheckRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case model.NodeRoleRelay:
		return model.NodeRoleRelay
	default:
		return model.NodeRoleEdge
	}
}

func nodeTrim(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) > max {
		return s[:max]
	}
	return s
}

func nodeClampPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
