package main

import (
	"os/exec"
	"strings"
	"testing"
)

// Le binaire doit au moins pouvoir parser ses flags. Trivial en apparence, mais
// c'est précisément ce qui a manqué : controller-runtime enregistre un flag
// `--kubeconfig` sur flag.CommandLine dans son init(), et en redéfinir un du
// même nom faisait paniquer l'opérateur au démarrage — en boucle, une fois
// déployé. Aucun test du reconciler ne pouvait l'attraper : ils construisent un
// manager, ils n'exécutent jamais main().
//
// `go run` plutôt qu'un appel de fonction : la panique venait de l'init() du
// paquet, donc il faut vraiment démarrer le programme pour la déclencher.
func TestBinaryParsesItsFlags(t *testing.T) {
	out, err := exec.Command("go", "run", ".", "--help").CombinedOutput()
	// --help sort en code 2 : c'est attendu, ce qu'on refuse c'est la panique.
	if strings.Contains(string(out), "flag redefined") || strings.Contains(string(out), "panic:") {
		t.Fatalf("le binaire panique au démarrage :\n%s", out)
	}
	if !strings.Contains(string(out), "-cell") {
		t.Fatalf("les flags ne sont pas exposés (err=%v) :\n%s", err, out)
	}
}
