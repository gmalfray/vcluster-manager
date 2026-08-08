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
