package controller

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/logger"
	"github.com/mhsanaei/3x-ui/v2/web/service"
	"github.com/mhsanaei/3x-ui/v2/web/session"
)

// apiTokenCtxKey marks a request as token-authenticated. Its presence, not its
// value, is what the guards below read.
const apiTokenCtxKey = "api_token_id"

// apiTokenAuth lets a program authenticate to the panel API with a bearer token.
//
// Placed BEFORE the API's own session check, and written to be invisible unless
// it is needed:
//
//   - A request with a session passes straight through. An operator clicking
//     around the panel must never be re-authenticated by this, and a token
//     header accidentally left on a browser request must not quietly downgrade
//     the rights of the person sending it.
//   - A request with neither session nor token also passes through, to be refused
//     by checkAPIAuth with the panel's usual 404. Answering 401 here would turn
//     every endpoint into a confirmation that it exists.
//   - A request that DID present a token gets 401 and the reason. The caller is a
//     script, and "expired", "disabled" and "its admin was removed" are three
//     different nights of debugging.
func apiTokenAuth() gin.HandlerFunc {
	tokens := service.ApiTokenService{}
	return func(c *gin.Context) {
		if session.IsLogin(c) {
			c.Next()
			return
		}
		raw := apiTokenFromRequest(c)
		if raw == "" {
			c.Next()
			return
		}
		tok, user, err := tokens.Authenticate(raw, c.ClientIP())
		if err != nil {
			// Logged on the panel side too: a token failing in someone else's
			// cron job is diagnosed from this log far more often than from theirs.
			logger.Warning("api token refused:", err)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"success": false, "msg": err.Error()})
			return
		}
		// The scoped copy, for this request only. Every permission gate in the
		// panel reads it through session.GetLoginUser, so they all apply to the
		// token without having been taught what a token is.
		session.SetRequestUser(c, user)
		c.Set(apiTokenCtxKey, tok.Id)
		c.Next()
	}
}

// apiTokenFromRequest reads the credential from wherever a client would put it.
//
// The query parameter is last and is a concession, not a recommendation: it ends
// up in access logs and browser history. It is accepted because the alternative
// is operators pasting tokens into tools that cannot set headers and concluding
// the API is broken.
func apiTokenFromRequest(c *gin.Context) string {
	if h := strings.TrimSpace(c.GetHeader("Authorization")); h != "" {
		if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
			return strings.TrimSpace(h[7:])
		}
		return h
	}
	if h := strings.TrimSpace(c.GetHeader("X-API-Token")); h != "" {
		return h
	}
	return strings.TrimSpace(c.Query("apiToken"))
}

// denyApiToken refuses a route group to token-authenticated callers.
//
// Used on token management itself. A token that can mint tokens launders its own
// scope: it would take one call to issue a wider credential, and the narrowing
// that makes the whole design safe would be worth nothing. Signing in to the
// panel is required for that, deliberately.
func denyApiToken() gin.HandlerFunc {
	return func(c *gin.Context) {
		if _, ok := c.Get(apiTokenCtxKey); ok {
			pureJsonMsg(c, http.StatusOK, false,
				"API tokens cannot manage API tokens. Sign in to the panel to issue or revoke one.")
			c.Abort()
			return
		}
		c.Next()
	}
}

// ApiTokenController is the panel's token management surface.
type ApiTokenController struct {
	BaseController
	tokenService service.ApiTokenService
}

func NewApiTokenController(g *gin.RouterGroup) *ApiTokenController {
	a := &ApiTokenController{}
	a.initRouter(g)
	return a
}

func (a *ApiTokenController) initRouter(g *gin.RouterGroup) {
	// Super admin, and never a token. Issuing a credential that carries an
	// admin's rights is the same class of act as creating an admin.
	g.Use(denyApiToken(), requireSuperAdmin())

	g.GET("/list", a.list)
	g.POST("/list", a.list)
	g.GET("/scopes", a.scopes)
	g.POST("/mint", a.mint)
	g.POST("/enable/:id", a.enable)
	g.POST("/del/:id", a.del)
}

func (a *ApiTokenController) list(c *gin.Context) {
	rows, err := a.tokenService.List()
	if err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	jsonObj(c, rows, nil)
}

// scopes publishes the vocabulary rather than making callers hardcode it. These
// are the same slugs the Admins page edits, which is the point: one list of
// rights in the product, not a second one invented for the API.
func (a *ApiTokenController) scopes(c *gin.Context) {
	jsonObj(c, model.AllPermissions, nil)
}

type apiTokenMintRequest struct {
	Name       string   `json:"name" form:"name"`
	Scopes     []string `json:"scopes" form:"scopes"`
	SuperAdmin bool     `json:"superAdmin" form:"superAdmin"`
	TtlDays    int      `json:"ttlDays" form:"ttlDays"`
}

// mint issues a token for the admin making the request and returns it once.
//
// There is no field for choosing an owner. The session is the owner: an endpoint
// that accepted a user id here would be an endpoint for minting other people's
// credentials, and the audit trail would name the wrong person.
func (a *ApiTokenController) mint(c *gin.Context) {
	var req apiTokenMintRequest
	_ = c.ShouldBind(&req)
	if len(req.Scopes) == 0 {
		// A form post sends repeated fields rather than a JSON array.
		req.Scopes = append(c.PostFormArray("scopes"), c.PostFormArray("scopes[]")...)
	}

	owner := session.GetLoginUser(c)
	tok, clear, err := a.tokenService.Mint(owner, req.Name, req.Scopes, req.SuperAdmin, req.TtlDays, c.ClientIP())
	if err != nil {
		// The service's refusals are written for an operator ("this token would
		// grant nothing; pick at least one scope"), so they are shown as-is.
		jsonMsg(c, err.Error(), err)
		return
	}
	jsonObj(c, gin.H{
		// The only time this value exists outside the caller's own machine. The
		// panel stores a hash, so it cannot be shown again, and the page that
		// receives it says so.
		"token":  clear,
		"id":     tok.Id,
		"name":   tok.Name,
		"hint":   tok.TokenHint,
		"scopes": tok.Permissions.Slugs(),
		"header": "Authorization: Bearer " + clear,
	}, nil)
}

func (a *ApiTokenController) enable(c *gin.Context) {
	id, err := apiTokenRouteId(c)
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
	if err := a.tokenService.SetEnable(id, enable); err != nil {
		jsonMsg(c, err.Error(), err)
		return
	}
	jsonMsg(c, "API token updated", nil)
}

func (a *ApiTokenController) del(c *gin.Context) {
	id, err := apiTokenRouteId(c)
	if err != nil {
		jsonMsg(c, err.Error(), err)
		return
	}
	if err := a.tokenService.Delete(id); err != nil {
		jsonMsg(c, err.Error(), err)
		return
	}
	jsonMsg(c, "API token revoked", nil)
}

func apiTokenRouteId(c *gin.Context) (int, error) {
	return strconv.Atoi(strings.TrimSpace(c.Param("id")))
}
