package config

// stateBackend abstracts persistence of settings.json and deleting.json.
// The default implementation uses local files; the configmap implementation
// uses a Kubernetes ConfigMap (survives pod rescheduling without a PVC).
type stateBackend interface {
	readSettings() ([]byte, error)
	writeSettings(data []byte) error
	readDeleting() ([]byte, error)
	writeDeleting(data []byte) error
}
