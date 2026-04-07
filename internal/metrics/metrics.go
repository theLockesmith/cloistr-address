package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// NIP05Requests tracks NIP-05 verification requests
	NIP05Requests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cloistr_address_nip05_requests_total",
			Help: "Total NIP-05 verification requests",
		},
		[]string{"status"}, // "found", "not_found", "error"
	)

	// LNURLRequests tracks Lightning Address requests
	LNURLRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cloistr_address_lnurl_requests_total",
			Help: "Total Lightning Address requests",
		},
		[]string{"endpoint", "status"}, // endpoint: "config", "callback"
	)

	// AddressCount tracks total registered addresses
	AddressCount = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "cloistr_address_registered_total",
			Help: "Total registered addresses",
		},
	)

	// HTTPRequestDuration tracks HTTP request latency
	HTTPRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "cloistr_address_http_request_duration_seconds",
			Help:    "HTTP request latency in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "path", "status"},
	)
)

// Init registers all metrics
func Init() {
	prometheus.MustRegister(NIP05Requests)
	prometheus.MustRegister(LNURLRequests)
	prometheus.MustRegister(AddressCount)
	prometheus.MustRegister(HTTPRequestDuration)
}

// Handler returns the Prometheus metrics handler
func Handler() http.Handler {
	return promhttp.Handler()
}
