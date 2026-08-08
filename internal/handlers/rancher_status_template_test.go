package handlers

import (
	"bytes"
	"html/template"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gmalfray/vcluster-manager/internal/service"
)

// renderRancherStatus rend le fragment tel que le serveur le rendrait.
func renderRancherStatus(t *testing.T, st service.RancherStatus) string {
	t.Helper()
	chemin := filepath.Join("..", "..", "web", "templates", "partials", "rancher_status.html")
	tmpl, err := template.ParseFiles(chemin)
	if err != nil {
		t.Fatalf("parsing %s: %v", chemin, err)
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "rancher_status.html", st); err != nil {
		t.Fatalf("rendu: %v", err)
	}
	return buf.String()
}

// TestRancherStatus_PairingOffersAWayOut ferme une impasse.
//
// `Pairing` se calcule en « le cluster existe dans Rancher mais n'est pas
// actif ». Rien ne le borne dans le temps : si l'appairage n'aboutit pas, le
// vcluster y reste indéfiniment. Tant que cet état désactivait l'interrupteur,
// il n'offrait aucune sortie.
//
// Ce n'est pas hypothétique. Le 2026-08-08, un appairage bloqué par un droit
// RBAC manquant (`create pods/portforward`) a laissé un vcluster avec un
// « appairage en cours » perpétuel et pas un seul bouton pour revenir en
// arrière. Il a fallu supprimer le cluster à la main côté Rancher.
//
// `UnpairRancher` n'exige pas que le cluster soit actif — il le cherche, le
// supprime s'il existe, puis nettoie. Il fonctionne donc sur un appairage à
// moitié fait : la sortie de secours existait côté service, seule l'IHM
// l'interdisait.
func TestRancherStatus_PairingOffersAWayOut(t *testing.T) {
	html := renderRancherStatus(t, service.RancherStatus{
		Enabled: true,
		Pairing: true,
		Name:    "recette-restore-a",
		Env:     "preprod",
	})

	if strings.Contains(html, "disabled") {
		t.Error("l'interrupteur est désactivé pendant l'appairage : un appairage qui n'aboutit pas " +
			"n'offre alors aucune sortie, et le vcluster reste bloqué sur « en cours » pour toujours")
	}
	if !strings.Contains(html, "unpair-rancher") {
		t.Error("l'action proposée pendant l'appairage ne pointe pas vers unpair-rancher : " +
			"le cluster existe déjà côté Rancher, relancer un pair ne ferait que rebondir " +
			"sur le garde « existe déjà »")
	}
	if strings.Contains(html, "pair-rancher") && !strings.Contains(html, "unpair-rancher") {
		t.Error("l'action pointe vers pair-rancher alors que le cluster existe déjà")
	}
}

// TestRancherStatus_CleaningStaysBlocked : la contrepartie. « Cleaning » est
// une suppression déjà engagée ; l'interrompre laisserait le vcluster à moitié
// détruit. Sans ce test, « débloquer Pairing » se ferait en débloquant les deux.
func TestRancherStatus_CleaningStaysBlocked(t *testing.T) {
	html := renderRancherStatus(t, service.RancherStatus{
		Enabled:  true,
		Cleaning: true,
		Name:     "demo",
		Env:      "preprod",
	})

	if !strings.Contains(html, "disabled") {
		t.Error("l'interrupteur est actionnable pendant le nettoyage : interrompre une " +
			"suppression déjà engagée laisserait le vcluster à moitié détruit")
	}
}

// TestRancherStatus_NominalPathsUnchanged garde les deux états de repos : un
// vcluster appairé propose de le dépairer, un vcluster libre propose de
// l'appairer. C'est le plancher qui empêche de « corriger » l'état Pairing en
// cassant les cas ordinaires.
func TestRancherStatus_NominalPathsUnchanged(t *testing.T) {
	appairé := renderRancherStatus(t, service.RancherStatus{
		Enabled: true, Paired: true, Name: "demo", Env: "preprod",
	})
	if !strings.Contains(appairé, "unpair-rancher") {
		t.Error("un vcluster appairé ne propose pas de le dépairer")
	}
	if strings.Contains(appairé, "disabled") {
		t.Error("un vcluster appairé au repos ne doit pas avoir d'interrupteur désactivé")
	}

	libre := renderRancherStatus(t, service.RancherStatus{
		Enabled: true, Name: "demo", Env: "preprod",
	})
	if !strings.Contains(libre, "/pair-rancher") {
		t.Error("un vcluster non appairé ne propose pas de l'appairer")
	}
}
