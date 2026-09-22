package job

import (
	"encoding/json"
	"strings"

	"github.com/mhsanaei/3x-ui/v2/logger"
	"github.com/mhsanaei/3x-ui/v2/web/websocket"
	"github.com/mhsanaei/3x-ui/v2/xray"

	"github.com/valyala/fasthttp"
)

// Delivery half of the traffic tick: the optional external webhook and the
// per-admin websocket push. Split out of xray_traffic_job.go, unchanged.

func (j *XrayTrafficJob) informTrafficToExternalAPI(inboundTraffics []*xray.Traffic, clientTraffics []*xray.ClientTraffic) {
	informURL, err := j.settingService.GetExternalTrafficInformURI()
	if err != nil {
		logger.Warning("get ExternalTrafficInformURI failed:", err)
		return
	}
	requestBody, err := json.Marshal(map[string]any{"clientTraffics": clientTraffics, "inboundTraffics": inboundTraffics})
	if err != nil {
		logger.Warning("parse client/inbound traffic failed:", err)
		return
	}
	request := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(request)
	request.Header.SetMethod("POST")
	request.Header.SetContentType("application/json; charset=UTF-8")
	request.SetBody([]byte(requestBody))
	request.SetRequestURI(informURL)
	response := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(response)
	if err := fasthttp.Do(request, response); err != nil {
		logger.Warning("POST ExternalTrafficInformURI failed:", err)
	}
}

// broadcastTrafficScoped delivers the traffic tick to each connected admin,
// filtered to the clients they own. Super admins get the unfiltered payload.
func (j *XrayTrafficJob) broadcastTrafficScoped(
	traffics []*xray.Traffic,
	clientTraffics []*xray.ClientTraffic,
	onlineClients []string,
	onlineMemberships []string,
	lastOnlineMap map[string]int64,
) {
	hub := websocket.GetHub()
	if hub == nil {
		return
	}
	userIds := hub.ConnectedUserIds()
	if len(userIds) == 0 {
		return
	}

	supers, err := j.adminService.SuperAdminIds()
	if err != nil {
		logger.Warning("traffic broadcast: cannot load super admins, skipping:", err)
		return
	}
	// Only pay for the access map if a non-super admin is actually watching.
	var access map[string]map[int]bool
	for _, id := range userIds {
		if !supers[id] {
			access, err = j.adminService.ClientEmailAccess()
			if err != nil {
				logger.Warning("traffic broadcast: cannot load client access, skipping:", err)
				return
			}
			break
		}
	}

	for _, userId := range userIds {
		if supers[userId] {
			websocket.BroadcastTrafficToUser(userId, map[string]any{
				"traffics":          traffics,
				"clientTraffics":    clientTraffics,
				"onlineClients":     onlineClients,
				"onlineMemberships": onlineMemberships,
				"lastOnlineMap":     lastOnlineMap,
			})
			continue
		}
		mine := make([]*xray.ClientTraffic, 0, len(clientTraffics))
		for _, ct := range clientTraffics {
			if access[ct.Email][userId] {
				mine = append(mine, ct)
			}
		}
		myOnline := make([]string, 0, len(onlineClients))
		for _, email := range onlineClients {
			if access[email][userId] {
				myOnline = append(myOnline, email)
			}
		}
		// Scoped on the email half of the pair, exactly as the list above is: the
		// pairs name the same clients, and shipping them unfiltered would hand a
		// delegated admin the emails the filtering exists to withhold.
		myMemberships := make([]string, 0, len(onlineMemberships))
		for _, pair := range onlineMemberships {
			if _, email, found := strings.Cut(pair, ":"); found && access[email][userId] {
				myMemberships = append(myMemberships, pair)
			}
		}
		myLastOnline := make(map[string]int64, len(lastOnlineMap))
		for email, t := range lastOnlineMap {
			if access[email][userId] {
				myLastOnline[email] = t
			}
		}
		// traffics is inbound-level and keyed by Xray tag rather than email, so it is
		// omitted for non-super admins rather than shipped unfiltered. The per-client
		// figures above are what the inbounds table renders.
		websocket.BroadcastTrafficToUser(userId, map[string]any{
			"clientTraffics":    mine,
			"onlineClients":     myOnline,
			"onlineMemberships": myMemberships,
			"lastOnlineMap":     myLastOnline,
		})
	}
}
