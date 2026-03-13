package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	HTTPRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "vcluster_manager_http_requests_total",
		Help: "Total HTTP requests",
	}, []string{"method", "path", "status"})

	HTTPDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "vcluster_manager_http_duration_seconds",
		Help:    "HTTP request duration",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path"})

	GitLabAPIErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "vcluster_manager_gitlab_api_errors_total",
		Help: "Total GitLab API errors (including retries)",
	}, []string{"operation"})

	GitLabAPIRetries = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "vcluster_manager_gitlab_api_retries_total",
		Help: "Total GitLab API retries",
	}, []string{"operation"})

	VClusterActions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "vcluster_manager_actions_total",
		Help: "Total vcluster actions performed",
	}, []string{"action", "env"})

	ActiveDeletions = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "vcluster_manager_active_deletions",
		Help: "Number of vclusters currently being deleted",
	})
)

// Handler returns the Prometheus metrics HTTP handler.
func Handler() http.Handler {
	return promhttp.Handler()
}

// responseWriter wraps http.ResponseWriter to capture status code.
type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

// Middleware instruments HTTP handlers with request count and duration.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		duration := time.Since(start).Seconds()

		// Use pattern if available (Go 1.22+), fall back to path
		path := r.Pattern
		if path == "" {
			path = r.URL.Path
		}

		HTTPRequests.WithLabelValues(r.Method, path, strconv.Itoa(rw.status)).Inc()
		HTTPDuration.WithLabelValues(r.Method, path).Observe(duration)
	})
}
