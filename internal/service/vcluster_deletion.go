package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/gmalfray/vcluster-manager/internal/audit"
	"github.com/gmalfray/vcluster-manager/internal/models"
)

// Ce fichier porte la séquence de suppression telle que le finalizer la joue
// (crd-vcluster.md §4.4). C'est le même enchaînement que PerformDeletion —
// dépairage Rancher, sauvegarde, retrait de la protection, destruction — coupé
// autrement : chaque étape est soit une **observation** sans effet de bord, soit
// une **action idempotente**, jamais les deux dans le même appel.
//
// Pourquoi ce découpage plutôt qu'un appel bloquant : le contrôleur doit pouvoir
// être tué entre deux étapes et reprendre. Il reprend en regardant l'état réel
// (Rancher connaît-il encore ce cluster ? une sauvegarde tourne-t-elle ? le
// namespace est-il encore protégé ?) plutôt qu'en relisant un registre écrit
// d'avance par le process qui vient de mourir. La leçon vient du contrôleur
// backup/restore, où un registre d'étapes s'est trompé précisément dans le cas
// où il était censé sauver (docs/poc-operator-tech-decision.md §5bis).
//
// Ce que la séquence ne fait PAS : commiter dans fluxprod. Sur le modèle C, le
// commit qui supprime le fichier du CR est ce qui a déclenché le
// deletionTimestamp — il a déjà eu lieu. PerformDeletion, qui commite, reste le
// chemin du monolithe tant que l'app n'est pas basculée.

// IsTerminalBackupPhase reports whether a Velero backup phase is settled. The
// restore counterpart is IsTerminalRestorePhase, next door in velero.go.
func IsTerminalBackupPhase(phase string) bool {
	switch phase {
	case "Completed", "Failed", "PartiallyFailed", "FailedValidation", "Deleting":
		return true
	}
	return false
}

// CleanupJobState is the observed state of the rancher-cleanup job, which runs
// *inside* the vcluster.
type CleanupJobState struct {
	// Observable is false when the vcluster's API does not answer. During the
	// grace period it is scaled to zero replicas, so this is the common case
	// rather than an anomaly: the job can then neither be deployed nor read, and
	// the caller has to decide on its deadline instead of waiting for a verdict
	// that will never come.
	Observable bool
	Found      bool
	Done       bool
	Failed     bool
	StartedAt  time.Time
	Detail     string
}

// RancherTeardownState is what Rancher and the vcluster say about the unpairing,
// at the moment we look.
type RancherTeardownState struct {
	// Enabled: Rancher is configured and turned on for this cell.
	Enabled bool
	// NotConfigured: ce processus n'a pas de client Rancher du tout.
	//
	// Distinct d'Enabled=false pour exactement la raison qui rend LookupFailed
	// distinct de StillKnown=false, un cran plus haut : « Rancher est éteint sur
	// cette cell » veut dire qu'il n'y a rien à dépairer, alors que « je n'ai pas
	// de client » veut dire que je ne peux pas savoir. Les confondre fait
	// rapporter l'étape comme faite par un binaire incapable de l'exécuter, et
	// laisse un cluster fantôme dans Rancher sans une ligne pour le dire.
	NotConfigured bool
	// LookupFailed: Rancher did not answer. Distinct from StillKnown=false —
	// treating an unreachable Rancher as "already unpaired" would skip the
	// unpairing entirely and leave a ghost cluster behind.
	LookupFailed bool
	// StillKnown: Rancher still has this cluster in its inventory, or Rancher
	// agents are still running inside the vcluster.
	StillKnown bool
	// Removing: Rancher has already taken the deletion into account, so asking
	// again would be noise.
	Removing bool
	Cleanup  CleanupJobState
	// Detail is the readable reason, for the controller's condition.
	Detail string
}

// InspectRancherTeardown observes where the unpairing stands. Read-only, no
// privilege required, no error returned: a failed lookup is projected onto
// LookupFailed so the caller always gets something to decide on.
func (s *Service) InspectRancherTeardown(ctx context.Context, name, env string) RancherTeardownState {
	env = envOrDefault(env)
	// NotConfigured seulement si la cell annonce Rancher actif. Un client absent
	// sur une cell sans Rancher n'est pas une mauvaise configuration : « il n'y a
	// rien à dépairer » y est un fait, et le signaler ferait crier l'étape sur
	// toute installation qui n'utilise pas Rancher.
	if s.rancher == nil && s.cfg.RancherEnabledForEnv(env) {
		return RancherTeardownState{
			NotConfigured: true,
			Detail:        "ce processus n'a pas de client Rancher : le dépairage ne peut pas être effectué ni vérifié",
		}
	}
	if s.rancher == nil || !s.cfg.RancherEnabledForEnv(env) {
		return RancherTeardownState{Detail: "Rancher n'est pas actif sur cette cell"}
	}
	st := RancherTeardownState{Enabled: true}

	info, found, err := s.rancher.FindClusterByName(name)
	if err != nil {
		st.LookupFailed = true
		st.Detail = "Rancher injoignable : " + err.Error()
		return st
	}
	if found {
		st.StillKnown = true
		st.Removing = info.State == "removing"
		st.Detail = "Rancher connaît encore le cluster (état " + info.State + ")"
	}

	k8s := s.k8sForEnv(env)
	if k8s == nil {
		st.Detail = "pas de client Kubernetes pour " + env
		return st
	}
	// Un dépairage manuel sous un autre nom ne se voit pas dans l'inventaire
	// Rancher, seulement aux agents synchronisés dans le namespace hôte.
	if !st.StillKnown && k8s.HasRancherAgents(ctx, name) {
		st.StillKnown = true
		st.Detail = "des agents Rancher tournent encore dans le vcluster"
	}
	if st.StillKnown {
		// Tant que le dépairage n'est pas passé, l'état du job ne sert à rien, et
		// aller le lire coûte un port-forward vers un vcluster qui est peut-être
		// éteint. On ne regarde que ce dont on a besoin pour décider.
		return st
	}

	job, err := k8s.GetVClusterJobState(ctx, name, "rancher-cleanup", "kube-system")
	if err != nil {
		st.Cleanup.Detail = err.Error()
		return st
	}
	st.Cleanup = CleanupJobState{
		Observable: true,
		Found:      job.Found,
		Done:       job.Complete,
		Failed:     job.Failed,
		StartedAt:  job.StartedAt,
		Detail:     job.Detail,
	}
	return st
}

// UnpairForDeletion removes the vcluster from Rancher and drops the cleanup job
// inside it. Admin only.
//
// Idempotent, because a finalizer replays: the Rancher deletion is skipped when
// Rancher no longer has the cluster (or is already removing it), and dropping
// the job is best-effort — during the grace period the vcluster is off, so
// there is nothing to deploy into. That is not a deletion failure, which is why
// it does not surface as an error: the current code already treats a missing
// cleanup as a warning and carries on.
func (s *Service) UnpairForDeletion(ctx context.Context, actor models.Actor, name, env string) error {
	if !actor.IsAdmin {
		return ErrForbidden
	}
	if !validName(name) {
		return ErrInvalidName
	}
	env = envOrDefault(env)
	// « Pas de client » ne peut pas rendre nil : pour l'appelant, nil veut dire
	// « le dépairage est passé ». Le finalizer posait donc la condition
	// RancherPaired/Unpairing et avançait, sur un dépairage qui n'avait jamais eu
	// lieu. Une erreur, au moins, se voit.
	if s.rancher == nil && s.cfg.RancherEnabledForEnv(env) {
		return ErrRancherNotConfigured
	}
	if s.rancher == nil || !s.cfg.RancherEnabledForEnv(env) {
		return nil
	}

	info, found, err := s.rancher.FindClusterByName(name)
	if err != nil {
		return &RancherOpError{Op: "lookup", Err: err}
	}
	if found && info.State != "removing" {
		if err := s.rancher.DeleteCluster(info.ID); err != nil {
			return &RancherOpError{Op: "delete", Err: err}
		}
		audit.LogActor(actor.Username, "unpair-rancher-delete", name, env)
		slog.Info("suppression : cluster retiré de Rancher", "vcluster", name, "cell", env, "rancher_id", info.ID)
	}

	// Le nettoyage tourne DANS le vcluster, donc il doit être déposé avant que
	// le vcluster disparaisse — c'est toute la raison pour laquelle cette étape
	// passe avant la destruction.
	k8s := s.k8sForEnv(env)
	if k8s == nil {
		return nil
	}
	if err := k8s.ApplyManifestToVClusterViaPortForward(ctx, name, []byte(RancherCleanupManifest)); err != nil {
		slog.Info("suppression : rancher-cleanup pas déposé, le vcluster ne répond pas", "vcluster", name, "cell", env, "err", err)
	}
	return nil
}

// DeletionBackupState is the Velero backup that covers this deletion, if there
// is one.
type DeletionBackupState struct {
	Found     bool
	Name      string
	Phase     string
	StartedAt time.Time
	Completed bool
	Failed    bool
}

// InspectDeletionBackup looks for the backup that covers a deletion, reading
// Velero rather than a name written down beforehand.
//
// Two things count as covering it, and both are directly observable:
//
//   - a backup of this vcluster that has not settled yet — it is adopted and
//     followed, exactly as an interrupted restore is adopted rather than taken
//     for a loss;
//   - a completed backup that started at or after `since` (the deletion
//     timestamp). A nightly backup from before the deletion does not qualify:
//     ADR-001 asks for a backup before destruction, not for the most recent one
//     lying around.
//
// A backup with no readable timestamp is ignored, which errs towards taking one
// backup too many rather than destroying without one.
func (s *Service) InspectDeletionBackup(ctx context.Context, name, env string, since time.Time) (DeletionBackupState, error) {
	if !validName(name) {
		return DeletionBackupState{}, ErrInvalidName
	}
	env = envOrDefault(env)
	k8s := s.k8sForEnv(env)
	if k8s == nil {
		return DeletionBackupState{}, ErrK8sUnavailable
	}

	backups, err := k8s.ListVeleroBackups(ctx, name, s.cfg.VeleroNamespace)
	if err != nil {
		return DeletionBackupState{}, err
	}
	return pickDeletionBackup(backups, since), nil
}

// pickDeletionBackup est la règle de sélection, sortie de son appel pour être
// vérifiable sans cluster : c'est elle qui décide si la destruction a un filet.
func pickDeletionBackup(backups []models.VeleroBackupInfo, since time.Time) DeletionBackupState {
	var running, completed, failed *DeletionBackupState
	for _, b := range backups {
		started := parseVeleroTime(b.StartTime)
		st := DeletionBackupState{Found: true, Name: b.Name, Phase: b.Phase, StartedAt: started}

		if !IsTerminalBackupPhase(b.Phase) {
			if running == nil || started.After(running.StartedAt) {
				cp := st
				running = &cp
			}
			continue
		}
		// Terminée : elle ne compte que si elle appartient à cette suppression.
		when := started
		if when.IsZero() {
			when = parseVeleroTime(b.CompletionTime)
		}
		if when.IsZero() || when.Before(since) {
			continue
		}
		st.StartedAt = when
		if b.Phase == "Completed" {
			st.Completed = true
			if completed == nil || when.After(completed.StartedAt) {
				cp := st
				completed = &cp
			}
			continue
		}
		st.Failed = true
		if failed == nil || when.After(failed.StartedAt) {
			cp := st
			failed = &cp
		}
	}

	// Une sauvegarde en cours prime : elle est encore en train de décider du
	// sort de la donnée, il n'y a rien à conclure avant qu'elle ait fini.
	switch {
	case running != nil:
		return *running
	case completed != nil:
		return *completed
	case failed != nil:
		return *failed
	}
	return DeletionBackupState{}
}

func parseVeleroTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// TeardownOptions carries the choices that used to be checkboxes on the delete
// form and have nowhere to live on a CR being deleted.
type TeardownOptions struct {
	// DeleteAppManifestsRepo also removes the vcluster's app-manifests GitLab
	// repo. False by default, deliberately: the repo is data, and data outliving
	// its vcluster is recoverable where a repo deleted by surprise is not.
	DeleteAppManifestsRepo bool
}

// TeardownVCluster is the last step: clear what would keep the namespace from
// going away, then tear down what lives outside Kubernetes. Admin only.
//
// The error and the warnings do not mean the same thing, and that is the point.
// The error is for what must succeed before the finalizer lets go — leaving Flux
// finalizers behind strands the namespace in Terminating, and with it the CR.
// The warnings are for the external systems: an orphaned Keycloak client is
// annoying and fixable by hand, whereas an object stuck in Terminating because
// Keycloak was down for a minute is neither.
func (s *Service) TeardownVCluster(ctx context.Context, actor models.Actor, name, env string, opts TeardownOptions) ([]string, error) {
	if !actor.IsAdmin {
		return nil, ErrForbidden
	}
	if !validName(name) {
		return nil, ErrInvalidName
	}
	env = envOrDefault(env)
	k8s := s.k8sForEnv(env)
	if k8s == nil {
		return nil, ErrK8sUnavailable
	}

	if err := k8s.CleanupNamespace(ctx, name); err != nil {
		return nil, fmt.Errorf("nettoyage du namespace vcluster-%s : %w", name, err)
	}

	var warnings []string
	warn := func(what string, err error) {
		slog.Warn("suppression : "+what, "vcluster", name, "cell", env, "err", err)
		warnings = append(warnings, what+" : "+err.Error())
	}
	absent := func(what string) {
		slog.Warn("suppression : étape non exécutable, client absent de ce processus",
			"vcluster", name, "cell", env, "etape", what)
		warnings = append(warnings, what+" : non traité, ce processus n'a pas le client correspondant")
	}

	// Un client absent produit un AVERTISSEMENT, pas un saut silencieux.
	//
	// Ces `!= nil` ont été écrits pour l'app web, où les clients sont toujours là.
	// Le finalizer les a hérités, et l'opérateur, lui, tourne avec un Deps
	// volontairement partiel : les trois branches ci-dessous ne s'exécutaient donc
	// jamais, et le status annonçait quand même « séquence de suppression
	// terminée ». Résultat sur un vcluster client : clients OIDC et backend d'auth
	// orphelins, invisibles.
	if s.keycloak == nil {
		absent("clients OIDC Keycloak")
	} else if err := s.keycloak.DeleteArgoCDClients(name); err != nil {
		warn("clients OIDC Keycloak pas supprimés", err)
	}

	if s.vault == nil {
		absent("backend d'auth Vault")
	} else if err := s.vault.DisableAuth(ctx, "kubernetes-vcluster-"+name+"-"+env); err != nil {
		warn("backend d'auth Vault pas désactivé", err)
	}

	if opts.DeleteAppManifestsRepo {
		// Le pire cas des trois : la suppression du dépôt est un geste EXPLICITE,
		// demandé par annotation. Sans avertissement, il devenait un no-op qui
		// rapportait un succès.
		if s.gitlab == nil {
			absent("dépôt app-manifests (suppression demandée par annotation)")
		} else if err := s.gitlab.DeleteProject(name); err != nil {
			warn("dépôt app-manifests pas supprimé", err)
		}
	}

	audit.LogActor(actor.Username, "vcluster-teardown", name, env)
	return warnings, nil
}

// DeleteHostNamespace supprime le namespace hôte du vcluster. Admin only, comme
// TeardownVCluster dont elle est la suite.
//
// Elle est séparée de TeardownVCluster, et non fondue dedans, parce qu'elle ne
// se conclut pas de la même façon : TeardownVCluster agit sur des systèmes qui
// répondent tout de suite (Keycloak, Vault, GitLab), alors qu'une suppression de
// namespace ne fait que POSER un deletionTimestamp. Ce qui la termine est une
// observation ultérieure — HostNamespaceState — et cette attente appartient à
// l'appelant.
func (s *Service) DeleteHostNamespace(ctx context.Context, actor models.Actor, name, env string) error {
	if !actor.IsAdmin {
		return ErrForbidden
	}
	if !validName(name) {
		return ErrInvalidName
	}
	env = envOrDefault(env)
	k8s := s.k8sForEnv(env)
	if k8s == nil {
		return ErrK8sUnavailable
	}
	requested, err := k8s.DeleteHostNamespace(ctx, name)
	if err != nil {
		return err
	}
	// Pas de ligne d'audit pour un namespace qui n'était pas là. Le finalizer
	// rejoue cette étape à chaque tour, donc journaliser sans condition remplirait
	// l'audit de suppressions qui n'ont rien supprimé — et rendrait illisible la
	// seule qui compte.
	if requested {
		audit.LogActor(actor.Username, "vcluster-namespace-delete", name, env)
	}
	return nil
}

// HostNamespaceState dit si le namespace hôte du vcluster existe, et si on a pu
// le savoir.
//
// Sert au finalizer à distinguer « ce vcluster n'a jamais été matérialisé » de
// « je n'arrive pas à regarder ». Le premier n'a pas de données à sauvegarder ; le
// second doit garder le filet.
//
// Pas dérivé du status : `chartVersion` vide voudrait dire « jamais provisionné »
// dans la plupart des cas, mais il serait aussi vide sur un vcluster dont
// l'observation n'a jamais abouti — donc éventuellement sur un vcluster qui porte
// des données. Pour un garde-fou de données, la question se pose au moment de la
// suppression, au cluster.
func (s *Service) HostNamespaceState(ctx context.Context, name, env string) (exists, known bool) {
	if !validName(name) {
		return false, false
	}
	k8s := s.k8sForEnv(envOrDefault(env))
	if k8s == nil {
		return false, false
	}
	return k8s.HostNamespaceExists(ctx, name)
}
