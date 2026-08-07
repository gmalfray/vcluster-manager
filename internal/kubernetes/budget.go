package kubernetes

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/gmalfray/vcluster-manager/internal/models"
)

// SumVClusterQuotaHardLimits adds up what the cell has already handed out: the
// `hard` limits of every ResourceQuota living in a vcluster-* namespace.
//
// The hard limit, not the usage. The question the budget answers is "what have
// we promised", not "what is being consumed right now" — a tenant idle today can
// claim its full quota tomorrow, and a ceiling checked against live usage would
// let the cell be oversubscribed by exactly the amount everyone happens not to
// be using at that instant.
//
// excluding is the vcluster being reconciled. Leaving it in would count it
// twice — once from the quota it already has, once from the request being
// checked — so a vcluster would start failing its own budget check the moment
// it was provisioned, on a reconcile that changed nothing.
func (s *StatusClient) SumVClusterQuotaHardLimits(ctx context.Context, excluding string) (models.BudgetUsage, error) {
	// Cluster-wide list, then filter: the vcluster-* namespaces come and go, so
	// there is nothing stable to enumerate namespace by namespace.
	list, err := s.client.Resource(resourceQuotaGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return models.BudgetUsage{}, fmt.Errorf("listing the cell's ResourceQuotas: %w", err)
	}

	skip := ""
	if excluding != "" {
		skip = "vcluster-" + excluding
	}

	var total models.BudgetUsage
	for _, rq := range list.Items {
		ns := rq.GetNamespace()
		if !strings.HasPrefix(ns, "vcluster-") || ns == skip {
			continue
		}
		// `spec.hard` rather than `status.hard`: a quota just applied has a spec
		// immediately, but its status is filled in by the quota controller a
		// moment later. Reading the status would make a brand-new tenant invisible
		// to the budget for as long as that takes — precisely when two creations
		// racing each other would both be told there is room.
		hard, found, _ := unstructured.NestedStringMap(rq.Object, "spec", "hard")
		if !found {
			hard, _, _ = unstructured.NestedStringMap(rq.Object, "status", "hard")
		}
		for key, dest := range map[string]*resource.Quantity{
			"requests.cpu":     &total.CPU,
			"requests.memory":  &total.Memory,
			"requests.storage": &total.Storage,
		} {
			raw, ok := hard[key]
			if !ok {
				continue
			}
			q, err := resource.ParseQuantity(raw)
			if err != nil {
				// Un quota illisible ne doit pas être compté pour zéro : ce serait
				// sous-estimer l'allocation de la cell et laisser passer un
				// dépassement. On refuse de conclure.
				return models.BudgetUsage{}, fmt.Errorf(
					"quota %s illisible (%q) dans %s : impossible de calculer l'allocation de la cell", key, raw, ns)
			}
			dest.Add(q)
		}
	}
	return total, nil
}
