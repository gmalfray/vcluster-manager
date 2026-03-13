package audit

import (
	"log"
	"net/http"

	"github.com/gmalfray/vcluster-manager/internal/auth"
	"github.com/gmalfray/vcluster-manager/internal/metrics"
)

// Log writes a structured audit entry to stdout and increments action metrics.
// Output is captured by Kubernetes/Fluentd.
func Log(r *http.Request, action, name, env string, extra ...string) {
	user := auth.UserFromRequest(r)
	username, _ := user["name"].(string)
	if username == "" {
		username = "unknown"
	}
	detail := ""
	if len(extra) > 0 {
		detail = " " + extra[0]
	}
	log.Printf("[audit] user=%q action=%q vcluster=%q env=%q%s", username, action, name, env, detail)
	metrics.VClusterActions.WithLabelValues(action, env).Inc()
}
