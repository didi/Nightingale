package writer

import "github.com/prometheus/client_golang/prometheus"

const (
	namespace = "n9e"
	subsystem = "pushgw"
)

var (
	// 发往后端TSDB，延迟如何
	ForwardDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Buckets:   []float64{.1, 1, 10},
			Name:      "forward_duration_seconds",
			Help:      "Forward samples to TSDB. latencies in seconds.",
		}, []string{"url"},
	)

	GaugeSampleQueueSize = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "sample_queue_size",
			Help:      "The size of sample queue.",
		}, []string{"host_ident"},
	)

	CounterWirteTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: subsystem,
		Name:      "write_total",
		Help:      "Number of write.",
	}, []string{"url"})

	CounterWirteErrorTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: subsystem,
		Name:      "write_error_total",
		Help:      "Number of write error.",
	}, []string{"url"})

	CounterPushQueueErrorTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: subsystem,
		Name:      "push_queue_error_total",
		Help:      "Number of push queue error.",
	}, []string{"host_ident"})

	CounterPushQueueOverLimitTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: subsystem,
		Name:      "push_queue_over_limit_error_total",
		Help:      "Number of push queue over limit.",
	})
)

func init() {
	prometheus.MustRegister(
		ForwardDuration,
		CounterWirteTotal,
		CounterWirteErrorTotal,
		CounterPushQueueErrorTotal,
		GaugeSampleQueueSize,
		CounterPushQueueOverLimitTotal,
	)
}
