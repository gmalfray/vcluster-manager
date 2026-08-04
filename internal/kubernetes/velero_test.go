package kubernetes

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// newTestStatusClient builds a StatusClient backed by a fake dynamic client
// seeded with objs, for testing the guard logic against Velero/pod state
// without a real cluster.
func newTestStatusClient(objs ...runtime.Object) *StatusClient {
	scheme := runtime.NewScheme()
	return &StatusClient{client: dynamicfake.NewSimpleDynamicClient(scheme, objs...)}
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

func newPodObj(name, namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
		},
	}}
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

// --- WaitForVClusterPodGone ---
//
// Deleting a still-mounted PVC leaves it stuck Terminating, so the restore
// path must wait for the pod to actually disappear before deleting it.

func TestWaitForVClusterPodGone_AlreadyAbsent(t *testing.T) {
	sc := newTestStatusClient()
	if err := sc.WaitForVClusterPodGone(context.Background(), "demo", 2*time.Second); err != nil {
		t.Errorf("expected no error when the pod is already gone, got %v", err)
	}
}

func TestWaitForVClusterPodGone_TimesOutWhilePodStillThere(t *testing.T) {
	sc := newTestStatusClient(newPodObj("vcluster-demo-0", "vcluster-demo"))
	err := sc.WaitForVClusterPodGone(context.Background(), "demo", 1*time.Second)
	if err == nil {
		t.Error("expected an error: the pod is still there")
	}
}

func TestWaitForVClusterPodGone_RespectsContextCancellation(t *testing.T) {
	sc := newTestStatusClient(newPodObj("vcluster-demo-0", "vcluster-demo"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := sc.WaitForVClusterPodGone(ctx, "demo", 5*time.Second)
	if err == nil {
		t.Error("expected an error when the context is already cancelled")
	}
}
