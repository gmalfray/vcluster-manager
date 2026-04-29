package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
