package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/ccfos/nightingale/v6/alert"
	"github.com/ccfos/nightingale/v6/alert/astats"
	"github.com/ccfos/nightingale/v6/alert/dispatch"
	"github.com/ccfos/nightingale/v6/alert/process"
	alertrt "github.com/ccfos/nightingale/v6/alert/router"
	"github.com/ccfos/nightingale/v6/center/metas"
	"github.com/ccfos/nightingale/v6/conf"
	"github.com/ccfos/nightingale/v6/dscache"
	"github.com/ccfos/nightingale/v6/dumper"
	"github.com/ccfos/nightingale/v6/memsto"
	"github.com/ccfos/nightingale/v6/pkg/ctx"
	"github.com/ccfos/nightingale/v6/pkg/httpx"
	"github.com/ccfos/nightingale/v6/pkg/logx"
	"github.com/ccfos/nightingale/v6/pkg/macros"
	"github.com/ccfos/nightingale/v6/prom"
	"github.com/ccfos/nightingale/v6/pushgw/idents"
	pushgwrt "github.com/ccfos/nightingale/v6/pushgw/router"
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
	//check CenterApi is default value
	if len(config.CenterApi.Addrs) < 1 {
		return nil, errors.New("failed to init config: the CenterApi configuration is missing")
	}
	ctx := ctx.NewContext(context.Background(), nil, false, config.CenterApi)

	var redis storage.Redis
	redis, err = storage.NewRedis(config.Redis)
	if err != nil {
		return nil, err
	}

	syncStats := memsto.NewSyncStats()

	targetCache := memsto.NewTargetCache(ctx, syncStats, redis)
	busiGroupCache := memsto.NewBusiGroupCache(ctx, syncStats)
	configCvalCache := memsto.NewCvalCache(ctx, syncStats)
	idents := idents.New(ctx, redis)
	metas := metas.New(redis)
	writers := writer.NewWriters(config.Pushgw)
	pushgwRouter := pushgwrt.New(config.HTTP, config.Pushgw, config.Alert, targetCache, busiGroupCache, idents, metas, writers, ctx)
	r := httpx.GinEngine(config.Global.RunMode, config.HTTP, configCvalCache.PrintBodyPaths, configCvalCache.PrintAccessLog)

	pushgwRouter.Config(r)
	macros.RegisterMacro(macros.MacroInVain)
	dscache.Init(ctx, false)

	if !config.Alert.Disable {
		configCache := memsto.NewConfigCache(ctx, syncStats, nil, "")
		alertStats := astats.NewSyncStats()
		dsCache := memsto.NewDatasourceCache(ctx, syncStats)
		alertMuteCache := memsto.NewAlertMuteCache(ctx, syncStats)
		alertRuleCache := memsto.NewAlertRuleCache(ctx, syncStats)
		notifyConfigCache := memsto.NewNotifyConfigCache(ctx, configCache)
		userCache := memsto.NewUserCache(ctx, syncStats)
		userGroupCache := memsto.NewUserGroupCache(ctx, syncStats)
		taskTplsCache := memsto.NewTaskTplCache(ctx)

		promClients := prom.NewPromClient(ctx)

		dispatch.InitRegisterQueryFunc(promClients)

		externalProcessors := process.NewExternalProcessors()

		alert.Start(config.Alert, config.Pushgw, syncStats, alertStats, externalProcessors, targetCache, busiGroupCache, alertMuteCache,
			alertRuleCache, notifyConfigCache, taskTplsCache, dsCache, ctx, promClients, userCache, userGroupCache, redis)

		alertrtRouter := alertrt.New(config.HTTP, config.Alert, alertMuteCache, targetCache, busiGroupCache, alertStats, ctx, externalProcessors)

		alertrtRouter.Config(r)

		if config.Ibex.Enable {
			ibex.ServerStart(false, nil, redis, config.HTTP.APIForService.BasicAuth, config.Alert.Heartbeat, &config.CenterApi, r, nil, config.Ibex, config.HTTP.Port)
		}
	}

	dumper.ConfigRouter(r)
	httpClean := httpx.Init(config.HTTP, r)

	return func() {
		logxClean()
		httpClean()
	}, nil
}
