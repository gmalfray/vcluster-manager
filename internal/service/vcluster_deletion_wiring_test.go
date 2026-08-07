package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gmalfray/vcluster-manager/internal/config"
	"github.com/gmalfray/vcluster-manager/internal/kubernetes"
	"github.com/gmalfray/vcluster-manager/internal/models"
)

// operatorWiredService construit le Service exactement comme cmd/operator/main.go
// le construit : une configuration, un client Kubernetes, et rien d'autre.
//
// Ce câblage a été écrit pour le contrôleur des marqueurs Velero, qui n'appelle
// effectivement que la moitié Velero du service. Le commentaire de main.go le
// dit : « les autres Deps appartiennent à des méthodes de cycle de vie que cet
// opérateur n'appelle pas ». C'était vrai. Le chantier finalizer a ajouté un
// second reconciler dans le même binaire, et celui-là les appelle.
func operatorWiredService(cfg *config.Config) *Service {
	var mu sync.RWMutex
	return New(Deps{
		Cfg: cfg,
		// Rancher, Keycloak, Vault, GitLab : nil, comme dans le binaire.
		K8sClients:   map[string]*kubernetes.StatusClient{},
		K8sClientsMu: &mu,
	})
}

// L'étape Rancher de la séquence de suppression est sautée en entier, et
// l'opérateur annonce que Rancher n'est pas activé sur la cell — alors que sa
// configuration dit le contraire.
//
// `InspectRancherTeardown` teste `s.rancher == nil || !cfg.RancherEnabledForEnv(env)`
// et rend le même état pour les deux : Enabled=false, « Rancher n'est pas actif
// sur cette cell ». Le finalizer lit Enabled et retourne « étape terminée » sans
// rien faire ni rien écrire. Le vcluster est détruit, son cluster reste dans la
// console Rancher, et rien nulle part ne le signale.
//
// Deux causes distinctes, une seule réponse : « la cell n'a pas Rancher » et
// « ce binaire n'a pas de client Rancher » ne veulent pas dire la même chose, et
// c'est la seconde qu'il faut corriger.
func TestOperatorWiringSkipsTheRancherTeardownSilently(t *testing.T) {
	ctx := context.Background()

	rancherOn := &config.Config{RancherEnabledPreprod: true}
	rancherOff := &config.Config{}

	stOn := operatorWiredService(rancherOn).InspectRancherTeardown(ctx, "demo", "preprod")
	stOff := operatorWiredService(rancherOff).InspectRancherTeardown(ctx, "demo", "preprod")

	// « Pas de client » et « Rancher désactivé » doivent être distinguables : le
	// premier veut dire « je ne peux pas savoir », le second « il n'y a rien à
	// dépairer ». Les confondre faisait rapporter l'étape comme faite par un
	// binaire incapable de l'exécuter, et laissait un cluster fantôme dans
	// Rancher sans une ligne pour le dire.
	if !stOn.NotConfigured {
		t.Error("client Rancher absent sur une cell qui l'annonce actif : NotConfigured=false, " +
			"donc le finalizer va sauter l'étape en silence")
	}
	if stOff.NotConfigured {
		t.Error("Rancher désactivé rapporté comme mal configuré : « rien à dépairer » est un fait")
	}
	if stOn == stOff {
		t.Fatalf("les deux cas restent indiscernables (%+v)", stOn)
	}
}

// Le dépairage ne répond plus « c'est fait » sans avoir rien fait.
func TestOperatorWiringReportsAnUnpairThatNeverHappened(t *testing.T) {
	s := operatorWiredService(&config.Config{RancherEnabledPreprod: true})
	admin := models.Actor{Username: "vcluster-operator", IsAdmin: true}

	// nil voudrait dire « le dépairage est passé » : le finalizer posait la
	// condition Unpairing et avançait sur un dépairage qui n'avait jamais eu lieu.
	// Même convention que son voisin UnpairRancher, qui rend ErrRancherNotConfigured
	// avant même de regarder l'environnement.
	if err := s.UnpairForDeletion(context.Background(), admin, "demo", "preprod"); !errors.Is(err, ErrRancherNotConfigured) {
		t.Fatalf("UnpairForDeletion rend %v, attendu ErrRancherNotConfigured : le finalizer "+
			"prendrait ce nil pour un dépairage réussi", err)
	}
}

// Le status observé ne dit plus « Rancher éteint » sur une cell où il est actif.
//
// C'était la version lisible du même trou : l'exploitant qui regarde
// `kubectl describe vc` concluait que l'appairage n'avait jamais été demandé.
//
// `Enabled` reste false — c'est le contrat de l'UI (« y a-t-il une section
// Rancher à montrer »), et le retourner afficherait une section inutilisable sur
// toute installation sans Rancher. C'est `NotConfigured` qui porte la nuance,
// donc l'ajout est purement additif et les tests existants de GetRancherStatus
// gardent leur sens.
func TestOperatorWiringReportsRancherOffOnACellWhereItIsOn(t *testing.T) {
	s := operatorWiredService(&config.Config{RancherEnabledPreprod: true})
	st := s.GetRancherStatus(context.Background(), "demo", "preprod")

	if !st.NotConfigured {
		t.Fatal("client absent non signalé : l'observation va écrire « rien à appairer »")
	}
	if got := rancherStateOf(st); got != RancherStateUnknown {
		t.Fatalf("état = %q, attendu %q : « éteint » est une affirmation, or on ne sait pas",
			got, RancherStateUnknown)
	}
	obs := s.ObserveVCluster(context.Background(), "demo", "preprod")
	if obs.RancherKnown {
		t.Error("RancherKnown=true sans client pour interroger Rancher")
	}
}

// Les gardes d'autorisation et de nom tiennent quand même sur les deux méthodes
// du finalizer qui écrivent. L'opérateur passe SystemActor (IsAdmin=true), donc
// c'est le nom qui reste la seule barrière avant une concaténation de namespace.
func TestDeletionOpsStillRefuseANonAdminAndABadName(t *testing.T) {
	ctx := context.Background()
	s := operatorWiredService(&config.Config{RancherEnabledPreprod: true})
	lecteur := models.Actor{Username: "bob"}
	admin := models.Actor{Username: "vcluster-operator", IsAdmin: true}

	if err := s.UnpairForDeletion(ctx, lecteur, "demo", "preprod"); err != ErrForbidden {
		t.Fatalf("UnpairForDeletion pour un lecteur = %v, attendu ErrForbidden", err)
	}
	if _, err := s.TeardownVCluster(ctx, lecteur, "demo", "preprod", TeardownOptions{}); err != ErrForbidden {
		t.Fatalf("TeardownVCluster pour un lecteur = %v, attendu ErrForbidden", err)
	}
	if err := s.UnpairForDeletion(ctx, admin, "../kube-system", "preprod"); err != ErrInvalidName {
		t.Fatalf("UnpairForDeletion sur un nom refusé = %v, attendu ErrInvalidName", err)
	}
	if _, err := s.TeardownVCluster(ctx, admin, "../kube-system", "preprod", TeardownOptions{}); err != ErrInvalidName {
		t.Fatalf("TeardownVCluster sur un nom refusé = %v, attendu ErrInvalidName", err)
	}
	if _, err := s.InspectDeletionBackup(ctx, "../kube-system", "preprod", time.Time{}); err != ErrInvalidName {
		t.Fatalf("InspectDeletionBackup sur un nom refusé = %v, attendu ErrInvalidName", err)
	}
}

// operatorWiredServiceWithK8s ajoute le seul client que l'opérateur a vraiment,
// pour que TeardownVCluster aille au-delà de son garde ErrK8sUnavailable.
func operatorWiredServiceWithK8s(cfg *config.Config) *Service {
	var mu sync.RWMutex
	return New(Deps{
		Cfg:          cfg,
		K8sClients:   map[string]*kubernetes.StatusClient{"preprod": kubernetes.NewTestStatusClient()},
		K8sClientsMu: &mu,
	})
}

// Chaque étape de destruction que ce processus ne peut pas exécuter doit
// apparaître dans les avertissements.
//
// C'était le trou le plus coûteux du câblage : les branches Keycloak, Vault et
// GitLab étaient gardées par des `!= nil` écrits pour l'app web, où les clients
// sont toujours là. Dans l'opérateur ils sont nil, donc les trois étaient sautées
// sans un mot, et le status annonçait « séquence de suppression terminée ».
//
// Le pire des trois est le dépôt app-manifests : sa suppression est un geste
// EXPLICITE, demandé par annotation, qui devenait un no-op rapportant un succès.
func TestOperatorWiringWarnsAboutEveryTeardownStepItCannotRun(t *testing.T) {
	ctx := context.Background()
	s := operatorWiredServiceWithK8s(&config.Config{})
	admin := models.Actor{Username: "vcluster-operator", IsAdmin: true}

	warnings, err := s.TeardownVCluster(ctx, admin, "demo", "preprod",
		TeardownOptions{DeleteAppManifestsRepo: true})
	if err != nil {
		t.Fatalf("TeardownVCluster: %v", err)
	}

	joint := strings.Join(warnings, " | ")
	for _, attendu := range []string{"Keycloak", "Vault", "app-manifests"} {
		if !strings.Contains(joint, attendu) {
			t.Errorf("aucun avertissement pour %s : l'étape est sautée en silence et le status "+
				"annoncera « séquence terminée ». Avertissements = %q", attendu, joint)
		}
	}
}

// Le pendant : les avertissements doivent être SPÉCIFIQUES, pas systématiques.
// Sans ce test, une implémentation qui avertit toujours de tout passerait le test
// ci-dessus sans rien dire d'utile.
func TestTeardownDoesNotWarnAboutAStepNobodyAskedFor(t *testing.T) {
	ctx := context.Background()
	s := operatorWiredServiceWithK8s(&config.Config{})
	admin := models.Actor{Username: "vcluster-operator", IsAdmin: true}

	// DeleteAppManifestsRepo à false : le dépôt survit par défaut, il n'y a donc
	// rien à signaler à son sujet.
	warnings, err := s.TeardownVCluster(ctx, admin, "demo", "preprod", TeardownOptions{})
	if err != nil {
		t.Fatalf("TeardownVCluster: %v", err)
	}
	if joint := strings.Join(warnings, " | "); strings.Contains(joint, "app-manifests") {
		t.Errorf("avertissement sur le dépôt app-manifests alors que sa suppression n'était pas "+
			"demandée : %q", joint)
	}
}
