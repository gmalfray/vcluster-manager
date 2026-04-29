package kubernetes

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	k8sclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"

	"github.com/gmalfray/vcluster-manager/internal/models"
)

var argoAppGVR = schema.GroupVersionResource{
	Group:    "argoproj.io",
	Version:  "v1alpha1",
	Resource: "applications",
}

// getInternalKubeconfig reads the vcluster internal kubeconfig from the K8s secret.
// This kubeconfig is accessible from within the cluster (not exposed externally).
func (s *StatusClient) getInternalKubeconfig(ctx context.Context, name string) ([]byte, error) {
	namespace := "vcluster-" + name
	secretName := "vc-vcluster-" + name + "-int"

	secret, err := s.client.Resource(secretGVR).Namespace(namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting secret %s: %w", secretName, err)
	}

	data, found, err := unstructured.NestedMap(secret.Object, "data")
	if err != nil || !found {
		return nil, fmt.Errorf("no data in secret %s", secretName)
	}

	configB64, ok := data["config"].(string)
	if !ok {
		return nil, fmt.Errorf("no config key in secret %s", secretName)
	}

	kubeconfig, err := base64.StdEncoding.DecodeString(configB64)
	if err != nil {
		return nil, fmt.Errorf("decoding kubeconfig: %w", err)
	}

	return kubeconfig, nil
}

// GetKubeconfig reads the vcluster external kubeconfig from the K8s secret.
func (s *StatusClient) GetKubeconfig(ctx context.Context, name string) ([]byte, error) {
	namespace := "vcluster-" + name
	secretName := "vc-vcluster-" + name + "-ext"

	secret, err := s.client.Resource(secretGVR).Namespace(namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting secret %s: %w", secretName, err)
	}

	data, found, err := unstructured.NestedMap(secret.Object, "data")
	if err != nil || !found {
		return nil, fmt.Errorf("no data in secret %s", secretName)
	}

	configB64, ok := data["config"].(string)
	if !ok {
		return nil, fmt.Errorf("no config key in secret %s", secretName)
	}

	kubeconfig, err := base64.StdEncoding.DecodeString(configB64)
	if err != nil {
		return nil, fmt.Errorf("decoding kubeconfig: %w", err)
	}

	return kubeconfig, nil
}

// getK8sVersion queries the vcluster's /version endpoint via port-forward.
func (s *StatusClient) getK8sVersion(ctx context.Context, name string) (string, error) {
	var version string
	err := s.withVClusterPortForward(ctx, name, func(restCfg *rest.Config) error {
		restCfg.Timeout = 5 * time.Second
		clientset, err := k8sclient.NewForConfig(restCfg)
		if err != nil {
			return fmt.Errorf("creating clientset: %w", err)
		}
		serverVersion, err := clientset.Discovery().ServerVersion()
		if err != nil {
			// Fallback: try direct HTTP to /version with TLS skip if needed
			v, fallbackErr := s.getK8sVersionHTTP(restCfg)
			if fallbackErr != nil {
				return fallbackErr
			}
			version = v
			return nil
		}
		version = serverVersion.GitVersion
		return nil
	})
	return version, err
}

// getK8sVersionHTTP is a fallback that queries /version directly.
func (s *StatusClient) getK8sVersionHTTP(cfg *rest.Config) (string, error) {
	transport, err := rest.TransportFor(cfg)
	if err != nil {
		return "", fmt.Errorf("creating transport: %w", err)
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
	}

	resp, err := client.Get(cfg.Host + "/version")
	if err != nil {
		return "", fmt.Errorf("querying /version: %w", err)
	}
	defer resp.Body.Close()

	var version struct {
		GitVersion string `json:"gitVersion"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&version); err != nil {
		return "", fmt.Errorf("decoding /version: %w", err)
	}

	return version.GitVersion, nil
}

// withVClusterPortForward sets up a temporary port-forward to a vcluster pod and calls fn
// with a REST config pointed at localhost. The port-forward is closed when fn returns.
func (s *StatusClient) withVClusterPortForward(ctx context.Context, name string, fn func(restCfg *rest.Config) error) error {
	namespace := "vcluster-" + name

	kubeconfigBytes, err := s.getInternalKubeconfig(ctx, name)
	if err != nil {
		return fmt.Errorf("getting internal kubeconfig: %w", err)
	}

	clientset, err := k8sclient.NewForConfig(s.restConfig)
	if err != nil {
		return fmt.Errorf("creating clientset: %w", err)
	}

	podList, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=vcluster,release=vcluster-%s", name),
	})
	if err != nil {
		return fmt.Errorf("listing vcluster pods: %w", err)
	}
	if len(podList.Items) == 0 {
		return fmt.Errorf("no vcluster pod found in namespace %s", namespace)
	}
	podName := podList.Items[0].Name

	localPort, err := getFreePort()
	if err != nil {
		return fmt.Errorf("finding free port: %w", err)
	}

	slog.Debug("starting port-forward", "namespace", namespace, "pod", podName, "local_port", localPort)

	stopChan := make(chan struct{}, 1)
	readyChan := make(chan struct{})
	errChan := make(chan error, 1)

	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(namespace).Name(podName).SubResource("portforward")

	transport, upgrader, err := spdy.RoundTripperFor(s.restConfig)
	if err != nil {
		return fmt.Errorf("creating SPDY roundtripper: %w", err)
	}

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, req.URL())
	fw, err := portforward.New(dialer, []string{fmt.Sprintf("%d:8443", localPort)}, stopChan, readyChan, io.Discard, io.Discard)
	if err != nil {
		return fmt.Errorf("creating port forwarder: %w", err)
	}

	go func() {
		if err := fw.ForwardPorts(); err != nil {
			errChan <- err
		}
	}()

	select {
	case <-readyChan:
		slog.Debug("port-forward ready", "local_port", localPort)
	case err := <-errChan:
		return fmt.Errorf("port-forward failed: %w", err)
	case <-time.After(10 * time.Second):
		close(stopChan)
		return fmt.Errorf("port-forward timeout")
	}

	defer func() {
		close(stopChan)
		slog.Debug("port-forward stopped", "namespace", namespace, "pod", podName)
	}()

	modifiedKubeconfig, err := replaceServerURL(kubeconfigBytes, fmt.Sprintf("https://127.0.0.1:%d", localPort))
	if err != nil {
		return fmt.Errorf("modifying vcluster kubeconfig: %w", err)
	}

	restCfg, err := clientcmd.RESTConfigFromKubeConfig(modifiedKubeconfig)
	if err != nil {
		return fmt.Errorf("parsing modified kubeconfig: %w", err)
	}

	return fn(restCfg)
}

// ApplyManifestToVCluster applies a multi-document YAML manifest inside the vcluster using
// server-side apply. Uses port-forward for cross-cluster access (same as ApplyManifestToVClusterViaPortForward).
func (s *StatusClient) ApplyManifestToVCluster(ctx context.Context, name string, manifestYAML []byte) error {
	err := s.withVClusterPortForward(ctx, name, func(restCfg *rest.Config) error {
		restCfg.Timeout = 30 * time.Second
		return applyManifestWithConfig(restCfg, manifestYAML)
	})
	if err != nil {
		return err
	}
	slog.Info("manifest applied to vcluster", "vcluster", name)
	return nil
}

// ApplyManifestToVClusterViaPortForward is a deprecated alias for ApplyManifestToVCluster.
// Kept for backward compatibility with existing callers.
func (s *StatusClient) ApplyManifestToVClusterViaPortForward(ctx context.Context, name string, manifestYAML []byte) error {
	return s.ApplyManifestToVCluster(ctx, name, manifestYAML)
}

// WaitForJobComplete waits for a job to complete inside a vcluster via port-forward.
// Returns nil when the job succeeds, error on timeout or failure.
func (s *StatusClient) WaitForJobComplete(ctx context.Context, name, jobName, jobNamespace string, timeout time.Duration) error {
	return s.withVClusterPortForward(ctx, name, func(restCfg *rest.Config) error {
		restCfg.Timeout = 30 * time.Second
		vcClientset, err := k8sclient.NewForConfig(restCfg)
		if err != nil {
			return fmt.Errorf("creating vcluster clientset: %w", err)
		}

		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			job, err := vcClientset.BatchV1().Jobs(jobNamespace).Get(ctx, jobName, metav1.GetOptions{})
			if err != nil {
				time.Sleep(10 * time.Second)
				continue
			}

			for _, cond := range job.Status.Conditions {
				if cond.Type == "Complete" && cond.Status == "True" {
					return nil
				}
				if cond.Type == "Failed" && cond.Status == "True" {
					return fmt.Errorf("job %s failed: %s", jobName, cond.Message)
				}
			}

			time.Sleep(10 * time.Second)
		}

		return fmt.Errorf("job %s did not complete within %v", jobName, timeout)
	})
}

// DeleteNamespaceInVCluster deletes a namespace inside a vcluster.
func (s *StatusClient) DeleteNamespaceInVCluster(ctx context.Context, name, namespace string) error {
	return s.withVClusterDynClient(ctx, name, func(dynClient dynamic.Interface) error {
		err := dynClient.Resource(namespaceGVR).Delete(ctx, namespace, metav1.DeleteOptions{})
		if err != nil && !strings.Contains(err.Error(), "not found") {
			return fmt.Errorf("deleting namespace %s: %w", namespace, err)
		}
		return nil
	})
}

// NamespaceExistsInVCluster checks if a namespace exists inside a vcluster.
func (s *StatusClient) NamespaceExistsInVCluster(ctx context.Context, name, namespace string) (bool, error) {
	var exists bool
	err := s.withVClusterDynClient(ctx, name, func(dynClient dynamic.Interface) error {
		_, err := dynClient.Resource(namespaceGVR).Get(ctx, namespace, metav1.GetOptions{})
		if err != nil {
			if strings.Contains(err.Error(), "not found") {
				exists = false
				return nil
			}
			return err
		}
		exists = true
		return nil
	})
	return exists, err
}

// WaitForVaultWebhookReady polls until both:
//  1. the vault-webhook-{name} Kustomization on the host cluster reports Ready=True
//  2. the vault-webhook HelmRelease in namespace vcluster-{name} reports Ready=True
//     (meaning the chart is actually deployed inside the vcluster and vault-system exists)
//
// Returns ctx.Err() if the context expires first.
func (s *StatusClient) WaitForVaultWebhookReady(ctx context.Context, name string) error {
	namespace := "vcluster-" + name
	ksName := "vault-webhook-" + name

	for {
		ks, err := s.client.Resource(kustomizationGVR).Namespace(namespace).Get(ctx, ksName, metav1.GetOptions{})
		if err == nil && extractConditionStatus(ks, "Ready") == "Ready" {
			// Also wait for the HelmRelease to be Ready (chart deployed inside the vcluster)
			hr, err := s.client.Resource(helmReleaseGVR).Namespace(namespace).Get(ctx, "vault-webhook", metav1.GetOptions{})
			if err == nil && extractConditionStatus(hr, "Ready") == "Ready" {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Second):
		}
	}
}

// CreateVaultReviewerToken creates a long-lived TokenRequest for the vault-webhook service
// account inside the vcluster. Returns the token string and the vcluster's PEM CA certificate
// (for use with Vault's kubernetes_ca_cert field).
func (s *StatusClient) CreateVaultReviewerToken(ctx context.Context, name string, duration time.Duration) (token string, caCert string, err error) {
	// Extract the CA cert from the internal kubeconfig before entering the clientset callback.
	kubeconfigBytes, err := s.getInternalKubeconfig(ctx, name)
	if err != nil {
		return "", "", fmt.Errorf("getting internal kubeconfig: %w", err)
	}
	config, err := clientcmd.Load(kubeconfigBytes)
	if err != nil {
		return "", "", fmt.Errorf("loading kubeconfig for CA extraction: %w", err)
	}
	var ca string
	for _, cluster := range config.Clusters {
		if len(cluster.CertificateAuthorityData) > 0 {
			ca = string(cluster.CertificateAuthorityData)
			break
		}
	}
	if ca == "" {
		return "", "", fmt.Errorf("no CA certificate found in vcluster kubeconfig")
	}

	expSec := int64(duration.Seconds())
	var tok string
	if fnErr := s.withVClusterClientset(ctx, name, func(clientset *k8sclient.Clientset) error {
		tr, err := clientset.CoreV1().ServiceAccounts("vault-system").CreateToken(ctx, "vault-webhook", &authv1.TokenRequest{
			Spec: authv1.TokenRequestSpec{
				ExpirationSeconds: &expSec,
			},
		}, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("creating token: %w", err)
		}
		tok = tr.Status.Token
		return nil
	}); fnErr != nil {
		return "", "", fnErr
	}

	return tok, ca, nil
}

// ListVClusterArgoApps lists ArgoCD Application objects from inside a vcluster.
// Uses internal kubeconfig (same cluster) or port-forward (cross-cluster).
func (s *StatusClient) ListVClusterArgoApps(ctx context.Context, name string) ([]models.ArgoApp, error) {
	var apps []models.ArgoApp
	err := s.withVClusterDynClient(ctx, name, func(dynClient dynamic.Interface) error {
		list, err := dynClient.Resource(argoAppGVR).Namespace("argocd").List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("listing ArgoCD applications: %w", err)
		}
		for _, item := range list.Items {
			appName := item.GetName()
			spec, _ := item.Object["spec"].(map[string]interface{})
			destination, _ := spec["destination"].(map[string]interface{})
			ns, _ := destination["namespace"].(string)
			source := extractArgoSource(spec)
			apps = append(apps, models.ArgoApp{
				Name:         appName,
				Namespace:    ns,
				Project:      stringField(spec, "project"),
				SourcePath:   source["path"],
				SourceBranch: source["targetRevision"],
			})
		}
		return nil
	})
	return apps, err
}

// withVClusterClientset returns a *k8sclient.Clientset connected to the vcluster API and calls fn with it.
// Uses internal kubeconfig directly when in-cluster (same cluster), otherwise port-forward.
func (s *StatusClient) withVClusterClientset(ctx context.Context, name string, fn func(*k8sclient.Clientset) error) error {
	if s.configSource == "in-cluster" {
		kc, err := s.getInternalKubeconfig(ctx, name)
		if err != nil {
			return fmt.Errorf("getting vcluster kubeconfig: %w", err)
		}
		restCfg, err := clientcmd.RESTConfigFromKubeConfig(kc)
		if err != nil {
			return fmt.Errorf("parsing vcluster kubeconfig: %w", err)
		}
		restCfg.Timeout = 30 * time.Second
		cs, err := k8sclient.NewForConfig(restCfg)
		if err != nil {
			return fmt.Errorf("creating clientset: %w", err)
		}
		return fn(cs)
	}
	// Cross-cluster: tunnel via port-forward
	return s.withVClusterPortForward(ctx, name, func(restCfg *rest.Config) error {
		restCfg.Timeout = 30 * time.Second
		cs, err := k8sclient.NewForConfig(restCfg)
		if err != nil {
			return fmt.Errorf("creating clientset: %w", err)
		}
		return fn(cs)
	})
}

// withVClusterDynClient returns a dynamic client connected to the vcluster API and calls fn with it.
// Uses internal kubeconfig directly when in-cluster (same cluster), otherwise port-forward.
func (s *StatusClient) withVClusterDynClient(ctx context.Context, name string, fn func(dynamic.Interface) error) error {
	if s.configSource == "in-cluster" {
		// Same cluster: use internal kubeconfig directly
		kc, err := s.getInternalKubeconfig(ctx, name)
		if err != nil {
			return fmt.Errorf("getting vcluster kubeconfig: %w", err)
		}
		restCfg, err := clientcmd.RESTConfigFromKubeConfig(kc)
		if err != nil {
			return fmt.Errorf("parsing vcluster kubeconfig: %w", err)
		}
		restCfg.Timeout = 15 * time.Second
		dynClient, err := dynamic.NewForConfig(restCfg)
		if err != nil {
			return fmt.Errorf("creating dynamic client: %w", err)
		}
		return fn(dynClient)
	}
	// Cross-cluster: tunnel via port-forward
	return s.withVClusterPortForward(ctx, name, func(restCfg *rest.Config) error {
		restCfg.Timeout = 15 * time.Second
		dynClient, err := dynamic.NewForConfig(restCfg)
		if err != nil {
			return fmt.Errorf("creating dynamic client: %w", err)
		}
		return fn(dynClient)
	})
}

// splitYAMLDocuments splits a multi-document YAML by "---" separators.
func splitYAMLDocuments(data []byte) [][]byte {
	var docs [][]byte
	for _, part := range bytes.Split(data, []byte("\n---")) {
		docs = append(docs, part)
	}
	return docs
}

// resolvedResource holds GVR and whether the resource is namespaced.
type resolvedResource struct {
	GVR        schema.GroupVersionResource
	Namespaced bool
}

// resolveGVR uses the discovery API to find the GroupVersionResource for a given GVK.
func resolveGVR(disc discovery.DiscoveryInterface, gvk schema.GroupVersionKind) (resolvedResource, error) {
	resources, err := disc.ServerResourcesForGroupVersion(gvk.GroupVersion().String())
	if err != nil {
		return resolvedResource{}, fmt.Errorf("discovering resources for %s: %w", gvk.GroupVersion().String(), err)
	}
	for _, r := range resources.APIResources {
		if r.Kind == gvk.Kind && !strings.Contains(r.Name, "/") {
			return resolvedResource{
				GVR: schema.GroupVersionResource{
					Group:    gvk.Group,
					Version:  gvk.Version,
					Resource: r.Name,
				},
				Namespaced: r.Namespaced,
			}, nil
		}
	}
	return resolvedResource{}, fmt.Errorf("resource not found for %s", gvk.String())
}

// applyManifestWithConfig applique un manifest multi-document YAML avec une rest.Config donnée
func applyManifestWithConfig(restCfg *rest.Config, manifestYAML []byte) error {
	dynClient, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("creating dynamic client: %w", err)
	}

	cs, err := k8sclient.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("creating clientset: %w", err)
	}
	discoveryClient := cs.Discovery()

	// Split multi-document YAML
	docs := splitYAMLDocuments(manifestYAML)

	for i, doc := range docs {
		doc = bytes.TrimSpace(doc)
		if len(doc) == 0 {
			continue
		}

		obj := &unstructured.Unstructured{}
		dec := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(doc), 4096)
		if err := dec.Decode(obj); err != nil {
			slog.Warn("skipping manifest document", "index", i, "err", err)
			continue
		}

		if obj.GetKind() == "" {
			continue
		}

		gvk := obj.GroupVersionKind()
		resolved, err := resolveGVR(discoveryClient, gvk)
		if err != nil {
			return fmt.Errorf("resolving GVR for %s: %w", gvk.String(), err)
		}

		// Use namespace only for namespaced resources (not for ClusterRole, ClusterRoleBinding, etc.)
		ns := obj.GetNamespace()
		var client dynamic.ResourceInterface
		if resolved.Namespaced && ns != "" {
			client = dynClient.Resource(resolved.GVR).Namespace(ns)
		} else {
			client = dynClient.Resource(resolved.GVR)
		}

		force := true
		patchOptions := metav1.PatchOptions{
			FieldManager: "vcluster-manager",
			Force:        &force,
		}

		data, err := json.Marshal(obj)
		if err != nil {
			return fmt.Errorf("marshaling object %d: %w", i, err)
		}

		_, err = client.Patch(context.Background(), obj.GetName(), "application/apply-patch+yaml", data, patchOptions)
		if err != nil {
			return fmt.Errorf("applying %s %s/%s: %w", gvk.Kind, ns, obj.GetName(), err)
		}

		slog.Debug("applied manifest object", "kind", gvk.Kind, "namespace", ns, "name", obj.GetName())
	}

	return nil
}

// getFreePort finds an available TCP port on localhost
func getFreePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

// replaceServerURL modifies the server URL in a kubeconfig
func replaceServerURL(kubeconfigBytes []byte, newServerURL string) ([]byte, error) {
	config, err := clientcmd.Load(kubeconfigBytes)
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}

	for name := range config.Clusters {
		config.Clusters[name].Server = newServerURL
	}

	return clientcmd.Write(*config)
}

func extractArgoSource(spec map[string]interface{}) map[string]string {
	out := map[string]string{}
	if src, ok := spec["source"].(map[string]interface{}); ok {
		out["path"] = stringField(src, "path")
		out["targetRevision"] = stringField(src, "targetRevision")
	}
	return out
}

func stringField(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}
