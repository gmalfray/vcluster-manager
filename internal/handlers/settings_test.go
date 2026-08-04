package handlers

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gmalfray/vcluster-manager/internal/config"
	"github.com/gmalfray/vcluster-manager/internal/service"
)

// settingsTestHandlers is minimalHandlers wired to a real Service (like
// veleroTestHandlers in api_velero_test.go), so UpdateSettings/UpdateVeleroConfig
// exercise the same RBAC/validation path they run in production.
func settingsTestHandlers() *Handlers {
	h := minimalHandlers()
	h.cfg = &config.Config{}
	h.svc = service.New(service.Deps{
		Cfg:          h.cfg,
		K8sClients:   h.k8sClients,
		K8sClientsMu: &h.k8sClientsMu,
	})
	return h
}

func TestUpdateSettings_RejectsMaliciousK8sVersion(t *testing.T) {
	h := settingsTestHandlers()
	w := httptest.NewRecorder()
	form := url.Values{"k8s_version": {"1.28.4\nnewTag: evil"}}
	r := adminFormRequest("/vclusters/demo/settings", form)
	r.SetPathValue("name", "demo")

	h.UpdateSettings(w, r)

	if got := w.Body.String(); !strings.Contains(got, "k8s_version :") {
		t.Errorf("expected a k8s_version validation toast, got %q", got)
	}
}

func TestUpdateSettings_RejectsShellInjectionInFluxBranch(t *testing.T) {
	h := settingsTestHandlers()
	w := httptest.NewRecorder()
	form := url.Values{"fluxcd_branch": {"main; rm -rf /"}}
	r := adminFormRequest("/vclusters/demo/settings", form)
	r.SetPathValue("name", "demo")

	h.UpdateSettings(w, r)

	if got := w.Body.String(); !strings.Contains(got, "fluxcd_branch :") {
		t.Errorf("expected a fluxcd_branch validation toast, got %q", got)
	}
}

func TestUpdateSettings_RejectsBadVeleroHour(t *testing.T) {
	h := settingsTestHandlers()
	w := httptest.NewRecorder()
	form := url.Values{"velero_hour": {"25:99"}}
	r := adminFormRequest("/vclusters/demo/settings", form)
	r.SetPathValue("name", "demo")

	h.UpdateSettings(w, r)

	if got := w.Body.String(); !strings.Contains(got, "velero_hour :") {
		t.Errorf("expected a velero_hour validation toast, got %q", got)
	}
}

func TestUpdateSettings_RequiresAdmin(t *testing.T) {
	h := settingsTestHandlers()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/vclusters/demo/settings", nil)
	r.SetPathValue("name", "demo")

	h.UpdateSettings(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for a non-admin caller, got %d", w.Code)
	}
}

func TestUpdateVeleroConfig_RejectsInvalidBucket(t *testing.T) {
	h := settingsTestHandlers()
	w := httptest.NewRecorder()
	form := url.Values{"velero_bucket_preprod": {"Not_A_Valid_Bucket"}}
	r := adminFormRequest("/api/velero/config", form)

	h.UpdateVeleroConfig(w, r)

	if got := w.Body.String(); !strings.Contains(got, "velero_bucket_preprod :") {
		t.Errorf("expected a bucket validation toast, got %q", got)
	}
}

func TestUpdateVeleroConfig_RejectsInvalidS3URL(t *testing.T) {
	h := settingsTestHandlers()
	w := httptest.NewRecorder()
	form := url.Values{"velero_s3_url": {"not a url"}}
	r := adminFormRequest("/api/velero/config", form)

	h.UpdateVeleroConfig(w, r)

	if got := w.Body.String(); !strings.Contains(got, "velero_s3_url :") {
		t.Errorf("expected an s3_url validation toast, got %q", got)
	}
}

func TestUpdateVeleroConfig_RequiresAdmin(t *testing.T) {
	h := settingsTestHandlers()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/velero/config", nil)

	h.UpdateVeleroConfig(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for a non-admin caller, got %d", w.Code)
	}
}
