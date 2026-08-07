package service

import (
	"testing"
	"time"

	"github.com/gmalfray/vcluster-manager/internal/models"
)

// La règle qui décide si la destruction a un filet : quelle sauvegarde Velero
// « couvre » cette suppression. Elle est lue depuis le cluster à chaque passage
// plutôt que notée quelque part, donc c'est elle qui porte toute la reprise
// après redémarrage du chemin sauvegarde.
func TestPickDeletionBackup(t *testing.T) {
	since := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return since.Add(d).Format(time.RFC3339) }

	cases := []struct {
		name      string
		backups   []models.VeleroBackupInfo
		wantFound bool
		wantName  string
		wantDone  bool
		wantFail  bool
	}{
		{
			name:      "rien du tout",
			wantFound: false,
		},
		{
			name: "la sauvegarde de la nuit ne compte pas : elle est antérieure à la suppression",
			backups: []models.VeleroBackupInfo{
				{Name: "nightly", Phase: "Completed", StartTime: at(-20 * time.Hour), CompletionTime: at(-19 * time.Hour)},
			},
			wantFound: false,
		},
		{
			name: "une sauvegarde terminée après le deletionTimestamp couvre la suppression",
			backups: []models.VeleroBackupInfo{
				{Name: "nightly", Phase: "Completed", StartTime: at(-20 * time.Hour)},
				{Name: "predelete", Phase: "Completed", StartTime: at(time.Minute)},
			},
			wantFound: true, wantName: "predelete", wantDone: true,
		},
		{
			name: "une sauvegarde en cours est adoptée, pas doublée",
			backups: []models.VeleroBackupInfo{
				{Name: "en-cours", Phase: "InProgress", StartTime: at(time.Minute)},
			},
			wantFound: true, wantName: "en-cours",
		},
		{
			name: "une sauvegarde tout juste créée, sans phase ni horodatage, est adoptée aussi",
			backups: []models.VeleroBackupInfo{
				{Name: "neuve"},
			},
			wantFound: true, wantName: "neuve",
		},
		{
			name: "une sauvegarde en cours prime sur une sauvegarde terminée",
			backups: []models.VeleroBackupInfo{
				{Name: "finie", Phase: "Completed", StartTime: at(time.Minute)},
				{Name: "en-cours", Phase: "InProgress", StartTime: at(2 * time.Minute)},
			},
			wantFound: true, wantName: "en-cours",
		},
		{
			name: "un échec postérieur à la suppression est remonté, pas ignoré",
			backups: []models.VeleroBackupInfo{
				{Name: "ratee", Phase: "PartiallyFailed", StartTime: at(time.Minute)},
			},
			wantFound: true, wantName: "ratee", wantFail: true,
		},
		{
			name: "une réussite l'emporte sur un échec de la même fenêtre",
			backups: []models.VeleroBackupInfo{
				{Name: "ratee", Phase: "Failed", StartTime: at(time.Minute)},
				{Name: "reprise", Phase: "Completed", StartTime: at(5 * time.Minute)},
			},
			wantFound: true, wantName: "reprise", wantDone: true,
		},
		{
			name: "sans startTimestamp, le completionTimestamp sert d'ancre",
			backups: []models.VeleroBackupInfo{
				{Name: "sans-debut", Phase: "Completed", CompletionTime: at(3 * time.Minute)},
			},
			wantFound: true, wantName: "sans-debut", wantDone: true,
		},
		{
			name: "une sauvegarde terminée sans aucun horodatage n'est pas attribuable : on en refait une",
			backups: []models.VeleroBackupInfo{
				{Name: "sans-date", Phase: "Completed"},
			},
			wantFound: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pickDeletionBackup(tc.backups, since)
			if got.Found != tc.wantFound {
				t.Fatalf("Found = %v, attendu %v (%+v)", got.Found, tc.wantFound, got)
			}
			if !tc.wantFound {
				return
			}
			if got.Name != tc.wantName {
				t.Fatalf("Name = %q, attendu %q", got.Name, tc.wantName)
			}
			if got.Completed != tc.wantDone {
				t.Fatalf("Completed = %v, attendu %v", got.Completed, tc.wantDone)
			}
			if got.Failed != tc.wantFail {
				t.Fatalf("Failed = %v, attendu %v", got.Failed, tc.wantFail)
			}
		})
	}
}

func TestIsTerminalBackupPhase(t *testing.T) {
	// Une phase vide est celle d'un objet Backup que Velero vient de créer et
	// n'a pas encore regardé. La compter comme terminale ferait conclure « pas
	// de sauvegarde en cours » et en lancerait une deuxième.
	for _, p := range []string{"", "New", "InProgress", "WaitingForPluginOperations"} {
		if IsTerminalBackupPhase(p) {
			t.Fatalf("phase %q comptée comme terminale", p)
		}
	}
	for _, p := range []string{"Completed", "Failed", "PartiallyFailed", "FailedValidation", "Deleting"} {
		if !IsTerminalBackupPhase(p) {
			t.Fatalf("phase %q pas comptée comme terminale", p)
		}
	}
}
