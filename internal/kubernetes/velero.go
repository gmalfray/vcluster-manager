package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/gmalfray/vcluster-manager/internal/models"
)

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

// ListActiveVeleroRestores returns Velero Restore objects for a vcluster that are not yet terminal.
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

// GetVeleroBackupPhase returns the phase of a single Velero backup (e.g.
// "Completed", "Failed", "InProgress"). Used to confirm a backup is restorable
// BEFORE a destructive in-place restore (which deletes the vcluster PVC).
func (s *StatusClient) GetVeleroBackupPhase(ctx context.Context, backupName, veleroNamespace string) (string, error) {
	obj, err := s.client.Resource(veleroBackupGVR).Namespace(veleroNamespace).Get(ctx, backupName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
	return phase, nil
}

// VClusterTopology describes how a vcluster's control-plane and data store
// are laid out in the host cluster. A vcluster can run with etcd embedded in
// its own StatefulSet, or with etcd split out into its own StatefulSet and
// the control-plane running as a Deployment — an in-place restore needs to
// know which one it's dealing with to scale down the right workloads and
// delete the right PVC.
type VClusterTopology struct {
	// ControlPlaneKind is "Deployment" or "StatefulSet".
	ControlPlaneKind string
	// EtcdStatefulSet is the etcd StatefulSet's name, empty when etcd is
	// embedded in the control-plane StatefulSet itself.
	EtcdStatefulSet string
	// PVCName is the data volume Velero needs to recreate from backup.
	PVCName string
}

// detectVClusterTopology looks at what's actually deployed for a vcluster
// rather than assuming a mode, since both embedded and external etcd are
// valid vcluster configurations:
//   - external etcd: control-plane = Deployment `vcluster-<name>` (or, on
//     older charts, still a StatefulSet), etcd = StatefulSet
//     `vcluster-<name>-etcd`, data in `data-vcluster-<name>-etcd-0`.
//   - embedded etcd: control-plane = StatefulSet `vcluster-<name>`, data in
//     `data-vcluster-<name>-0`.
func (s *StatusClient) detectVClusterTopology(ctx context.Context, name string) (VClusterTopology, error) {
	ns := "vcluster-" + name
	etcdName := "vcluster-" + name + "-etcd"

	_, err := s.client.Resource(statefulSetGVR).Namespace(ns).Get(ctx, etcdName, metav1.GetOptions{})
	switch {
	case err == nil:
		kind := "Deployment"
		if _, err := s.client.Resource(statefulSetGVR).Namespace(ns).Get(ctx, "vcluster-"+name, metav1.GetOptions{}); err == nil {
			kind = "StatefulSet"
		}
		return VClusterTopology{
			ControlPlaneKind: kind,
			EtcdStatefulSet:  etcdName,
			PVCName:          "data-" + etcdName + "-0",
		}, nil
	case apierrors.IsNotFound(err):
		return VClusterTopology{
			ControlPlaneKind: "StatefulSet",
			PVCName:          "data-vcluster-" + name + "-0",
		}, nil
	default:
		return VClusterTopology{}, fmt.Errorf("checking etcd statefulset %s: %w", etcdName, err)
	}
}

// controlPlaneGVR returns the GVR to use for the control-plane workload
// according to the detected topology.
func controlPlaneGVR(topo VClusterTopology) schema.GroupVersionResource {
	if topo.ControlPlaneKind == "Deployment" {
		return deploymentGVR
	}
	return statefulSetGVR
}

// waitForWorkloadPodsGone waits until no pod matching workloadName's own
// selector remains in ns. It reads the selector from the workload itself
// (spec.selector.matchLabels) instead of guessing pod names, so it works the
// same whether the workload is a Deployment (hashed pod names) or a
// StatefulSet (fixed pod names). A workload that's already gone counts as
// "pods gone" too — nothing left to wait for.
func (s *StatusClient) waitForWorkloadPodsGone(ctx context.Context, gvr schema.GroupVersionResource, ns, workloadName string, timeout time.Duration) error {
	obj, err := s.client.Resource(gvr).Namespace(ns).Get(ctx, workloadName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting %s/%s: %w", gvr.Resource, workloadName, err)
	}
	matchLabels, _, _ := unstructured.NestedStringMap(obj.Object, "spec", "selector", "matchLabels")
	if len(matchLabels) == 0 {
		// An empty selector would list every pod in the namespace, not just
		// this workload's — nothing sane to wait on, so treat it like "already
		// gone" instead of watching unrelated pods.
		slog.Warn("workload has no selector labels, skipping the pod wait", "namespace", ns, "workload", workloadName)
		return nil
	}
	selector := labels.SelectorFromSet(matchLabels).String()

	deadline := time.After(timeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		list, err := s.client.Resource(podGVR).Namespace(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return fmt.Errorf("listing pods for %s: %w", workloadName, err)
		}
		if len(list.Items) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("%d pod(s) for %s/%s still there after %s", len(list.Items), ns, workloadName, timeout)
		case <-ticker.C:
		}
	}
}

// WaitForVClusterPodsGone waits until the vcluster's control-plane pod(s) —
// and, with external etcd, its etcd pod(s) too — are really gone, which is
// what actually releases the PVC(s). Beats a fixed sleep: we return as soon
// as the pods disappear, and we don't delete a PVC that's still mounted
// (which would leave it stuck Terminating).
func (s *StatusClient) WaitForVClusterPodsGone(ctx context.Context, name string, timeout time.Duration) error {
	topo, err := s.detectVClusterTopology(ctx, name)
	if err != nil {
		return fmt.Errorf("detecting vcluster topology: %w", err)
	}
	ns := "vcluster-" + name

	if err := s.waitForWorkloadPodsGone(ctx, controlPlaneGVR(topo), ns, "vcluster-"+name, timeout); err != nil {
		return fmt.Errorf("waiting for control-plane pod(s): %w", err)
	}
	if topo.EtcdStatefulSet != "" {
		if err := s.waitForWorkloadPodsGone(ctx, statefulSetGVR, ns, topo.EtcdStatefulSet, timeout); err != nil {
			return fmt.Errorf("waiting for etcd pod(s): %w", err)
		}
	}
	return nil
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

// DeleteVeleroBackup deletes a Velero Backup object by name.
func (s *StatusClient) DeleteVeleroBackup(ctx context.Context, backupName, veleroNamespace string) error {
	err := s.client.Resource(veleroBackupGVR).Namespace(veleroNamespace).Delete(ctx, backupName, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("deleting velero backup %s: %w", backupName, err)
	}
	return nil
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

// ScaleVClusterWorkloads scales the vcluster's control-plane (Deployment or
// StatefulSet, whichever is actually deployed) to the given number of
// replicas, and its etcd StatefulSet too when etcd runs external. Used to
// quiesce the vcluster before an in-place Velero restore so the PVC(s) are
// released.
func (s *StatusClient) ScaleVClusterWorkloads(ctx context.Context, name string, replicas int32) error {
	topo, err := s.detectVClusterTopology(ctx, name)
	if err != nil {
		return fmt.Errorf("detecting vcluster topology: %w", err)
	}
	patch, err := json.Marshal(map[string]interface{}{
		"spec": map[string]interface{}{"replicas": replicas},
	})
	if err != nil {
		return err
	}
	ns := "vcluster-" + name

	if _, err := s.client.Resource(controlPlaneGVR(topo)).Namespace(ns).Patch(
		ctx, "vcluster-"+name, k8stypes.MergePatchType, patch, metav1.PatchOptions{},
	); err != nil {
		return fmt.Errorf("scaling %s vcluster-%s: %w", topo.ControlPlaneKind, name, err)
	}

	if topo.EtcdStatefulSet != "" {
		if _, err := s.client.Resource(statefulSetGVR).Namespace(ns).Patch(
			ctx, topo.EtcdStatefulSet, k8stypes.MergePatchType, patch, metav1.PatchOptions{},
		); err != nil {
			return fmt.Errorf("scaling statefulset %s: %w", topo.EtcdStatefulSet, err)
		}
	}
	return nil
}

// RequestVeleroOps deposits a backup/restore order as annotations on the
// vcluster's VClusterVeleroOps marker, creating the marker if it does not exist
// yet.
//
// Merge patch and not Server-Side Apply: the fake dynamic client used by the
// tests cannot Apply. The consequence matters for callers — a merge patch never
// REMOVES a key, so an annotation that must not persist across requests has to
// be rewritten explicitly, empty string included.
//
// DeleteVClusterPVC deletes the vcluster's data PVC so Velero can restore it
// from backup — `data-vcluster-<name>-etcd-0` with external etcd,
// `data-vcluster-<name>-0` when it's embedded. The workload(s) must be
// scaled to 0 first, or the PVC stays stuck Terminating.
// RequestVeleroOps posts a trigger annotation on a vcluster's VClusterVeleroOps
// marker, creating the marker if it does not exist yet.
//
// Lazy creation, rather than creating a marker alongside every vcluster, keeps
// Service.Create untouched and means an existing vcluster gets one the first time
// someone asks for a backup.
//
// A merge patch on the annotations, not a Server-Side Apply: the app only ever
// writes annotations and the operator only ever writes status, so there is no
// shared field for two managers to arbitrate — and unlike SSA, this is
// exercisable against the fake dynamic client the rest of the package tests
// with. If the marker does not exist yet, create it; if two requests race to
// create it, the loser patches instead.
func (s *StatusClient) RequestVeleroOps(ctx context.Context, name string, annotations map[string]string) error {
	ns := "vcluster-" + name
	res := s.client.Resource(veleroOpsGVR).Namespace(ns)

	patch, err := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{"annotations": annotations},
	})
	if err != nil {
		return fmt.Errorf("building the annotation patch: %w", err)
	}

	_, err = res.Patch(ctx, name, k8stypes.MergePatchType, patch, metav1.PatchOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("annotating the marker %s: %w", name, err)
	}

	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "vcluster.rebuild-it.fr/v1alpha1",
		"kind":       "VClusterVeleroOps",
		"metadata": map[string]interface{}{
			"name":        name,
			"namespace":   ns,
			"annotations": toStringMap(annotations),
		},
	}}
	if _, err := res.Create(ctx, obj, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating the marker %s: %w", name, err)
		}
		if _, err := res.Patch(ctx, name, k8stypes.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
			return fmt.Errorf("annotating the marker %s after a concurrent create: %w", name, err)
		}
	}
	return nil
}

// toStringMap converts a map[string]string to the map[string]interface{} an
// unstructured object needs.
func toStringMap(in map[string]string) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// VeleroOpsRestoreState is the restore half of a VClusterVeleroOps marker's
// status, as written by the operator.
type VeleroOpsRestoreState struct {
	Found           bool
	RestoreName     string
	Phase           string
	FromBackup      string
	InPlace         bool
	ResumePending   bool
	ResumeFailed    bool
	ResumeError     string
	VolumeDestroyed bool
}

// GetVeleroOpsRestoreState reads the restore status the operator publishes on a
// vcluster's marker. Found is false when no marker exists yet, which is the
// normal state of a vcluster nobody has ever asked a backup for.
func (s *StatusClient) GetVeleroOpsRestoreState(ctx context.Context, name string) (VeleroOpsRestoreState, error) {
	obj, err := s.client.Resource(veleroOpsGVR).Namespace("vcluster-"+name).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return VeleroOpsRestoreState{}, nil
	}
	if err != nil {
		return VeleroOpsRestoreState{}, fmt.Errorf("reading the marker %s: %w", name, err)
	}

	restoreName, _, _ := unstructured.NestedString(obj.Object, "status", "restore", "restoreName")
	phase, _, _ := unstructured.NestedString(obj.Object, "status", "restore", "phase")
	fromBackup, _, _ := unstructured.NestedString(obj.Object, "status", "restore", "fromBackup")
	inPlace, _, _ := unstructured.NestedBool(obj.Object, "status", "restore", "inPlace")
	resumePending, _, _ := unstructured.NestedBool(obj.Object, "status", "restore", "resumePending")
	resumeFailed, _, _ := unstructured.NestedBool(obj.Object, "status", "restore", "resumeFailed")
	resumeError, _, _ := unstructured.NestedString(obj.Object, "status", "restore", "resumeError")
	volumeDestroyed, _, _ := unstructured.NestedBool(obj.Object, "status", "restore", "volumeDestroyed")

	return VeleroOpsRestoreState{
		Found:           true,
		RestoreName:     restoreName,
		Phase:           phase,
		FromBackup:      fromBackup,
		InPlace:         inPlace,
		ResumePending:   resumePending,
		ResumeFailed:    resumeFailed,
		ResumeError:     resumeError,
		VolumeDestroyed: volumeDestroyed,
	}, nil
}

// GetVClusterPVCState reports whether a vcluster's data volume is still there.
//
// This is how a caller that was interrupted mid-restore finds out which side of
// the point of no return it is on, without having had to write anything down
// beforehand: the cluster already knows. deleting is true once the PVC carries a
// deletionTimestamp — Delete only returns after the apiserver has set it, so a
// process killed during the deletion is still detected as "past the point of no
// return", which is the safe direction.
func (s *StatusClient) GetVClusterPVCState(ctx context.Context, name string) (exists, deleting bool, err error) {
	topo, err := s.detectVClusterTopology(ctx, name)
	if err != nil {
		return false, false, fmt.Errorf("detecting vcluster topology: %w", err)
	}
	ns := "vcluster-" + name
	pvc, err := s.client.Resource(persistentVolumeClaimGVR).Namespace(ns).Get(ctx, topo.PVCName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return false, false, nil
	case err != nil:
		return false, false, fmt.Errorf("reading PVC %s: %w", topo.PVCName, err)
	}
	return true, pvc.GetDeletionTimestamp() != nil, nil
}

func (s *StatusClient) DeleteVClusterPVC(ctx context.Context, name string) error {
	topo, err := s.detectVClusterTopology(ctx, name)
	if err != nil {
		return fmt.Errorf("detecting vcluster topology: %w", err)
	}
	ns := "vcluster-" + name
	if err := s.client.Resource(persistentVolumeClaimGVR).Namespace(ns).Delete(ctx, topo.PVCName, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("deleting PVC %s: %w", topo.PVCName, err)
	}
	return nil
}
