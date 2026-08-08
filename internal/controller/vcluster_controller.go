package controller

import (
	"context"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

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

// VClusterServiceOps est tout ce que le reconciler tire du service : les six
// seams (VClusterOps, VClusterObserver, VClusterProvisioner, VClusterDeletionOps,
// QuotaResolver, VClusterIntegrationOps — déclarés chacun dans le fichier de
// l'étape qui les consomme) fusionnés en un seul type.
//
// Avant, chaque étape faisait sa propre assertion `r.Ops.(X)` sur un champ
// `Ops VClusterOps` trop étroit, et traitait le cas `!ok` — impossible en
// production, où *service.Service implémente tout — comme un cas normal à
// dégrader proprement. En test, ça a fait l'inverse : un faux qui n'implémentait
// qu'une partie du seam faisait quand même passer `Ops: faux` à la compilation,
// l'assertion échouait en silence à l'exécution, et la campagne mesurait le
// comportement d'un opérateur amputé sans le savoir — ça a mordu une fois à la
// fusion de plusieurs chantiers.
//
// Un champ de CE type ferme la porte : un faux qui n'implémente pas les six
// interfaces ne compile plus du tout, il ne dégrade plus. Le coût est réel — les
// faux de test qui ne couvraient qu'un seam doivent maintenant compiler contre
// les six, quitte à ne réellement exercer qu'un seul d'entre eux — mais c'est le
// prix pour que l'incomplétude soit une erreur de compilation et non un
// comportement à débusquer en observant un test vert qui mesure autre chose que
// ce qu'il prétend.
type VClusterServiceOps interface {
	VClusterOps
	VClusterObserver
	VClusterProvisioner
	VClusterDeletionOps
	QuotaResolver
	VClusterIntegrationOps
}

var _ VClusterServiceOps = (*service.Service)(nil)

// VClusterReconciler owns the lifecycle of a VCluster CR.
//
// Today it implements only the reversible half of deletion: reacting to
// spec.suspend. The full provisioning reconcile (expanding the CR into Flux
// resources) and the finalizer come next — deliberately in that order, because
// suspend is what the finalizer's grace period hangs off, and it destroys
// nothing.
type VClusterReconciler struct {
	client.Client

	// Ops is the service seam — *service.Service in production. Typed as the
	// union of every seam a reconcile step touches (VClusterServiceOps), pas
	// juste VClusterOps : voir le commentaire de ce type pour pourquoi.
	Ops VClusterServiceOps

	// Cell names the host cluster this operator reconciles (ADR-002).
	Cell string

	// Budget est le plafond de ressources de la cell. Vide = aucun plafond
	// configuré, ce qui fait REFUSER les créations avec quotas (crd-vcluster.md
	// §5.3) plutôt que les laisser passer sans contrôle.
	Budget BudgetLimits

	// BudgetOps lit ce que la cell a déjà alloué. Nil désactive la vérification
	// — réservé aux tests qui ne portent pas sur le budget.
	BudgetOps BudgetReader

	// Namespace est le seul namespace d'où des VCluster sont acceptés. Vide =
	// DefaultVClustersNamespace. Voir namespace_guard.go pour le pourquoi.
	Namespace string

	// GracePeriod overrides DefaultGracePeriod. Zero means the default.
	GracePeriod time.Duration

	// Recorder émet les Events Kubernetes qui survivent à la disparition du CR
	// qu'ils décrivent — utile pour un seul cas ici : la conclusion de la
	// séquence de suppression, écrite dans le status d'un objet dont le
	// finalizer part deux appels plus loin. Nil dans la plupart des tests, qui
	// ne portent pas là-dessus ; recordEvent le tolère.
	Recorder record.EventRecorder
}

// recordEvent émet un Event Kubernetes attaché à vc, si un recorder est câblé.
//
// Le nil-check n'est pas là pour la production — cmd/operator/main.go câble
// toujours un recorder — mais pour les tests qui construisent un reconciler à
// la main sans lui : ils sont bien plus nombreux que ceux qui portent sur les
// events, et un appel sur une interface nil paniquerait au lieu de rester
// silencieux.
func (r *VClusterReconciler) recordEvent(vc *v1alpha1.VCluster, eventType, reason, message string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Event(vc, eventType, reason, message)
}

func (r *VClusterReconciler) vclustersNamespace() string {
	if r.Namespace != "" {
		return r.Namespace
	}
	return DefaultVClustersNamespace
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

	// Avant tout le reste, y compris la suppression : un CR mal placé ne doit
	// ni piloter le vcluster homonyme, ni se voir poser un finalizer.
	if reason := vclusterMisplaced(&vc, r.vclustersNamespace()); reason != "" {
		// Un objet refusé qui s'en va et qui porte quand même notre finalizer doit
		// pouvoir partir. Sinon le refus le coince en Terminating pour toujours et
		// il faut retirer le finalizer à la main — un garde-fou ne doit pas créer
		// l'objet inamovible qu'il est censé éviter. Le cas existe pour de vrai :
		// un CR provisionné avant que cette garde n'existe.
		if !vc.DeletionTimestamp.IsZero() && controllerutil.ContainsFinalizer(&vc, VClusterFinalizer) {
			controllerutil.RemoveFinalizer(&vc, VClusterFinalizer)
			return ctrl.Result{}, r.Update(ctx, &vc)
		}
		setVClusterCond(&vc, v1alpha1.CondAccepted, metav1.ConditionFalse, "NamespaceMismatch", reason)
		return ctrl.Result{}, r.Status().Update(ctx, &vc)
	}

	// Un deletionTimestamp veut dire que le CR s'en va : ce chemin appartient au
	// finalizer, pas à la réconciliation normale.
	if !vc.DeletionTimestamp.IsZero() {
		return r.reconcileDeletion(ctx, &vc)
	}

	// Le finalizer se pose ici, sur le chemin vivant : l'API server refuse d'en
	// ajouter un à un objet qui porte déjà un deletionTimestamp.
	if err := r.ensureFinalizer(ctx, &vc); err != nil {
		return ctrl.Result{}, err
	}

	vc.Status.ObservedGeneration = vc.Generation

	// Le status est écrit sur TOUS les chemins, y compris en erreur.
	//
	// Sinon toute condition posée avant un échec est perdue : on retourne
	// l'erreur, le status n'est jamais écrit, et un SuspendFailed n'apparaît
	// jamais dans `kubectl describe`. C'est exactement l'échec silencieux contre
	// lequel ADR-001 met en garde — l'exploitant voit un reconcile qui boucle
	// sans jamais savoir pourquoi.
	requeue, reconcileErr := r.reconcileAll(ctx, &vc)

	if err := r.Status().Update(ctx, &vc); err != nil {
		// L'erreur de réconciliation prime : c'est elle qui dit ce qui ne va
		// vraiment pas. L'échec d'écriture du status est journalisé sans masquer
		// la première.
		if reconcileErr != nil {
			log.FromContext(ctx).Error(err, "écriture du status après un échec de réconciliation")
			return ctrl.Result{}, reconcileErr
		}
		return ctrl.Result{}, err
	}
	if reconcileErr != nil {
		return ctrl.Result{}, reconcileErr
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// reconcileAll enchaîne les étapes et rend la main à la première erreur. Son
// appelant écrit le status quoi qu'il arrive, donc les conditions posées ici
// survivent à un échec.
func (r *VClusterReconciler) reconcileAll(ctx context.Context, vc *v1alpha1.VCluster) (time.Duration, error) {
	// Le sommeil d'abord : inutile de provisionner ce qu'on vient d'endormir.
	if err := r.reconcileSuspend(ctx, vc); err != nil {
		return 0, err
	}
	if vc.Status.Phase == v1alpha1.VClusterPhaseSuspended {
		return 0, nil
	}

	// Le budget avant le provisionnement : on ne matérialise rien qu'on
	// refuserait ensuite.
	withinBudget, err := r.checkResourceBudget(ctx, vc)
	if err != nil {
		return 0, err
	}
	if !withinBudget {
		// Refus explicite, pas une erreur de réconciliation. Mais il ne faut pas
		// non plus s'arrêter là, et pour deux raisons distinctes.
		//
		// 1. Un vcluster DÉJÀ EN MARCHE continue d'être observé. Le budget est un
		//    contrôle d'admission — « faut-il matérialiser ceci » — pas un
		//    interrupteur d'extinction. Un plafond baissé, ou un voisin qui
		//    grossit, refusait jusqu'ici un vcluster parfaitement sain : son
		//    chartVersion, son usage de quotas et son état Rancher se figeaient au
		//    dernier passage. Le témoin est chartVersion, écrit uniquement quand un
		//    chart a réellement déployé et jamais effacé.
		//
		// 2. Un vcluster jamais monté doit revenir frapper à la porte. On ne
		//    l'observe pas — il n'y a rien à observer, et un port-forward vers un
		//    namespace inexistant coûte son délai d'attente à chaque tour — mais on
		//    demande un requeue périodique.
		if vc.Status.ChartVersion != "" {
			return r.reconcileObservedState(ctx, vc)
		}
		// L'agrégation, sans laquelle le refus resterait enfermé dans BudgetOK :
		// Ready garderait sa valeur du passage précédent, donc le health check de
		// la Kustomization Flux verrait un vcluster sain alors que rien n'a été
		// provisionné.
		aggregateVClusterStatus(vc)
		return BudgetRetryInterval, nil
	}

	provisioned, err := r.reconcileProvisioning(ctx, vc)
	if err != nil {
		return 0, err
	}
	if !provisioned {
		// Refus, pas erreur : le spec doit changer. On agrège quand même, pour la
		// raison qui vaut pour le budget juste au-dessus — sans ça le refus reste
		// enfermé dans ResourcesProvisioned, l'observation qui suit la réécrit
		// depuis ce qu'elle voit, et un CR refusé ressort Ready=True sur le canal
		// que Flux lit.
		aggregateVClusterStatus(vc)
		return 0, nil
	}

	// Les intégrations (Vault, Keycloak, Rancher) avant l'observation : c'est
	// elle qui constate leur résultat, et constater avant d'agir ferait toujours
	// voir l'état du passage précédent.
	integrationRequeue, err := r.reconcileIntegrations(ctx, vc)
	if err != nil {
		return 0, err
	}

	observeRequeue, err := r.reconcileObservedState(ctx, vc)
	if err != nil {
		return 0, err
	}
	// Le plus court des deux : une intégration qui progresse ne doit pas attendre
	// le rythme de croisière de l'observation.
	if integrationRequeue > 0 && (observeRequeue == 0 || integrationRequeue < observeRequeue) {
		return integrationRequeue, nil
	}
	return observeRequeue, nil
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
		if vc.Status.Deletion == nil {
			vc.Status.Deletion = &v1alpha1.DeletionStatus{}
		}
		// La fenêtre ne redémarre pas à chaque passage.
		//
		// Une mise en sommeil réussie dont l'écriture du status échoue est
		// rejouée au reconcile suivant : recalculer la date la ferait glisser de
		// sept jours à chaque tentative, et une écriture qui échoue en boucle
		// donnerait une fenêtre qui n'expire jamais. Elle est posée une fois, au
		// premier endormissement, et un réveil la remet à nil — c'est là, et
		// seulement là, qu'une nouvelle fenêtre a un sens.
		if vc.Status.Deletion.GracePeriodEndsAt == nil {
			endsAt := metav1.NewTime(time.Now().Add(r.gracePeriod()))
			vc.Status.Deletion.GracePeriodEndsAt = &endsAt
		}
		endsAt := *vc.Status.Deletion.GracePeriodEndsAt
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

// reconcileObservedState est implémenté dans vcluster_status.go : il remplit le
// status observé (versions, quotas, Rancher, protection, dernier backup) et
// agrège la phase + la condition Ready (crd-vcluster.md §2.4, §3.3).

// reconcileIntegrations configure ce qui vit hors du cluster hôte et que le CR
// doit piloter : le backend d'authentification Vault, le client OIDC Keycloak,
// l'appairage Rancher.
// → implémenté dans vcluster_integrations.go.

// reconcileDeletion porte le finalizer et la séquence de suppression, garde-fou
// deletionProtection compris (crd-vcluster.md §4.3, §4.4).
// → implémenté dans vcluster_finalizer.go, avec ensureFinalizer.

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
