package config

import (
	"context"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const configMapName = "vcluster-manager-state"

// configmapBackend stores state in a Kubernetes ConfigMap.
// It survives pod rescheduling without requiring a PVC.
type configmapBackend struct {
	client    k8sclient.Interface
	namespace string
}

func newConfigMapBackend() (*configmapBackend, error) {
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("configmap backend requires in-cluster config: %w", err)
	}
	client, err := k8sclient.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("creating k8s client for state backend: %w", err)
	}
	ns := os.Getenv("K8S_NAMESPACE")
	if ns == "" {
		// Try to read the pod's namespace from the mounted serviceaccount file
		data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
		if err == nil {
			ns = string(data)
		}
	}
	if ns == "" {
		ns = "vcluster-manager"
	}
	return &configmapBackend{client: client, namespace: ns}, nil
}

func (b *configmapBackend) readKey(key string) ([]byte, error) {
	cm, err := b.client.CoreV1().ConfigMaps(b.namespace).Get(
		context.Background(), configMapName, metav1.GetOptions{},
	)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	v, ok := cm.Data[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	return []byte(v), nil
}

func (b *configmapBackend) writeKey(key string, data []byte) error {
	ctx := context.Background()
	cms := b.client.CoreV1().ConfigMaps(b.namespace)

	cm, err := cms.Get(ctx, configMapName, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		_, err = cms.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      configMapName,
				Namespace: b.namespace,
				Labels:    map[string]string{"app": "vcluster-manager"},
			},
			Data: map[string]string{key: string(data)},
		}, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}

	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	cm.Data[key] = string(data)
	_, err = cms.Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

func (b *configmapBackend) readSettings() ([]byte, error)   { return b.readKey("settings.json") }
func (b *configmapBackend) writeSettings(data []byte) error { return b.writeKey("settings.json", data) }
func (b *configmapBackend) readDeleting() ([]byte, error)   { return b.readKey("deleting.json") }
func (b *configmapBackend) writeDeleting(data []byte) error { return b.writeKey("deleting.json", data) }
