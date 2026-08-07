package service

import (
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/gmalfray/vcluster-manager/internal/gitops"
	"github.com/gmalfray/vcluster-manager/internal/models"
)

// ErrGeneratorUnavailable means the service was built without a generator, so
// it cannot derive anything from a name and a cell.
var ErrGeneratorUnavailable = errors.New("générateur non configuré")

// RenderVClusterSubstitutions builds the two objects the operator owns outright
// on the host cluster: the vcluster's namespace, and the ConfigMap of values
// Flux substitutes into the shared tenant templates.
//
// Only two, and that is the whole design. Everything else in a vcluster's tree
// stays Flux's to apply, so nothing has two writers — see the tenant
// Kustomizations, which point at ./lib and read their per-vcluster values from
// this ConfigMap instead of from an overlay committed next to them.
//
// k8sVersion is separate from req because CreateRequest has no field for it; it
// arrives on UpdateRequest, and buildData takes it apart for the same reason.
func (s *Service) RenderVClusterSubstitutions(req *models.CreateRequest, env, k8sVersion string) ([]*unstructured.Unstructured, error) {
	// Le nom devient un namespace deux lignes plus bas, et un namespace est la
	// seule frontière entre deux tenants. On le valide avant toute
	// concaténation, pas après.
	if !validName(req.Name) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidName, req.Name)
	}
	if s.generator == nil {
		return nil, ErrGeneratorUnavailable
	}

	return []*unstructured.Unstructured{
		gitops.HostNamespace(req.Name),
		s.generator.SubstitutionConfigMap(req, envOrDefault(env), k8sVersion),
	}, nil
}

// EffectiveQuotas rend le quota qui sera RÉELLEMENT écrit pour ce vcluster.
//
// Elle existe pour que le contrôle de budget interroge la même source que le
// provisionnement, au lieu de recalculer la règle de son côté. Les deux avaient
// divergé : le provisionnement traitait un bloc `quotas` absent comme « quotas
// actifs, valeurs par défaut du générateur » — ce que le commentaire de QuotaSpec
// revendique — tandis que le budget le traitait comme « rien à imputer ». Un CR
// qui omettait trois lignes obtenait donc un ResourceQuota que le plafond de la
// cell ne comptait jamais.
//
// Recopier les valeurs par défaut dans le contrôleur aurait rouvert le même trou
// par une autre porte : deux endroits à configurer, dont un qu'on peut oublier
// sans que rien ne le dise. Ici il n'y a qu'une source, et elle est celle qui
// rend le ConfigMap.
func (s *Service) EffectiveQuotas(req *models.CreateRequest, env string) (cpu, mem, sto string, enabled bool, err error) {
	if s.generator == nil {
		return "", "", "", false, ErrGeneratorUnavailable
	}
	subs := s.generator.Substitutions(req, envOrDefault(env), "")
	return subs["QUOTA_CPU"], subs["QUOTA_MEMORY"], subs["QUOTA_STORAGE"],
		subs["QUOTAS_ENABLED"] == "true", nil
}
