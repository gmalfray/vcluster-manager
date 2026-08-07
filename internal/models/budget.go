package models

import "k8s.io/apimachinery/pkg/api/resource"

// BudgetUsage is what a cell has already handed out — the sum of the `hard`
// limits of every vcluster's ResourceQuota.
//
// It lives in models rather than next to the reconciler that consumes it
// because the service is what produces it, and the service cannot import the
// controller package.
type BudgetUsage struct {
	CPU     resource.Quantity
	Memory  resource.Quantity
	Storage resource.Quantity
}
