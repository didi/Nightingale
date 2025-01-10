package writer

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ccfos/nightingale/v6/pkg/fasttime"
	"github.com/ccfos/nightingale/v6/pushgw/pconf"

	"github.com/golang/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/prometheus/client_golang/api"
	"github.com/prometheus/prometheus/prompb"
	"github.com/toolkits/pkg/logger"
)

type WriterType struct {
	Opts             pconf.WriterOptions
	ForceUseServerTS bool
	Client           api.Client
	RetryCount       int
	RetryInterval    int64 // 单位秒
}

func (w WriterType) writeRelabel(items []prompb.TimeSeries) []prompb.TimeSeries {
	ritems := make([]prompb.TimeSeries, 0, len(items))
	for _, item := range items {
		lbls := Process(item.Labels, w.Opts.WriteRelabels...)
		if len(lbls) == 0 {
			continue
		}
		item.Labels = lbls
		ritems = append(ritems, item)
	}
	return ritems
}

func (w WriterType) Write(key string, items []prompb.TimeSeries, headers ...map[string]string) {
	if len(items) == 0 {
		return
	}

	items = w.writeRelabel(items)
	if len(items) == 0 {
		return
	}

	CounterWirteTotal.WithLabelValues(key).Add(float64(len(items)))
	start := time.Now()
	defer func() {
		ForwardDuration.WithLabelValues(key).Observe(time.Since(start).Seconds())
	}()

	if w.ForceUseServerTS {
		ts := int64(fasttime.UnixTimestamp()) * 1000
		for i := 0; i < len(items); i++ {
			if len(items[i].Samples) == 0 {
				continue
			}
			items[i].Samples[0].Timestamp = ts
		}
	}

	req := &prompb.WriteRequest{
		Timeseries: items,
	}

	data, err := proto.Marshal(req)
	if err != nil {
		logger.Warningf("marshal prom data to proto got error: %v, data: %+v", err, items)
		return
	}

	for i := 0; i < w.RetryCount; i++ {
		err := w.Post(snappy.Encode(nil, data), headers...)
		if err == nil {
			break
		}

		CounterWirteErrorTotal.WithLabelValues(key).Add(float64(len(items)))
		logger.Warningf("post to %s got error: %v in %d times", w.Opts.Url, err, i)

		if i == 0 {
			logger.Warning("example timeseries:", items[0].String())
		}

		time.Sleep(time.Duration(w.RetryInterval) * time.Second)
	}
}

func (w WriterType) Post(req []byte, headers ...map[string]string) error {
	urls := strings.Split(w.Opts.Url, ",")
	var err error
	var newRequestErr error
	var httpReq *http.Request
	for _, url := range urls {
		httpReq, newRequestErr = http.NewRequest("POST", url, bytes.NewReader(req))
		if newRequestErr != nil {
			logger.Warningf("create remote write:%s request got error: %s", url, newRequestErr.Error())
			continue
		}

		httpReq.Header.Add("Content-Encoding", "snappy")
		httpReq.Header.Set("Content-Type", "application/x-protobuf")
		httpReq.Header.Set("User-Agent", "n9e")
		httpReq.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")

		if len(headers) > 0 {
			for k, v := range headers[0] {
				httpReq.Header.Set(k, v)
			}
		}

		if w.Opts.BasicAuthUser != "" {
			httpReq.SetBasicAuth(w.Opts.BasicAuthUser, w.Opts.BasicAuthPass)
		}

		headerCount := len(w.Opts.Headers)
		if headerCount > 0 && headerCount%2 == 0 {
			for i := 0; i < len(w.Opts.Headers); i += 2 {
				httpReq.Header.Add(w.Opts.Headers[i], w.Opts.Headers[i+1])
				if w.Opts.Headers[i] == "Host" {
					httpReq.Host = w.Opts.Headers[i+1]
				}
			}
		}

		resp, body, e := w.Client.Do(context.Background(), httpReq)
		if e != nil {
			logger.Warningf("push data with remote write:%s request got error: %v, response body: %s", url, e, string(body))
			err = e
			continue
		}

		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			logger.Warningf("push data with remote write:%s request got status code: %v, response body: %s", url, resp.StatusCode, string(body))
			continue
		}

		if resp.StatusCode >= 500 {
			err = fmt.Errorf("push data with remote write:%s request got status code: %v, response body: %s", url, resp.StatusCode, string(body))
			logger.Warning(err)
			continue
		}

		err = nil
		break
	}

	return err
}

type WritersType struct {
	pushgw      pconf.Pushgw
	backends    map[string]WriterType
	queues      map[string]*IdentQueue
	allQueueLen atomic.Value
	sync.RWMutex
}

type IdentQueue struct {
	list    *SafeListLimited
	closeCh chan struct{}
	ts      int64
}

func (ws *WritersType) ReportQueueStats(ident string, identQueue *IdentQueue) (interface{}, bool) {
	for {
		time.Sleep(60 * time.Second)
		count := identQueue.list.Len()
		if count > ws.pushgw.IdentStatsThreshold {
			GaugeSampleQueueSize.WithLabelValues(ident).Set(float64(count))
		}
	}
}

func (ws *WritersType) SetAllQueueLen() {
	for {
		curMetricLen := 0
		ws.RLock()
		for _, q := range ws.queues {
			curMetricLen += q.list.Len()
		}
		ws.RUnlock()
		ws.allQueueLen.Store(curMetricLen)
		time.Sleep(time.Duration(ws.pushgw.WriterOpt.AllQueueMaxSizeInterval) * time.Millisecond)
	}
}

func NewWriters(pushgwConfig pconf.Pushgw) *WritersType {
	writers := &WritersType{
		backends:    make(map[string]WriterType),
		queues:      make(map[string]*IdentQueue),
		pushgw:      pushgwConfig,
		allQueueLen: atomic.Value{},
	}

	writers.Init()

	go writers.SetAllQueueLen()
	go writers.CleanExpQueue()
	return writers
}

func (ws *WritersType) Put(name string, writer WriterType) {
	ws.backends[name] = writer
}

func (ws *WritersType) CleanExpQueue() {
	for {
		ws.Lock()
		for ident := range ws.queues {
			identQueue := ws.queues[ident]
			if identQueue == nil {
				delete(ws.queues, ident)
				logger.Warningf("Write channel(%s) not found", ident)
				continue
			}

			if time.Now().Unix()-identQueue.ts > 3600 {
				close(identQueue.closeCh)
				delete(ws.queues, ident)
			}
		}
		ws.Unlock()
		time.Sleep(time.Second * 600)
	}
}

func (ws *WritersType) PushSample(ident string, v interface{}) error {
	ws.RLock()
	identQueue := ws.queues[ident]
	ws.RUnlock()
	if identQueue == nil {
		identQueue = &IdentQueue{
			list:    NewSafeListLimited(ws.pushgw.WriterOpt.QueueMaxSize),
			closeCh: make(chan struct{}),
			ts:      time.Now().Unix(),
		}

		ws.Lock()
		ws.queues[ident] = identQueue
		ws.Unlock()

		go ws.ReportQueueStats(ident, identQueue)
		go ws.StartConsumer(identQueue)
	}

	identQueue.ts = time.Now().Unix()
	curLen := ws.allQueueLen.Load().(int)
	if curLen > ws.pushgw.WriterOpt.AllQueueMaxSize {
		err := fmt.Errorf("write queue full, metric count over limit: %d", curLen)
		logger.Warning(err)
		CounterPushQueueOverLimitTotal.Inc()
		return err
	}

	succ := identQueue.list.PushFront(v)
	if !succ {
		logger.Warningf("Write channel(%s) full, current channel size: %d", ident, identQueue.list.Len())
		CounterPushQueueErrorTotal.WithLabelValues(ident).Inc()
	}
	return nil
}

func (ws *WritersType) StartConsumer(identQueue *IdentQueue) {
	for {
		select {
		case <-identQueue.closeCh:
			logger.Infof("write queue:%v closed", identQueue)
			return
		default:
			series := identQueue.list.PopBack(ws.pushgw.WriterOpt.QueuePopSize)
			if len(series) == 0 {
				time.Sleep(time.Millisecond * 400)
				continue
			}
			for key := range ws.backends {
				ws.backends[key].Write(key, series)
			}
		}
	}
}

func (ws *WritersType) Init() error {
	opts := ws.pushgw.Writers
	ws.allQueueLen.Store(0)

	for i := 0; i < len(opts); i++ {
		tlsConf, err := opts[i].ClientConfig.TLSConfig()
		if err != nil {
			return err
		}

		trans := &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   time.Duration(opts[i].DialTimeout) * time.Millisecond,
				KeepAlive: time.Duration(opts[i].KeepAlive) * time.Millisecond,
			}).DialContext,
			ResponseHeaderTimeout: time.Duration(opts[i].Timeout) * time.Millisecond,
			TLSHandshakeTimeout:   time.Duration(opts[i].TLSHandshakeTimeout) * time.Millisecond,
			ExpectContinueTimeout: time.Duration(opts[i].ExpectContinueTimeout) * time.Millisecond,
			MaxConnsPerHost:       opts[i].MaxConnsPerHost,
			MaxIdleConns:          opts[i].MaxIdleConns,
			MaxIdleConnsPerHost:   opts[i].MaxIdleConnsPerHost,
			IdleConnTimeout:       time.Duration(opts[i].IdleConnTimeout) * time.Millisecond,
		}

		if tlsConf != nil {
			trans.TLSClientConfig = tlsConf
		}

		cli, err := api.NewClient(api.Config{
			Address:      opts[i].Url,
			RoundTripper: trans,
		})

		if err != nil {
			return err
		}

		writer := WriterType{
			Opts:             opts[i],
			Client:           cli,
			ForceUseServerTS: ws.pushgw.ForceUseServerTS,
			RetryCount:       ws.pushgw.WriterOpt.RetryCount,
			RetryInterval:    ws.pushgw.WriterOpt.RetryInterval,
		}

		ws.Put(opts[i].Url, writer)
	}

	return nil
}
