package kubernetes

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// testListKinds maps every GVR this package Lists to its List kind. The fake
// dynamic client needs this up front: NewSimpleDynamicClient alone only
// learns a GVR's list kind from the seed objects, so a List call against a
// resource with zero seeded objects of that kind panics ("coding error: you
// must register resource to list kind..."). Declaring the mapping here means
// tests can seed nothing of a given kind and still List it and get an empty
// result, matching what a real API server does.
var testListKinds = map[schema.GroupVersionResource]string{
	helmReleaseGVR:           "HelmReleaseList",
	kustomizationGVR:         "KustomizationList",
	secretGVR:                "SecretList",
	resourceQuotaGVR:         "ResourceQuotaList",
	veleroBackupGVR:          "BackupList",
	veleroDownloadRequestGVR: "DownloadRequestList",
	veleroRestoreGVR:         "RestoreList",
	statefulSetGVR:           "StatefulSetList",
	deploymentGVR:            "DeploymentList",
	persistentVolumeClaimGVR: "PersistentVolumeClaimList",
	namespaceGVR:             "NamespaceList",
	podGVR:                   "PodList",
	veleroOpsGVR:             "VClusterVeleroOpsList",
}

// NewTestStatusClient builds a StatusClient backed by a fake dynamic client
// seeded with objs. It exists so tests in other packages (the service layer,
// in particular) can exercise guard logic against Velero/pod/PVC state
// without a real cluster — not meant to be used from production code.
func NewTestStatusClient(objs ...runtime.Object) *StatusClient {
	scheme := runtime.NewScheme()
	return &StatusClient{client: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, testListKinds, objs...)}
}

// SeedTestVeleroOpsMarker creates a VClusterVeleroOps marker carrying a restore
// status, as the operator would have written it. Test-only.
//
// It exists because seeding the marker as a plain object does not work: the fake
// dynamic client derives a resource name from the kind by pluralising it, and
// "VClusterVeleroOps" is already plural — its guess never matches the real
// resource (`vclusterveleroops`), so the object lands under a GVR nobody reads.
// Going through the same GVR the production code uses sidesteps the guess.
func (s *StatusClient) SeedTestVeleroOpsMarker(ctx context.Context, name string, st VeleroOpsRestoreState) error {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "vcluster.rebuild-it.fr/v1alpha1",
		"kind":       "VClusterVeleroOps",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": "vcluster-" + name,
		},
		"status": map[string]interface{}{
			"restore": map[string]interface{}{
				"restoreName":     st.RestoreName,
				"phase":           st.Phase,
				"fromBackup":      st.FromBackup,
				"inPlace":         st.InPlace,
				"resumePending":   st.ResumePending,
				"resumeFailed":    st.ResumeFailed,
				"resumeError":     st.ResumeError,
				"volumeDestroyed": st.VolumeDestroyed,
			},
		},
	}}
	_, err := s.client.Resource(veleroOpsGVR).Namespace("vcluster-"+name).Create(ctx, obj, metav1.CreateOptions{})
	return err
}

// NewTestStatusClientWithReactor is NewTestStatusClient plus a reactor
// installed on the underlying fake dynamic client — for simulating an API
// call failing in a way the fake client can't produce on its own (e.g. the
// Velero Restore creation itself failing after every earlier step of an
// in-place restore already succeeded). Not meant to be used from production
// code.
func NewTestStatusClientWithReactor(reactor clienttesting.ReactionFunc, objs ...runtime.Object) *StatusClient {
	scheme := runtime.NewScheme()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, testListKinds, objs...)
	client.PrependReactor("*", "*", reactor)
	return &StatusClient{client: client}
}

// ReadTestVeleroOpsAnnotations reads back the annotations RequestVeleroOps
// wrote on a marker. Test-only, and it goes through the production GVR for the
// same pluralisation reason as SeedTestVeleroOpsMarker.
func (s *StatusClient) ReadTestVeleroOpsAnnotations(ctx context.Context, name string) (map[string]string, error) {
	obj, err := s.client.Resource(veleroOpsGVR).Namespace("vcluster-"+name).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return obj.GetAnnotations(), nil
}
