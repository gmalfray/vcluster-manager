package handlers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gmalfray/vcluster-manager/internal/service"
)

// The handleXxxError methods are the whole contract between the service's typed
// errors and what the operator sees. Each sentinel or typed error has to land on
// one precise toast, and ErrForbidden has to carry a 403 on top. Get an arm
// wrong and the UI lies: "existe deja" where it should say "en prod", a commit
// failure rendered as a bare Go error, or a delete blocked by a Rancher cleanup
// shown as something else entirely. Nothing drives these arms through a fully
// wired handler, so they're called directly here with hand-built errors.
//
// The toast template is the stub from minimalHandlers: it renders
// "{{.Level}}:{{.Message}}", so the body is "error:<message>".

type errMapCase struct {
	name       string
	err        error
	wantStatus int
	wantText   string // substring the toast must contain
	wantAbsent string // substring the toast must NOT contain ("" = no check)
}

func runErrMapCases(t *testing.T, cases []errMapCase, call func(*Handlers, http.ResponseWriter, error)) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := minimalHandlers()
			w := httptest.NewRecorder()

			call(h, w, tc.err)

			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body %q)", w.Code, tc.wantStatus, w.Body.String())
			}
			body := w.Body.String()
			if !strings.Contains(body, tc.wantText) {
				t.Errorf("toast = %q, want it to contain %q", body, tc.wantText)
			}
			if tc.wantAbsent != "" && strings.Contains(body, tc.wantAbsent) {
				t.Errorf("toast = %q, should not contain %q", body, tc.wantAbsent)
			}
		})
	}
}

func TestHandleCreateError(t *testing.T) {
	cases := []errMapCase{
		{
			name:       "non-admin gets a 403 and the access-refused toast",
			err:        service.ErrForbidden,
			wantStatus: http.StatusForbidden,
			wantText:   "Accès refusé",
		},
		{
			name:       "name taken in prod says so explicitly",
			err:        &service.ExistsError{Name: "demo", Env: "prod"},
			wantStatus: http.StatusOK,
			wantText:   "existe deja en prod",
		},
		{
			// The preprod branch must stay generic. "existe deja en prod" also
			// contains "existe deja", so the discriminator is the absence of
			// "en prod" — otherwise a swapped branch would slip through.
			name:       "name taken on preprod stays generic, no prod mention",
			err:        &service.ExistsError{Name: "demo", Env: "preprod"},
			wantStatus: http.StatusOK,
			wantText:   "existe deja",
			wantAbsent: "en prod",
		},
		{
			name:       "gitlab commit failure surfaces as a commit toast",
			err:        &service.CommitError{Err: errors.New("boom")},
			wantStatus: http.StatusOK,
			wantText:   "Erreur lors du commit GitLab : boom",
		},
		{
			// Field-validation errors arrive as plain "field : reason" and must
			// reach the operator verbatim, not be swallowed by a typed arm.
			name:       "field validation error passes through verbatim",
			err:        errors.New("cpu : quantité Kubernetes invalide"),
			wantStatus: http.StatusOK,
			wantText:   "cpu : quantité Kubernetes invalide",
		},
	}
	runErrMapCases(t, cases, (*Handlers).handleCreateError)
}

func TestHandleUpdateSettingsError(t *testing.T) {
	cases := []errMapCase{
		{
			name:       "non-admin gets a 403 and the access-refused toast",
			err:        service.ErrForbidden,
			wantStatus: http.StatusForbidden,
			wantText:   "Accès refusé",
		},
		{
			name:       "invalid name maps to the name toast",
			err:        service.ErrInvalidName,
			wantStatus: http.StatusOK,
			wantText:   "Nom invalide",
		},
		{
			name:       "unknown vcluster surfaces the not-found toast",
			err:        &service.VClusterNotFoundError{Err: errors.New("parse fail")},
			wantStatus: http.StatusOK,
			wantText:   "VCluster introuvable : parse fail",
		},
		{
			name:       "gitlab commit failure surfaces as a commit toast",
			err:        &service.CommitError{Err: errors.New("boom")},
			wantStatus: http.StatusOK,
			wantText:   "Erreur commit : boom",
		},
		{
			name:       "field validation error passes through verbatim",
			err:        errors.New("k8s_version : version invalide"),
			wantStatus: http.StatusOK,
			wantText:   "k8s_version : version invalide",
		},
	}
	runErrMapCases(t, cases, (*Handlers).handleUpdateSettingsError)
}

func TestHandleDeleteError(t *testing.T) {
	cases := []errMapCase{
		{
			name:       "non-admin gets a 403 and the access-refused toast",
			err:        service.ErrForbidden,
			wantStatus: http.StatusForbidden,
			wantText:   "Accès refusé",
		},
		{
			// A running Rancher cleanup is the reason a delete is refused mid-way.
			// The toast has to name the vcluster and its env so the operator knows
			// what to wait on.
			name:       "cleanup in progress names the vcluster and env",
			err:        &service.CleaningError{Name: "demo", Env: "preprod"},
			wantStatus: http.StatusOK,
			wantText:   "Nettoyage Rancher en cours pour demo (preprod)",
		},
		{
			name:       "rancher unpair failure surfaces the underlying error",
			err:        &service.UnpairError{Err: errors.New("rancher down")},
			wantStatus: http.StatusOK,
			wantText:   "Erreur dépairage Rancher : rancher down",
		},
	}
	runErrMapCases(t, cases, (*Handlers).handleDeleteError)
}
