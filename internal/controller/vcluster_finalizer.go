package controller

import (
	"context"
	"errors"
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
// Elle est déclarée à part de VClusterOps parce que trois chantiers se
// partagent vcluster_controller.go ; les deux interfaces ont vocation à
// fusionner quand ils se rejoignent, et le champ `Ops` du reconciler portera
// alors le tout. En attendant, l'assertion à la compilation ci-dessous garantit
// que le vrai service satisfait l'ensemble, donc la conversion de type en
// production ne peut pas échouer.
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

	// TeardownVCluster détruit : finalizers Flux du namespace, puis Keycloak,
	// Vault et éventuellement le dépôt app-manifests.
	TeardownVCluster(ctx context.Context, actor models.Actor, name, env string, opts service.TeardownOptions) ([]string, error)
}

var _ VClusterDeletionOps = (*service.Service)(nil)

// errNoDeletionOps ne peut arriver qu'avec un faux qui n'implémente que la
// moitié du seam ; en production l'assertion ci-dessus l'exclut.
var errNoDeletionOps = errors.New("l'implémentation de VClusterOps ne porte pas la séquence de suppression")

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
	ops, ok := r.Ops.(VClusterDeletionOps)
	if !ok {
		return ctrl.Result{}, errNoDeletionOps
	}

	if vc.Status.Deletion == nil {
		vc.Status.Deletion = &v1alpha1.DeletionStatus{}
	}
	vc.Status.Phase = v1alpha1.VClusterPhaseDeleting

	done, requeue, seqErr := r.runDeletionSequence(ctx, ops, vc)

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

// runDeletionSequence enchaîne les étapes de §4.4 dans l'ordre. Chacune commence
// par constater si elle a déjà été faite, donc rejouer la séquence ne redétruit
// rien et ne se bloque pas.
func (r *VClusterReconciler) runDeletionSequence(ctx context.Context, ops VClusterDeletionOps, vc *v1alpha1.VCluster) (bool, time.Duration, error) {
	steps := []func(context.Context, VClusterDeletionOps, *v1alpha1.VCluster) (bool, time.Duration, error){
		r.checkDeletionProtection,
		r.reconcileRancherTeardown,
		r.reconcileDeletionBackup,
		r.reconcileFinalTeardown,
	}
	for _, step := range steps {
		done, requeue, err := step(ctx, ops, vc)
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
func (r *VClusterReconciler) checkDeletionProtection(_ context.Context, _ VClusterDeletionOps, vc *v1alpha1.VCluster) (bool, time.Duration, error) {
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
func (r *VClusterReconciler) reconcileRancherTeardown(ctx context.Context, ops VClusterDeletionOps, vc *v1alpha1.VCluster) (bool, time.Duration, error) {
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

func (r *VClusterReconciler) reconcileDeletionBackup(ctx context.Context, ops VClusterDeletionOps, vc *v1alpha1.VCluster) (bool, time.Duration, error) {
	vc.Status.Deletion.Stage = stageBackupPending

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
func (r *VClusterReconciler) reconcileFinalTeardown(ctx context.Context, ops VClusterDeletionOps, vc *v1alpha1.VCluster) (bool, time.Duration, error) {
	vc.Status.Deletion.Stage = stageProtectionRemoval
	if p := ops.GetProtection(ctx, vc.Name, r.Cell); p.Available && p.Protected {
		if _, err := ops.SetProtection(ctx, SystemActor, vc.Name, r.Cell, false); err != nil {
			setVClusterCond(vc, v1alpha1.CondVClusterReady, metav1.ConditionFalse, "ProtectionRemovalFailed",
				"protection du namespace pas retirée : "+err.Error())
			return false, deletionStepRequeue, err
		}
	}
	vc.Status.ProtectionEnabled = false

	vc.Status.Deletion.Stage = stageDestroying
	opts := service.TeardownOptions{
		DeleteAppManifestsRepo: vc.Annotations[v1alpha1.AnnDeletionDeleteAppManifestsRepo] == "true",
	}
	warnings, err := ops.TeardownVCluster(ctx, SystemActor, vc.Name, r.Cell, opts)
	if err != nil {
		setVClusterCond(vc, v1alpha1.CondVClusterReady, metav1.ConditionFalse, "TeardownFailed", err.Error())
		return false, deletionStepRequeue, err
	}

	msg := "séquence de suppression terminée"
	if len(warnings) > 0 {
		msg += " — restes à reprendre à la main : " + strings.Join(warnings, " ; ")
	}
	vc.Status.Deletion.Message = msg
	setVClusterCond(vc, v1alpha1.CondVClusterReady, metav1.ConditionFalse, "Deleted", msg)
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
