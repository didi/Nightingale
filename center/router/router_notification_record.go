package router

import (
	"strings"

	"github.com/ccfos/nightingale/v6/alert/sender"
	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ctx"

	"github.com/gin-gonic/gin"
	"github.com/toolkits/pkg/ginx"
	"github.com/toolkits/pkg/logger"
)

type NotificationResponse struct {
	SubRules []SubRule           `json:"sub_rules"`
	Notifies map[string][]Record `json:"notifies"`
}

type SubRule struct {
	SubID    int64               `json:"sub_id"`
	Notifies map[string][]Record `json:"notifies"`
}

type Notify struct {
	Channel string   `json:"channel"`
	Records []Record `json:"records"`
}

type Record struct {
	Target   string `json:"target"`
	Username string `json:"username"`
	Status   int    `json:"status"`
	Detail   string `json:"detail"`
}

// notificationRecordAdd
func (rt *Router) notificationRecordAdd(c *gin.Context) {
	var req []*models.NotificaitonRecord
	ginx.BindJSON(c, &req)
	err := sender.PushNotifyRecords(req)
	ginx.Dangerous(err, 429)
	
	ginx.NewRender(c).Data(nil, err)
}

func (rt *Router) notificationRecordList(c *gin.Context) {
	eid := ginx.UrlParamInt64(c, "eid")
	lst, err := models.NotificaitonRecordsGetByEventId(rt.Ctx, eid)
	ginx.Dangerous(err)

	response := buildNotificationResponse(rt.Ctx, lst)
	ginx.NewRender(c).Data(response, nil)
}

func buildNotificationResponse(ctx *ctx.Context, nl []*models.NotificaitonRecord) NotificationResponse {
	response := NotificationResponse{
		SubRules: []SubRule{},
		Notifies: make(map[string][]Record),
	}

	subRuleMap := make(map[int64]*SubRule)

	// Collect all group IDs
	groupIdSet := make(map[int64]struct{})

	// map[SubId]map[Channel]map[Target]index
	filter := make(map[int64]map[string]map[string]int)

	for i, n := range nl {
		// 对相同的 channel-target 进行合并
		for _, gid := range n.GetGroupIds(ctx) {
			groupIdSet[gid] = struct{}{}
		}

		if _, exists := filter[n.SubId]; !exists {
			filter[n.SubId] = make(map[string]map[string]int)
		}

		if _, exists := filter[n.SubId][n.Channel]; !exists {
			filter[n.SubId][n.Channel] = make(map[string]int)
		}

		idx, exists := filter[n.SubId][n.Channel][n.Target]
		if !exists {
			filter[n.SubId][n.Channel][n.Target] = i
		} else {
			if nl[idx].Status < n.Status {
				nl[idx].Status = n.Status
			}
			nl[idx].Details = nl[idx].Details + ", " + n.Details
			nl[i] = nil
		}

	}

	// Fill usernames only once
	usernameByTarget := fillUserNames(ctx, groupIdSet)

	for _, n := range nl {
		if n == nil {
			continue
		}

		m := usernameByTarget[n.Target]
		usernames := make([]string, 0, len(m))
		for k := range m {
			usernames = append(usernames, k)
		}

		if !checkChannel(n.Channel) {
			// Hide sensitive information
			n.Target = replaceLastEightChars(n.Target)
		}
		record := Record{
			Target: n.Target,
			Status: n.Status,
			Detail: n.Details,
		}

		record.Username = strings.Join(usernames, ",")

		if n.SubId > 0 {
			// Handle SubRules
			subRule, ok := subRuleMap[n.SubId]
			if !ok {
				newSubRule := &SubRule{
					SubID: n.SubId,
				}
				newSubRule.Notifies = make(map[string][]Record)
				newSubRule.Notifies[n.Channel] = []Record{record}

				subRuleMap[n.SubId] = newSubRule
			} else {
				if _, exists := subRule.Notifies[n.Channel]; !exists {

					subRule.Notifies[n.Channel] = []Record{record}
				} else {
					subRule.Notifies[n.Channel] = append(subRule.Notifies[n.Channel], record)
				}
			}
			continue
		}

		if response.Notifies == nil {
			response.Notifies = make(map[string][]Record)
		}

		if _, exists := response.Notifies[n.Channel]; !exists {
			response.Notifies[n.Channel] = []Record{record}
		} else {
			response.Notifies[n.Channel] = append(response.Notifies[n.Channel], record)
		}
	}

	for _, subRule := range subRuleMap {
		response.SubRules = append(response.SubRules, *subRule)
	}

	return response
}

// check channel is one of the following:  tx-sms, tx-voice, ali-sms, ali-voice, email, script
func checkChannel(channel string) bool {
	switch channel {
	case "tx-sms", "tx-voice", "ali-sms", "ali-voice", "email", "script":
		return true
	}
	return false
}

func replaceLastEightChars(s string) string {
	if len(s) <= 8 {
		return strings.Repeat("*", len(s))
	}
	return s[:len(s)-8] + strings.Repeat("*", 8)
}

func fillUserNames(ctx *ctx.Context, groupIdSet map[int64]struct{}) map[string]map[string]struct{} {
	userNameByTarget := make(map[string]map[string]struct{})

	gids := make([]int64, 0, len(groupIdSet))
	for gid := range groupIdSet {
		gids = append(gids, gid)
	}

	users, err := models.UsersGetByGroupIds(ctx, gids)
	if err != nil {
		logger.Errorf("UsersGetByGroupIds failed, err: %v", err)
		return userNameByTarget
	}

	for _, user := range users {
		logger.Warningf("user: %s", user.Username)
		for _, ch := range models.DefaultChannels {
			target, exist := user.ExtractToken(ch)
			if exist {
				if _, ok := userNameByTarget[target]; !ok {
					userNameByTarget[target] = make(map[string]struct{})
				}
				userNameByTarget[target][user.Username] = struct{}{}
			}
		}
	}

	return userNameByTarget
}
