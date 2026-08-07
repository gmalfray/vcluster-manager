package service

import (
	"context"

	"github.com/gmalfray/vcluster-manager/internal/audit"
	"github.com/gmalfray/vcluster-manager/internal/models"
)

// SuspendVCluster met un vcluster en sommeil sans rien détruire : Flux est
// suspendu pour que rien ne le remonte, puis les charges de travail passent à
// zéro réplique. Le volume, les manifests et le namespace restent intacts.
//
// C'est la moitié réversible de la suppression (crd-vcluster.md §4.2). Elle
// existe parce que `metadata.deletionTimestamp` ne peut pas la porter : une fois
// posé par Kubernetes il est irréversible, même avec un finalizer qui retient
// encore l'objet. Le délai de grâce annulable doit donc se jouer avant, sur le
// `spec`.
//
// Admin uniquement.
func (s *Service) SuspendVCluster(ctx context.Context, actor models.Actor, name, env string) error {
	if !actor.IsAdmin {
		return ErrForbidden
	}
	if !validName(name) {
		return ErrInvalidName
	}
	env = envOrDefault(env)
	k8s := s.k8sForEnv(env)
	if k8s == nil {
		return ErrK8sUnavailable
	}

	// Suspendre Flux d'abord : le faire après le scale laisserait une fenêtre où
	// Flux remonte les répliques qu'on vient de descendre.
	if err := k8s.SetFluxSuspend(ctx, name, true); err != nil {
		return err
	}
	if err := k8s.ScaleVClusterWorkloads(ctx, name, 0); err != nil {
		return err
	}

	audit.LogActor(actor.Username, "vcluster-suspend", name, env)
	return nil
}

// ResumeVCluster annule une mise en sommeil. Reprendre Flux suffit : c'est lui
// qui remonte les répliques en réconciliant le HelmRelease, donc on ne scale pas
// à la main — ce serait se battre avec lui pour le même champ.
//
// Même mécanisme que l'abandon d'une restauration in-place, mais une ligne
// d'audit distincte : « pourquoi » compte autant que « quoi » dans une trace.
//
// Admin uniquement.
func (s *Service) ResumeVCluster(ctx context.Context, actor models.Actor, name, env string) error {
	if !actor.IsAdmin {
		return ErrForbidden
	}
	if !validName(name) {
		return ErrInvalidName
	}
	env = envOrDefault(env)
	k8s := s.k8sForEnv(env)
	if k8s == nil {
		return ErrK8sUnavailable
	}

	if err := k8s.SetFluxSuspend(ctx, name, false); err != nil {
		return err
	}

	audit.LogActor(actor.Username, "vcluster-resume", name, env)
	return nil
}
