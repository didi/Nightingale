package router

import (
	"fmt"
	"net/http"
	"path"
	"runtime"
	"strings"
	"time"

	"github.com/ccfos/nightingale/v6/alert/aconf"
	"github.com/ccfos/nightingale/v6/center/cconf"
	"github.com/ccfos/nightingale/v6/center/cstats"
	"github.com/ccfos/nightingale/v6/center/metas"
	"github.com/ccfos/nightingale/v6/center/sso"
	"github.com/ccfos/nightingale/v6/conf"
	_ "github.com/ccfos/nightingale/v6/front/statik"
	"github.com/ccfos/nightingale/v6/memsto"
	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/aop"
	"github.com/ccfos/nightingale/v6/pkg/ctx"
	"github.com/ccfos/nightingale/v6/pkg/httpx"
	"github.com/ccfos/nightingale/v6/pkg/version"
	"github.com/ccfos/nightingale/v6/prom"
	"github.com/ccfos/nightingale/v6/pushgw/idents"
	"github.com/ccfos/nightingale/v6/storage"

	"github.com/gin-gonic/gin"
	"github.com/rakyll/statik/fs"
	"github.com/toolkits/pkg/ginx"
	"github.com/toolkits/pkg/logger"
	"github.com/toolkits/pkg/runner"
)

type Router struct {
	HTTP              httpx.Config
	Center            cconf.Center
	Ibex              conf.Ibex
	Alert             aconf.Alert
	Operations        cconf.Operation
	DatasourceCache   *memsto.DatasourceCacheType
	NotifyConfigCache *memsto.NotifyConfigCacheType
	PromClients       *prom.PromClientMap
	Redis             storage.Redis
	MetaSet           *metas.Set
	IdentSet          *idents.Set
	TargetCache       *memsto.TargetCacheType
	Sso               *sso.SsoClient
	UserCache         *memsto.UserCacheType
	UserGroupCache    *memsto.UserGroupCacheType
	Ctx               *ctx.Context
	HeartbeatHook     HeartbeatHookFunc
	TargetDeleteHook  models.TargetDeleteHookFunc
}

func New(httpConfig httpx.Config, center cconf.Center, alert aconf.Alert, ibex conf.Ibex,
	operations cconf.Operation, ds *memsto.DatasourceCacheType, ncc *memsto.NotifyConfigCacheType,
	pc *prom.PromClientMap, redis storage.Redis,
	sso *sso.SsoClient, ctx *ctx.Context, metaSet *metas.Set, idents *idents.Set,
	tc *memsto.TargetCacheType, uc *memsto.UserCacheType, ugc *memsto.UserGroupCacheType) *Router {
	return &Router{
		HTTP:              httpConfig,
		Center:            center,
		Alert:             alert,
		Ibex:              ibex,
		Operations:        operations,
		DatasourceCache:   ds,
		NotifyConfigCache: ncc,
		PromClients:       pc,
		Redis:             redis,
		MetaSet:           metaSet,
		IdentSet:          idents,
		TargetCache:       tc,
		Sso:               sso,
		UserCache:         uc,
		UserGroupCache:    ugc,
		Ctx:               ctx,
		HeartbeatHook:     func(ident string) map[string]interface{} { return nil },
		TargetDeleteHook:  emptyDeleteHook,
	}
}

func emptyDeleteHook(ctx *ctx.Context, idents []string) error {
	return nil
}

func stat() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		code := fmt.Sprintf("%d", c.Writer.Status())
		method := c.Request.Method
		labels := []string{cstats.Service, code, c.FullPath(), method}

		cstats.RequestCounter.WithLabelValues(labels...).Inc()
		cstats.RequestDuration.WithLabelValues(labels...).Observe(float64(time.Since(start).Seconds()))
	}
}

func languageDetector(i18NHeaderKey string) gin.HandlerFunc {
	headerKey := i18NHeaderKey
	return func(c *gin.Context) {
		if headerKey != "" {
			lang := c.GetHeader(headerKey)
			if lang != "" {
				if strings.HasPrefix(lang, "zh_HK") {
					c.Request.Header.Set("X-Language", "zh_HK")
				} else if strings.HasPrefix(lang, "zh") {
					c.Request.Header.Set("X-Language", "zh_CN")
				} else if strings.HasPrefix(lang, "en") {
					c.Request.Header.Set("X-Language", "en")
				} else {
					c.Request.Header.Set("X-Language", lang)
				}
			} else {
				c.Request.Header.Set("X-Language", "zh_CN")
			}
		}
		c.Next()
	}
}

func (rt *Router) configNoRoute(r *gin.Engine, fs *http.FileSystem) {
	r.NoRoute(func(c *gin.Context) {
		arr := strings.Split(c.Request.URL.Path, ".")
		suffix := arr[len(arr)-1]

		switch suffix {
		case "png", "jpeg", "jpg", "svg", "ico", "gif", "css", "js", "html", "htm", "gz", "zip", "map", "ttf", "md":
			if !rt.Center.UseFileAssets {
				c.FileFromFS(c.Request.URL.Path, *fs)
			} else {
				cwdarr := []string{"/"}
				if runtime.GOOS == "windows" {
					cwdarr[0] = ""
				}
				cwdarr = append(cwdarr, strings.Split(runner.Cwd, "/")...)
				cwdarr = append(cwdarr, "pub")
				cwdarr = append(cwdarr, strings.Split(c.Request.URL.Path, "/")...)
				c.File(path.Join(cwdarr...))
			}
		default:
			if !rt.Center.UseFileAssets {
				c.FileFromFS("/", *fs)
			} else {
				cwdarr := []string{"/"}
				if runtime.GOOS == "windows" {
					cwdarr[0] = ""
				}
				cwdarr = append(cwdarr, strings.Split(runner.Cwd, "/")...)
				cwdarr = append(cwdarr, "pub")
				cwdarr = append(cwdarr, "index.html")
				c.File(path.Join(cwdarr...))
			}
		}
	})
}

func (rt *Router) Config(r *gin.Engine) {

	r.Use(stat())
	r.Use(languageDetector(rt.Center.I18NHeaderKey))
	r.Use(aop.Recovery())

	statikFS, err := fs.New()
	if err != nil {
		logger.Errorf("cannot create statik fs: %v", err)
	}

	if !rt.Center.UseFileAssets {
		r.StaticFS("/pub", statikFS)
	}

	pagesPrefix := "/api/n9e"
	pages := r.Group(pagesPrefix)
	{

		if rt.Center.AnonymousAccess.PromQuerier {
			pages.Any("/proxy/:id/*url", rt.dsProxy)
			pages.POST("/query-range-batch", rt.promBatchQueryRange)
			pages.POST("/query-instant-batch", rt.promBatchQueryInstant)
			pages.GET("/datasource/brief", rt.datasourceBriefs)
			pages.POST("/datasource/query", rt.datasourceQuery)

			pages.POST("/ds-query", rt.QueryData)
			pages.POST("/logs-query", rt.QueryLogV2)

			pages.POST("/tdengine-databases", rt.tdengineDatabases)
			pages.POST("/tdengine-tables", rt.tdengineTables)
			pages.POST("/tdengine-columns", rt.tdengineColumns)

			pages.POST("/log-query-batch", rt.QueryLogBatch)

			// 数据库元数据接口
			pages.POST("/db-databases", rt.ShowDatabases)
			pages.POST("/db-tables", rt.ShowTables)
			pages.POST("/db-desc-table", rt.DescribeTable)

			// es 专用接口
			pages.POST("/indices", rt.auth(), rt.user(), rt.QueryIndices)
			pages.POST("/es-variable", rt.auth(), rt.user(), rt.QueryESVariable)
			pages.POST("/fields", rt.auth(), rt.user(), rt.QueryFields)
			pages.POST("/log-query", rt.auth(), rt.user(), rt.QueryLog)
		} else {
			pages.Any("/proxy/:id/*url", rt.auth(), rt.dsProxy)
			pages.POST("/query-range-batch", rt.auth(), rt.promBatchQueryRange)
			pages.POST("/query-instant-batch", rt.auth(), rt.promBatchQueryInstant)
			pages.GET("/datasource/brief", rt.auth(), rt.user(), rt.datasourceBriefs)
			pages.POST("/datasource/query", rt.auth(), rt.user(), rt.datasourceQuery)

			pages.POST("/ds-query", rt.auth(), rt.QueryData)
			pages.POST("/logs-query", rt.auth(), rt.QueryLogV2)

			pages.POST("/tdengine-databases", rt.auth(), rt.tdengineDatabases)
			pages.POST("/tdengine-tables", rt.auth(), rt.tdengineTables)
			pages.POST("/tdengine-columns", rt.auth(), rt.tdengineColumns)

			pages.POST("/log-query-batch", rt.auth(), rt.user(), rt.QueryLogBatch)

			// 数据库元数据接口
			pages.POST("/db-databases", rt.auth(), rt.user(), rt.ShowDatabases)
			pages.POST("/db-tables", rt.auth(), rt.user(), rt.ShowTables)
			pages.POST("/db-desc-table", rt.auth(), rt.user(), rt.DescribeTable)

			// es 专用接口
			pages.POST("/indices", rt.auth(), rt.user(), rt.QueryIndices)
			pages.POST("/es-variable", rt.QueryESVariable)
			pages.POST("/fields", rt.QueryFields)
			pages.POST("/log-query", rt.QueryLog)
		}

		pages.GET("/sql-template", rt.QuerySqlTemplate)
		pages.POST("/auth/login", rt.jwtMock(), rt.loginPost)
		pages.POST("/auth/logout", rt.jwtMock(), rt.auth(), rt.user(), rt.logoutPost)
		pages.POST("/auth/refresh", rt.jwtMock(), rt.refreshPost)
		pages.POST("/auth/captcha", rt.jwtMock(), rt.generateCaptcha)
		pages.POST("/auth/captcha-verify", rt.jwtMock(), rt.captchaVerify)
		pages.GET("/auth/ifshowcaptcha", rt.ifShowCaptcha)

		pages.GET("/auth/sso-config", rt.ssoConfigNameGet)
		pages.GET("/auth/rsa-config", rt.rsaConfigGet)
		pages.GET("/auth/redirect", rt.loginRedirect)
		pages.GET("/auth/redirect/cas", rt.loginRedirectCas)
		pages.GET("/auth/redirect/oauth", rt.loginRedirectOAuth)
		pages.GET("/auth/callback", rt.loginCallback)
		pages.GET("/auth/callback/cas", rt.loginCallbackCas)
		pages.GET("/auth/callback/oauth", rt.loginCallbackOAuth)
		pages.GET("/auth/perms", rt.allPerms)

		pages.GET("/metrics/desc", rt.metricsDescGetFile)
		pages.POST("/metrics/desc", rt.metricsDescGetMap)

		pages.GET("/notify-channels", rt.notifyChannelsGets)
		pages.GET("/contact-keys", rt.contactKeysGets)

		pages.GET("/self/perms", rt.auth(), rt.user(), rt.permsGets)
		pages.GET("/self/profile", rt.auth(), rt.user(), rt.selfProfileGet)
		pages.PUT("/self/profile", rt.auth(), rt.user(), rt.selfProfilePut)
		pages.PUT("/self/password", rt.auth(), rt.user(), rt.selfPasswordPut)
		pages.GET("/self/token", rt.auth(), rt.user(), rt.getToken)
		pages.POST("/self/token", rt.auth(), rt.user(), rt.addToken)
		pages.DELETE("/self/token/:id", rt.auth(), rt.user(), rt.deleteToken)

		pages.GET("/users", rt.auth(), rt.user(), rt.perm("/users"), rt.userGets)
		pages.POST("/users", rt.auth(), rt.admin(), rt.userAddPost)
		pages.GET("/user/:id/profile", rt.auth(), rt.userProfileGet)
		pages.PUT("/user/:id/profile", rt.auth(), rt.admin(), rt.userProfilePut)
		pages.PUT("/user/:id/password", rt.auth(), rt.admin(), rt.userPasswordPut)
		pages.DELETE("/user/:id", rt.auth(), rt.admin(), rt.userDel)

		pages.GET("/metric-views", rt.auth(), rt.metricViewGets)
		pages.DELETE("/metric-views", rt.auth(), rt.user(), rt.metricViewDel)
		pages.POST("/metric-views", rt.auth(), rt.user(), rt.metricViewAdd)
		pages.PUT("/metric-views", rt.auth(), rt.user(), rt.metricViewPut)

		pages.GET("/builtin-metric-filters", rt.auth(), rt.user(), rt.metricFilterGets)
		pages.DELETE("/builtin-metric-filters", rt.auth(), rt.user(), rt.metricFilterDel)
		pages.POST("/builtin-metric-filters", rt.auth(), rt.user(), rt.metricFilterAdd)
		pages.PUT("/builtin-metric-filters", rt.auth(), rt.user(), rt.metricFilterPut)
		pages.POST("/builtin-metric-promql", rt.auth(), rt.user(), rt.getMetricPromql)

		pages.POST("/builtin-metrics", rt.auth(), rt.user(), rt.perm("/builtin-metrics/add"), rt.builtinMetricsAdd)
		pages.PUT("/builtin-metrics", rt.auth(), rt.user(), rt.perm("/builtin-metrics/put"), rt.builtinMetricsPut)
		pages.DELETE("/builtin-metrics", rt.auth(), rt.user(), rt.perm("/builtin-metrics/del"), rt.builtinMetricsDel)
		pages.GET("/builtin-metrics", rt.auth(), rt.user(), rt.builtinMetricsGets)
		pages.GET("/builtin-metrics/types", rt.auth(), rt.user(), rt.builtinMetricsTypes)
		pages.GET("/builtin-metrics/types/default", rt.auth(), rt.user(), rt.builtinMetricsDefaultTypes)
		pages.GET("/builtin-metrics/collectors", rt.auth(), rt.user(), rt.builtinMetricsCollectors)

		pages.GET("/user-groups", rt.auth(), rt.user(), rt.userGroupGets)
		pages.POST("/user-groups", rt.auth(), rt.user(), rt.perm("/user-groups/add"), rt.userGroupAdd)
		pages.GET("/user-group/:id", rt.auth(), rt.user(), rt.userGroupGet)
		pages.PUT("/user-group/:id", rt.auth(), rt.user(), rt.perm("/user-groups/put"), rt.userGroupWrite(), rt.userGroupPut)
		pages.DELETE("/user-group/:id", rt.auth(), rt.user(), rt.perm("/user-groups/del"), rt.userGroupWrite(), rt.userGroupDel)
		pages.POST("/user-group/:id/members", rt.auth(), rt.user(), rt.perm("/user-groups/put"), rt.userGroupWrite(), rt.userGroupMemberAdd)
		pages.DELETE("/user-group/:id/members", rt.auth(), rt.user(), rt.perm("/user-groups/put"), rt.userGroupWrite(), rt.userGroupMemberDel)

		pages.GET("/busi-groups", rt.auth(), rt.user(), rt.busiGroupGets)
		pages.POST("/busi-groups", rt.auth(), rt.user(), rt.perm("/busi-groups/add"), rt.busiGroupAdd)
		pages.GET("/busi-groups/alertings", rt.auth(), rt.busiGroupAlertingsGets)
		pages.GET("/busi-group/:id", rt.auth(), rt.user(), rt.bgro(), rt.busiGroupGet)
		pages.PUT("/busi-group/:id", rt.auth(), rt.user(), rt.perm("/busi-groups/put"), rt.bgrw(), rt.busiGroupPut)
		pages.POST("/busi-group/:id/members", rt.auth(), rt.user(), rt.perm("/busi-groups/put"), rt.bgrw(), rt.busiGroupMemberAdd)
		pages.DELETE("/busi-group/:id/members", rt.auth(), rt.user(), rt.perm("/busi-groups/put"), rt.bgrw(), rt.busiGroupMemberDel)
		pages.DELETE("/busi-group/:id", rt.auth(), rt.user(), rt.perm("/busi-groups/del"), rt.bgrw(), rt.busiGroupDel)
		pages.GET("/busi-group/:id/perm/:perm", rt.auth(), rt.user(), rt.checkBusiGroupPerm)
		pages.GET("/busi-groups/tags", rt.auth(), rt.user(), rt.busiGroupsGetTags)

		pages.GET("/targets", rt.auth(), rt.user(), rt.targetGets)
		pages.GET("/target/extra-meta", rt.auth(), rt.user(), rt.targetExtendInfoByIdent)
		pages.POST("/target/list", rt.auth(), rt.user(), rt.targetGetsByHostFilter)
		pages.DELETE("/targets", rt.auth(), rt.user(), rt.perm("/targets/del"), rt.targetDel)
		pages.GET("/targets/tags", rt.auth(), rt.user(), rt.targetGetTags)
		pages.POST("/targets/tags", rt.auth(), rt.user(), rt.perm("/targets/put"), rt.targetBindTagsByFE)
		pages.DELETE("/targets/tags", rt.auth(), rt.user(), rt.perm("/targets/put"), rt.targetUnbindTagsByFE)
		pages.PUT("/targets/note", rt.auth(), rt.user(), rt.perm("/targets/put"), rt.targetUpdateNote)
		pages.PUT("/targets/bgids", rt.auth(), rt.user(), rt.perm("/targets/put"), rt.targetBindBgids)

		pages.POST("/builtin-cate-favorite", rt.auth(), rt.user(), rt.builtinCateFavoriteAdd)
		pages.DELETE("/builtin-cate-favorite/:name", rt.auth(), rt.user(), rt.builtinCateFavoriteDel)

		pages.GET("/integrations/icon/:cate/:name", rt.builtinIcon)

		// pages.GET("/builtin-boards", rt.builtinBoardGets)
		// pages.GET("/builtin-board/:name", rt.builtinBoardGet)
		// pages.GET("/dashboards/builtin/list", rt.builtinBoardGets)
		// pages.GET("/builtin-boards-cates", rt.auth(), rt.user(), rt.builtinBoardCateGets)
		// pages.POST("/builtin-boards-detail", rt.auth(), rt.user(), rt.builtinBoardDetailGets)
		// pages.GET("/integrations/makedown/:cate", rt.builtinMarkdown)

		pages.GET("/busi-groups/public-boards", rt.auth(), rt.user(), rt.perm("/dashboards"), rt.publicBoardGets)
		pages.GET("/busi-groups/boards", rt.auth(), rt.user(), rt.perm("/dashboards"), rt.boardGetsByGids)
		pages.GET("/busi-group/:id/boards", rt.auth(), rt.user(), rt.perm("/dashboards"), rt.bgro(), rt.boardGets)
		pages.POST("/busi-group/:id/boards", rt.auth(), rt.user(), rt.perm("/dashboards/add"), rt.bgrw(), rt.boardAdd)
		pages.POST("/busi-group/:id/board/:bid/clone", rt.auth(), rt.user(), rt.perm("/dashboards/add"), rt.bgrw(), rt.boardClone)
		pages.POST("/busi-groups/boards/clones", rt.auth(), rt.user(), rt.perm("/dashboards/add"), rt.boardBatchClone)

		pages.GET("/boards", rt.auth(), rt.user(), rt.boardGetsByBids)
		pages.GET("/board/:bid", rt.boardGet)
		pages.GET("/board/:bid/pure", rt.boardPureGet)
		pages.PUT("/board/:bid", rt.auth(), rt.user(), rt.perm("/dashboards/put"), rt.boardPut)
		pages.PUT("/board/:bid/configs", rt.auth(), rt.user(), rt.perm("/dashboards/put"), rt.boardPutConfigs)
		pages.PUT("/board/:bid/public", rt.auth(), rt.user(), rt.perm("/dashboards/put"), rt.boardPutPublic)
		pages.DELETE("/boards", rt.auth(), rt.user(), rt.perm("/dashboards/del"), rt.boardDel)

		pages.GET("/share-charts", rt.chartShareGets)
		pages.POST("/share-charts", rt.auth(), rt.chartShareAdd)

		pages.POST("/dashboard-annotations", rt.auth(), rt.user(), rt.perm("/dashboards/put"), rt.dashAnnotationAdd)
		pages.GET("/dashboard-annotations", rt.dashAnnotationGets)
		pages.PUT("/dashboard-annotation/:id", rt.auth(), rt.user(), rt.perm("/dashboards/put"), rt.dashAnnotationPut)
		pages.DELETE("/dashboard-annotation/:id", rt.auth(), rt.user(), rt.perm("/dashboards/del"), rt.dashAnnotationDel)

		// pages.GET("/alert-rules/builtin/alerts-cates", rt.auth(), rt.user(), rt.builtinAlertCateGets)
		// pages.GET("/alert-rules/builtin/list", rt.auth(), rt.user(), rt.builtinAlertRules)
		pages.GET("/alert-rules/callbacks", rt.auth(), rt.user(), rt.alertRuleCallbacks)

		pages.GET("/busi-groups/alert-rules", rt.auth(), rt.user(), rt.perm("/alert-rules"), rt.alertRuleGetsByGids)
		pages.GET("/busi-group/:id/alert-rules", rt.auth(), rt.user(), rt.perm("/alert-rules"), rt.alertRuleGets)
		pages.POST("/busi-group/:id/alert-rules", rt.auth(), rt.user(), rt.perm("/alert-rules/add"), rt.bgrw(), rt.alertRuleAddByFE)
		pages.POST("/busi-group/:id/alert-rules/import", rt.auth(), rt.user(), rt.perm("/alert-rules/add"), rt.bgrw(), rt.alertRuleAddByImport)
		pages.POST("/busi-group/:id/alert-rules/import-prom-rule", rt.auth(),
			rt.user(), rt.perm("/alert-rules/add"), rt.bgrw(), rt.alertRuleAddByImportPromRule)
		pages.DELETE("/busi-group/:id/alert-rules", rt.auth(), rt.user(), rt.perm("/alert-rules/del"), rt.bgrw(), rt.alertRuleDel)
		pages.PUT("/busi-group/:id/alert-rules/fields", rt.auth(), rt.user(), rt.perm("/alert-rules/put"), rt.bgrw(), rt.alertRulePutFields)
		pages.PUT("/busi-group/:id/alert-rule/:arid", rt.auth(), rt.user(), rt.perm("/alert-rules/put"), rt.alertRulePutByFE)
		pages.GET("/alert-rule/:arid", rt.auth(), rt.user(), rt.perm("/alert-rules"), rt.alertRuleGet)
		pages.GET("/alert-rule/:arid/pure", rt.auth(), rt.user(), rt.perm("/alert-rules"), rt.alertRulePureGet)
		pages.PUT("/busi-group/alert-rule/validate", rt.auth(), rt.user(), rt.perm("/alert-rules/put"), rt.alertRuleValidation)
		pages.POST("/relabel-test", rt.auth(), rt.user(), rt.relabelTest)
		pages.POST("/busi-group/:id/alert-rules/clone", rt.auth(), rt.user(), rt.perm("/alert-rules/add"), rt.bgrw(), rt.cloneToMachine)

		pages.GET("/busi-groups/recording-rules", rt.auth(), rt.user(), rt.perm("/recording-rules"), rt.recordingRuleGetsByGids)
		pages.GET("/busi-group/:id/recording-rules", rt.auth(), rt.user(), rt.perm("/recording-rules"), rt.recordingRuleGets)
		pages.POST("/busi-group/:id/recording-rules", rt.auth(), rt.user(), rt.perm("/recording-rules/add"), rt.bgrw(), rt.recordingRuleAddByFE)
		pages.DELETE("/busi-group/:id/recording-rules", rt.auth(), rt.user(), rt.perm("/recording-rules/del"), rt.bgrw(), rt.recordingRuleDel)
		pages.PUT("/busi-group/:id/recording-rule/:rrid", rt.auth(), rt.user(), rt.perm("/recording-rules/put"), rt.bgrw(), rt.recordingRulePutByFE)
		pages.GET("/recording-rule/:rrid", rt.auth(), rt.user(), rt.perm("/recording-rules"), rt.recordingRuleGet)
		pages.PUT("/busi-group/:id/recording-rules/fields", rt.auth(), rt.user(), rt.perm("/recording-rules/put"), rt.recordingRulePutFields)

		pages.GET("/busi-groups/alert-mutes", rt.auth(), rt.user(), rt.perm("/alert-mutes"), rt.alertMuteGetsByGids)
		pages.GET("/busi-group/:id/alert-mutes", rt.auth(), rt.user(), rt.perm("/alert-mutes"), rt.bgro(), rt.alertMuteGetsByBG)
		pages.POST("/busi-group/:id/alert-mutes/preview", rt.auth(), rt.user(), rt.perm("/alert-mutes/add"), rt.bgrw(), rt.alertMutePreview)
		pages.POST("/busi-group/:id/alert-mutes", rt.auth(), rt.user(), rt.perm("/alert-mutes/add"), rt.bgrw(), rt.alertMuteAdd)
		pages.DELETE("/busi-group/:id/alert-mutes", rt.auth(), rt.user(), rt.perm("/alert-mutes/del"), rt.bgrw(), rt.alertMuteDel)
		pages.PUT("/busi-group/:id/alert-mute/:amid", rt.auth(), rt.user(), rt.perm("/alert-mutes/put"), rt.alertMutePutByFE)
		pages.GET("/busi-group/:id/alert-mute/:amid", rt.auth(), rt.user(), rt.perm("/alert-mutes"), rt.alertMuteGet)
		pages.PUT("/busi-group/:id/alert-mutes/fields", rt.auth(), rt.user(), rt.perm("/alert-mutes/put"), rt.bgrw(), rt.alertMutePutFields)

		pages.GET("/busi-groups/alert-subscribes", rt.auth(), rt.user(), rt.perm("/alert-subscribes"), rt.alertSubscribeGetsByGids)
		pages.GET("/busi-group/:id/alert-subscribes", rt.auth(), rt.user(), rt.perm("/alert-subscribes"), rt.bgro(), rt.alertSubscribeGets)
		pages.GET("/alert-subscribe/:sid", rt.auth(), rt.user(), rt.perm("/alert-subscribes"), rt.alertSubscribeGet)
		pages.POST("/busi-group/:id/alert-subscribes", rt.auth(), rt.user(), rt.perm("/alert-subscribes/add"), rt.bgrw(), rt.alertSubscribeAdd)
		pages.PUT("/busi-group/:id/alert-subscribes", rt.auth(), rt.user(), rt.perm("/alert-subscribes/put"), rt.bgrw(), rt.alertSubscribePut)
		pages.DELETE("/busi-group/:id/alert-subscribes", rt.auth(), rt.user(), rt.perm("/alert-subscribes/del"), rt.bgrw(), rt.alertSubscribeDel)

		if rt.Center.AnonymousAccess.AlertDetail {
			pages.GET("/alert-cur-event/:eid", rt.alertCurEventGet)
			pages.GET("/alert-his-event/:eid", rt.alertHisEventGet)
			pages.GET("/event-notify-records/:eid", rt.notificationRecordList)
		} else {
			pages.GET("/alert-cur-event/:eid", rt.auth(), rt.user(), rt.alertCurEventGet)
			pages.GET("/alert-his-event/:eid", rt.auth(), rt.user(), rt.alertHisEventGet)
			pages.GET("/event-notify-records/:eid", rt.auth(), rt.user(), rt.notificationRecordList)
		}

		// card logic
		pages.GET("/alert-cur-events/list", rt.auth(), rt.user(), rt.alertCurEventsList)
		pages.GET("/alert-cur-events/card", rt.auth(), rt.user(), rt.alertCurEventsCard)
		pages.POST("/alert-cur-events/card/details", rt.auth(), rt.alertCurEventsCardDetails)
		pages.GET("/alert-his-events/list", rt.auth(), rt.user(), rt.alertHisEventsList)
		pages.DELETE("/alert-cur-events", rt.auth(), rt.user(), rt.perm("/alert-cur-events/del"), rt.alertCurEventDel)
		pages.GET("/alert-cur-events/stats", rt.auth(), rt.alertCurEventsStatistics)

		pages.GET("/alert-aggr-views", rt.auth(), rt.alertAggrViewGets)
		pages.DELETE("/alert-aggr-views", rt.auth(), rt.user(), rt.alertAggrViewDel)
		pages.POST("/alert-aggr-views", rt.auth(), rt.user(), rt.alertAggrViewAdd)
		pages.PUT("/alert-aggr-views", rt.auth(), rt.user(), rt.alertAggrViewPut)

		pages.GET("/busi-groups/task-tpls", rt.auth(), rt.user(), rt.perm("/job-tpls"), rt.taskTplGetsByGids)
		pages.GET("/busi-group/:id/task-tpls", rt.auth(), rt.user(), rt.perm("/job-tpls"), rt.bgro(), rt.taskTplGets)
		pages.POST("/busi-group/:id/task-tpls", rt.auth(), rt.user(), rt.perm("/job-tpls/add"), rt.bgrw(), rt.taskTplAdd)
		pages.DELETE("/busi-group/:id/task-tpl/:tid", rt.auth(), rt.user(), rt.perm("/job-tpls/del"), rt.bgrw(), rt.taskTplDel)
		pages.POST("/busi-group/:id/task-tpls/tags", rt.auth(), rt.user(), rt.perm("/job-tpls/put"), rt.bgrw(), rt.taskTplBindTags)
		pages.DELETE("/busi-group/:id/task-tpls/tags", rt.auth(), rt.user(), rt.perm("/job-tpls/put"), rt.bgrw(), rt.taskTplUnbindTags)
		pages.GET("/busi-group/:id/task-tpl/:tid", rt.auth(), rt.user(), rt.perm("/job-tpls"), rt.bgro(), rt.taskTplGet)
		pages.PUT("/busi-group/:id/task-tpl/:tid", rt.auth(), rt.user(), rt.perm("/job-tpls/put"), rt.bgrw(), rt.taskTplPut)

		pages.GET("/busi-groups/tasks", rt.auth(), rt.user(), rt.perm("/job-tasks"), rt.taskGetsByGids)
		pages.GET("/busi-group/:id/tasks", rt.auth(), rt.user(), rt.perm("/job-tasks"), rt.bgro(), rt.taskGets)
		pages.POST("/busi-group/:id/tasks", rt.auth(), rt.user(), rt.perm("/job-tasks/add"), rt.bgrw(), rt.taskAdd)

		pages.GET("/servers", rt.auth(), rt.user(), rt.serversGet)
		pages.GET("/server-clusters", rt.auth(), rt.user(), rt.serverClustersGet)

		pages.POST("/datasource/list", rt.auth(), rt.user(), rt.datasourceList)
		pages.POST("/datasource/plugin/list", rt.auth(), rt.pluginList)
		pages.POST("/datasource/upsert", rt.auth(), rt.admin(), rt.datasourceUpsert)
		pages.POST("/datasource/desc", rt.auth(), rt.admin(), rt.datasourceGet)
		pages.POST("/datasource/status/update", rt.auth(), rt.admin(), rt.datasourceUpdataStatus)
		pages.DELETE("/datasource/", rt.auth(), rt.admin(), rt.datasourceDel)

		pages.GET("/roles", rt.auth(), rt.admin(), rt.roleGets)
		pages.POST("/roles", rt.auth(), rt.admin(), rt.roleAdd)
		pages.PUT("/roles", rt.auth(), rt.admin(), rt.rolePut)
		pages.DELETE("/role/:id", rt.auth(), rt.admin(), rt.roleDel)

		pages.GET("/role/:id/ops", rt.auth(), rt.admin(), rt.operationOfRole)
		pages.PUT("/role/:id/ops", rt.auth(), rt.admin(), rt.roleBindOperation)
		pages.GET("/operation", rt.operations)

		pages.GET("/notify-tpls", rt.auth(), rt.user(), rt.notifyTplGets)
		pages.PUT("/notify-tpl/content", rt.auth(), rt.user(), rt.notifyTplUpdateContent)
		pages.PUT("/notify-tpl", rt.auth(), rt.user(), rt.notifyTplUpdate)
		pages.POST("/notify-tpl", rt.auth(), rt.user(), rt.notifyTplAdd)
		pages.DELETE("/notify-tpl/:id", rt.auth(), rt.user(), rt.notifyTplDel)
		pages.POST("/notify-tpl/preview", rt.auth(), rt.user(), rt.notifyTplPreview)

		pages.GET("/sso-configs", rt.auth(), rt.admin(), rt.ssoConfigGets)
		pages.PUT("/sso-config", rt.auth(), rt.admin(), rt.ssoConfigUpdate)

		pages.GET("/webhooks", rt.auth(), rt.user(), rt.webhookGets)
		pages.PUT("/webhooks", rt.auth(), rt.admin(), rt.webhookPuts)

		pages.GET("/notify-script", rt.auth(), rt.user(), rt.perm("/help/notification-settings"), rt.notifyScriptGet)
		pages.PUT("/notify-script", rt.auth(), rt.admin(), rt.notifyScriptPut)

		pages.GET("/notify-channel", rt.auth(), rt.user(), rt.perm("/help/notification-settings"), rt.notifyChannelGets)
		pages.PUT("/notify-channel", rt.auth(), rt.admin(), rt.notifyChannelPuts)

		pages.GET("/notify-contact", rt.auth(), rt.user(), rt.perm("/help/notification-settings"), rt.notifyContactGets)
		pages.PUT("/notify-contact", rt.auth(), rt.admin(), rt.notifyContactPuts)

		pages.GET("/notify-config", rt.auth(), rt.user(), rt.perm("/help/notification-settings"), rt.notifyConfigGet)
		pages.PUT("/notify-config", rt.auth(), rt.admin(), rt.notifyConfigPut)
		pages.PUT("/smtp-config-test", rt.auth(), rt.admin(), rt.attemptSendEmail)

		pages.GET("/es-index-pattern", rt.auth(), rt.esIndexPatternGet)
		pages.GET("/es-index-pattern-list", rt.auth(), rt.esIndexPatternGetList)
		pages.POST("/es-index-pattern", rt.auth(), rt.admin(), rt.esIndexPatternAdd)
		pages.PUT("/es-index-pattern", rt.auth(), rt.admin(), rt.esIndexPatternPut)
		pages.DELETE("/es-index-pattern", rt.auth(), rt.admin(), rt.esIndexPatternDel)

		pages.GET("/embedded-dashboards", rt.auth(), rt.user(), rt.perm("/embedded-dashboards"), rt.embeddedDashboardsGet)
		pages.PUT("/embedded-dashboards", rt.auth(), rt.user(), rt.perm("/embedded-dashboards/put"), rt.embeddedDashboardsPut)

		pages.GET("/user-variable-configs", rt.auth(), rt.user(), rt.perm("/help/variable-configs"), rt.userVariableConfigGets)
		pages.POST("/user-variable-config", rt.auth(), rt.user(), rt.perm("/help/variable-configs"), rt.userVariableConfigAdd)
		pages.PUT("/user-variable-config/:id", rt.auth(), rt.user(), rt.perm("/help/variable-configs"), rt.userVariableConfigPut)
		pages.DELETE("/user-variable-config/:id", rt.auth(), rt.user(), rt.perm("/help/variable-configs"), rt.userVariableConfigDel)

		pages.GET("/config", rt.auth(), rt.admin(), rt.configGetByKey)
		pages.PUT("/config", rt.auth(), rt.admin(), rt.configPutByKey)
		pages.GET("/site-info", rt.siteInfo)

		// for admin api
		pages.GET("/user/busi-groups", rt.auth(), rt.admin(), rt.userBusiGroupsGets)

		pages.GET("/builtin-components", rt.auth(), rt.user(), rt.builtinComponentsGets)
		pages.POST("/builtin-components", rt.auth(), rt.user(), rt.perm("/built-in-components/add"), rt.builtinComponentsAdd)
		pages.PUT("/builtin-components", rt.auth(), rt.user(), rt.perm("/built-in-components/put"), rt.builtinComponentsPut)
		pages.DELETE("/builtin-components", rt.auth(), rt.user(), rt.perm("/built-in-components/del"), rt.builtinComponentsDel)

		pages.GET("/builtin-payloads", rt.auth(), rt.user(), rt.builtinPayloadsGets)
		pages.GET("/builtin-payloads/cates", rt.auth(), rt.user(), rt.builtinPayloadcatesGet)
		pages.POST("/builtin-payloads", rt.auth(), rt.user(), rt.perm("/built-in-components/add"), rt.builtinPayloadsAdd)
		pages.GET("/builtin-payload/:id", rt.auth(), rt.user(), rt.perm("/built-in-components"), rt.builtinPayloadGet)
		pages.PUT("/builtin-payloads", rt.auth(), rt.user(), rt.perm("/built-in-components/put"), rt.builtinPayloadsPut)
		pages.DELETE("/builtin-payloads", rt.auth(), rt.user(), rt.perm("/built-in-components/del"), rt.builtinPayloadsDel)
		pages.GET("/builtin-payload", rt.auth(), rt.user(), rt.builtinPayloadsGetByUUIDOrID)

	}

	r.GET("/api/n9e/versions", func(c *gin.Context) {
		v := version.Version
		lastIndex := strings.LastIndex(version.Version, "-")
		if lastIndex != -1 {
			v = version.Version[:lastIndex]
		}

		gv := version.GithubVersion.Load()
		if gv != nil {
			ginx.NewRender(c).Data(gin.H{"version": v, "github_verison": gv.(string)}, nil)
		} else {
			ginx.NewRender(c).Data(gin.H{"version": v, "github_verison": ""}, nil)
		}
	})

	if rt.HTTP.APIForService.Enable {
		service := r.Group("/v1/n9e")
		if len(rt.HTTP.APIForService.BasicAuth) > 0 {
			service.Use(gin.BasicAuth(rt.HTTP.APIForService.BasicAuth))
		}
		{
			service.Any("/prometheus/*url", rt.dsProxy)
			service.POST("/users", rt.userAddPost)
			service.PUT("/user/:id", rt.userProfilePutByService)
			service.DELETE("/user/:id", rt.userDel)
			service.GET("/users", rt.userFindAll)

			service.GET("/user-groups", rt.userGroupGetsByService)
			service.GET("/user-group-members", rt.userGroupMemberGetsByService)

			service.GET("/targets", rt.targetGetsByService)
			service.GET("/target/extra-meta", rt.targetExtendInfoByIdent)
			service.POST("/target/list", rt.targetGetsByHostFilter)
			service.DELETE("/targets", rt.targetDelByService)
			service.GET("/targets/tags", rt.targetGetTags)
			service.POST("/targets/tags", rt.targetBindTagsByService)
			service.DELETE("/targets/tags", rt.targetUnbindTagsByService)
			service.PUT("/targets/note", rt.targetUpdateNoteByService)
			service.PUT("/targets/bgid", rt.targetUpdateBgidByService)

			service.POST("/alert-rules", rt.alertRuleAddByService)
			service.POST("/alert-rule-add", rt.alertRuleAddOneByService)
			service.DELETE("/alert-rules", rt.alertRuleDelByService)
			service.PUT("/alert-rule/:arid", rt.alertRulePutByService)
			service.GET("/alert-rule/:arid", rt.alertRuleGet)
			service.GET("/alert-rules", rt.alertRulesGetByService)

			service.GET("/alert-subscribes", rt.alertSubscribeGetsByService)

			service.GET("/busi-groups", rt.busiGroupGetsByService)

			service.GET("/datasources", rt.datasourceGetsByService)
			service.GET("/datasource-ids", rt.getDatasourceIds)
			service.POST("/server-heartbeat", rt.serverHeartbeat)
			service.GET("/servers-active", rt.serversActive)

			service.GET("/recording-rules", rt.recordingRuleGetsByService)

			service.GET("/alert-mutes", rt.alertMuteGets)
			service.POST("/alert-mutes", rt.alertMuteAddByService)
			service.DELETE("/alert-mutes", rt.alertMuteDel)

			service.GET("/alert-cur-events", rt.alertCurEventsList)
			service.GET("/alert-cur-events-get-by-rid", rt.alertCurEventsGetByRid)
			service.GET("/alert-his-events", rt.alertHisEventsList)
			service.GET("/alert-his-event/:eid", rt.alertHisEventGet)

			service.GET("/task-tpl/:tid", rt.taskTplGetByService)
			service.GET("/task-tpls", rt.taskTplGetsByService)
			service.GET("/task-tpl/statistics", rt.taskTplStatistics)

			service.GET("/config/:id", rt.configGet)
			service.GET("/configs", rt.configsGet)
			service.GET("/config", rt.configGetByKey)
			service.GET("/all-configs", rt.configGetAll)
			service.PUT("/configs", rt.configsPut)
			service.POST("/configs", rt.configsPost)
			service.DELETE("/configs", rt.configsDel)

			service.POST("/conf-prop/encrypt", rt.confPropEncrypt)
			service.POST("/conf-prop/decrypt", rt.confPropDecrypt)

			service.GET("/statistic", rt.statistic)

			service.GET("/notify-tpls", rt.notifyTplGets)

			service.POST("/task-record-add", rt.taskRecordAdd)

			service.GET("/user-variable/decrypt", rt.userVariableGetDecryptByService)

			service.GET("/targets-of-alert-rule", rt.targetsOfAlertRule)

			service.POST("/notify-record", rt.notificationRecordAdd)

			service.GET("/alert-cur-events-del-by-hash", rt.alertCurEventDelByHash)

			service.POST("/center/heartbeat", rt.heartbeat)
		}
	}

	if rt.HTTP.APIForAgent.Enable {
		heartbeat := r.Group("/v1/n9e")
		{
			if len(rt.HTTP.APIForAgent.BasicAuth) > 0 {
				heartbeat.Use(gin.BasicAuth(rt.HTTP.APIForAgent.BasicAuth))
			}
			heartbeat.POST("/heartbeat", rt.heartbeat)
		}
	}

	rt.configNoRoute(r, &statikFS)

}

func Render(c *gin.Context, data, msg interface{}) {
	if msg == nil {
		if data == nil {
			data = struct{}{}
		}
		c.JSON(http.StatusOK, gin.H{"data": data, "error": ""})
	} else {
		c.JSON(http.StatusOK, gin.H{"error": gin.H{"message": msg}})
	}
}

func Dangerous(c *gin.Context, v interface{}, code ...int) {
	if v == nil {
		return
	}

	switch t := v.(type) {
	case string:
		if t != "" {
			c.JSON(http.StatusOK, gin.H{"error": v})
		}
	case error:
		c.JSON(http.StatusOK, gin.H{"error": t.Error()})
	}
}
