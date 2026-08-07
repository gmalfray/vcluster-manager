package service

import (
	"errors"
	"fmt"

	"github.com/gmalfray/vcluster-manager/internal/gitops"
)

// La règle elle-même vit dans gitops, près du rendu qu'elle protège. Ici on ne
// fait que l'appliquer plus tôt : la CRD refuse à l'admission ce qui arrive par
// Flux, ce contrôle refuse ce qui arrive par l'UI et par l'API REST, qui ne
// passent pas par l'API server.

// ErrInvalidRBACGroup means a requested OIDC group could not be rendered safely.
var ErrInvalidRBACGroup = errors.New("invalid ArgoCD RBAC group")

// ValidateRBACGroups refuse un groupe qui ne peut pas être rendu sans risque.
//
// Refuser et non assainir : ces valeurs deviennent des lignes de policy.csv,
// c'est-à-dire des droits d'accès. Un groupe discrètement corrigé ou écarté
// donne un accès qui n'existe pas, et personne ne s'en aperçoit avant que
// quelqu'un se plaigne de ne pas pouvoir entrer.
func ValidateRBACGroups(groups []string) error {
	for _, g := range groups {
		if g == "" {
			continue
		}
		if !gitops.ValidRBACGroup(g) {
			return fmt.Errorf("%w: %q — seuls [A-Za-z0-9_.:@/-] sont acceptés, "+
				"un saut de ligne ou une virgule permettrait d'injecter une règle arbitraire", ErrInvalidRBACGroup, g)
		}
	}
	return nil
}
