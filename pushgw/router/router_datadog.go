package router

import (
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	easyjson "github.com/mailru/easyjson"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/prompb"
)

//easyjson:json
type TimeSeries struct {
	Series []*DatadogMetric `json:"series"`
}

//easyjson:json
type DatadogMetric struct {
	Metric string         `json:"metric"`
	Points []DatadogPoint `json:"points"`
	Host   string         `json:"host"`
	Tags   []string       `json:"tags,omitempty"`
}

//easyjson:json
type DatadogPoint [2]float64

func (m *DatadogMetric) Clean() error {
	if m.Metric == "" {
		return fmt.Errorf("metric is blank")
	}
	return nil
}

func (m *DatadogMetric) ToProm() (*prompb.TimeSeries, string, error) {
	pt := &prompb.TimeSeries{}
	for i := 0; i < len(m.Points); i++ {
		pt.Samples = append(pt.Samples, prompb.Sample{
			// use ms
			Timestamp: int64(m.Points[i][0]) * 1000,
			Value:     m.Points[i][1],
		})
	}

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

	identInTag := ""
	hostInTag := ""

	for i := 0; i < len(m.Tags); i++ {
		arr := strings.SplitN(m.Tags[i], ":", 2)
		if len(arr) != 2 {
			continue
		}

		key := arr[0]

		if key == "ident" {
			identInTag = arr[1]
			pt.Labels = append(pt.Labels, prompb.Label{
				Name:  key,
				Value: arr[1],
			})
			continue
		}

		if key == "host" {
			hostInTag = arr[1]
			continue
		}

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
			Value: arr[1],
		})
	}

	if m.Host != "" {
		// m.Host has high priority
		hostInTag = m.Host
	}

	if hostInTag != "" {
		if identInTag != "" {
			pt.Labels = append(pt.Labels, prompb.Label{
				Name:  "host",
				Value: hostInTag,
			})
		} else {
			pt.Labels = append(pt.Labels, prompb.Label{
				Name:  "ident",
				Value: hostInTag,
			})
		}
	}

	ident := hostInTag
	if identInTag != "" {
		ident = identInTag
	}

	return pt, ident, nil
}

func datadogCheckRun(c *gin.Context) {
	c.String(200, "not implemented")
}

func datadogValidate(c *gin.Context) {
	c.String(200, "not implemented")
}

func datadogIntake(c *gin.Context) {
	c.String(200, "not implemented")
}

func datadogMetadata(c *gin.Context) {
	// body, err := readDatadogBody(c)
	// fmt.Println("metadata:", string(body), err)
	c.String(200, "not implemented")
}

func readDatadogBody(c *gin.Context) ([]byte, error) {
	var bs []byte
	var err error

	enc := c.GetHeader("Content-Encoding")

	if enc == "gzip" {
		r, e := gzip.NewReader(c.Request.Body)
		if e != nil {
			return nil, e
		}
		defer r.Close()
		bs, err = ioutil.ReadAll(r)
	} else if enc == "deflate" {
		r, e := zlib.NewReader(c.Request.Body)
		if e != nil {
			return nil, e
		}
		defer r.Close()
		bs, err = ioutil.ReadAll(r)
	} else {
		defer c.Request.Body.Close()
		bs, err = ioutil.ReadAll(c.Request.Body)
	}

	return bs, err
}

func (r *Router) datadogSeries(c *gin.Context) {
	apiKey, has := c.GetQuery("api_key")
	if !has {
		apiKey = ""
	}

	if len(r.HTTP.APIForAgent.BasicAuth) > 0 {
		ok := false
		for _, v := range r.HTTP.APIForAgent.BasicAuth {
			if apiKey == v {
				ok = true
				break
			}
		}

		if !ok {
			c.String(http.StatusUnauthorized, "unauthorized")
			return
		}
	}

	bs, err := readDatadogBody(c)
	if err != nil {
		c.String(400, err.Error())
		return
	}

	var series TimeSeries
	err = easyjson.Unmarshal(bs, &series)
	if err != nil {
		c.String(400, err.Error())
		return
	}

	cnt := len(series.Series)
	if cnt == 0 {
		c.String(400, "series empty")
		return
	}

	var (
		succ int
		fail int
		msg  = "received"
		ids  = make(map[string]struct{})
	)

	for i := 0; i < cnt; i++ {
		item := series.Series[i]

		if item == nil {
			continue
		}

		if err = item.Clean(); err != nil {
			fail++
			continue
		}

		pt, ident, err := item.ToProm()
		if err != nil {
			fail++
			continue
		}

		if ident != "" {
			// register host
			ids[ident] = struct{}{}

			// fill tags
			target, has := r.TargetCache.Get(ident)
			if has {
				r.AppendLabels(pt, target, r.BusiGroupCache)
			}
		}

		if ident != "" {
			err = r.ForwardByIdent(c.ClientIP(), ident, pt)
		} else {
			err = r.ForwardByMetric(c.ClientIP(), item.Metric, pt)
		}

		if err != nil {
			c.String(r.Pushgw.WriterOpt.OverLimitStatusCode, err.Error())
			return
		}

		succ++
	}

	if succ > 0 {
		CounterSampleTotal.WithLabelValues("datadog").Add(float64(succ))
		r.IdentSet.MSet(ids)
	}

	c.JSON(200, gin.H{
		"succ": succ,
		"fail": fail,
		"msg":  msg,
	})
}
