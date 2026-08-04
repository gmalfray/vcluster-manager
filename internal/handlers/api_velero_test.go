package handlers

import (
	"encoding/base64"
	"encoding/json"
	"html/template"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gmalfray/vcluster-manager/internal/config"
)

// adminRequest builds a request carrying a session_token cookie that
// auth.IsAdmin recognizes as the local admin login. IsAdmin only decodes the
// JWT payload, it never checks the signature, so a fake unsigned token is
// enough to drive these handler tests.
func adminRequest(method, target string) *http.Request {
	payload, _ := json.Marshal(map[string]interface{}{"iss": "vcluster-manager-local"})
	token := "header." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
	r := httptest.NewRequest(method, target, nil)
	r.AddCookie(&http.Cookie{Name: "session_token", Value: token})
	return r
}

// veleroTestHandlers is minimalHandlers plus the Velero partials the handlers
// under test render (toast.html alone isn't enough for these).
func veleroTestHandlers() *Handlers {
	h := minimalHandlers()
	h.partials = template.Must(template.New("toast.html").Parse(`{{define "toast.html"}}{{.Level}}:{{.Message}}{{end}}
{{define "velero_backup_content.html"}}{{if .Error}}error:{{.Error}}{{else}}content:{{.Content}}{{end}}{{end}}
{{define "velero_restore_status.html"}}{{if .Error}}error:{{.Error}}{{else}}phase:{{.Phase}}{{end}}{{end}}`))
	h.cfg = &config.Config{VeleroNamespace: "velero-system"}
	return h
}

func TestValidBackupName(t *testing.T) {
	tests := []struct {
		name   string
		backup string
		want   bool
	}{
		{"manual backup name", "manual-demo-1717000000000", true},
		{"schedule-generated name", "daily-20230101020000", true},
		{"single char", "a", true},
		{"with dots", "backup.v1", true},
		{"empty rejected", "", false},
		{"path traversal", "../etc/passwd", false},
		{"embedded slash", "foo/bar", false},
		{"embedded newline", "foo\nbar", false},
		{"starts with dash", "-backup", false},
		{"uppercase rejected", "Manual-Demo", false},
		{"with spaces", "backup name", false},
		{"quote injection", `backup" \n evil: true`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validBackupName(tt.backup); got != tt.want {
				t.Errorf("validBackupName(%q) = %v, want %v", tt.backup, got, tt.want)
			}
		})
	}
}

func TestCreateVeleroRestore_RequiresAdmin(t *testing.T) {
	h := veleroTestHandlers()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/vclusters/demo/velero/restore?backup=manual-demo-1", nil)
	r.SetPathValue("name", "demo")

	h.CreateVeleroRestore(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for a non-admin caller, got %d", w.Code)
	}
}

func TestCreateVeleroRestore_RejectsMissingBackupName(t *testing.T) {
	h := veleroTestHandlers()
	w := httptest.NewRecorder()
	r := adminRequest(http.MethodPost, "/api/vclusters/demo/velero/restore")
	r.SetPathValue("name", "demo")

	h.CreateVeleroRestore(w, r)

	if got := w.Body.String(); got != "error:Nom du backup manquant" {
		t.Errorf("expected the missing-backup toast, got %q", got)
	}
}

func TestCreateVeleroRestore_RejectsInvalidBackupName(t *testing.T) {
	h := veleroTestHandlers()
	w := httptest.NewRecorder()
	r := adminRequest(http.MethodPost, "/api/vclusters/demo/velero/restore?backup=..%2f..%2fetc%2fpasswd")
	r.SetPathValue("name", "demo")

	h.CreateVeleroRestore(w, r)

	if got := w.Body.String(); got != "error:Nom de backup invalide" {
		t.Errorf("expected the invalid-backup-name toast, got %q", got)
	}
}

func TestCreateVeleroRestore_K8sUnavailable(t *testing.T) {
	h := veleroTestHandlers()
	w := httptest.NewRecorder()
	// No k8sClients entries configured — the handler must not touch anything.
	r := adminRequest(http.MethodPost, "/api/vclusters/demo/velero/restore?backup=manual-demo-123")
	r.SetPathValue("name", "demo")

	h.CreateVeleroRestore(w, r)

	if got := w.Body.String(); got != "error:Client Kubernetes non configuré" {
		t.Errorf("expected the k8s-unavailable toast, got %q", got)
	}
}

func TestVeleroBackupContent_RejectsInvalidBackupName(t *testing.T) {
	h := veleroTestHandlers()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/vclusters/demo/velero/backups/..%2f..%2fetc/content", nil)
	r.SetPathValue("name", "demo")
	r.SetPathValue("backup", "../../etc/passwd")

	h.VeleroBackupContent(w, r)

	if got := w.Body.String(); got != "error:Nom de backup invalide" {
		t.Errorf("expected the invalid-backup-name fragment, got %q", got)
	}
}

func TestDeleteVeleroBackup_RejectsInvalidBackupName(t *testing.T) {
	h := veleroTestHandlers()
	w := httptest.NewRecorder()
	r := adminRequest(http.MethodDelete, "/api/vclusters/demo/velero/backups/evil")
	r.SetPathValue("name", "demo")
	r.SetPathValue("backup", "../../etc/passwd")

	h.DeleteVeleroBackup(w, r)

	if got := w.Body.String(); got != "error:Nom de backup invalide" {
		t.Errorf("expected the invalid-backup-name toast, got %q", got)
	}
}

func TestIsTerminalRestorePhase(t *testing.T) {
	tests := []struct {
		phase string
		want  bool
	}{
		{"Completed", true},
		{"Failed", true},
		{"PartiallyFailed", true},
		{"New", false},
		{"InProgress", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isTerminalRestorePhase(tt.phase); got != tt.want {
			t.Errorf("isTerminalRestorePhase(%q) = %v, want %v", tt.phase, got, tt.want)
		}
	}
}
