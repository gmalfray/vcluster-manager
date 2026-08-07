package v1alpha1

// Annotations de la séquence de suppression.
//
// Pourquoi des annotations et pas des champs de spec : elles sont posées sur un
// objet qui porte déjà un `deletionTimestamp`. Le fichier du CR n'existe plus
// dans fluxprod — c'est sa disparition qui a déclenché la suppression — donc
// Flux n'a plus rien à appliquer, et un champ de spec n'aurait aucun chemin
// pour arriver jusqu'ici. Un `kubectl annotate` en a un.
//
// Elles restent malgré tout lisibles dans le diff Git tant que le CR existe :
// `metadata.annotations` fait partie du fichier commité, donc le geste peut
// aussi être pris d'avance, en revue, plutôt qu'en urgence sur un objet coincé
// en Terminating.
const (
	// AnnDeletionBackupOverride, à "true", laisse la suppression continuer sans
	// sauvegarde Velero terminée. ADR-001 (« se prémunir de la suppression par
	// la réversibilité », point 2) exige que ce soit un geste explicite : sans
	// lui, une sauvegarde qui échoue arrête la séquence, elle ne la laisse pas
	// passer avec un avertissement.
	AnnDeletionBackupOverride = "deletion.vcluster.rebuild-it.fr/backup-override"

	// AnnDeletionDeleteAppManifestsRepo, à "true", supprime aussi le dépôt
	// app-manifests du vcluster. C'était une case à cocher du formulaire de
	// suppression ; par défaut le dépôt survit.
	AnnDeletionDeleteAppManifestsRepo = "deletion.vcluster.rebuild-it.fr/delete-app-manifests-repo"
)
