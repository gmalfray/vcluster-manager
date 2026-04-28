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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	k8sclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"

	"github.com/gmalfray/vcluster-manager/internal/models"
)

var (
	helmReleaseGVR = schema.GroupVersionResource{
		Group:    "helm.toolkit.fluxcd.io",
		Version:  "v2",
		Resource: "helmreleases",
	}
	kustomizationGVR = schema.GroupVersionResource{
		Group:    "kustomize.toolkit.fluxcd.io",
		Version:  "v1",
		Resource: "kustomizations",
	}
	secretGVR = schema.GroupVersionResource{
		Group:    "",
		Version:  "v1",
		Resource: "secrets",
	}
	resourceQuotaGVR = schema.GroupVersionResource{
		Group:    "",
		Version:  "v1",
		Resource: "resourcequotas",
	}
	veleroBackupGVR = schema.GroupVersionResource{
		Group:    "velero.io",
		Version:  "v1",
		Resource: "backups",
	}
	veleroDownloadRequestGVR = schema.GroupVersionResource{
		Group:    "velero.io",
		Version:  "v1",
		Resource: "downloadrequests",
	}
	veleroRestoreGVR = schema.GroupVersionResource{
		Group:    "velero.io",
		Version:  "v1",
		Resource: "restores",
	}
	statefulSetGVR = schema.GroupVersionResource{
		Group:    "apps",
		Version:  "v1",
		Resource: "statefulsets",
	}
	persistentVolumeClaimGVR = schema.GroupVersionResource{
		Group:    "",
		Version:  "v1",
		Resource: "persistentvolumeclaims",
	}
)

// StatusClient queries the Kubernetes cluster for vcluster status.
type StatusClient struct {
	client       dynamic.Interface
	configSource string // "in-cluster", "kubeconfig", "kubeconfig+ssh"
	kubeconfig   string // path to kubeconfig file (if any)
	sshTarget    string // SSH tunnel target (if any)
	restConfig   *rest.Config
}

// NewStatusClient creates a client from in-cluster config or kubeconfig path.
func NewStatusClient(kubeconfigPath string) (*StatusClient, error) {
	var cfg *rest.Config
	var err error
	var source string

	if kubeconfigPath != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
		source = "kubeconfig"
	} else {
		cfg, err = rest.InClusterConfig()
		source = "in-cluster"
	}
	if err != nil {
		return nil, fmt.Errorf("building k8s config: %w", err)
	}

	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating dynamic client: %w", err)
	}

	return &StatusClient{
		client:       client,
		configSource: source,
		kubeconfig:   kubeconfigPath,
		restConfig:   cfg,
	}, nil
}

// NewStatusClientWithTunnel creates a client that connects through an SSH tunnel.
// It rewrites the kubeconfig's server address to go through the local tunnel endpoint.
func NewStatusClientWithTunnel(kubeconfigPath, sshTarget, sshKeyPath string) (*StatusClient, *SSHTunnel, error) {
	// Load kubeconfig to find the API server address
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, nil, fmt.Errorf("building k8s config: %w", err)
	}

	// Parse the remote API server address from kubeconfig
	remoteAddr := cfg.Host
	if !strings.Contains(remoteAddr, ":") {
		remoteAddr += ":443"
	}
	// Strip scheme if present
	remoteAddr = strings.TrimPrefix(remoteAddr, "https://")
	remoteAddr = strings.TrimPrefix(remoteAddr, "http://")

	// Create SSH tunnel
	tunnel, err := NewSSHTunnel(sshTarget, sshKeyPath, remoteAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("creating SSH tunnel: %w", err)
	}

	// Rewrite the config to use the local tunnel endpoint
	cfg.Host = "https://" + tunnel.LocalAddr()
	// Since we're going through a tunnel, the TLS cert won't match the local address
	cfg.TLSClientConfig.Insecure = true
	cfg.TLSClientConfig.CAData = nil
	cfg.TLSClientConfig.CAFile = ""

	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		_ = tunnel.Close()
		return nil, nil, fmt.Errorf("creating dynamic client: %w", err)
	}

	return &StatusClient{
		client:       client,
		configSource: "kubeconfig+ssh",
		kubeconfig:   kubeconfigPath,
		sshTarget:    sshTarget,
		restConfig:   cfg,
	}, tunnel, nil
}

// TestConnection verifies connectivity to the Kubernetes API server.
func (s *StatusClient) TestConnection(ctx context.Context) error {
	// Use the discovery API (server version) which requires no RBAC permissions
	cs, err := k8sclient.NewForConfig(s.restConfig)
	if err != nil {
		return fmt.Errorf("connection test failed: %w", err)
	}
	_, err = cs.Discovery().ServerVersion()
	if err != nil {
		return fmt.Errorf("connection test failed: %w", err)
	}
	return nil
}

// ConfigSource returns how this client is configured ("in-cluster", "kubeconfig", "kubeconfig+ssh").
func (s *StatusClient) ConfigSource() string {
	return s.configSource
}

// KubeconfigPath returns the path of the kubeconfig file used, if any.
func (s *StatusClient) KubeconfigPath() string {
	return s.kubeconfig
}

// SSHTarget returns the SSH tunnel target, if any.
func (s *StatusClient) SSHTarget() string {
	return s.sshTarget
}

// APIServerURL returns the API server URL from the REST config.
func (s *StatusClient) APIServerURL() string {
	if s.restConfig != nil {
		return s.restConfig.Host
	}
	return ""
}

// GetVClusterStatus returns status info for a specific vcluster.
func (s *StatusClient) GetVClusterStatus(ctx context.Context, name string) (*models.StatusInfo, error) {
	namespace := "vcluster-" + name
	info := &models.StatusInfo{
		HelmRelease:       "Unknown",
		FluxKustomization: "Unknown",
	}

	// HelmRelease status
	hr, err := s.client.Resource(helmReleaseGVR).Namespace(namespace).Get(ctx, "vcluster-"+name, metav1.GetOptions{})
	if err == nil {
		info.HelmRelease = extractConditionStatus(hr, "Ready")
		info.ChartVersion = extractChartVersion(hr)
	}

	// Flux Kustomization status (in flux-system namespace)
	ks, err := s.client.Resource(kustomizationGVR).Namespace("flux-system").Get(ctx, "tenant-"+name, metav1.GetOptions{})
	if err == nil {
		info.FluxKustomization = extractConditionStatus(ks, "Ready")
	}

	// K8s version inside the vcluster (best effort, non-blocking — errors are expected when unreachable)
	k8sVersion, err := s.getK8sVersion(ctx, name)
	if err == nil {
		info.K8sVersion = k8sVersion
	}

	// ResourceQuota usage (best effort, non-blocking)
	s.populateQuotaUsage(ctx, namespace, info)

	return info, nil
}

// populateQuotaUsage reads ResourceQuota objects from the namespace and populates usage fields.
func (s *StatusClient) populateQuotaUsage(ctx context.Context, namespace string, info *models.StatusInfo) {
	rqList, err := s.client.Resource(resourceQuotaGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		slog.Warn("could not list ResourceQuotas", "namespace", namespace, "err", err)
		return
	}

	if len(rqList.Items) == 0 {
		return
	}

	// Aggregate across all ResourceQuota objects in the namespace
	for _, rq := range rqList.Items {
		hard, _, _ := unstructured.NestedStringMap(rq.Object, "status", "hard")
		used, _, _ := unstructured.NestedStringMap(rq.Object, "status", "used")

		if cpu, ok := hard["requests.cpu"]; ok {
			usedCPU := used["requests.cpu"]
			info.CPUUsage = formatQuantityPair(usedCPU, cpu)
			info.CPUPercent = calcPercent(usedCPU, cpu)
		}
		if mem, ok := hard["requests.memory"]; ok {
			usedMem := used["requests.memory"]
			info.MemoryUsage = formatQuantityPair(usedMem, mem)
			info.MemoryPercent = calcPercent(usedMem, mem)
		}
		if stor, ok := hard["requests.storage"]; ok {
			usedStor := used["requests.storage"]
			info.StorageUsage = formatQuantityPair(usedStor, stor)
			info.StoragePercent = calcPercent(usedStor, stor)
		}
	}
}

// formatQuantityPair formats "used/limit" with human-readable units in the same scale.
func formatQuantityPair(usedStr, limitStr string) string {
	limit, err := resource.ParseQuantity(limitStr)
	if err != nil {
		return formatQuantity(usedStr) + "/" + formatQuantity(limitStr)
	}
	used, err := resource.ParseQuantity(usedStr)
	if err != nil {
		return "0/" + formatQuantity(limitStr)
	}
	// Format both in the same unit for readability
	if limit.Format == resource.DecimalSI {
		return formatCPU(used) + "/" + formatCPU(limit)
	}
	return formatBinarySI(used, limit) + "/" + formatBinarySI(limit, limit)
}

// formatQuantity formats a single K8s quantity string for display.
func formatQuantity(s string) string {
	if s == "" {
		return "0"
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return s
	}
	if q.Format == resource.DecimalSI {
		return formatCPU(q)
	}
	return q.String()
}

// formatCPU formats CPU quantities as cores (decimal), e.g. "4.8" or "24".
func formatCPU(q resource.Quantity) string {
	milli := q.MilliValue()
	if milli%1000 == 0 {
		return fmt.Sprintf("%d", milli/1000)
	}
	return fmt.Sprintf("%.1f", float64(milli)/1000)
}

// formatBinarySI formats memory/storage in the same unit as the reference quantity.
// This ensures "used" and "limit" display in the same unit (e.g. both in Gi).
func formatBinarySI(q resource.Quantity, ref resource.Quantity) string {
	const (
		ki = 1024
		mi = ki * 1024
		gi = mi * 1024
		ti = gi * 1024
	)
	refBytes := ref.Value()
	bytes := q.Value()

	switch {
	case refBytes >= ti:
		v := float64(bytes) / float64(ti)
		if v == float64(int64(v)) {
			return fmt.Sprintf("%dTi", int64(v))
		}
		return fmt.Sprintf("%.1fTi", v)
	case refBytes >= gi:
		v := float64(bytes) / float64(gi)
		if v == float64(int64(v)) {
			return fmt.Sprintf("%dGi", int64(v))
		}
		return fmt.Sprintf("%.1fGi", v)
	case refBytes >= mi:
		v := float64(bytes) / float64(mi)
		if v == float64(int64(v)) {
			return fmt.Sprintf("%dMi", int64(v))
		}
		return fmt.Sprintf("%.0fMi", v)
	default:
		return q.String()
	}
}

// calcPercent returns the usage percentage (0-100).
func calcPercent(usedStr, limitStr string) int {
	if usedStr == "" || limitStr == "" {
		return 0
	}
	used, err := resource.ParseQuantity(usedStr)
	if err != nil {
		return 0
	}
	limit, err := resource.ParseQuantity(limitStr)
	if err != nil {
		return 0
	}
	if limit.IsZero() {
		return 0
	}
	// Use MilliValue for precision (works for all resource types)
	return int(used.MilliValue() * 100 / limit.MilliValue())
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

// extractChartVersion reads the chart version from HelmRelease status.history[0].chartVersion.
func extractChartVersion(hr *unstructured.Unstructured) string {
	history, found, err := unstructured.NestedSlice(hr.Object, "status", "history")
	if err != nil || !found || len(history) == 0 {
		return ""
	}
	entry, ok := history[0].(map[string]interface{})
	if !ok {
		return ""
	}
	version, _ := entry["chartVersion"].(string)
	return version
}

// CountReadyHelmReleases counts HelmReleases in vcluster-* namespaces (total and ready).
func (s *StatusClient) CountReadyHelmReleases(ctx context.Context) (total, ready int, err error) {
	list, err := s.client.Resource(helmReleaseGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, 0, err
	}
	for _, item := range list.Items {
		if !strings.HasPrefix(item.GetNamespace(), "vcluster-") {
			continue
		}
		total++
		if extractConditionStatus(&item, "Ready") == "Ready" {
			ready++
		}
	}
	return total, ready, nil
}

// HasRancherAgents checks whether Rancher agents are running inside a vcluster.
// vcluster syncs pods from inside the virtual cluster to the host namespace (vcluster-{name})
// with a label indicating their original namespace. We look for pods from cattle-system,
// which is the namespace Rancher agents run in — this detects pairings regardless of
// how the cluster was named in Rancher.
func (s *StatusClient) HasRancherAgents(ctx context.Context, name string) bool {
	namespace := "vcluster-" + name
	podGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}

	// vcluster labels synced pods with their original namespace.
	// Try both label keys used by different vcluster versions.
	for _, labelSelector := range []string{
		"vcluster.loft.sh/namespace=cattle-system",
		"vcluster.loft.sh/object-namespace=cattle-system",
	} {
		list, err := s.client.Resource(podGVR).Namespace(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
			Limit:         1,
		})
		if err == nil && len(list.Items) > 0 {
			return true
		}
	}
	return false
}

// HelmReleaseExists checks if the HelmRelease for a vcluster still exists in K8s.
func (s *StatusClient) HelmReleaseExists(ctx context.Context, name string) bool {
	namespace := "vcluster-" + name
	_, err := s.client.Resource(helmReleaseGVR).Namespace(namespace).Get(ctx, "vcluster-"+name, metav1.GetOptions{})
	return err == nil
}

// CleanupNamespace removes FluxCD finalizers from all Kustomizations and HelmReleases
// in the vcluster namespace, so the namespace can be deleted cleanly.
func (s *StatusClient) CleanupNamespace(ctx context.Context, name string) error {
	namespace := "vcluster-" + name

	// Remove finalizers from all Flux Kustomizations in the namespace
	ksList, err := s.client.Resource(kustomizationGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, ks := range ksList.Items {
			if len(ks.GetFinalizers()) > 0 {
				ks.SetFinalizers(nil)
				if _, err := s.client.Resource(kustomizationGVR).Namespace(namespace).Update(ctx, &ks, metav1.UpdateOptions{}); err != nil {
					return fmt.Errorf("removing finalizers from kustomization %s: %w", ks.GetName(), err)
				}
			}
		}
	}

	// Remove finalizers from all HelmReleases in the namespace
	hrList, err := s.client.Resource(helmReleaseGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, hr := range hrList.Items {
			if len(hr.GetFinalizers()) > 0 {
				hr.SetFinalizers(nil)
				if _, err := s.client.Resource(helmReleaseGVR).Namespace(namespace).Update(ctx, &hr, metav1.UpdateOptions{}); err != nil {
					return fmt.Errorf("removing finalizers from helmrelease %s: %w", hr.GetName(), err)
				}
			}
		}
	}

	return nil
}

var namespaceGVR = schema.GroupVersionResource{
	Group:    "",
	Version:  "v1",
	Resource: "namespaces",
}

// GetNamespaceProtection returns true if the vcluster host namespace has the protect-deletion annotation.
func (s *StatusClient) GetNamespaceProtection(ctx context.Context, name string) bool {
	ns, err := s.client.Resource(namespaceGVR).Get(ctx, "vcluster-"+name, metav1.GetOptions{})
	if err != nil {
		return false
	}
	return ns.GetAnnotations()["protect-deletion"] == "true"
}

// SetNamespaceProtection adds or removes the protect-deletion annotation on the vcluster host namespace.
func (s *StatusClient) SetNamespaceProtection(ctx context.Context, name string, enabled bool) error {
	ns, err := s.client.Resource(namespaceGVR).Get(ctx, "vcluster-"+name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting namespace vcluster-%s: %w", name, err)
	}
	annotations := ns.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	if enabled {
		annotations["protect-deletion"] = "true"
	} else {
		delete(annotations, "protect-deletion")
	}
	ns.SetAnnotations(annotations)
	_, err = s.client.Resource(namespaceGVR).Update(ctx, ns, metav1.UpdateOptions{})
	return err
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

// ApplyManifestToVClusterViaPortForward applies a manifest to a vcluster on a remote cluster
// by creating a temporary port-forward to the vcluster pod using client-go API.
// This is used when the vcluster is on a different cluster than where vcluster-manager runs.
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

	// Modifier l'URL du serveur dans tous les clusters
	for name := range config.Clusters {
		config.Clusters[name].Server = newServerURL
	}

	return clientcmd.Write(*config)
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
		dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(doc), 4096)
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

// ListVeleroBackups returns all Velero backups targeting the vcluster-{name} namespace.
func (s *StatusClient) ListVeleroBackups(ctx context.Context, name, veleroNamespace string) ([]models.VeleroBackupInfo, error) {
	targetNS := "vcluster-" + name
	list, err := s.client.Resource(veleroBackupGVR).Namespace(veleroNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing velero backups: %w", err)
	}

	var result []models.VeleroBackupInfo
	for _, item := range list.Items {
		// Filter: spec.includedNamespaces must contain vcluster-{name}
		includedNS, _, _ := unstructured.NestedStringSlice(item.Object, "spec", "includedNamespaces")
		found := false
		for _, ns := range includedNS {
			if ns == targetNS || ns == "*" {
				found = true
				break
			}
		}
		if !found {
			continue
		}

		phase, _, _ := unstructured.NestedString(item.Object, "status", "phase")
		startTime, _, _ := unstructured.NestedString(item.Object, "status", "startTimestamp")
		completionTime, _, _ := unstructured.NestedString(item.Object, "status", "completionTimestamp")
		itemsBackedUp, _, _ := unstructured.NestedInt64(item.Object, "status", "progress", "itemsBackedUp")
		totalItems, _, _ := unstructured.NestedInt64(item.Object, "status", "progress", "totalItems")
		ttl, _, _ := unstructured.NestedString(item.Object, "spec", "ttl")

		result = append(result, models.VeleroBackupInfo{
			Name:           item.GetName(),
			Phase:          phase,
			StartTime:      startTime,
			CompletionTime: completionTime,
			ItemsBackedUp:  int(itemsBackedUp),
			TotalItems:     int(totalItems),
			Namespace:      targetNS,
			TTL:            ttl,
		})
	}
	return result, nil
}

// ListActiveVeleroRestores returns Velero Restore objects for a vcluster that are not yet terminal (InProgress, New, WaitingForPluginOperations, etc.).
func (s *StatusClient) ListActiveVeleroRestores(ctx context.Context, name, veleroNamespace string) ([]models.VeleroRestoreInfo, error) {
	targetNS := "vcluster-" + name
	list, err := s.client.Resource(veleroRestoreGVR).Namespace(veleroNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing velero restores: %w", err)
	}

	terminal := map[string]bool{"Completed": true, "Failed": true, "PartiallyFailed": true}
	var result []models.VeleroRestoreInfo
	for _, item := range list.Items {
		phase, _, _ := unstructured.NestedString(item.Object, "status", "phase")
		if terminal[phase] {
			continue
		}
		// Only keep restores targeting this vcluster (includedNamespaces or namespaceMapping source)
		includedNS, _, _ := unstructured.NestedStringSlice(item.Object, "spec", "includedNamespaces")
		found := false
		for _, ns := range includedNS {
			if ns == targetNS {
				found = true
				break
			}
		}
		if !found {
			continue
		}
		result = append(result, models.VeleroRestoreInfo{
			Name:  item.GetName(),
			Phase: phase,
		})
	}
	return result, nil
}

// GetBackupContentURL creates a DownloadRequest for a backup's resource list and returns the presigned URL.
func (s *StatusClient) GetBackupContentURL(ctx context.Context, backupName, veleroNamespace string) (string, error) {
	drName := fmt.Sprintf("vcluster-manager-%s-%d", backupName, time.Now().UnixNano()/int64(time.Millisecond))
	dr := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "velero.io/v1",
			"kind":       "DownloadRequest",
			"metadata": map[string]interface{}{
				"name":      drName,
				"namespace": veleroNamespace,
			},
			"spec": map[string]interface{}{
				"target": map[string]interface{}{
					"kind": "BackupResourceList",
					"name": backupName,
				},
			},
		},
	}

	created, err := s.client.Resource(veleroDownloadRequestGVR).Namespace(veleroNamespace).Create(ctx, dr, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("creating download request: %w", err)
	}
	drName = created.GetName()

	// Clean up the DownloadRequest when done (best-effort)
	defer s.client.Resource(veleroDownloadRequestGVR).Namespace(veleroNamespace).Delete(context.Background(), drName, metav1.DeleteOptions{}) //nolint:errcheck

	// Poll for Processed phase (up to 30s)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		obj, err := s.client.Resource(veleroDownloadRequestGVR).Namespace(veleroNamespace).Get(ctx, drName, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("getting download request: %w", err)
		}
		phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
		if phase == "Processed" {
			url, _, _ := unstructured.NestedString(obj.Object, "status", "downloadURL")
			if url == "" {
				return "", fmt.Errorf("download request processed but downloadURL is empty")
			}
			return url, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return "", fmt.Errorf("timeout waiting for download request to be processed")
}

// CreateVeleroRestore creates a Velero Restore for backupName targeting targetNS (may differ from sourceNS for cross-vcluster restores).
func (s *StatusClient) CreateVeleroRestore(ctx context.Context, backupName, sourceNS, targetNS, veleroNamespace string) (string, error) {
	restoreName := fmt.Sprintf("vm-%s-%d", backupName, time.Now().UnixNano()/int64(time.Millisecond))
	// Truncate to 63 chars (K8s name limit)
	if len(restoreName) > 63 {
		restoreName = restoreName[:63]
	}

	spec := map[string]interface{}{
		"backupName":             backupName,
		"includedNamespaces":     []interface{}{sourceNS},
		"existingResourcePolicy": "update",
	}
	if targetNS != "" && targetNS != sourceNS {
		spec["namespaceMapping"] = map[string]interface{}{
			sourceNS: targetNS,
		}
	}

	restore := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "velero.io/v1",
			"kind":       "Restore",
			"metadata": map[string]interface{}{
				"name":      restoreName,
				"namespace": veleroNamespace,
			},
			"spec": spec,
		},
	}

	created, err := s.client.Resource(veleroRestoreGVR).Namespace(veleroNamespace).Create(ctx, restore, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("creating restore: %w", err)
	}
	return created.GetName(), nil
}

// CreateVeleroBackup creates an on-demand Velero Backup for a given vcluster namespace.
func (s *StatusClient) CreateVeleroBackup(ctx context.Context, vcName, veleroNamespace, ttl, storageLocation string) (string, error) {
	backupName := fmt.Sprintf("manual-%s-%d", vcName, time.Now().UnixNano()/int64(time.Millisecond))
	if len(backupName) > 63 {
		backupName = backupName[:63]
	}
	if storageLocation == "" {
		storageLocation = "default"
	}
	if ttl == "" {
		ttl = "720h0m0s"
	}
	ns := "vcluster-" + vcName
	backup := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "velero.io/v1",
			"kind":       "Backup",
			"metadata": map[string]interface{}{
				"name":      backupName,
				"namespace": veleroNamespace,
			},
			"spec": map[string]interface{}{
				"includedNamespaces":       []interface{}{ns},
				"defaultVolumesToFsBackup": true,
				"snapshotVolumes":          false,
				"storageLocation":          storageLocation,
				"ttl":                      ttl,
				// Pods and replicasets are ephemeral and synced by vcluster — they are recreated
				// automatically when the vcluster starts. Including them causes Velero to inject
				// restore-wait init containers which fail when pods have runAsNonRoot security
				// contexts (velero image uses non-numeric user "cnb"). Only the PVCs matter.
				"excludedResources": []interface{}{
					"events", "leases",
					"pods", "replicasets.apps",
				},
			},
		},
	}

	created, err := s.client.Resource(veleroBackupGVR).Namespace(veleroNamespace).Create(ctx, backup, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("creating backup: %w", err)
	}
	return created.GetName(), nil
}

// GetRestoreStatus returns the phase of a Velero Restore object.
func (s *StatusClient) GetRestoreStatus(ctx context.Context, restoreName, veleroNamespace string) (string, error) {
	obj, err := s.client.Resource(veleroRestoreGVR).Namespace(veleroNamespace).Get(ctx, restoreName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("getting restore %s: %w", restoreName, err)
	}
	phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
	if phase == "" {
		phase = "New"
	}
	return phase, nil
}

// SetFluxSuspend suspends or resumes the HelmRelease and Kustomization for a vcluster.
// This is used before/after a Velero in-place restore to prevent Flux from fighting the restore.
func (s *StatusClient) SetFluxSuspend(ctx context.Context, name string, suspend bool) error {
	patch, err := json.Marshal(map[string]interface{}{
		"spec": map[string]interface{}{"suspend": suspend},
	})
	if err != nil {
		return err
	}

	// Patch HelmRelease vcluster-{name} in namespace vcluster-{name}
	ns := "vcluster-" + name
	if _, err := s.client.Resource(helmReleaseGVR).Namespace(ns).Patch(
		ctx, "vcluster-"+name, k8stypes.MergePatchType, patch, metav1.PatchOptions{},
	); err != nil {
		return fmt.Errorf("patching helmrelease %s: %w", name, err)
	}

	// Patch Kustomization tenant-{name} in flux-system
	if _, err := s.client.Resource(kustomizationGVR).Namespace("flux-system").Patch(
		ctx, "tenant-"+name, k8stypes.MergePatchType, patch, metav1.PatchOptions{},
	); err != nil {
		return fmt.Errorf("patching kustomization tenant-%s: %w", name, err)
	}

	return nil
}

// ScaleVClusterStatefulSet scales the vcluster StatefulSet to the given number of replicas.
// Used to quiesce the vcluster before an in-place Velero restore so the PVC is released.
func (s *StatusClient) ScaleVClusterStatefulSet(ctx context.Context, name string, replicas int32) error {
	patch, err := json.Marshal(map[string]interface{}{
		"spec": map[string]interface{}{"replicas": replicas},
	})
	if err != nil {
		return err
	}
	ns := "vcluster-" + name
	_, err = s.client.Resource(statefulSetGVR).Namespace(ns).Patch(
		ctx, "vcluster-"+name, k8stypes.MergePatchType, patch, metav1.PatchOptions{},
	)
	return err
}

// DeleteVClusterPVC deletes the main PVC of a vcluster so Velero can restore it from backup.
// The StatefulSet must be scaled to 0 first.
func (s *StatusClient) DeleteVClusterPVC(ctx context.Context, name string) error {
	ns := "vcluster-" + name
	pvcName := "data-vcluster-" + name + "-0"
	err := s.client.Resource(persistentVolumeClaimGVR).Namespace(ns).Delete(ctx, pvcName, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("deleting PVC %s: %w", pvcName, err)
	}
	return nil
}

// DeleteVeleroBackup deletes a Velero Backup object by name.
func (s *StatusClient) DeleteVeleroBackup(ctx context.Context, backupName, veleroNamespace string) error {
	err := s.client.Resource(veleroBackupGVR).Namespace(veleroNamespace).Delete(ctx, backupName, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("deleting velero backup %s: %w", backupName, err)
	}
	return nil
}

func extractConditionStatus(obj *unstructured.Unstructured, condType string) string {
	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return "Unknown"
	}
	for _, c := range conditions {
		cond, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _ := cond["type"].(string); t == condType {
			if status, _ := cond["status"].(string); status == "True" {
				return "Ready"
			}
			if reason, _ := cond["reason"].(string); reason != "" {
				return reason
			}
			return "NotReady"
		}
	}
	return "Unknown"
}

var argoAppGVR = schema.GroupVersionResource{
	Group:    "argoproj.io",
	Version:  "v1alpha1",
	Resource: "applications",
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

// vclusterDynClient returns a dynamic client connected to the vcluster API and calls fn with it.
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
