package controller

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/logger"
	"github.com/mhsanaei/3x-ui/v2/web/service"
)

// NodeController is the panel's side of the node fleet: the Nodes page and the
// API behind it.
//
// Reads open at settings level because fleet health is an operations view, but
// every mutation here asks for super admin. That is not caution for its own
// sake: a node is handed account credentials and can be pointed at any address
// on the internet, so adding one is closer to handing over the panel than to
// editing an inbound, and it must not be reachable through a delegated bit.
type NodeController struct {
	BaseController
	nodeService service.NodeService
}

func NewNodeController(g *gin.RouterGroup) *NodeController {
	a := &NodeController{}
	a.initRouter(g)
	return a
}

func (a *NodeController) initRouter(g *gin.RouterGroup) {
	// GET and POST both answer the reads: POST matches the idiom the panel's own
	// pages already post, GET is what any external caller expects.
	g.GET("/list", a.list)
	g.POST("/list", a.list)
	g.GET("/locations", a.locations)
	g.GET("/inbounds/:id", a.inbounds)

	super := requireSuperAdmin()
	g.POST("/save", super, a.save)
	g.POST("/del/:id", super, a.del)
	g.POST("/enable/:id", super, a.enable)
	g.POST("/inbounds/:id", super, a.setInbounds)
	g.POST("/token/:id", super, a.mintToken)
}

// nodeSaveRequest is an explicit allowlist of the fields an operator may set.
//
// Binding straight onto model.Node would have been shorter and wrong: the row
// also carries the node's token hash and everything the agent reports, and a
// form that can reach those columns is a form that can hand itself a node's
// credentials or fake a node's health.
type nodeSaveRequest struct {
	Id          int    `json:"id" form:"id"`
	Name        string `json:"name" form:"name"`
	Address     string `json:"address" form:"address"`
	Port        int    `json:"port" form:"port"`
	UseTLS      bool   `json:"useTls" form:"useTls"`
	Fingerprint string `json:"fingerprint" form:"fingerprint"`
	Role        string `json:"role" form:"role"`
	Enable      bool   `json:"enable" form:"enable"`
	CountryCode string `json:"countryCode" form:"countryCode"`
	City        string `json:"city" form:"city"`
	Provider    string `json:"provider" form:"provider"`
	Tags        string `json:"tags" form:"tags"`
	RelayViaId  int    `json:"relayViaId" form:"relayViaId"`
	RelayKind   string `json:"relayKind" form:"relayKind"`
	PublicHost  string `json:"publicHost" form:"publicHost"`
}

func (a *NodeController) list(c *gin.Context) {
	rows, err := a.nodeService.List()
	if err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	jsonObj(c, rows, nil)
}

func (a *NodeController) locations(c *gin.Context) {
	locations, err := a.nodeService.Locations()
	if err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	jsonObj(c, locations, nil)
}

func (a *NodeController) save(c *gin.Context) {
	var req nodeSaveRequest
	if err := c.ShouldBind(&req); err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	node := &model.Node{
		Id:          req.Id,
		Name:        req.Name,
		Address:     req.Address,
		Port:        req.Port,
		UseTLS:      req.UseTLS,
		Fingerprint: req.Fingerprint,
		Role:        req.Role,
		Enable:      req.Enable,
		CountryCode: req.CountryCode,
		City:        req.City,
		Provider:    req.Provider,
		Tags:        req.Tags,
		RelayViaId:  req.RelayViaId,
		RelayKind:   req.RelayKind,
		PublicHost:  req.PublicHost,
	}
	if err := a.nodeService.Save(node); err != nil {
		// The service's refusals are written for an operator ("that relay target
		// would form a loop"), so they are shown as-is instead of being replaced
		// by a generic failure the user can do nothing with.
		jsonMsg(c, err.Error(), err)
		return
	}
	jsonObj(c, node, nil)
}

func (a *NodeController) del(c *gin.Context) {
	id, err := nodeRouteId(c)
	if err != nil {
		jsonMsg(c, err.Error(), err)
		return
	}
	if err := a.nodeService.Delete(id); err != nil {
		jsonMsg(c, err.Error(), err)
		return
	}
	jsonMsg(c, "Node deleted", nil)
}

func (a *NodeController) enable(c *gin.Context) {
	id, err := nodeRouteId(c)
	if err != nil {
		jsonMsg(c, err.Error(), err)
		return
	}
	enable := strings.EqualFold(strings.TrimSpace(c.PostForm("enable")), "true")
	if v, ok := c.GetPostForm("enable"); !ok || v == "" {
		var body struct {
			Enable bool `json:"enable"`
		}
		_ = c.ShouldBindJSON(&body)
		enable = body.Enable
	}
	if err := a.nodeService.SetEnable(id, enable); err != nil {
		jsonMsg(c, err.Error(), err)
		return
	}
	jsonMsg(c, "Node updated", nil)
}

func (a *NodeController) inbounds(c *gin.Context) {
	id, err := nodeRouteId(c)
	if err != nil {
		jsonMsg(c, err.Error(), err)
		return
	}
	links, err := a.nodeService.Inbounds(id)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	jsonObj(c, links, nil)
}

type nodeInboundsRequest struct {
	InboundIds []int `json:"inboundIds" form:"inboundIds"`
}

func (a *NodeController) setInbounds(c *gin.Context) {
	id, err := nodeRouteId(c)
	if err != nil {
		jsonMsg(c, err.Error(), err)
		return
	}
	var req nodeInboundsRequest
	_ = c.ShouldBind(&req)
	ids := req.InboundIds
	if len(ids) == 0 {
		// A form post arrives as repeated fields rather than a JSON array, and an
		// endpoint that only understood one shape would be a trap for whichever
		// caller guessed the other.
		for _, raw := range append(c.PostFormArray("inboundIds"), c.PostFormArray("inboundIds[]")...) {
			if v, convErr := strconv.Atoi(strings.TrimSpace(raw)); convErr == nil {
				ids = append(ids, v)
			}
		}
	}
	if err := a.nodeService.SetInbounds(id, ids); err != nil {
		jsonMsg(c, err.Error(), err)
		return
	}
	jsonMsg(c, "Node inbounds updated", nil)
}

// mintToken issues a single-use join token and returns it once.
//
// The response carries the pieces an install command needs rather than a
// finished URL: the panel cannot know whether it is reached over TLS, through a
// reverse proxy, or by address, so it hands back host, base path and route and
// lets the page that has that context assemble the line the operator pastes.
func (a *NodeController) mintToken(c *gin.Context) {
	id, err := nodeRouteId(c)
	if err != nil {
		jsonMsg(c, err.Error(), err)
		return
	}
	token, err := a.nodeService.MintEnrollment(id, c.ClientIP())
	if err != nil {
		jsonMsg(c, err.Error(), err)
		return
	}
	jsonObj(c, gin.H{
		"token":      token,
		"expiresIn":  int((30 * time.Minute).Seconds()),
		"host":       c.Request.Host,
		"basePath":   c.GetString("base_path"),
		"enrollPath": "node/enroll",
		"tls":        c.Request.TLS != nil,
	}, nil)
}

func nodeRouteId(c *gin.Context) (int, error) {
	return strconv.Atoi(strings.TrimSpace(c.Param("id")))
}

// NodeAgentController is the agents' control channel.
//
// It is a separate controller, not a group inside the panel API, because an
// agent has no session and must never be sent through the panel's login
// middleware: it presents the bearer token it received at enrollment. The routes
// still sit behind the panel's secret base path, so an agent is handed a full
// URL when it enrolls and nothing scanning the internet finds them.
type NodeAgentController struct {
	nodeService service.NodeService
}

func NewNodeAgentController(g *gin.RouterGroup) *NodeAgentController {
	a := &NodeAgentController{}
	a.initRouter(g)
	return a
}

func (a *NodeAgentController) initRouter(g *gin.RouterGroup) {
	agent := g.Group("/node")
	agent.POST("/enroll", a.enroll)

	authed := agent.Group("")
	authed.Use(a.authenticate)
	authed.POST("/heartbeat", a.heartbeat)
	authed.GET("/config", a.config)
	authed.POST("/traffic", a.traffic)
}

// authenticate resolves the bearer token to a node.
//
// A refusal is 401 with a reason, not the 404 the panel API uses to hide itself.
// The caller here is a daemon on the operator's own machine, and "your token is
// no longer valid, enroll again" is the difference between a node that reports
// its own problem and one that is silently missing for a week.
func (a *NodeAgentController) authenticate(c *gin.Context) {
	node, err := a.nodeService.Authenticate(nodeBearerToken(c))
	if err != nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"success": false, "msg": err.Error()})
		return
	}
	c.Set("node_id", node.Id)
	c.Next()
}

type nodeEnrollRequest struct {
	Token        string `json:"token" form:"token"`
	AgentVersion string `json:"agentVersion" form:"agentVersion"`
	Fingerprint  string `json:"fingerprint" form:"fingerprint"`
}

// enroll trades a single-use join token for the node's long-lived one.
func (a *NodeAgentController) enroll(c *gin.Context) {
	var req nodeEnrollRequest
	_ = c.ShouldBind(&req)
	if strings.TrimSpace(req.Token) == "" {
		req.Token = strings.TrimSpace(nodeBearerToken(c))
	}

	node, token, err := a.nodeService.ConsumeEnrollment(req.Token, c.ClientIP())
	if err != nil {
		// Logged on the panel side as well: a node failing to join is diagnosed
		// from the master's log far more often than from the new machine's.
		logger.Warning("node enrollment refused:", err)
		c.JSON(http.StatusForbidden, gin.H{"success": false, "msg": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success":   true,
		"nodeToken": token,
		"node": gin.H{
			"id":         node.Id,
			"name":       node.Name,
			"role":       node.Role,
			"relayViaId": node.RelayViaId,
		},
		"heartbeatSeconds": 30,
	})
}

// heartbeat stores one report and answers with the hash the node should be
// running, so an agent learns it is behind on the same call it uses to say it is
// alive rather than polling a second endpoint to find out.
func (a *NodeAgentController) heartbeat(c *gin.Context) {
	nodeId := c.GetInt("node_id")
	var hb service.NodeHeartbeat
	_ = c.ShouldBind(&hb)

	if err := a.nodeService.ApplyHeartbeat(nodeId, hb); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "msg": err.Error()})
		return
	}

	bundle, err := a.nodeService.Bundle(nodeId)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"success": true, "serverTime": time.Now().Unix()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success":    true,
		"configHash": bundle.Hash,
		"inSync":     bundle.Hash != "" && bundle.Hash == strings.TrimSpace(hb.AppliedHash),
		"serverTime": time.Now().Unix(),
	})
}

// config hands a node its assignment.
func (a *NodeAgentController) config(c *gin.Context) {
	bundle, err := a.nodeService.Bundle(c.GetInt("node_id"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "msg": err.Error()})
		return
	}
	c.JSON(http.StatusOK, bundle)
}

// traffic takes a node's own measurement of what it served and bills it.
//
// A node is the only thing that can measure its own connections, so this is
// the one place an account's usage on a node enters the panel at all. The
// count of accepted records goes back in the answer because the agent uses it
// for nothing and an operator reading a request log uses it for everything: a
// node reporting zero accounts while its interface counters climb is a node
// whose core is serving someone the panel does not know about.
func (a *NodeAgentController) traffic(c *gin.Context) {
	nodeId := c.GetInt("node_id")
	var report service.NodeTrafficReport
	// Deliberately not fatal. A body this cannot read becomes an empty report
	// and a zero answer, which the agent retries with everything it has; a 400
	// here would cost the node its accounting for good, because the agent moves
	// its baselines on any answer it can parse.
	_ = c.ShouldBind(&report)

	accepted, err := a.nodeService.IngestTraffic(nodeId, report)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "msg": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "accepted": accepted})
}

// nodeBearerToken reads the token from whichever place a daemon, a curl command
// or a systemd unit would naturally put it.
func nodeBearerToken(c *gin.Context) string {
	if h := strings.TrimSpace(c.GetHeader("Authorization")); h != "" {
		if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
			return strings.TrimSpace(h[7:])
		}
		return h
	}
	if h := strings.TrimSpace(c.GetHeader("X-Node-Token")); h != "" {
		return h
	}
	return strings.TrimSpace(c.Query("token"))
}
