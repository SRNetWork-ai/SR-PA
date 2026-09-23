package controller

import (
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/web/service"
)

// DeviceController serves the per-account device registry: what is actually
// connected, rather than which addresses were logged.
//
// Scoping note, because this is the part that is easy to get wrong: every route
// here is keyed by :email and therefore returns ONE ACCOUNT'S data. That makes
// it exactly as sensitive as POST /panel/api/inbounds/clientIps/:email, so it
// takes the same pair of guards that route takes - the inbounds permission on
// the group, plus requireClientAccess() per route, which is what answers "is
// this caller allowed to see THIS account" for admins and resellers alike.
// Shipping it with only the group permission would have been a working feature
// and a cross-tenant read at the same time.
type DeviceController struct {
	BaseController
}

// NewDeviceController creates a new DeviceController instance and initializes its routes.
func NewDeviceController(g *gin.RouterGroup) *DeviceController {
	a := &DeviceController{}
	a.initRouter(g)
	return a
}

func (a *DeviceController) initRouter(g *gin.RouterGroup) {
	ownsClient := requireClientAccess()

	// GET and POST both answer the read: POST matches the existing clientIps
	// idiom the panel's own pages use, GET is what any external caller expects.
	g.GET("/list/:email", ownsClient, a.listDevices)
	g.POST("/list/:email", ownsClient, a.listDevices)

	// Forgetting history is a write against the account, so it asks for the
	// client-edit claim, mirroring clearClientIps/:email.
	g.POST("/forget/:email", requirePerm(model.PermEditClient), ownsClient, a.forgetDevices)
}

// listDevices returns the account's devices, newest sighting first, each one
// annotated with what is known about its current address.
//
// The response separates the two counts on purpose. "total" is history and
// "active" is what a device cap should be compared against; conflating them is
// how an operator ends up disconnecting a customer over a phone that was last
// seen a week ago.
func (a *DeviceController) listDevices(c *gin.Context) {
	email := strings.TrimSpace(c.Param("email"))
	if email == "" {
		jsonObj(c, gin.H{"email": "", "devices": []service.DeviceRow{}, "total": 0, "active": 0}, nil)
		return
	}

	rows, err := service.ListAccountDevices(email)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}

	jsonObj(c, gin.H{
		"email":   email,
		"devices": rows,
		"total":   len(rows),
		"active":  service.CountAccountDevices(email, 0),
	}, nil)
}

// forgetDevices clears one account's device history.
func (a *DeviceController) forgetDevices(c *gin.Context) {
	email := strings.TrimSpace(c.Param("email"))
	if email == "" {
		jsonMsg(c, "Devices cleared", nil)
		return
	}
	if err := service.ForgetAccountDevices(email); err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	jsonMsg(c, "Devices cleared", nil)
}
