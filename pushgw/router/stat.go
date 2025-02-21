package router

import "github.com/prometheus/client_golang/prometheus"

const (
	namespace = "n9e"
	subsystem = "pushgw"
)

var (
	labels = []string{"service", "code", "path", "method"}

	CounterSampleTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: subsystem,
		Name:      "samples_received_total",
		Help:      "Total number samples received.",
	}, []string{"channel"})

	CounterDropSampleTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: subsystem,
		Name:      "drop_sample_total",
		Help:      "Number of drop sample.",
	}, []string{"client_ip"})

	CounterSampleReceivedByIdent = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: subsystem,
		Name:      "sample_received_by_ident",
		Help:      "Number of sample push by ident.",
	}, []string{"host_ident"})

	RequestCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "http_request_count_total",
			Help:      "Total number of HTTP requests made.",
		}, labels,
	)

	RequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "http_request_duration_seconds",
			Help:      "HTTP request latencies in seconds.",
		}, labels,
	)

	CounterSampleReceivedBySource = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "sample_received_by_source",
			Help:      "Number of sample push by source.",
		}, []string{"source"},
	)
)

func registerMetrics() {
	prometheus.MustRegister(
		CounterSampleTotal,
		CounterDropSampleTotal,
		CounterSampleReceivedByIdent,
		RequestCounter,
		RequestDuration,
		CounterSampleReceivedBySource,
	)
}
