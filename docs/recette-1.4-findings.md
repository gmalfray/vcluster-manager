# Recette 1.4.0 — findings (déploiement réel + fonctionnel + UI/UX)

> Recette menée sur une infra de test complète (Hetzner + K3s + Flux + Keycloak + Vault +
> cert-manager + Velero), app 1.4.0 déployée par image `ghcr.io/gmalfray/vcluster-manager:latest`,
> vcluster créé via GitOps. Objectif : valider en réel le travail de la 1.4.0 (extraction couche
> service + durcissements sécu) et sortir les correctifs pour la **1.4.1**.

## ✅ Validé en réel

- **Déploiement bout-en-bout** : infra → app → vcluster créé via Flux/GitOps, app joignable en HTTPS (Let's Encrypt), OIDC + admin local.
- **Anti-injection GitOps (F1)** : rejet confirmé sur **deux** chemins — settings (CPU `2"; injected: true` → « quantité Kubernetes invalide ») et création (nom `../Evil"; injected` → « Nom invalide »). Aucun commit fluxprod déclenché.
- **Contenu de backup admin-only (S1)** : en tant que `reader`, `GET /api/vclusters/demo/velero/backups/…/content` → **HTTP 403**. Enforcement serveur, pas juste masquage UI.
- **RBAC** : le reader ne voit ni création, ni suppression, ni settings, ni backup/restore ; les mutations sont refusées côté service.
- **Backup Velero FSB** : `Completed` (79/79), node-agent actif.
- **Protection namespace** : annotation `protect-deletion: true` réellement posée + événement d'audit.
- **Audit** : backup / restore / protection loggés avec l'acteur.

## 🔴 / 🟠 Findings fonctionnels → 1.4.1

### 1. 🔴 Restore Velero in-place — RBAC du ServiceAccount
Le SA `vcluster-manager` ne peut pas `patch` les `helmreleases` (Flux) ni les `statefulsets` dans les namespaces `vcluster-*`. Les 3 étapes du restore (suspend Flux → scale down → resume Flux) échouent en `forbidden`.
- **Preuve** : `helmreleases.helm.toolkit.fluxcd.io "vcluster-demo" is forbidden … cannot patch` + idem statefulsets.
- **Fix** : élargir la ClusterRole du SA (`deploy/base/rbac.yaml` côté app + RBAC infra) — verbs `patch` sur `helmreleases`, `statefulsets` (et probablement `deployments`) dans les namespaces tenant.

### 2. 🟠 Restore Velero in-place — topologie ciblée fausse
`ScaleVClusterStatefulSet(name)` / `DeleteVClusterPVC(name)` visent un StatefulSet/PVC nommés `vcluster-<name>`, mais la topologie réelle du vcluster est : **control-plane = Deployment** `vcluster-<name>` + **etcd = StatefulSet** `vcluster-<name>-etcd`, dont la donnée vit dans la PVC **`data-vcluster-<name>-etcd-0`**.
- **Effet** : le scale et la suppression de PVC ne trouvent pas leur cible → no-op → la séquence destructrice ne s'exécute jamais.
- **Fix** (`internal/kubernetes/velero.go`) : cibler l'etcd StatefulSet + sa PVC (`data-vcluster-<name>-etcd-0`), ou détecter la topologie (Deployment vs StatefulSet embarqué). Attention : ces fonctions ont besoin des droits du point 1.

### 3. 🟠 Restore Velero in-place — remontée d'échec trompeuse
Toutes les étapes échouent (Warn/Error, dont un `ERROR could not resume flux after restore`) **mais l'UI affiche « Restauration terminée — Flux repris »**. Le handler doit surfacer l'échec (statut d'erreur, message), pas claironner le succès.
- **Fix** (`internal/handlers/api_velero.go` + fragment `velero_restore_status.html`) : propager l'état réel des étapes ; ne pas afficher « Flux repris » si la reprise a échoué.
- **Note positive** : malgré ces échecs, **fail-safe** — aucune donnée perdue (no-op sûr, vcluster intact).

### 4. 🟠 Bouton « Contenu » du backup non gardé admin (UI)
`web/templates/partials/velero_backups.html` (~l.68) : le bouton « Contenu » n'est pas enveloppé dans `{{if $.User.isAdmin}}` (contrairement à « Restaurer », l.74). Le reader le voit et se prend le 403. Pas de faille (serveur = 403, cf. S1), mais UI incohérente.
- **Fix** : envelopper « Contenu » dans le même garde admin.

### 5. 🟠 Expiration de session → login injecté dans un fragment HTMX
Sur expiration, un poll HTMX (ex. la carte Status) reçoit la page de login complète et la **swappe dans le fragment** → formulaire de login flottant dans une carte.
- **Fix** : sur 401/403 d'un endpoint HTMX, renvoyer un header `HX-Redirect: /auth/login` (HTMX gère nativement) plutôt qu'un template de page. Garde-fou client possible en attendant.

### 6. 🟠 Flash de contenu vide à chaque navigation
Le body se vide plusieurs secondes avant l'arrivée du fragment async — pas de skeleton/loader.
- **Fix** : soit skeleton via `hx-indicator`, soit garder l'ancien contenu à `opacity-50 pointer-events-none` pendant la requête (`htmx:beforeRequest`/`afterRequest`) et ne swapper qu'au retour.

### 7. 🟡 Vault webhook — template incompatible avec le Flux déployé
Le tenant `vault-webhook` ne se déploie pas (donc le setup Vault du vcluster reste « En attente »). Trois couches d'incompatibilité identifiées avec Flux v1.4.1 / helm-controller v1.1.0 :
1. `OCIRepository` en `source.toolkit.fluxcd.io/v1` → n'existe qu'en `v1beta2` sur ce Flux.
2. `HelmRelease.spec.chart.spec.sourceRef.kind: OCIRepository` non supporté → doit passer par `spec.chartRef`.
3. Après ces deux corrections : `namespaces "vault-system" not found` → la Kustomization place les ressources dans un namespace inexistant (structure/namespace du template à revoir).
- **Fix** : refonte du template `lib/tenant-template/vault-webhook` (chartRef + création/targeting du namespace) OU bump de Flux ≥ 2.6. **Le même correctif vaut pour `examples/gitops-repo`** du repo app.

## 🎨 Revue UI/UX (agent design-ux)

**Base graphique saine** (palette sombre cohérente, cards à barre colorée, bouton primaire reconnaissable). Les points ci-dessous cassent la confiance au mauvais moment ou nuisent au balayage — plus importants que le polish pour un outil ops.

### Quick wins (fort impact / faible coût)
- **Session expirée** : `HX-Redirect` au lieu d'injecter le login dans un fragment (= finding #5).
- **`window.confirm()` → modale existante** : réutiliser la modale du « Restaurer » pour Backup maintenant / Supprimer / désactivation protection → une seule modale de confirmation générique (titre, texte, bouton coloré selon sévérité).
- **Bouton « Modifier » (version K8s par défaut)** : le laisser `disabled` tant que le select « Choisir… » est vide.
- **Toggle « Protection namespace »** : il est **rouge** en état « Inactive ». Convention à fixer : off = gris neutre, on = couleur positive ; rouge réservé aux vraies alertes.
- **Colonne STATUS du dashboard** : FLUX/VAULT/QUOTAS empilés verticalement font exploser la hauteur de ligne et cassent la comparaison multi-lignes → une colonne par sous-statut, alignées.

### Chantiers courts
- **Flash de contenu vide** (= finding #6) : skeleton ou opacité + swap différé.
- **Contrastes** des libellés en petites capitales (gris sur fond quasi noir) à vérifier (AA).
- **États vides dédiés** (dashboard 0 vcluster, table backups vide) : message + CTA plutôt qu'un tableau vide.
- **Tokens de statut** formalisés en variables CSS (`--status-ready/pending/inactive/error`) — corrige aussi le toggle rouge.

### Chantiers plus lourds
- **Charte Rebuild IT (éditeur)** : vCluster Manager est un produit Rebuild IT → appliquer la charte éditeur (indigo `#4F46E5` / orange `#FF7A45` / mint `#22C3A6`, Space Grotesk + Inter, logo 4 blocs — source `rebuild-it/branding/brand.md`, distincte du Terracotta client). L'indigo primaire actuel est déjà proche ; reste le logo, les couleurs secondaires (orange/mint pour accents/états), les fonts, et la cohérence avec le thème Keycloak. Chantier à mener avec les tokens de statut et le thème Keycloak ci-dessous.
- **Thème Keycloak** custom (logo, couleurs, fond sombre, **charte Rebuild IT**) — la rupture de charte au SSO est le pire endroit (souvent le 1er écran).
- **Densité du dashboard à l'échelle** : à 8-10 vclusters la table empilée devient un scroll interminable → badge de synthèse agrégé + détail au survol/clic ; barres CPU/MEM/STOR gardées en page détail, valeur texte compacte en vue liste.
- **Accessibilité** clavier/lecteurs d'écran : focus visible en thème sombre, `aria-label` sur les icônes seules (thème, menu ⋮), `aria-live`/`role="alert"` sur les toasts d'erreur.

### À ne pas casser
Cards à barre colorée (leur **donner un sens** d'état plutôt qu'une couleur arbitraire), badges d'état cohérents, bouton « Supprimer » en contour rouge (bonne référence de danger).

## Findings infra (repo `vcluster-manager-infra`, la plupart déjà corrigés sur `refactor/infra-fixes`)

- SKU `cpx51` indispo à nbg1 → `cx53`.
- Token Cloudflare scopé sur la mauvaise zone (pensebonheur.fr) → ajout de rebuild-it.fr.
- GitRepository Flux : URL SCP `git@…:` → forme `ssh://…` requise (`fluxcd.tf`).
- ClusterIssuer `letsencrypt-prod` non créé au 1er apply (timing null_resource) → `-replace`.
- Rancher : chart `kubeVersion < 1.36` incompatible avec k3s v1.36.2 → désactivé (`rancher.tf.disabled`) ; à réactiver avec un k3s pinné.
- `04-app` : branche suivie `master` → `main`.
- `window.confirm()` sur le backup côté app (cf. #4/UX).
</content>
