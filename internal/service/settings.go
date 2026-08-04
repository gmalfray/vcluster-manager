package service

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"

	"github.com/gmalfray/vcluster-manager/internal/audit"
	"github.com/gmalfray/vcluster-manager/internal/gitops"
	"github.com/gmalfray/vcluster-manager/internal/models"
)

// versionRegex accepts a K8s/ArgoCD version or tag string. validateQuantity,
// validateFluxRepoURL, validateBranchOrPath and validateVeleroHour already
// live in vcluster.go (Create needs them too); this is the one field-checker
// Create never needed, added here for UpdateSettings.
var versionRegex = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,62}$`)

// validateVersion checks a K8s/ArgoCD version or tag string. Empty is valid:
// it means "use the configured default".
func validateVersion(field, value string) error {
	if value == "" {
		return nil
	}
	if !versionRegex.MatchString(value) {
		return fieldError(field, "version invalide")
	}
	return nil
}

// UpdateSettingsInput carries the parsed settings-form values into the
// service. RBACGroups arrives already split and defaulted (splitGroups in the
// handler) — the service doesn't touch form-shorthand parsing, only the
// GitOps side of applying the change. VeleroTTL is likewise already the Go
// duration string (parseTTLText in the handler), not the short form.
type UpdateSettingsInput struct {
	VeleroEnabled bool
	VeleroHour    string
	VeleroTTL     string
	CPU           string
	Memory        string
	Storage       string
	NoQuotas      bool
	RBACGroups    []string
	K8sVersion    string
	ArgoCDVersion string
	FluxCDEnabled bool
	FluxCDRepoURL string
	FluxCDBranch  string
	FluxCDPath    string

	// Toggles: "on", "off", or "" (leave unchanged). When a toggle actually
	// flips the current state, the whole vcluster is reconfigured and the
	// call returns early; otherwise the flow falls through to a plain
	// settings update.
	ArgoCDToggle string
	FluxCDToggle string
	// DeleteRepo, when disabling ArgoCD, also deletes the app-manifests repo.
	DeleteRepo bool
}

// UpdateSettingsResult carries what the adapter needs to redirect and flash a
// message — the service decides both so the adapter stays a thin transport.
type UpdateSettingsResult struct {
	RedirectURL  string
	FlashLevel   string
	FlashMessage string
	// MRURL is the prod merge-request URL when a plain prod update opened
	// one. Empty otherwise; informational only, not shown in the toast
	// (matches the former handler).
	MRURL string
	Name  string
	Env   string
}

// UpdateSettings applies a vcluster settings change: it patches values.yaml
// (quotas, velero, RBAC, K8s/ArgoCD versions…), handles the ArgoCD/FluxCD
// toggles (full reconfigure), and commits to preprod or opens a prod MR
// depending on the environment and pending-MR state. Admin only (RBAC
// enforced here, in the service).
func (s *Service) UpdateSettings(ctx context.Context, actor models.Actor, name, env string, in UpdateSettingsInput) (UpdateSettingsResult, error) {
	if !actor.IsAdmin {
		return UpdateSettingsResult{}, ErrForbidden
	}
	if !validName(name) {
		return UpdateSettingsResult{}, ErrInvalidName
	}

	// These fields land in fluxprod YAML through a text/template that doesn't
	// escape anything (internal/gitops/generator.go), so they're checked
	// before anything gets committed.
	if err := validateQuantity("cpu", in.CPU); err != nil {
		return UpdateSettingsResult{}, err
	}
	if err := validateQuantity("memory", in.Memory); err != nil {
		return UpdateSettingsResult{}, err
	}
	if err := validateQuantity("storage", in.Storage); err != nil {
		return UpdateSettingsResult{}, err
	}
	if err := validateVersion("k8s_version", in.K8sVersion); err != nil {
		return UpdateSettingsResult{}, err
	}
	if err := validateVersion("argocd_version", in.ArgoCDVersion); err != nil {
		return UpdateSettingsResult{}, err
	}
	if err := validateFluxRepoURL("fluxcd_repo_url", in.FluxCDRepoURL); err != nil {
		return UpdateSettingsResult{}, err
	}
	if err := validateBranchOrPath("fluxcd_branch", in.FluxCDBranch); err != nil {
		return UpdateSettingsResult{}, err
	}
	if err := validateBranchOrPath("fluxcd_path", in.FluxCDPath); err != nil {
		return UpdateSettingsResult{}, err
	}
	if err := validateVeleroHour("velero_hour", in.VeleroHour); err != nil {
		return UpdateSettingsResult{}, err
	}

	env = envOrDefault(env)
	isPending := env == "prod" && s.isPendingProd(ctx, name)

	req := &models.UpdateRequest{
		VeleroEnabled: in.VeleroEnabled,
		VeleroHour:    in.VeleroHour,
		VeleroTTL:     in.VeleroTTL,
		CPU:           in.CPU,
		Memory:        in.Memory,
		Storage:       in.Storage,
		NoQuotas:      in.NoQuotas,
		RBACGroups:    in.RBACGroups,
		K8sVersion:    in.K8sVersion,
		ArgoCDVersion: in.ArgoCDVersion,
		FluxCDEnabled: in.FluxCDEnabled,
		FluxCDRepoURL: in.FluxCDRepoURL,
		FluxCDBranch:  in.FluxCDBranch,
		FluxCDPath:    in.FluxCDPath,
	}

	// Handle the ArgoCD toggle (any env, any deployment state).
	if in.ArgoCDToggle != "" {
		currentVC, err := s.parser.ParseVCluster(ctx, env, name)
		if err != nil {
			return UpdateSettingsResult{}, &VClusterNotFoundError{Err: err}
		}
		newArgoCD := in.ArgoCDToggle == "on"

		if newArgoCD != currentVC.ArgoCD {
			// Rebuild all files with the new ArgoCD flag.
			vcPath := fmt.Sprintf("clusters/%s/vclusters/%s", env, name)
			existingFiles, _ := s.gitlab.ListFiles(ctx, "preprod", vcPath)

			var actions []gitops.CommitAction
			for _, f := range existingFiles {
				actions = append(actions, gitops.CommitAction{Action: "delete", Path: f})
			}

			// This branch deletes every file of the vcluster before
			// regenerating them, so anything missing from createReq isn't
			// "left unchanged" — it's dropped. FluxCD has to be carried over
			// explicitly: the toggle flips ArgoCD, it must not silently
			// un-bootstrap Flux. Mirrors the FluxCD toggle below, which
			// carries ArgoCD over the same way.
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
				FluxCDEnabled: currentVC.FluxCD.Enabled,
				FluxCDRepoURL: firstNonEmpty(req.FluxCDRepoURL, currentVC.FluxCD.RepoURL),
				FluxCDBranch:  firstNonEmpty(req.FluxCDBranch, currentVC.FluxCD.Branch),
				FluxCDPath:    firstNonEmpty(req.FluxCDPath, currentVC.FluxCD.Path),
			}
			for _, f := range s.generator.GenerateVCluster(createReq, env) {
				actions = append(actions, gitops.CommitAction{
					Action:  "create",
					Path:    f.Path,
					Content: f.Content,
				})
			}

			commitMsg := fmt.Sprintf("feat: reconfigure vcluster %s (%s, argocd=%v)", name, env, newArgoCD)

			if env == "preprod" || isPending {
				if err := s.gitlab.Commit(ctx, "preprod", commitMsg, actions); err != nil {
					return UpdateSettingsResult{}, &CommitError{Err: err}
				}
			} else {
				// Deployed prod: via MR. A failure here is logged only — it
				// mirrors the former handler, which never turned it into a
				// user-facing error.
				if _, err := s.commitProdMRActions(
					ctx,
					commitMsg,
					fmt.Sprintf("Reconfiguration ArgoCD du vcluster **%s** en production (argocd=%v).\n\nCréé automatiquement par vcluster-manager.", name, newArgoCD),
					actions,
				); err != nil {
					slog.Error("MR creation failed for ArgoCD reconfigure", "vcluster", name, "err", err)
				}
			}

			if newArgoCD {
				// Enabling: create the repo only if it doesn't exist yet, and
				// the Keycloak clients.
				if !s.gitlab.AppManifestsRepoExists(name) {
					if _, err := s.gitlab.CreateAppManifestsRepo(name); err != nil {
						slog.Error("app-manifests repo creation failed", "vcluster", name, "err", err)
					}
				} else {
					slog.Info("app-manifests repo already exists, skipping creation", "vcluster", name)
				}
				if s.keycloak != nil {
					if err := s.keycloak.CreateArgoCDClients(name, env); err != nil {
						slog.Error("Keycloak ArgoCD clients creation failed", "vcluster", name, "err", err)
					}
				}
			} else if in.DeleteRepo {
				// Disabling: delete the repo only if explicitly requested.
				if err := s.gitlab.DeleteProject(name); err != nil {
					slog.Error("app-manifests repo deletion failed", "vcluster", name, "err", err)
				}
			}

			return UpdateSettingsResult{
				RedirectURL:  fmt.Sprintf("/vclusters/%s?env=%s", name, env),
				FlashLevel:   "success",
				FlashMessage: "Configuration ArgoCD modifiée",
				Name:         name,
				Env:          env,
			}, nil
		}
	}

	// Handle the FluxCD toggle.
	if in.FluxCDToggle != "" {
		currentVC, err := s.parser.ParseVCluster(ctx, env, name)
		if err != nil {
			return UpdateSettingsResult{}, &VClusterNotFoundError{Err: err}
		}
		newFluxCD := in.FluxCDToggle == "on"

		if newFluxCD != currentVC.FluxCD.Enabled {
			vcPath := fmt.Sprintf("clusters/%s/vclusters/%s", env, name)
			existingFiles, _ := s.gitlab.ListFiles(ctx, "preprod", vcPath)

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
			for _, f := range s.generator.GenerateVCluster(createReq, env) {
				actions = append(actions, gitops.CommitAction{
					Action:  "create",
					Path:    f.Path,
					Content: f.Content,
				})
			}

			commitMsg := fmt.Sprintf("feat: reconfigure vcluster %s (%s, fluxcd=%v)", name, env, newFluxCD)

			if env == "preprod" || isPending {
				if err := s.gitlab.Commit(ctx, "preprod", commitMsg, actions); err != nil {
					return UpdateSettingsResult{}, &CommitError{Err: err}
				}
			} else {
				if _, err := s.commitProdMRActions(
					ctx,
					commitMsg,
					fmt.Sprintf("Reconfiguration FluxCD du vcluster **%s** en production (fluxcd=%v).\n\nCréé automatiquement par vcluster-manager.", name, newFluxCD),
					actions,
				); err != nil {
					slog.Error("MR creation failed for FluxCD reconfigure", "vcluster", name, "err", err)
				}
			}

			return UpdateSettingsResult{
				RedirectURL:  fmt.Sprintf("/vclusters/%s?env=%s", name, env),
				FlashLevel:   "success",
				FlashMessage: "Configuration FluxCD modifiée",
				Name:         name,
				Env:          env,
			}, nil
		}
	}

	var mrURL string

	if env == "preprod" {
		// Commit preprod changes to the preprod branch.
		var preprodActions []gitops.CommitAction
		vf := s.generator.GenerateUpdatedValues(name, "preprod", req)
		preprodActions = append(preprodActions, gitops.CommitAction{
			Action:  "update",
			Path:    vf.Path,
			Content: vf.Content,
		})
		vc, err := s.parser.ParseVCluster(ctx, "preprod", name)
		if err == nil && vc.ArgoCD {
			if len(req.RBACGroups) > 0 {
				rf := s.generator.GenerateUpdatedRBAC(name, "preprod", req.RBACGroups)
				preprodActions = append(preprodActions, gitops.CommitAction{
					Action:  "update",
					Path:    rf.Path,
					Content: rf.Content,
				})
			}
			af := s.generator.GenerateUpdatedArgocdOverlay(name, "preprod", req.ArgoCDVersion)
			preprodActions = append(preprodActions, gitops.CommitAction{
				Action:  "update",
				Path:    af.Path,
				Content: af.Content,
			})
		}
		if err == nil && vc.FluxCD.Enabled && req.FluxCDRepoURL != "" {
			ff := s.generator.GenerateUpdatedFluxBootstrapOverlay(name, "preprod", req.FluxCDRepoURL, req.FluxCDBranch, req.FluxCDPath)
			preprodActions = append(preprodActions, gitops.CommitAction{
				Action:  "update",
				Path:    ff.Path,
				Content: ff.Content,
			})
		}

		if err := s.gitlab.Commit(ctx, "preprod", fmt.Sprintf("feat: update vcluster %s settings", name), preprodActions); err != nil {
			slog.Error("GitLab commit failed", "vcluster", name, "env", "preprod", "err", err)
			return UpdateSettingsResult{}, &CommitError{Err: err}
		}
	} else if env == "prod" {
		var prodActions []gitops.CommitAction
		pvf := s.generator.GenerateUpdatedValues(name, "prod", req)
		prodActions = append(prodActions, gitops.CommitAction{
			Action: "update", Path: pvf.Path, Content: pvf.Content,
		})
		vcProd, err := s.parser.ParseVCluster(ctx, "prod", name)
		if err == nil && vcProd.ArgoCD {
			if len(req.RBACGroups) > 0 {
				rf := s.generator.GenerateUpdatedRBAC(name, "prod", req.RBACGroups)
				prodActions = append(prodActions, gitops.CommitAction{
					Action: "update", Path: rf.Path, Content: rf.Content,
				})
			}
			af := s.generator.GenerateUpdatedArgocdOverlay(name, "prod", req.ArgoCDVersion)
			prodActions = append(prodActions, gitops.CommitAction{
				Action: "update", Path: af.Path, Content: af.Content,
			})
		}
		if err == nil && vcProd.FluxCD.Enabled && req.FluxCDRepoURL != "" {
			ff := s.generator.GenerateUpdatedFluxBootstrapOverlay(name, "prod", req.FluxCDRepoURL, req.FluxCDBranch, req.FluxCDPath)
			prodActions = append(prodActions, gitops.CommitAction{
				Action: "update", Path: ff.Path, Content: ff.Content,
			})
		}

		if isPending {
			if err := s.gitlab.Commit(ctx, "preprod", fmt.Sprintf("feat: update vcluster %s settings (prod)", name), prodActions); err != nil {
				slog.Error("GitLab commit failed (prod pending)", "vcluster", name, "err", err)
				return UpdateSettingsResult{}, &CommitError{Err: err}
			}
		} else {
			// Not yet promoted: opens (or reuses) the standing prod MR. A
			// failure here is logged only, same as the former handler.
			url, err := s.commitProdMRActions(
				ctx,
				fmt.Sprintf("feat: update vcluster %s settings", name),
				fmt.Sprintf("Mise à jour des paramètres du vcluster **%s** en production.\n\nCréé automatiquement par vcluster-manager.", name),
				prodActions,
			)
			if err != nil {
				slog.Error("MR creation failed for settings update", "vcluster", name, "err", err)
			} else {
				mrURL = url
				slog.Info("MR created for prod settings update", "vcluster", name, "url", mrURL)
			}
		}
	}

	audit.LogActor(actor.Username, "update-settings", name, env)
	redirectURL := fmt.Sprintf("/vclusters/%s", name)
	if env == "prod" {
		redirectURL += "?env=prod"
	}
	return UpdateSettingsResult{
		RedirectURL:  redirectURL,
		FlashLevel:   "success",
		FlashMessage: "Paramètres mis à jour",
		MRURL:        mrURL,
		Name:         name,
		Env:          env,
	}, nil
}

// firstNonEmpty returns a, or b when a is empty. Used when a form may or may
// not carry a field the caller must not lose.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
