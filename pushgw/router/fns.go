package router

import (
	"github.com/ccfos/nightingale/v6/memsto"
	"github.com/ccfos/nightingale/v6/models"

	"github.com/prometheus/prometheus/prompb"
	"github.com/toolkits/pkg/logger"
)

func (rt *Router) AppendLabels(pt *prompb.TimeSeries, target *models.Target, bgCache *memsto.BusiGroupCacheType) {
	if target == nil {
		return
	}

	labelKeys := make(map[string]int)
	for j := 0; j < len(pt.Labels); j++ {
		labelKeys[pt.Labels[j].Name] = j
	}

	for key, value := range target.TagsMap {
		if index, has := labelKeys[key]; has {
			// e.g. busigroup=cloud
			if _, has := labelKeys[rt.Pushgw.BusiGroupLabelKey]; has {
				// busigroup key already exists, skip
				continue
			}

			// overwrite labels
			if rt.Pushgw.LabelRewrite {
				pt.Labels[index].Value = value
			}
			continue
		}

		pt.Labels = append(pt.Labels, prompb.Label{
			Name:  key,
			Value: value,
		})
	}

	// e.g. busigroup=cloud
	if _, has := labelKeys[rt.Pushgw.BusiGroupLabelKey]; has {
		return
	}

	// append busigroup tags
	if target.GroupId > 0 && len(rt.Pushgw.BusiGroupLabelKey) > 0 {
		bg := bgCache.GetByBusiGroupId(target.GroupId)
		if bg == nil {
			return
		}

		if bg.LabelEnable == 0 {
			return
		}

		if index, has := labelKeys[rt.Pushgw.BusiGroupLabelKey]; has {
			// overwrite labels
			if rt.Pushgw.LabelRewrite {
				pt.Labels[index].Value = bg.LabelValue
			}
			return
		}

		pt.Labels = append(pt.Labels, prompb.Label{
			Name:  rt.Pushgw.BusiGroupLabelKey,
			Value: bg.LabelValue,
		})
	}
}

// func getTs(pt *prompb.TimeSeries) int64 {
// 	if len(pt.Samples) == 0 {
// 		return 0
// 	}

// 	return pt.Samples[0].Timestamp
// }

func (rt *Router) debugSample(remoteAddr string, v *prompb.TimeSeries) {
	if v == nil {
		return
	}

	filter := rt.Pushgw.DebugSample
	if len(filter) == 0 {
		return
	}

	labelMap := make(map[string]string)
	for i := 0; i < len(v.Labels); i++ {
		labelMap[v.Labels[i].Name] = v.Labels[i].Value
	}

	for k, v := range filter {
		labelValue, exists := labelMap[k]
		if !exists {
			return
		}

		if labelValue != v {
			return
		}
	}

	logger.Debugf("--> debug sample from: %s, sample: %s", remoteAddr, v.String())
}

func (rt *Router) DropSample(v *prompb.TimeSeries) bool {
	filters := rt.Pushgw.DropSample
	if len(filters) == 0 {
		return false
	}

	labelMap := make(map[string]string)
	for i := 0; i < len(v.Labels); i++ {
		labelMap[v.Labels[i].Name] = v.Labels[i].Value
	}

	for _, filter := range filters {
		if len(filter) == 0 {
			continue
		}

		if matchSample(filter, labelMap) {
			return true
		}
	}

	return false
}

func matchSample(filterMap, sampleMap map[string]string) bool {
	for k, v := range filterMap {
		labelValue, exists := sampleMap[k]
		if !exists {
			return false
		}

		if labelValue != v {
			return false
		}
	}
	return true
}

func (rt *Router) ForwardByIdent(clientIP string, ident string, v *prompb.TimeSeries) error {
	v = rt.BeforePush(clientIP, v)
	if v == nil {
		return nil
	}

	IdentStats.Increment(ident, 1)
	if rt.DropSample(v) {
		CounterDropSampleTotal.WithLabelValues(ident).Inc()
		return nil
	}

	count := IdentStats.Get(ident)
	if count > rt.Pushgw.IdentDropThreshold {
		CounterDropSampleTotal.WithLabelValues(ident).Inc()
		// 单个 ident 的样本数超过阈值，不算异常，不影响其他机器的上报，不返回 err
		return nil
	}

	return rt.Writers.PushSample(ident, *v)
}

func (rt *Router) ForwardByMetric(clientIP string, metric string, v *prompb.TimeSeries) error {
	v = rt.BeforePush(clientIP, v)
	rt.debugSample(clientIP, v)
	if v == nil {
		return nil
	}

	IdentStats.Increment(metric, 1)
	if rt.DropSample(v) {
		CounterDropSampleTotal.WithLabelValues(metric).Inc()
		return nil
	}

	var hashkey string
	if len(metric) >= 2 {
		hashkey = metric[0:2]
	} else {
		hashkey = metric[0:1]
	}

	return rt.Writers.PushSample(hashkey, *v)
}

func (rt *Router) BeforePush(clientIP string, v *prompb.TimeSeries) *prompb.TimeSeries {
	rt.debugSample(clientIP, v)
	return rt.HandleTS(v)
}
