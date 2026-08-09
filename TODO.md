# TODO

Backlog des évolutions à venir. Les items terminés sont archivés dans
[`CHANGELOG.md`](CHANGELOG.md).

## ArgoCD dans un vcluster avec quota — hypothèse tranchée (infirmée)

- [x] ~~🔴 **Hypothèse : ArgoCD refusé à l'admission faute de `requests`**~~ —
  **INFIRMÉE le 2026-08-09**, sur une plateforme de recette remontée pour
  l'occasion (K3s 1.32, chart vcluster 0.34.7).

  Ce qui était supposé : le manifeste amont d'ArgoCD ne déclare aucune
  `resources.requests`, or un `resourceQuota` portant sur `requests.X` rend la
  déclaration de X obligatoire — donc ArgoCD serait refusé sur tout vcluster
  ayant un quota.

  La première moitié est vraie et vérifiable sans cluster :
  `argo-cd/stable/manifests/install.yaml` contient 7 workloads et 10 conteneurs,
  **aucun ne déclare `resources`** (les 42 occurrences de `resources:` du fichier
  sont des règles RBAC).

  La seconde moitié est fausse, et c'est le point décisif que ce TODO avait bien
  identifié : **le chart pose un `LimitRange`**. Le wrapper
  `platform-helm-charts/charts/vcluster/values.yaml` porte `limitRange.enabled:
  true` avec `defaultRequest: cpu 50m / memory 128Mi`, et le namespace hôte le
  montre en vrai (`ephemeral-storage: 3Gi` s'y ajoute). Les pods sans requests
  **héritent donc de valeurs et passent l'admission**.

  Constaté de bout en bout sur `recette-restore-a` (quota 2 CPU / 4Gi / 20Gi,
  ArgoCD activé) : Kustomization `argocd-*` `Ready=True`, les 7 pods ArgoCD
  `1/1 Running`, **0 redémarrage**, aucun OOMKill, aucun event d'admission
  refusée. L'échec du 2026-08-08 avait une autre cause — le diagnostic d'alors
  était interrompu par la destruction du cluster, pas par un blocage réel.

  **Coût réel d'ArgoCD, mesuré** (différence entre un vcluster avec et un sans,
  tous deux convergés, lue sur `.status.used` de leur ResourceQuota) :

  | | CPU | mémoire | pods |
  |---|---|---|---|
  | socle vcluster seul (`demo`) | 140m | 470Mi | 3 |
  | avec ArgoCD (`recette-restore-a`) | 490m | 1366Mi | 10 |
  | **ArgoCD** | **350m** | **896Mi** | **7** |

  896Mi = 7 × 128Mi et 350m = 7 × 50m : ce que le quota compte n'est pas un
  besoin d'ArgoCD, c'est le `defaultRequest` du LimitRange appliqué à ses pods.
  Ces chiffres bougeront donc si le LimitRange change, ou le jour où ArgoCD
  déclarera ses propres requests — pas si ArgoCD consomme davantage.
  On est très loin des ~7,2 Gi redoutés.

  ⚠️ **Limite de cette mesure** : cluster de recette au repos, un seul nœud, une
  poignée d'objets à réconcilier. Elle dit qu'ArgoCD démarre et tient, pas qu'il
  tiendrait sous charge — sa `limit` héritée est 1Gi par conteneur, et c'est par
  là qu'un OOMKill arriverait, pas par le quota.

- [ ] 🟠 **Piste distincte, toujours ouverte** : le dépôt GitOps de production
  (gitlab.kosmos.fr) impose `requests.memory: 6Gi` ET `limits.memory: 6Gi` au
  seul `argocd-application-controller` (total ArgoCD ≈ 7,2 Gi). Une `request`
  est une réservation ferme, pas un plafond : cette valeur rend ArgoCD
  incompatible avec tout quota inférieur à 8 Gi. Ce réglage n'existe PAS dans le
  dépôt de recette — la mesure ci-dessus ne dit donc rien de la production.
  Reste à faire, et hors de ce dépôt : descendre les requests du controller vers
  ~1 Gi après mesure réelle sur un ArgoCD de production chargé.

- [x] ~~🔴 **Contrôle bloquant à la création** : refuser une combinaison
  quota/options que le produit sait impossible, avec le minimum requis dans le
  message.~~ Fait — `internal/service/vcluster_quota_floor.go`,
  `ValidateQuotaFitsOptions`, branché dans `Service.Create` (donc UI **et** API
  REST, qui ne passent pas par l'API server).

  Il lit les quotas **effectifs** (`EffectiveQuotas`), pas `req.CPU`/`req.Memory`
  bruts : un champ vide vaut « quota par défaut du générateur », et le lire brut
  laisserait passer précisément les créations qui n'ont rien saisi — la majorité.
  Planchers issus de la mesure ci-dessus, avec leur provenance en commentaire ;
  seules les options dont le coût est mesuré entrent dans le calcul (Velero et le
  bootstrap FluxCD n'y sont pas — les compter avec un chiffre inventé ferait
  refuser des combinaisons viables, ce qui est pire que pas de contrôle).

  Ce que ça ne couvre pas, sciemment : le chemin CR/Flux, où c'est le reconcile
  qui garde (`checkResourceBudget`) et qui répond à la question inverse — « la
  cell a-t-elle encore la place ? ». Les deux contrôles sont complémentaires,
  aucun ne remplace l'autre.

  Vérifié par mutation, 8 mutants, 8 tués, aucun survivant, témoin au vert, avec
  chaque test isolé dans son propre `go test -run` (sans ça, un mutant qui fait
  paniquer `Create` masque tous les tests suivants et on ne sait plus qui tue
  quoi) : appel retiré de `Create` → les 2 tests de `Create` tombent ; champs
  bruts au lieu des effectifs → seul le test qui vise ça tombe ; ArgoCD rendu
  gratuit → 4 tests tombent mais **pas** celui du même quota sans ArgoCD ;
  plancher mémoire divisé par deux → 3 tests tombent, donc c'est bien la valeur
  mesurée qui est verrouillée et pas seulement la présence du contrôle ;
  comparaison inversée → 5 tests dont les cas passants, qui ne passent donc pas
  par accident.

## Certificats des tenants — arbitrage résolu, reste à nettoyer le template

- [x] ~~🔴 **L'arbitrage « faut-il un jeton Cloudflare par tenant ? »**~~ —
  **tranché le 2026-08-09 : non, et la solution est déjà en place.** La
  troisième option envisagée ci-dessous (« un solveur central côté hôte qui
  émette pour eux ») est ce que la plateforme fait déjà : l'hôte porte les
  `ClusterIssuer` `letsencrypt-dns01` (DNS-01 Cloudflare) et `letsencrypt-prod`,
  émet un certificat **wildcard**, et `kubernetes-reflector` le réplique dans les
  namespaces `vcluster-*`. Vérifié sur le cluster : `wildcard-preprod-rebuild-it-fr-tls`
  présent dans les 4 namespaces `vcluster-*`, les deux ClusterIssuers hôte
  `Ready=True`, aucun Gandi côté hôte. Le template ArgoCD du tenant consomme
  déjà ce secret (`spec.tls[0].secretName` patché sur `{{.TLSSecret}}`).
  **Aucun jeton d'API Cloudflare n'est distribué à un tenant**, ce qui était tout
  l'enjeu. Design d'origine : `vcluster-manager-infra/docs/plan-argocd-dns01.md`.

- [ ] 🟠 **Reste à faire : retirer le `ClusterIssuer` Gandi du template tenant.**
  Il n'a plus d'usage sur cette plateforme mais il y est toujours, et il échoue
  bruyamment quand un vcluster l'active. Le seed de recette le contourne déjà par
  suppression (`clusters/preprod/vclusters/demo/tenant/kustomization.yaml` retire
  cert-manager du tenant, avec la note « à réintroduire avec un webhook DNS01
  adapté à Cloudflare ») — c'est un contournement par vcluster, pas un correctif.
  Le fichier visé vit dans le dépôt fluxprod, hors du périmètre de ce dépôt.

  <details><summary>Constat d'origine (2026-08-08), conservé</summary>

  Le template tenant déploie dans chaque vcluster un `ClusterIssuer`
  `letsencrypt-production-dns-gandi`, qui résout ses challenges DNS-01 via
  l'API Gandi. Or le domaine de cette plateforme est géré par **Cloudflare** —
  le `ClusterIssuer` de l'hôte le montre :

  ```
  {"dns01":{"cloudflare":{"apiTokenSecretRef":{"key":"api-token","name":"cloudflare-api-token"}}}}
  ```

  L'API Gandi répond donc `400 : The server could not comply with the request`
  quand le webhook tente de créer l'enregistrement TXT — le domaine n'existe pas
  dans ce compte. Constaté sur `recette-restore-a` :
  `challenge recette-restore-a.preprod.rebuild-it.fr -> pending`,
  `unable to create TXT record: StatusCode: 400`.

  Ce n'est pas un défaut du produit mais un écart entre le dépôt GitOps de
  recette et la plateforme qu'il déploie : le template vient d'un environnement
  où Gandi est bien le registrar. Ça n'en reste pas moins bloquant, puisque plus
  rien ne valide de bout en bout la chaîne « un tenant obtient un certificat ».

  **L'arbitrage n'est pas technique** : aligner le tenant sur Cloudflare suppose
  de distribuer un jeton d'API Cloudflare *dans le vcluster de chaque tenant*,
  donc de confier à chaque tenant un jeton qui porte sur la zone DNS entière.
  Avant d'écrire cette ligne, il faut décider si c'est acceptable, ou s'il faut
  un jeton restreint par tenant, ou un solveur central côté hôte qui émette pour
  eux. À trancher, pas à coder d'abord.

  Distinct du correctif `sourceRef` de cert-manager (PR #13, mergée) : celui-là
  faisait que cert-manager n'était pas **installé** ; ici il l'est, il tourne, et
  c'est son émission qui échoue.

  </details>

## Opérateur — durcissement issu de l'audit N6

- [x] ~~🔴 **`ValidatingAdmissionPolicy` sur les namespaces de l'opérateur**~~ :
      `deploy/base/operator-admission-policy.yaml` restreint DELETE/UPDATE du
      ServiceAccount de l'opérateur aux namespaces `vcluster-*` (hors
      `vcluster-manager`) porteurs du label `vcluster.rebuild-it.fr/managed-namespace:
      "true"`, posé par `gitops.hostNamespace()`. La validation lit `oldObject`
      (l'état avant la requête), jamais `object` : un Server-Side Apply ne peut pas
      poser le label et agir sur l'objet dans le même geste. ⚠️ La flotte
      historique n'a pas ce label et reste donc hors de portée de l'opérateur tant
      qu'elle n'est pas ré-étiquetée sciemment — procédure et grille de décision
      dans `docs/recette-n6-namespace.md` §« VAP namespaces ». Prouvé par
      `kustomize build` (base + overlays) et par les tests Go sur le label ; **pas
      encore vérifié contre un vrai apiserver** (CEL non évalué faute d'un
      cluster 1.35 disponible ici) — à faire en recette.
- [x] ~~🟠 **VAP sur `CREATE vclusters`** (nom contraint)~~ : même fichier,
      seconde policy — `[a-z][a-z0-9-]{0,53}` et `manager` refusé, redondant à
      dessein avec la règle CEL de la CRD (autre chantier) pour ne pas dépendre
      d'un seul fichier. Même limite de preuve que ci-dessus : YAML valide,
      CEL non évalué en réel.
- [x] ~~🟡 **Règle CEL de nom sur la CRD**~~ : deuxième `XValidation` sur `VCluster`
      (`api/v1alpha1/vcluster_types.go`, à côté de la règle `manager`) —
      `self.metadata.name.matches('^[a-z][a-z0-9-]{0,53}$')`. Elle ne fait pas
      double emploi avec `service.ValidName` : les deux couvrent le même charset,
      mais `ValidName` n'a pas de plafond de longueur — la CEL est donc la
      première à borner un nom trop long, avant que le contrôleur ne tente de
      créer un namespace de plus de 63 caractères. Testé sous envtest
      (`internal/controller/vcluster_name_validation_test.go`) : nom valide,
      borne à 54/55 caractères, majuscule, chiffre en tête, `manager` (non-
      régression), message d'erreur lisible. Vérifié par mutation : règle
      retirée, borne relâchée, classe de caractères élargie (majuscule, chiffre
      en tête), message vidé de son contenu — chaque mutant fait tomber
      exactement le test qui le vise, aucun autre.
      ⚠️ Régression connue et non corrigée ici (hors périmètre) :
      `TestAnInvalidNameIsStoppedByTheGuardBeforeProvisioning`
      (`internal/controller/interactions_test.go`) crée un CR nommé `1cluster`
      en supposant que K8s l'accepte pour vérifier ensuite que le contrôleur le
      rattrape — cette règle CEL le refuse maintenant à l'admission, donc ce test
      échoue. Le fixer suppose de changer l'entrée du test ou son intention ;
      `interactions_test.go` est un fichier carrefour, à traiter par qui le
      possède.

## Correctifs recette 1.4.0 → 1.4.1

> Issus de la recette fonctionnelle réelle. Détail + preuves : [`docs/recette-1.4-findings.md`](docs/recette-1.4-findings.md).

- [x] ~~🔴 **Restore Velero — RBAC SA** : `deploy/base/rbac.yaml` — `patch` sur `helmreleases`/`statefulsets`/`deployments` dans les ns tenant (suspend/scale/resume échouent en `forbidden`).~~
      Déjà fait (commit `ca43aaa`, complété par `042c5ad`) : `patch`/`update` sur `helmreleases`,
      `kustomizations`, `statefulsets`, `deployments`, `delete` sur les PVC, `list`/`get` sur les
      pods. **Vérifié directement sur le cluster de recette**, pas seulement lu dans le fichier —
      un test Go ne peut rien prouver ici (envtest n'applique pas le RBAC) : la ClusterRole
      déployée (`kubectl get clusterrole vcluster-manager -o yaml`) est identique bit à bit au
      fichier de HEAD, et `kubectl auth can-i patch {statefulsets,deployments,helmreleases} --as
      system:serviceaccount:vcluster-manager:vcluster-manager` répond `yes` sur les trois. Contre-
      preuve : ClusterRole mutée (verbes `patch`/`update` retirés) → les trois `can-i` basculent à
      `no`, ClusterRole restaurée → retour à `yes`, diff nul avec le fichier source.
      ⚠️ **Même famille que le `list resourcequotas` manquant du ClusterRole opérateur**, trouvé
      à la première minute de la recette du 2026-08-08 : il rendait l'opérateur incapable de
      réconcilier quoi que ce soit, et **aucun test ne pouvait le voir — envtest n'applique pas
      le RBAC**. Les deux ClusterRoles (app et opérateur) ne couvrent pas les mêmes appels et
      rien ne le vérifie. Le vrai correctif est en dessous, pas ici.

- [x] ~~🔴 **Vérifier le RBAC contre les appels réellement émis** (issu de la recette). Ces deux
      trous ont la même cause : les ClusterRoles sont écrits à la main en dérivant « ce que
      telle fonctionnalité touche », donc ils dérivent dès qu'un chemin de code change, et la
      campagne de tests est structurellement aveugle.~~
      **Aucune des trois pistes proposées n'a été retenue telle quelle** : envtest applique déjà
      le RBAC (`--authorization-mode=RBAC`), ce qui l'a rendu invisible jusqu'ici est que le
      client rendu par `testEnv.Start()` est dans `system:masters`, qui court-circuite
      l'autorisation. Il suffit donc de parler à ce même apiserver en IMPERSONANT le
      ServiceAccount de l'opérateur pour que le vrai ClusterRole s'applique — sans cluster, dans
      la suite qui existe déjà. C'est ce qui est fait :
      `internal/controller/rbac_probe_test.go` (le harnais) et `rbac_operator_test.go` (les
      scénarios). Un reverse proxy local réémet les appels sous cette identité et lit le code
      HTTP de chaque réponse ; le kubeconfig du service pointe dessus, donc les appels de
      `internal/kubernetes` sont couverts au même titre que ceux du client controller-runtime —
      y compris ceux dont le code AVALE le refus (`HostNamespaceState`, `CountVClusterPods`
      rendent « je ne sais pas » sur un 403).
      La source de « ce qui est attendu » est le code exécuté, pas une seconde liste : le
      reconcile complet, la séquence de suppression, et les appels Velero/Flux/volume que le
      service émet. Quatre scénarios + un **canari** qui exige un refus sur `list nodes` (sans
      lui, un dispositif qui n'appliquerait plus rien passerait au vert), + `requireExercised`
      qui refuse le vert vide, + `TestOperatorRBACStopsAtWhatTheDesignRefuses` qui transforme en
      test les droits que le fichier dit refuser volontairement (`create vclusterveleroops`,
      `delete backups`, `create vclusters`).
      **Les deux ClusterRoles sont couverts**, pas seulement celui de l'opérateur — c'est le
      constat de l'incident : ils ne couvrent pas les mêmes appels.
      Vérifié par mutation, chaque mutant tué par exactement le test qui le vise et aucun autre :
      `list resourcequotas` retiré → 3 tests tombent, dont le reconcile complet avec le message
      exact de production ; `delete namespaces` retiré → seule la séquence de suppression tombe ;
      `patch kustomizations` retiré → seuls les appels du service tombent ; `delete backups`
      retiré du ClusterRole de l'app → seul le test de l'app tombe.
      Trouvé au passage, sans le chercher : ma propre liste d'opérations incluait
      `RequestVeleroOps`, que l'opérateur n'émet jamais (c'est l'app qui pose l'ordre) — 403
      immédiat. Le dispositif attrape donc aussi une liste trop large, pas seulement un droit
      manquant.
      **Ce qui reste non couvert et pourquoi** : `create pods/portforward` (il faut un pod qui
      tourne, envtest n'a ni kubelet ni scheduler — le `get secrets` qui le précède, lui, est
      couvert) ; les `events` et les `leases`, émis par le manager controller-runtime et non par
      un reconcile ; le `watch` ; le Role namespacé `vcluster-manager-state` ; et surtout
      **l'écart entre le ClusterRole commité et celui réellement déployé** — ces tests lisent
      `deploy/base/*.yaml`, un cluster où le manifeste n'a pas été réappliqué reste cassé avec
      des tests verts. C'est le seul endroit où la piste 3 garde sa valeur : un
      `kubectl auth can-i` en précondition de recette. Reste ouvert ci-dessous.

- [x] ~~🟠 **Précondition de recette : le ClusterRole déployé == le ClusterRole commité.**~~
      Exécutée le 2026-08-09 sur la plateforme remontée, sur les **deux** ClusterRoles :
      comparaison règle par règle entre `kubectl get clusterrole <nom> -o yaml` et les fichiers
      de HEAD (`deploy/base/rbac.yaml`, `operator-rbac.yaml`), après normalisation (tri des
      verbes/ressources, tri des règles) — `vcluster-manager` 12 règles des deux côtés,
      `vcluster-manager-operator` 14 des deux côtés, **diff vide dans les deux sens** (aucune
      règle déployée en trop, aucune manquante). Puis droits effectifs : `create pods
      --subresource=portforward` → `yes` pour l'app (le droit du fix #17 est bien déployé),
      `list resourcequotas` → `yes` pour l'opérateur (le trou du 2026-08-08 est refermé), et
      `create vclusters` / `delete backups` → `no`, conformément à ce que le design refuse.
      ⚠️ **Le piège de la forme avec slash est reproduit à l'identique sur ce cluster** :
      `kubectl auth can-i create pods/portforward` répond `no` alors que
      `--subresource=portforward` répond `yes` sur le même droit. Une précondition écrite avec
      la forme à slash déclarerait un No-Go sur un droit qui est là.
      À rejouer à chaque recette — c'est une précondition, pas un acquis : elle vérifie l'état
      d'un cluster à un instant donné, et le seul trou que les tests Go ne peuvent pas fermer par
      construction reste un cluster où le manifeste n'a pas été réappliqué.
      ⚠️ **Piège mesuré le 2026-08-08 sur le cluster de recette** (kubectl v1.35.2) : la forme
      `kubectl auth can-i create pods/portforward` répond **`no`** alors que le droit est bien
      accordé — `kubectl auth can-i create pods --subresource=portforward` répond `yes`, et
      `--list` affiche bien `pods/portforward [create]`. La forme avec slash ment sur les
      sous-ressources. Une précondition écrite avec elle ferait déclarer un No-Go sur un droit
      qui est là. N'utiliser que `--list` ou `--subresource=`.
      Rien à corriger côté dépôt au moment de ce constat : le ClusterRole déployé sur le cluster
      de recette est identique règle pour règle au fichier de HEAD (17 règles, diff vide).
- [x] ~~🟠 **Restore Velero — topologie** : `internal/kubernetes/velero.go` — cibler l'etcd StatefulSet `vcluster-<name>-etcd` + PVC `data-vcluster-<name>-etcd-0` (control-plane = Deployment, pas StatefulSet).~~
      Déjà fait (commit `3f931c6`) : `detectVClusterTopology` observe ce qui est réellement
      déployé (etcd StatefulSet présent ou non, control-plane Deployment ou StatefulSet) plutôt
      que de supposer un mode, et `ScaleVClusterWorkloads`/`DeleteVClusterPVC`/
      `WaitForVClusterPodsGone`/`GetVClusterPVCState` s'appuient tous dessus — plus de règle
      dupliquée. **Vérifié sur le cluster de recette** (`kubectl get sts,deploy,pvc -n
      vcluster-demo`) : control-plane = `Deployment/vcluster-demo`, etcd =
      `StatefulSet/vcluster-demo-etcd`, PVC = `data-vcluster-demo-etcd-0` — exactement la
      topologie que le code détecte. Couvert par `internal/kubernetes/velero_test.go`
      (`TestDetectVClusterTopology_*`, `TestScaleVClusterWorkloads_*`,
      `TestDeleteVClusterPVC_*`) ; vérifié par mutation : PVC recalculée sur l'ancien nom
      `data-vcluster-<name>-0` → tué par deux tests, `ControlPlaneKind` figé à `StatefulSet` →
      tué par le test topologie Deployment.
- [x] ~~🟠 **Restore Velero — remontée d'échec** : `handlers/api_velero.go` + `velero_restore_status.html` — ne pas afficher « terminé / Flux repris » quand les étapes ont échoué.~~
      Déjà fait côté service (commits `bd4264a`, `9e18675`, `2f0776f`) : `VeleroRestoreStatusView`
      porte `ResumePending`/`ResumeFailed`/`ResumeError`/`VolumeDestroyed`, suivant le même patron
      que `ProtectionState` — un statut non résolu reste « en attente », il ne se travestit ni en
      succès ni en échec. Vérifié par mutation côté service : `VolumeDestroyed` figé à `false` →
      tué par `TestGetVeleroRestoreStatus_FailedInPlaceRestoreFlagsVolumeDestroyed` ; un échec
      transitoire de reprise déclaré `ResumeFailed` au lieu de `ResumePending` → tué par
      `TestGetVeleroRestoreStatus_UnresolvedResumeFailureIsPendingNotFailed`. Le fragment HTML
      lui-même n'avait aucun test qui rende le vrai template (seulement un stub) — ajouté
      `internal/handlers/velero_restore_status_template_test.go`, qui charge le fichier réel et
      vérifie qu'aucun état non résolu ou en échec n'affiche « Flux repris ». Vérifié par
      mutation : suppression de la branche `{{if .ResumeFailed}}` du template → le fragment
      retombe sur « Restauration terminée — Flux repris », tué par ce nouveau test.
- [x] ~~🟠 **UI — bouton « Contenu » backup** : `velero_backups.html` l.68 — envelopper dans `{{if $.User.isAdmin}}` (comme « Restaurer »).~~
      Déjà fait (commit `b0c7351`) : le bouton est sous le même garde que « Restaurer ». Vérifié
      que ce n'est pas de la cosmétique seule — `VeleroBackupContent` (`api_velero.go`) renvoie
      `403` via `service.ErrForbidden` avant même de lire le backup, couvert par
      `TestVeleroBackupContent_RequiresAdmin`. Confirmé aussi en recette réelle (finding S1 du
      doc) : `GET .../content` en tant que `reader` → 403 serveur.
- [x] ~~🟠 **Session expirée** : renvoyer `HX-Redirect: /auth/login` sur 401/403 des endpoints HTMX (au lieu d'injecter le login dans un fragment).~~
      Déjà fait (commits `1ad8939`, `eee9297`) : `redirectToLogin` (`internal/auth/oidc.go`) pose
      `HX-Redirect` sur `HX-Request: true`, 307 classique sinon. Posé dans le middleware
      d'authentification (`CombinedMiddleware`/`OIDCAuth.Middleware`), pas endpoint par endpoint.
      401 vs 403 tranché par construction, pas par un `if` : « je ne sais pas qui tu es » (pas de
      cookie, cookie invalide) passe par `redirectToLogin` → HX-Redirect ; « je sais qui tu es,
      tu n'as pas le droit » (`requireAdmin`, `service.ErrForbidden`) rend un 403 + toast sur
      place, jamais de redirection — rediriger un lecteur vers le login sur un refus d'action
      lui ferait croire qu'il est déconnecté. Test ajouté pour verrouiller cette frontière :
      `TestRequireAdmin_DoesNotRedirectToLogin`. Vérifié par mutation (voir rapport de session) :
      condition `HX-Request` inversée → 5 tests tombent ; `HX-Redirect` ajouté à `requireAdmin` →
      le nouveau test tombe.
- [x] ~~🟠 **Flash de contenu vide** : skeleton (`hx-indicator`) ou opacité + swap différé sur la navigation HTMX.~~
      Déjà fait (commit `b0c7351`) : `layout.html` écoute `htmx:beforeRequest` et ajoute
      `.wf-swap-loading` (opacity 0.5 + pointer-events none, `app.css`) sur la cible du swap
      tant qu'elle a du contenu ; retiré au `htmx:afterSwap` (et en secours à `afterRequest` si
      la requête échoue sans swap). Pas de test Go possible ici (comportement JS/CSS côté
      navigateur) — vérifié par lecture du code, pas par exécution.
- [x] ~~🟡 **Vault webhook** : refonte du template `examples/gitops-repo/lib/tenant-template/vault-webhook` (OCIRepository `v1beta2`, `chartRef`, création du ns `vault-system`) — incompatible avec Flux < 2.6.~~
      Investigué : rien à changer dans ce repo. Le chemin cité n'a jamais existé ici — `git log --all
      -S"gitops-repo"` ne remonte que le commit qui a écrit ce finding, et `git log --all -- examples/`
      n'a qu'un seul commit, l'ajout du pattern navlink. `examples/fluxprod` (le nom actuel) ne démontre
      que ce pattern, jamais de copie de vault-webhook. Ce que ce repo possède réellement
      (`internal/gitops/templates/tenant/vault-webhook/kustomization.yaml.tmpl`, source de vérité —
      déclaré dans `objects.go`) n'est qu'un patch JSON6902 sur la base fluxprod
      (`metadata.namespace` + `spec.kubeConfig.secretRef`), agnostique de la façon dont cette base
      source son chart. Rien à y toucher.
      **Prémisse fausse à corriger** : « incompatible avec Flux < 2.6 » ne tient pas. Vérifié en direct
      sur le cluster de recette (`export KUBECONFIG=~/GIT/github/vcluster-manager-infra/terraform/kubeconfig`) :
      `flux version` → `distribution: flux-2.4.0`, `helm-controller: v1.1.0`, `source-controller: v1.4.1` ;
      `kubectl get crd ocirepositories.source.toolkit.fluxcd.io` → seule `v1beta2` est servie (`v1`
      n'existe pas sur ce Flux) ; `kubectl explain helmrelease.spec.chartRef` → le champ existe et
      accepte `kind: OCIRepository`. Le HelmRelease `vault-webhook` réellement déployé sur ce cluster
      (`vcluster-demo`, ns `vault-system`) utilise déjà `chartRef: {kind: OCIRepository, name:
      vault-secrets-webhook}` + `install.createNamespace: true`, `Ready=True` — la refonte visée par ce
      TODO est déjà en place et fonctionnelle sur le repo fluxprod suivi par la recette
      (`gitlab.com/vcluster-manager/fluxprod.git`, branche `preprod`, hors périmètre de cet agent). Le
      vrai bug n'était pas une histoire de version de Flux mais d'`apiVersion` : `source.toolkit.fluxcd.io/v1`
      n'existe pas sur ce Flux, il fallait `v1beta2`. `install.createNamespace: true` sur le HelmRelease
      crée `vault-system` lui-même, pas besoin d'un namespace pré-créé côté générateur. Détail dans
      `docs/ARCHITECTURE.md` §« Vault webhook (tenant) ».
- [x] ~~**UX — modale de confirmation générique** : remplacer les `window.confirm()` (backup) ; unifier Supprimer / Backup / Restaurer / désactivation protection.~~
      Déjà fait : `wfConfirm` (`layout.html`) intercepte `htmx:confirm` et lit
      `data-confirm-severity`/`data-confirm-label` sur l'élément déclencheur. Utilisé par
      `dashboard.html`, `vcluster_detail.html`, `rancher_status.html`, `velero_backups.html`,
      `protection_status.html`. Vérifié : plus aucun `window.confirm()` dans `web/templates/**`
      (les deux seules occurrences restantes du texte sont des commentaires qui documentent le
      remplacement, pas des appels).
- [x] ~~**UX — toggle protection** : off = gris neutre, on = couleur positive (plus de rouge sur « Inactive »).~~
      Déjà fait : `protection_status.html` l.19-20, commentaire explicite dans le template
      (« Convention : off = gris neutre, on = couleur positive. Le rouge est réservé aux vraies
      alertes. ») — fond `var(--surface3)` à l'arrêt, `var(--status-ready-fg)` (mint) actif.
      Même logique dans `rancher_status.html` pour le pairing.
- [ ] **UX — colonne STATUS dashboard** : dé-empiler FLUX/VAULT/QUOTAS ; badge de synthèse agrégé + détail au survol pour tenir à l'échelle (8-10 vclusters).
      Moitié faite. Le dé-empilement est déjà en place (`status_badge.html`, `.wf-status-groups` en
      colonnes alignées au lieu de blocs empilés). En le lisant j'ai trouvé et corrigé un bug de
      fond distinct : `HelmRelease`/`Kustomization` à `"Unknown"` (valeur réelle documentée dans
      `internal/models/vcluster.go`, produite quand le contrôleur Flux n'a rien de concluant à lire)
      tombaient dans la même branche `{{else}}` que `"NotReady"`/`"Error"` et s'affichaient donc en
      rouge — un signal absent peint comme un échec constaté, le même mensonge que « inconnu peint
      en vert », inversé. Route maintenant vers la classe grise `na`, comme `"N/A"`. Vérifié par
      rendu du template (`html/template` réel, pas un stub) avec `HelmRelease`/`Kustomization` à
      `"Unknown"` → sortie `class="wf-status-tag na"` sur les deux tags.
      Ce qui reste, volontairement pas fait ici : le **badge de synthèse agrégé + détail au
      survol**. C'est une vraie refonte d'information (regrouper Flux/Vault/Quotas — 3 à 6 signaux
      par ligne — dans un badge unique avec détail au survol, sans jamais laisser un état inconnu
      se peindre en "tout va bien") plutôt qu'un ajustement CSS ; je n'ai pas d'accès à design-ux
      dans cette session pour cadrer l'agrégation, et l'imposer sans ce passage risquerait de
      réintroduire un faux vert ailleurs — précisément ce que ce dépôt vient de corriger à plusieurs
      endroits. Item laissé ouvert plutôt que bâclé.
- [x] ~~**UX — tokens de statut** en variables CSS (`--status-ready/pending/inactive/error`) ; états vides dédiés ; contrastes AA des libellés gris.~~
      Tokens et états vides déjà en place : `--status-{ready,pending,inactive,error}-{fg,bg}`
      (`app.css`, commentaire « une seule source de vérité pour badges/toggles/toasts »), alignés
      sur `rebuild-it/branding/brand.md` (mint `#22C3A6` = statuts positifs, indigo `#4F46E5`,
      orange `#FF7A45`) ; états vides avec message + CTA sur dashboard, liste par env et table de
      backups (`vcluster_list.html`, `dashboard.html`, `velero_backups.html`).
      Contrastes AA : pas déjà faits, vérifiés et corrigés ici (calcul WCAG manuel, pas de test Go
      possible sur des valeurs CSS). `--text-muted` (`#565e78`) servait aussi de premier plan aux
      badges "inactif"/"N/A" posés directement sur `--surface3` (leur propre fond) : 2.3:1, loin
      des 4.5:1 requis pour du texte de cette taille — éclairci à `#868fa8` (4.6:1 sur `--surface3`,
      5.1 à 6:1 sur les fonds plus sombres). Conséquence assumée : il se distingue moins de
      `--text-secondary` qu'avant, l'écart de teinte n'était qu'une nuance esthétique.
- [ ] **Branding — charte Rebuild IT** : appliquer la charte éditeur (indigo `#4F46E5` / orange `#FF7A45` / mint `#22C3A6`, Space Grotesk + Inter, logo 4 blocs — `rebuild-it/branding/brand.md`) à l'app ; à mener avec les tokens de statut et le thème Keycloak.
      Largement fait (commits `e4adb51`, `d9224f4`, `61a51bf`) : tokens indigo/orange/mint
      alignés sur `brand.md`, violet hors-charte purgé, fonds sombres recalés sur l'ink de charte,
      polices Space Grotesk/Inter, identité éditeur en nav. Laissé décoché : je n'ai pas vérifié le
      logo 4 blocs contre `rebuild-it/branding/` ni fait de relecture visuelle complète (pas de
      navigateur dans cette session) — à confirmer avant de cocher définitivement.
- [x] ~~**UX — thème Keycloak** custom (logo/couleurs/fond sombre, charte Rebuild IT) pour la page SSO.~~
      Fait : `deploy/keycloak-theme/vcluster-manager/` (`theme.properties` + `login.css` +
      `logo.svg`, pas de `.ftl` réécrit). Déjà déployé et actif en recette (realm
      `vcluster-manager`, ConfigMap `keycloak-login-theme-vcluster-manager`) — vérifié en lisant le
      cluster (lecture seule) et en comparant la page de login servie en HTTP au contenu de ce
      dossier. Voir `deploy/keycloak-theme/README.md` pour ce qui reste à valider visuellement.
- [x] ~~**A11y** : focus visible en thème sombre, `aria-label` sur icônes seules, `aria-live`/`role="alert"` sur les toasts.~~
      `aria-label`/toasts déjà faits (`layout.html`, `toast.html`) : `aria-live="polite"` sur le
      conteneur, `role="alert"`+`aria-live="assertive"` sur les toasts d'erreur. Balayé tous les
      `<button>`/`<a>` sans texte de `web/templates/**` : aucun sans `aria-label`.
      Focus visible existait déjà (anneau `outline` sur thème sombre) mais l'anneau lui-même ratait
      sa cible : indigo sur les fonds de carte ne tient que 2.4:1 à 3.1:1 selon l'endroit, sous les
      3:1 exigés pour un indicateur d'interface (WCAG 1.4.11) — un anneau qu'on distingue mal ne
      sert à rien. Passé à l'orange de charte (5.8 à 7.6:1 partout, calcul WCAG manuel). Trouvé et
      corrigé au passage : le toggle générique `.switch` (checkbox masquée + `.switch-track`)
      dessinait son anneau en `box-shadow` collé au bord de la piste, dont la couleur change avec
      l'état (gris éteint / indigo allumé) — aucune couleur d'anneau unique ne tient 3:1 dans les
      deux cas à la fois. Passé en `outline` + `outline-offset:2px` comme le reste du site : l'anneau
      se dessine sur le fond de page, uniformément sombre, plus sur la piste elle-même. Ajouté aussi
      `input[type="checkbox"]` à la liste générique des sélecteurs `:focus-visible`, qui l'omettait
      (sans effet sur les checkbox masquées `.sr-only`/`.switch`, mais nécessaire pour les checkbox
      visibles des formulaires — création, suppression, détail).
      Rien de tout ça n'est testable par `go test` (CSS/attributs HTML) : vérifié par lecture
      exhaustive + calcul de contraste WCAG, pas par exécution automatisée.

## Améliorations Go (issu de l'audit skills)

- [x] ~~**Migration `log` → `slog` (phase 1)** : init JSON handler dans
      `main.go`, bridge du package `log` standard via
      `slog.SetLogLoggerLevel`, conversion enrichie de `cmd/server/main.go`
      et `internal/audit/audit.go`.~~
- [x] ~~**Migration `log` → `slog` (phase 2)** : enrichir avec des fields
      structurés (`slog.Error("foo", "err", err)`) les ~150 call sites
      restants dans `internal/handlers/*` (98), `internal/kubernetes/*` (10),
      `internal/gitops/*` (8), `internal/rancher/`, `internal/vault/`, etc.~~
      Phase 3 (corrélation `slog.*Context(ctx, ...)`) à planifier séparément.
- [x] ~~**Cache GitLab maison → `samber/hot`** : `internal/gitops/gitlab.go`
      embarque un cache `map+sync.RWMutex+TTL 30s` (~40 LOC). `samber/hot`
      apporte LRU/TinyLFU, métriques Prometheus, et purge des entrées expirées
      (le maison ne purge jamais : croissance mémoire non bornée).~~
- [x] ~~**`errgroup` au lieu de `WaitGroup`** dans `parser.ListVClusters` : pas
      de propagation d'erreur, pas d'annulation si un parse échoue.~~
- [x] ~~**`withRetry` cancellable** : `time.Sleep` bloquant dans
      `gitops/gitlab.go:80` retarde le shutdown jusqu'à 17s. Ajouter `ctx` et
      `select { case <-ctx.Done(): ...; case <-time.After(delay): }`.~~
- [x] ~~**`notify.Send` avec contexte** : `n.client.Post(...)` → utiliser
      `http.NewRequestWithContext(ctx, ...)`. Permet d'annuler un webhook
      bloqué quand l'utilisateur ferme l'onglet.~~
- [x] ~~**Constructeur `handlers.New` à 12 args** : remplacer par struct config
      ou functional options.~~ Struct `handlers.Deps`.
- [x] ~~**Découpe `internal/handlers/api.go`** (1275 LOC) : `api_velero.go`,
      `api_rancher.go`, `api_protection.go`, `api_chart.go`, `api_apps.go`.~~
- [x] ~~**Découpe `internal/kubernetes/status.go`** (1413 LOC) : `status.go`,
      `vcluster_access.go`, `velero.go`, `rancher.go`, `protection.go`.~~
- [x] ~~**`atoi` avec `fmt.Sscanf`** dans `gitlab.go` → `strconv.Atoi` (plus
      rapide, erreur explicite).~~ Étendu à `gitops/generator.go`,
      `github/releases.go` et `config/config.go`.
- [x] ~~**`/metrics` derrière le rate limiter ?** Aujourd'hui sur le mux global
      sans middleware. À décider : assumé (Prom scrape interne) vs DoS-vector.~~
      Rate-limiter dédié (5 req/s, burst 10) sans auth.
- [x] ~~**`doc.go` par package** : permettrait de réactiver `ST1000/ST1020` au
      lint. Une passe mécanique.~~ ST1000 activé ; ST1020 (exported symbols)
      reste désactivé — une passe godoc complète est un effort séparé.
- [x] ~~**Tests manquants** : `internal/gitops/gitlab.go` (`withRetry` testable
      avec `httptest`), `internal/notify/webhook.go`, `internal/auth/oidc.go`,
      `internal/rancher/client.go`.~~
- [x] ~~**Errcheck cleanup** : ~21 erreurs réelles non check via `go fmt` dans
      handlers et clients. Soit fix soit `//nolint:errcheck` motivé.~~

## Portabilité Git provider

- [ ] **Support GitHub** (ou tout autre provider Git) : actuellement couplé à
      l'API GitLab via `github.com/xanzy/go-gitlab`. Nécessiterait une
      interface `GitProvider` abstraite + réimplémentation complète de
      `internal/gitops/gitlab.go` (commits multi-fichiers atomiques, MR→PR,
      deploy keys, création de repos dans une org). Voir analyse dans
      `FORK.md`. Grosse feature, à planifier séparément.
- [x] ~~**Migration `xanzy/go-gitlab` → `gitlab.com/gitlab-org/api/client-go`** :
      le module `xanzy` est archivé depuis 2024. La migration permet aussi de
      retirer l'exclusion `SA1019` au lint.~~

## UX / Internationalisation

- [ ] **Support multilingue (i18n)** : l'interface est actuellement en
      français mixte avec quelques termes anglais ; prévoir FR/EN minimum via
      un mécanisme de traduction (fichiers de messages, Accept-Language, ou
      cookie de préférence).

## À retirer quand résolu

- [ ] Workaround Pod exclusion ArgoCD
      (`fluxprod/lib/tenant-template/argocd/base/configmap-argocd-cm.yaml`) à
      retirer quand le bug est corrigé upstream (ArgoCD 3.3.3+).
      Vérifié le 2026-08-08, laissé ouvert : le bug est toujours ouvert en amont —
      [argoproj/argo-cd#26529](https://github.com/argoproj/argo-cd/issues/26529), panic
      « assignment to entry in nil map » dans `k8s.io/kubectl/pkg/util/resource.maxResourceList`,
      appelé par `populatePodInfo` sur un pod sans `resources` défini, dans un vcluster avec
      LimitRange actif. Le changelog de la 3.3.3 (récupéré via l'API GitHub) ne mentionne aucun
      correctif dessus, et la dernière release (v3.5.0) n'a rien fermé non plus — la timeline de
      l'issue montre d'autres équipes contournant encore le même bug par la même exclusion en
      mai et juillet 2026. Rien à changer ici : le fichier visé vit dans le repo fluxprod, hors du
      périmètre de cet agent, et la condition de retrait (bug corrigé) n'est de toute façon pas
      remplie. Repasser dessus quand `argoproj/argo-cd#26529` ferme.
