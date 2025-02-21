package router

import (
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/prometheus/prompb"

	"github.com/ccfos/nightingale/v6/alert/aconf"
	"github.com/ccfos/nightingale/v6/center/metas"
	"github.com/ccfos/nightingale/v6/memsto"
	"github.com/ccfos/nightingale/v6/pkg/ctx"
	"github.com/ccfos/nightingale/v6/pkg/httpx"
	"github.com/ccfos/nightingale/v6/pushgw/idents"
	"github.com/ccfos/nightingale/v6/pushgw/pconf"
	"github.com/ccfos/nightingale/v6/pushgw/writer"
)

type HandleTSFunc func(pt *prompb.TimeSeries) *prompb.TimeSeries

type Router struct {
	HTTP           httpx.Config
	Pushgw         pconf.Pushgw
	Aconf          aconf.Alert
	TargetCache    *memsto.TargetCacheType
	BusiGroupCache *memsto.BusiGroupCacheType
	IdentSet       *idents.Set
	MetaSet        *metas.Set
	Writers        *writer.WritersType
	Ctx            *ctx.Context
	HandleTS       HandleTSFunc
	HeartbeartApi  string
}

func stat() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		code := fmt.Sprintf("%d", c.Writer.Status())
		method := c.Request.Method
		labels := []string{"pushgw", code, c.FullPath(), method}

		RequestCounter.WithLabelValues(labels...).Inc()
		RequestDuration.WithLabelValues(labels...).Observe(float64(time.Since(start).Seconds()))
	}
}

func New(httpConfig httpx.Config, pushgw pconf.Pushgw, aconf aconf.Alert, tc *memsto.TargetCacheType, bg *memsto.BusiGroupCacheType,
	idents *idents.Set, metas *metas.Set,
	writers *writer.WritersType, ctx *ctx.Context) *Router {
	return &Router{
		HTTP:           httpConfig,
		Pushgw:         pushgw,
		Aconf:          aconf,
		Writers:        writers,
		Ctx:            ctx,
		TargetCache:    tc,
		BusiGroupCache: bg,
		IdentSet:       idents,
		MetaSet:        metas,
		HandleTS:       func(pt *prompb.TimeSeries) *prompb.TimeSeries { return pt },
	}
}

func (rt *Router) Config(r *gin.Engine) {
	service := r.Group("/v1/n9e")
	if len(rt.HTTP.APIForService.BasicAuth) > 0 {
		service.Use(gin.BasicAuth(rt.HTTP.APIForService.BasicAuth))
	}
	service.POST("/target-update", rt.targetUpdate)

	if !rt.HTTP.APIForAgent.Enable {
		return
	}

	registerMetrics()
	go rt.ReportIdentStats()
	rt.ReportSourceStats()

	r.Use(stat())
	// datadog url: http://n9e-pushgw.foo.com/datadog
	// use apiKey not basic auth
	r.POST("/datadog/api/v1/series", rt.datadogSeries)
	r.POST("/datadog/api/v1/check_run", datadogCheckRun)
	r.GET("/datadog/api/v1/validate", datadogValidate)
	r.POST("/datadog/api/v1/metadata", datadogMetadata)
	r.POST("/datadog/intake/", datadogIntake)

	if len(rt.HTTP.APIForAgent.BasicAuth) > 0 {
		// enable basic auth
		auth := gin.BasicAuth(rt.HTTP.APIForAgent.BasicAuth)
		r.POST("/opentsdb/put", auth, rt.openTSDBPut)
		r.POST("/openfalcon/push", auth, rt.falconPush)
		r.POST("/prometheus/v1/write", auth, rt.remoteWrite)
		r.POST("/v1/n9e/edge/heartbeat", auth, rt.heartbeat)

		if len(rt.Ctx.CenterApi.Addrs) > 0 {
			r.POST("/v1/n9e/heartbeat", auth, rt.heartbeat)
		}
	} else {
		// no need basic auth
		r.POST("/opentsdb/put", rt.openTSDBPut)
		r.POST("/openfalcon/push", rt.falconPush)
		r.POST("/prometheus/v1/write", rt.remoteWrite)
		r.POST("/v1/n9e/edge/heartbeat", rt.heartbeat)

		if len(rt.Ctx.CenterApi.Addrs) > 0 {
			r.POST("/v1/n9e/heartbeat", rt.heartbeat)
		}
	}
}
