package main

import (
	"os/exec"
	"strings"
	"testing"
)

// Même garde-fou que cmd/operator/main_test.go, et pour la même raison :
// controller-runtime enregistre un flag `--kubeconfig` dans son init(), et
// en redéfinir un du même nom fait paniquer le binaire au démarrage. Aucun
// test du reconciler ne peut l'attraper puisqu'aucun ne passe par main().
func TestBinaryParsesItsFlags(t *testing.T) {
	out, err := exec.Command("go", "run", ".", "--help").CombinedOutput()
	if strings.Contains(string(out), "flag redefined") || strings.Contains(string(out), "panic:") {
		t.Fatalf("le binaire panique au démarrage :\n%s", out)
	}
	if !strings.Contains(string(out), "-cell") {
		t.Fatalf("les flags ne sont pas exposés (err=%v) :\n%s", err, out)
	}
}
