package gitops

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/gmalfray/vcluster-manager/internal/models"
)

// Combien de la sortie du générateur est réellement applicable par un API server,
// et combien reste de la tuyauterie kustomize lue depuis Git. Le chiffre décide
// si l'opérateur peut remplacer l'arborescence ou seulement s'y ajouter.
func TestHowMuchOfTheTreeIsApplicable(t *testing.T) {
	g := NewGenerator(testConfig())
	req := &models.CreateRequest{
		Name: "myvc", ArgoCD: true,
		FluxCDEnabled: true, FluxCDRepoURL: "ssh://git@example.com/x.git",
	}

	files := g.GenerateVCluster(req, "preprod")
	objects, err := g.ClusterObjects(req, "preprod", "")
	if err != nil {
		t.Fatalf("ClusterObjects: %v", err)
	}

	t.Logf("fichiers dans l'arbre : %d", len(files))
	for _, o := range objects {
		t.Logf("objet : %s %s/%s", o.GetKind(), o.GetNamespace(), o.GetName())
	}

	// 16 fichiers depuis la bascule navlink (l'overlay par vcluster a disparu),
	// dont 7 seulement sont des objets Kubernetes ; les 2 autres objets
	// (Namespace, ConfigMap de values) sont synthétisés, pas lus depuis un
	// fichier de l'arbre.
	if len(files) != 16 {
		t.Fatalf("arbre = %d fichiers, attendu 16", len(files))
	}
	if len(objects) != 9 {
		t.Fatalf("objets = %d, attendu 9", len(objects))
	}
}

// La bascule navlink : la Kustomization pointe sur le lib partagé et lit ses
// valeurs dans le ConfigMap de substitutions, au lieu d'un overlay commité à
// côté. C'est le patron que les quatre autres suivront.
func TestNavlinkReadsSharedLibAndSubstitutes(t *testing.T) {
	g := NewGenerator(testConfig())
	req := &models.CreateRequest{Name: "myvc", ArgoCD: true}
	files := g.GenerateVCluster(req, "preprod")

	var navlink string
	for _, f := range files {
		if strings.HasSuffix(f.Path, "/tenant/navlink_kustomization.yaml") {
			navlink = f.Content
		}
		if strings.Contains(f.Path, "/tenant/navlink/") {
			t.Fatalf("overlay navlink par vcluster toujours commité : %s", f.Path)
		}
	}
	if navlink == "" {
		t.Fatal("pas de Kustomization navlink")
	}
	if !strings.Contains(navlink, "path: ./lib/tenant-template/argocd/navlink") {
		t.Errorf("navlink ne pointe pas sur le lib partagé:\n%s", navlink)
	}
	if !strings.Contains(navlink, "name: vcluster-myvc-substitutions") {
		t.Errorf("navlink ne lit pas le ConfigMap de substitutions:\n%s", navlink)
	}
}

// Le ConfigMap de substitutions porte tout ce que le CR dérive, avec un jeu de
// clés stable : une option désactivée vide sa valeur, elle ne retire pas la clé.
// Une clé absente laisserait Flux poser "${ARGOCD_URL}" tel quel dans un objet.
func TestSubstitutionsKeepAStableKeySet(t *testing.T) {
	g := NewGenerator(testConfig())

	full := g.Substitutions(&models.CreateRequest{
		Name: "myvc", ArgoCD: true, ArgoCDVersion: "v2.9.3",
		RBACGroups:    []string{"team-a"},
		VeleroEnabled: true, VeleroHour: "03:00", VeleroTTL: "720h0m0s",
		FluxCDEnabled: true, FluxCDRepoURL: "ssh://git@example.com/x.git",
	}, "preprod", "1.31.0")
	bare := g.Substitutions(&models.CreateRequest{Name: "myvc"}, "preprod", "")

	if len(full) != len(bare) {
		t.Fatalf("jeu de clés instable : %d avec tout activé, %d à nu", len(full), len(bare))
	}
	for k := range full {
		if _, ok := bare[k]; !ok {
			t.Errorf("clé %s absente quand l'option est désactivée", k)
		}
	}

	// Quelques dérivés qui ne doivent jamais venir du spec.
	want := map[string]string{
		"VCLUSTER_NAMESPACE":         "vcluster-myvc",
		"VCLUSTER_API_HOST":          "myvc.api.preprod.example.com",
		"VCLUSTER_DOMAIN":            "myvc.preprod.example.com",
		"VCLUSTER_TARGET_REVISION":   "preprod",
		"VCLUSTER_TLS_SECRET":        "wildcard-preprod-example-com-tls",
		"VAULT_KUBERNETES_AUTH_PATH": "kubernetes-vcluster-myvc-preprod",
		"ARGOCD_CLIENT_ID":           "argocd-k8s-myvc-preprod",
		"ARGOCD_URL":                 "https://argocd.myvc.preprod.example.com/",
		"ARGOCD_NAVLINK_LABEL":       "ArgoCD myvc preprod",
		"VCLUSTER_K8S_VERSION":       "1.31.0",
	}
	for k, v := range want {
		if full[k] != v {
			t.Errorf("%s = %q, attendu %q", k, full[k], v)
		}
	}
	if bare["ARGOCD_ENABLED"] != "false" || bare["ARGOCD_URL"] != "" {
		t.Errorf("ArgoCD désactivé devrait vider les valeurs, pas les remplir : %q / %q",
			bare["ARGOCD_ENABLED"], bare["ARGOCD_URL"])
	}
	// Les dérivés changent avec la cell sans que le CR bouge : c'est ce qui fait
	// tenir la promotion preprod→prod (crd-vcluster.md §2.2).
	prod := g.Substitutions(&models.CreateRequest{Name: "myvc"}, "prod", "")
	if prod["VCLUSTER_DOMAIN"] == full["VCLUSTER_DOMAIN"] {
		t.Error("le domaine ne suit pas la cell")
	}
}

// Aucun overlay kustomize ne doit se retrouver dans les objets : il n'a pas de
// sens pour un API server, et l'y envoyer échouerait en « no matches for kind ».
func TestKustomizeOverlaysAreNotApplied(t *testing.T) {
	g := NewGenerator(testConfig())
	req := &models.CreateRequest{Name: "myvc", ArgoCD: true}
	objects, err := g.ClusterObjects(req, "preprod", "")
	if err != nil {
		t.Fatalf("ClusterObjects: %v", err)
	}
	for _, o := range objects {
		if strings.HasPrefix(o.GetAPIVersion(), "kustomize.config.k8s.io/") {
			t.Fatalf("overlay kustomize dans les objets : %s", o.GetName())
		}
	}
}

// Le ConfigMap de values doit porter le même nom que celui que le
// configMapGenerator produit dans l'arbre : le HelmRelease le référence par nom
// dans valuesFrom, un nom qui bouge le décroche.
func TestValuesConfigMapMatchesTheGeneratedOne(t *testing.T) {
	g := NewGenerator(testConfig())
	req := &models.CreateRequest{Name: "myvc", VeleroEnabled: true, VeleroHour: "03:00"}
	objects, err := g.ClusterObjects(req, "preprod", "1.31.0")
	if err != nil {
		t.Fatalf("ClusterObjects: %v", err)
	}

	var cm map[string]any
	for _, o := range objects {
		if o.GetKind() == "ConfigMap" {
			cm = o.Object
		}
	}
	if cm == nil {
		t.Fatal("pas de ConfigMap de values")
	}
	meta := cm["metadata"].(map[string]any)
	if meta["name"] != "vcluster-myvc-values" || meta["namespace"] != "vcluster-myvc" {
		t.Fatalf("nom/namespace = %v/%v", meta["name"], meta["namespace"])
	}

	values := cm["data"].(map[string]any)["values.yaml"].(string)
	for _, want := range []string{"myvc.api.preprod.example.com", "CRON_TZ=Europe/Paris", `version: "1.31.0"`} {
		if !strings.Contains(values, want) {
			t.Errorf("values.yaml sans %q", want)
		}
	}
}

// Le namespace hôte porte le label que la ValidatingAdmissionPolicy
// (deploy/base/operator-admission-policy.yaml) exige pour laisser l'opérateur
// le modifier ou le supprimer. Sans lui, un namespace appliqué par l'opérateur
// resterait aussi bloqué que la flotte historique qu'on cherche à exclure.
func TestHostNamespaceCarriesTheManagedLabel(t *testing.T) {
	ns := HostNamespace("myvc")
	if ns.GetKind() != "Namespace" || ns.GetName() != "vcluster-myvc" {
		t.Fatalf("namespace = %s %q, attendu Namespace vcluster-myvc", ns.GetKind(), ns.GetName())
	}
	labels := ns.GetLabels()
	if labels["vcluster.rebuild-it.fr/managed-namespace"] != "true" {
		t.Fatalf("label vcluster.rebuild-it.fr/managed-namespace = %q, attendu \"true\" (labels = %v)",
			labels["vcluster.rebuild-it.fr/managed-namespace"], labels)
	}
}

func TestVeleroTTLFromShort(t *testing.T) {
	tests := map[string]string{
		"30j": "720h0m0s", "12h": "12h0m0s", "90m": "0h90m0s",
		"": "", "abc": "", "0j": "", "-3h": "",
	}
	for in, want := range tests {
		if got := VeleroTTLFromShort(in); got != want {
			t.Errorf("VeleroTTLFromShort(%q) = %q, attendu %q", in, got, want)
		}
	}
}

// TestNavlinkIsSelfSufficientOnTheGitOpsPath verrouille ce qui rend le navlink
// déployable sans opérateur.
//
// Ce test existe parce que le cas s'est produit et n'a été vu qu'en recette, le
// 2026-08-09, dans les logs de Flux : la Kustomization du navlink déclarait un
// `substituteFrom` NON optionnel sur `vcluster-<nom>-substitutions`, un
// ConfigMap que seul l'opérateur produit (SubstitutionConfigMap : « the single
// object the operator owns on the host cluster »). Sur un vcluster créé par le
// chemin app→GitOps il n'y a pas de CR, donc pas de ConfigMap, donc :
//
//	post build failed for 'link-argocd': substitute from
//	'ConfigMap/vcluster-recette-restore-a-substitutions' error: not found
//
// en boucle, et le lien n'apparaissait jamais dans Rancher. Côté application,
// rien ne le signalait : la création réussissait, les fichiers étaient commités.
//
// Deux propriétés sont donc exigées ici, et il faut les deux : le ConfigMap de
// l'opérateur doit être `optional` (sinon le chemin GitOps échoue), et les
// valeurs doivent être substituées en direct (sinon `${ARGOCD_URL}` partirait
// littéralement dans l'objet NavLink, ce que le commentaire de Substitutions
// désigne déjà comme le résultat à éviter).
func TestNavlinkIsSelfSufficientOnTheGitOpsPath(t *testing.T) {
	g := NewGenerator(testConfig())
	req := &models.CreateRequest{Name: "myvc", ArgoCD: true}

	var navlink string
	for _, f := range g.GenerateVCluster(req, "preprod") {
		if strings.HasSuffix(f.Path, "navlink_kustomization.yaml") {
			navlink = f.Content
		}
	}
	if navlink == "" {
		t.Fatal("aucun navlink_kustomization.yaml généré alors qu'ArgoCD est activé")
	}

	var k struct {
		Spec struct {
			PostBuild struct {
				SubstituteFrom []struct {
					Name     string `yaml:"name"`
					Optional bool   `yaml:"optional"`
				} `yaml:"substituteFrom"`
				Substitute map[string]string `yaml:"substitute"`
			} `yaml:"postBuild"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(navlink), &k); err != nil {
		t.Fatalf("parsing du navlink généré : %v", err)
	}

	for _, sf := range k.Spec.PostBuild.SubstituteFrom {
		if strings.HasSuffix(sf.Name, "-substitutions") && !sf.Optional {
			t.Errorf("le navlink dépend de %q sans `optional: true` : ce ConfigMap n'existe "+
				"que sur le chemin opérateur, donc la Kustomization échoue en boucle pour tout "+
				"vcluster créé par le chemin app→GitOps", sf.Name)
		}
	}

	// Les valeurs doivent venir du fichier généré, pas d'un objet que seul
	// l'opérateur pose.
	for _, key := range []string{"ARGOCD_URL", "ARGOCD_NAVLINK_LABEL"} {
		v, ok := k.Spec.PostBuild.Substitute[key]
		if !ok || v == "" {
			t.Errorf("%s n'est pas substitué en direct dans le navlink : sans lui le chemin "+
				"GitOps rendrait un NavLink portant le placeholder littéral", key)
		}
	}

	// Le lien doit viser l'ArgoCD DU vcluster, pas l'ArgoCD central : c'est
	// toute la raison d'être de cet overlay par rapport au navlink générique.
	if url := k.Spec.PostBuild.Substitute["ARGOCD_URL"]; !strings.Contains(url, "myvc") {
		t.Errorf("ARGOCD_URL = %q ne porte pas le nom du vcluster : le lien pointerait vers "+
			"un autre ArgoCD que celui du tenant", url)
	}
}
