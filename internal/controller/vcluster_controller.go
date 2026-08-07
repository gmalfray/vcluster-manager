package controller

import (
	"context"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	"github.com/gmalfray/vcluster-manager/internal/models"
	"github.com/gmalfray/vcluster-manager/internal/service"

	"github.com/gmalfray/vcluster-manager/api/v1alpha1"
)

// DefaultGracePeriod is how long a suspended vcluster stays recoverable before
// the deletion may proceed. A duration, not a policy: nothing here deletes
// anything when it expires — expiry only marks the window as closed, and the
// third Git commit is still what triggers the real deletion
// (crd-vcluster.md §4.2).
const DefaultGracePeriod = 7 * 24 * time.Hour

// VClusterOps is the slice of the service the vcluster reconciler needs.
// Declared here, where it is consumed, rather than in the service: the seam
// belongs to the caller. seam_assert below proves *service.Service satisfies it.
type VClusterOps interface {
	SuspendVCluster(ctx context.Context, actor models.Actor, name, env string) error
	ResumeVCluster(ctx context.Context, actor models.Actor, name, env string) error
}

var _ VClusterOps = (*service.Service)(nil)

// VClusterReconciler owns the lifecycle of a VCluster CR.
//
// Today it implements only the reversible half of deletion: reacting to
// spec.suspend. The full provisioning reconcile (expanding the CR into Flux
// resources) and the finalizer come next — deliberately in that order, because
// suspend is what the finalizer's grace period hangs off, and it destroys
// nothing.
type VClusterReconciler struct {
	client.Client

	// Ops is the service seam — *service.Service in production.
	Ops VClusterOps

	// Cell names the host cluster this operator reconciles (ADR-002).
	Cell string

	// Budget est le plafond de ressources de la cell. Vide = aucun plafond
	// configuré, ce qui fait REFUSER les créations avec quotas (crd-vcluster.md
	// §5.3) plutôt que les laisser passer sans contrôle.
	Budget BudgetLimits

	// BudgetOps lit ce que la cell a déjà alloué. Nil désactive la vérification
	// — réservé aux tests qui ne portent pas sur le budget.
	BudgetOps BudgetReader

	// GracePeriod overrides DefaultGracePeriod. Zero means the default.
	GracePeriod time.Duration
}

func (r *VClusterReconciler) gracePeriod() time.Duration {
	if r.GracePeriod > 0 {
		return r.GracePeriod
	}
	return DefaultGracePeriod
}

// Reconcile drives a VCluster towards its desired state.
func (r *VClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var vc v1alpha1.VCluster
	if err := r.Get(ctx, req.NamespacedName, &vc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Un deletionTimestamp veut dire que le CR s'en va : ce chemin appartient au
	// finalizer, pas à la réconciliation normale.
	if !vc.DeletionTimestamp.IsZero() {
		return r.reconcileDeletion(ctx, &vc)
	}

	vc.Status.ObservedGeneration = vc.Generation

	// Le sommeil d'abord : inutile de provisionner ce qu'on vient d'endormir.
	if err := r.reconcileSuspend(ctx, &vc); err != nil {
		return ctrl.Result{}, err
	}

	var requeue time.Duration
	if vc.Status.Phase != v1alpha1.VClusterPhaseSuspended {
		// Le budget avant le provisionnement : on ne matérialise rien qu'on
		// refuserait ensuite.
		ok, err := r.checkResourceBudget(ctx, &vc)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ok {
			// Refus explicite, pas une erreur de réconciliation : rien ne sera
			// réessayé tant que le spec ou le plafond n'a pas changé, et la
			// condition BudgetOK dit pourquoi.
			if err := r.Status().Update(ctx, &vc); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}

		if err := r.reconcileProvisioning(ctx, &vc); err != nil {
			return ctrl.Result{}, err
		}
		d, err := r.reconcileObservedState(ctx, &vc)
		if err != nil {
			return ctrl.Result{}, err
		}
		requeue = d
	}

	// Status only, always: the spec belongs to whoever commits in Git.
	if err := r.Status().Update(ctx, &vc); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// reconcileSuspend applies — or undoes — the reversible sleep.
//
// The state is derived from the phase rather than from a flag of its own: a
// vcluster is asleep iff its phase says so. One less thing to keep in sync, and
// a restart reads it back from the object like everything else.
func (r *VClusterReconciler) reconcileSuspend(ctx context.Context, vc *v1alpha1.VCluster) error {
	asleep := vc.Status.Phase == v1alpha1.VClusterPhaseSuspended

	switch {
	case vc.Spec.Suspend && !asleep:
		if err := r.Ops.SuspendVCluster(ctx, SystemActor, vc.Name, r.Cell); err != nil {
			setVClusterCond(vc, v1alpha1.CondVClusterReady, metav1.ConditionFalse, "SuspendFailed", err.Error())
			return err
		}
		endsAt := metav1.NewTime(time.Now().Add(r.gracePeriod()))
		if vc.Status.Deletion == nil {
			vc.Status.Deletion = &v1alpha1.DeletionStatus{}
		}
		vc.Status.Deletion.GracePeriodEndsAt = &endsAt
		vc.Status.Deletion.Message = "vcluster en sommeil, rien n'a été détruit — un revert du commit qui a posé suspend le remet debout"
		vc.Status.Phase = v1alpha1.VClusterPhaseSuspended
		setVClusterCond(vc, v1alpha1.CondVClusterReady, metav1.ConditionFalse, "Suspended",
			"Flux suspendu et charges à zéro réplique ; fenêtre d'annulation jusqu'à "+endsAt.Format(time.RFC3339))

	case !vc.Spec.Suspend && asleep:
		if err := r.Ops.ResumeVCluster(ctx, SystemActor, vc.Name, r.Cell); err != nil {
			setVClusterCond(vc, v1alpha1.CondVClusterReady, metav1.ConditionFalse, "ResumeFailed", err.Error())
			return err
		}
		// La fenêtre n'a plus d'objet : la laisser ferait croire à une suppression
		// encore en cours.
		vc.Status.Deletion = nil
		vc.Status.Phase = v1alpha1.VClusterPhasePending
		setVClusterCond(vc, v1alpha1.CondVClusterReady, metav1.ConditionFalse, "Resuming",
			"Flux repris ; il remonte les répliques")
	}
	return nil
}

// --- Points d'intégration ------------------------------------------------
//
// Trois chantiers distincts s'accrochent ici, chacun dans son propre fichier
// pour qu'ils puissent avancer sans se marcher dessus. Les signatures sont
// figées ; les implémentations remplacent ces stubs.

// reconcileProvisioning est implémenté dans vcluster_provision.go.

// reconcileObservedState remplit le status observé (versions, pods, quotas,
// Rancher, Vault, dernier backup) et agrège la phase + la condition Ready
// (crd-vcluster.md §2.4, §3.3). Retourne le délai de re-scrutation souhaité.
// Implémentation attendue dans vcluster_status.go.
func (r *VClusterReconciler) reconcileObservedState(ctx context.Context, vc *v1alpha1.VCluster) (time.Duration, error) {
	_, _ = ctx, vc
	return 0, nil
}

// reconcileDeletion porte le finalizer et la séquence de suppression, garde-fou
// deletionProtection compris (crd-vcluster.md §4.3, §4.4).
// Implémentation attendue dans vcluster_finalizer.go.
func (r *VClusterReconciler) reconcileDeletion(ctx context.Context, vc *v1alpha1.VCluster) (ctrl.Result, error) {
	_, _ = ctx, vc
	return ctrl.Result{}, nil
}

func setVClusterCond(vc *v1alpha1.VCluster, condType string, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&vc.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: vc.Generation,
	})
}

// SetupWithManager wires the reconciler.
func (r *VClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.VCluster{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 4}).
		Named("vcluster").
		Complete(r)
}
