package deploymanifests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// baseDir est deploy/base vu depuis ce paquet.
const baseDir = "../../deploy/base"

// TestEveryManifestIsReferencedInKustomization attrape un défaut qui ne
// provoque aucune erreur : un fichier ajouté à deploy/base mais absent de
// kustomization.yaml n'est tout simplement jamais déployé. Le fichier est là,
// il est correct, il est relu en revue — et l'objet qu'il décrit n'existe sur
// aucun cluster. Rien ne le signale : le rendu kustomize réussit, Flux
// réconcilie, tout est vert.
//
// Ce test existe parce que le cas s'est produit en écrivant
// namespace-protection-policy.yaml : la policy avait été testée sur un cluster
// et fonctionnait, mais elle serait partie en production sans jamais être
// appliquée.
func TestEveryManifestIsReferencedInKustomization(t *testing.T) {
	kustomization, err := os.ReadFile(filepath.Join(baseDir, "kustomization.yaml"))
	if err != nil {
		t.Fatalf("reading kustomization.yaml: %v", err)
	}

	var k struct {
		Resources []string `yaml:"resources"`
		Patches   []struct {
			Path string `yaml:"path"`
		} `yaml:"patches"`
	}
	if err := yaml.Unmarshal(kustomization, &k); err != nil {
		t.Fatalf("parsing kustomization.yaml: %v", err)
	}

	referenced := map[string]bool{"kustomization.yaml": true}
	for _, r := range k.Resources {
		referenced[r] = true
	}
	for _, p := range k.Patches {
		referenced[p.Path] = true
	}

	entries, err := os.ReadDir(baseDir)
	if err != nil {
		t.Fatalf("reading %s: %v", baseDir, err)
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		if !referenced[e.Name()] {
			t.Errorf("deploy/base/%s n'est référencé ni dans resources ni dans patches de "+
				"kustomization.yaml : il ne sera jamais déployé, et rien ne le signalera", e.Name())
		}
	}
}

// TestEveryAdmissionPolicyHasItsBinding verrouille l'invariant que les
// commentaires de operator-admission-policy.yaml et
// namespace-protection-policy.yaml énoncent tous les deux : « une
// ValidatingAdmissionPolicy sans ValidatingAdmissionPolicyBinding ne s'applique
// à personne ».
//
// C'est le pire des faux verts d'admission : la policy apparaît bien dans
// `kubectl get validatingadmissionpolicy`, elle a l'air en place, et elle
// n'évalue rien du tout. Un contrôle de sécurité qu'on croit actif est plus
// dangereux qu'un contrôle absent, parce qu'on cesse de chercher ailleurs.
func TestEveryAdmissionPolicyHasItsBinding(t *testing.T) {
	policies := map[string]string{}   // nom de la policy -> fichier
	bindings := map[string]string{}   // policyName visé -> fichier
	bindingNames := map[string]bool{} // noms de bindings, pour le sens inverse

	entries, err := os.ReadDir(baseDir)
	if err != nil {
		t.Fatalf("reading %s: %v", baseDir, err)
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(baseDir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		dec := yaml.NewDecoder(strings.NewReader(string(content)))
		for {
			var doc struct {
				Kind     string `yaml:"kind"`
				Metadata struct {
					Name string `yaml:"name"`
				} `yaml:"metadata"`
				Spec struct {
					PolicyName string `yaml:"policyName"`
				} `yaml:"spec"`
			}
			if err := dec.Decode(&doc); err != nil {
				break // fin du fichier, ou document non conforme : sans intérêt ici
			}
			switch doc.Kind {
			case "ValidatingAdmissionPolicy":
				policies[doc.Metadata.Name] = e.Name()
			case "ValidatingAdmissionPolicyBinding":
				bindings[doc.Spec.PolicyName] = e.Name()
				bindingNames[doc.Metadata.Name] = true
			}
		}
	}

	if len(policies) == 0 {
		t.Fatal("aucune ValidatingAdmissionPolicy trouvée : la prémisse de ce test a disparu, relire")
	}

	for name, file := range policies {
		if _, ok := bindings[name]; !ok {
			t.Errorf("la ValidatingAdmissionPolicy %q (%s) n'a aucun Binding : elle sera visible "+
				"dans le cluster et n'évaluera rien", name, file)
		}
	}

	// Le sens inverse : un binding qui vise une policy absente ne protège rien
	// non plus, et ne se voit pas mieux.
	for policyName, file := range bindings {
		if _, ok := policies[policyName]; !ok {
			t.Errorf("le Binding de %s vise la policy %q, qui n'est définie nulle part dans deploy/base",
				file, policyName)
		}
	}
}

// TestMetricsIsRefusedWithoutRelyingOnASourceAddress verrouille la forme du
// refus sur /metrics, pas seulement son existence.
//
// Ce test existe parce que la version précédente du manifeste avait l'air
// correcte et ne protégeait rien : `whitelist-source-range: 127.0.0.1/32`, avec
// le raisonnement « le loopback, donc personne ». Mesuré le 2026-08-09 sur la
// plateforme de recette : nginx voit 127.0.0.1 comme adresse source pour le
// trafic venu d'Internet, si bien que cette whitelist autorisait exactement
// tout le monde. `/metrics` répondait 200 depuis Internet, sans
// authentification, alors que la règle `allow 127.0.0.1/32; deny all;` était
// bien présente dans la configuration nginx générée.
//
// Le défaut n'était donc pas dans le manifeste mais dans l'hypothèse qu'il
// faisait sur ce que le proxy observe. Un test qui vérifierait seulement
// « il y a une restriction » aurait été vert tout du long. Celui-ci exige que
// le refus ne dépende d'AUCUNE adresse : c'est la seule propriété qu'une erreur
// de topologie réseau ne peut pas retourner en autorisation.
func TestMetricsIsRefusedWithoutRelyingOnASourceAddress(t *testing.T) {
	content, err := os.ReadFile(filepath.Join(baseDir, "ingress.yaml"))
	if err != nil {
		t.Fatalf("reading ingress.yaml: %v", err)
	}

	const (
		denyKey  = "nginx.ingress.kubernetes.io/denylist-source-range"
		allowKey = "nginx.ingress.kubernetes.io/whitelist-source-range"
	)

	found := false
	dec := yaml.NewDecoder(strings.NewReader(string(content)))
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name        string            `yaml:"name"`
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"metadata"`
			Spec struct {
				Rules []struct {
					HTTP struct {
						Paths []struct {
							Path string `yaml:"path"`
						} `yaml:"paths"`
					} `yaml:"http"`
				} `yaml:"rules"`
			} `yaml:"spec"`
		}
		if err := dec.Decode(&doc); err != nil {
			break
		}
		if doc.Kind != "Ingress" {
			continue
		}
		servesMetrics := false
		for _, r := range doc.Spec.Rules {
			for _, p := range r.HTTP.Paths {
				if p.Path == "/metrics" {
					servesMetrics = true
				}
			}
		}
		if !servesMetrics {
			continue
		}
		found = true

		if src, ok := doc.Metadata.Annotations[allowKey]; ok {
			t.Errorf("l'Ingress %q protège /metrics avec %s=%q : une allow-list fondée sur "+
				"l'adresse source ne tient que si le proxy observe la vraie adresse du client, "+
				"ce qui n'est pas garanti — c'est exactement ainsi que /metrics s'est retrouvé "+
				"ouvert sur Internet", doc.Metadata.Name, allowKey, src)
		}

		deny := doc.Metadata.Annotations[denyKey]
		if !strings.Contains(deny, "0.0.0.0/0") {
			t.Errorf("l'Ingress %q sert /metrics sans refus inconditionnel : %s=%q, "+
				"attendu une plage couvrant 0.0.0.0/0", doc.Metadata.Name, denyKey, deny)
		}
	}

	if !found {
		t.Fatal("aucun Ingress de deploy/base ne sert /metrics : si le chemin n'est plus " +
			"intercepté, c'est l'Ingress principal qui le sert, donc sans aucune restriction")
	}
}
