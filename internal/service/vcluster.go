package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/gmalfray/vcluster-manager/internal/audit"
	"github.com/gmalfray/vcluster-manager/internal/gitops"
	"github.com/gmalfray/vcluster-manager/internal/models"
)

// nameRegex is the accepted shape of a vcluster name (also a K8s namespace
// suffix): starts with a letter, then lowercase alphanumerics and dashes. It
// already rules out "..", "/" and anything else a path-traversal attempt would
// need. Moved here from the handlers package during the service-layer
// extraction so every domain (and every adapter) validates names against a
// single copy instead of each keeping its own.
var nameRegex = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// OperatorNamespace is where the app and the operator run. Everything derived
// from a vcluster name is `"vcluster-" + name`, so this namespace is reachable
// by naming a vcluster after its suffix.
const OperatorNamespace = "vcluster-manager"

// reservedNames are the names whose derived namespace lands on something that
// is not a tenant.
//
// « manager » n'est pas un nom interdit par prudence, c'est une collision
// arithmétique : "vcluster-" + "manager" == le namespace de l'app et de
// l'opérateur. Sans cette liste, la garde de placement accepte un marqueur
// nommé `manager` déposé dans `vcluster-manager` — les deux règles coïncident —
// et un backup Velero de ce « vcluster » exporte les Secrets de l'app (token
// GitLab, secret client Keycloak, JWT_SECRET) vers le bucket S3. Le chemin
// destructeur, lui, ne s'arrête aujourd'hui que sur une chance de nommage :
// l'app est un Deployment là où le code cherche un StatefulSet. Une chance
// n'est pas un contrôle.
//
// Le refus vit ici plutôt que dans la garde parce que les 22 appelants de
// validName en bénéficient d'un coup, y compris les routes HTTP qui
// construisent un marqueur à la volée.
var reservedNames = map[string]bool{
	strings.TrimPrefix(OperatorNamespace, "vcluster-"): true,
}

// validName reports whether name is a usable vcluster name.
func validName(name string) bool {
	if reservedNames[name] {
		return false
	}
	return nameRegex.MatchString(name)
}

// ValidName is validName, exported for callers outside this package that need
// to check a vcluster name before it reaches something that isn't a service
// method — a handler validating a path parameter before handing it to a
// client-go call, for instance. Everything inside the service package should
// keep using the unexported validName.
func ValidName(name string) bool {
	return validName(name)
}

// ErrInvalidName means the requested name doesn't match nameRegex. Not every
// domain wires this in yet — it's here so the ones that do (and the ones
// extracted later) share the same sentinel instead of each declaring their own.
var ErrInvalidName = errors.New("invalid vcluster name")

// vcluster-domain sentinel errors.
var (
	// ErrVClusterExists means a vcluster with that name already exists in the
	// targeted environment.
	ErrVClusterExists = errors.New("vcluster already exists")
	// ErrVClusterNotFound means the fluxprod files for that name/env couldn't
	// be parsed.
	ErrVClusterNotFound = errors.New("vcluster not found")
	// ErrCleaningInProgress (a Rancher cleanup job is already running) is
	// declared in rancher.go — the deletion path reuses that same sentinel.
)

// ExistsError carries the environment where the name is already taken, so the
// adapter can render the exact message it used to ("... existe deja en prod").
type ExistsError struct {
	Name string
	Env  string
}

func (e *ExistsError) Error() string {
	return fmt.Sprintf("vcluster %s already exists in %s", e.Name, e.Env)
}
func (e *ExistsError) Is(target error) bool { return target == ErrVClusterExists }

// VClusterNotFoundError wraps the underlying parse error of a missing vcluster.
type VClusterNotFoundError struct{ Err error }

func (e *VClusterNotFoundError) Error() string        { return "vcluster not found: " + e.Err.Error() }
func (e *VClusterNotFoundError) Unwrap() error        { return e.Err }
func (e *VClusterNotFoundError) Is(target error) bool { return target == ErrVClusterNotFound }

// CleaningError says a Rancher cleanup is still running, and in which
// environment.
type CleaningError struct {
	Name string
	Env  string
}

func (e *CleaningError) Error() string {
	return fmt.Sprintf("rancher cleanup in progress for %s (%s)", e.Name, e.Env)
}
func (e *CleaningError) Is(target error) bool { return target == ErrCleaningInProgress }

// UnpairError wraps a Rancher unpair failure hit on the deletion path.
type UnpairError struct{ Err error }

func (e *UnpairError) Error() string { return "rancher unpair failed: " + e.Err.Error() }
func (e *UnpairError) Unwrap() error { return e.Err }

// CommitError wraps a GitLab commit failure on the vcluster-creation path
// (preprod files). Fatal: creation aborts before touching anything else.
type CommitError struct{ Err error }

func (e *CommitError) Error() string { return "gitlab commit failed: " + e.Err.Error() }
func (e *CommitError) Unwrap() error { return e.Err }

// --- field validation --------------------------------------------------
//
// Shared by every domain in this package that commits generated YAML to
// fluxprod, settings.go included (UpdateSettings calls these same functions
// directly, they're not duplicated elsewhere).

var (
	quantityRegex    = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(m|k|M|G|T|P|Ki|Mi|Gi|Ti|Pi)?$`)
	fluxRepoSSHRegex = regexp.MustCompile(`^ssh://[A-Za-z0-9_.-]+@[A-Za-z0-9_.-]+(:[0-9]+)?/[A-Za-z0-9_./-]+$`)
	fluxRepoSCPRegex = regexp.MustCompile(`^[A-Za-z0-9_.-]+@[A-Za-z0-9_.-]+:[A-Za-z0-9_./-]+$`)
	branchPathRegex  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,254}$`)
	veleroHourRegex  = regexp.MustCompile(`^([01]?[0-9]|2[0-3]):[0-5][0-9]$`)
)

// fieldError formats a rejected-field message the same way validate.go does,
// so the toast reads "cpu : quantité Kubernetes invalide (...)".
func fieldError(field, reason string) error {
	return fmt.Errorf("%s : %s", field, reason)
}

// validateQuantity checks a Kubernetes resource quantity (CPU/memory/storage).
// Empty is valid: it means "use the configured default".
func validateQuantity(field, value string) error {
	if value == "" {
		return nil
	}
	if !quantityRegex.MatchString(value) {
		return fieldError(field, "quantité Kubernetes invalide (ex: 8, 500m, 32Gi)")
	}
	return nil
}

// validateFluxRepoURL checks a FluxCD bootstrap repo URL (SSH form only).
func validateFluxRepoURL(field, value string) error {
	if value == "" {
		return nil
	}
	if !fluxRepoSSHRegex.MatchString(value) && !fluxRepoSCPRegex.MatchString(value) {
		return fieldError(field, "URL SSH invalide (ex: ssh://git@host:port/chemin.git)")
	}
	return nil
}

// validateBranchOrPath checks a FluxCD branch or path: no newlines, no shell
// or YAML metacharacters, nothing a path-traversal attempt would need.
func validateBranchOrPath(field, value string) error {
	if value == "" {
		return nil
	}
	if !branchPathRegex.MatchString(value) {
		return fieldError(field, "caractères non autorisés")
	}
	return nil
}

// validateVeleroHour checks the "HH:MM" backup time. Empty is valid: Velero
// stays on its current schedule.
func validateVeleroHour(field, value string) error {
	if value == "" {
		return nil
	}
	if !veleroHourRegex.MatchString(value) {
		return fieldError(field, "format attendu HH:MM (ex: 03:00)")
	}
	return nil
}

// --- read side -----------------------------------------------------------

// DetailData is one vcluster plus the context the detail page needs.
// Formatting (TTL as "30j", labels) stays in the adapters — the service hands
// over raw values.
type DetailData struct {
	VCluster           *models.VCluster
	Env                string
	EnvLabel           string
	APIHost            string
	ArgoURL            string
	AppManifestsExists bool
	// Pending is a prod vcluster committed on preprod but not yet on master.
	Pending bool
	// ProdDeployed is a prod vcluster already on master (read-only in the UI).
	ProdDeployed       bool
	PendingMRURL       string
	HasPendingMRChange bool
	RancherEnabled     bool
	RancherPaired      bool
	K8sVersions        []string
	ArgoCDVersions     []string
}

// GetVCluster returns one vcluster with its detail-page context. Read-only, no
// privilege required. Returns a VClusterNotFoundError when the name/env is
// unknown.
func (s *Service) GetVCluster(ctx context.Context, name, env string) (DetailData, error) {
	env = envOrDefault(env)

	vc, err := s.parser.ParseVCluster(ctx, env, name)
	if err != nil {
		return DetailData{}, &VClusterNotFoundError{Err: err}
	}

	var apiHost, argoURL string
	if env == "preprod" {
		apiHost = name + ".api." + s.cfg.BaseDomainPreprod
		if vc.ArgoCD {
			argoURL = "https://argocd." + name + "." + s.cfg.BaseDomainPreprod
		}
	} else {
		apiHost = name + ".api." + s.cfg.BaseDomainProd
		if vc.ArgoCD {
			argoURL = "https://argocd." + name + "." + s.cfg.BaseDomainProd
		}
	}

	appManifestsExists := false
	if vc.ArgoCD && s.gitlab != nil {
		appManifestsExists = s.gitlab.AppManifestsRepoExists(name)
	}

	isPending := env == "prod" && s.isPendingProd(ctx, name)
	// Deployed prod vclusters are read-only: changes go through preprod.
	prodDeployed := env == "prod" && !isPending

	data := DetailData{
		VCluster:           vc,
		Env:                env,
		EnvLabel:           s.cfg.ClusterLabel(env),
		APIHost:            apiHost,
		ArgoURL:            argoURL,
		AppManifestsExists: appManifestsExists,
		Pending:            isPending,
		ProdDeployed:       prodDeployed,
	}

	if env == "prod" && s.gitlab != nil {
		mrURL, mrChangedNames, _ := s.gitlab.GetOpenPreprodMRInfo()
		data.PendingMRURL = mrURL
		if isPending {
			// Pending: the vcluster only exists on preprod, the MR is what deploys it.
			data.HasPendingMRChange = mrURL != ""
		} else {
			// Deployed: only relevant if the standing MR touches this one.
			data.HasPendingMRChange = mrChangedNames[name]
		}
	}

	data.RancherEnabled = s.rancher != nil && s.cfg.RancherEnabledForEnv(env)
	if data.RancherEnabled && !isPending {
		info, found, err := s.rancher.FindClusterByName(name)
		if err != nil {
			slog.Warn("Rancher lookup failed", "vcluster", name, "err", err)
		}
		data.RancherPaired = found && info.State == "active"
	}

	if s.ghReleases != nil {
		if versions, err := s.ghReleases.GetAvailableK8sVersions(); err == nil {
			data.K8sVersions = versions
		}
		if versions, err := s.ghReleases.GetAvailableArgoCDVersions(); err == nil {
			data.ArgoCDVersions = versions
		}
	}

	return data, nil
}

// DeleteConfirmData backs the deletion confirmation page: what exists
// elsewhere, and whether namespace protection is about to be lifted
// automatically.
type DeleteConfirmData struct {
	Name              string
	Env               string
	VCluster          *models.VCluster
	HasCounterpart    bool
	ProtectionEnabled bool
}

// GetDeleteConfirm gathers what the confirmation page shows before a
// deletion. Admin only (it is the gate to a destructive action).
func (s *Service) GetDeleteConfirm(ctx context.Context, actor models.Actor, name, env string) (DeleteConfirmData, error) {
	if !actor.IsAdmin {
		return DeleteConfirmData{}, ErrForbidden
	}
	env = envOrDefault(env)

	data := DeleteConfirmData{Name: name, Env: env}
	// Best effort: a half-broken vcluster must stay deletable.
	if vc, err := s.parser.ParseVCluster(ctx, env, name); err == nil {
		data.VCluster = vc
	}

	var counterpartPath string
	if env == "prod" {
		counterpartPath = fmt.Sprintf("%s/preprod/vclusters/%s", s.cfg.FluxprodClustersPath, name)
	} else {
		counterpartPath = fmt.Sprintf("%s/prod/vclusters/%s", s.cfg.FluxprodClustersPath, name)
	}
	counterpartFiles, _ := s.gitlab.ListFiles(ctx, gitops.SourceBranch, counterpartPath)
	data.HasCounterpart = len(counterpartFiles) > 0

	// Warn that protection will be lifted automatically by the deletion. This
	// is display-only — it does not gate anything below — so on a failed read
	// we just skip the warning instead of guessing, same best-effort spirit as
	// the ParseVCluster call above.
	if k8s := s.k8sForEnv(env); k8s != nil {
		if protected, err := k8s.GetNamespaceProtection(ctx, name); err == nil {
			data.ProtectionEnabled = protected
		}
	}

	return data, nil
}

// --- creation --------------------------------------------------------------

// CreateResult reports what the creation did. Warnings are the non-fatal
// failures (GitLab repo, Keycloak, prod MR) the UI appends to its toast.
// VaultSetupEnvs lists the environments the caller must launch the async
// Vault auth-backend setup goroutine for (empty when Vault isn't configured).
type CreateResult struct {
	Name           string
	Scope          string
	Warnings       []string
	VaultSetupEnvs []string
}

// Create provisions a vcluster through GitOps: it commits the generated files
// to fluxprod (preprod branch and/or a prod MR), creates the ArgoCD
// app-manifests repo and its Keycloak clients. Admin only.
//
// Only the fluxprod commits are fatal — the side integrations degrade into
// warnings, exactly as before, because the vcluster itself is already
// declared. The Vault auth-backend setup is async and stays the caller's
// responsibility (see CreateResult.VaultSetupEnvs) — goroutine management
// lives in the handler, not here.
func (s *Service) Create(ctx context.Context, actor models.Actor, req *models.CreateRequest, scope string) (CreateResult, error) {
	if !actor.IsAdmin {
		return CreateResult{}, ErrForbidden
	}
	if scope == "" {
		scope = "both"
	}

	// validName et pas nameRegex directement : la forme ne suffit pas, il faut
	// aussi écarter les noms dont le namespace dérivé retombe sur celui de
	// l'opérateur. Court-circuiter validName rouvrirait le trou pour ce chemin.
	if !validName(req.Name) {
		return CreateResult{}, ErrInvalidName
	}
	if err := ValidateRBACGroups(req.RBACGroups); err != nil {
		return CreateResult{}, err
	}
	// These fields land in fluxprod YAML through an unescaped text/template, so
	// they're checked before the vcluster is declared to exist anywhere.
	if err := validateQuantity("cpu", req.CPU); err != nil {
		return CreateResult{}, err
	}
	if err := validateQuantity("memory", req.Memory); err != nil {
		return CreateResult{}, err
	}
	if err := validateQuantity("storage", req.Storage); err != nil {
		return CreateResult{}, err
	}
	if err := validateFluxRepoURL("fluxcd_repo_url", req.FluxCDRepoURL); err != nil {
		return CreateResult{}, err
	}
	if err := validateBranchOrPath("fluxcd_branch", req.FluxCDBranch); err != nil {
		return CreateResult{}, err
	}
	if err := validateBranchOrPath("fluxcd_path", req.FluxCDPath); err != nil {
		return CreateResult{}, err
	}
	if err := validateVeleroHour("velero_hour", req.VeleroHour); err != nil {
		return CreateResult{}, err
	}

	checkEnv := "preprod"
	if scope == "prod" {
		checkEnv = "prod"
	}
	if s.parser.Exists(ctx, checkEnv, req.Name) {
		return CreateResult{}, &ExistsError{Name: req.Name, Env: checkEnv}
	}

	var warnings []string

	// 1. Preprod files.
	if scope == "preprod" || scope == "both" {
		var actions []gitops.CommitAction
		for _, f := range s.generator.GenerateVCluster(req, "preprod") {
			actions = append(actions, gitops.CommitAction{
				Action:  "create",
				Path:    f.Path,
				Content: f.Content,
			})
		}
		kustAction, err := s.kustomizationAction(ctx, gitops.SourceBranch, "preprod", req.Name, true)
		if err != nil {
			slog.Warn("could not update kustomization.yaml", "env", "preprod", "err", err)
		} else {
			actions = append(actions, kustAction)
		}
		if err := s.gitlab.Commit(ctx, gitops.SourceBranch, fmt.Sprintf("feat: add vcluster %s", req.Name), actions); err != nil {
			slog.Error("GitLab commit failed", "vcluster", req.Name, "err", err)
			return CreateResult{}, &CommitError{Err: err}
		}
	}

	// 2. Prod files. Both scopes land on the preprod branch (source of truth)
	// and are promoted by the standing preprod→master MR.
	if scope == "prod" || scope == "both" {
		var actions []gitops.CommitAction
		for _, f := range s.generator.GenerateVCluster(req, "prod") {
			actions = append(actions, gitops.CommitAction{
				Action:  "create",
				Path:    f.Path,
				Content: f.Content,
			})
		}
		kustAction, err := s.kustomizationAction(ctx, "prod", "preprod", req.Name, true)
		if err != nil {
			slog.Warn("could not read prod kustomization.yaml", "err", err)
		} else {
			actions = append(actions, kustAction)
		}

		mrURL, err := s.commitProdMRActions(
			ctx,
			fmt.Sprintf("feat: add vcluster %s (prod)", req.Name),
			actions,
		)
		if err != nil {
			slog.Error("MR creation failed for vcluster", "vcluster", req.Name, "err", err)
			warnings = append(warnings, "Erreur création MR prod : "+err.Error())
		} else {
			slog.Info("MR created/found for vcluster", "vcluster", req.Name, "url", mrURL)
		}
	}

	// 3. GitLab repo hosting the ArgoCD app manifests.
	if req.ArgoCD {
		if _, err := s.gitlab.CreateAppManifestsRepo(req.Name); err != nil {
			slog.Error("GitLab repo creation failed", "vcluster", req.Name, "err", err)
			warnings = append(warnings, "Erreur repo GitLab : "+err.Error())
		}
	}

	// 4. Keycloak OIDC clients for ArgoCD.
	if req.ArgoCD {
		if s.keycloak != nil {
			if err := s.keycloak.CreateArgoCDClients(req.Name, scope); err != nil {
				slog.Error("Keycloak client creation failed", "vcluster", req.Name, "err", err)
				warnings = append(warnings, "Erreur Keycloak : "+err.Error())
			} else {
				slog.Info("Keycloak OIDC clients created", "vcluster", req.Name)
			}
		} else {
			slog.Warn("Keycloak not configured, skipping OIDC client creation", "vcluster", req.Name)
			warnings = append(warnings, "Keycloak non configure : le client OIDC d'ArgoCD ne sera pas cree")
		}
	}

	// 5. Vault Kubernetes auth backend — async, waits for vault-webhook to be
	// deployed inside the fresh vcluster. Just report which envs need it: the
	// handler owns the goroutine.
	var vaultSetupEnvs []string
	if s.vault != nil {
		if scope == "preprod" || scope == "both" {
			vaultSetupEnvs = append(vaultSetupEnvs, "preprod")
		}
		if scope == "prod" || scope == "both" {
			vaultSetupEnvs = append(vaultSetupEnvs, "prod")
		}
	}

	audit.LogActor(actor.Username, "create", req.Name, scope)

	return CreateResult{
		Name:           req.Name,
		Scope:          scope,
		Warnings:       warnings,
		VaultSetupEnvs: vaultSetupEnvs,
	}, nil
}

// --- deletion ----------------------------------------------------------

// DeleteInput carries the deletion options of the confirmation form.
type DeleteInput struct {
	Env string
	// DeleteCounterpart also deletes the same vcluster in the other environment.
	DeleteCounterpart bool
	// DeleteGitlab also deletes the app-manifests GitLab project.
	DeleteGitlab bool
}

// DeleteResult reports the outcome. Async is true when the deletion was
// deferred behind a Rancher cleanup job — the handler must then launch
// runCleanupAndDelete(name, CleaningEnv, ...) itself, since that goroutine
// stays in the handler.
type DeleteResult struct {
	Name           string
	Env            string
	DeletePreprod  bool
	DeleteProd     bool
	DeleteGitlab   bool
	DeleteKeycloak bool
	Async          bool
	CleaningEnv    string
}

// Delete removes a vcluster through GitOps. Admin only.
//
// Rancher comes first: the cleanup job runs *inside* the vcluster, so
// unpairing after the vcluster is gone would never run. When the vcluster is
// still paired, this unpairs it and records the cleaning state, then returns
// Async=true so the caller can defer the actual deletion to a background
// goroutine that waits for the cleanup job to finish.
func (s *Service) Delete(ctx context.Context, actor models.Actor, name string, in DeleteInput) (DeleteResult, error) {
	if !actor.IsAdmin {
		return DeleteResult{}, ErrForbidden
	}
	env := in.Env
	env = envOrDefault(env)

	deletePreprod := env == "preprod" || (env == "prod" && in.DeleteCounterpart)
	deleteProd := env == "prod" || (env == "preprod" && in.DeleteCounterpart)
	deleteGitlab := in.DeleteGitlab
	// OIDC clients always go away with the vcluster when Keycloak is configured.
	deleteKeycloak := s.keycloak != nil

	if s.rancher != nil {
		for _, e := range []string{"preprod", "prod"} {
			if (e == "preprod" && !deletePreprod) || (e == "prod" && !deleteProd) {
				continue
			}
			if !s.cfg.RancherEnabledForEnv(e) {
				continue
			}
			if s.cfg.IsCleaning(name, e) {
				return DeleteResult{}, &CleaningError{Name: name, Env: e}
			}
			info, found, err := s.rancher.FindClusterByName(name)
			if err != nil || !found {
				continue
			}
			// Still paired: unpair now, then delete once the cleanup job is done.
			if err := s.rancher.DeleteCluster(info.ID); err != nil {
				return DeleteResult{}, &UnpairError{Err: err}
			}
			slog.Info("delete: Rancher cluster deleted, launching cleanup then deletion", "vcluster", name, "rancher_id", info.ID)
			s.cfg.AddCleaning(name, e, deletePreprod, deleteProd, deleteGitlab, deleteKeycloak)
			audit.LogActor(actor.Username, "delete", name, env)

			return DeleteResult{
				Name:           name,
				Env:            env,
				DeletePreprod:  deletePreprod,
				DeleteProd:     deleteProd,
				DeleteGitlab:   deleteGitlab,
				DeleteKeycloak: deleteKeycloak,
				Async:          true,
				CleaningEnv:    e,
			}, nil
		}
	}

	audit.LogActor(actor.Username, "delete", name, env)
	// PerformDeletion goes through GitOps and must complete even if the caller
	// gives up on ctx (an HTTP request whose client disconnected, in
	// particular) — detach it from a background context, like the async path
	// through runCleanupAndDelete already does.
	s.PerformDeletion(context.Background(), name, deletePreprod, deleteProd, deleteGitlab, deleteKeycloak)

	return DeleteResult{
		Name:           name,
		Env:            env,
		DeletePreprod:  deletePreprod,
		DeleteProd:     deleteProd,
		DeleteGitlab:   deleteGitlab,
		DeleteKeycloak: deleteKeycloak,
	}, nil
}

// PerformDeletion executes the deletion itself: K8s cleanup, fluxprod commits
// (or a prod MR), then GitLab / Keycloak / Vault teardown. Exported because
// the handler's runCleanupAndDelete goroutine (which waits for the Rancher
// cleanup job — an async concern that stays in the handler) calls it once
// that wait is over.
func (s *Service) PerformDeletion(ctx context.Context, name string, deletePreprod, deleteProd, deleteGitlab, deleteKeycloak bool) {
	// Clear K8s finalizers and lift namespace protection so FluxCD can actually
	// delete the namespace.
	for _, e := range []string{"preprod", "prod"} {
		if (e == "preprod" && !deletePreprod) || (e == "prod" && !deleteProd) {
			continue
		}
		k8s := s.k8sForEnv(e)
		if k8s == nil {
			continue
		}
		// Only lift the flag when we've actually confirmed it's set. A failed read
		// here isn't "not protected": clearing the annotation on a guess would
		// strip a safeguard we never actually saw, on nothing more than an API
		// hiccup.
		//
		// Be clear about what staying cautious costs, because it is NOT a free
		// "we'll get it next time". There is no next time: everything below runs
		// regardless — CleanupNamespace strips the Flux finalizers, the fluxprod
		// commits remove the vcluster's files, and GitLab/Keycloak/Vault are torn
		// down. The vcluster then vanishes from the dashboard, which is built from
		// the Git branch, so no UI path leads back to it. What's left is a
		// namespace still carrying protect-deletion and nothing in Git to reclaim
		// it — an orphan, to be finished by hand. Hence the Error, not a Warn: this
		// line is the only trace it will ever get.
		//
		// Whether that orphan actually survives depends on something this repo does
		// not contain: no policy here enforces protect-deletion, so if nothing
		// enforces it in the cluster either, the namespace goes away anyway and the
		// annotation was never a safeguard to begin with.
		protected, err := k8s.GetNamespaceProtection(ctx, name)
		if err != nil {
			slog.Error("suppression : protection du namespace illisible, laissée en place — "+
				"le vcluster est supprimé partout ailleurs, ce namespace est à reprendre à la main",
				"vcluster", name, "env", e, "err", err)
		} else if protected {
			if err := k8s.SetNamespaceProtection(ctx, name, false); err != nil {
				slog.Warn("disabling namespace-protection failed", "vcluster", name, "env", e, "err", err)
			} else {
				slog.Info("delete: namespace-protection disabled", "vcluster", name, "env", e)
			}
		}
		if err := k8s.CleanupNamespace(ctx, name); err != nil {
			slog.Warn("K8s cleanup failed", "vcluster", name, "env", e, "err", err)
		}
	}

	// 1. Preprod files + kustomization entry, on the preprod branch.
	if deletePreprod {
		preprodPath := fmt.Sprintf("%s/preprod/vclusters/%s", s.cfg.FluxprodClustersPath, name)
		preprodFiles, err := s.gitlab.ListFiles(ctx, gitops.SourceBranch, preprodPath)
		if err != nil {
			slog.Error("error listing preprod files", "vcluster", name, "err", err)
		}
		var actions []gitops.CommitAction
		for _, f := range preprodFiles {
			actions = append(actions, gitops.CommitAction{Action: "delete", Path: f})
		}
		kustAction, err := s.kustomizationAction(ctx, gitops.SourceBranch, "preprod", name, false)
		if err != nil {
			slog.Warn("could not update kustomization.yaml", "env", "preprod", "err", err)
		} else {
			actions = append(actions, kustAction)
		}
		if len(actions) > 0 {
			if err := s.gitlab.Commit(ctx, gitops.SourceBranch, fmt.Sprintf("feat: remove vcluster %s", name), actions); err != nil {
				slog.Error("error committing preprod deletion", "vcluster", name, "err", err)
				return
			}
			s.cfg.AddDeleting(name, "preprod", "")
			go s.notify(fmt.Sprintf("Suppression du vcluster *%s* (preprod) en cours...", name))
		}
	}

	// 2. Prod files: direct commit when still pending, MR once deployed.
	if deleteProd {
		prodPath := fmt.Sprintf("%s/prod/vclusters/%s", s.cfg.FluxprodClustersPath, name)
		prodFiles, err := s.gitlab.ListFiles(ctx, gitops.SourceBranch, prodPath)
		if err != nil {
			slog.Error("error listing prod files", "vcluster", name, "err", err)
		}
		if len(prodFiles) > 0 {
			isPending := s.isPendingProd(ctx, name)
			var actions []gitops.CommitAction
			for _, f := range prodFiles {
				actions = append(actions, gitops.CommitAction{Action: "delete", Path: f})
			}
			kustAction, err := s.kustomizationAction(ctx, "prod", "preprod", name, false)
			if err != nil {
				slog.Warn("could not update prod kustomization.yaml", "err", err)
			} else {
				actions = append(actions, kustAction)
			}

			if isPending {
				// Never deployed: no MR to open, and no HelmRelease to wait for — so
				// no AddDeleting either.
				if err := s.gitlab.Commit(ctx, gitops.SourceBranch, fmt.Sprintf("feat: remove vcluster %s (prod)", name), actions); err != nil {
					slog.Error("error deleting pending prod files", "vcluster", name, "err", err)
				}
			} else {
				mrURL, err := s.commitProdMRActions(
					ctx,
					fmt.Sprintf("feat: remove vcluster %s", name),
					actions,
				)
				if err != nil {
					slog.Error("error creating MR for prod deletion", "vcluster", name, "err", err)
				} else {
					slog.Info("MR created for prod deletion", "vcluster", name, "url", mrURL)
					s.cfg.AddDeleting(name, "prod", mrURL)
				}
			}
		}
	}

	if deleteGitlab {
		if err := s.gitlab.DeleteProject(name); err != nil {
			slog.Error("error deleting GitLab repo", "vcluster", name, "err", err)
		}
	}

	if deleteKeycloak && s.keycloak != nil {
		if err := s.keycloak.DeleteArgoCDClients(name); err != nil {
			slog.Error("error deleting Keycloak clients", "vcluster", name, "err", err)
		}
	}

	if s.vault != nil {
		if deletePreprod {
			if err := s.vault.DisableAuth(context.Background(), "kubernetes-vcluster-"+name+"-preprod"); err != nil {
				slog.Warn("vault cleanup failed", "env", "preprod", "vcluster", name, "err", err)
			}
		}
		if deleteProd {
			if err := s.vault.DisableAuth(context.Background(), "kubernetes-vcluster-"+name+"-prod"); err != nil {
				slog.Warn("vault cleanup failed", "env", "prod", "vcluster", name, "err", err)
			}
		}
	}
}

// --- shared gitops helpers -----------------------------------------------

// commitProdMRActions commits prod file changes to the preprod branch (source
// of truth), then gets or creates the standing MR preprod→master that
// promotes them.
func (s *Service) commitProdMRActions(ctx context.Context, commitMsg string, actions []gitops.CommitAction) (string, error) {
	if err := s.gitlab.Commit(ctx, gitops.SourceBranch, commitMsg, actions); err != nil {
		return "", fmt.Errorf("committing to preprod: %w", err)
	}

	mrNote := "Promotion des changements de preprod vers la production.\n\n" +
		"Créé automatiquement par vcluster-manager.\n\n---\n\n" +
		"> ℹ️ **Note sur le diff** : Ce MR contient des fichiers sous `clusters/preprod/` **et** `clusters/prod/`.\n" +
		"> Seuls les fichiers sous **`clusters/prod/`** ont un impact sur la production.\n" +
		"> Les fichiers `clusters/preprod/` sont présents car la branche **preprod est la source de vérité** pour les deux environnements."
	mrURL, err := s.gitlab.GetOrCreateMergeRequest(
		"preprod", "master",
		"feat: promote preprod to prod",
		mrNote,
	)
	if err != nil {
		return "", fmt.Errorf("creating MR: %w", err)
	}
	return mrURL, nil
}

// kustomizationAction reads the cluster kustomization.yaml and returns the
// commit action adding or removing the vcluster entry.
func (s *Service) kustomizationAction(ctx context.Context, env, branch, name string, add bool) (gitops.CommitAction, error) {
	kustPath := fmt.Sprintf("%s/%s/kustomization.yaml", s.cfg.FluxprodClustersPath, env)
	content, err := s.gitlab.GetFile(ctx, branch, kustPath)
	if err != nil {
		return gitops.CommitAction{}, fmt.Errorf("reading %s: %w", kustPath, err)
	}
	return gitops.CommitAction{
		Action:  "update",
		Path:    kustPath,
		Content: gitops.UpdateKustomization(content, name, add),
	}, nil
}

// isPendingProd reports whether a prod vcluster exists on preprod but not yet
// on master.
func (s *Service) isPendingProd(ctx context.Context, name string) bool {
	for _, n := range s.parser.ListVClusterNamesOnBranch(ctx, "master", "prod") {
		if n == name {
			return false
		}
	}
	return true
}

// notify fires a webhook notification when one is configured. Errors are
// logged only — a missed notification must never break a deletion.
func (s *Service) notify(text string) {
	if s.notifier == nil {
		return
	}
	if err := s.notifier.Send(context.Background(), text); err != nil {
		slog.Warn("webhook notification failed", "err", err)
	}
}
