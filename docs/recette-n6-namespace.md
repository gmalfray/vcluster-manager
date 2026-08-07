# Recette preprod — N6 : le finalizer supprime le namespace qu'il a créé

> Périmètre : l'arbitrage **N6** (`docs/etat-brique-operateur.md`, « Arbitrages rendus le
> 2026-08-07 »). L'étape `Destroying` du finalizer ne supprimait rien ; elle demande
> désormais la suppression du namespace hôte, **constate** sa disparition, et ne lâche le
> CR qu'après — ou renonce au bout de dix minutes en nommant ce qui reste.
>
> Ce plan est le premier passage sur cluster réel de la brique opérateur. Rien de ce qui
> suit n'a jamais tourné ailleurs qu'en envtest.

## Ce que la recette doit trancher

Cinq questions, une par cas. Chacune a une réponse observable, et l'endroit où la lire.

| Cas | Question | Verdict lu dans |
|---|---|---|
| A | Le namespace part-il vraiment, et le CR le suit-il ? | `NamespaceRemoved=True/NamespaceGone`, puis absence des deux objets |
| B | Un namespace qu'un finalizer tiers retient bloque-t-il, puis débloque-t-il ? | `NamespaceRemoved` False → Unknown/`RemovalUnconfirmed` à 10 min |
| C | Que se passe-t-il quand Flux réapplique le namespace qu'on supprime ? | `NamespaceRemoved`, et l'état du namespace après le lâcher du CR |
| D | Un opérateur sans le verbe `delete` se tait-il, ou le dit-il ? | `NamespaceRemoved=False/DeleteFailed` + logs |
| E | Un redémarrage en plein milieu perd-il la borne des dix minutes ? | `lastTransitionTime` de `NamespaceRemoved` avant / après |

## Règles de sécurité de ce plan

- **Preprod uniquement.** Aucun geste de ce document ne se fait en prod, pas même en
  lecture adaptée : le cas D dégrade temporairement un ClusterVersion partagé.
- Chaque cas nomme sa cible. Les vclusters jetables sont `recette-n6-a` à `recette-n6-e`,
  créés et détruits par la recette. **Ne pas réutiliser un vcluster existant** : ce qui
  est testé ici supprime un namespace pour de bon.
- Le cas D casse le RBAC de l'opérateur pour **toute la cell** le temps de la
  vérification. Prévenir, et garder la fenêtre courte — la commande de remise en état est
  donnée avec l'étape qui la crée.
- Aucun kubeconfig, token ou contenu de sauvegarde ne va dans le compte rendu.

## Préconditions

1. Cell preprod joignable, `kubectl` en admin sur la cell, `flux` et `velero` installés
   en local.
2. Image de l'opérateur portant N6 déployée, et **le ClusterRole redéployé avec elle** —
   c'est la précondition qu'on oublie, et le cas D est là parce qu'elle s'oublie :

   ```bash
   kubectl auth can-i delete namespaces \
     --as=system:serviceaccount:vcluster-manager:vcluster-manager-operator
   # attendu : yes
   ```

   Si la réponse est `no`, appliquer `deploy/base/operator-rbac.yaml` avant d'aller plus
   loin, sinon tous les cas sauf D mesureront le cas D.
3. CRD `vclusters.vcluster.rebuild-it.fr` à jour (le champ `status.conditions` accepte le
   type `NamespaceRemoved` sans schéma particulier, mais la règle CEL sur le nom réservé
   doit être là) :

   ```bash
   kubectl get crd vclusters.vcluster.rebuild-it.fr -o jsonpath='{.spec.versions[0].name}'
   ```
4. Velero fonctionnel sur la cell (cas A). Les cas B à E désarment le filet par annotation
   pour ne pas attendre une sauvegarde à chaque itération — c'est un choix de recette,
   assumé, et le cas A est là pour que le chemin complet soit passé **une fois** pour de
   vrai.
5. Un terminal libre pour un `watch` : le message final du finalizer s'écrit dans le
   status d'un objet qui disparaît dans la même seconde (voir « Limite connue » plus bas).

### Variables utilisées par les commandes

```bash
export CELL=preprod                 # --cell de l'opérateur
export CRNS=vcluster-manager        # --vclusters-namespace : là où vivent les CR
export OPDEPLOY=deploy/vcluster-manager-operator
```

### Gabarit du CR jetable

Le générateur ne commite pas encore de `VCluster` dans fluxprod : le CR se pose à la main
pour cette recette. C'est un écart avec la cible (ADR-001), pas avec le code testé.

```bash
cat <<EOF | kubectl apply -f -
apiVersion: vcluster.rebuild-it.fr/v1alpha1
kind: VCluster
metadata:
  name: recette-n6-a
  namespace: ${CRNS}
spec:
  owner: recette
EOF
```

### Les trois commandes de lecture, valables partout

```bash
# 1. où en est la séquence, et pourquoi
kubectl -n $CRNS get vcluster recette-n6-X -o jsonpath='
stage : {.status.deletion.stage}
msg   : {.status.deletion.message}
{range .status.conditions[?(@.type=="NamespaceRemoved")]}cond  : {.status}/{.reason} depuis {.lastTransitionTime}
        {.message}{end}{"\n"}'

# 2. l'état réel du namespace
kubectl get ns vcluster-recette-n6-X -o jsonpath='
phase       : {.status.phase}
deletionTS  : {.metadata.deletionTimestamp}
finalizers  : {.metadata.finalizers}{"\n"}'

# 3. ce que l'opérateur a fait, et pour quel objet
kubectl -n $CRNS logs $OPDEPLOY --since=15m | grep -E 'audit|namespace|finalizer'
```

---

## Cas A — le chemin nominal, filet compris

**Cible** : `recette-n6-a`. **Préconditions** : Velero opérationnel, aucun vcluster de ce
nom.

1. Poser le CR (gabarit ci-dessus, `name: recette-n6-a`).
2. Attendre que l'opérateur provisionne :

   ```bash
   kubectl -n $CRNS get vcluster recette-n6-a -o jsonpath='{.status.phase}{"\n"}'
   kubectl get ns vcluster-recette-n6-a
   ```

   **Attendu** : le namespace `vcluster-recette-n6-a` existe, et le ConfigMap
   `vcluster-recette-n6-a-substitutions` est dedans. Si `BudgetOK=False`, le plafond de la
   cell est trop bas pour un vcluster de plus — le régler avant de continuer, sinon rien
   n'est provisionné et la suite ne teste rien.
3. Ouvrir le `watch` dans le second terminal (il sert au point 7) :

   ```bash
   kubectl -n $CRNS get vcluster recette-n6-a -o yaml -w > /tmp/recette-n6-a.watch
   ```
4. Supprimer le CR :

   ```bash
   kubectl -n $CRNS delete vcluster recette-n6-a --wait=false
   ```

   `--wait=false` est important : la commande rendrait la main seulement au retrait du
   finalizer, et on veut regarder ce qui se passe entre-temps.
5. Suivre la séquence. **Attendu**, dans l'ordre : `stage: BackupPending` avec
   `BackupCompleted=Unknown/InProgress`, puis la sauvegarde qui termine
   (`velero backup get | grep recette-n6-a`), puis `stage: Destroying`.
6. **Le point de la recette.** Une fois la sauvegarde terminée :

   **Attendu** : `NamespaceRemoved=True/NamespaceGone`, le namespace a disparu
   (`kubectl get ns vcluster-recette-n6-a` → NotFound), le CR a disparu, et les logs
   portent une ligne `"action":"vcluster-namespace-delete"` avec
   `"vcluster":"recette-n6-a"` et `"env":"preprod"`.

   **Ce qui invalide le cas** : le CR disparaît alors que `kubectl get ns
   vcluster-recette-n6-a` répond encore. C'est exactement le défaut que N6 corrige, et
   c'est un No-Go.
7. Relire `/tmp/recette-n6-a.watch` : la dernière version du CR doit porter
   `message: séquence de suppression terminée`. Noter si elle y est ou non — voir
   « Limite connue ».

**Retour arrière** : aucun. Le vcluster est jetable et sa suppression est le test. Si la
séquence se bloque, la sortie est au cas B (retrait du finalizer parasite) ou à la section
« Débloquer un CR coincé ».

---

## Cas B — un finalizer tiers retient le namespace

C'est le cas que la borne de dix minutes existe pour couvrir. On le fabrique, parce qu'en
vrai il vient d'un opérateur tiers (Kyverno, un CSI, un webhook) qu'on ne va pas installer
pour l'occasion.

**Cible** : `recette-n6-b`.

1. Poser le CR `recette-n6-b`, attendre que le namespace existe.
2. Poser un finalizer parasite sur le namespace :

   ```bash
   kubectl patch ns vcluster-recette-n6-b --type=merge \
     -p '{"metadata":{"finalizers":["recette.local/bloque"]}}'
   kubectl get ns vcluster-recette-n6-b -o jsonpath='{.metadata.finalizers}{"\n"}'
   ```
3. Désarmer le filet de sauvegarde, en le signant :

   ```bash
   kubectl -n $CRNS annotate vcluster recette-n6-b \
     deletion.vcluster.rebuild-it.fr/backup-override="$(whoami) — recette N6"
   ```

   La valeur n'est pas décorative : elle atterrit dans la ligne d'audit
   `vcluster-deletion-backup-override`. Une valeur qui veut dire non (`false`, `no`, `0`)
   ne désarme rien, par construction.
4. Noter l'heure, puis supprimer le CR :

   ```bash
   date -u +%H:%M:%S; kubectl -n $CRNS delete vcluster recette-n6-b --wait=false
   ```
5. Dans la minute. **Attendu** :
   - namespace en `phase: Terminating` avec un `deletionTimestamp` posé ;
   - `NamespaceRemoved=False/NamespaceTerminating`, message
     « suppression du namespace vcluster-recette-n6-b demandée, il est encore là » ;
   - `stage: Destroying` ;
   - le CR **est toujours là**.

   **Ce qui invalide le cas** : le CR est parti. Rien ne prouvait la disparition.
6. Vérifier le rythme de réessai : la condition doit garder le **même**
   `lastTransitionTime` d'un tour à l'autre (c'est l'ancre du délai), et les logs doivent
   montrer un passage toutes les 30 s. Compter aussi les lignes d'audit :

   ```bash
   kubectl -n $CRNS logs $OPDEPLOY --since=5m \
     | grep -c '"action":"vcluster-namespace-delete"'
   ```

   **Attendu : exactement 1**, quel que soit le nombre de tours. La demande est bien
   rejouée toutes les 30 s — c'est la reprise par observation, elle ne relit pas un
   registre — mais elle commence par lire le namespace : un `deletionTimestamp` déjà
   posé vaut « rien de neuf », donc pas de ligne. Sans ça, un namespace retenu dix
   minutes produisait une vingtaine de lignes « namespace supprimé » identiques, et
   noyait la seule qui compte.

   **Ce qui invalide le cas** : plus d'une ligne. Le filtre ne fait pas son travail.
7. Attendre **dix minutes** après l'heure notée au point 4. **Attendu** :
   - `NamespaceRemoved=Unknown/RemovalUnconfirmed` ;
   - le message nomme le namespace resté debout ET l'état dans lequel il est laissé :
     « le namespace vcluster-recette-n6-b est toujours là après 10m0s : un finalizer tiers
     le retient, ou la Kustomization Flux du tenant le réapplique — à finir à la main ;
     sa protection a été levée et ses finalizers Flux retirés — il ne tient plus à rien » ;
   - le CR est lâché juste après.

**Retour arrière** — obligatoire, sinon le namespace reste en Terminating pour toujours :

```bash
kubectl patch ns vcluster-recette-n6-b --type=merge -p '{"metadata":{"finalizers":[]}}'
kubectl get ns vcluster-recette-n6-b   # attendu : NotFound
```

---

## Cas C — la course avec Flux

Le cas que le commentaire de `ProvisionFieldManager` décrit depuis qu'il a été corrigé : le
namespace a **deux** écrivains. Il vient aussi de `clusters/<cell>/base`, dont l'overlay du
tenant patche le `metadata.name`. Supprimer le namespace alors que
`clusters/preprod/vclusters/recette-n6-c/` est encore commité, c'est le supprimer pendant
que Flux le réapplique.

**Cible** : `recette-n6-c`. **Précondition** : l'arborescence tenant de `recette-n6-c` est
**présente** dans fluxprod (créer le vcluster par l'app, ou commiter l'arbre à la main),
et la Kustomization `tenant-recette-n6-c` est active.

1. Vérifier que Flux applique bien cet arbre :

   ```bash
   flux get kustomizations -n flux-system | grep recette-n6-c
   kubectl get ns vcluster-recette-n6-c -o jsonpath='{.metadata.managedFields[*].manager}{"\n"}'
   ```

   **Attendu** : deux managers sur le namespace — celui de l'opérateur et
   `kustomize-controller`. C'est la preuve de la double propriété, et elle se lit avant de
   rien casser.
2. Désarmer le filet (comme au cas B), noter l'heure, supprimer le CR :

   ```bash
   kubectl -n $CRNS annotate vcluster recette-n6-c \
     deletion.vcluster.rebuild-it.fr/backup-override="$(whoami) — recette N6"
   date -u +%H:%M:%S; kubectl -n $CRNS delete vcluster recette-n6-c --wait=false
   ```
3. Boucler toutes les dix secondes pendant deux minutes sur les deux lectures (namespace et
   condition), et **écrire ce qu'on voit** : ce cas a deux issues plausibles, et laquelle
   se produit dépend du temps que met Flux à réappliquer.

   ```bash
   for i in $(seq 12); do
     date -u +%H:%M:%S
     kubectl get ns vcluster-recette-n6-c -o jsonpath='{.status.phase} ts={.metadata.deletionTimestamp}{"\n"}' 2>&1
     kubectl -n $CRNS get vcluster recette-n6-c \
       -o jsonpath='{range .status.conditions[?(@.type=="NamespaceRemoved")]}{.status}/{.reason}{"\n"}{end}' 2>&1
     sleep 10
   done
   ```

   **Issue attendue 1 — le namespace ne part jamais** : Flux le recrée aussi vite qu'il est
   supprimé, `NamespaceRemoved` reste `False/NamespaceTerminating`, et à dix minutes la
   borne rend `Unknown/RemovalUnconfirmed` avec le message qui cite la Kustomization du
   tenant. C'est le comportement conçu.

   **Issue attendue 2 — le namespace disparaît une fraction de seconde, puis revient** :
   l'opérateur observe « disparu » pendant cette fenêtre, conclut `True/NamespaceGone` et
   lâche le CR ; Flux recrée ensuite un namespace vide, que plus rien ne pilote. Le
   résultat est un namespace orphelin qu'aucun CR ne mentionne.

   **L'issue 2 n'est pas un échec de N6** — la séquence a bien constaté ce qu'elle
   annonçait — mais c'est un défaut d'exploitation à remonter, avec l'heure exacte et la
   sortie de la boucle. La contre-mesure est en amont : retirer l'arbre du tenant de
   fluxprod (ou suspendre `tenant-recette-n6-c`) **avant** de supprimer le CR, ce qui est
   l'ordre que suit le parcours de suppression de l'app.
4. Rejouer une fois dans le bon ordre, pour vérifier que ce chemin-là est propre :

   ```bash
   flux suspend kustomization tenant-recette-n6-c -n flux-system
   # puis reposer un CR recette-n6-c et le supprimer comme au cas B
   ```

   **Attendu** : `NamespaceRemoved=True/NamespaceGone` en un ou deux tours.

**Retour arrière** :

```bash
flux resume kustomization tenant-recette-n6-c -n flux-system   # si suspendue
# retirer l'arbre clusters/preprod/vclusters/recette-n6-c/ de fluxprod (MR), puis :
kubectl delete ns vcluster-recette-n6-c --ignore-not-found
```

---

## Cas D — le ClusterRole n'a pas été redéployé

Le déploiement de l'opérateur et son ClusterRole partent dans deux objets distincts. Livrer
l'image de N6 sans le RBAC est le scénario de rate le plus probable de tout ce chantier.

**Cible** : `recette-n6-d`. **Impact** : la fenêtre dégrade l'opérateur pour **toute la
cell**. Prévenir avant, et ne pas dépasser quelques minutes.

1. Poser le CR `recette-n6-d`, attendre le namespace, désarmer le filet (cas B, point 3).
2. Sauvegarder le ClusterRole, puis lui retirer `delete` :

   ```bash
   kubectl get clusterrole vcluster-manager-operator -o yaml > /tmp/n6-clusterrole.bak.yaml
   kubectl get clusterrole vcluster-manager-operator -o json \
     | jq '(.rules[] | select(.resources[]? == "namespaces") | .verbs) |= map(select(. != "delete"))' \
     | kubectl apply -f -
   kubectl auth can-i delete namespaces \
     --as=system:serviceaccount:vcluster-manager:vcluster-manager-operator
   # attendu : no
   ```
3. Supprimer le CR `recette-n6-d`.
4. **Attendu** :
   - `NamespaceRemoved=False/DeleteFailed`, message
     « suppression du namespace refusée : … is forbidden … cannot delete resource
     "namespaces" » ;
   - le CR **reste** ;
   - le namespace n'a **pas** de `deletionTimestamp` ;
   - les logs de l'opérateur portent l'erreur de réconciliation, et **aucune** ligne
     d'audit `vcluster-namespace-delete` — on ne journalise pas une suppression refusée ;
   - la réconciliation est réessayée (backoff de controller-runtime sur erreur).

   **Ce qui invalide le cas** : le CR part quand même, ou la condition passe à `True`. Un
   refus lu comme une disparition serait le pire des deux mondes.
5. Remettre le ClusterRole, immédiatement :

   ```bash
   kubectl apply -f /tmp/n6-clusterrole.bak.yaml
   kubectl auth can-i delete namespaces \
     --as=system:serviceaccount:vcluster-manager:vcluster-manager-operator   # attendu : yes
   ```
6. **Attendu après remise en état** : sans aucun geste sur le CR, la séquence repart au
   tour suivant, `NamespaceRemoved` passe à `True/NamespaceGone`, le CR est lâché. C'est
   la moitié qui compte : un refus n'est pas un état absorbant.

**Retour arrière** : le point 5, et il n'est pas optionnel. Vérifier avec
`kubectl diff -f /tmp/n6-clusterrole.bak.yaml` qu'il ne reste aucun écart.

---

## Cas E — redémarrage de l'opérateur en plein milieu

La doctrine dit « reprise par observation, jamais par registre écrit ». La borne des dix
minutes s'ancre sur le `lastTransitionTime` d'une condition, donc dans le status, donc elle
doit survivre au redémarrage. C'est ce qu'on vérifie, et rien d'autre.

**Cible** : `recette-n6-e`.

1. Reproduire le cas B jusqu'au point 5 inclus : namespace retenu par
   `recette.local/bloque`, `NamespaceRemoved=False/NamespaceTerminating`, CR retenu.
2. Relever l'ancre et le nombre de tours déjà passés :

   ```bash
   kubectl -n $CRNS get vcluster recette-n6-e -o jsonpath='
   {range .status.conditions[?(@.type=="NamespaceRemoved")]}{.lastTransitionTime}{"\n"}{end}'
   ```
3. Tuer l'opérateur au milieu de l'attente (vers la cinquième minute) :

   ```bash
   kubectl -n $CRNS delete pod -l app=vcluster-manager-operator
   kubectl -n $CRNS rollout status $OPDEPLOY
   ```
4. **Attendu** :
   - la même valeur de `lastTransitionTime` qu'au point 2 — **elle ne doit pas repartir de
     zéro**, sinon la borne se rallonge à chaque redémarrage et un opérateur qui redémarre
     souvent n'abandonne jamais ;
   - le nouveau pod **redemande** la suppression sans avoir rien relu d'un registre : une
     nouvelle ligne d'audit `vcluster-namespace-delete` apparaît dans les logs du nouveau
     pod ;
   - la condition ne repasse pas par `Unknown` en cours de route (elle reste `False`, seule
     la raison peut alterner).
5. Attendre l'échéance des dix minutes comptée **depuis le point 1**, pas depuis le
   redémarrage. **Attendu** : `Unknown/RemovalUnconfirmed` et lâcher du CR à l'heure
   initialement prévue.
6. Variante à faire une fois, si le temps le permet : tuer le pod **entre** la demande de
   suppression et l'observation (fenêtre de quelques millisecondes, donc à obtenir par
   répétition ou à déclarer non vérifiée). L'effet attendu est nul : la demande est
   idempotente et rejouée à chaque tour.

**Retour arrière** : identique au cas B (retrait du finalizer parasite).

---

## Débloquer un CR coincé

Si un CR reste en `Terminating` hors des cas ci-dessus, le geste de sortie — à n'utiliser
qu'en dernier recours, et à consigner :

```bash
kubectl -n $CRNS patch vcluster <nom> --type=merge -p '{"metadata":{"finalizers":[]}}'
```

Il saute **toute** la séquence : pas de dépairage Rancher, pas de sauvegarde, pas de
suppression de namespace. Ce qui reste debout est à finir à la main.

## Remise en état après la recette

```bash
for n in a b c d e; do
  kubectl -n $CRNS delete vcluster recette-n6-$n --ignore-not-found --wait=false
  kubectl patch ns vcluster-recette-n6-$n --type=merge -p '{"metadata":{"finalizers":[]}}' 2>/dev/null
  kubectl delete ns vcluster-recette-n6-$n --ignore-not-found
done
kubectl diff -f /tmp/n6-clusterrole.bak.yaml            # doit être vide
velero backup get | grep recette-n6                     # supprimer les sauvegardes de recette
flux get kustomizations -n flux-system | grep recette-n6  # aucune ne doit rester suspendue
```

## Limite connue, à confirmer pendant la recette

**Le message qui nomme les restes s'écrit dans un status qui disparaît aussitôt.** À
l'abandon (cas B, C, E), `deletionDone` écrit dans `status.deletion.message` la liste de ce
qui reste — namespace debout, avertissements du teardown — puis le finalizer est retiré
dans la même réconciliation et l'objet s'en va. Le seul lecteur possible est un `watch`
ouvert avant. Il n'y a ni Event Kubernetes (l'opérateur n'a pas d'`EventRecorder`, trou
déjà consigné) ni ligne de log portant ce message : la trace de « ce qui reste à finir à la
main » n'est donc pas récupérable après coup.

À vérifier au cas B point 7, et à remonter avec la sortie du `watch` comme preuve. La
correction n'appartient pas à cette recette.

## Ce que la recette doit remonter, même si tout passe

1. **Cas C, issue 2** : si le CR est lâché parce que le namespace a disparu une fraction de
   seconde avant d'être recréé par Flux, le noter avec l'horodatage. C'est un namespace
   orphelin, pas une perte de données, mais ça pousse à retirer l'arbre du tenant avant le
   CR — donc à documenter l'ordre des gestes.
2. **Toute condition posée avec un `reason` hors de cette liste** :
   `NamespaceGone`, `NamespaceTerminating`, `NamespaceStateUnknown`, `RemovalUnconfirmed`,
   `DeleteFailed` — et, sur `Ready`, `ProtectionUnknown`.
3. **La protection illisible arrête tout, et c'est nouveau.** Si `GetProtection` ne répond
   pas — RBAC sans `get` sur les namespaces, apiserver qui hoquette — la séquence s'arrête
   AVANT la levée de l'annotation, avant le teardown et avant la suppression du namespace :
   `Ready=False/ProtectionUnknown`, message portant la cause exacte, requeue de 30 s, et
   rien de détruit. Si la recette voit cette condition, ce n'est pas un échec de N6 : c'est
   le garde-fou qui fonctionne, et ce qu'il faut remonter est la cause de la panne de
   lecture.

## Grille de verdict

Go recette suivante si, et seulement si :

- cas A : namespace supprimé et CR lâché **dans cet ordre**, ligne d'audit présente ;
- cas B : blocage constaté, borne à dix minutes respectée, message nommant le namespace ;
- cas D : refus RBAC visible dans la condition, CR conservé, reprise automatique après
  remise du ClusterRole.

No-Go si l'un de ceux-ci se produit :

- un CR lâché alors que `kubectl get ns vcluster-<nom>` répond encore (le défaut d'origine
  de N6 est intact) ;
- `NamespaceRemoved=True` sans que le namespace ait disparu ;
- un refus de l'API server absorbé en silence (cas D avec CR lâché) ;
- la borne des dix minutes qui repart de zéro à chaque redémarrage (cas E) : un opérateur
  qui redémarre régulièrement ne renoncerait jamais, et le CR resterait coincé sans fin.

## Ce que ce plan ne couvre pas

- **La suppression via le parcours de l'app** (MR dans fluxprod, revue, fusion). Ce plan
  agit directement sur le CR ; le chemin complet app → commit → Flux → CR reste à recetter
  à part, et c'est lui qui a le vrai ordre des gestes du cas C.
- **Le dépairage Rancher et le nettoyage Keycloak/Vault/GitLab** pendant ces suppressions :
  l'opérateur n'a aujourd'hui aucun de ces clients, donc les étapes rendent
  `Unknown/NotConfigured`. Les cas ci-dessus les traversent sans les tester. Rien de ce
  plan ne dit si ces nettoyages fonctionnent.
- **La protection de namespace** (`protect-deletion`) : l'étape qui la retire est traversée
  mais aucun cas ne la pose. Un vcluster protégé qu'on supprime est un parcours à part.
- **La restauration Velero et le contenu des sauvegardes** : le cas A vérifie qu'une
  sauvegarde termine avant destruction, pas qu'elle est restaurable. Une sauvegarde
  `Completed` qui ne restaure rien passerait ce plan.
- **Le cas E point 6** (mort du process entre la demande et l'observation) est déclaré
  non déterministe : s'il n'est pas obtenu, l'écrire comme non vérifié plutôt que comme
  vérifié.
- **La prod.** Rien ici ne s'y transpose tel quel.
