package service

import (
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/gmalfray/vcluster-manager/internal/models"
)

// ErrQuotaBelowFloor means the requested quota cannot host the requested
// options, whatever the cell has left.
//
// Distinct d'ErrBudgetExceeded (contrôleur, vcluster_budget.go), qui répond à
// la question inverse : « la cell a-t-elle encore la place ? ». Ici la cell
// n'entre pas en jeu — le quota demandé est trop petit pour ce qu'on lui
// demande d'héberger, et il le resterait sur un cluster vide.
var ErrQuotaBelowFloor = errors.New("quota trop petit pour les options demandées")

// Planchers de ressources, MESURÉS le 2026-08-09 sur la plateforme de recette
// (K3s 1.32, chart vcluster 0.34.7, ArgoCD `stable`) et non estimés.
//
// Protocole : un vcluster sans ArgoCD (`demo`) et un avec (`recette-restore-a`),
// tous deux convergés, puis lecture de `.status.used` de leur ResourceQuota.
//
//	demo               : 140m cpu / 470Mi  / 3 pods
//	recette-restore-a  : 490m cpu / 1366Mi / 10 pods
//	différence (ArgoCD): 350m cpu / 896Mi  / 7 pods
//
// D'où viennent ces valeurs, et c'est ce qui compte pour les maintenir : le
// manifeste amont d'ArgoCD ne déclare AUCUNE `resources.requests` — vérifié sur
// argo-cd/stable/manifests/install.yaml, 7 workloads, 10 conteneurs, zéro
// `resources`. Ce que le quota compte n'est donc pas un besoin d'ArgoCD mais le
// `defaultRequest` du LimitRange que pose le chart vcluster (50m / 128Mi par
// conteneur) : 7 × 128Mi = 896Mi, 7 × 50m = 350m, à l'unité près.
//
// Conséquence pour qui met ces chiffres à jour : ils bougent quand le
// LimitRange du chart change, ou le jour où ArgoCD se met à déclarer ses
// propres requests — pas quand ArgoCD consomme davantage. Un dépassement de
// consommation réelle se traduit par un OOMKill contre la `limit` héritée, pas
// par un refus de quota, et ce contrôle-ci ne le verrait pas.
const (
	floorCPUVCluster    = "140m"
	floorMemoryVCluster = "470Mi"

	floorCPUArgoCD    = "350m"
	floorMemoryArgoCD = "896Mi"
)

// quotaFloor est le plancher cumulé qu'un jeu d'options impose au quota.
type quotaFloor struct {
	cpu    resource.Quantity
	memory resource.Quantity
	// detail décrit la composition, pour que le message dise d'où vient le
	// chiffre plutôt que de l'asséner.
	detail string
}

// floorFor computes the floor for what the request actually asks to deploy.
//
// Seules les options dont le coût a été MESURÉ entrent dans ce calcul. Velero
// (node-agent côté hôte, hors du namespace du tenant) et le bootstrap FluxCD
// n'y sont pas : les compter avec un chiffre inventé transformerait ce garde-fou
// en refus arbitraire, ce qui est pire que l'absence de contrôle — un admin qui
// se voit refuser une combinaison viable contourne le produit.
func floorFor(req *models.CreateRequest) quotaFloor {
	cpu := resource.MustParse(floorCPUVCluster)
	mem := resource.MustParse(floorMemoryVCluster)
	detail := "socle du vcluster " + floorMemoryVCluster

	if req.ArgoCD {
		cpu.Add(resource.MustParse(floorCPUArgoCD))
		mem.Add(resource.MustParse(floorMemoryArgoCD))
		detail += " + ArgoCD " + floorMemoryArgoCD
	}
	return quotaFloor{cpu: cpu, memory: mem, detail: detail}
}

// ValidateQuotaFitsOptions refuse une combinaison quota/options que le produit
// sait impossible, en disant le minimum requis.
//
// Sans ce contrôle, l'UI propose une case « ArgoCD » sans jamais confronter son
// coût au quota demandé : le vcluster se crée, les fichiers sont commités, Flux
// réconcilie, et une partie des pods est refusée par le ResourceQuota — dans une
// boucle de réconciliation, donc sans que rien ne remonte à celui qui a coché la
// case. L'échec est muet, et c'est ce qui le rend coûteux.
//
// cpu et memory sont les quotas EFFECTIFS (défauts déjà appliqués), pas les
// champs bruts de la requête : un champ vide vaut « quota par défaut », qui doit
// être confronté au plancher comme les autres. Les lire bruts laisserait passer
// exactement les créations qui n'ont rien renseigné.
//
// Un quota vide sur une dimension (aucun défaut configuré) n'est pas contrôlé :
// rien ne sera écrit pour elle, il n'y a pas de plafond à comparer.
func ValidateQuotaFitsOptions(req *models.CreateRequest, cpu, memory string) error {
	if req.NoQuotas {
		return nil
	}
	floor := floorFor(req)

	for _, d := range []struct {
		field    string
		label    string
		demandee string
		plancher resource.Quantity
	}{
		{"cpu", "CPU", cpu, floor.cpu},
		{"memory", "mémoire", memory, floor.memory},
	} {
		if d.demandee == "" {
			continue
		}
		q, err := resource.ParseQuantity(d.demandee)
		if err != nil {
			// validateQuantity a déjà tourné en amont ; si on arrive ici avec une
			// valeur illisible, c'est qu'un chemin d'entrée l'a contournée. Ne pas
			// conclure « ça passe » sur une valeur qu'on ne sait pas lire.
			return fieldError(d.field, fmt.Sprintf("quantité illisible (%q)", d.demandee))
		}
		if q.Cmp(d.plancher) < 0 {
			return fmt.Errorf("%w : %s %s demandé, minimum %s (%s)",
				ErrQuotaBelowFloor, d.label, q.String(), d.plancher.String(), floor.detail)
		}
	}
	return nil
}
