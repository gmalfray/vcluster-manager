package service

import (
	"context"
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

	if stOn.Enabled {
		t.Fatal("Enabled=true : le service distingue enfin « pas de client » de " +
			"« Rancher désactivé », ce test a fait son temps")
	}
	if stOn != stOff {
		t.Fatalf("les deux cas sont désormais distinguables (%+v vs %+v) : très bien, "+
			"mettre à jour ce test", stOn, stOff)
	}
	t.Logf("RANCHER_ENABLED_PREPROD=true mais client nil ⇒ %+v — le finalizer saute l'étape", stOn)
}

// Le dépairage lui-même répond « c'est fait » sans avoir rien fait.
//
// UnpairForDeletion rend nil quand le client Rancher est absent. Pour le
// finalizer, nil veut dire « le dépairage est passé » : il pose la condition
// RancherPaired/Unpairing et avance. Un retour d'erreur ferait au moins
// apparaître la panne dans le status.
func TestOperatorWiringReportsAnUnpairThatNeverHappened(t *testing.T) {
	s := operatorWiredService(&config.Config{RancherEnabledPreprod: true})
	admin := models.Actor{Username: "vcluster-operator", IsAdmin: true}

	if err := s.UnpairForDeletion(context.Background(), admin, "demo", "preprod"); err != nil {
		t.Fatalf("UnpairForDeletion rend %v : le service signale enfin le client manquant, "+
			"ce test a fait son temps", err)
	}
}

// Le status observé ment de la même façon : la condition RancherPaired sort à
// False/Disabled — « Rancher n'est pas activé pour cette cell » — sur une cell
// où il l'est.
//
// C'est la version lisible du même trou : l'exploitant qui regarde
// `kubectl describe vc` conclut que l'appairage Rancher n'a jamais été demandé.
func TestOperatorWiringReportsRancherOffOnACellWhereItIsOn(t *testing.T) {
	s := operatorWiredService(&config.Config{RancherEnabledPreprod: true})
	st := s.GetRancherStatus(context.Background(), "demo", "preprod")

	if st.Enabled {
		t.Fatal("Enabled=true : le service distingue enfin les deux cas, retirer ce test")
	}
	if rancherStateOf(st) != RancherStateOff {
		t.Fatalf("état = %q, ce test fige Off", rancherStateOf(st))
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
