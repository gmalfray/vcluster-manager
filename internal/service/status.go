package service

import (
	"context"
	"log/slog"

	"github.com/gmalfray/vcluster-manager/internal/kubernetes"
	"github.com/gmalfray/vcluster-manager/internal/models"
)

// StatusData is the real-time status of a vcluster as consumed by the status
// badge fragment (web) and the JSON API. It carries only the fields rendered by
// status_badge.html on the normal (non-deleting) path; the deleting branch and
// the Vault annotation stay in the web adapter, which owns that state.
type StatusData struct {
	HelmRelease    string `json:"helm_release"`
	Kustomization  string `json:"kustomization"`
	K8sVersion     string `json:"k8s_version"`
	CPUUsage       string `json:"cpu_usage"`
	MemoryUsage    string `json:"memory_usage"`
	StorageUsage   string `json:"storage_usage"`
	CPUPercent     int    `json:"cpu_percent"`
	MemoryPercent  int    `json:"memory_percent"`
	StoragePercent int    `json:"storage_percent"`
}

// FluxSummary aggregates HelmRelease readiness counts across environments for the
// dashboard Flux Status card.
type FluxSummary struct {
	Total        int `json:"total"`
	Ready        int `json:"ready"`
	PreprodTotal int `json:"preprod_total"`
	PreprodReady int `json:"preprod_ready"`
	ProdTotal    int `json:"prod_total"`
	ProdReady    int `json:"prod_ready"`
	// Unready nomme les réconciliations en échec, parce que le compteur
	// ci-dessus ne suffit pas : un « 8/14 » ambre dit qu'il faut chercher,
	// pas où chercher. Le 2026-08-08, six HelmReleases cert-manager étaient
	// en échec depuis des heures — le chiffre l'affichait, et il a fallu
	// ouvrir un kubectl pour savoir lesquels.
	Unready []kubernetes.UnreadyReconciliation `json:"unready,omitempty"`
}

// GetStatus returns the real-time status of a vcluster (the normal path of the
// status badge). Read-only, no privilege required. Returns ErrK8sUnavailable when
// no Kubernetes client is configured for the environment, or the underlying
// Kubernetes error when the status lookup fails.
func (s *Service) GetStatus(ctx context.Context, name, env string) (StatusData, error) {
	env = envOrDefault(env)

	k8s := s.k8sForEnv(env)
	if k8s == nil {
		return StatusData{}, ErrK8sUnavailable
	}

	status, err := k8s.GetVClusterStatus(ctx, name)
	if err != nil {
		return StatusData{}, err
	}

	return StatusData{
		HelmRelease:    status.HelmRelease,
		Kustomization:  status.FluxKustomization,
		K8sVersion:     status.K8sVersion,
		CPUUsage:       status.CPUUsage,
		MemoryUsage:    status.MemoryUsage,
		StorageUsage:   status.StorageUsage,
		CPUPercent:     status.CPUPercent,
		MemoryPercent:  status.MemoryPercent,
		StoragePercent: status.StoragePercent,
	}, nil
}

// GetFluxSummary counts HelmReleases (total and ready) on preprod and prod, plus
// the combined totals. Read-only, no privilege required. Environments without a
// configured client — or whose count fails — contribute zero (the error is
// logged), mirroring the historical dashboard behavior.
func (s *Service) GetFluxSummary(ctx context.Context) FluxSummary {
	var summary FluxSummary

	for _, env := range []string{"preprod", "prod"} {
		k8s := s.k8sForEnv(env)
		if k8s == nil {
			continue
		}
		total, ready, err := k8s.CountReadyHelmReleases(ctx)
		if err != nil {
			slog.Error("error counting helm releases", "env", env, "err", err)
			continue
		}
		switch env {
		case "preprod":
			summary.PreprodTotal, summary.PreprodReady = total, ready
		case "prod":
			summary.ProdTotal, summary.ProdReady = total, ready
		}

		// Le détail est ce qui rend le compteur exploitable. Son échec ne doit
		// pas emporter le compteur lui-même : « 8/14 » sans le détail reste
		// plus utile que rien, alors qu'un tableau de bord vide ne dit pas si
		// tout va bien ou si personne ne regarde.
		unready, err := k8s.ListUnreadyReconciliations(ctx)
		if err != nil {
			slog.Error("error listing unready reconciliations", "env", env, "err", err)
			continue
		}
		summary.Unready = append(summary.Unready, unready...)
	}

	summary.Total = summary.PreprodTotal + summary.ProdTotal
	summary.Ready = summary.PreprodReady + summary.ProdReady
	return summary
}

// GetQuotaForm parses the vcluster config backing the quota editing form.
// Read-only, no privilege required. Returns ErrVClusterNotFound when the
// vcluster cannot be parsed (unknown name/env).
func (s *Service) GetQuotaForm(ctx context.Context, env, name string) (*models.VCluster, error) {
	env = envOrDefault(env)

	vc, err := s.parser.ParseVCluster(ctx, env, name)
	if err != nil {
		return nil, ErrVClusterNotFound
	}
	return vc, nil
}
