package handlers

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
)

// realVeleroRestoreStatusTemplate parses the actual partial shipped to
// production, not the string stub veleroTestHandlers() uses for the other
// tests in this package. Those stubs only exercise the handler's Go logic;
// this one exercises what a user actually reads, which is the point of item
// 3 of the 1.4.1 findings — the fragment must not claim "Flux repris" when
// the resume failed or is still unsettled.
func realVeleroRestoreStatusTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl, err := template.ParseFiles("../../web/templates/partials/velero_restore_status.html")
	if err != nil {
		t.Fatalf("parsing the real velero_restore_status.html: %v", err)
	}
	return tmpl
}

func renderVeleroRestoreStatus(t *testing.T, data map[string]interface{}) string {
	t.Helper()
	var buf bytes.Buffer
	if err := realVeleroRestoreStatusTemplate(t).ExecuteTemplate(&buf, "velero_restore_status.html", data); err != nil {
		t.Fatalf("executing velero_restore_status.html: %v", err)
	}
	return buf.String()
}

// TestVeleroRestoreStatusFragment_FluxResumedOnlyOnASettledSuccess is the
// negative-space check the recette finding called for: a fragment claiming
// "Flux repris" while the resume actually failed is what shipped in 1.4.0.
// Each case below is a state GetVeleroRestoreStatus / GetVeleroOpsRestoreStatus
// can actually return, and only the last one is allowed to say it's done.
func TestVeleroRestoreStatusFragment_FluxResumedOnlyOnASettledSuccess(t *testing.T) {
	tests := []struct {
		name        string
		data        map[string]interface{}
		wantContain string
		mustNotHave string
	}{
		{
			name: "resume failed for good — must not claim success",
			data: map[string]interface{}{
				"RestoreName": "vm-manual-demo-1", "Phase": "Completed", "InPlace": true,
				"ResumeFailed": true, "ResumeError": "helmreleases … forbidden",
				"PollURL": "/x",
			},
			wantContain: "reprise du flux échouée",
			mustNotHave: "Flux repris",
		},
		{
			name: "resume not settled yet — must keep polling, not claim success",
			data: map[string]interface{}{
				"RestoreName": "vm-manual-demo-1", "Phase": "Completed", "InPlace": true,
				"ResumePending": true, "PollURL": "/x",
			},
			wantContain: "reprise du flux en cours",
			mustNotHave: "Flux repris",
		},
		{
			name: "restore itself failed — must say échouée, not terminée",
			data: map[string]interface{}{
				"RestoreName": "vm-manual-demo-1", "Phase": "Failed", "InPlace": true,
				"PollURL": "/x",
			},
			wantContain: "Restauration échouée",
			mustNotHave: "Restauration terminée",
		},
		{
			name: "restore failed AND resume failed — both failures surfaced",
			data: map[string]interface{}{
				"RestoreName": "vm-manual-demo-1", "Phase": "Failed", "InPlace": true,
				"ResumeFailed": true, "ResumeError": "boom", "PollURL": "/x",
			},
			wantContain: "reprise du flux également échouée",
			mustNotHave: "Flux repris",
		},
		{
			name: "resume genuinely settled and succeeded — the one case allowed to say so",
			data: map[string]interface{}{
				"RestoreName": "vm-manual-demo-1", "Phase": "Completed", "InPlace": true,
				"PollURL": "/x",
			},
			wantContain: "Flux repris",
			mustNotHave: "échouée",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := renderVeleroRestoreStatus(t, tt.data)
			if !strings.Contains(out, tt.wantContain) {
				t.Errorf("expected output to contain %q, got:\n%s", tt.wantContain, out)
			}
			if strings.Contains(out, tt.mustNotHave) {
				t.Errorf("expected output NOT to contain %q, got:\n%s", tt.mustNotHave, out)
			}
		})
	}
}
