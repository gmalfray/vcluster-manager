package handlers

import (
	"fmt"
	"log"
	"net/http"

	"github.com/gmalfray/vcluster-manager/internal/audit"
	"github.com/gmalfray/vcluster-manager/internal/gitops"
	"github.com/gmalfray/vcluster-manager/internal/models"
)

// UpdateSettings handles vcluster settings modification.
func (h *Handlers) UpdateSettings(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	name := r.PathValue("name")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}

	req := &models.UpdateRequest{
		VeleroEnabled: r.FormValue("velero_enabled") == "on",
		VeleroHour:    r.FormValue("velero_hour"),
		VeleroTTL:     parseTTLText(r.FormValue("velero_ttl")),
		CPU:           r.FormValue("cpu"),
		Memory:        r.FormValue("memory"),
		Storage:       r.FormValue("storage"),
		NoQuotas:      r.FormValue("no_quotas") == "on",
		RBACGroups:    splitGroups(r.FormValue("rbac_groups"), h.cfg.DefaultRBACGroup),
		K8sVersion:    r.FormValue("k8s_version"),
		ArgoCDVersion: r.FormValue("argocd_version"),
		FluxCDEnabled: r.FormValue("fluxcd_enabled") == "on",
		FluxCDRepoURL: r.FormValue("fluxcd_repo_url"),
		FluxCDBranch:  r.FormValue("fluxcd_branch"),
		FluxCDPath:    r.FormValue("fluxcd_path"),
	}

	argoCDToggle := r.FormValue("argocd")  // "on", "off", or "" (not changing)
	fluxCDToggle := r.FormValue("fluxcd")  // "on", "off", or "" (not changing)
	deleteRepo := r.FormValue("delete_repo") == "on"

	env := r.URL.Query().Get("env")
	if env == "" {
		env = "preprod"
	}
	isPending := env == "prod" && h.isPendingProd(name)

	// Handle ArgoCD toggle (any env, any deployment state)
	if argoCDToggle != "" {
		currentVC, err := h.parser.ParseVCluster(env, name)
		if err != nil {
			h.renderToast(w, "error", "VCluster introuvable : "+err.Error())
			return
		}
		newArgoCD := argoCDToggle == "on"

		if newArgoCD != currentVC.ArgoCD {
			// Rebuild all files with the new ArgoCD flag
			vcPath := fmt.Sprintf("clusters/%s/vclusters/%s", env, name)
			existingFiles, _ := h.gitlab.ListFiles("preprod", vcPath)

			var actions []gitops.CommitAction
			for _, f := range existingFiles {
				actions = append(actions, gitops.CommitAction{Action: "delete", Path: f})
			}

			createReq := &models.CreateRequest{
				Name:          name,
				ArgoCD:        newArgoCD,
				RBACGroups:    req.RBACGroups,
				VeleroEnabled: req.VeleroEnabled,
				VeleroHour:    req.VeleroHour,
				VeleroTTL:     req.VeleroTTL,
				CPU:           req.CPU,
				Memory:        req.Memory,
				Storage:       req.Storage,
				NoQuotas:      req.NoQuotas,
				ArgoCDVersion: req.ArgoCDVersion,
			}
			for _, f := range h.generator.GenerateVCluster(createReq, env) {
				actions = append(actions, gitops.CommitAction{
					Action:  "create",
					Path:    f.Path,
					Content: f.Content,
				})
			}

			commitMsg := fmt.Sprintf("feat: reconfigure vcluster %s (%s, argocd=%v)", name, env, newArgoCD)

			if env == "preprod" || isPending {
				if err := h.gitlab.Commit("preprod", commitMsg, actions); err != nil {
					h.renderToast(w, "error", "Erreur commit : "+err.Error())
					return
				}
			} else {
				// Deployed prod: via MR
				if _, err := h.commitProdMRActions(
					commitMsg,
					fmt.Sprintf("Reconfiguration ArgoCD du vcluster **%s** en production (argocd=%v).\n\nCréé automatiquement par vcluster-manager.", name, newArgoCD),
					actions,
				); err != nil {
					log.Printf("MR creation error for ArgoCD reconfigure: %v", err)
				}
			}

			if newArgoCD {
				// Enabling: create repo only if it doesn't exist, create Keycloak clients
				if !h.gitlab.AppManifestsRepoExists(name) {
					if _, err := h.gitlab.CreateAppManifestsRepo(name); err != nil {
						log.Printf("Erreur création repo app-manifests-%s: %v", name, err)
					}
				} else {
					log.Printf("Repo app-manifests-%s already exists, skipping creation", name)
				}
				if h.keycloak != nil {
					if err := h.keycloak.CreateArgoCDClients(name, env); err != nil {
						log.Printf("Erreur création clients Keycloak: %v", err)
					}
				}
			} else {
				// Disabling: delete repo only if explicitly requested
				if deleteRepo {
					if err := h.gitlab.DeleteProject(name); err != nil {
						log.Printf("Erreur suppression repo app-manifests-%s: %v", name, err)
					}
				}
			}

			h.redirectWithFlash(w, fmt.Sprintf("/vclusters/%s?env=%s", name, env), "success", "Configuration ArgoCD modifiée")
			return
		}
	}

	// Handle FluxCD toggle
	if fluxCDToggle != "" {
		currentVC, err := h.parser.ParseVCluster(env, name)
		if err != nil {
			h.renderToast(w, "error", "VCluster introuvable : "+err.Error())
			return
		}
		newFluxCD := fluxCDToggle == "on"

		if newFluxCD != currentVC.FluxCD.Enabled {
			vcPath := fmt.Sprintf("clusters/%s/vclusters/%s", env, name)
			existingFiles, _ := h.gitlab.ListFiles("preprod", vcPath)

			var actions []gitops.CommitAction
			for _, f := range existingFiles {
				actions = append(actions, gitops.CommitAction{Action: "delete", Path: f})
			}

			createReq := &models.CreateRequest{
				Name:          name,
				ArgoCD:        currentVC.ArgoCD,
				RBACGroups:    req.RBACGroups,
				VeleroEnabled: req.VeleroEnabled,
				VeleroHour:    req.VeleroHour,
				VeleroTTL:     req.VeleroTTL,
				CPU:           req.CPU,
				Memory:        req.Memory,
				Storage:       req.Storage,
				NoQuotas:      req.NoQuotas,
				ArgoCDVersion: req.ArgoCDVersion,
				FluxCDEnabled: newFluxCD,
				FluxCDRepoURL: req.FluxCDRepoURL,
				FluxCDBranch:  req.FluxCDBranch,
				FluxCDPath:    req.FluxCDPath,
			}
			for _, f := range h.generator.GenerateVCluster(createReq, env) {
				actions = append(actions, gitops.CommitAction{
					Action:  "create",
					Path:    f.Path,
					Content: f.Content,
				})
			}

			commitMsg := fmt.Sprintf("feat: reconfigure vcluster %s (%s, fluxcd=%v)", name, env, newFluxCD)

			if env == "preprod" || isPending {
				if err := h.gitlab.Commit("preprod", commitMsg, actions); err != nil {
					h.renderToast(w, "error", "Erreur commit : "+err.Error())
					return
				}
			} else {
				if _, err := h.commitProdMRActions(
					commitMsg,
					fmt.Sprintf("Reconfiguration FluxCD du vcluster **%s** en production (fluxcd=%v).\n\nCréé automatiquement par vcluster-manager.", name, newFluxCD),
					actions,
				); err != nil {
					log.Printf("MR creation error for FluxCD reconfigure: %v", err)
				}
			}

			h.redirectWithFlash(w, fmt.Sprintf("/vclusters/%s?env=%s", name, env), "success", "Configuration FluxCD modifiée")
			return
		}
	}

	if env == "preprod" {
		// Commit preprod changes to preprod branch
		var preprodActions []gitops.CommitAction
		vf := h.generator.GenerateUpdatedValues(name, "preprod", req)
		preprodActions = append(preprodActions, gitops.CommitAction{
			Action:  "update",
			Path:    vf.Path,
			Content: vf.Content,
		})
		vc, err := h.parser.ParseVCluster("preprod", name)
		if err == nil && vc.ArgoCD {
			if len(req.RBACGroups) > 0 {
				rf := h.generator.GenerateUpdatedRBAC(name, "preprod", req.RBACGroups)
				preprodActions = append(preprodActions, gitops.CommitAction{
					Action:  "update",
					Path:    rf.Path,
					Content: rf.Content,
				})
			}
			af := h.generator.GenerateUpdatedArgocdOverlay(name, "preprod", req.ArgoCDVersion)
			preprodActions = append(preprodActions, gitops.CommitAction{
				Action:  "update",
				Path:    af.Path,
				Content: af.Content,
			})
		}
		if err == nil && vc.FluxCD.Enabled && req.FluxCDRepoURL != "" {
			ff := h.generator.GenerateUpdatedFluxBootstrapOverlay(name, "preprod", req.FluxCDRepoURL, req.FluxCDBranch, req.FluxCDPath)
			preprodActions = append(preprodActions, gitops.CommitAction{
				Action:  "update",
				Path:    ff.Path,
				Content: ff.Content,
			})
		}

		if err := h.gitlab.Commit("preprod", fmt.Sprintf("feat: update vcluster %s settings", name), preprodActions); err != nil {
			log.Printf("GitLab commit error: %v", err)
			h.renderToast(w, "error", "Erreur commit : "+err.Error())
			return
		}
	} else if env == "prod" {
		// Handle prod changes
		var prodActions []gitops.CommitAction
		pvf := h.generator.GenerateUpdatedValues(name, "prod", req)
		prodActions = append(prodActions, gitops.CommitAction{
			Action: "update", Path: pvf.Path, Content: pvf.Content,
		})
		vcProd, err := h.parser.ParseVCluster("prod", name)
		if err == nil && vcProd.ArgoCD {
			if len(req.RBACGroups) > 0 {
				rf := h.generator.GenerateUpdatedRBAC(name, "prod", req.RBACGroups)
				prodActions = append(prodActions, gitops.CommitAction{
					Action: "update", Path: rf.Path, Content: rf.Content,
				})
			}
			af := h.generator.GenerateUpdatedArgocdOverlay(name, "prod", req.ArgoCDVersion)
			prodActions = append(prodActions, gitops.CommitAction{
				Action: "update", Path: af.Path, Content: af.Content,
			})
		}
		if err == nil && vcProd.FluxCD.Enabled && req.FluxCDRepoURL != "" {
			ff := h.generator.GenerateUpdatedFluxBootstrapOverlay(name, "prod", req.FluxCDRepoURL, req.FluxCDBranch, req.FluxCDPath)
			prodActions = append(prodActions, gitops.CommitAction{
				Action: "update", Path: ff.Path, Content: ff.Content,
			})
		}

		if isPending {
			if err := h.gitlab.Commit("preprod", fmt.Sprintf("feat: update vcluster %s settings (prod)", name), prodActions); err != nil {
				log.Printf("GitLab commit error (prod pending): %v", err)
				h.renderToast(w, "error", "Erreur commit : "+err.Error())
				return
			}
		} else {
			mrURL, err := h.commitProdMRActions(
				fmt.Sprintf("feat: update vcluster %s settings", name),
				fmt.Sprintf("Mise à jour des paramètres du vcluster **%s** en production.\n\nCréé automatiquement par vcluster-manager.", name),
				prodActions,
			)
			if err != nil {
				log.Printf("MR creation error for settings update: %v", err)
			} else {
				log.Printf("MR created for prod settings update: %s", mrURL)
			}
		}
	}

	audit.Log(r, "update-settings", name, env)
	redirectURL := fmt.Sprintf("/vclusters/%s", name)
	if env == "prod" {
		redirectURL += "?env=prod"
	}
	h.redirectWithFlash(w, redirectURL, "success", "Paramètres mis à jour")
}
