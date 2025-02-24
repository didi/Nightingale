package alert

import (
	"context"
	"fmt"

	"github.com/ccfos/nightingale/v6/dscache"

	"github.com/ccfos/nightingale/v6/alert/aconf"
	"github.com/ccfos/nightingale/v6/alert/astats"
	"github.com/ccfos/nightingale/v6/alert/dispatch"
	"github.com/ccfos/nightingale/v6/alert/eval"
	"github.com/ccfos/nightingale/v6/alert/naming"
	"github.com/ccfos/nightingale/v6/alert/process"
	"github.com/ccfos/nightingale/v6/alert/queue"
	"github.com/ccfos/nightingale/v6/alert/record"
	"github.com/ccfos/nightingale/v6/alert/router"
	"github.com/ccfos/nightingale/v6/alert/sender"
	"github.com/ccfos/nightingale/v6/conf"
	"github.com/ccfos/nightingale/v6/dumper"
	"github.com/ccfos/nightingale/v6/memsto"
	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ctx"
	"github.com/ccfos/nightingale/v6/pkg/httpx"
	"github.com/ccfos/nightingale/v6/pkg/logx"
	"github.com/ccfos/nightingale/v6/pkg/macros"
	"github.com/ccfos/nightingale/v6/prom"
	"github.com/ccfos/nightingale/v6/pushgw/pconf"
	"github.com/ccfos/nightingale/v6/pushgw/writer"
	"github.com/ccfos/nightingale/v6/storage"
	"github.com/flashcatcloud/ibex/src/cmd/ibex"
)

func Initialize(configDir string, cryptoKey string) (func(), error) {
	config, err := conf.InitConfig(configDir, cryptoKey)
	if err != nil {
		return nil, fmt.Errorf("failed to init config: %v", err)
	}

	logxClean, err := logx.Init(config.Log)
	if err != nil {
		return nil, err
	}

	ctx := ctx.NewContext(context.Background(), nil, false, config.CenterApi)

	var redis storage.Redis
	redis, err = storage.NewRedis(config.Redis)
	if err != nil {
		return nil, err
	}

	syncStats := memsto.NewSyncStats()
	alertStats := astats.NewSyncStats()

	configCache := memsto.NewConfigCache(ctx, syncStats, nil, "")
	targetCache := memsto.NewTargetCache(ctx, syncStats, redis)
	busiGroupCache := memsto.NewBusiGroupCache(ctx, syncStats)
	alertMuteCache := memsto.NewAlertMuteCache(ctx, syncStats)
	alertRuleCache := memsto.NewAlertRuleCache(ctx, syncStats)
	notifyConfigCache := memsto.NewNotifyConfigCache(ctx, configCache)
	dsCache := memsto.NewDatasourceCache(ctx, syncStats)
	userCache := memsto.NewUserCache(ctx, syncStats)
	userGroupCache := memsto.NewUserGroupCache(ctx, syncStats)
	taskTplsCache := memsto.NewTaskTplCache(ctx)
	configCvalCache := memsto.NewCvalCache(ctx, syncStats)

	promClients := prom.NewPromClient(ctx)
	dispatch.InitRegisterQueryFunc(promClients)

	externalProcessors := process.NewExternalProcessors()

	macros.RegisterMacro(macros.MacroInVain)
	dscache.Init(ctx, false)
	Start(config.Alert, config.Pushgw, syncStats, alertStats, externalProcessors, targetCache, busiGroupCache, alertMuteCache, alertRuleCache, notifyConfigCache, taskTplsCache, dsCache, ctx, promClients, userCache, userGroupCache)

	r := httpx.GinEngine(config.Global.RunMode, config.HTTP,
		configCvalCache.PrintBodyPaths, configCvalCache.PrintAccessLog)
	rt := router.New(config.HTTP, config.Alert, alertMuteCache, targetCache, busiGroupCache, alertStats, ctx, externalProcessors)

	if config.Ibex.Enable {
		ibex.ServerStart(false, nil, redis, config.HTTP.APIForService.BasicAuth, config.Alert.Heartbeat, &config.CenterApi, r, nil, config.Ibex, config.HTTP.Port)
	}

	rt.Config(r)
	dumper.ConfigRouter(r)

	httpClean := httpx.Init(config.HTTP, r)

	return func() {
		logxClean()
		httpClean()
	}, nil
}

func Start(alertc aconf.Alert, pushgwc pconf.Pushgw, syncStats *memsto.Stats, alertStats *astats.Stats, externalProcessors *process.ExternalProcessorsType, targetCache *memsto.TargetCacheType, busiGroupCache *memsto.BusiGroupCacheType,
	alertMuteCache *memsto.AlertMuteCacheType, alertRuleCache *memsto.AlertRuleCacheType, notifyConfigCache *memsto.NotifyConfigCacheType, taskTplsCache *memsto.TaskTplCache, datasourceCache *memsto.DatasourceCacheType, ctx *ctx.Context,
	promClients *prom.PromClientMap, userCache *memsto.UserCacheType, userGroupCache *memsto.UserGroupCacheType) {
	alertSubscribeCache := memsto.NewAlertSubscribeCache(ctx, syncStats)
	recordingRuleCache := memsto.NewRecordingRuleCache(ctx, syncStats)
	targetsOfAlertRulesCache := memsto.NewTargetOfAlertRuleCache(ctx, alertc.Heartbeat.EngineName, syncStats)

	go models.InitNotifyConfig(ctx, alertc.Alerting.TemplatesDir)

	naming := naming.NewNaming(ctx, alertc.Heartbeat, alertStats)

	writers := writer.NewWriters(pushgwc)
	record.NewScheduler(alertc, recordingRuleCache, promClients, writers, alertStats, datasourceCache)

	eval.NewScheduler(alertc, externalProcessors, alertRuleCache, targetCache, targetsOfAlertRulesCache,
		busiGroupCache, alertMuteCache, datasourceCache, promClients, naming, ctx, alertStats)

	dp := dispatch.NewDispatch(alertRuleCache, userCache, userGroupCache, alertSubscribeCache, targetCache, notifyConfigCache, taskTplsCache, alertc.Alerting, ctx, alertStats)
	consumer := dispatch.NewConsumer(alertc.Alerting, ctx, dp, promClients)

	notifyRecordComsumer := sender.NewNotifyRecordConsumer(ctx)

	go dp.ReloadTpls()
	go consumer.LoopConsume()
	go notifyRecordComsumer.LoopConsume()

	go queue.ReportQueueSize(alertStats)
	go sender.ReportNotifyRecordQueueSize(alertStats)
	go sender.InitEmailSender(ctx, notifyConfigCache)
}
