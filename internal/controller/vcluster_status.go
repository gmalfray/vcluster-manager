package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gmalfray/vcluster-manager/api/v1alpha1"
	"github.com/gmalfray/vcluster-manager/internal/service"
)

// How often the observed status is refreshed.
//
// Two rates, because the cost and the value of a read are not the same in both
// situations:
//
//   - Moving: something is changing, or we are blind. 30s is short enough that a
//     vcluster which becomes ready is seen ready before Flux gives up on its
//     health check, and short enough to feel live on a dashboard during a
//     creation. Much shorter would mean one port-forward per vcluster every few
//     seconds to learn nothing.
//   - Settled: everything is known and healthy. On a steady vcluster the only
//     field that moves is quota usage, and it moves slowly. Six vclusters at one
//     read every 5 minutes is about one port-forward a minute on the host
//     cluster. The UI keeps its own HTMX polling for second-by-second numbers;
//     the CR status is not that view and should not pay its price.
const (
	ObserveIntervalMoving  = 30 * time.Second
	ObserveIntervalSettled = 5 * time.Minute
)

// VClusterObserver is the read half of the service seam. It is kept apart from
// VClusterOps because the reconciler struct is shared with other work in
// progress and cannot grow a field for it: reconcileObservedState type-asserts
// r.Ops instead. The assertion below is what stops that from rotting silently.
type VClusterObserver interface {
	ObserveVCluster(ctx context.Context, name, env string) service.VClusterObservation
}

var _ VClusterObserver = (*service.Service)(nil)

// Condition vocabulary, applied strictly everywhere below:
//
//	True     we read it, and it is fine
//	False    we read it, and it is not ready (not yet, or broken)
//	Unknown  we did not manage to read it
//
// The third one is the whole point. "Unknown" and "False" are two different
// facts, and a status that blurs them tells the operator that a vcluster is
// unpaired when Rancher merely failed to answer.

// reconcileObservedState fills in the observed half of the status — versions,
// quota usage, Rancher, protection, last backup — then aggregates the phase and
// the Ready condition (crd-vcluster.md §2.4, §3.3).
//
// It never returns an error. A source that does not answer is a finding about
// the status, not a failure of the reconcile: returning an error here would
// retry the whole lifecycle because Rancher is down, and would drop the very
// status update that says so. The retry is the requeue delay it returns.
func (r *VClusterReconciler) reconcileObservedState(ctx context.Context, vc *v1alpha1.VCluster) (time.Duration, error) {
	observer, ok := r.Ops.(VClusterObserver)
	if !ok {
		setVClusterCond(vc, v1alpha1.CondResourcesProvisioned, metav1.ConditionUnknown, "NoObserver",
			"cet opérateur n'a pas de quoi lire l'état du cluster : le status affiché date du passage précédent")
		aggregateVClusterStatus(vc)
		return ObserveIntervalMoving, nil
	}

	obs := observer.ObserveVCluster(ctx, vc.Name, r.Cell)
	applyObservation(vc, obs)
	aggregateVClusterStatus(vc)
	return observeInterval(vc, obs), nil
}

// applyObservation copies what was read onto the status, and — the part that
// matters — leaves alone what was not.
//
// The rule throughout: a failed read makes the status stale, it does not empty
// it. Watching chartVersion vanish would read as an uninstall; watching
// rancher.paired flip to false would read as an unpairing. Neither happened.
func applyObservation(vc *v1alpha1.VCluster, obs service.VClusterObservation) {
	st, reason, msg := provisionedFrom(obs)
	setVClusterCond(vc, v1alpha1.CondResourcesProvisioned, st, reason, msg)
	st, reason, msg = rancherPairedFrom(obs)
	setVClusterCond(vc, v1alpha1.CondRancherPaired, st, reason, msg)
	st, reason, msg = deletionProtectedFrom(vc, obs)
	setVClusterCond(vc, v1alpha1.CondDeletionProtected, st, reason, msg)

	// Nothing was read at all: the conditions above already say so, and there is
	// no value worth overwriting with a blank.
	if obs.Err != nil {
		return
	}

	if obs.ChartVersion != "" {
		vc.Status.ChartVersion = obs.ChartVersion
	}
	if obs.K8sVersion != "" {
		vc.Status.K8sVersion = obs.K8sVersion
	}
	if obs.CPUUsage != "" || obs.MemoryUsage != "" || obs.StorageUsage != "" {
		vc.Status.ResourceUsage = &v1alpha1.ResourceUsage{
			CPU:     obs.CPUUsage,
			Memory:  obs.MemoryUsage,
			Storage: obs.StorageUsage,
		}
	}

	// status.podCount is deliberately never written. No reader populates
	// models.StatusInfo.PodCount today, and writing 0 would claim "this vcluster
	// runs no pods" — an assertion, where what we have is an absence.
	//
	// status.vault is left alone for the same kind of reason: the Vault setup
	// state still lives in the handlers' in-memory map, and the reconcile loop
	// that takes it over owns that field. Re-stamping "waiting" on every pass
	// would be inventing a state.

	// GetNamespaceProtection returns false both for "no annotation" and for "the
	// namespace could not be read". Copying it after a read that timed out would
	// quietly drop the protection flag, so we only take it when the cluster read
	// actually completed. The real fix is upstream — that function should return
	// (bool, error) — but it is shared code and not this change's business.
	if obs.ProtectionKnown && !obs.ClusterTimedOut {
		vc.Status.ProtectionEnabled = obs.Protected
	}

	if vc.Status.Rancher == nil {
		vc.Status.Rancher = &v1alpha1.RancherStatus{}
	}
	vc.Status.Rancher.State = obs.RancherState
	if obs.RancherKnown {
		vc.Status.Rancher.Paired = obs.RancherPaired
	}
	// When state is Unknown, paired keeps its last known value instead of
	// falling back to false. False reads as "unpaired", and offering to pair a
	// cluster that is already paired is exactly what GetRancherStatus's Unknown
	// state exists to prevent.

	if obs.BackupsKnown {
		// The list came back. An empty one is a fact — no backup — so clearing
		// the field is right here, unlike everywhere else in this function.
		vc.Status.LastBackup = nil
		if obs.LastBackup != nil {
			vc.Status.LastBackup = &v1alpha1.LastBackup{
				Name:        obs.LastBackup.Name,
				Phase:       obs.LastBackup.Phase,
				CompletedAt: parseBackupTime(obs.LastBackup.CompletionTime),
			}
		}
	}
}

// provisionedFrom reports the observed health of the resources the CR expands
// into: the vcluster HelmRelease and the tenant Kustomization.
//
// This condition is also written by the provisioning step, which sets it when it
// applies the graph. Running after it is intentional: the last word goes to what
// the cluster says, not to what we asked for.
func provisionedFrom(obs service.VClusterObservation) (metav1.ConditionStatus, string, string) {
	switch {
	case obs.Err != nil:
		return metav1.ConditionUnknown, "NoClusterClient",
			"aucun client Kubernetes pour cette cell : " + obs.Err.Error()
	case obs.ClusterTimedOut:
		return metav1.ConditionUnknown, "ReadTimedOut",
			fmt.Sprintf("la lecture du cluster n'a pas abouti en %s ; le status affiché date du passage précédent", service.ObserveBudgetCluster)
	}

	hr, ks := obs.HelmRelease, obs.Kustomization
	switch {
	case hr == "Ready" && ks == "Ready":
		return metav1.ConditionTrue, "Healthy", "HelmRelease et Kustomization tenant prêts"

	// An explicit failure beats an unknown: better to show the problem we know
	// than to report that we know nothing.
	case explicitlyNotReady(hr) || explicitlyNotReady(ks):
		return metav1.ConditionFalse, "NotReady",
			fmt.Sprintf("HelmRelease=%s, Kustomization=%s", orUnknown(hr), orUnknown(ks))

	default:
		return metav1.ConditionUnknown, "NotReadable",
			fmt.Sprintf("HelmRelease=%s, Kustomization=%s : pas encore créés ou illisibles — rien qui permette de conclure", orUnknown(hr), orUnknown(ks))
	}
}

// explicitlyNotReady is true only when the object answered something other than
// Ready. GetVClusterStatus reports the Flux condition's reason in that case
// ("Progressing", "UpgradeFailed"...), so anything that is neither "Ready" nor
// "Unknown" nor empty is a real, read answer.
func explicitlyNotReady(state string) bool {
	return state != "" && state != "Ready" && state != "Unknown"
}

func orUnknown(state string) string {
	if state == "" {
		return "Unknown"
	}
	return state
}

// rancherPairedFrom maps the pairing state onto a condition. It is informative,
// never blocking: a vcluster that runs perfectly but is absent from the Rancher
// console is not broken, and a Rancher outage must not turn every Kustomization
// red in `flux get kustomizations`.
func rancherPairedFrom(obs service.VClusterObservation) (metav1.ConditionStatus, string, string) {
	if obs.Err != nil {
		return metav1.ConditionUnknown, "NotObserved", "état Rancher non lu : " + obs.Err.Error()
	}
	switch obs.RancherState {
	case service.RancherStateUnknown:
		return metav1.ConditionUnknown, "LookupFailed",
			"Rancher n'a pas répondu : ni appairé ni dépairé, on ne sait pas"
	case service.RancherStateOff:
		return metav1.ConditionFalse, "Disabled", "Rancher n'est pas activé pour cette cell"
	case service.RancherStatePaired:
		return metav1.ConditionTrue, "Paired", "cluster actif dans Rancher"
	case service.RancherStateManuallyPaired:
		return metav1.ConditionTrue, "ManuallyPaired",
			"agents Rancher actifs dans le vcluster, sous un nom de cluster qui n'est pas le nôtre"
	case service.RancherStatePairing:
		return metav1.ConditionFalse, "Pairing", "import en cours, l'agent n'a pas encore rappelé"
	case service.RancherStateCleaning:
		return metav1.ConditionFalse, "Cleaning", "job rancher-cleanup en cours"
	default:
		return metav1.ConditionFalse, "NotPaired", "cluster absent de Rancher"
	}
}

// deletionProtectedFrom mirrors spec.deletionProtection so the intent is visible
// without reading the spec, and flags the case where the host namespace does not
// agree with it — the finalizer checks the spec at the last moment
// (crd-vcluster.md §4.3), so a divergence is worth seeing before then.
func deletionProtectedFrom(vc *v1alpha1.VCluster, obs service.VClusterObservation) (metav1.ConditionStatus, string, string) {
	want := vc.Spec.DeletionProtection
	status := metav1.ConditionFalse
	if want {
		status = metav1.ConditionTrue
	}

	usable := obs.Err == nil && obs.ProtectionKnown && !obs.ClusterTimedOut
	switch {
	case !usable:
		return status, "SpecOnly",
			fmt.Sprintf("spec.deletionProtection=%t ; l'annotation du namespace n'a pas pu être lue", want)
	case obs.Protected != want:
		return status, "NamespaceDiverges",
			fmt.Sprintf("spec.deletionProtection=%t mais l'annotation protect-deletion du namespace vaut %t", want, obs.Protected)
	default:
		return status, "InSync", fmt.Sprintf("spec.deletionProtection=%t, annotation du namespace alignée", want)
	}
}

// blockingConditions are the ones Ready aggregates, in the order their message
// deserves to win. Budget first: when it is refused nothing is provisioned, so
// every condition below it would report a consequence rather than the cause.
//
// Deliberately absent: RancherPaired, DeletionProtected and BackupCompleted.
// None of them says whether the vcluster works — they describe an external
// integration, an intent, and a step of the deletion sequence.
func blockingConditions(vc *v1alpha1.VCluster) []string {
	types := []string{
		v1alpha1.CondBudgetOK,
		v1alpha1.CondResourcesProvisioned,
		v1alpha1.CondVaultConfigured,
	}
	// A stale ArgoCDReady left over from before ArgoCD was turned off must not
	// keep blocking Ready.
	if vc.Spec.ArgoCD != nil && vc.Spec.ArgoCD.Enabled {
		types = append(types, v1alpha1.CondArgoCDReady)
	}
	return types
}

// aggregateVClusterStatus computes Ready, then the phase.
//
// Ready is what Flux uses as a Kustomization health check (crd-vcluster.md
// §3.3), which is what keeps a failure visible in `flux get kustomizations`
// without an admission webhook. So the rule is stated plainly rather than
// inferred:
//
//   - one blocking condition False  → Ready False. A known failure wins over an
//     unknown, whatever the order the conditions come in.
//   - otherwise one of them Unknown → Ready Unknown. We are not claiming a
//     failure we did not observe.
//   - otherwise, all True           → Ready True.
//
// ResourcesProvisioned is the floor: without it there is no evidence the
// vcluster's own resources even exist. The other blocking conditions count only
// once the step that owns them has written one. An absent condition means "that
// step has not run yet" — holding Ready hostage to a step nobody has wired would
// report every vcluster as broken for a reason that has nothing to do with it.
func aggregateVClusterStatus(vc *v1alpha1.VCluster) {
	if apimeta.FindStatusCondition(vc.Status.Conditions, v1alpha1.CondResourcesProvisioned) == nil {
		setVClusterCond(vc, v1alpha1.CondVClusterReady, metav1.ConditionUnknown, "NotObservedYet",
			"l'état des ressources du vcluster n'a pas encore été lu")
		vc.Status.Phase = phaseFor(vc, metav1.ConditionUnknown)
		return
	}

	var (
		unknown *metav1.Condition
		present []string
	)

	for _, t := range blockingConditions(vc) {
		c := apimeta.FindStatusCondition(vc.Status.Conditions, t)
		if c == nil {
			continue
		}
		present = append(present, t)
		switch c.Status {
		case metav1.ConditionFalse:
			setVClusterCond(vc, v1alpha1.CondVClusterReady, metav1.ConditionFalse,
				c.Type+"NotMet", c.Type+" : "+c.Message)
			vc.Status.Phase = phaseFor(vc, metav1.ConditionFalse)
			return
		case metav1.ConditionUnknown:
			if unknown == nil {
				unknown = c
			}
		}
	}

	if unknown != nil {
		setVClusterCond(vc, v1alpha1.CondVClusterReady, metav1.ConditionUnknown,
			unknown.Type+"Unknown", unknown.Type+" : "+unknown.Message)
		vc.Status.Phase = phaseFor(vc, metav1.ConditionUnknown)
		return
	}

	setVClusterCond(vc, v1alpha1.CondVClusterReady, metav1.ConditionTrue, "AllChecksPassed",
		"conditions bloquantes satisfaites : "+strings.Join(present, ", "))
	vc.Status.Phase = v1alpha1.VClusterPhaseReady
}

// phaseFor turns a non-True Ready into the one-word summary.
func phaseFor(vc *v1alpha1.VCluster, ready metav1.ConditionStatus) v1alpha1.VClusterPhase {
	if ready == metav1.ConditionTrue {
		return v1alpha1.VClusterPhaseReady
	}

	// The budget is the one definitive refusal: nothing is provisioned and
	// nothing will be until the cap moves or another vcluster goes away
	// (crd-vcluster.md §4.1 step 2).
	if c := apimeta.FindStatusCondition(vc.Status.Conditions, v1alpha1.CondBudgetOK); c != nil && c.Status == metav1.ConditionFalse {
		return v1alpha1.VClusterPhaseFailed
	}

	// "Not ready" does not mean the same thing before and after the first time
	// we saw it running. A first install is provisioning; the same reading on a
	// vcluster that has already been up is a regression. chartVersion is the
	// marker, because it is only ever written when a chart actually deployed and
	// never cleared.
	if vc.Status.ChartVersion == "" {
		return v1alpha1.VClusterPhaseProvisioning
	}
	return v1alpha1.VClusterPhaseDegraded
}

// observeInterval picks the re-read delay. Settled means Ready is True and
// nothing is mid-flight; anything else keeps the short rate, including "we could
// not read", because being blind is precisely when waiting five minutes to look
// again is wrong.
func observeInterval(vc *v1alpha1.VCluster, obs service.VClusterObservation) time.Duration {
	if vc.Status.Phase != v1alpha1.VClusterPhaseReady {
		return ObserveIntervalMoving
	}
	// Pairing and cleaning settle on their own in a minute or two; following
	// them at the slow rate would leave a spinner up long after it is over.
	switch obs.RancherState {
	case service.RancherStatePairing, service.RancherStateCleaning:
		return ObserveIntervalMoving
	}
	if obs.LastBackup != nil && !isSettledBackupPhase(obs.LastBackup.Phase) {
		return ObserveIntervalMoving
	}
	return ObserveIntervalSettled
}

func isSettledBackupPhase(phase string) bool {
	switch phase {
	case "InProgress", "New", "Uploading", "WaitingForPluginOperations", "Finalizing", "":
		return false
	default:
		return true
	}
}

// parseBackupTime turns Velero's RFC3339 timestamp into a metav1.Time. An
// unparseable or missing value yields nil rather than the zero time, which would
// display as 1 January year 1.
func parseBackupTime(s string) *metav1.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	mt := metav1.NewTime(t)
	return &mt
}
