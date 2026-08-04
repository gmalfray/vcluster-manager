package kubernetes

import (
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// NewTestStatusClient builds a StatusClient backed by a fake dynamic client
// seeded with objs. It exists so tests in other packages (the service layer,
// in particular) can exercise guard logic against Velero/pod/PVC state
// without a real cluster — not meant to be used from production code.
func NewTestStatusClient(objs ...runtime.Object) *StatusClient {
	scheme := runtime.NewScheme()
	return &StatusClient{client: dynamicfake.NewSimpleDynamicClient(scheme, objs...)}
}
