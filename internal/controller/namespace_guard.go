package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gmalfray/vcluster-manager/api/v1alpha1"
	"github.com/gmalfray/vcluster-manager/internal/service"
)

// DefaultVClustersNamespace is where the cell's Flux applies its VCluster CRs.
//
// One namespace for all of them, and not `vcluster-<name>` like the markers:
// the CR is what *declares* a vcluster, so it has to exist before the vcluster's
// own namespace does. Making it live in the namespace it is about would be
// circular. Bounding it to a single namespace instead means "only the cell's
// GitOps repo may declare a vcluster", which is exactly the ADR-001 rule
// expressed as an RBAC boundary.
const DefaultVClustersNamespace = service.OperatorNamespace

// vclusterNamespace is the host namespace of a vcluster. The single place this
// concatenation is allowed to happen outside the service.
func vclusterNamespace(name string) string { return "vcluster-" + name }

// Both reconcilers derive everything they act upon — namespaces, workloads,
// volumes — from `metadata.name` alone. That is fine for a name that can only
// be written by someone already entitled to it, and a hole otherwise: a marker
// named after somebody else's vcluster, dropped in a namespace of one's own,
// would drive a destructive restore on the victim. Worse, a marker named
// `manager` resolves to `vcluster-manager`, the app's own namespace, and a
// backup of it ships the GitLab token, the Keycloak client secret and
// JWT_SECRET off to the S3 bucket.
//
// The fix is not to authorize the operator differently — it acts as SystemActor
// by design. It is to make the object *self-bounding*: an object may only speak
// about the vcluster whose namespace it already sits in. Naming the victim then
// requires write access to the victim's namespace, which is the authorization
// check that was missing, expressed where the API server can enforce it.
//
// Returns the reason to refuse, or "" when the object is legitimately placed.
func markerMisplaced(ops *v1alpha1.VClusterVeleroOps) string {
	if !service.ValidName(ops.Name) {
		return "nom de vcluster refusé : " + ops.Name + " — soit il n'a pas la forme attendue, " +
			"soit son namespace dérivé retombe sur celui de l'opérateur, ce qui ferait de ce " +
			"marqueur un ordre sur l'app elle-même"
	}
	want := vclusterNamespace(ops.Name)
	if ops.Namespace == want {
		return ""
	}
	return "un marqueur nommé " + ops.Name + " ne peut vivre que dans le namespace " + want +
		", pas dans " + ops.Namespace + " : sans cela, un marqueur déposé dans n'importe quel " +
		"namespace piloterait le vcluster homonyme, sauvegarde et restauration destructrice comprises"
}

// vclusterMisplaced is the same rule for the VCluster CR, with the flat
// namespace explained above.
func vclusterMisplaced(vc *v1alpha1.VCluster, want string) string {
	// Le nom d'abord : un nom réservé est accepté par la règle de namespace
	// puisque `vcluster-manager` est justement le namespace attendu. Sans ce
	// contrôle, les deux règles coïncident sur le nom `manager`.
	if !service.ValidName(vc.Name) {
		return "nom de vcluster refusé : " + vc.Name + " — soit il n'a pas la forme attendue, " +
			"soit son namespace dérivé retombe sur celui de l'opérateur"
	}
	if vc.Namespace == want {
		return ""
	}
	return "un VCluster ne peut être déclaré que dans le namespace " + want + ", pas dans " +
		vc.Namespace + " : sinon un CR déposé n'importe où pilote le vcluster homonyme " +
		"(spec.suspend l'endort, la suppression du CR déclenche son finalizer)"
}

// refuse records why an object was ignored. It is written to the status rather
// than only logged: an operator that silently drops objects is indistinguishable
// from one that is broken, and the legitimate case for this rule is a
// misplaced CR, not an attack.
func refuseMarker(ops *v1alpha1.VClusterVeleroOps, reason string) {
	setCond(ops, v1alpha1.CondAccepted, metav1.ConditionFalse, "NamespaceMismatch", reason)
}
