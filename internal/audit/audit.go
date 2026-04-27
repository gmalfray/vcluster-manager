package audit

import (
	"log/slog"
	"net/http"

	"github.com/gmalfray/vcluster-manager/internal/auth"
	"github.com/gmalfray/vcluster-manager/internal/metrics"
)

// Log writes a structured audit entry via slog and increments action metrics.
// Output is captured by Kubernetes/Fluentd. The "audit" key marks the entry
// so log pipelines can route it to a dedicated index.
func Log(r *http.Request, action, name, env string, extra ...string) {
	user := auth.UserFromRequest(r)
	username, _ := user["name"].(string)
	if username == "" {
		username = "unknown"
	}
	attrs := []any{
		"audit", true,
		"user", username,
		"action", action,
		"vcluster", name,
		"env", env,
	}
	if len(extra) > 0 && extra[0] != "" {
		attrs = append(attrs, "detail", extra[0])
	}
	slog.Info("audit event", attrs...)
	metrics.VClusterActions.WithLabelValues(action, env).Inc()
}
