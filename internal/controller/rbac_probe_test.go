package controller

// Le harnais qui fait tourner du vrai code d'opérateur derrière le VRAI
// ClusterRole commité — pour LES DEUX binaires opérateur (cmd/operator et
// cmd/veleroops-operator), chacun avec le sien. rbac_operator_test.go porte
// les scénarios du premier, rbac_veleroops_test.go ceux du second — y compris
// la preuve croisée que ni l'un ni l'autre ne peut faire le travail de
// l'autre, qui est le vrai livrable de sécurité de leur séparation en deux
// pods.
//
// Pourquoi il existe : envtest démarre un kube-apiserver avec
// `--authorization-mode=RBAC`, mais le client qu'il rend appartient au groupe
// `system:masters`, qui court-circuite l'autorisation. Les 41 fichiers de test
// du dépôt tournent donc avec un client tout-puissant — aucun ne peut voir un
// droit manquant. C'est ce trou qui a laissé partir en production un ClusterRole
// sans `list resourcequotas`, dont dépend l'étape de budget : plus AUCUN
// VCluster n'était réconcilié, et tout était vert.
//
// Ce que ce fichier change : les appels passent par un reverse proxy local qui
// les réémet vers l'apiserver d'envtest en IMPERSONANT le ServiceAccount de
// l'opérateur. À partir de là, l'apiserver applique le ClusterRole du dépôt pour
// de vrai, et le proxy voit le code HTTP de chaque réponse — y compris les 403
// que le code appelant avale (HostNamespaceState rend `known=false`,
// CountVClusterPods rend `known=false` : un refus y devient un silence).
//
// La source de « ce qui est attendu » n'est donc pas une liste de verbes tenue à
// la main, mais le code lui-même : ce qui n'est pas accordé et qui est appelé
// fait tomber le test.
//
// CE QUE ÇA NE COUVRE PAS. Un dispositif dynamique ne voit que ce qu'on exécute,
// et le dire est la moitié de sa valeur :
//
//   - `create pods/portforward` — l'accès à l'API INTERNE d'un vcluster (job
//     rancher-cleanup, apps ArgoCD, webhook Vault, version K8s). Il faut un pod
//     qui tourne vraiment ; envtest n'a ni kubelet ni scheduler. Le `get secrets`
//     qui le précède, lui, est couvert.
//   - `create/patch events` et tout le groupe `coordination.k8s.io` (leases) —
//     émis par le manager controller-runtime, pas par un reconcile. Ces tests
//     appellent Reconcile directement, sans manager.
//   - le `watch` — les clients d'ici lisent, ils n'ouvrent pas d'informer.
//   - le Role namespacé `vcluster-manager-state` (backend ConfigMap de l'app).
//   - et surtout : l'écart entre le ClusterRole COMMITÉ et celui qui est
//     réellement DÉPLOYÉ. Ces tests lisent deploy/base/*.yaml ; un cluster où le
//     manifeste n'a pas été réappliqué reste un cluster cassé avec des tests
//     verts. C'est ce que le `kubectl auth can-i` en précondition de recette
//     couvre, et rien d'autre ne le couvre.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gmalfray/vcluster-manager/api/v1alpha1"
	"github.com/gmalfray/vcluster-manager/internal/config"
	"github.com/gmalfray/vcluster-manager/internal/kubernetes"
	"github.com/gmalfray/vcluster-manager/internal/models"
	"github.com/gmalfray/vcluster-manager/internal/service"
)

// operatorServiceAccount est le sujet impersonné : exactement celui que
// deploy/base/operator-rbac.yaml déclare, et que le Deployment cmd/operator
// utilise en production.
const operatorServiceAccount = "system:serviceaccount:vcluster-manager:vcluster-manager-operator"

// veleroopsServiceAccount est le sujet du second binaire, cmd/veleroops-operator
// — un pod distinct, un ClusterRole distinct
// (deploy/base/veleroops-operator-rbac.yaml). Les deux tournaient dans le même
// manager avant ce découpage ; le prouver par impersonation, et pas seulement
// par lecture des deux fichiers, est le seul moyen de garantir qu'aucun des
// deux ne peut plus faire ce que l'autre fait.
const veleroopsServiceAccount = "system:serviceaccount:vcluster-manager:vcluster-manager-veleroops-operator"

// appServiceAccount est celui de cmd/server. Les trois binaires n'ont ni les
// mêmes droits ni les mêmes chemins de code, et c'est précisément en dérivant
// l'un de l'autre qu'un verbe s'est perdu.
const appServiceAccount = "system:serviceaccount:vcluster-manager:vcluster-manager"

// Les fichiers commités, pas des copies. C'est le point : le test lit les
// manifestes qui partent en production.
var (
	operatorRBACFile  = filepath.Join("..", "..", "deploy", "base", "operator-rbac.yaml")
	veleroopsRBACFile = filepath.Join("..", "..", "deploy", "base", "veleroops-operator-rbac.yaml")
	appRBACFile       = filepath.Join("..", "..", "deploy", "base", "rbac.yaml")
)

// --- ce que le proxy voit --------------------------------------------------

// apiCall est une requête telle que l'apiserver l'a comprise : le verbe RBAC, la
// ressource, et le code que la réponse a rendu.
type apiCall struct {
	Verb        string
	Group       string
	Resource    string
	Subresource string
	Namespace   string
	Name        string
	Method      string
	Path        string
	Status      int
	watch       bool
}

func (c apiCall) String() string {
	res := c.Resource
	if c.Subresource != "" {
		res += "/" + c.Subresource
	}
	grp := c.Group
	if grp == "" {
		grp = "core"
	}
	return fmt.Sprintf("%s %s.%s (%s %s) → %d", c.Verb, res, grp, c.Method, c.Path, c.Status)
}

// apiRecorder rassemble les appels. Partagé entre le client controller-runtime et
// le client dynamique du service, qui passent tous deux par le même proxy.
type apiRecorder struct {
	mu    sync.Mutex
	calls []apiCall
}

func (r *apiRecorder) add(c apiCall) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, c)
}

func (r *apiRecorder) snapshot() []apiCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]apiCall, len(r.calls))
	copy(out, r.calls)
	return out
}

// forbidden rend les appels que l'apiserver a refusés faute de droit. C'est la
// seule chose que ce dispositif cherche : un 404 (la CRD Velero n'est pas
// installée dans envtest) ou un 409 sont des réponses normales ici, un 403 non.
func (r *apiRecorder) forbidden() []apiCall {
	var out []apiCall
	for _, c := range r.snapshot() {
		if c.Status == http.StatusForbidden {
			out = append(out, c)
		}
	}
	return out
}

// saw dit si un appel de ce verbe sur cette ressource a réellement été émis.
// Sert au plancher de couverture : un test qui n'exerce plus rien passerait au
// vert sans protéger quoi que ce soit.
func (r *apiRecorder) saw(verb, resource string) bool {
	for _, c := range r.snapshot() {
		full := c.Resource
		if c.Subresource != "" {
			full += "/" + c.Subresource
		}
		if c.Verb == verb && full == resource {
			return true
		}
	}
	return false
}

// requireNoForbidden est l'assertion centrale. Le message nomme l'appel refusé
// et la règle qu'il faudrait ajouter — c'est ce qui manquait le jour de
// l'incident, où le symptôme était « aucun VCluster n'est réconcilié ».
func requireNoForbidden(t *testing.T, rec *apiRecorder, rbacFile string) {
	t.Helper()
	refus := rec.forbidden()
	if len(refus) == 0 {
		return
	}
	var lignes []string
	seen := map[string]bool{}
	for _, c := range refus {
		res := c.Resource
		if c.Subresource != "" {
			res += "/" + c.Subresource
		}
		grp := c.Group
		if grp == "" {
			grp = `""`
		}
		regle := fmt.Sprintf("- apiGroups: [%s] resources: [%q] verbs: [… %q]", grp, res, c.Verb)
		if seen[regle] {
			continue
		}
		seen[regle] = true
		lignes = append(lignes, fmt.Sprintf("  %s\n    à ajouter dans %s : %s", c, rbacFile, regle))
	}
	sort.Strings(lignes)
	t.Fatalf("l'apiserver a refusé %d appel(s) que ce code émet, avec le ClusterRole de %s :\n%s\n\n"+
		"En production, un refus ne se voit pas forcément comme une erreur : plusieurs de ces appels "+
		"avalent leur échec et rendent « je ne sais pas ». Le symptôme observé le 2026-08-08 était "+
		"« aucun VCluster n'est réconcilié », pas un message parlant de droits.",
		len(refus), rbacFile, strings.Join(lignes, "\n"))
}

// --- le proxy impersonné ---------------------------------------------------

type recordingTransport struct {
	next http.RoundTripper
	rec  *apiRecorder
}

func (t *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	c := parseAPIPath(req.URL.Path, req.URL.Query().Get("watch") == "true")
	c.Method = req.Method
	c.Path = req.URL.Path
	c.Verb = rbacVerb(req.Method, c)
	if resp != nil {
		c.Status = resp.StatusCode
	}
	t.rec.add(c)
	return resp, err
}

// operatorAPIProxy monte un apiserver « vu par l'opérateur » : même serveur,
// mais chaque requête est réémise sous l'identité du ServiceAccount, donc
// soumise au ClusterRole.
//
// Un proxy plutôt qu'un simple rest.Config impersonné, parce que
// kubernetes.NewStatusClient ne prend qu'un CHEMIN de kubeconfig : c'est le seul
// moyen de faire passer le client du service — celui qui émet les appels Velero,
// Flux, quotas — par la même identité et le même enregistreur. Le kubeconfig
// écrit pointe donc en clair sur ce proxy, qui porte seul de quoi s'authentifier.
func operatorAPIProxy(t *testing.T) (proxyURL string, rec *apiRecorder) {
	t.Helper()
	return apiProxyAs(t, operatorServiceAccount)
}

// veleroopsAPIProxy est le même montage pour cmd/veleroops-operator.
func veleroopsAPIProxy(t *testing.T) (proxyURL string, rec *apiRecorder) {
	t.Helper()
	return apiProxyAs(t, veleroopsServiceAccount)
}

// apiProxyAs est le même montage pour n'importe quel sujet — l'opérateur ou
// l'app.
func apiProxyAs(t *testing.T, subject string) (proxyURL string, rec *apiRecorder) {
	t.Helper()

	target, err := url.Parse(restCfg.Host)
	if err != nil {
		t.Fatalf("URL de l'apiserver envtest (%q) illisible : %v", restCfg.Host, err)
	}

	impersonated := rest.CopyConfig(restCfg)
	impersonated.Impersonate = rest.ImpersonationConfig{UserName: subject}
	rt, err := rest.TransportFor(impersonated)
	if err != nil {
		t.Fatalf("transport impersonné : %v", err)
	}

	rec = &apiRecorder{}
	proxy := &httputil.ReverseProxy{
		Rewrite:   func(pr *httputil.ProxyRequest) { pr.SetURL(target) },
		Transport: &recordingTransport{next: rt, rec: rec},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, "proxy: "+err.Error(), http.StatusBadGateway)
		},
		// Sans ça, le log par défaut de ReverseProxy part sur la sortie standard
		// du test à chaque connexion coupée en fin de suite.
		ErrorLog: nil,
	}
	srv := httptest.NewServer(proxy)
	t.Cleanup(srv.Close)
	return srv.URL, rec
}

// operatorKubeconfig écrit un kubeconfig qui vise le proxy, sans identifiants :
// c'est le proxy qui présente le certificat et pose l'en-tête d'impersonation.
func operatorKubeconfig(t *testing.T, proxyURL string) string {
	t.Helper()
	cfg := clientcmdapi.Config{
		Clusters:       map[string]*clientcmdapi.Cluster{"envtest": {Server: proxyURL}},
		Contexts:       map[string]*clientcmdapi.Context{"envtest": {Cluster: "envtest", AuthInfo: "operator"}},
		AuthInfos:      map[string]*clientcmdapi.AuthInfo{"operator": {}},
		CurrentContext: "envtest",
	}
	raw, err := clientcmd.Write(cfg)
	if err != nil {
		t.Fatalf("sérialisation du kubeconfig : %v", err)
	}
	path := filepath.Join(t.TempDir(), "operator.kubeconfig")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("écriture du kubeconfig : %v", err)
	}
	return path
}

// --- application du ClusterRole commité ------------------------------------

// applyOperatorRBAC pose dans l'apiserver ce que deploy/base/operator-rbac.yaml
// contient — ClusterRole, ServiceAccount, ClusterRoleBinding — sans rien
// réécrire au passage.
//
// La liaison est appliquée telle quelle, sujets compris : un ClusterRoleBinding
// qui viserait le mauvais namespace ou le mauvais nom de SA laisserait
// l'opérateur sans aucun droit, et c'est un mode de panne aussi réel qu'un verbe
// oublié.
func applyOperatorRBAC(t *testing.T, ctx context.Context, admin ctrlclient.Client) {
	t.Helper()
	// Trois documents attendus : ClusterRole, ServiceAccount, ClusterRoleBinding.
	// S'il en manque un — la liaison, typiquement — tout le reste de ce fichier
	// mesurerait un opérateur sans droits, et échouerait pour la mauvaise raison.
	applyRBACFile(t, ctx, admin, operatorRBACFile, 3)
}

// applyVeleroOpsRBAC fait la même chose pour le second binaire,
// deploy/base/veleroops-operator-rbac.yaml. Les deux ClusterRoles sont posés
// indépendamment — un test qui veut prouver l'un ne peut pas faire le travail
// de l'autre pose les DEUX (voir TestOperatorRBACCannotDoVeleroOpsWork et
// TestVeleroOpsRBACCannotDoOperatorWork) : sans le second, un refus
// n'établirait rien, faute d'un ClusterRole en face qui aurait pu accorder le
// droit.
func applyVeleroOpsRBAC(t *testing.T, ctx context.Context, admin ctrlclient.Client) {
	t.Helper()
	applyRBACFile(t, ctx, admin, veleroopsRBACFile, 3)
}

// applyAppRBAC fait la même chose pour le ClusterRole de l'app (cmd/server).
// Cinq documents : ClusterRole, ClusterRoleBinding, Role, RoleBinding — et pas
// de ServiceAccount, il vient d'ailleurs dans le kustomize.
func applyAppRBAC(t *testing.T, ctx context.Context, admin ctrlclient.Client) {
	t.Helper()
	applyRBACFile(t, ctx, admin, appRBACFile, 4)
}

func applyRBACFile(t *testing.T, ctx context.Context, admin ctrlclient.Client, path string, minObjets int) {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("lecture de %s : %v", path, err)
	}

	// Le namespace des ServiceAccounts et des Role doit exister avant eux.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "vcluster-manager"}}
	if err := admin.Create(ctx, ns); err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("création du namespace vcluster-manager : %v", err)
	}

	dec := k8syaml.NewYAMLOrJSONDecoder(strings.NewReader(string(raw)), 4096)
	applied := 0
	for {
		var obj unstructured.Unstructured
		err := dec.Decode(&obj)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("décodage de %s : %v", path, err)
		}
		if len(obj.Object) == 0 {
			continue
		}
		if err := admin.Apply(ctx, ctrlclient.ApplyConfigurationFromUnstructured(&obj),
			ctrlclient.FieldOwner("rbac-probe"), ctrlclient.ForceOwnership); err != nil {
			t.Fatalf("application de %s/%s depuis %s : %v", obj.GetKind(), obj.GetName(), path, err)
		}
		applied++
	}
	if applied < minObjets {
		t.Fatalf("%s n'a produit que %d objets, attendu au moins %d — un document perdu "+
			"(la liaison, typiquement) laisserait le sujet sans aucun droit et ferait échouer "+
			"les tests pour la mauvaise raison", path, applied, minObjets)
	}
}

// adminClient est un client system:masters, pour poser les fixtures. Il ne sert
// JAMAIS à exercer du code d'opérateur — c'est tout l'objet du fichier.
func adminClient(t *testing.T) ctrlclient.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme corev1 : %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme v1alpha1 : %v", err)
	}
	// SubjectAccessReview : ce client sert aussi à demander à l'apiserver ce
	// qu'il REFUSERAIT, sans l'essayer.
	if err := authzv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme authorization : %v", err)
	}
	c, err := ctrlclient.New(restCfg, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("client admin : %v", err)
	}
	return c
}

// operatorClient est le client controller-runtime que le reconciler utilise dans
// ces tests : même scheme qu'en production, mais soumis au ClusterRole.
func operatorClient(t *testing.T, proxyURL string) ctrlclient.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme corev1 : %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme v1alpha1 : %v", err)
	}
	c, err := ctrlclient.New(&rest.Config{Host: proxyURL}, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("client opérateur : %v", err)
	}
	return c
}

// --- le câblage de production, mais sous RBAC ------------------------------

// rbacOps est fullOps — le faux qui couvre les six seams — avec les méthodes
// qui parlent RÉELLEMENT au cluster hôte rebranchées sur le vrai service.
//
// C'est le compromis assumé de ce fichier : ce qui sort du cluster (Rancher,
// Keycloak, Vault, GitLab, Velero pendant le reconcile) reste faux, parce qu'un
// test hors ligne ne peut pas le joindre ; ce qui touche l'apiserver passe par
// le vrai code, parce que c'est précisément là que le RBAC se joue.
type rbacOps struct {
	*fullOps
	svc *service.Service
}

func (o *rbacOps) HostNamespaceState(ctx context.Context, name, env string) service.NamespaceState {
	return o.svc.HostNamespaceState(ctx, name, env)
}

func (o *rbacOps) DeleteHostNamespace(ctx context.Context, actor models.Actor, name, env string) error {
	return o.svc.DeleteHostNamespace(ctx, actor, name, env)
}

func (o *rbacOps) GetProtection(ctx context.Context, name, env string) service.ProtectionState {
	return o.svc.GetProtection(ctx, name, env)
}

func (o *rbacOps) SetProtection(ctx context.Context, actor models.Actor, name, env string, enabled bool) (service.ProtectionState, error) {
	return o.svc.SetProtection(ctx, actor, name, env, enabled)
}

// SuspendVCluster/ResumeVCluster rebranchés aussi : `spec.suspend` suspend
// Flux et scale le vcluster à zéro réplique (internal/service/
// vcluster_lifecycle.go), donc c'est ici, pas sur le chemin de suppression,
// que `patch helmreleases`/`patch kustomizations`/`patch statefulsets` sont
// exercés pour de vrai. Les laisser sur le faux aurait fait le même trou que
// celui déjà refermé sur InspectDeletionBackup/TriggerVeleroBackup : un test
// qui prétend couvrir le reconcile complet sans jamais toucher l'apiserver
// sur cette étape.
func (o *rbacOps) SuspendVCluster(ctx context.Context, actor models.Actor, name, env string) error {
	return o.svc.SuspendVCluster(ctx, actor, name, env)
}

func (o *rbacOps) ResumeVCluster(ctx context.Context, actor models.Actor, name, env string) error {
	return o.svc.ResumeVCluster(ctx, actor, name, env)
}

var _ VClusterServiceOps = (*rbacOps)(nil)

// operatorStatusClient est le client que le service utilise, monté comme
// cmd/operator/main.go le monte : kubernetes.NewStatusClient sur un chemin de
// kubeconfig. Celui-ci vise le proxy, donc l'opérateur.
func operatorStatusClient(t *testing.T, proxyURL string) *kubernetes.StatusClient {
	t.Helper()
	c, err := kubernetes.NewStatusClient(operatorKubeconfig(t, proxyURL))
	if err != nil {
		t.Fatalf("client Kubernetes de l'opérateur : %v", err)
	}
	return c
}

// operatorService reproduit le service que cmd/operator/main.go construit : la
// configuration, le générateur, et un seul client Kubernetes indexé par la cell.
// Les autres dépendances restent nil, comme en production — l'opérateur ne les
// appelle pas.
func operatorService(t *testing.T, proxyURL string) *service.Service {
	t.Helper()
	return service.New(service.Deps{
		Cfg: &config.Config{
			VeleroNamespace:  "velero-system",
			VeleroDefaultTTL: "720h0m0s",
		},
		K8sClients:   map[string]*kubernetes.StatusClient{"preprod": operatorStatusClient(t, proxyURL)},
		K8sClientsMu: &sync.RWMutex{},
	})
}

// operatorReconcilerUnderRBAC câble le reconciler comme cmd/operator/main.go,
// à une chose près : son client et celui de son service n'ont que les droits du
// ClusterRole commité.
func operatorReconcilerUnderRBAC(t *testing.T, proxyURL string) (*VClusterReconciler, *service.Service) {
	t.Helper()
	svc := operatorService(t, proxyURL)

	ops := &rbacOps{fullOps: newFullOps(), svc: svc}
	// La sauvegarde d'avant destruction est déclarée déjà terminée. Sans ça la
	// séquence de suppression s'arrête à sa deuxième étape en attendant Velero —
	// qui n'est pas installé dans envtest — et n'atteint jamais les étapes qui,
	// elles, tapent vraiment sur l'apiserver : retrait de la protection et
	// suppression du namespace. Un test qui s'arrête avant ce qu'il prétend
	// couvrir est le vert vide que requireExercised existe pour attraper.
	ops.backup = service.DeletionBackupState{Found: true, Name: "b1", Phase: "Completed", Completed: true}

	return &VClusterReconciler{
		Client:    operatorClient(t, proxyURL),
		Ops:       ops,
		BudgetOps: svc,
		Budget:    BudgetLimits{CPU: "100", Memory: "400Gi", Storage: "10Ti"},
		Cell:      "preprod",
		Namespace: DefaultVClustersNamespace,
	}, svc
}

// veleroopsReconcilerUnderRBAC câble VeleroOpsReconciler comme
// cmd/veleroops-operator/main.go le fait : le VRAI service, une Deps aussi
// maigre que celle du binaire (ni générateur, ni Keycloak, ni Rancher, ni
// Vault — operatorService en construit déjà exactement la forme), et un
// client controller-runtime qui n'a que les droits du ClusterRole visé par
// proxyURL. C'est proxyURL, pas le nom de cette fonction, qui décide de
// l'identité : appelée avec un proxy monté sur veleroopsServiceAccount, elle
// donne le reconciler tel qu'il tourne vraiment en production.
func veleroopsReconcilerUnderRBAC(t *testing.T, proxyURL string) *VeleroOpsReconciler {
	t.Helper()
	return &VeleroOpsReconciler{
		Client: operatorClient(t, proxyURL),
		Ops:    operatorService(t, proxyURL),
		Cell:   "preprod",
	}
}

// requireExercised est le garde-fou contre le vert vide : « aucun 403 » ne veut
// rien dire si aucun appel n'a été émis. Chaque entrée dit AUSSI à quoi
// l'appel sert, pour que la personne qui verra ce test tomber sache s'il faut
// corriger le RBAC ou le test.
func requireExercised(t *testing.T, rec *apiRecorder, attendus map[string]string) {
	t.Helper()
	var manquants []string
	for appel, pourquoi := range attendus {
		verbe, ressource, ok := strings.Cut(appel, " ")
		if !ok {
			t.Fatalf("entrée de plancher mal formée : %q, attendu \"verbe ressource\"", appel)
		}
		if !rec.saw(verbe, ressource) {
			manquants = append(manquants, fmt.Sprintf("  %s — %s", appel, pourquoi))
		}
	}
	if len(manquants) == 0 {
		return
	}
	sort.Strings(manquants)
	t.Fatalf("ce test n'exerce plus %d appel(s) qu'il est censé couvrir :\n%s\n\n"+
		"Il passerait donc au vert sans protéger ces droits-là. Soit le code a cessé de les émettre "+
		"(et le ClusterRole a du gras à retirer), soit ce test ne suit plus le chemin qu'il croit suivre.",
		len(manquants), strings.Join(manquants, "\n"))
}

// cleanupVCluster libère le CR à la fin d'un test : sans retrait du finalizer il
// resterait en Terminating pour toujours, envtest n'ayant pas de contrôleur pour
// le faire à notre place.
func cleanupVCluster(t *testing.T, admin ctrlclient.Client, nom string) {
	t.Helper()
	ctx := context.Background()
	var vc v1alpha1.VCluster
	key := types.NamespacedName{Name: nom, Namespace: DefaultVClustersNamespace}
	if err := admin.Get(ctx, key, &vc); err != nil {
		return
	}
	if len(vc.Finalizers) > 0 {
		vc.Finalizers = nil
		_ = admin.Update(ctx, &vc)
	}
	_ = admin.Delete(ctx, &vc)
}

// --- CRD tierces, en version squelette -------------------------------------

// probeCRDs sont les ressources tierces que l'opérateur touche et qu'envtest ne
// connaît pas : Flux et Velero.
//
// Sans elles, un appel répond 404 et le code s'arrête là — SetFluxSuspend, par
// exemple, patche le HelmRelease puis la Kustomization, donc un 404 sur le
// premier fait que le second n'est jamais émis et que son droit n'est jamais
// exercé. Ce sont des squelettes (`x-kubernetes-preserve-unknown-fields`), pas
// les vraies CRD : ce qui est testé ici, c'est le droit d'écrire dessus, pas la
// validité du contenu.
var probeCRDs = []struct{ group, version, plural, kind string }{
	{"helm.toolkit.fluxcd.io", "v2", "helmreleases", "HelmRelease"},
	{"kustomize.toolkit.fluxcd.io", "v1", "kustomizations", "Kustomization"},
	{"velero.io", "v1", "backups", "Backup"},
	{"velero.io", "v1", "restores", "Restore"},
	{"velero.io", "v1", "downloadrequests", "DownloadRequest"},
}

func installProbeCRDs(t *testing.T, ctx context.Context, admin ctrlclient.Client) {
	t.Helper()
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		t.Fatalf("client dynamique admin : %v", err)
	}

	for _, c := range probeCRDs {
		crd := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "apiextensions.k8s.io/v1",
			"kind":       "CustomResourceDefinition",
			"metadata":   map[string]any{"name": c.plural + "." + c.group},
			"spec": map[string]any{
				"group": c.group,
				"scope": "Namespaced",
				"names": map[string]any{
					"plural":   c.plural,
					"singular": strings.ToLower(c.kind),
					"kind":     c.kind,
					"listKind": c.kind + "List",
				},
				"versions": []any{map[string]any{
					"name": c.version, "served": true, "storage": true,
					"schema": map[string]any{"openAPIV3Schema": map[string]any{
						"type": "object", "x-kubernetes-preserve-unknown-fields": true,
					}},
				}},
			},
		}}
		if err := admin.Apply(ctx, ctrlclient.ApplyConfigurationFromUnstructured(crd),
			ctrlclient.FieldOwner("rbac-probe"), ctrlclient.ForceOwnership); err != nil {
			t.Fatalf("installation de la CRD %s : %v", crd.GetName(), err)
		}
	}

	// Une CRD acceptée n'est pas encore servie. Attendre explicitement, plutôt que
	// de laisser un 404 de course se faire passer pour « pas de CRD » — le test
	// dirait alors qu'un droit n'est pas exercé, sans dire pourquoi.
	deadline := time.Now().Add(30 * time.Second)
	for _, c := range probeCRDs {
		gvr := schema.GroupVersionResource{Group: c.group, Version: c.version, Resource: c.plural}
		for {
			_, err := dyn.Resource(gvr).Namespace("default").List(ctx, metav1.ListOptions{})
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("la CRD %s.%s n'est toujours pas servie après 30 s : %v", c.plural, c.group, err)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// --- objets semés pour que les chemins réels aient quelque chose à toucher --

func unstructuredObject(apiVersion, kind, ns, name string, spec map[string]any) *unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   map[string]any{"name": name},
	}
	if ns != "" {
		obj["metadata"].(map[string]any)["namespace"] = ns
	}
	if spec != nil {
		obj["spec"] = spec
	}
	return &unstructured.Unstructured{Object: obj}
}

// withFinalizer marque un objet comme retenu, ce qui est la condition pour que
// CleanupNamespace le réécrive.
func withFinalizer(obj *unstructured.Unstructured) *unstructured.Unstructured {
	obj.SetFinalizers([]string{"finalizers.fluxcd.io"})
	return obj
}

func nsObject(name string) *unstructured.Unstructured {
	return unstructuredObject("v1", "Namespace", "", name, nil)
}

// workloadSpec est le minimum qu'un StatefulSet ou un Deployment doit porter
// pour être accepté : un sélecteur, un template qui lui correspond, un conteneur.
func workloadSpec(name string) map[string]any {
	labels := map[string]any{"app": name}
	return map[string]any{
		"replicas": int64(1),
		"selector": map[string]any{"matchLabels": labels},
		"template": map[string]any{
			"metadata": map[string]any{"labels": labels},
			"spec": map[string]any{
				"containers": []any{map[string]any{"name": "c", "image": "registry.invalid/pause:latest"}},
			},
		},
	}
}

func statefulSetObject(ns, name string) *unstructured.Unstructured {
	spec := workloadSpec(name)
	spec["serviceName"] = name
	return unstructuredObject("apps/v1", "StatefulSet", ns, name, spec)
}

func deploymentObject(ns, name string) *unstructured.Unstructured {
	return unstructuredObject("apps/v1", "Deployment", ns, name, workloadSpec(name))
}

// vclusterPodObject plante le pod du control-plane d'un vcluster, avec les
// labels EXACTS que withVClusterPortForward cherche
// (`app=vcluster,release=vcluster-<nom>`, internal/kubernetes/vcluster_access.go).
//
// Sans ce pod, tout appel passant par le port-forward s'arrête sur « no vcluster
// pod found » AVANT d'émettre la moindre requête portforward — et le 403 qu'on
// veut détecter n'est jamais émis. C'est très exactement pour ça que le trou de
// droit sur `pods/portforward` a survécu à une suite RBAC qui exerçait déjà une
// vingtaine d'appels : le test ne pouvait pas atteindre la ligne fautive.
func vclusterPodObject(ns, name string) *unstructured.Unstructured {
	obj := unstructuredObject("v1", "Pod", ns, name, map[string]any{
		"containers": []any{map[string]any{"name": "syncer", "image": "registry.invalid/pause:latest"}},
	})
	obj.SetLabels(map[string]string{"app": "vcluster", "release": ns})
	return obj
}

func pvcObject(ns, name string) *unstructured.Unstructured {
	return unstructuredObject("v1", "PersistentVolumeClaim", ns, name, map[string]any{
		"accessModes": []any{"ReadWriteOnce"},
		"resources":   map[string]any{"requests": map[string]any{"storage": "1Gi"}},
	})
}

// veleroBackupObject plante un Backup Velero déjà en phase donnée. Les CRD
// squelettes de probeCRDs ne déclarent pas de sous-ressource status, donc
// `status` posé à la création est bien lu — c'est ce qui permet de faire
// passer le précheck d'une restauration in-place (createVeleroRestore exige
// phase=="Completed") sans faire tourner Velero.
func veleroBackupObject(ns, name, phase string) *unstructured.Unstructured {
	obj := unstructuredObject("velero.io/v1", "Backup", ns, name, map[string]any{
		"includedNamespaces": []any{ns},
	})
	obj.Object["status"] = map[string]any{"phase": phase}
	return obj
}

// --- lecture d'un chemin d'API ---------------------------------------------

// parseAPIPath décompose une URL d'apiserver comme l'apiserver le fait lui-même
// pour décider de l'autorisation. Écrit ici plutôt qu'importé : le paquet qui
// porte RequestInfoFactory (k8s.io/apiserver) est une dépendance de serveur, pas
// du dépôt.
func parseAPIPath(path string, watch bool) apiCall {
	c := apiCall{}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 {
		return c
	}

	var rest []string
	switch {
	case parts[0] == "api" && len(parts) >= 2:
		// /api/v1/...
		rest = parts[2:]
	case parts[0] == "apis" && len(parts) >= 3:
		// /apis/<group>/<version>/...
		c.Group = parts[1]
		rest = parts[3:]
	default:
		// /healthz, /openapi/... : hors du modèle de ressources, rien à autoriser.
		return c
	}

	if len(rest) >= 2 && rest[0] == "namespaces" && len(rest) != 2 {
		// « /namespaces/<ns> » tout court désigne LE namespace, pas un préfixe.
		c.Namespace = rest[1]
		rest = rest[2:]
	}
	if len(rest) > 0 {
		c.Resource = rest[0]
	}
	if len(rest) > 1 {
		c.Name = rest[1]
	}
	if len(rest) > 2 {
		c.Subresource = rest[2]
	}
	c.watch = watch
	return c
}

// rbacVerb rend le verbe que l'autorisateur applique, qui n'est pas la méthode
// HTTP : un GET sans nom est un `list`, un GET avec `?watch=true` un `watch`, un
// DELETE sans nom un `deletecollection`.
func rbacVerb(method string, c apiCall) string {
	if c.watch {
		return "watch"
	}
	switch method {
	case http.MethodPost:
		return "create"
	case http.MethodPut:
		return "update"
	case http.MethodPatch:
		return "patch"
	case http.MethodDelete:
		if c.Name == "" {
			return "deletecollection"
		}
		return "delete"
	case http.MethodGet, http.MethodHead:
		if c.Name == "" {
			return "list"
		}
		return "get"
	}
	return strings.ToLower(method)
}
