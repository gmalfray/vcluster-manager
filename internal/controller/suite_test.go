package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/gmalfray/vcluster-manager/api/v1alpha1"
)

// The suite runs against a real kube-apiserver (envtest), not a fake client:
// the properties under test — status subresource semantics, optimistic
// concurrency, what actually persists across a restart — are exactly the ones a
// fake client would paper over.
var (
	testEnv   *envtest.Environment
	k8sClient client.Client
	restCfg   *rest.Config
)

func TestMain(m *testing.M) {
	// These tests need a real kube-apiserver + etcd (envtest binaries). Now that
	// the operator lives in the app module, `go test ./...` would otherwise fail
	// for anyone who has not fetched them. Say so loudly and skip rather than
	// break the whole suite — `make test-operator` sets this up.
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Fprintln(os.Stderr, "SKIP internal/controller: KUBEBUILDER_ASSETS non défini (voir `make test-operator`)")
		os.Exit(0)
	}

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := testEnv.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "envtest start: %v\n", err)
		os.Exit(1)
	}
	restCfg = cfg

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "add to scheme: %v\n", err)
		os.Exit(1)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "add corev1: %v\n", err)
		os.Exit(1)
	}
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "client: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()
	if err := testEnv.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "envtest stop: %v\n", err)
	}
	os.Exit(code)
}

// newMarker creates the namespace and the marker object, mirroring the design's
// layout: one marker named after the vcluster, in vcluster-<name>.
func newMarker(t *testing.T, ctx context.Context, name string, annotations map[string]string) *v1alpha1.VClusterVeleroOps {
	t.Helper()
	ns := "vcluster-" + name
	if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	obj := &v1alpha1.VClusterVeleroOps{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Annotations: annotations},
	}
	if err := k8sClient.Create(ctx, obj); err != nil {
		t.Fatalf("create marker: %v", err)
	}
	return obj
}

// newReconciler builds a reconciler with no shared state whatsoever, so calling
// it twice with two different instances is a faithful stand-in for a restart.
func newReconciler(ops *fakeOps) *VeleroOpsReconciler {
	return &VeleroOpsReconciler{Client: k8sClient, Ops: ops}
}

// seedRestoreStatus puts the marker in a given restore state, the way a restart
// would find it.
func seedRestoreStatus(t *testing.T, ctx context.Context, obj *v1alpha1.VClusterVeleroOps, st v1alpha1.RestoreOpsStatus) {
	t.Helper()
	obj.Status.Restore = st
	if err := k8sClient.Status().Update(ctx, obj); err != nil {
		t.Fatalf("seed status: %v", err)
	}
}

func reqFor(obj *v1alpha1.VClusterVeleroOps) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: obj.Name, Namespace: obj.Namespace}}
}

func fetch(t *testing.T, ctx context.Context, obj *v1alpha1.VClusterVeleroOps) *v1alpha1.VClusterVeleroOps {
	t.Helper()
	var got v1alpha1.VClusterVeleroOps
	key := types.NamespacedName{Name: obj.Name, Namespace: obj.Namespace}
	if err := k8sClient.Get(ctx, key, &got); err != nil {
		t.Fatalf("get marker: %v", err)
	}
	return &got
}

// reconcileSurvivingAKill runs one reconcile and swallows the panic the fake
// raises to simulate the process dying mid-sequence.
func reconcileSurvivingAKill(t *testing.T, ctx context.Context, r *VeleroOpsReconciler, obj *v1alpha1.VClusterVeleroOps) {
	t.Helper()
	killed := false
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				killed = true
			}
		}()
		_, _ = r.Reconcile(ctx, reqFor(obj))
	}()
	if !killed {
		t.Fatal("expected the fake to simulate a process kill mid-sequence")
	}
}

func condition(obj *v1alpha1.VClusterVeleroOps, condType string) *metav1.Condition {
	for i := range obj.Status.Conditions {
		if obj.Status.Conditions[i].Type == condType {
			return &obj.Status.Conditions[i]
		}
	}
	return nil
}

func requireCond(t *testing.T, obj *v1alpha1.VClusterVeleroOps, condType string, status metav1.ConditionStatus, reason string) {
	t.Helper()
	c := condition(obj, condType)
	if c == nil {
		t.Fatalf("condition %s absente (conditions: %+v)", condType, obj.Status.Conditions)
	}
	if c.Status != status {
		t.Fatalf("condition %s: status %s, attendu %s (message: %s)", condType, c.Status, status, c.Message)
	}
	if reason != "" && c.Reason != reason {
		t.Fatalf("condition %s: reason %s, attendu %s", condType, c.Reason, reason)
	}
}
