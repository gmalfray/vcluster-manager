package service

import (
	"context"

	"github.com/gmalfray/vcluster-manager/internal/audit"
	"github.com/gmalfray/vcluster-manager/internal/models"
)

// ProtectionState is the namespace-deletion-protection status of a vcluster.
// It is the single result type returned to both adapters (rendered as an HTMX
// fragment by the web layer, serialized as JSON by the REST layer).
type ProtectionState struct {
	// Available reports whether a Kubernetes client exists for the environment.
	// When false, the protection toggle cannot be shown/used.
	Available bool   `json:"available"`
	Protected bool   `json:"protected"`
	Name      string `json:"name"`
	Env       string `json:"env"`
}

// GetProtection reads whether the vcluster's host namespace carries the
// protect-deletion annotation. Read-only, no privilege required.
func (s *Service) GetProtection(ctx context.Context, name, env string) ProtectionState {
	env = envOrDefault(env)
	k8s := s.k8sForEnv(env)
	if k8s == nil {
		return ProtectionState{Available: false, Name: name, Env: env}
	}
	return ProtectionState{
		Available: true,
		Protected: k8s.GetNamespaceProtection(ctx, name),
		Name:      name,
		Env:       env,
	}
}

// SetProtection enables or disables the protect-deletion annotation on the
// vcluster's host namespace. Admin only (RBAC is enforced here, in the
// service, so both adapters inherit it). Returns ErrForbidden or
// ErrK8sUnavailable when applicable, or the underlying Kubernetes error on
// failure.
func (s *Service) SetProtection(ctx context.Context, actor models.Actor, name, env string, enabled bool) (ProtectionState, error) {
	if !actor.IsAdmin {
		return ProtectionState{}, ErrForbidden
	}
	env = envOrDefault(env)

	k8s := s.k8sForEnv(env)
	if k8s == nil {
		return ProtectionState{}, ErrK8sUnavailable
	}

	if err := k8s.SetNamespaceProtection(ctx, name, enabled); err != nil {
		return ProtectionState{}, err
	}

	action := "disable-protection"
	if enabled {
		action = "enable-protection"
	}
	audit.LogActor(actor.Username, action, name, env)

	return ProtectionState{Available: true, Protected: enabled, Name: name, Env: env}, nil
}
