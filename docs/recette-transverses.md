# Recette preprod — transverses : notifications, audit, CSRF, auth, métriques, rate limiting

> Périmètre : ce qui n'appartient à aucune fonctionnalité et que personne ne recette
> donc jamais. Six questions, six cas. Rien ici ne crée ni ne supprime de vcluster :
> les parcours métier sont couverts par `docs/recette-cycle-de-vie.md` et
> `docs/recette-restauration.md`, qui tournaient en même temps que ce passage.
>
> **Passage joué le 2026-08-08 entre 16:10Z et 16:24Z** sur
> `https://vcluster-manager.rebuild-it.fr` (image `:main`, pod
> `vcluster-manager-b6b96565c-tb2pg`, démarré à 11:02:33Z, 0 redémarrage). Les blocs
> « MESURÉ » donnent ce qui a été observé ; le reste est le plan, rejouable tel quel.

## Ce que la recette doit trancher

| Cas | Question | Verdict lu dans | Verdict rendu |
|---|---|---|---|
| T1 | Une suppression émet-elle une notification webhook ? | env du pod, puis logs du receveur | **NON COUVERT** + 2 écarts lus dans le code |
| T2 | Une écriture laisse-t-elle une ligne d'audit, avec le bon acteur ? | `kubectl logs` filtré sur `"audit":true` | **PASS avec réserves** |
| T3 | Une écriture sans en-tête CSRF est-elle refusée, sur **toutes** les routes ? | code HTTP **et** corps de la réponse | **PASS** (19/19) |
| T4 | Une route d'écriture est-elle atteignable sans session ? | code HTTP des 19 routes sans cookie | **PASS**, avec 2 défauts sur les chemins d'authentification |
| T5 | `/metrics` répond-il, et n'expose-t-il rien de sensible ? | corps de `/metrics` | **PASS** sur le contenu ; exposition publique confirmée puis corrigée en cours de route (D8) |
| T6 | Le limiteur bride-t-il réellement, et sur quelle clé ? | répartition 200/307 vs 429 | **PASS**, avec un défaut de conception non exploitable ici |

## Règles de sécurité de ce plan

- **Aucune écriture métier.** Les cas d'écriture visent soit un nom de vcluster qui
  n'existe pas (`zz-csrf-inexistant`), soit un namespace créé pour l'occasion
  (`vcluster-zz-audit`), jamais `demo` ni les vclusters des deux autres recettes.
- **Le témoin des cas CSRF passe par un lecteur, pas par un admin.** Rejouer une route
  d'écriture avec un jeton CSRF valide et une session admin exécuterait l'action.
  Avec une session lecteur, la requête traverse le CSRF et s'arrête au contrôle RBAC :
  la route est prouvée atteignable sans que rien ne parte dans fluxprod.
- **Le rate limiting se joue en dernier et en fenêtre nommée.** Les seaux sont par IP,
  et les autres recettes sortent par la même IP publique. Trois des quatre volées
  passent par un port-forward (seau `127.0.0.1`, isolé) ; la quatrième, publique, est
  volontairement calibrée à 60 requêtes.
- Aucun cookie de session, jeton CSRF, mot de passe ou contenu de secret dans ce
  document. Les cookies capturés vivent dans un répertoire temporaire en 0600.

## Préconditions

```bash
export KUBECONFIG=~/GIT/github/vcluster-manager-infra/terraform/kubeconfig
export B=https://vcluster-manager.rebuild-it.fr
kubectl -n vcluster-manager get pod -l app=vcluster-manager   # doit être Ready
```

Trois sessions sont nécessaires, et la recette ne veut rien dire si elles ne sont pas
les trois :

| Session | Ouverture | Rôle attendu |
|---|---|---|
| admin local | `POST /auth/local/login` (`username=admin`, mot de passe = `terraform output -raw admin_password` dans `03-config`) | admin |
| `testadmin` | flow OIDC Keycloak complet | admin |
| `testreader` | flow OIDC Keycloak complet | lecteur |

Le flow OIDC ne se fait pas en une commande : `curl -L` perd le cookie `oauth_state`
entre la redirection vers Keycloak et le retour sur `/auth/callback`, et le callback
répond alors `400 Invalid state`. Il faut le dérouler en quatre appels — `GET /auth/sso`
(pose `oauth_state`), la page Keycloak, le `POST` du formulaire, puis le callback — en
partageant le même cookie jar. Le script utilisé est reproduit en annexe.

Vérification que les trois sessions sont bien ce qu'elles prétendent :

```bash
curl -s -b jar-admin.txt     -o /dev/null -w '%{http_code}\n' $B/config   # attendu 200
curl -s -b jar-testadmin.txt -o /dev/null -w '%{http_code}\n' $B/config   # attendu 200
curl -s -b jar-reader.txt    -o /dev/null -w '%{http_code}\n' $B/config   # attendu 403
```

> **MESURÉ** : 200 / 200 / 403. `GET /config` est gardé par `requireAdmin`, c'est donc
> un discriminant suffisant pour le rôle.

---

## Cas T1 — les notifications webhook

**Ce qu'on cherche.** Le README annonce (ligne 33) « notification
Slack/Mattermost/Rocket.Chat a la suppression d'un vcluster (debut et confirmation) ».
Cinq vclusters ont été supprimés dans la journée sans qu'aucune notification n'arrive.
Deux hypothèses, et il faut trancher laquelle avant de parler de défaut : soit aucun
webhook n'est configuré, soit il l'est et les notifications sont muettes.

### T1.1 — qu'est-ce qui est configuré ?

```bash
kubectl -n vcluster-manager get cm vcluster-manager-config -o jsonpath='{.data}' | grep -o WEBHOOK_URL
kubectl -n vcluster-manager get secret vcluster-manager-secrets -o jsonpath='{.data}' | grep -o WEBHOOK_URL
kubectl -n vcluster-manager get secret vcluster-manager-auth    -o jsonpath='{.data}' | grep -o WEBHOOK_URL
POD=$(kubectl -n vcluster-manager get pod -l app=vcluster-manager -o jsonpath='{.items[0].metadata.name}')
kubectl -n vcluster-manager exec $POD -- printenv | grep -i webhook
```

**Attendu si le webhook est configuré** : `WEBHOOK_URL` dans le Secret, et présent dans
l'environnement du process. **Où le lire** : la sortie ci-dessus, et au démarrage la
ligne `"webhook notifications enabled"` dans les logs de l'app.

> **MESURÉ — c'est la réponse à la question posée.** `WEBHOOK_URL` n'est **nulle part** :
> ni dans le ConfigMap (39 clés, aucune), ni dans les deux Secrets (9 + 4 clés, aucune),
> ni dans l'environnement du process (`printenv | grep -i webhook` → aucune ligne, code
> de retour 1). `deploy/base/configmap.yaml` porte d'ailleurs un commentaire qui dit de
> le mettre dans le Secret, et personne ne l'y a mis.
>
> `cmd/server/main.go:248` fait `if cfg.WebhookURL != ""` : sans la variable, le
> `Notifier` reste nil, et `notify()` retourne immédiatement. **Aucune notification ne
> pouvait partir.** C'est une non-couverture de la configuration, pas un défaut du code.

### T1.2 — le chemin réseau existe-t-il ?

Poser un receveur jetable et vérifier que le pod de l'app peut l'atteindre, pour que
le seul inconnu restant soit « est-ce que le point d'appel se déclenche ».

```bash
kubectl create ns zz-recette-transverses
# pod webhook-receiver + service, manifeste en annexe
kubectl -n vcluster-manager exec $POD -- sh -c \
  'wget -q -O- --header="Content-Type: application/json" \
   --post-data="{\"text\":\"sonde\"}" \
   http://webhook-receiver.zz-recette-transverses.svc.cluster.local/hook'
kubectl -n zz-recette-transverses logs webhook-receiver
```

**Attendu** : `ok` côté app, et côté receveur une ligne
`RECU /hook application/json {"text":"sonde"}` — même forme de charge utile que
`notify.payload`.

> **MESURÉ** : exactement ça. Le conteneur de l'app embarque `wget`, la résolution DNS
> inter-namespace passe, aucune NetworkPolicy ne bloque. Si `WEBHOOK_URL` pointait sur
> ce service, le POST arriverait.

### T1.3 — le déclenchement réel — **NON COUVERT, et pourquoi**

Pointer la configuration sur le receveur demande `WEBHOOK_URL` dans le Secret
`vcluster-manager-secrets` puis un redémarrage du pod : les variables d'environnement
sont lues une fois, dans `run()`, il n'y a pas de rechargement à chaud. Le Deployment
est par ailleurs géré par Flux (`kustomize.toolkit.fluxcd.io/name: vcluster-manager`),
donc un patch manuel serait repris.

Ce redémarrage n'a **pas** été fait, et c'est une décision, pas un oubli : deux autres
recettes utilisaient l'app au même moment, l'une en pleine restauration Velero. Un
redémarrage tue les goroutines en vol (attente du setup Vault, attente du job
`rancher-cleanup`, migrations d'apps) — elles sont reprises au démarrage par les
réconciliateurs, mais la reprise est justement un des parcours qu'ils recettent.
Ils auraient chassé une panne que j'aurais fabriquée.

**Procédure à jouer quand l'app est libre** (elle tient en cinq gestes) :

```bash
flux suspend kustomization vcluster-manager -n flux-system
kubectl -n vcluster-manager patch secret vcluster-manager-secrets --type=merge -p \
  "{\"stringData\":{\"WEBHOOK_URL\":\"http://webhook-receiver.zz-recette-transverses.svc.cluster.local/hook\"}}"
kubectl -n vcluster-manager rollout restart deploy/vcluster-manager
kubectl -n vcluster-manager logs deploy/vcluster-manager | grep "webhook notifications enabled"
# puis supprimer un vcluster preprod jetable depuis l'UI, onglet ouvert sur le détail
kubectl -n zz-recette-transverses logs -f webhook-receiver
```

**Attendu** : deux lignes chez le receveur — `Suppression du vcluster *<nom>* (preprod)
en cours...` au commit, puis `vcluster *<nom>* (preprod) supprime avec succes.` quand le
HelmRelease disparaît.

**Retour arrière** : retirer la clé du Secret, `flux resume kustomization
vcluster-manager -n flux-system`, supprimer le namespace `zz-recette-transverses`.

### T1.4 — ce que le code dit déjà de la couverture

Lu, pas exécuté, et à confirmer par T1.3. Il n'y a que deux appels à `Send` dans tout le
dépôt hors tests : `internal/service/vcluster.go:698` et `internal/handlers/api.go:79`.

**D1 — aucune notification pour une suppression en prod.** L'appel du début est à
l'intérieur du bloc `if deletePreprod`, après le commit preprod. Le bloc prod juste en
dessous (commit direct si `pending`, MR sinon) n'en a aucun. Supprimer un vcluster prod
n'annonce rien, ni au début ni à la fin. Le README ne distingue pas les environnements.

**D2 — la confirmation dépend d'un navigateur resté ouvert.** L'appel de confirmation
est dans `StatusFragment`, la branche `IsDeleting(...) && r.URL.Query().Get("deleting")
== "true"`. C'est le poll HTMX de la page de détail qui la déclenche. Personne sur la
page au moment où le HelmRelease disparaît, personne n'est notifié — et l'entrée
`deleting` reste posée jusqu'à sa péremption à 24 h. Une notification « la suppression
est finie » qui exige qu'un humain regarde l'écran au bon moment ne remplit pas la
fonction annoncée.

**Ce qui invalide ces deux points** : les voir démentis par T1.3 joué jusqu'au bout,
c'est-à-dire une notification de fin reçue sans onglet ouvert, ou une notification pour
une suppression prod.

**Verdict T1 : NON COUVERT** pour le déclenchement réel (aucun webhook configuré, test
de bout en bout non joué et pourquoi). **Écart documentaire constaté** : le README
promet plus que ce que les deux points d'appel couvrent (D1, D2).

---

## Cas T2 — l'audit log

**Ce qu'on cherche.** Qu'une écriture laisse une ligne, avec l'acteur réel — et pas
toujours le même acteur, ce qui serait la façon la plus discrète de ne rien tracer.

### T2.1 — trois acteurs, une action réversible

L'action choisie est la protection de namespace : elle est admin, elle a une ligne
d'audit (`internal/service/protection.go:99`), elle ne touche à rien d'autre qu'une
annotation, et elle peut viser un namespace créé pour l'occasion.

```bash
kubectl create ns vcluster-zz-audit
date -u +%H:%M:%SZ
# les trois POST portent Cookie: session_token + csrf_token, et X-CSRF-Token
POST $B/api/vclusters/zz-audit/enable-protection   # session admin local
POST $B/api/vclusters/zz-audit/disable-protection  # session testadmin
POST $B/api/vclusters/zz-audit/enable-protection   # session testreader
kubectl -n vcluster-manager logs deploy/vcluster-manager --since=5m | grep '"audit":true'
```

**Attendu** : deux lignes, avec **deux acteurs différents**, et **rien** pour le lecteur.

> **MESURÉ à 16:16:07Z** :
>
> ```
> 200 | Protection namespace Active     (admin local)
> 200 | Protection namespace Inactive   (testadmin)
> 403 | Accès refusé : droits administrateur requis   (testreader)
> ```
>
> ```
> 2026-08-08T16:16:07 | user='admin'      | action=enable-protection  | vcluster=zz-audit | env=preprod
> 2026-08-08T16:16:07 | user='Test Admin' | action=disable-protection | vcluster=zz-audit | env=preprod
> ```
>
> Deux acteurs distincts, l'écriture refusée ne laisse rien. Le piège de l'acteur
> toujours identique n'est pas là. À noter pour la lecture des logs de la journée :
> les trois lignes `create` de 16:07–16:09 portent toutes `admin` parce que les deux
> autres recettes travaillent en admin local, pas parce que l'audit écrase l'acteur.

**D3 — l'acteur est un nom d'affichage, pas un identifiant.** `admin` vient du claim
`name` du JWT local (`internal/auth/local.go:61`) ; `Test Admin` vient du claim `name`
du jeton Keycloak. Ni `preferred_username`, ni `sub`, ni l'e-mail. Deux comptes portant
le même nom d'affichage sont indistinguables dans l'audit, et un utilisateur qui change
son nom dans Keycloak change son identité dans toutes les lignes qui suivent. Pour un
journal dont la fonction est de dire qui a fait quoi, c'est la mauvaise clé.

### T2.2 — quelles écritures ne laissent rien

Inventaire croisé entre les 19 routes d'écriture et les appels à `audit.Log` /
`audit.LogActor`. Lu dans le code, pas exécuté : déclencher ces trois routes-là
écrirait pour de vrai dans fluxprod ou changerait le kubeconfig de l'app.

```bash
grep -rn "audit.Log\|audit.LogActor" --include=*.go . | grep -v _test.go
grep -c "internal/audit" internal/handlers/cluster_config.go   # attendu : 0
```

**D4 — trois écritures sans ligne d'audit :**

| Route | Handler | Ce qu'elle fait sans laisser de trace |
|---|---|---|
| `POST /config/{env}` | `UpdateClusterConfig` | téléverse un kubeconfig, change la cible SSH et le label du cluster, reconstruit le client k8s |
| `POST /api/vclusters/{name}/create-prod-mr` | `CreateProdMR` | ouvre la MR `preprod → master`, c'est-à-dire la porte de la production |
| `POST /api/vclusters/{name}/vault-setup-retry` | `RetryVaultSetup` | relance un setup Vault |

La première est la plus gênante : `internal/handlers/cluster_config.go` n'importe même
pas le paquet `audit`, et la fonction fait 104 lignes. Changer le kubeconfig, c'est
changer le cluster que l'app pilote. Aucune trace de qui l'a fait.

### T2.3 — l'audit survit-il au pod ?

```bash
kubectl get ds,deploy -A | grep -iE "fluent|vector|loki|promtail|filebeat|otel"
```

> **MESURÉ** : aucun collecteur. Le commentaire de `internal/audit/audit.go` dit
> « Output is captured by Kubernetes/Fluentd » ; sur cette plateforme, rien ne le
> capture.
>
> **D5 — l'audit meurt avec le pod.** Vérifié par les faits du jour : le ReplicaSet
> `vcluster-manager-67d86f5d66` a été mis à zéro vers 11:02Z et les logs de son pod
> sont perdus. C'est pour ça que les cinq suppressions signalées n'apparaissent nulle
> part — les logs du pod courant (démarré 11:02:33Z, 0 redémarrage) ne contiennent
> **aucune** trace de suppression, ni ligne d'audit `delete`, ni message
> `remove vcluster`. Je ne peux donc ni confirmer ni infirmer que ces suppressions sont
> passées par l'app. Ce que l'absence de collecteur garantit, c'est que la question
> restera sans réponse à chaque redéploiement.

**Verdict T2 : PASS avec réserves.** L'audit trace, l'acteur est réel et varie, une
écriture refusée n'écrit rien. Trois écritures manquent (D4), l'identité est un nom
d'affichage (D3), et le journal n'est pas durable (D5).

---

## Cas T3 — CSRF sur toutes les routes d'écriture

**Ce qu'on cherche.** Qu'un POST/DELETE **sans** `X-CSRF-Token` soit refusé sur les
19 routes d'écriture, pas sur un échantillon. Et surtout : que le refus vienne bien du
contrôle CSRF, pas d'un 404, d'un 401 ou d'une mauvaise méthode.

Inventaire (`cmd/server/main.go`) : 37 routes sur le mux protégé + 7 sur le mux par
défaut (`/metrics`, `/static/`, `/auth/sso`, `/auth/callback`, `/auth/login`,
`/auth/local/login`, `/auth/logout`) = 44. Dont **19 en écriture**, toutes sur le mux
protégé.

L'ordre des intergiciels compte pour lire les résultats :
`metrics → rateLimiter → auth → CSRF → mux`. L'authentification passe **avant** le
CSRF : sans session, on obtient une redirection, jamais un refus CSRF. Les trois volées
ci-dessous portent donc toutes une session valide.

### T3.1 — cookie CSRF présent, en-tête absent

```bash
# pour chacune des 19 routes, avec Cookie: session_token=… ; csrf_token=… et pas d'en-tête
curl -s -o body -w '%{http_code}' -X POST -H "Cookie: session_token=…; csrf_token=…" $B<route>
```

**Attendu** : `403` **et** corps `Token CSRF invalide`. Le corps fait partie de
l'attendu : c'est lui qui distingue ce refus d'un autre.

> **MESURÉ à 16:14Z — 19/19.** Les dix-neuf routes répondent `403 Token CSRF invalide`,
> y compris le `DELETE` de sauvegarde Velero. Aucune exception.

### T3.2 — aucun cookie CSRF du tout

**Attendu** : `403` et corps `CSRF token manquant` — l'autre branche du middleware.

> **MESURÉ — 19/19**, message `CSRF token manquant` partout.

### T3.3 — le témoin, sans lequel T3.1 ne prouve rien

Même volée, session **lecteur**, avec un `X-CSRF-Token` égal au cookie.

**Attendu** : plus aucun message CSRF. Chaque route doit répondre autre chose, ce qui
prouve à la fois qu'elle existe, qu'elle est bien routée, et que le refus de T3.1
venait du contrôle testé.

> **MESURÉ — 18 routes sur 19** répondent `403 Accès refusé : droits administrateur
> requis`. La dix-neuvième, `POST /vclusters/{name}/delete`, répond `200 Le nom ne
> correspond pas` : le handler vérifie `confirm_name` **avant** d'appeler le service,
> donc avant le contrôle RBAC. Le CSRF a bien été franchi dans les deux cas, le témoin
> est valide.
>
> **Observation mineure (pas un trou RBAC)** : `service.Delete` commence par
> `if !actor.IsAdmin { return ErrForbidden }` (`internal/service/vcluster.go:558`), donc
> un lecteur qui renseignerait le bon `confirm_name` serait arrêté là. L'ordre des deux
> contrôles fait juste qu'un lecteur reçoit « Le nom ne correspond pas » au lieu de
> « Accès refusé », et un `200` au lieu d'un `403`.

### T3.4 — deux variantes qui décrivent la limite du double-submit

```bash
# jeton choisi par l'appelant : cookie et en-tête portent la même valeur arbitraire
-H "Cookie: session_token=…; csrf_token=AAAAAAAAAAAA" -H "X-CSRF-Token: AAAAAAAAAAAA"
# jeton passé par le champ de formulaire au lieu de l'en-tête
-d "_csrf=<valeur du cookie>"
```

> **MESURÉ** : les deux passent le CSRF (elles atteignent le contrôle RBAC et rendent
> `403 Accès refusé`). C'est le comportement attendu d'un double-submit : il ne vérifie
> pas la provenance du jeton, seulement que le porteur peut lire **et** écrire le
> cookie. Ce qui tient la protection, ce sont les attributs des cookies :
> `csrf_token` en `SameSite=Strict`, `session_token` en `SameSite=Lax` — une requête
> POST venue d'un autre site n'emporte ni l'un ni l'autre, et s'arrête sur
> l'authentification. À garder en tête si `SameSite` est un jour desserré : c'est lui
> qui porte la garantie, pas la comparaison de jetons.
>
> Note pour l'API REST à venir sous `/api/v1` : quand l'en-tête est absent, le
> middleware appelle `r.ParseForm()`, qui ne lit pas un corps `application/json`. Un
> client JSON devra passer par `X-CSRF-Token`, le champ `_csrf` ne lui servira à rien.

**Verdict T3 : PASS.** 19/19 en refus, témoin valide sur les 19.

---

## Cas T4 — les deux chemins d'authentification

### T4.1 — une écriture est-elle atteignable sans session ?

```bash
for r in les 19 routes; do curl -s -o /dev/null -w '%{http_code}' -X POST $B$r; done
# et les méthodes non prévues sur les routes hors mux
curl -X POST $B/auth/sso ; curl -X POST $B/auth/callback ; curl -X POST $B/static/app.css
```

**Attendu** : `307` vers `/auth/login` partout, jamais un `200` ni un `403` CSRF — un
`403` CSRF signifierait que la requête a dépassé l'authentification.

> **MESURÉ** : `307` sur les 19 routes d'écriture sans cookie. `POST /auth/sso`,
> `POST /auth/callback`, `POST /static/app.css` et `DELETE /auth/logout` : `307`
> également — ces motifs sont enregistrés en `GET`, une autre méthode retombe sur `/`
> et donc sur l'intergiciel d'authentification. **Aucune écriture anonyme.**

### T4.2 — le callback OIDC

```bash
curl "$B/auth/callback?code=zzz&state=zzz"                       # sans cookie oauth_state
curl -b jar "$B/auth/callback?code=zzz&state=je-nai-rien-a-voir" # avec un state incohérent
```

**Attendu** : `400 Invalid state` dans les deux cas.

> **MESURÉ** : `400 Invalid state` dans les deux cas. Le `state` est bien lié au
> navigateur qui a lancé le flow.

### T4.3 — le login local

**D6 — `POST /auth/local/login` est hors CSRF et hors limiteur de débit.** Les routes
`/auth/*` sont enregistrées sur le mux par défaut (`http.HandleFunc`), pas sur le mux
protégé ; un motif plus spécifique que `/` gagne, elles ne traversent donc ni
`rateLimiter.Middleware` ni `CSRFMiddleware`.

```bash
curl -s -o /dev/null -w '%{http_code} %{redirect_url}\n' -X POST \
  -d "username=admin" -d "password=volontairement-faux" $B/auth/local/login
```

> **MESURÉ** : `303` vers `/auth/login?error=Identifiants+invalides`, sans aucun cookie
> ni en-tête CSRF. Les identifiants sont donc évalués sur une requête qu'aucun contrôle
> n'a filtrée. Conséquences : le mot de passe admin local se teste sans plafond de
> débit (`subtle.ConstantTimeCompare` protège du timing, pas du volume), et un site
> tiers peut provoquer une connexion sous un compte qu'il contrôle — le login CSRF.
> Non testé en volume, par la règle « pas de saturation » de T6.

### T4.4 — les cookies de session ne sont pas durcis pareil des deux côtés

```bash
curl -s -D- -o /dev/null -X POST -d "username=admin" --data-urlencode "password=$PW" \
  $B/auth/local/login | grep -i '^set-cookie'
grep -i '^set-cookie: session' <en-têtes du callback OIDC>
```

> **MESURÉ** (valeurs caviardées) :
>
> ```
> login local : session_token=<REDACTED>; Path=/; Max-Age=28800; HttpOnly; SameSite=Lax
> callback OIDC : session_token=<REDACTED>; Path=/; Max-Age=28800; HttpOnly; Secure; SameSite=Lax
> ```
>
> **D7 — le cookie de session du login local n'a pas l'attribut `Secure`.**
> `internal/auth/local.go:82` écrit `Secure: r.TLS != nil` : derrière un ingress qui
> termine TLS, `r.TLS` est nil, donc l'attribut saute. Le callback OIDC, lui, force
> `Secure: true` (`internal/auth/oidc.go:138`). Deux chemins vers la même session, deux
> niveaux de durcissement. Le HSTS présent sur le domaine
> (`max-age=31536000; includeSubDomains`) limite la portée, mais il ne couvre pas un
> navigateur qui n'a pas encore visité le site en HTTPS.
>
> Le cookie CSRF, pour mémoire : `csrf_token=…; Path=/; Max-Age=86400; SameSite=Strict`,
> sans `HttpOnly` — c'est voulu, HTMX doit le lire — et sans `Secure` non plus.

**Verdict T4 : PASS** sur la question posée (aucune écriture sans authentification),
**avec deux défauts** : D6 (login hors CSRF et hors limiteur) et D7 (cookie sans
`Secure` sur le chemin local).

---

## Cas T5 — les métriques Prometheus

### T5.1 — le terrain a bougé sous la recette, à lire avant les résultats

**D8 — un correctif a atterri sur `/metrics` pendant que je le mesurais.** Chronologie :

| Heure | Fait |
|---|---|
| 16:10:27Z | `GET /metrics` par l'ingress public → **200**, 24 665 octets |
| 16:15:25Z | création de l'Ingress `vcluster-manager-metrics-deny` (`creationTimestamp`) |
| 16:16:32Z | `GET /metrics` par l'ingress public → **403**, corps HTML nginx |

Ce n'est pas une rustine anonyme : l'objet vient d'un chantier parallèle, il est écrit
dans `deploy/base/ingress.yaml` (non commité au moment où j'écris) avec son
raisonnement, et il a été appliqué à la main sur le cluster avant que Flux ne le
reprenne. Il porte `whitelist-source-range: 127.0.0.1/32` sur `path: /metrics,
pathType: Exact`, ce qui l'emporte sur le `/` en Prefix de l'Ingress principal.

Deux conséquences pour ce document. La première : mon `200` de 16:10:27Z **confirme
indépendamment** ce que ce chantier a trouvé — `/metrics` était bien servi
publiquement, sans authentification, depuis Internet. La seconde tient à la méthode :
jouer T5 après 16:15:25Z et conclure « `/metrics` n'est pas exposé » aurait mesuré le
correctif de quelqu'un d'autre, posé trois minutes plus tôt, et pas le comportement de
l'application. Sans horodatage des deux appels, la conclusion aurait été fausse sans
que rien ne le signale. **L'objet a été laissé en place** et la mesure du contenu a été
refaite hors ingress, par port-forward.

### T5.2 — contenu de `/metrics`

```bash
kubectl -n vcluster-manager port-forward svc/vcluster-manager 18080:http &
curl -s -o metrics.txt -w 'code=%{http_code} taille=%{size_download}\n' http://127.0.0.1:18080/metrics
grep -inE "token|secret|password|kubeconfig|bearer|api_key|X-Amz|Signature|https?://" metrics.txt
grep -iE "demo|recette-" metrics.txt
```

**Attendu** : `200`, les familles `vcluster_manager_*` présentes, aucun secret, aucune
URL signée. Les noms de vclusters seraient acceptables ; leur absence est un bonus.

> **MESURÉ à 16:17:29Z** : `200`, 58 881 octets, 44 familles. Les cinq familles métier
> répondent et ont du sens :
>
> ```
> vcluster_manager_actions_total{action="create",env="preprod"} 3
> vcluster_manager_actions_total{action="enable-protection",env="preprod"} 1
> vcluster_manager_actions_total{action="disable-protection",env="preprod"} 1
> vcluster_manager_active_deletions 0
> vcluster_manager_gitlab_api_errors_total{operation="list-files"} 48
> vcluster_manager_gitlab_api_errors_total{operation="get-file"} 20
> ```
>
> Les compteurs correspondent aux actions réellement faites, y compris les deux miennes.
> `vcluster_manager_http_requests_total` porte 39 séries, étiquetées par **motif de
> route** (`POST /api/vclusters/{name}/velero/restore`) et non par URL concrète : pas
> d'explosion de cardinalité, et aucun nom de vcluster.
>
> **Aucune fuite.** La recherche de chaînes sensibles ne remonte que le mot
> `kubeconfig` comme composant du motif de route
> `GET /api/vclusters/{name}/kubeconfig` — un nom de route, pas un contenu. Aucun nom
> de vcluster (`demo`, `recette-*`, `zz-audit`) nulle part. La seule information
> d'environnement est `go_info{version="go1.25.12"}`, standard pour un exporteur Go.

**D9 — rien ne consomme ces métriques ici.** `kubectl get deploy,sts -A | grep -i
prometheus` ne rend rien. L'endpoint est correct et personne ne le lit : les métriques
ne sont donc pas non plus un filet de rattrapage pour les notifications manquantes
de T1.

**Verdict T5 : PASS** sur le contenu et l'absence de fuite. La mesure de l'exposition
publique est **datée** : `200` à 16:10:27Z par l'ingress, `403` ensuite à cause de D8.

---

## Cas T6 — le rate limiting

> **Fenêtre : 2026-08-08 de 16:22:37Z à 16:23:52Z.** Toute erreur `429` observée par les
> autres recettes hors de cet intervalle ne vient pas d'ici. Dans l'intervalle, une
> seule volée a touché l'IP publique partagée : T6.4, entre 16:23:30.76Z et
> 16:23:31.20Z, cinq refus.

Trois des quatre volées passent par un port-forward. La raison est mesurable :
`clientIP()` prend `X-Forwarded-For` s'il existe, sinon `RemoteAddr` ; par port-forward
il n'y a pas de proxy, donc le seau est `127.0.0.1`, distinct de celui des clients qui
arrivent par l'ingress. Saturer ce seau-là ne bride personne.

### T6.1 — le limiteur dédié à `/metrics` (annoncé 5 r/s, burst 10)

```bash
urls=$(for i in $(seq 25); do echo -n "http://127.0.0.1:18080/metrics "; done)
curl -s -o /dev/null -w '%{http_code}\n' $urls | sort | uniq -c
```

> **MESURÉ à 16:22:37Z** : `14 × 200`, `11 × 429` sur 25. Cohérent avec un seau de 10
> plus le réapprovisionnement à 5/s pendant la volée. Le limiteur dédié existe et bride.

### T6.2 — le limiteur principal (annoncé 20 r/s, burst 50)

```bash
seq 100 | xargs -P 25 -I{} curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18080/ | sort | uniq -c
```

> **MESURÉ à 16:22:51Z**, 100 requêtes en 507 ms : `57 × 307`, `43 × 429`. Burst 50 plus
> une dizaine de jetons réapprovisionnés : conforme aux 20 r/s burst 50 annoncés.

### T6.3 — sur quelle clé le seau est-il indexé ?

Deux volées identiques, l'une avec un `X-Forwarded-For` différent à chaque requête,
l'autre avec un `X-Forwarded-For` constant.

> **MESURÉ à 16:23:01Z** :
>
> | Volée | Résultat |
> |---|---|
> | 100 requêtes, XFF tournant (`198.51.100.{1..100}`) | `100 × 307`, **0 × 429** |
> | 100 requêtes, XFF constant (`203.0.113.77`) | `57 × 307`, `43 × 429` |
>
> **D10 — `clientIP()` fait confiance à `X-Forwarded-For` sans condition.**
> `internal/auth/ratelimit.go:62` prend le premier élément de l'en-tête, quel qu'il
> soit. Un appelant qui fait tourner l'en-tête obtient un seau neuf à chaque requête et
> n'est plus bridé du tout. Le même code sert les deux limiteurs, celui de `/metrics`
> compris.

### T6.4 — le défaut est-il atteignable de l'extérieur ?

Volée discriminante par l'ingress public, calibrée à 60 requêtes — c'est-à-dire
juste au-dessus du burst de 50, pour que les deux issues se distinguent sans saturer :

- si nginx conserve l'en-tête envoyé → 60 seaux distincts → aucun `429` ;
- si nginx l'écrase par l'IP réelle → 60 > 50 → quelques `429`.

```bash
seq 60 | xargs -P 20 -I{} curl -s -o /dev/null -w '%{http_code}\n' \
  -H "X-Forwarded-For: 198.51.100.{}" https://vcluster-manager.rebuild-it.fr/ | sort | uniq -c
```

> **MESURÉ entre 16:23:30.76Z et 16:23:31.20Z** : `55 × 307`, `5 × 429`. C'est la
> seconde issue. L'en-tête forgé est ignoré, le seau reste celui de l'IP réelle, et
> l'écart 60 − 50 = 10 encadre correctement les 5 refus observés (le réapprovisionnement
> à 20/s en rend quelques-uns pendant les 440 ms).
>
> Confirmé par la configuration du contrôleur en service :
> `kubectl -n ingress-nginx exec ds/ingress-nginx-controller -- grep X-Forwarded-For
> /etc/nginx/nginx.conf` rend `proxy_set_header X-Forwarded-For $remote_addr;` — nginx
> **remplace** l'en-tête au lieu de l'ajouter (`use-forwarded-headers` n'est pas activé,
> le ConfigMap ne contient que `allow-snippet-annotations: "false"`).
>
> **D10 n'est donc pas exploitable dans ce déploiement.** Il le redevient dès que l'app
> est exposée sans ce proxy, ou derrière un proxy qui utilise
> `$proxy_add_x_forwarded_for` — la valeur forgée serait alors en tête de liste, et
> c'est celle que le code lit. La protection tient à une ligne de configuration qui ne
> vit pas dans ce dépôt.

Après la fenêtre : `GET /` par l'ingress répond `307` à 16:23:52Z, l'app est saine
(0 redémarrage), et les deux autres recettes continuent d'écrire leurs lignes d'audit
(`velero-backup-manual` à 16:17:54Z, `velero-restore` à 16:21:19Z).

**Verdict T6 : PASS.** Les deux limiteurs brident aux valeurs annoncées. D10 est un
défaut de conception réel, neutralisé par l'ingress en place, prouvé des deux côtés.

---

## Défauts remontés

| # | Cas | Défaut | Preuve |
|---|---|---|---|
| D1 | T1 | Aucune notification pour une suppression **prod**, ni début ni fin | `internal/service/vcluster.go:698` dans le bloc `if deletePreprod` ; aucun appel dans le bloc prod (lu, non exécuté) |
| D2 | T1 | La notification de confirmation dépend d'un onglet HTMX ouvert | `internal/handlers/api.go:79`, dans `StatusFragment` (lu, non exécuté) |
| D3 | T2 | L'acteur audité est un nom d'affichage, pas un identifiant stable | lignes `user='admin'` et `user='Test Admin'` mesurées à 16:16:07Z |
| D4 | T2 | `POST /config/{env}`, `create-prod-mr` et `vault-setup-retry` n'écrivent aucune ligne d'audit | `grep -c "internal/audit" internal/handlers/cluster_config.go` → 0 |
| D5 | T2 | Aucun collecteur de logs : l'audit disparaît avec le pod | aucun fluentd/vector/loki/promtail ; logs du RS précédent perdus |
| D6 | T4 | `POST /auth/local/login` échappe au CSRF et au limiteur de débit | `303` sans cookie ni en-tête CSRF ; routes `/auth/*` sur le mux par défaut |
| D7 | T4 | Le cookie `session_token` du login local n'a pas `Secure` | `Set-Cookie` comparés entre login local et callback OIDC |
| D8 | T5 | `/metrics` était servi publiquement sans authentification ; correctif d'un chantier parallèle appliqué à 16:15:25Z, en pleine mesure | `200`, 24 665 octets à 16:10:27Z depuis Internet, puis `403` à 16:16:32Z |
| D9 | T5 | Aucun Prometheus ne scrape `/metrics` sur cette plateforme | `kubectl get deploy,sts -A \| grep -i prometheus` vide |
| D10 | T6 | `clientIP()` fait confiance à `X-Forwarded-For` sans condition | XFF tournant → `0 × 429` sur 100 requêtes (port-forward) |

Aucun de ces défauts n'a été corrigé ici : ce document ne touche à aucun fichier de code.

## État dans lequel le cluster est laissé

- Namespaces `zz-recette-transverses` (receveur webhook) et `vcluster-zz-audit`
  **supprimés**, vérifié.
- Aucun vcluster créé, modifié ni supprimé. `demo`, `recette-cdv-1`,
  `recette-restore-a` et `recette-restore-b` intacts.
- Pod `vcluster-manager` jamais redémarré (0 restart, démarré 11:02:33Z).
- Deux lignes d'audit ajoutées sur `zz-audit` (enable puis disable-protection), sur un
  namespace qui n'existe plus.
- **Laissé en place, pas à moi** : l'Ingress `vcluster-manager-metrics-deny` (D8), qui
  appartient à un chantier parallèle.
- Trois sessions ouvertes côté Keycloak (`testreader`, `testadmin`) et une session
  locale, valables 8 h. Les cookie jars sont dans un répertoire temporaire, hors dépôt.

## Ce que ce plan ne couvre pas

- **Le déclenchement réel d'une notification webhook** (T1.3) : aucun webhook n'est
  configuré et le configurer demande un redémarrage de l'app partagée. Procédure prête,
  non jouée, raison donnée. C'est le trou principal de ce passage.
- **D1 et D2 ne sont que lus.** Ils décrivent le code, pas une observation. T1.3 est ce
  qui les confirmerait ou les démentirait.
- **Les trois écritures sans audit (D4) ne sont pas exécutées.** Déclencher
  `POST /config/{env}` changerait le kubeconfig de l'app pour tout le monde, et
  `create-prod-mr` ouvrirait une vraie MR vers `master`. Le constat est statique.
- **Le login local en volume** : ni brute force ni mesure du plafond réel sur
  `/auth/local/login`. D6 est établi par la lecture du routage et par une seule requête
  acceptée sans CSRF, pas par une campagne.
- **Le CSRF sur un vrai navigateur** : tout est passé en `curl`. Le comportement de
  HTMX quand il reçoit un `403 Token CSRF invalide` (le texte brut est-il injecté dans
  le fragment ?) n'est pas vérifié.
- **La rotation et l'expiration des sessions** : les jetons durent 8 h, aucun cas ne
  vérifie ce qui se passe à l'échéance ni au `logout`.
- **L'audit d'une suppression, d'un appairage Rancher et d'une restauration Velero**
  avec un acteur nommé : seule la protection de namespace a été jouée. Les autres
  actions appartiennent aux deux recettes parallèles ; leurs lignes existent bien dans
  les logs (`velero-backup-manual`, `velero-restore` à 16:17–16:21Z) mais toutes en
  `admin`, donc elles ne testent pas la variation d'acteur.
- **La prod.** Rien ici n'y a été joué et rien ne s'y transpose tel quel.

## Annexes

### Ouverture d'une session OIDC en ligne de commande

```bash
B=https://vcluster-manager.rebuild-it.fr
user=$1; J=$2; rm -f "$J"
loc=$(curl -s -c "$J" -o /dev/null -D- "$B/auth/sso" \
      | grep -i '^location:' | tr -d '\r' | sed 's/^[Ll]ocation: //')
curl -s -c "$J" -b "$J" -o kc.html "$loc"
action=$(grep -o 'action="[^"]*"' kc.html | head -1 | sed 's/action="//; s/"$//; s/&amp;/\&/g')
cb=$(curl -s -c "$J" -b "$J" -o /dev/null -D- -X POST \
      --data-urlencode "username=$user" --data-urlencode "password=$TPW" \
      --data "credentialId=" "$action" \
     | grep -i '^location:' | tr -d '\r' | sed 's/^[Ll]ocation: //')
curl -s -c "$J" -b "$J" -o /dev/null "$cb"   # pose session_token
curl -s -c "$J" -b "$J" -o /dev/null "$B/"   # pose csrf_token
chmod 600 "$J"
```

Le mot de passe vient de `terraform.tfvars`, variable `test_user_password`. Il ne doit
apparaître ni dans un rapport ni dans un fichier du dépôt.

### Receveur webhook jetable

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: webhook-receiver
  namespace: zz-recette-transverses
  labels: {app: webhook-receiver}
spec:
  containers:
  - name: receiver
    image: python:3.12-alpine
    command: ["python","-u","-c"]
    args:
    - |
      from http.server import BaseHTTPRequestHandler, HTTPServer
      class H(BaseHTTPRequestHandler):
          def do_POST(self):
              n = int(self.headers.get('Content-Length', 0))
              print("RECU", self.path, self.rfile.read(n).decode('utf-8','replace'), flush=True)
              self.send_response(200); self.end_headers(); self.wfile.write(b'ok')
          def log_message(self, *a): pass
      HTTPServer(('', 8080), H).serve_forever()
    ports: [{containerPort: 8080}]
---
apiVersion: v1
kind: Service
metadata: {name: webhook-receiver, namespace: zz-recette-transverses}
spec:
  selector: {app: webhook-receiver}
  ports: [{port: 80, targetPort: 8080}]
```
