package kubernetes

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
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
}

// NewTestStatusClient builds a StatusClient backed by a fake dynamic client
// seeded with objs. It exists so tests in other packages (the service layer,
// in particular) can exercise guard logic against Velero/pod/PVC state
// without a real cluster — not meant to be used from production code.
func NewTestStatusClient(objs ...runtime.Object) *StatusClient {
	scheme := runtime.NewScheme()
	return &StatusClient{client: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, testListKinds, objs...)}
}
