package handlers

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/gmalfray/vcluster-manager/internal/kubernetes"
)

// UpdateClusterConfig handles the form submission to update cluster connectivity configuration.
func (h *Handlers) UpdateClusterConfig(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}

	env := r.PathValue("env")
	if env != "preprod" && env != "prod" {
		h.renderToast(w, "error", "Environnement invalide")
		return
	}

	// Parse multipart form (max 10MB)
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		h.renderToast(w, "error", "Erreur lors de la lecture du formulaire")
		return
	}

	dataDir := h.cfg.DataDir()
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		h.renderToast(w, "error", fmt.Sprintf("Erreur creation dossier data : %v", err))
		return
	}

	// Update cluster label
	clusterLabel := r.FormValue("cluster_label")
	if clusterLabel != "" {
		h.cfg.SetClusterLabel(env, clusterLabel)
	}

	sshTarget := r.FormValue("ssh_target")

	// Get current config as defaults
	currentKubeconfig, currentSSHTunnel, currentSSHKey := h.cfg.GetClusterConfig(env)
	kubeconfigPath := currentKubeconfig
	sshKeyPath := currentSSHKey

	// Override SSH tunnel if form field is present (even if empty — allows clearing)
	if r.Form.Has("ssh_target") {
		currentSSHTunnel = sshTarget
	}

	// Handle kubeconfig file upload (optional)
	if file, _, err := r.FormFile("kubeconfig_file"); err == nil {
		defer file.Close()
		content, err := io.ReadAll(file)
		if err != nil {
			h.renderToast(w, "error", fmt.Sprintf("Erreur lecture kubeconfig : %v", err))
			return
		}
		kubeconfigPath = filepath.Join(dataDir, fmt.Sprintf("kubeconfig-%s.yaml", env))
		if err := os.WriteFile(kubeconfigPath, content, 0600); err != nil {
			h.renderToast(w, "error", fmt.Sprintf("Erreur ecriture kubeconfig : %v", err))
			return
		}
	}

	// Handle SSH key file upload (optional)
	if file, _, err := r.FormFile("ssh_key_file"); err == nil {
		defer file.Close()
		content, err := io.ReadAll(file)
		if err != nil {
			h.renderToast(w, "error", fmt.Sprintf("Erreur lecture cle SSH : %v", err))
			return
		}
		sshKeyPath = filepath.Join(dataDir, fmt.Sprintf("ssh-key-%s", env))
		if err := os.WriteFile(sshKeyPath, content, 0600); err != nil {
			h.renderToast(w, "error", fmt.Sprintf("Erreur ecriture cle SSH : %v", err))
			return
		}
	}

	// Persist config (always, even if client init fails later)
	h.cfg.SetClusterConfig(env, kubeconfigPath, currentSSHTunnel, sshKeyPath)

	// Try to initialize K8s client (non-blocking: save succeeds even if connection fails)
	if kubeconfigPath != "" {
		var client *kubernetes.StatusClient
		var err error

		if currentSSHTunnel != "" {
			var tunnel *kubernetes.SSHTunnel
			client, tunnel, err = kubernetes.NewStatusClientWithTunnel(kubeconfigPath, currentSSHTunnel, sshKeyPath)
			if err == nil {
				_ = tunnel
			}
		} else {
			client, err = kubernetes.NewStatusClient(kubeconfigPath)
		}

		if err != nil {
			slog.Warn("K8s client init failed (config saved, will retry on restart)", "env", env, "err", err)
			w.Header().Set("HX-Refresh", "true")
			h.renderToast(w, "warning", fmt.Sprintf("Configuration %s sauvegardee. Connexion echouee : %v", env, err))
			return
		}

		h.k8sClientsMu.Lock()
		h.k8sClients[env] = client
		h.k8sClientsMu.Unlock()
	}

	w.Header().Set("HX-Refresh", "true")
	h.renderToast(w, "success", fmt.Sprintf("Configuration %s mise a jour", env))
}
