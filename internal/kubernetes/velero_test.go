package kubernetes

import (
	"context"
	"fmt"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// newTestStatusClient builds a StatusClient backed by a fake dynamic client
// seeded with objs, for testing the guard logic against Velero/pod state
// without a real cluster. Uses NewTestStatusClient's exported list-kind
// mapping (testListKinds) so a List against a resource with nothing seeded
// of that kind returns empty instead of panicking — see its comment.
func newTestStatusClient(objs ...runtime.Object) *StatusClient {
	scheme := runtime.NewScheme()
	return &StatusClient{client: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, testListKinds, objs...)}
}

func newBackupObj(name, namespace, phase string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "velero.io/v1",
		"kind":       "Backup",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
		},
		"status": map[string]interface{}{
			"phase": phase,
		},
	}}
}

// newPodObj builds a Pod carrying matchLabels — waitForWorkloadPodsGone finds
// pods by their owning workload's selector, not by name, so tests need pods
// labeled to match (or deliberately not match) that selector.
func newPodObj(name, namespace string, matchLabels map[string]string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
			"labels":    toInterfaceMap(matchLabels),
		},
	}}
}

// newStatefulSetObj builds a StatefulSet with a selector, needed for
// waitForWorkloadPodsGone to find its pods.
func newStatefulSetObj(name, namespace string, matchLabels map[string]string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "StatefulSet",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
		},
		"spec": map[string]interface{}{
			"replicas": int64(1),
			"selector": map[string]interface{}{
				"matchLabels": toInterfaceMap(matchLabels),
			},
		},
	}}
}

// newDeploymentObj builds a Deployment with a selector, mirroring
// newStatefulSetObj for the external-etcd topology's control-plane.
func newDeploymentObj(name, namespace string, matchLabels map[string]string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
		},
		"spec": map[string]interface{}{
			"replicas": int64(1),
			"selector": map[string]interface{}{
				"matchLabels": toInterfaceMap(matchLabels),
			},
		},
	}}
}

func newPVCObj(name, namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
		},
	}}
}

func toInterfaceMap(m map[string]string) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// --- CreateVeleroBackup ---
//
// D1 (docs/recette-restauration.md, cas A): a manual backup used to exclude
// pods, which sounded harmless ("vcluster recreates them anyway") but is not:
// Velero creates one PodVolumeBackup per POD that mounts a volume, not one per
// PVC. Without pods in the backup, defaultVolumesToFsBackup has nothing to
// act on and Velero creates zero PodVolumeBackup — the backup ends up holding
// an empty PVC object instead of the volume's data. Proven on the recette
// cell: excluding pods gave phase=Completed, 127 items, zero PodVolumeBackup;
// the same backup without that exclusion gave six PodVolumeBackup, the etcd
// one at 144 801 792 bytes.
//
// A real end-to-end check (did a PodVolumeBackup actually get created) needs
// Velero's own controller running against real pods and volumes, which is out
// of reach for a fake dynamic client — that part was verified on-cluster
// instead. What these tests pin down is the two knobs Velero's controller
// reads to decide whether to even try: defaultVolumesToFsBackup, and whether
// pods are excluded. Get either wrong and the backup goes quietly empty again.

func TestCreateVeleroBackup_DoesNotExcludePodsOrReplicaSets(t *testing.T) {
	sc := newTestStatusClient()
	backupName, err := sc.CreateVeleroBackup(context.Background(), "demo", "velero-system", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	obj, err := sc.client.Resource(veleroBackupGVR).Namespace("velero-system").Get(context.Background(), backupName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("unexpected error re-fetching backup: %v", err)
	}
	excluded, _, _ := unstructured.NestedStringSlice(obj.Object, "spec", "excludedResources")
	for _, res := range excluded {
		if res == "pods" || res == "replicasets.apps" {
			t.Errorf("excludedResources = %v: must not exclude %q — without pods in the backup, Velero produces no PodVolumeBackup at all (D1)", excluded, res)
		}
	}
}

func TestCreateVeleroBackup_RequestsFilesystemBackupOfEveryVolume(t *testing.T) {
	sc := newTestStatusClient()
	backupName, err := sc.CreateVeleroBackup(context.Background(), "demo", "velero-system", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	obj, err := sc.client.Resource(veleroBackupGVR).Namespace("velero-system").Get(context.Background(), backupName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("unexpected error re-fetching backup: %v", err)
	}
	fsBackup, _, _ := unstructured.NestedBool(obj.Object, "spec", "defaultVolumesToFsBackup")
	if !fsBackup {
		t.Error("defaultVolumesToFsBackup = false, want true — without it Velero backs up no volume data regardless of what's excluded")
	}
}

func TestCreateVeleroBackup_ExcludesOnlyEventsAndLeases(t *testing.T) {
	sc := newTestStatusClient()
	backupName, err := sc.CreateVeleroBackup(context.Background(), "demo", "velero-system", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	obj, err := sc.client.Resource(veleroBackupGVR).Namespace("velero-system").Get(context.Background(), backupName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("unexpected error re-fetching backup: %v", err)
	}
	excluded, _, _ := unstructured.NestedStringSlice(obj.Object, "spec", "excludedResources")
	// "events.events.k8s.io", not "events.k8s.io": Velero's ParseGroupResource
	// splits on the FIRST dot, so the group name has to be repeated after the
	// resource name to actually match the events.k8s.io Event (recette cas E:
	// the bare "events.k8s.io" form left events.k8s.io/v1 Event in the backup).
	want := map[string]bool{"events": true, "events.events.k8s.io": true, "leases": true}
	if len(excluded) != len(want) {
		t.Fatalf("excludedResources = %v, want exactly %v", excluded, want)
	}
	for _, res := range excluded {
		if !want[res] {
			t.Errorf("excludedResources contains unexpected %q (got %v)", res, excluded)
		}
	}
}

// --- GetVeleroBackupPhase ---
//
// This is the pre-flight check an in-place restore relies on: CreateVeleroRestore
// must not touch anything unless the phase it reports is "Completed".

func TestGetVeleroBackupPhase(t *testing.T) {
	tests := []struct {
		name      string
		phase     string
		wantPhase string
	}{
		{"completed backup reports its phase", "Completed", "Completed"},
		{"in-progress backup reports its phase", "InProgress", "InProgress"},
		{"failed backup reports its phase", "Failed", "Failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := newTestStatusClient(newBackupObj("manual-demo-123", "velero-system", tt.phase))
			phase, err := sc.GetVeleroBackupPhase(context.Background(), "manual-demo-123", "velero-system")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if phase != tt.wantPhase {
				t.Errorf("phase = %q, want %q", phase, tt.wantPhase)
			}
		})
	}
}

func TestGetVeleroBackupPhase_UnknownBackupErrors(t *testing.T) {
	sc := newTestStatusClient(newBackupObj("manual-demo-123", "velero-system", "Completed"))
	if _, err := sc.GetVeleroBackupPhase(context.Background(), "does-not-exist", "velero-system"); err == nil {
		t.Error("expected an error looking up a backup that doesn't exist")
	}
}

// --- detectVClusterTopology ---
//
// A vcluster can run with etcd embedded in its own StatefulSet, or split out
// into a dedicated etcd StatefulSet with the control-plane as a Deployment.
// Everything downstream (scaling, waiting for pods, picking the PVC) depends
// on telling these apart correctly.

func TestDetectVClusterTopology_EmbeddedWhenNoEtcdStatefulSet(t *testing.T) {
	sc := newTestStatusClient(newStatefulSetObj("vcluster-demo", "vcluster-demo", map[string]string{"app": "vcluster"}))
	topo, err := sc.detectVClusterTopology(context.Background(), "demo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if topo.ControlPlaneKind != "StatefulSet" {
		t.Errorf("ControlPlaneKind = %q, want StatefulSet", topo.ControlPlaneKind)
	}
	if topo.EtcdStatefulSet != "" {
		t.Errorf("EtcdStatefulSet = %q, want empty (embedded)", topo.EtcdStatefulSet)
	}
	if topo.PVCName != "data-vcluster-demo-0" {
		t.Errorf("PVCName = %q, want data-vcluster-demo-0", topo.PVCName)
	}
}

func TestDetectVClusterTopology_ExternalEtcdWithDeploymentControlPlane(t *testing.T) {
	sc := newTestStatusClient(
		newDeploymentObj("vcluster-demo", "vcluster-demo", map[string]string{"app": "vcluster"}),
		newStatefulSetObj("vcluster-demo-etcd", "vcluster-demo", map[string]string{"app": "etcd"}),
	)
	topo, err := sc.detectVClusterTopology(context.Background(), "demo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if topo.ControlPlaneKind != "Deployment" {
		t.Errorf("ControlPlaneKind = %q, want Deployment", topo.ControlPlaneKind)
	}
	if topo.EtcdStatefulSet != "vcluster-demo-etcd" {
		t.Errorf("EtcdStatefulSet = %q, want vcluster-demo-etcd", topo.EtcdStatefulSet)
	}
	if topo.PVCName != "data-vcluster-demo-etcd-0" {
		t.Errorf("PVCName = %q, want data-vcluster-demo-etcd-0", topo.PVCName)
	}
}

func TestDetectVClusterTopology_ExternalEtcdWithStatefulSetControlPlane(t *testing.T) {
	// Older vcluster charts run the control-plane as a StatefulSet even with
	// external etcd — both "vcluster-demo" and "vcluster-demo-etcd" exist.
	sc := newTestStatusClient(
		newStatefulSetObj("vcluster-demo", "vcluster-demo", map[string]string{"app": "vcluster"}),
		newStatefulSetObj("vcluster-demo-etcd", "vcluster-demo", map[string]string{"app": "etcd"}),
	)
	topo, err := sc.detectVClusterTopology(context.Background(), "demo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if topo.ControlPlaneKind != "StatefulSet" {
		t.Errorf("ControlPlaneKind = %q, want StatefulSet", topo.ControlPlaneKind)
	}
	if topo.EtcdStatefulSet != "vcluster-demo-etcd" {
		t.Errorf("EtcdStatefulSet = %q, want vcluster-demo-etcd", topo.EtcdStatefulSet)
	}
}

// --- ScaleVClusterWorkloads ---

func TestScaleVClusterWorkloads_EmbeddedScalesTheSingleStatefulSet(t *testing.T) {
	sc := newTestStatusClient(newStatefulSetObj("vcluster-demo", "vcluster-demo", map[string]string{"app": "vcluster"}))
	if err := sc.ScaleVClusterWorkloads(context.Background(), "demo", 0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	obj, err := sc.client.Resource(statefulSetGVR).Namespace("vcluster-demo").Get(context.Background(), "vcluster-demo", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("unexpected error re-fetching statefulset: %v", err)
	}
	replicas, _, _ := unstructured.NestedInt64(obj.Object, "spec", "replicas")
	if replicas != 0 {
		t.Errorf("replicas = %d, want 0", replicas)
	}
}

func TestScaleVClusterWorkloads_ExternalEtcdScalesBothWorkloads(t *testing.T) {
	sc := newTestStatusClient(
		newDeploymentObj("vcluster-demo", "vcluster-demo", map[string]string{"app": "vcluster"}),
		newStatefulSetObj("vcluster-demo-etcd", "vcluster-demo", map[string]string{"app": "etcd"}),
	)
	if err := sc.ScaleVClusterWorkloads(context.Background(), "demo", 0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dep, err := sc.client.Resource(deploymentGVR).Namespace("vcluster-demo").Get(context.Background(), "vcluster-demo", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("unexpected error re-fetching deployment: %v", err)
	}
	if replicas, _, _ := unstructured.NestedInt64(dep.Object, "spec", "replicas"); replicas != 0 {
		t.Errorf("control-plane replicas = %d, want 0", replicas)
	}

	sts, err := sc.client.Resource(statefulSetGVR).Namespace("vcluster-demo").Get(context.Background(), "vcluster-demo-etcd", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("unexpected error re-fetching etcd statefulset: %v", err)
	}
	if replicas, _, _ := unstructured.NestedInt64(sts.Object, "spec", "replicas"); replicas != 0 {
		t.Errorf("etcd replicas = %d, want 0", replicas)
	}
}

func TestScaleVClusterWorkloads_MissingControlPlaneErrors(t *testing.T) {
	// Nothing seeded: embedded topology is assumed (no etcd statefulset), but
	// there's no "vcluster-demo" statefulset to patch either.
	sc := newTestStatusClient()
	if err := sc.ScaleVClusterWorkloads(context.Background(), "demo", 0); err == nil {
		t.Error("expected an error scaling a control-plane workload that doesn't exist")
	}
}

// --- DeleteVClusterPVC ---
//
// Which PVC to delete depends entirely on the topology: the wrong name here
// is a silent no-op that leaves the in-place restore's destructive step never
// actually running (the original bug this fixes).

func TestDeleteVClusterPVC_EmbeddedTargetsTheEmbeddedPVCName(t *testing.T) {
	sc := newTestStatusClient(
		newStatefulSetObj("vcluster-demo", "vcluster-demo", map[string]string{"app": "vcluster"}),
		newPVCObj("data-vcluster-demo-0", "vcluster-demo"),
	)
	if err := sc.DeleteVClusterPVC(context.Background(), "demo"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := sc.client.Resource(persistentVolumeClaimGVR).Namespace("vcluster-demo").Get(context.Background(), "data-vcluster-demo-0", metav1.GetOptions{}); err == nil {
		t.Error("expected the embedded PVC to be gone")
	}
}

func TestDeleteVClusterPVC_ExternalEtcdTargetsTheEtcdPVCName(t *testing.T) {
	sc := newTestStatusClient(
		newDeploymentObj("vcluster-demo", "vcluster-demo", map[string]string{"app": "vcluster"}),
		newStatefulSetObj("vcluster-demo-etcd", "vcluster-demo", map[string]string{"app": "etcd"}),
		newPVCObj("data-vcluster-demo-etcd-0", "vcluster-demo"),
		// The embedded-mode PVC name must be left alone: it doesn't even exist
		// here, but if the code guessed wrong it would try to delete this one
		// and quietly succeed at deleting nothing.
	)
	if err := sc.DeleteVClusterPVC(context.Background(), "demo"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := sc.client.Resource(persistentVolumeClaimGVR).Namespace("vcluster-demo").Get(context.Background(), "data-vcluster-demo-etcd-0", metav1.GetOptions{}); err == nil {
		t.Error("expected the etcd PVC to be gone")
	}
}

// --- WaitForVClusterPodsGone ---
//
// Deleting a still-mounted PVC leaves it stuck Terminating, so the restore
// path must wait for the pods to actually disappear before deleting it. Pods
// are found via the workload's own selector, not by guessing names — that's
// what makes this work for both a StatefulSet's fixed pod name and a
// Deployment's hashed one.

func TestWaitForVClusterPodsGone_AlreadyAbsent(t *testing.T) {
	sc := newTestStatusClient()
	if err := sc.WaitForVClusterPodsGone(context.Background(), "demo", 2*time.Second); err != nil {
		t.Errorf("expected no error when nothing is deployed, got %v", err)
	}
}

func TestWaitForVClusterPodsGone_ReturnsOnceControlPlanePodIsGone(t *testing.T) {
	// StatefulSet is there (embedded topology) but its pod already terminated.
	sc := newTestStatusClient(newStatefulSetObj("vcluster-demo", "vcluster-demo", map[string]string{"app": "vcluster"}))
	if err := sc.WaitForVClusterPodsGone(context.Background(), "demo", 2*time.Second); err != nil {
		t.Errorf("expected no error when the pod is already gone, got %v", err)
	}
}

func TestWaitForVClusterPodsGone_TimesOutWhileControlPlanePodStillThere(t *testing.T) {
	labels := map[string]string{"app": "vcluster"}
	sc := newTestStatusClient(
		newStatefulSetObj("vcluster-demo", "vcluster-demo", labels),
		newPodObj("vcluster-demo-0", "vcluster-demo", labels),
	)
	err := sc.WaitForVClusterPodsGone(context.Background(), "demo", 1*time.Second)
	if err == nil {
		t.Error("expected an error: the control-plane pod is still there")
	}
}

func TestWaitForVClusterPodsGone_TimesOutWhileEtcdPodStillThere(t *testing.T) {
	cpLabels := map[string]string{"app": "vcluster"}
	etcdLabels := map[string]string{"app": "etcd"}
	sc := newTestStatusClient(
		newDeploymentObj("vcluster-demo", "vcluster-demo", cpLabels),
		newStatefulSetObj("vcluster-demo-etcd", "vcluster-demo", etcdLabels),
		// Control-plane pod already gone, etcd's isn't — both must be waited on.
		newPodObj("vcluster-demo-etcd-0", "vcluster-demo", etcdLabels),
	)
	err := sc.WaitForVClusterPodsGone(context.Background(), "demo", 1*time.Second)
	if err == nil {
		t.Error("expected an error: the etcd pod is still there")
	}
}

func TestWaitForVClusterPodsGone_IgnoresPodsThatDontMatchTheSelector(t *testing.T) {
	// A pod sitting in the namespace under a different label shouldn't block
	// the wait — only pods matching the workload's own selector count.
	sc := newTestStatusClient(
		newStatefulSetObj("vcluster-demo", "vcluster-demo", map[string]string{"app": "vcluster"}),
		newPodObj("some-other-pod", "vcluster-demo", map[string]string{"app": "unrelated"}),
	)
	if err := sc.WaitForVClusterPodsGone(context.Background(), "demo", 2*time.Second); err != nil {
		t.Errorf("expected no error, unrelated pod should not block: %v", err)
	}
}

func TestWaitForVClusterPodsGone_SkipsWaitWhenSelectorIsEmpty(t *testing.T) {
	// StatefulSet with no selector labels at all: an empty label selector
	// would match every pod in the namespace, so this must return
	// immediately instead of waiting on pods that have nothing to do with
	// this workload.
	sc := newTestStatusClient(
		newStatefulSetObj("vcluster-demo", "vcluster-demo", nil),
		newPodObj("some-other-pod", "vcluster-demo", map[string]string{"app": "unrelated"}),
	)
	if err := sc.WaitForVClusterPodsGone(context.Background(), "demo", 1*time.Second); err != nil {
		t.Errorf("expected no error with an empty selector, got %v", err)
	}
}

func TestWaitForVClusterPodsGone_RespectsContextCancellation(t *testing.T) {
	labels := map[string]string{"app": "vcluster"}
	sc := newTestStatusClient(
		newStatefulSetObj("vcluster-demo", "vcluster-demo", labels),
		newPodObj("vcluster-demo-0", "vcluster-demo", labels),
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := sc.WaitForVClusterPodsGone(ctx, "demo", 5*time.Second)
	if err == nil {
		t.Error("expected an error when the context is already cancelled")
	}
}

// --- QuiesceVClusterForInPlaceRestore ---
//
// D2/D3 (docs/recette-restauration.md): scaling the control-plane and etcd
// workloads to 0 before an in-place restore leaves them alive, still
// reconciling — Velero's own restore of the SAME object (existingResourcePolicy:
// update) races that live controller and recreates a pod node-agent never
// tracks, which either hangs the whole Restore forever (Deployment: new random
// pod name) or silently restores into an empty volume (StatefulSet: same pod
// name, but the CONTROLLER created it, not Velero, so the volume never gets
// filled). Reproduced live against Velero v1.15.1 on the recette cell; the fix
// is deleting the workloads outright instead of scaling them down.

func TestQuiesceVClusterForInPlaceRestore_EmbeddedDeletesTheSingleStatefulSet(t *testing.T) {
	sc := newTestStatusClient(newStatefulSetObj("vcluster-demo", "vcluster-demo", map[string]string{"app": "vcluster"}))

	topo, err := sc.QuiesceVClusterForInPlaceRestore(context.Background(), "demo", 200*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if topo.ControlPlaneKind != "StatefulSet" || topo.PVCName != "data-vcluster-demo-0" {
		t.Errorf("topology = %+v, want embedded StatefulSet / data-vcluster-demo-0", topo)
	}
	if _, err := sc.client.Resource(statefulSetGVR).Namespace("vcluster-demo").Get(context.Background(), "vcluster-demo", metav1.GetOptions{}); err == nil {
		t.Error("expected the control-plane statefulset to be DELETED, not merely scaled to 0")
	}
}

func TestQuiesceVClusterForInPlaceRestore_ExternalEtcdDeletesBothWorkloads(t *testing.T) {
	sc := newTestStatusClient(
		newDeploymentObj("vcluster-demo", "vcluster-demo", map[string]string{"app": "vcluster"}),
		newStatefulSetObj("vcluster-demo-etcd", "vcluster-demo", map[string]string{"app": "etcd"}),
	)

	topo, err := sc.QuiesceVClusterForInPlaceRestore(context.Background(), "demo", 200*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if topo.ControlPlaneKind != "Deployment" || topo.EtcdStatefulSet != "vcluster-demo-etcd" || topo.PVCName != "data-vcluster-demo-etcd-0" {
		t.Errorf("topology = %+v, want external-etcd Deployment / vcluster-demo-etcd / data-vcluster-demo-etcd-0", topo)
	}
	if _, err := sc.client.Resource(deploymentGVR).Namespace("vcluster-demo").Get(context.Background(), "vcluster-demo", metav1.GetOptions{}); err == nil {
		t.Error("expected the control-plane deployment to be DELETED, not merely scaled to 0")
	}
	if _, err := sc.client.Resource(statefulSetGVR).Namespace("vcluster-demo").Get(context.Background(), "vcluster-demo-etcd", metav1.GetOptions{}); err == nil {
		t.Error("expected the etcd statefulset to be DELETED, not merely scaled to 0")
	}
}

func TestQuiesceVClusterForInPlaceRestore_IdempotentWhenAlreadyDeleted(t *testing.T) {
	// Nothing seeded: a retry of a sequence whose earlier attempt already
	// deleted the workloads (or a vcluster with nothing deployed at all) must
	// not be treated as a failure.
	sc := newTestStatusClient()
	if _, err := sc.QuiesceVClusterForInPlaceRestore(context.Background(), "demo", 100*time.Millisecond); err != nil {
		t.Fatalf("expected no error when the workloads are already gone, got %v", err)
	}
}

func TestQuiesceVClusterForInPlaceRestore_PropagatesARealDeleteFailure(t *testing.T) {
	refus := func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.GetVerb() == "delete" && action.GetResource().Resource == "statefulsets" {
			return true, nil, apierrors.NewForbidden(
				schema.GroupResource{Resource: "statefulsets"}, "vcluster-demo",
				fmt.Errorf("refusé par un webhook"))
		}
		return false, nil, nil
	}
	sc := NewTestStatusClientWithReactor(refus,
		newStatefulSetObj("vcluster-demo", "vcluster-demo", map[string]string{"app": "vcluster"}),
	)

	if _, err := sc.QuiesceVClusterForInPlaceRestore(context.Background(), "demo", 100*time.Millisecond); err == nil {
		t.Error("expected the forbidden delete to surface as an error, not be swallowed like a NotFound")
	}
}

func TestQuiesceVClusterForInPlaceRestore_SelectorIsCapturedBeforeTheDelete(t *testing.T) {
	// The whole point of capturing the selector up front: by the time the pod
	// wait runs, a Get on the workload returns NotFound (we just deleted it).
	// If the code re-read the selector at that point instead of reusing what
	// it captured before deleting, it would see "no selector" and return
	// immediately — instead of actually waiting out the full timeout below on
	// the pod that (the fake client does no real garbage collection) never
	// disappears on its own.
	labels := map[string]string{"app": "vcluster"}
	sc := newTestStatusClient(
		newStatefulSetObj("vcluster-demo", "vcluster-demo", labels),
		newPodObj("vcluster-demo-0", "vcluster-demo", labels),
	)

	const timeout = 300 * time.Millisecond
	start := time.Now()
	_, err := sc.QuiesceVClusterForInPlaceRestore(context.Background(), "demo", timeout)
	elapsed := time.Since(start)

	if err != nil {
		// A pod-wait timeout is logged and swallowed, not fatal — the caller
		// still deletes the PVC (see the comment on QuiesceVClusterForInPlaceRestore).
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed < timeout {
		t.Errorf("returned after %s, want at least the %s pod-wait timeout — the selector must not have been captured before the delete", elapsed, timeout)
	}
}

// --- DeleteVClusterPVCNamed ---

func TestDeleteVClusterPVCNamed_DoesNotReDeriveTheNameFromTopology(t *testing.T) {
	// Nothing here would let detectVClusterTopology guess this exact PVC
	// name — proving this path trusts the caller's topology instead of
	// re-detecting it, which is the whole reason it exists (see
	// QuiesceVClusterForInPlaceRestore: by the time this runs, the workload
	// detectVClusterTopology would inspect is already gone, deleted on
	// purpose).
	sc := newTestStatusClient(newPVCObj("data-vcluster-demo-etcd-0", "vcluster-demo"))
	if err := sc.DeleteVClusterPVCNamed(context.Background(), "demo", "data-vcluster-demo-etcd-0"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := sc.client.Resource(persistentVolumeClaimGVR).Namespace("vcluster-demo").Get(context.Background(), "data-vcluster-demo-etcd-0", metav1.GetOptions{}); err == nil {
		t.Error("expected the PVC to be gone")
	}
}

// --- DeleteVeleroBackup ---
//
// D5 (docs/recette-restauration.md, cas F): deleting the Backup Kubernetes
// object directly is not how you delete a Velero backup — the data stays in
// the bucket and backup-sync resurrects the object. Confirmed on the recette
// cell: toast said "supprimé", the object was back three minutes later with a
// fresh creationTimestamp, and `kubectl get deletebackuprequests` was empty
// the whole time. The fix creates a DeleteBackupRequest instead; these tests
// check that a request actually gets created and names the right backup, and
// — deliberately — that the Backup object itself is left alone: Velero, not
// this code, is what removes it once the data is gone.

func TestDeleteVeleroBackup_CreatesADeleteBackupRequestNamingTheBackup(t *testing.T) {
	var captured *unstructured.Unstructured
	capture := func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.GetVerb() == "create" && action.GetResource().Resource == "deletebackuprequests" {
			if create, ok := action.(clienttesting.CreateAction); ok {
				if u, ok := create.GetObject().(*unstructured.Unstructured); ok {
					captured = u
				}
			}
		}
		return false, nil, nil
	}
	sc := NewTestStatusClientWithReactor(capture, newBackupObj("manual-demo-1", "velero-system", "Completed"))

	if err := sc.DeleteVeleroBackup(context.Background(), "manual-demo-1", "velero-system"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if captured == nil {
		t.Fatal("expected a DeleteBackupRequest to be created — no create action observed")
	}
	if captured.GetKind() != "DeleteBackupRequest" {
		t.Errorf("kind = %q, want DeleteBackupRequest", captured.GetKind())
	}
	if captured.GetNamespace() != "velero-system" {
		t.Errorf("namespace = %q, want velero-system", captured.GetNamespace())
	}
	backupName, _, _ := unstructured.NestedString(captured.Object, "spec", "backupName")
	if backupName != "manual-demo-1" {
		t.Errorf("spec.backupName = %q, want manual-demo-1", backupName)
	}
}

func TestDeleteVeleroBackup_DoesNotDeleteTheBackupObjectItself(t *testing.T) {
	sc := newTestStatusClient(newBackupObj("manual-demo-1", "velero-system", "Completed"))

	if err := sc.DeleteVeleroBackup(context.Background(), "manual-demo-1", "velero-system"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := sc.client.Resource(veleroBackupGVR).Namespace("velero-system").Get(context.Background(), "manual-demo-1", metav1.GetOptions{}); err != nil {
		t.Errorf("expected the Backup object to still be there (Velero deletes it once the DeleteBackupRequest is processed), got %v", err)
	}
}

// --- RestartVClusterDNS ---

// TestRestartVClusterDNS_OnlyTargetsThisVClustersDNS verrouille le sélecteur.
//
// Les pods CoreDNS syncés portent `k8s-app: vcluster-kube-dns`, recopié tel
// quel depuis le vcluster : deux vclusters du même hôte ont donc des pods
// strictement homonymes de ce point de vue. Filtrer sur ce seul label
// redémarrerait le DNS de tous les vclusters à chaque restauration de l'un
// d'eux — une panne infligée à des tenants qui n'ont rien demandé, au pire
// moment, celui où un incident est déjà en cours.
//
// C'est `vcluster.loft.sh/managed-by`, posé par le syncer, qui nomme le
// propriétaire. Les deux labels ensemble, et eux seuls, désignent le bon
// vcluster.
func TestRestartVClusterDNS_OnlyTargetsThisVClustersDNS(t *testing.T) {
	const dnsLabel = "vcluster-kube-dns"

	mine := newPodObj("coredns-mine", "vcluster-alpha", map[string]string{
		"k8s-app":                     dnsLabel,
		"vcluster.loft.sh/managed-by": "alpha",
	})
	// Même label k8s-app, autre propriétaire : un voisin dans le même
	// namespace hôte ne doit pas être touché.
	neighbour := newPodObj("coredns-neighbour", "vcluster-alpha", map[string]string{
		"k8s-app":                     dnsLabel,
		"vcluster.loft.sh/managed-by": "beta",
	})
	// Une charge du tenant, qui n'a rien à voir avec le DNS.
	workload := newPodObj("app-du-tenant", "vcluster-alpha", map[string]string{
		"app":                         "facturation",
		"vcluster.loft.sh/managed-by": "alpha",
	})

	s := NewTestStatusClient(mine, neighbour, workload)

	n, err := s.RestartVClusterDNS(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("RestartVClusterDNS: %v", err)
	}
	if n != 1 {
		t.Errorf("expected exactly 1 pod deleted, got %d", n)
	}

	remaining := map[string]bool{}
	list, err := s.client.Resource(podGVR).Namespace("vcluster-alpha").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing pods: %v", err)
	}
	for _, p := range list.Items {
		remaining[p.GetName()] = true
	}

	if remaining["coredns-mine"] {
		t.Error("le CoreDNS du vcluster restauré n'a pas été supprimé : il gardera son CA périmé")
	}
	if !remaining["coredns-neighbour"] {
		t.Error("le CoreDNS d'un AUTRE vcluster a été supprimé : restaurer un vcluster casserait le DNS des voisins")
	}
	if !remaining["app-du-tenant"] {
		t.Error("une charge du tenant a été supprimée")
	}
}

// TestRestartVClusterDNS_NoDNSPodIsNotAnError : un vcluster sans pod CoreDNS
// syncé (jamais démarré, ou déjà en cours de redémarrage) ne doit pas faire
// échouer la reprise. Il n'y a rien à redémarrer, ce qui est le résultat voulu.
func TestRestartVClusterDNS_NoDNSPodIsNotAnError(t *testing.T) {
	s := NewTestStatusClient(newPodObj("autre-chose", "vcluster-alpha", map[string]string{"app": "x"}))

	n, err := s.RestartVClusterDNS(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("aucun pod DNS ne doit pas être une erreur, got %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 deleted, got %d", n)
	}
}
