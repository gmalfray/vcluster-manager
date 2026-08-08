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

// VClusterObserver is the read half of the service seam, kept as its own small
// interface for readability. r.Ops (vcluster_controller.go) is typed as
// VClusterServiceOps, which embeds this one along with the other five — so
// reconcileObservedState below reads straight off r.Ops, no type assertion.
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
	obs := r.Ops.ObserveVCluster(ctx, vc.Name, r.Cell)
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

	// podCount only moves when the pod list actually came back. A failed list
	// and "the host namespace runs zero pods right now" both leave PodCount at
	// its zero value — PodCountKnown is the only thing telling them apart, same
	// discipline as everywhere else in this function.
	if obs.PodCountKnown {
		vc.Status.PodCount = int32(obs.PodCount)
	}

	refineArgoCDReady(vc, obs)

	// status.vault is left alone for the same kind of reason: the Vault setup
	// state still lives in the handlers' in-memory map, and the reconcile loop
	// that takes it over owns that field. Re-stamping "waiting" on every pass
	// would be inventing a state.

	// ProtectionKnown now genuinely means "the read succeeded, whether or not
	// the annotation was there". GetNamespaceProtection
	// (internal/kubernetes/protection.go) used to fold "no annotation" and
	// "namespace unreadable" into the same false, and ProtectionKnown inherited
	// that ambiguity through it. That's fixed upstream now: a failed read comes
	// back as an error, not a false.
	//
	// !obs.ClusterTimedOut stays regardless: a request whose context is dying
	// can cut every source's read short at once, and this field would rather
	// stay stale than take a value from a pass that may not have actually
	// finished checking.
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

// refineArgoCDReady adds what the cluster shows about the ArgoCD Kustomization
// (argocd-<name>, the tenant graph the generator commits) on top of what
// reconcileKeycloak (vcluster_integrations.go) already wrote this same pass.
// Same ordering choice as provisionedFrom: the last word goes to what was
// actually observed, not to what the integrations step believed it achieved.
//
// Only touches a condition that is currently True. Anything reconcileKeycloak
// already marked False or Unknown (ArgoCD disabled, no Keycloak client, a
// Keycloak failure) already names a cause more specific than anything this
// function could add — piling a Kustomization detail on top of it would trade
// a named reason for a vaguer one.
//
// Runs only when obs.Err == nil (the caller places it after that guard,
// applyObservation): on an unreachable cluster obs.ArgoCDKustomization stays
// at its zero value, and treating that as "Kustomization unreadable" would
// flip a healthy ArgoCD to Unknown for a reason that has nothing to do with
// ArgoCD — the same trap chartVersion and rancher.paired avoid a few lines
// below by simply not being touched on a failed read.
//
// The GitLab volet stays out of this on purpose: the operator has no GitLab
// client to check it with (main.go leaves Deps.GitLab nil for this binary —
// crd-vcluster.md §3.2), and even the existing client
// (internal/gitops/gitlab.go, AppManifestsRepoExists) folds "the repo is
// missing" and "the GitLab API failed" into the same false. Wiring it here
// today could turn a GitLab hiccup into a False that reads as "ArgoCD is
// broken" — worse than the reserve staying in the message.
func refineArgoCDReady(vc *v1alpha1.VCluster, obs service.VClusterObservation) {
	current := apimeta.FindStatusCondition(vc.Status.Conditions, v1alpha1.CondArgoCDReady)
	if current == nil || current.Status != metav1.ConditionTrue {
		return
	}

	switch {
	case obs.ArgoCDKustomization == "Ready":
		setVClusterCond(vc, v1alpha1.CondArgoCDReady, metav1.ConditionTrue, "KeycloakAndKustomizationReady",
			"client OIDC ArgoCD prêt dans Keycloak, Kustomization argocd-"+vc.Name+" saine — "+
				"seul le dépôt GitLab app-manifests reste non vérifié : aucun client GitLab sur cet opérateur")
	case explicitlyNotReady(obs.ArgoCDKustomization):
		setVClusterCond(vc, v1alpha1.CondArgoCDReady, metav1.ConditionFalse, "ArgoCDKustomizationNotReady",
			"client OIDC Keycloak prêt, mais la Kustomization argocd-"+vc.Name+" ne l'est pas : "+obs.ArgoCDKustomization)
	default:
		setVClusterCond(vc, v1alpha1.CondArgoCDReady, metav1.ConditionUnknown, "ArgoCDKustomizationNotReadable",
			"client OIDC Keycloak prêt, mais la Kustomization argocd-"+vc.Name+" est illisible ou pas encore créée")
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
		v1alpha1.CondResourcesProvisioned,
		v1alpha1.CondVaultConfigured,
	}
	// BudgetOK ne bloque que tant que le vcluster n'a jamais tourné.
	//
	// Sur un vcluster déjà en marche, un dépassement de plafond est un fait
	// d'exploitation — plafond baissé, voisin qui a grossi — et non une panne :
	// faire virer Ready au rouge rendrait le health check de la Kustomization Flux
	// rouge pour un vcluster qui va parfaitement bien. La condition BudgetOK reste
	// visible et dit ce qui se passe ; elle ne prétend simplement plus que le
	// vcluster est cassé.
	if vc.Status.ChartVersion == "" {
		types = append([]string{v1alpha1.CondBudgetOK}, types...)
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
//
// Ce plancher passe APRÈS l'échec constaté, et pas avant. Un refus de budget
// coupe la réconciliation avant l'observation, donc ResourcesProvisioned est
// justement absente au moment où le refus doit se voir : appliquer le plancher
// d'abord rendrait Ready=Unknown/NotObservedYet et enfermerait le refus dans
// BudgetOK, où le health check de Flux ne le lit pas. « Pas encore observé »
// ne peut pas l'emporter sur « on sait déjà que non ».
func aggregateVClusterStatus(vc *v1alpha1.VCluster) {
	if c := firstFalseBlocking(vc); c != nil {
		setVClusterCond(vc, v1alpha1.CondVClusterReady, metav1.ConditionFalse,
			c.Type+"NotMet", c.Type+" : "+c.Message)
		vc.Status.Phase = phaseFor(vc, metav1.ConditionFalse)
		return
	}

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
		// Le cas False est déjà traité en tête : ici il ne reste que True et
		// Unknown.
		if c.Status == metav1.ConditionUnknown && unknown == nil {
			unknown = c
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

// firstFalseBlocking rend la première condition bloquante explicitement à
// False, dans l'ordre de priorité des messages. Un échec constaté l'emporte sur
// tout le reste, quel que soit l'ordre d'arrivée des conditions.
func firstFalseBlocking(vc *v1alpha1.VCluster) *metav1.Condition {
	for _, t := range blockingConditions(vc) {
		if c := apimeta.FindStatusCondition(vc.Status.Conditions, t); c != nil && c.Status == metav1.ConditionFalse {
			return c
		}
	}
	return nil
}

// phaseFor turns a non-True Ready into the one-word summary.
func phaseFor(vc *v1alpha1.VCluster, ready metav1.ConditionStatus) v1alpha1.VClusterPhase {
	if ready == metav1.ConditionTrue {
		return v1alpha1.VClusterPhaseReady
	}

	// The budget is the one definitive refusal: nothing is provisioned and
	// nothing will be until the cap moves or another vcluster goes away
	// (crd-vcluster.md §4.1 step 2).
	//
	// Tant que rien n'a jamais tourné, seulement : un vcluster debout que le
	// plafond vient de refuser n'est pas en échec, il est trop gros pour la cell.
	// Le déclarer Failed effacerait de la vue qu'il fonctionne.
	if c := apimeta.FindStatusCondition(vc.Status.Conditions, v1alpha1.CondBudgetOK); c != nil &&
		c.Status == metav1.ConditionFalse && vc.Status.ChartVersion == "" {
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
