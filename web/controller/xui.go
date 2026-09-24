package controller

import (
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/web/service"

	"github.com/gin-gonic/gin"
)

// XUIController is the main controller for the vpn-ui panel, managing sub-controllers.
type XUIController struct {
	BaseController

	settingController     *SettingController
	xraySettingController *XraySettingController
	coreController        *CoreController
	adminController       *AdminController
	resellerController    *ResellerController
}

// NewXUIController creates a new XUIController and initializes its routes.
func NewXUIController(g *gin.RouterGroup) *XUIController {
	a := &XUIController{}
	a.initRouter(g)
	return a
}

// initRouter sets up the main panel routes and initializes sub-controllers.
func (a *XUIController) initRouter(g *gin.RouterGroup) {
	g = g.Group("/panel")
	g.Use(a.checkLogin)

	// Not requirePerm: the overview is the one page whose grant lives in two columns
	// (an admin's PermAccessOverview, a reseller's AllowOverview), and
	// requireOverviewAccess reads both. A denial here goes to landingPath, never
	// blindly back to this route, which is what used to make gating it impossible.
	g.GET("/", requireOverviewAccess(), a.index)
	g.GET("/inbounds", requirePerm(model.PermAccessInbounds), a.inbounds)
	// The account-centric view of the same data the Inbounds page shows, so it
	// takes the same claim.
	g.GET("/clients", requirePerm(model.PermAccessInbounds), a.clients)
	g.GET("/settings", requirePerm(model.PermPanelSettings), a.settings)
	g.GET("/xray", requirePerm(model.PermXraySettings), a.xraySettings)
	g.GET("/core", requirePerm(model.PermCoreSettings), a.coreSettings)
	// Fleet health is an operations view, so opening the page takes the same claim
	// as the rest of the server-wide settings. Everything the page can change is
	// refused by /panel/api/nodes unless the caller is a super admin: a node is
	// handed account credentials and can be pointed at any address on the
	// internet, which is closer to handing over the panel than to editing an
	// inbound.
	g.GET("/nodes", requirePerm(model.PermPanelSettings), a.nodes)
	g.GET("/admins", requireSuperAdmin(), a.admins)
	// Resellers is a permission and not requireSuperAdmin(), so a delegated admin can
	// run their own resellers. The escalation that opens (assigning someone else's
	// inbound to a reseller you then log in as) is closed in the service.
	g.GET("/resellers", requirePerm(model.PermManageResellers), a.resellers)

	a.settingController = NewSettingController(g)
	a.xraySettingController = NewXraySettingController(g)
	a.coreController = NewCoreController(g)
	a.adminController = NewAdminController(g)
	a.resellerController = NewResellerController(g)
}

// index renders the main panel index page.
//
// Who may be here at all is decided by requireOverviewAccess above, for both roles.
// A reseller used to be turned away by a redirect written out here, and the outcome
// is unchanged: their profile's allowOverview still decides, and without it
// landingPath sends them to the accounts page the role exists for. The check moved
// so that one function answers "may this caller open the overview" for the route,
// the landing resolver and the nav entry alike.
func (a *XUIController) index(c *gin.Context) {
	// The two backup-filename components the browser cannot work out for itself,
	// already sanitized, so the picker's preview shows the name /getDb will really
	// send. Resolved here rather than reimplemented in JS so there is one copy of the
	// fallback chains; the cost is that renaming the panel in this same session shows
	// in the preview only after a reload, while the download itself is always current.
	var serverService service.ServerService
	panelName, domain := serverService.BackupNameParts(browserHost(c))

	// The donate dialog on the VPN-UI tile. Rendered server-side rather than
	// fetched: the list is static, so a round trip would only add a spinner.
	html(c, "index.html", "pages.index.title", gin.H{
		"donate":            donateAddresses,
		"backup_panel_name": panelName,
		"backup_domain":     domain,
	})
}

// inbounds renders the inbounds management page.
//
// It carries which inbound dialog to serve, because the two forms are different
// templates and Go picks between them while writing the page (see
// modals/inbound_modal.html). A read failure serves the current form: the panel's
// default, and the wrong thing to fail closed on.
func (a *XUIController) inbounds(c *gin.Context) {
	var settingService service.SettingService
	legacyForm, err := settingService.GetLegacyInboundForm()
	if err != nil {
		legacyForm = false
	}
	html(c, "inbounds.html", "pages.inbounds.title", gin.H{
		"legacyInboundForm": legacyForm,
	})
}

// clients renders the account-centric Clients page.
func (a *XUIController) clients(c *gin.Context) {
	html(c, "clients.html", "pages.clients.title", nil)
}

// settings renders the settings management page.
func (a *XUIController) settings(c *gin.Context) {
	html(c, "settings.html", "pages.settings.title", nil)
}

// xraySettings renders the Xray settings page.
func (a *XUIController) xraySettings(c *gin.Context) {
	html(c, "xray.html", "pages.xray.title", nil)
}

// coreSettings renders the Core Settings page (per-core status + provisioning).
func (a *XUIController) coreSettings(c *gin.Context) {
	html(c, "core.html", "pages.core.title", nil)
}

// nodes renders the Nodes page: the fleet, its relay chains and the join tokens
// new machines enroll with.
func (a *XUIController) nodes(c *gin.Context) {
	html(c, "nodes.html", "pages.nodes.title", nil)
}

// admins renders the Admins management page (super admin only).
func (a *XUIController) admins(c *gin.Context) {
	html(c, "admins.html", "pages.admins.title", nil)
}

// resellers renders the Resellers management page.
func (a *XUIController) resellers(c *gin.Context) {
	html(c, "resellers.html", "pages.resellers.title", nil)
}
