package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/gmalfray/vcluster-manager/internal/config"
	"github.com/gmalfray/vcluster-manager/internal/gitops"
	"github.com/gmalfray/vcluster-manager/internal/kubernetes"
	"github.com/gmalfray/vcluster-manager/internal/models"
)

// serviceWithQuotaDefaults builds a service whose generator carries the given
// default quotas — the only way to exercise the "champ vide = défaut" path.
//
// Pas de parser : le contrôle refuse AVANT `parser.Exists`, donc ces tests
// n'atteignent jamais un client nil. Un test du cas passant irait plus loin et
// demanderait tout le harnais GitLab ; il est couvert unitairement plus haut.
func serviceWithQuotaDefaults(cpu, memory string) *Service {
	var mu sync.RWMutex
	return New(Deps{
		Cfg: &config.Config{},
		Generator: gitops.NewGenerator(gitops.GeneratorConfig{
			DefaultCPU:    cpu,
			DefaultMemory: memory,
		}),
		K8sClients:   map[string]*kubernetes.StatusClient{},
		K8sClientsMu: &mu,
	})
}

// Le contrôle doit refuser DEPUIS Create, pas seulement quand on l'appelle
// directement. Sans ce test, le branchement pourrait disparaître de Create sans
// qu'un seul test ne tombe : tous les autres tests de Create s'arrêtent plus tôt
// (nom invalide, RBAC, quantité malformée) et ne traversent jamais cette ligne.
func TestCreate_RefusesAQuotaThatCannotHostArgoCD(t *testing.T) {
	s := serviceWithQuotaDefaults("8", "32Gi")
	admin := models.Actor{Username: "alice", IsAdmin: true}

	_, err := s.Create(context.Background(), admin, &models.CreateRequest{
		Name: "trop-petit", ArgoCD: true, CPU: "2", Memory: "1Gi",
	}, "preprod")

	if !errors.Is(err, ErrQuotaBelowFloor) {
		t.Fatalf("err = %v, attendu ErrQuotaBelowFloor : Create doit refuser avant de committer quoi que ce soit", err)
	}
}

// Le cas qui motive la lecture des quotas EFFECTIFS : la requête ne renseigne
// aucun quota, donc ce sont les défauts du générateur qui seront écrits. Lire
// req.CPU/req.Memory bruts les verrait vides et laisserait passer précisément
// les créations qui n'ont rien saisi — la majorité d'entre elles.
func TestCreate_ChecksTheEffectiveQuotaNotTheRawFields(t *testing.T) {
	s := serviceWithQuotaDefaults("2", "1Gi")
	admin := models.Actor{Username: "alice", IsAdmin: true}

	_, err := s.Create(context.Background(), admin, &models.CreateRequest{
		Name: "defauts-trop-petits", ArgoCD: true,
	}, "preprod")

	if !errors.Is(err, ErrQuotaBelowFloor) {
		t.Fatalf("err = %v, attendu ErrQuotaBelowFloor : les défauts du générateur (2 / 1Gi) "+
			"ne peuvent pas héberger ArgoCD, et les champs de la requête sont vides", err)
	}
}

// Le cas qui motive tout le contrôle : l'UI propose une case « ArgoCD » sans
// jamais confronter son coût au quota demandé. Le vcluster se crée, Flux
// réconcilie, et une partie des pods est refusée par le ResourceQuota dans une
// boucle de réconciliation — personne ne voit rien.
func TestArgoCDIsRefusedWhenTheQuotaCannotHostIt(t *testing.T) {
	req := &models.CreateRequest{Name: "trop-petit", ArgoCD: true}

	err := ValidateQuotaFitsOptions(req, "2", "1Gi")
	if err == nil {
		t.Fatal("1Gi accepté avec ArgoCD : le plancher mesuré est 1366Mi (470Mi de socle + 896Mi d'ArgoCD)")
	}
	if !errors.Is(err, ErrQuotaBelowFloor) {
		t.Fatalf("err = %v, attendu ErrQuotaBelowFloor", err)
	}
	// Le message doit porter le minimum requis, sinon il ne dit pas quoi faire.
	// C'est toute la différence avec l'échec muet qu'il remplace.
	for _, attendu := range []string{"1366Mi", "socle du vcluster", "ArgoCD"} {
		if !strings.Contains(err.Error(), attendu) {
			t.Errorf("le message ne contient pas %q : %v", attendu, err)
		}
	}
}

// Le pendant, sans lequel le contrôle serait un refus arbitraire : le MÊME
// quota doit passer sans ArgoCD. Le plancher suit les options demandées, il
// n'est pas un minimum universel.
func TestTheSameQuotaPassesWithoutArgoCD(t *testing.T) {
	req := &models.CreateRequest{Name: "trop-petit"}

	if err := ValidateQuotaFitsOptions(req, "2", "1Gi"); err != nil {
		t.Fatalf("1Gi refusé sans ArgoCD alors que le socle ne demande que 470Mi : %v", err)
	}
}

func TestCPUIsCheckedToNotJustMemory(t *testing.T) {
	req := &models.CreateRequest{Name: "cpu-court", ArgoCD: true}

	err := ValidateQuotaFitsOptions(req, "250m", "8Gi")
	if err == nil {
		t.Fatal("250m accepté avec ArgoCD : le plancher CPU mesuré est 490m (140m + 350m)")
	}
	if !strings.Contains(err.Error(), "490m") {
		t.Errorf("le message ne porte pas le minimum CPU : %v", err)
	}
}

// Un quota confortable ne doit rien déclencher — un garde-fou qui mord sur les
// créations légitimes se fait désactiver, et on perd les deux.
func TestARealisticQuotaIsAccepted(t *testing.T) {
	req := &models.CreateRequest{Name: "normal", ArgoCD: true}

	if err := ValidateQuotaFitsOptions(req, "8", "32Gi"); err != nil {
		t.Fatalf("le quota par défaut du générateur (8 / 32Gi) est refusé : %v", err)
	}
}

// `no_quotas` est un opt-out explicite : aucun ResourceQuota n'est écrit, donc
// il n'y a pas de plafond contre lequel comparer un plancher.
func TestNoQuotasSkipsTheCheckEntirely(t *testing.T) {
	req := &models.CreateRequest{Name: "sans-quota", ArgoCD: true, NoQuotas: true}

	if err := ValidateQuotaFitsOptions(req, "1m", "1Mi"); err != nil {
		t.Fatalf("contrôle appliqué malgré no_quotas : %v", err)
	}
}

// Une dimension sans valeur effective n'est pas contrôlée : rien ne sera écrit
// pour elle. Contrôler quand même reviendrait à refuser sur une comparaison
// avec du vide.
func TestAnEmptyDimensionIsNotChecked(t *testing.T) {
	req := &models.CreateRequest{Name: "partiel", ArgoCD: true}

	if err := ValidateQuotaFitsOptions(req, "", "32Gi"); err != nil {
		t.Fatalf("CPU vide traité comme un dépassement : %v", err)
	}
}

// validateQuantity tourne en amont dans Create ; arriver ici avec une valeur
// illisible signifie qu'un chemin d'entrée l'a contournée. Ne pas conclure
// « ça passe » sur une valeur qu'on ne sait pas lire.
func TestAnUnreadableQuantityIsRefusedNotIgnored(t *testing.T) {
	req := &models.CreateRequest{Name: "illisible", ArgoCD: true}

	if err := ValidateQuotaFitsOptions(req, "2", "beaucoup"); err == nil {
		t.Fatal("quantité illisible acceptée : le contrôle est contournable en envoyant n'importe quoi")
	}
}
