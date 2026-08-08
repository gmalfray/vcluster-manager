package controller

import (
	"context"
	"strings"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/gmalfray/vcluster-manager/internal/audit"
	"github.com/gmalfray/vcluster-manager/internal/models"
	"github.com/gmalfray/vcluster-manager/internal/service"

	"github.com/gmalfray/vcluster-manager/api/v1alpha1"
)

// VClusterFinalizer retient le CR le temps que la séquence de suppression se
// déroule (crd-vcluster.md §4.4).
//
// Il ne rend pas la suppression annulable : une fois `deletionTimestamp` posé,
// Kubernetes ne revient pas en arrière, finalizer ou pas. Ce qui est annulable
// se joue avant, sur `spec.suspend` (§4.2) — deux mécanismes, à ne pas
// confondre.
const VClusterFinalizer = "vcluster.rebuild-it.fr/finalizer"

// Bornes de la séquence. Une étape qui n'aboutit pas doit finir par le dire :
// le contrôleur voisin réessayait une reprise de Flux toutes les dix secondes
// indéfiniment, sans que personne ne l'apprenne
// (docs/poc-operator-tech-decision.md §5bis, bug 1).
//
// Les deux bornes ne mènent pas au même endroit, et c'est délibéré. Un
// nettoyage Rancher qui traîne ne doit pas empêcher une suppression demandée —
// le code actuel en fait déjà un simple avertissement. Une sauvegarde manquante
// arrête tout : c'est le filet, pas un détail d'exploitation.
const (
	rancherTeardownGiveUpAfter = 10 * time.Minute
	backupGiveUpAfter          = 2 * time.Hour
	deletionStepRequeue        = 30 * time.Second
	// namespaceRemovalGiveUpAfter borne l'attente de disparition du namespace.
	//
	// Dix minutes comme Rancher, et pour la même raison : au-delà, ce qui retient
	// le namespace n'est plus un délai de terminaison mais quelque chose qui le
	// retient POUR DE BON — un finalizer tiers, ou la Kustomization Flux du tenant
	// qui le réapplique aussi vite qu'on le supprime. Insister n'y changerait
	// rien ; ce qu'il faut alors, c'est le dire.
	namespaceRemovalGiveUpAfter = 10 * time.Minute
)

// Étapes écrites dans `status.deletion.stage`.
//
// Elles sont là POUR ÊTRE LUES PAR UN HUMAIN : un `kubectl get vc -o yaml` sur
// un objet coincé doit dire où il est coincé. Le contrôleur, lui, ne les relit
// jamais — il redemande au cluster. C'est la conclusion de la 3ᵉ passe du POC :
// sur quatre étapes persistées, une seule discriminait, et elle se trompait
// exactement dans le cas qu'elle devait couvrir. Prendre une décision à partir
// de `Stage` réintroduirait ce bug.
const (
	stageBlocked           = "Blocked"
	stageRancherUnpairing  = "RancherUnpairing"
	stageBackupPending     = "BackupPending"
	stageProtectionRemoval = "ProtectionRemoval"
	stageDestroying        = "Destroying"
)

// reasonBackupProgress distingue « une sauvegarde a été lancée » de « on n'a pas
// réussi à lire Velero », deux situations qui portent le même statut Unknown et
// n'appellent pas la même suite.
const reasonBackupProgress = "InProgress"

// VClusterDeletionOps est la tranche du service que le finalizer consomme.
//
// Elle reste déclarée à part de VClusterOps — un seam par étape, plus lisible
// qu'une seule grosse interface — mais le champ `Ops` du reconciler porte
// `VClusterServiceOps` (vcluster_controller.go), qui les fusionne toutes. C'est
// ce qui garantit ici que `r.Ops` implémente VClusterDeletionOps sans assertion
// de type à l'exécution.
type VClusterDeletionOps interface {
	// InspectRancherTeardown observe où en est le dépairage, sans rien changer.
	InspectRancherTeardown(ctx context.Context, name, env string) service.RancherTeardownState
	// UnpairForDeletion retire le cluster de Rancher et dépose le job de
	// nettoyage dans le vcluster. Idempotent.
	UnpairForDeletion(ctx context.Context, actor models.Actor, name, env string) error

	// InspectDeletionBackup cherche dans Velero la sauvegarde qui couvre cette
	// suppression, plutôt que de relire un nom noté d'avance.
	InspectDeletionBackup(ctx context.Context, name, env string, since time.Time) (service.DeletionBackupState, error)
	// TriggerVeleroBackup lance la sauvegarde d'avant destruction.
	TriggerVeleroBackup(ctx context.Context, actor models.Actor, name, env string) (service.VeleroBackupCreated, error)

	// GetProtection / SetProtection : l'annotation protect-deletion du namespace
	// hôte, qui tombe juste avant la destruction et pas plus tôt.
	GetProtection(ctx context.Context, name, env string) service.ProtectionState
	SetProtection(ctx context.Context, actor models.Actor, name, env string, enabled bool) (service.ProtectionState, error)

	// HostNamespaceState dit si le namespace hôte existe, et si on a pu le savoir.
	// Un vcluster jamais matérialisé n'a pas de données à sauvegarder ; et c'est
	// aussi ce qui CONSTATE la fin de la suppression.
	HostNamespaceState(ctx context.Context, name, env string) (exists, known bool)

	// DeleteHostNamespace supprime le namespace du vcluster. Idempotente : elle ne
	// conclut rien, elle demande — c'est HostNamespaceState qui conclut.
	DeleteHostNamespace(ctx context.Context, actor models.Actor, name, env string) error

	// TeardownVCluster détruit : finalizers Flux du namespace, puis Keycloak,
	// Vault et éventuellement le dépôt app-manifests.
	TeardownVCluster(ctx context.Context, actor models.Actor, name, env string, opts service.TeardownOptions) ([]string, error)
}

var _ VClusterDeletionOps = (*service.Service)(nil)

// ensureFinalizer pose le finalizer sur le chemin vivant.
//
// Il faut que ce soit là et pas ailleurs : l'API server refuse d'ajouter un
// finalizer à un objet qui porte déjà un `deletionTimestamp`, donc un finalizer
// posé au moment de la suppression arriverait toujours trop tard.
func (r *VClusterReconciler) ensureFinalizer(ctx context.Context, vc *v1alpha1.VCluster) error {
	if controllerutil.ContainsFinalizer(vc, VClusterFinalizer) {
		return nil
	}
	controllerutil.AddFinalizer(vc, VClusterFinalizer)
	// Update et pas Status().Update : un finalizer vit dans metadata. C'est la
	// seule écriture du contrôleur hors sous-ressource status, et la seule
	// raison pour laquelle son RBAC a besoin d'`update` sur les vclusters.
	return r.Update(ctx, vc)
}

// reconcileDeletion joue la séquence de suppression et, seulement quand elle est
// finie, lâche l'objet.
//
// Rien ici ne dépend de ce qu'un reconcile précédent aurait gardé en mémoire :
// un opérateur redémarré au milieu reprend en regardant le cluster. Les tests
// le vérifient avec des reconcilers neufs, ce qui est un redémarrage fidèle.
func (r *VClusterReconciler) reconcileDeletion(ctx context.Context, vc *v1alpha1.VCluster) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(vc, VClusterFinalizer) {
		// Rien ne retient l'objet : Kubernetes finit sans nous, et écrire un
		// status sur un objet qui n'existera plus dans la seconde ne servirait
		// personne.
		return ctrl.Result{}, nil
	}
	if vc.Status.Deletion == nil {
		vc.Status.Deletion = &v1alpha1.DeletionStatus{}
	}
	vc.Status.Phase = v1alpha1.VClusterPhaseDeleting

	done, requeue, seqErr := r.runDeletionSequence(ctx, r.Ops, vc)

	// Le status part avant le retrait du finalizer : après, l'objet n'existe
	// plus et il n'y a plus rien où écrire.
	if err := r.Status().Update(ctx, vc); err != nil {
		return ctrl.Result{}, err
	}
	if seqErr != nil {
		return ctrl.Result{}, seqErr
	}
	if !done {
		return ctrl.Result{RequeueAfter: requeue}, nil
	}

	controllerutil.RemoveFinalizer(vc, VClusterFinalizer)
	if err := r.Update(ctx, vc); err != nil {
		return ctrl.Result{}, err
	}
	log.FromContext(ctx).Info("vcluster supprimé, finalizer retiré", "vcluster", vc.Name, "cell", r.Cell)
	return ctrl.Result{}, nil
}

// deletionRun porte ce que deux étapes d'un MÊME tour se transmettent — en
// l'occurrence les restes que le teardown signale et que la conclusion, écrite
// une étape plus loin, doit reprendre.
//
// Il est alloué par tour et passé en paramètre, jamais posé en champ du
// reconciler : le manager réconcilie plusieurs vclusters en parallèle sur la même
// instance, et un champ partagé attribuerait les restes de l'un au status de
// l'autre — quand il ne les corromprait pas franchement.
//
// Ce n'est pas un registre au sens que la doctrine interdit : rien ici ne
// survit au tour, et aucune décision ne s'y prend. Ce qui décide se relit
// toujours au cluster.
type deletionRun struct {
	teardownWarnings []string
}

// runDeletionSequence enchaîne les étapes de §4.4 dans l'ordre. Chacune commence
// par constater si elle a déjà été faite, donc rejouer la séquence ne redétruit
// rien et ne se bloque pas.
func (r *VClusterReconciler) runDeletionSequence(ctx context.Context, ops VClusterDeletionOps, vc *v1alpha1.VCluster) (bool, time.Duration, error) {
	run := &deletionRun{}
	steps := []func(context.Context, VClusterDeletionOps, *v1alpha1.VCluster, *deletionRun) (bool, time.Duration, error){
		r.checkDeletionProtection,
		r.reconcileRancherTeardown,
		r.reconcileDeletionBackup,
		r.reconcileFinalTeardown,
		r.reconcileNamespaceRemoval,
	}
	for _, step := range steps {
		done, requeue, err := step(ctx, ops, vc, run)
		if err != nil || !done {
			return false, requeue, err
		}
	}
	return true, 0, nil
}

// checkDeletionProtection est le garde-fou de §4.3, revérifié au moment le plus
// tardif possible.
//
// C'est ce qui rend le retrait de la merge request acceptable (ADR-001,
// condition de bascule) : la protection n'est pas seulement lue dans un diff
// pendant la revue, elle est relue ici, alors que l'objet est déjà en
// Terminating et qu'il ne reste que la destruction à faire.
func (r *VClusterReconciler) checkDeletionProtection(_ context.Context, _ VClusterDeletionOps, vc *v1alpha1.VCluster, _ *deletionRun) (bool, time.Duration, error) {
	if !vc.Spec.DeletionProtection {
		setVClusterCond(vc, v1alpha1.CondDeletionProtected, metav1.ConditionFalse, "ProtectionLifted",
			"protection levée, la séquence de suppression peut se dérouler")
		return true, 0, nil
	}

	msg := "spec.deletionProtection est encore à true : la séquence s'arrête avant de détruire quoi que ce soit. " +
		"L'objet reste en Terminating tant que la protection n'est pas levée, et un deletionTimestamp ne s'annule pas — " +
		"pour garder ce vcluster, remettre son CR dans fluxprod avant de lever la protection."
	vc.Status.Deletion.Stage = stageBlocked
	vc.Status.Deletion.Message = msg
	setVClusterCond(vc, v1alpha1.CondDeletionProtected, metav1.ConditionTrue, "DeletionProtectionBlocked", msg)
	setVClusterCond(vc, v1alpha1.CondVClusterReady, metav1.ConditionFalse, "DeletionProtectionBlocked",
		"suppression bloquée par la protection, rien n'a été détruit")

	// Pas de requeue : ce qui débloque est un changement de spec, et le watch le
	// rapporte. Réessayer en boucle n'apprendrait rien et noierait l'arrêt dans
	// du bruit — l'arrêt, lui, est écrit noir sur blanc dans la condition.
	return false, 0, nil
}

// reconcileRancherTeardown dépaire et laisse le job de nettoyage tourner *dans*
// le vcluster, avant que celui-ci disparaisse (§4.4 étape 1).
func (r *VClusterReconciler) reconcileRancherTeardown(ctx context.Context, ops VClusterDeletionOps, vc *v1alpha1.VCluster, _ *deletionRun) (bool, time.Duration, error) {
	st := ops.InspectRancherTeardown(ctx, vc.Name, r.Cell)
	if st.NotConfigured {
		// On continue — un CR ne doit pas rester coincé en Terminating parce que
		// l'opérateur est mal câblé — mais on l'écrit. Sans cette condition,
		// l'étape se rapportait comme faite et le cluster restait dans Rancher
		// sans que personne ne l'apprenne.
		setVClusterCond(vc, v1alpha1.CondRancherPaired, metav1.ConditionUnknown, "NoRancherClient",
			st.Detail+" ; la suppression continue, le cluster est à retirer à la main dans Rancher")
		return true, 0, nil
	}
	if !st.Enabled {
		return true, 0, nil
	}
	vc.Status.Deletion.Stage = stageRancherUnpairing

	// Tant que Rancher n'a pas confirmé qu'il ne connaît plus ce vcluster, on
	// insiste. La borne s'ancre sur la condition, qui garde sa
	// LastTransitionTime tant que son statut ne change pas — donc « depuis quand
	// on insiste » vit dans le status et survit à un redémarrage.
	if st.LookupFailed || st.StillKnown {
		if r.overdue(vc, v1alpha1.CondRancherPaired, metav1.ConditionTrue, rancherTeardownGiveUpAfter) {
			// Un système tiers en panne ne doit pas laisser un CR coincé en
			// Terminating pour toujours. On continue, et on écrit ce qui reste à
			// faire à la main.
			reason, msg := "UnpairTimedOut", "Rancher connaît toujours ce vcluster après "+rancherTeardownGiveUpAfter.String()
			if st.LookupFailed {
				reason, msg = "UnpairUnconfirmed", "dépairage non confirmé après "+rancherTeardownGiveUpAfter.String()+" — "+st.Detail
			}
			setVClusterCond(vc, v1alpha1.CondRancherPaired, metav1.ConditionUnknown, reason,
				msg+" ; la suppression continue, le cluster est à retirer à la main dans Rancher")
			return true, 0, nil
		}
		if err := ops.UnpairForDeletion(ctx, SystemActor, vc.Name, r.Cell); err != nil {
			// Même statut True que la ligne suivante : l'ancre de délai ne bouge
			// pas d'un tour à l'autre selon qu'un appel a échoué ou non.
			setVClusterCond(vc, v1alpha1.CondRancherPaired, metav1.ConditionTrue, "UnpairFailed", err.Error())
			return false, deletionStepRequeue, err
		}
		setVClusterCond(vc, v1alpha1.CondRancherPaired, metav1.ConditionTrue, "Unpairing",
			"dépairage demandé, nettoyage en cours dans le vcluster")
		return false, deletionStepRequeue, nil
	}

	// Dépairé. Reste le job de nettoyage, qu'on n'attend que tant qu'il est
	// visible : il porte un ttlSecondsAfterFinished et s'efface tout seul, donc
	// « absent après un dépairage » veut dire « il a fait son travail, ou il n'a
	// jamais pu être déposé », pas « il va arriver ».
	if st.Cleanup.Found && !st.Cleanup.Done && !st.Cleanup.Failed {
		anchor := st.Cleanup.StartedAt
		if anchor.IsZero() {
			anchor = vc.DeletionTimestamp.Time
		}
		if time.Since(anchor) > rancherTeardownGiveUpAfter {
			setVClusterCond(vc, v1alpha1.CondRancherPaired, metav1.ConditionFalse, "CleanupTimedOut",
				"le job rancher-cleanup tourne depuis plus de "+rancherTeardownGiveUpAfter.String()+
					" : la suppression continue sans attendre sa fin")
			return true, 0, nil
		}
		setVClusterCond(vc, v1alpha1.CondRancherPaired, metav1.ConditionFalse, "CleanupRunning",
			"dépairé ; le job rancher-cleanup tourne encore dans le vcluster")
		return false, deletionStepRequeue, nil
	}

	switch {
	case st.Cleanup.Failed:
		setVClusterCond(vc, v1alpha1.CondRancherPaired, metav1.ConditionFalse, "CleanupFailed",
			"dépairé, mais le job rancher-cleanup a échoué : "+st.Cleanup.Detail)
	case !st.Cleanup.Observable:
		setVClusterCond(vc, v1alpha1.CondRancherPaired, metav1.ConditionFalse, "UnpairedCleanupUnobserved",
			"dépairé ; le nettoyage interne n'a pas pu être observé, le vcluster ne répond pas (il est à zéro réplique)")
	default:
		setVClusterCond(vc, v1alpha1.CondRancherPaired, metav1.ConditionFalse, "Unpaired",
			"Rancher ne connaît plus ce vcluster")
	}
	return true, 0, nil
}

// reconcileDeletionBackup exige une sauvegarde terminée avant de détruire
// (§4.4 étape 2, ADR-001 « se prémunir de la suppression par la réversibilité »
// point 2).
//
// La sauvegarde qui compte n'est notée nulle part, elle est retrouvée dans
// Velero : soit une sauvegarde de ce vcluster tourne encore et on l'adopte, soit
// une sauvegarde terminée a démarré après le deletionTimestamp. Un contrôleur
// tué juste après avoir lancé la sauvegarde n'en relance donc pas une deuxième.
// overrideDisarms dit si la valeur de l'annotation lève réellement l'exigence de
// sauvegarde.
//
// Il ne suffit PAS que l'annotation soit présente. En faisant porter au champ le
// nom du décideur plutôt qu'un « true », on a rendu toute valeur non vide
// désarmante — y compris celles qui veulent dire non. `backup-override: "false"`
// détruisait donc sans sauvegarde, et la ligne d'audit annonçait « sauvegarde
// sautée sur décision de false ». Un garde-fou qu'on lève en écrivant « non »
// est pire que pas de garde-fou : il donne l'impression d'avoir refusé.
//
// La casse et les espaces sont normalisés parce que le geste se fait à la main,
// sous pression, sur un objet déjà en Terminating.
func overrideDisarms(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "false", "no", "non", "0", "off":
		return false
	}
	return true
}

func (r *VClusterReconciler) reconcileDeletionBackup(ctx context.Context, ops VClusterDeletionOps, vc *v1alpha1.VCluster, _ *deletionRun) (bool, time.Duration, error) {
	vc.Status.Deletion.Stage = stageBackupPending

	// Rien à sauvegarder s'il n'y a rien : un CR refusé par le budget, ou dont le
	// provisionnement n'a jamais abouti, reçoit quand même son finalizer — il est
	// posé sur le chemin vivant, avant le contrôle de budget. Sa suppression
	// déclenchait alors l'exigence de sauvegarde Velero d'un namespace inexistant,
	// et le seul déblocage était l'annotation « détruire sans filet ». Normaliser
	// ce geste-là est bien plus dangereux que le cas qu'il débloque.
	//
	// `known` compte autant que `exists` : sur une lecture ratée on garde le filet.
	// « Je n'arrive pas à regarder » n'est pas « il n'y a rien ».
	if exists, known := ops.HostNamespaceState(ctx, vc.Name, r.Cell); known && !exists {
		setVClusterCond(vc, v1alpha1.CondVClusterBackupCompleted, metav1.ConditionTrue, "NothingToBackUp",
			"aucun namespace vcluster-"+vc.Name+" sur la cell : ce vcluster n'a jamais été "+
				"matérialisé, il n'y a pas de données à sauvegarder")
		return true, 0, nil
	}

	if override := vc.Annotations[v1alpha1.AnnDeletionBackupOverride]; overrideDisarms(override) {
		// Qui a désarmé le filet. L'annotation est le seul garde-fou de données
		// qu'un `patch` suffit à lever, et sur un objet en Terminating il n'y a
		// plus de diff Git où le lire — sans cette ligne, la destruction sans
		// sauvegarde ne laisse aucune trace nommée.
		par := override
		if par == "true" {
			par = "anonyme (annotation posée à \"true\" plutôt qu'au nom du décideur)"
		}
		audit.LogActor(par, "vcluster-deletion-backup-override", vc.Name, r.Cell,
			"destruction autorisée sans sauvegarde Velero terminée")
		setVClusterCond(vc, v1alpha1.CondVClusterBackupCompleted, metav1.ConditionFalse, "BackupOverridden",
			"sauvegarde sautée sur décision de "+par+" : la destruction se fait sans filet")
		return true, 0, nil
	}

	launched := r.condHasReason(vc, v1alpha1.CondVClusterBackupCompleted, reasonBackupProgress)

	st, err := ops.InspectDeletionBackup(ctx, vc.Name, r.Cell, vc.DeletionTimestamp.Time)
	if err != nil {
		// Ne pas écraser « une sauvegarde est partie » par « je n'ai pas réussi à
		// lire » : le tour suivant en lancerait une deuxième.
		if !launched {
			setVClusterCond(vc, v1alpha1.CondVClusterBackupCompleted, metav1.ConditionUnknown, "BackupUnknown",
				"état des sauvegardes Velero illisible : "+err.Error())
		}
		return false, RequeueInterval, err
	}

	blocked := func(reason, msg string) (bool, time.Duration, error) {
		msg += " — pour détruire sans sauvegarde, poser l'annotation " + v1alpha1.AnnDeletionBackupOverride + `="true"`
		vc.Status.Deletion.Message = msg
		setVClusterCond(vc, v1alpha1.CondVClusterBackupCompleted, metav1.ConditionFalse, reason, msg)
		// Pas de requeue : réessayer une sauvegarde qui ne part pas ne la fera
		// pas partir. Le déblocage est un geste humain, et il réveille le watch.
		return false, 0, nil
	}

	switch {
	case st.Completed:
		setVClusterCond(vc, v1alpha1.CondVClusterBackupCompleted, metav1.ConditionTrue, "Completed",
			"sauvegarde "+st.Name+" terminée avant destruction")
		return true, 0, nil

	case st.Failed:
		return blocked("BackupFailed", "sauvegarde "+st.Name+" en phase "+st.Phase)

	case st.Found:
		anchor := st.StartedAt
		if anchor.IsZero() {
			anchor = vc.DeletionTimestamp.Time
		}
		if time.Since(anchor) > backupGiveUpAfter {
			return blocked("BackupTimedOut",
				"sauvegarde "+st.Name+" toujours en phase "+st.Phase+" après "+backupGiveUpAfter.String())
		}
		setVClusterCond(vc, v1alpha1.CondVClusterBackupCompleted, metav1.ConditionUnknown, reasonBackupProgress,
			"sauvegarde "+st.Name+" en phase "+st.Phase)
		return false, RequeueInterval, nil

	case launched:
		// Une sauvegarde est partie au tour précédent mais Velero ne la montre
		// pas encore. En relancer une deuxième serait le réflexe coûteux.
		if r.overdue(vc, v1alpha1.CondVClusterBackupCompleted, metav1.ConditionUnknown, backupGiveUpAfter) {
			return blocked("BackupTimedOut",
				"la sauvegarde lancée n'est jamais apparue dans Velero après "+backupGiveUpAfter.String())
		}
		return false, RequeueInterval, nil

	default:
		created, err := ops.TriggerVeleroBackup(ctx, SystemActor, vc.Name, r.Cell)
		if err != nil {
			return blocked("BackupTriggerFailed", "impossible de lancer la sauvegarde : "+err.Error())
		}
		setVClusterCond(vc, v1alpha1.CondVClusterBackupCompleted, metav1.ConditionUnknown, reasonBackupProgress,
			"sauvegarde "+created.BackupName+" lancée avant destruction")
		return false, RequeueInterval, nil
	}
}

// reconcileFinalTeardown retire la protection puis détruit (§4.4 étapes 3 et 4).
//
// Les deux tiennent dans la même passe parce qu'elles n'attendent rien. La
// protection tombe au dernier moment, précisément pour que tout ce qui précède
// ait pu échouer sans jamais laisser le namespace à découvert.
func (r *VClusterReconciler) reconcileFinalTeardown(ctx context.Context, ops VClusterDeletionOps, vc *v1alpha1.VCluster, run *deletionRun) (bool, time.Duration, error) {
	vc.Status.Deletion.Stage = stageProtectionRemoval

	p := ops.GetProtection(ctx, vc.Name, r.Cell)
	if !p.Available {
		// On ne SAIT PAS si ce namespace est protégé, et tout ce qui suit détruit :
		// les finalizers Flux, Keycloak, Vault, puis le namespace lui-même. Ne pas
		// lever une annotation qu'on n'a pas lue est le bon réflexe, mais s'arrêter
		// là et détruire quand même le vide de son sens — le garde-fou serait
		// contourné par la panne qu'il devrait faire échouer.
		//
		// C'est un durcissement de N6, et il n'était pas nécessaire avant : tant
		// que la destruction du namespace passait par le prune Flux, une protection
		// non lue restait posée et retenait Flux. Depuis que le finalizer supprime
		// lui-même, plus rien ne la lit sur ce chemin.
		//
		// On requeue plutôt que de bloquer sec : ce qui débloque ici est une panne
		// d'API qui se répare toute seule, contrairement à une sauvegarde en échec,
		// dont le déblocage est un geste humain que le watch rapporte.
		msg := "impossible de lire la protection du namespace : " + p.Detail +
			" — la séquence s'arrête avant de détruire quoi que ce soit"
		vc.Status.Deletion.Message = msg
		setVClusterCond(vc, v1alpha1.CondVClusterReady, metav1.ConditionFalse, "ProtectionUnknown", msg)
		return false, deletionStepRequeue, nil
	}
	if p.Protected {
		if _, err := ops.SetProtection(ctx, SystemActor, vc.Name, r.Cell, false); err != nil {
			setVClusterCond(vc, v1alpha1.CondVClusterReady, metav1.ConditionFalse, "ProtectionRemovalFailed",
				"protection du namespace pas retirée : "+err.Error())
			return false, deletionStepRequeue, err
		}
	}
	// Écrit seulement ici, une fois la lecture aboutie : sur la branche du dessus,
	// affirmer `false` serait dire « ce namespace n'est plus protégé » sans avoir
	// regardé — le défaut même qu'on vient de fermer, un maillon plus loin.
	vc.Status.ProtectionEnabled = false

	// Le stage passe à stageDestroying dans reconcileNamespaceRemoval, l'étape
	// suivante de la même passe : l'écrire aussi ici serait retenu une fraction de
	// seconde puis écrasé avant qu'aucun Status().Update() n'ait eu la moindre
	// chance de le publier — runDeletionSequence enchaîne les étapes sans écrire
	// entre les deux.
	opts := service.TeardownOptions{
		DeleteAppManifestsRepo: vc.Annotations[v1alpha1.AnnDeletionDeleteAppManifestsRepo] == "true",
	}
	warnings, err := ops.TeardownVCluster(ctx, SystemActor, vc.Name, r.Cell, opts)
	if err != nil {
		setVClusterCond(vc, v1alpha1.CondVClusterReady, metav1.ConditionFalse, "TeardownFailed", err.Error())
		return false, deletionStepRequeue, err
	}
	run.teardownWarnings = warnings
	return true, 0, nil
}

// reconcileNamespaceRemoval supprime le namespace du vcluster et attend de le
// voir disparaître (§4.4 étape 4, arbitrage N6 du 2026-08-07).
//
// C'est l'opérateur qui supprime, et pas Flux. L'étape `Destroying` se contentait
// avant de retirer les finalizers Flux du namespace « pour qu'il puisse être
// supprimé proprement », puis annonçait « séquence de suppression terminée » : la
// suppression elle-même était le prune d'un commit que le finalizer n'écrit pas
// et ne vérifie pas. Un CR pouvait donc disparaître en laissant le namespace, ses
// pods et son volume derrière lui, avec un status qui affirmait le contraire.
//
// Des deux issues possibles — supprimer soi-même, ou attendre de constater que
// Flux l'a fait — c'est la première : l'opérateur applique déjà ce namespace en
// Server-Side Apply, il en est propriétaire de fait, et la faire dépendre de Flux
// aurait été une attente sans borne naturelle sur un acteur qu'on ne pilote pas.
//
// L'observation conclut, pas l'appel. Un `delete` sur un namespace ne fait que
// poser un deletionTimestamp : rendre `true` juste après aurait reproduit le
// défaut qu'on corrige, à un maillon près.
func (r *VClusterReconciler) reconcileNamespaceRemoval(ctx context.Context, ops VClusterDeletionOps, vc *v1alpha1.VCluster, run *deletionRun) (bool, time.Duration, error) {
	vc.Status.Deletion.Stage = stageDestroying

	// Demander d'abord, constater ensuite, dans le même tour. L'ordre inverse
	// serait plus joli à lire mais coûterait un requeue de 30 s à toute
	// suppression, y compris au cas courant où le namespace part tout de suite —
	// il ne reste rien dedans à ce stade, le teardown vient de retirer les
	// finalizers Flux qui le retenaient.
	//
	// La demande est rejouée à chaque tour tant que la disparition n'est pas
	// constatée. Elle est idempotente, et la rejouer couvre le cas où le premier
	// appel est parti avec le process qui l'a émis : la reprise ne relit pas un
	// registre, elle redemande.
	if err := ops.DeleteHostNamespace(ctx, SystemActor, vc.Name, r.Cell); err != nil {
		// Un refus n'a PAS de borne, contrairement à un namespace qui traîne, et
		// c'est délibéré. Les deux situations n'ont pas la même issue sûre : un
		// namespace en Terminating est déjà condamné, donc lâcher le CR ne perd
		// rien ; un `forbidden` — le ClusterRole pas redéployé — veut dire que RIEN
		// n'a été détruit et que les données sont intactes. Lâcher le CR
		// transformerait alors une panne réparable, visible et nommée, en un
		// namespace orphelin que plus aucun objet ne réclame.
		//
		// Rapporté sur CondVClusterReady, PAS sur CondNamespaceRemoved — c'est le
		// correctif d'un bug trouvé en recette. Les deux conditions partageaient
		// avant la même ancre de délai (SetStatusCondition ne remet
		// LastTransitionTime à zéro que quand le STATUT change, pas la raison), donc
		// un refus qui dure plus de dix minutes — le scénario le plus probable du
		// chantier : le ClusterRole pas redéployé — faisait hériter à l'ATTENTE
		// ci-dessous une horloge déjà expirée. Dès que quelqu'un corrigeait le
		// ClusterRole, le tout premier tour où la suppression passait enfin lâchait
		// le CR sans avoir observé la disparition une seule fois, avec un message
		// qui accusait un finalizer tiers plutôt que la vraie cause. En écrivant
		// ailleurs, CondNamespaceRemoved n'est plus alimentée que par l'attente
		// réelle (voir namespaceRemovalOverdue), et sa propre ancre ne mesure plus
		// qu'une seule chose.
		//
		// Pas de délai affiché ici non plus : LastTransitionTime de CondVClusterReady
		// dit déjà « depuis quand » à qui lit l'objet, pas besoin de le répéter en
		// toutes lettres dans le message.
		msg := "suppression du namespace refusée : " + err.Error() +
			" — vérifier que le ClusterRole de l'opérateur porte bien `delete` sur les namespaces. " +
			"Rien n'a été détruit, le CR attend."
		setVClusterCond(vc, v1alpha1.CondVClusterReady, metav1.ConditionFalse, "NamespaceDeletionForbidden", msg)
		return false, deletionStepRequeue, err
	}

	exists, known := ops.HostNamespaceState(ctx, vc.Name, r.Cell)
	if known && !exists {
		setVClusterCond(vc, v1alpha1.CondNamespaceRemoved, metav1.ConditionTrue, "NamespaceGone",
			"le namespace vcluster-"+vc.Name+" a disparu du cluster")
		return r.deletionDone(ctx, vc, run, "")
	}

	// L'ancre du délai est la condition, dont la LastTransitionTime survit au
	// redémarrage. Statut False dans les deux branches ci-dessous, précisément
	// pour que l'ancre ne se remette pas à zéro quand la raison alterne entre
	// « encore là » et « je n'arrive pas à regarder » — mais namespaceRemovalOverdue
	// vérifie AUSSI la raison, en défense en profondeur : si une condition
	// CondNamespaceRemoved=False finit un jour posée pour une tout autre cause
	// (un downgrade, un `kubectl patch` de dépannage), elle ne doit pas prêter son
	// âge à une attente qui vient tout juste de commencer.
	if r.namespaceRemovalOverdue(vc) {
		// On lâche le CR malgré tout. Le laisser en Terminating pour toujours
		// n'efface pas le namespace et ajoute un objet coincé au problème ; ce qui
		// aide, c'est de nommer ce qui reste.
		//
		// Dire aussi dans quel ÉTAT on le laisse, et pas seulement qu'il est là :
		// à ce stade la protection a été levée et les finalizers Flux retirés, donc
		// ce namespace est à découvert. Qui lit ce message doit savoir qu'il ne
		// tient plus à rien, pas seulement qu'il reste à faire.
		const decouvert = " ; sa protection a été levée et ses finalizers Flux retirés — il ne tient plus à rien"
		leftover := "le namespace vcluster-" + vc.Name + " est toujours là après " +
			namespaceRemovalGiveUpAfter.String() + " : un finalizer tiers le retient, " +
			"ou la Kustomization Flux du tenant le réapplique — à finir à la main" + decouvert
		if !known {
			leftover = "impossible de savoir si le namespace vcluster-" + vc.Name + " a disparu après " +
				namespaceRemovalGiveUpAfter.String() + " : suppression demandée, résultat non constaté — " +
				"à vérifier à la main" + decouvert
		}
		setVClusterCond(vc, v1alpha1.CondNamespaceRemoved, metav1.ConditionUnknown, "RemovalUnconfirmed", leftover)
		return r.deletionDone(ctx, vc, run, leftover)
	}

	reason, msg := "NamespaceTerminating", "suppression du namespace vcluster-"+vc.Name+" demandée, il est encore là"
	if !known {
		reason, msg = "NamespaceStateUnknown", "suppression du namespace vcluster-"+vc.Name+
			" demandée ; son état n'a pas pu être lu, on ne conclut pas"
	}
	setVClusterCond(vc, v1alpha1.CondNamespaceRemoved, metav1.ConditionFalse, reason, msg)
	return false, deletionStepRequeue, nil
}

// deletionDone écrit la conclusion de la séquence, restes compris.
//
// Elle est appelée par la dernière étape et par elle seule : tant qu'on écrivait
// « séquence de suppression terminée » avant la disparition du namespace, la
// phrase était fausse pour le seul lecteur qui compte — celui qui vient voir
// pourquoi un vcluster supprimé occupe encore de la place.
func (r *VClusterReconciler) deletionDone(ctx context.Context, vc *v1alpha1.VCluster, run *deletionRun, leftover string) (bool, time.Duration, error) {
	rests := run.teardownWarnings
	if leftover != "" {
		rests = append(append([]string(nil), rests...), leftover)
	}
	msg := "séquence de suppression terminée"
	if len(rests) > 0 {
		msg += " — restes à reprendre à la main : " + strings.Join(rests, " ; ")
	}
	vc.Status.Deletion.Message = msg
	setVClusterCond(vc, v1alpha1.CondVClusterReady, metav1.ConditionFalse, "Deleted", msg)

	// Et dans le log, parce que le status ne survit pas à la phrase qu'il porte :
	// deux appels plus loin le finalizer est retiré et l'objet disparaît. Écrire
	// « voilà ce qui reste à reprendre à la main » uniquement dans le status d'un
	// objet en train d'être effacé, c'est ne l'écrire que pour un `watch` déjà
	// ouvert. Ce log est la seule trace durable de ce que la séquence n'a pas fait.
	if len(rests) > 0 {
		log.FromContext(ctx).Info("suppression terminée avec des restes",
			"vcluster", vc.Name, "cell", r.Cell, "restes", rests)
	}
	return true, 0, nil
}

// condHasReason dit si une condition est actuellement posée pour cette raison.
func (r *VClusterReconciler) condHasReason(vc *v1alpha1.VCluster, condType, reason string) bool {
	c := apimeta.FindStatusCondition(vc.Status.Conditions, condType)
	return c != nil && c.Reason == reason
}

// overdue borne une étape sur la LastTransitionTime de sa condition.
//
// C'est un horodatage que Kubernetes tient déjà, qui vit dans le status et donc
// survit à un redémarrage — plutôt qu'un champ « démarré à » de plus, écrit par
// un process dont on ne sait pas s'il ira jusqu'au bout. L'ancre est le
// *statut* et pas la raison, parce que SetStatusCondition ne remet
// LastTransitionTime à zéro que quand le statut change : la borne tient même si
// la raison alterne d'un tour à l'autre.
func (r *VClusterReconciler) overdue(vc *v1alpha1.VCluster, condType string, status metav1.ConditionStatus, after time.Duration) bool {
	c := apimeta.FindStatusCondition(vc.Status.Conditions, condType)
	if c == nil || c.Status != status || c.LastTransitionTime.IsZero() {
		return false
	}
	return time.Since(c.LastTransitionTime.Time) > after
}

// namespaceRemovalOverdue est overdue(), mais pour CondNamespaceRemoved
// seulement, et avec un contrôle de plus : la raison.
//
// overdue() partage volontairement son ancre entre deux raisons pour l'étape
// Rancher — la panne de lecture et l'attente du job de nettoyage sont la même
// horloge. Ici, une seule raison a le droit d'alimenter le délai : les deux
// qu'écrit l'attente elle-même (NamespaceTerminating, NamespaceStateUnknown).
// Le refus de suppression (NamespaceDeletionForbidden) n'écrit plus cette
// condition du tout depuis le correctif ci-dessus, mais garder ce filtre est
// une défense en profondeur bon marché : si une raison étrangère venait un jour
// s'y poser — un downgrade, un `kubectl patch` — elle ne doit pas prêter son âge
// à une attente qui commence tout juste, et transformer une suppression qui
// vient enfin de réussir en un CR lâché sans avoir rien observé.
func (r *VClusterReconciler) namespaceRemovalOverdue(vc *v1alpha1.VCluster) bool {
	c := apimeta.FindStatusCondition(vc.Status.Conditions, v1alpha1.CondNamespaceRemoved)
	if c == nil || c.Status != metav1.ConditionFalse || c.LastTransitionTime.IsZero() {
		return false
	}
	switch c.Reason {
	case "NamespaceTerminating", "NamespaceStateUnknown":
	default:
		return false
	}
	return time.Since(c.LastTransitionTime.Time) > namespaceRemovalGiveUpAfter
}
