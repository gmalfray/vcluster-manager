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
	// Available reports whether the annotation was actually read: false when
	// there is no client for the environment, or when the read itself failed.
	// When false, the protection toggle cannot be shown/used, and Protected
	// must not be trusted.
	Available bool   `json:"available"`
	Protected bool   `json:"protected"`
	Name      string `json:"name"`
	Env       string `json:"env"`
	// Detail says WHY the answer is unavailable — no client for this cell, or a
	// read that failed, and which. Empty when Available is true.
	//
	// It exists because the finalizer now stops on !Available before destroying
	// anything (vcluster_finalizer.go): "protection unreadable" ends up in a
	// condition someone has to act on, and "je n'ai pas pu lire" without the
	// reason sends them looking. Same channel as RancherTeardownState.Detail.
	Detail string `json:"detail,omitempty"`
}

// GetProtection reads whether the vcluster's host namespace carries the
// protect-deletion annotation. Read-only, no privilege required.
//
// Available is the "did we actually get an answer" channel: false both when
// there is no client for the environment and when the read itself failed.
// Either way, callers must not read Protected as a fact — GetNamespaceProtection
// already keeps "no annotation" apart from "couldn't read", and this is where
// that distinction has to survive into the one field every caller checks.
func (s *Service) GetProtection(ctx context.Context, name, env string) ProtectionState {
	env = envOrDefault(env)
	k8s := s.k8sForEnv(env)
	if k8s == nil {
		return ProtectionState{
			Available: false, Name: name, Env: env,
			Detail: "aucun client Kubernetes pour la cell " + env,
		}
	}
	protected, err := k8s.GetNamespaceProtection(ctx, name)
	if err != nil {
		return ProtectionState{
			Available: false, Name: name, Env: env,
			Detail: err.Error(),
		}
	}
	return ProtectionState{
		Available: true,
		Protected: protected,
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
