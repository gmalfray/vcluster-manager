package service

import (
	"context"

	"github.com/gmalfray/vcluster-manager/internal/models"
)

// SumVClusterQuotas reports what the cell has already promised to its tenants,
// excluding the vcluster currently being reconciled.
//
// The exclusion is the whole subtlety: the caller adds the requested quota to
// this total, so leaving the vcluster's own existing quota in would count it
// twice. A vcluster would then start failing its own budget check as soon as it
// was provisioned, on a reconcile that changed nothing about it.
func (s *Service) SumVClusterQuotas(ctx context.Context, env, excluding string) (models.BudgetUsage, error) {
	k8s := s.k8sForEnv(envOrDefault(env))
	if k8s == nil {
		return models.BudgetUsage{}, ErrK8sUnavailable
	}
	return k8s.SumVClusterQuotaHardLimits(ctx, excluding)
}
