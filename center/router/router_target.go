package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ctx"
	"github.com/ccfos/nightingale/v6/storage"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/common/model"
	"github.com/toolkits/pkg/ginx"
	"github.com/toolkits/pkg/logger"
	"github.com/toolkits/pkg/str"
)

type TargetQuery struct {
	Filters []models.HostQuery `json:"queries"`
	P       int                `json:"p"`
	Limit   int                `json:"limit"`
}

func (rt *Router) targetGetsByHostFilter(c *gin.Context) {
	var f TargetQuery
	ginx.BindJSON(c, &f)

	query := models.GetHostsQuery(f.Filters)

	hosts, err := models.TargetGetsByFilter(rt.Ctx, query, f.Limit, (f.P-1)*f.Limit)
	ginx.Dangerous(err)

	total, err := models.TargetCountByFilter(rt.Ctx, query)
	ginx.Dangerous(err)

	ginx.NewRender(c).Data(gin.H{
		"list":  hosts,
		"total": total,
	}, nil)
}

func (rt *Router) targetGets(c *gin.Context) {
	bgids := str.IdsInt64(ginx.QueryStr(c, "gids", ""), ",")
	query := ginx.QueryStr(c, "query", "")
	limit := ginx.QueryInt(c, "limit", 30)
	downtime := ginx.QueryInt64(c, "downtime", 0)
	dsIds := queryDatasourceIds(c)

	order := ginx.QueryStr(c, "order", "ident")
	desc := ginx.QueryBool(c, "desc", false)

	hosts := queryStrListField(c, "hosts", ",", " ", "\n")

	var err error
	if len(bgids) == 0 {
		user := c.MustGet("user").(*models.User)
		if !user.IsAdmin() {
			// 如果是非 admin 用户，全部对象的情况，找到用户有权限的业务组
			var err error
			bgids, err = models.MyBusiGroupIds(rt.Ctx, user.Id)
			ginx.Dangerous(err)

			// 将未分配业务组的对象也加入到列表中
			bgids = append(bgids, 0)
		}
	}

	options := []models.BuildTargetWhereOption{
		models.BuildTargetWhereWithBgids(bgids),
		models.BuildTargetWhereWithDsIds(dsIds),
		models.BuildTargetWhereWithQuery(query),
		models.BuildTargetWhereWithDowntime(downtime),
		models.BuildTargetWhereWithHosts(hosts),
	}
	total, err := models.TargetTotal(rt.Ctx, options...)
	ginx.Dangerous(err)

	list, err := models.TargetGets(rt.Ctx, limit,
		ginx.Offset(c, limit), order, desc, options...)
	ginx.Dangerous(err)

	tgs, err := models.TargetBusiGroupsGetAll(rt.Ctx)
	ginx.Dangerous(err)

	for _, t := range list {
		t.GroupIds = tgs[t.Ident]
	}

	if err == nil {
		now := time.Now()
		cache := make(map[int64]*models.BusiGroup)

		var keys []string
		for i := 0; i < len(list); i++ {
			ginx.Dangerous(list[i].FillGroup(rt.Ctx, cache))
			keys = append(keys, models.WrapIdent(list[i].Ident))

			if now.Unix()-list[i].UpdateAt < 60 {
				list[i].TargetUp = 2
			} else if now.Unix()-list[i].UpdateAt < 180 {
				list[i].TargetUp = 1
			}
		}

		if len(keys) > 0 {
			metaMap := make(map[string]*models.HostMeta)
			vals := storage.MGet(context.Background(), rt.Redis, keys)
			for _, value := range vals {
				var meta models.HostMeta
				if value == nil {
					continue
				}
				err := json.Unmarshal(value, &meta)
				if err != nil {
					logger.Warningf("unmarshal %v host meta failed: %v", value, err)
					continue
				}
				metaMap[meta.Hostname] = &meta
			}

			for i := 0; i < len(list); i++ {
				if meta, ok := metaMap[list[i].Ident]; ok {
					list[i].FillMeta(meta)
				} else {
					// 未上报过元数据的主机，cpuNum默认为-1, 用于前端展示 unknown
					list[i].CpuNum = -1
				}
			}
		}

	}

	ginx.NewRender(c).Data(gin.H{
		"list":  list,
		"total": total,
	}, nil)
}

func (rt *Router) targetExtendInfoByIdent(c *gin.Context) {
	ident := ginx.QueryStr(c, "ident", "")
	key := models.WrapExtendIdent(ident)
	vals := storage.MGet(context.Background(), rt.Redis, []string{key})
	if len(vals) > 0 {
		extInfo := string(vals[0])
		if extInfo == "null" {
			extInfo = ""
		}
		ginx.NewRender(c).Data(gin.H{
			"extend_info": extInfo,
			"ident":       ident,
		}, nil)
		return
	}
	ginx.NewRender(c).Data(gin.H{
		"extend_info": "",
		"ident":       ident,
	}, nil)
}

func (rt *Router) targetGetsByService(c *gin.Context) {
	lst, err := models.TargetGetsAll(rt.Ctx)
	ginx.NewRender(c).Data(lst, err)
}

func (rt *Router) targetGetTags(c *gin.Context) {
	idents := ginx.QueryStr(c, "idents", "")
	idents = strings.ReplaceAll(idents, ",", " ")
	ignoreHostTag := ginx.QueryBool(c, "ignore_host_tag", false)
	lst, err := models.TargetGetTags(rt.Ctx, strings.Fields(idents), ignoreHostTag, "")
	ginx.NewRender(c).Data(lst, err)
}

type targetTagsForm struct {
	Idents  []string `json:"idents" binding:"required_without=HostIps"`
	HostIps []string `json:"host_ips" binding:"required_without=Idents"`
	Tags    []string `json:"tags" binding:"required"`
}

func (rt *Router) targetBindTagsByFE(c *gin.Context) {
	var f targetTagsForm
	var err error
	var failedResults = make(map[string]string)
	ginx.BindJSON(c, &f)

	if len(f.Idents) == 0 && len(f.HostIps) == 0 {
		ginx.Bomb(http.StatusBadRequest, "idents or host_ips must be provided")
	}
	// Acquire idents by idents and hostIps
	failedResults, f.Idents, err = models.TargetsGetIdentsByIdentsAndHostIps(rt.Ctx, f.Idents, f.HostIps)
	if err != nil {
		ginx.Bomb(http.StatusBadRequest, err.Error())
	}

	rt.checkTargetPerm(c, f.Idents)

	ginx.NewRender(c).Data(rt.targetBindTags(f, failedResults))
}

func (rt *Router) targetBindTagsByService(c *gin.Context) {
	var f targetTagsForm
	var err error
	var failedResults = make(map[string]string)
	ginx.BindJSON(c, &f)

	if len(f.Idents) == 0 && len(f.HostIps) == 0 {
		ginx.Bomb(http.StatusBadRequest, "idents or host_ips must be provided")
	}
	// Acquire idents by idents and hostIps
	failedResults, f.Idents, err = models.TargetsGetIdentsByIdentsAndHostIps(rt.Ctx, f.Idents, f.HostIps)
	if err != nil {
		ginx.Bomb(http.StatusBadRequest, err.Error())
	}

	ginx.NewRender(c).Data(rt.targetBindTags(f, failedResults))
}

func (rt *Router) targetBindTags(f targetTagsForm, failedIdents map[string]string) (map[string]string, error) {
	// 1. Check tags
	if err := rt.validateTags(f.Tags); err != nil {
		return nil, err
	}

	// 2. Acquire targets by idents
	targets, err := models.TargetsGetByIdents(rt.Ctx, f.Idents)
	if err != nil {
		return nil, err
	}

	// 3. Add tags to targets
	for _, target := range targets {
		if err = rt.addTagsToTarget(target, f.Tags); err != nil {
			failedIdents[target.Ident] = err.Error()
		}
	}

	return failedIdents, nil
}

func (rt *Router) validateTags(tags []string) error {
	for _, tag := range tags {
		arr := strings.Split(tag, "=")
		if len(arr) != 2 {
			return fmt.Errorf("invalid tag format: %s (expected format: key=value)", tag)
		}

		key, value := strings.TrimSpace(arr[0]), strings.TrimSpace(arr[1])
		if key == "" {
			return fmt.Errorf("invalid tag: key is empty in tag %s", tag)
		}
		if value == "" {
			return fmt.Errorf("invalid tag: value is empty in tag %s", tag)
		}

		if strings.Contains(key, ".") {
			return fmt.Errorf("invalid tag key: %s (key cannot contain '.')", key)
		}

		if strings.Contains(key, "-") {
			return fmt.Errorf("invalid tag key: %s (key cannot contain '-')", key)
		}

		if !model.LabelNameRE.MatchString(key) {
			return fmt.Errorf("invalid tag key: %s "+
				"(key must start with a letter or underscore, followed by letters, digits, or underscores)", key)
		}
	}

	return nil
}

func (rt *Router) addTagsToTarget(target *models.Target, tags []string) error {
	for _, tag := range tags {
		tagKey := strings.Split(tag, "=")[0]
		if _, exist := target.TagsMap[tagKey]; exist {
			return fmt.Errorf("duplicate tagkey(%s)", tagKey)
		}
	}

	return target.AddTags(rt.Ctx, tags)
}

func (rt *Router) targetUnbindTagsByFE(c *gin.Context) {
	var f targetTagsForm
	var err error
	var failedResults = make(map[string]string)
	ginx.BindJSON(c, &f)

	if len(f.Idents) == 0 && len(f.HostIps) == 0 {
		ginx.Bomb(http.StatusBadRequest, "idents or host_ips must be provided")
	}
	// Acquire idents by idents and hostIps
	failedResults, f.Idents, err = models.TargetsGetIdentsByIdentsAndHostIps(rt.Ctx, f.Idents, f.HostIps)
	if err != nil {
		ginx.Bomb(http.StatusBadRequest, err.Error())
	}

	rt.checkTargetPerm(c, f.Idents)

	ginx.NewRender(c).Data(rt.targetUnbindTags(f, failedResults))
}

func (rt *Router) targetUnbindTagsByService(c *gin.Context) {
	var f targetTagsForm
	var err error
	var failedResults = make(map[string]string)
	ginx.BindJSON(c, &f)

	if len(f.Idents) == 0 && len(f.HostIps) == 0 {
		ginx.Bomb(http.StatusBadRequest, "idents or host_ips must be provided")
	}
	// Acquire idents by idents and hostIps
	failedResults, f.Idents, err = models.TargetsGetIdentsByIdentsAndHostIps(rt.Ctx, f.Idents, f.HostIps)
	if err != nil {
		ginx.Bomb(http.StatusBadRequest, err.Error())
	}

	ginx.NewRender(c).Data(rt.targetUnbindTags(f, failedResults))
}

func (rt *Router) targetUnbindTags(f targetTagsForm, failedIdents map[string]string) (map[string]string, error) {
	// 1. Acquire targets by idents
	targets, err := models.TargetsGetByIdents(rt.Ctx, f.Idents)
	if err != nil {
		return nil, err
	}

	// 2. Remove tags from targets
	for _, target := range targets {
		err = target.DelTags(rt.Ctx, f.Tags)
		if err != nil {
			failedIdents[target.Ident] = err.Error()
			continue
		}
	}

	return failedIdents, nil
}

type targetNoteForm struct {
	Idents  []string `json:"idents" binding:"required_without=HostIps"`
	HostIps []string `json:"host_ips" binding:"required_without=Idents"`
	Note    string   `json:"note"`
}

func (rt *Router) targetUpdateNote(c *gin.Context) {
	var f targetNoteForm
	var err error
	var failedResults = make(map[string]string)
	ginx.BindJSON(c, &f)

	if len(f.Idents) == 0 && len(f.HostIps) == 0 {
		ginx.Bomb(http.StatusBadRequest, "idents or host_ips must be provided")
	}

	// Acquire idents by idents and hostIps
	failedResults, f.Idents, err = models.TargetsGetIdentsByIdentsAndHostIps(rt.Ctx, f.Idents, f.HostIps)
	if err != nil {
		ginx.Bomb(http.StatusBadRequest, err.Error())
	}

	rt.checkTargetPerm(c, f.Idents)

	ginx.NewRender(c).Data(failedResults, models.TargetUpdateNote(rt.Ctx, f.Idents, f.Note))
}

func (rt *Router) targetUpdateNoteByService(c *gin.Context) {
	var f targetNoteForm
	var err error
	var failedResults = make(map[string]string)
	ginx.BindJSON(c, &f)

	if len(f.Idents) == 0 && len(f.HostIps) == 0 {
		ginx.Bomb(http.StatusBadRequest, "idents or host_ips must be provided")
	}

	// Acquire idents by idents and hostIps
	failedResults, f.Idents, err = models.TargetsGetIdentsByIdentsAndHostIps(rt.Ctx, f.Idents, f.HostIps)
	if err != nil {
		ginx.Bomb(http.StatusBadRequest, err.Error())
	}

	ginx.NewRender(c).Data(failedResults, models.TargetUpdateNote(rt.Ctx, f.Idents, f.Note))
}

type targetBgidForm struct {
	Idents  []string `json:"idents" binding:"required_without=HostIps"`
	HostIps []string `json:"host_ips" binding:"required_without=Idents"`
	Bgid    int64    `json:"bgid"`
}

type targetBgidsForm struct {
	Idents  []string `json:"idents" binding:"required_without=HostIps"`
	HostIps []string `json:"host_ips" binding:"required_without=Idents"`
	Bgids   []int64  `json:"bgids"`
	Tags    []string `json:"tags"`
	Action  string   `json:"action"` // add del reset
}

func haveNeverGroupedIdent(ctx *ctx.Context, idents []string) (bool, error) {
	for _, ident := range idents {
		bgids, err := models.TargetGroupIdsGetByIdent(ctx, ident)
		if err != nil {
			return false, err
		}

		if len(bgids) <= 0 {
			return true, nil
		}
	}

	return false, nil
}

func (rt *Router) targetBindBgids(c *gin.Context) {
	var f targetBgidsForm
	var err error
	var failedResults = make(map[string]string)
	ginx.BindJSON(c, &f)

	if len(f.Idents) == 0 && len(f.HostIps) == 0 {
		ginx.Bomb(http.StatusBadRequest, "idents or host_ips must be provided")
	}

	// Acquire idents by idents and hostIps
	failedResults, f.Idents, err = models.TargetsGetIdentsByIdentsAndHostIps(rt.Ctx, f.Idents, f.HostIps)
	if err != nil {
		ginx.Bomb(http.StatusBadRequest, err.Error())
	}

	user := c.MustGet("user").(*models.User)
	if !user.IsAdmin() {
		// 普通用户，检查用户是否有权限操作所有请求的业务组
		existing, _, err := models.SeparateTargetIdents(rt.Ctx, f.Idents)
		ginx.Dangerous(err)
		rt.checkTargetPerm(c, existing)

		var groupIds []int64
		if f.Action == "reset" {
			// 如果是复写，则需要检查用户是否有权限操作机器之前的业务组
			bgids, err := models.TargetGroupIdsGetByIdents(rt.Ctx, f.Idents)
			ginx.Dangerous(err)

			groupIds = append(groupIds, bgids...)
		}
		groupIds = append(groupIds, f.Bgids...)

		for _, bgid := range groupIds {
			bg := BusiGroup(rt.Ctx, bgid)
			can, err := user.CanDoBusiGroup(rt.Ctx, bg, "rw")
			ginx.Dangerous(err)

			if !can {
				ginx.Bomb(http.StatusForbidden, "No permission. You are not admin of BG(%s)", bg.Name)
			}
		}
		isNeverGrouped, checkErr := haveNeverGroupedIdent(rt.Ctx, f.Idents)
		ginx.Dangerous(checkErr)

		if isNeverGrouped {
			can, err := user.CheckPerm(rt.Ctx, "/targets/bind")
			ginx.Dangerous(err)
			if !can {
				ginx.Bomb(http.StatusForbidden, "No permission. Only admin can assign BG")
			}
		}
	}

	switch f.Action {
	case "add":
		ginx.NewRender(c).Data(failedResults, models.TargetBindBgids(rt.Ctx, f.Idents, f.Bgids, f.Tags))
	case "del":
		ginx.NewRender(c).Data(failedResults, models.TargetUnbindBgids(rt.Ctx, f.Idents, f.Bgids))
	case "reset":
		ginx.NewRender(c).Data(failedResults, models.TargetOverrideBgids(rt.Ctx, f.Idents, f.Bgids, f.Tags))
	default:
		ginx.Bomb(http.StatusBadRequest, "invalid action")
	}
}

func (rt *Router) targetUpdateBgidByService(c *gin.Context) {
	var f targetBgidForm
	var err error
	var failedResults = make(map[string]string)
	ginx.BindJSON(c, &f)

	if len(f.Idents) == 0 && len(f.HostIps) == 0 {
		ginx.Bomb(http.StatusBadRequest, "idents or host_ips must be provided")
	}

	// Acquire idents by idents and hostIps
	failedResults, f.Idents, err = models.TargetsGetIdentsByIdentsAndHostIps(rt.Ctx, f.Idents, f.HostIps)
	if err != nil {
		ginx.Bomb(http.StatusBadRequest, err.Error())
	}

	ginx.NewRender(c).Data(failedResults, models.TargetOverrideBgids(rt.Ctx, f.Idents, []int64{f.Bgid}, nil))
}

type identsForm struct {
	Idents  []string `json:"idents" binding:"required_without=HostIps"`
	HostIps []string `json:"host_ips" binding:"required_without=Idents"`
}

func (rt *Router) targetDel(c *gin.Context) {
	var f identsForm
	var err error
	var failedResults = make(map[string]string)
	ginx.BindJSON(c, &f)

	if len(f.Idents) == 0 && len(f.HostIps) == 0 {
		ginx.Bomb(http.StatusBadRequest, "idents or host_ips must be provided")
	}

	// Acquire idents by idents and hostIps
	failedResults, f.Idents, err = models.TargetsGetIdentsByIdentsAndHostIps(rt.Ctx, f.Idents, f.HostIps)
	if err != nil {
		ginx.Bomb(http.StatusBadRequest, err.Error())
	}

	ginx.NewRender(c).Data(failedResults, models.TargetDel(rt.Ctx, f.Idents, rt.TargetDeleteHook))
}

func (rt *Router) targetDelByService(c *gin.Context) {
	var f identsForm
	var err error
	var failedResults = make(map[string]string)
	ginx.BindJSON(c, &f)

	if len(f.Idents) == 0 && len(f.HostIps) == 0 {
		ginx.Bomb(http.StatusBadRequest, "idents or host_ips must be provided")
	}

	// Acquire idents by idents and hostIps
	failedResults, f.Idents, err = models.TargetsGetIdentsByIdentsAndHostIps(rt.Ctx, f.Idents, f.HostIps)
	if err != nil {
		ginx.Bomb(http.StatusBadRequest, err.Error())
	}

	ginx.NewRender(c).Data(failedResults, models.TargetDel(rt.Ctx, f.Idents, rt.TargetDeleteHook))
}

func (rt *Router) checkTargetPerm(c *gin.Context, idents []string) {
	user := c.MustGet("user").(*models.User)
	nopri, err := user.NopriIdents(rt.Ctx, idents)
	ginx.Dangerous(err)

	if len(nopri) > 0 {
		ginx.Bomb(http.StatusForbidden, "No permission to operate the targets: %s", strings.Join(nopri, ", "))
	}
}

func (rt *Router) targetsOfAlertRule(c *gin.Context) {
	engineName := ginx.QueryStr(c, "engine_name", "")
	m, err := models.GetTargetsOfHostAlertRule(rt.Ctx, engineName)
	ret := make(map[string]map[int64][]string)
	for en, v := range m {
		if en != engineName {
			continue
		}

		ret[en] = make(map[int64][]string)
		for rid, idents := range v {
			ret[en][rid] = idents
		}
	}

	ginx.NewRender(c).Data(ret, err)
}
