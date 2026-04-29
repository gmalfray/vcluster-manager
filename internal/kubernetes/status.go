package kubernetes

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	k8sclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

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
	namespaceGVR = schema.GroupVersionResource{
		Group:    "",
		Version:  "v1",
		Resource: "namespaces",
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
