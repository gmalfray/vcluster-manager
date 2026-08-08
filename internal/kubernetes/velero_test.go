package kubernetes

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
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
