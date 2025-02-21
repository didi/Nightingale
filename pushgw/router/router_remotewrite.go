package router

import (
	"fmt"
	"io"
	"io/ioutil"
	"net/http"

	"github.com/ccfos/nightingale/v6/pushgw/writer"
	"github.com/gin-gonic/gin"
	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"
	"github.com/toolkits/pkg/ginx"
	"github.com/toolkits/pkg/logger"
)

func extractMetricFromTimeSeries(s *prompb.TimeSeries) string {
	for i := 0; i < len(s.Labels); i++ {
		if s.Labels[i].Name == "__name__" {
			return s.Labels[i].Value
		}
	}
	return ""
}

// 返回的第二个参数，bool，表示是否需要把 ident 写入 target 表
func extractIdentFromTimeSeries(s *prompb.TimeSeries, ignoreIdent, ignoreHost bool, identMetrics []string) (string, bool) {
	if s == nil {
		return "", false
	}

	labelMap := make(map[string]int)
	for i, label := range s.Labels {
		labelMap[label.Name] = i
	}

	var ident string

	// 如果标签中有ident，则直接使用
	if idx, ok := labelMap["ident"]; ok {
		ident = s.Labels[idx].Value
	}

	if ident == "" {
		// 没有 ident 标签，尝试使用 agent_hostname 作为 ident
		// agent_hostname for grafana-agent and categraf
		if idx, ok := labelMap["agent_hostname"]; ok {
			s.Labels[idx].Name = "ident"
			ident = s.Labels[idx].Value
		}
	}

	if !ignoreHost && ident == "" {
		// agent_hostname 没有，那就使用 host 作为 ident，用于 telegraf 的场景
		// 但是，有的时候 nginx 采集的指标中带有 host 标签表示域名，这个时候就不能用 host 作为 ident，此时需要在 url 中设置 ignore_host=true
		// telegraf, output plugin: http, format: prometheusremotewrite
		if idx, ok := labelMap["host"]; ok {
			s.Labels[idx].Name = "ident"
			ident = s.Labels[idx].Value
		}
	}

	if ident == "" {
		// 上报的监控数据中并没有 ident 信息
		return "", false
	}

	if len(identMetrics) > 0 {
		metricFound := false
		for _, identMetric := range identMetrics {
			if idx, has := labelMap["__name__"]; has && s.Labels[idx].Value == identMetric {
				metricFound = true
				break
			}
		}

		if !metricFound {
			return ident, false
		}
	}

	return ident, !ignoreIdent
}

func duplicateLabelKey(series *prompb.TimeSeries) bool {
	if series == nil {
		return false
	}

	labelKeys := make(map[string]struct{})

	for j := 0; j < len(series.Labels); j++ {
		if _, has := labelKeys[series.Labels[j].Name]; has {
			return true
		} else {
			labelKeys[series.Labels[j].Name] = struct{}{}
		}
	}

	return false
}

func (rt *Router) remoteWrite(c *gin.Context) {
	curLen := rt.Writers.AllQueueLen.Load().(int)
	if curLen > rt.Pushgw.WriterOpt.AllQueueMaxSize {
		err := fmt.Errorf("write queue full, metric count over limit: %d", curLen)
		logger.Warning(err)
		writer.CounterPushQueueOverLimitTotal.Inc()
		c.String(rt.Pushgw.WriterOpt.OverLimitStatusCode, err.Error())
		return
	}

	req, err := DecodeWriteRequest(c.Request.Body)
	if err != nil {
		c.String(http.StatusBadRequest, err.Error())
		return
	}

	count := len(req.Timeseries)

	if count == 0 {
		c.String(200, "")
		return
	}

	var (
		ignoreIdent = ginx.QueryBool(c, "ignore_ident", false)
		ignoreHost  = ginx.QueryBool(c, "ignore_host", true) // 默认值改成 true，要不然答疑成本太高。发版的时候通知 telegraf 用户，让他们设置 ignore_host=false
		ids         = make(map[string]struct{})
	)

	for i := 0; i < count; i++ {
		if duplicateLabelKey(&req.Timeseries[i]) {
			continue
		}

		ident, insertTarget := extractIdentFromTimeSeries(&req.Timeseries[i], ignoreIdent, ignoreHost, rt.Pushgw.IdentMetrics)
		if len(ident) > 0 {
			// enrich host labels
			target, has := rt.TargetCache.Get(ident)
			if has {
				rt.AppendLabels(&req.Timeseries[i], target, rt.BusiGroupCache)
			}
		}

		if insertTarget {
			// has ident tag or agent_hostname tag
			// register host in table target
			ids[ident] = struct{}{}
		}

		if rt.Pushgw.EnableSourceStats {
			SourceStats.Increase(c.ClientIP())
		}

		var err error
		if len(ident) > 0 {
			err = rt.ForwardByIdent(c.ClientIP(), ident, &req.Timeseries[i])
		} else {
			err = rt.ForwardByMetric(c.ClientIP(), extractMetricFromTimeSeries(&req.Timeseries[i]), &req.Timeseries[i])
		}

		if err != nil {
			c.String(rt.Pushgw.WriterOpt.OverLimitStatusCode, err.Error())
			return
		}
	}

	CounterSampleTotal.WithLabelValues("prometheus").Add(float64(count))
	rt.IdentSet.MSet(ids)

	c.String(200, "")
}

// DecodeWriteRequest from an io.Reader into a prompb.WriteRequest, handling
// snappy decompression.
func DecodeWriteRequest(r io.Reader) (*prompb.WriteRequest, error) {
	compressed, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}

	reqBuf, err := snappy.Decode(nil, compressed)
	if err != nil {
		return nil, err
	}

	var req prompb.WriteRequest
	if err := proto.Unmarshal(reqBuf, &req); err != nil {
		return nil, err
	}

	return &req, nil
}
