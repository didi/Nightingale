package router

import (
	"compress/gzip"
	"fmt"
	"io/ioutil"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/mailru/easyjson"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/prompb"
)

//easyjson:json
type FalconMetric struct {
	Metric       string      `json:"metric"`
	Endpoint     string      `json:"endpoint"`
	Timestamp    int64       `json:"timestamp"`
	ValueUnTyped interface{} `json:"value"`
	Value        float64     `json:"-"`
	Tags         string      `json:"tags"`
}

//easyjson:json
type FalconMetricArr []FalconMetric

func (m *FalconMetric) Clean(ts int64) error {
	if m.Metric == "" {
		return fmt.Errorf("metric is blank")
	}

	switch v := m.ValueUnTyped.(type) {
	case string:
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			m.Value = f
		} else {
			return fmt.Errorf("unparseable value %v", v)
		}
	case float64:
		m.Value = v
	case uint64:
		m.Value = float64(v)
	case int64:
		m.Value = float64(v)
	case int:
		m.Value = float64(v)
	default:
		return fmt.Errorf("unparseable value %v", v)
	}

	// if timestamp bigger than 32 bits, likely in milliseconds
	if m.Timestamp > 0xffffffff {
		m.Timestamp /= 1000
	}

	// If the timestamp is greater than 5 minutes, the current time shall prevail
	diff := m.Timestamp - ts
	if diff > 300 {
		m.Timestamp = ts
	}
	return nil
}

func (m *FalconMetric) ToProm() (*prompb.TimeSeries, string, error) {
	pt := &prompb.TimeSeries{}
	pt.Samples = append(pt.Samples, prompb.Sample{
		// use ms
		Timestamp: m.Timestamp * 1000,
		Value:     m.Value,
	})

	if strings.IndexByte(m.Metric, '.') != -1 {
		m.Metric = strings.ReplaceAll(m.Metric, ".", "_")
	}

	if strings.IndexByte(m.Metric, '-') != -1 {
		m.Metric = strings.ReplaceAll(m.Metric, "-", "_")
	}

	if !model.MetricNameRE.MatchString(m.Metric) {
		return nil, "", fmt.Errorf("invalid metric name: %s", m.Metric)
	}

	pt.Labels = append(pt.Labels, prompb.Label{
		Name:  model.MetricNameLabel,
		Value: m.Metric,
	})

	tagarr := strings.Split(m.Tags, ",")
	tagmap := make(map[string]string, len(tagarr)+1)

	for i := 0; i < len(tagarr); i++ {
		tmp := strings.SplitN(tagarr[i], "=", 2)
		if len(tmp) != 2 {
			continue
		}

		tagmap[tmp[0]] = tmp[1]
	}

	ident := ""

	if len(m.Endpoint) > 0 {
		ident = m.Endpoint
		if id, exists := tagmap["ident"]; exists {
			ident = id
			// use ident in tags
			tagmap["endpoint"] = m.Endpoint
		} else {
			// use endpoint as ident
			tagmap["ident"] = m.Endpoint
		}
	}

	for key, value := range tagmap {
		if strings.IndexByte(key, '.') != -1 {
			key = strings.ReplaceAll(key, ".", "_")
		}

		if strings.IndexByte(key, '-') != -1 {
			key = strings.ReplaceAll(key, "-", "_")
		}

		if !model.LabelNameRE.MatchString(key) {
			return nil, "", fmt.Errorf("invalid tag name: %s", key)
		}

		pt.Labels = append(pt.Labels, prompb.Label{
			Name:  key,
			Value: value,
		})
	}

	return pt, ident, nil
}

func (rt *Router) falconPush(c *gin.Context) {
	var bs []byte
	var err error
	var r *gzip.Reader

	if c.GetHeader("Content-Encoding") == "gzip" {
		r, err = gzip.NewReader(c.Request.Body)
		if err != nil {
			c.String(400, err.Error())
			return
		}
		defer r.Close()
		bs, err = ioutil.ReadAll(r)
	} else {
		defer c.Request.Body.Close()
		bs, err = ioutil.ReadAll(c.Request.Body)
	}

	if err != nil {
		c.String(400, err.Error())
		return
	}

	var arr FalconMetricArr

	if bs[0] == '[' {
		err = easyjson.Unmarshal(bs, &arr)
	} else {
		var one FalconMetric
		err = easyjson.Unmarshal(bs, &one)
		arr = []FalconMetric{one}
	}

	if err != nil {
		c.String(400, err.Error())
		return
	}

	var (
		succ int
		fail int
		msg  = "received"
		ts   = time.Now().Unix()
		ids  = make(map[string]struct{})
	)

	for i := 0; i < len(arr); i++ {
		if err := arr[i].Clean(ts); err != nil {
			fail++
			continue
		}

		pt, ident, err := arr[i].ToProm()
		if err != nil {
			fail++
			continue
		}

		if ident != "" {
			// register host
			ids[ident] = struct{}{}

			// fill tags
			target, has := rt.TargetCache.Get(ident)
			if has {
				rt.AppendLabels(pt, target, rt.BusiGroupCache)
			}
		}

		if ident != "" {
			err = rt.ForwardByIdent(c.ClientIP(), ident, pt)
		} else {
			err = rt.ForwardByMetric(c.ClientIP(), arr[i].Metric, pt)
		}

		if err != nil {
			c.String(rt.Pushgw.WriterOpt.OverLimitStatusCode, err.Error())
			return
		}

		succ++
	}

	if succ > 0 {
		CounterSampleTotal.WithLabelValues("openfalcon").Add(float64(succ))
		rt.IdentSet.MSet(ids)
	}

	c.JSON(200, gin.H{
		"succ": succ,
		"fail": fail,
		"msg":  msg,
	})
}
