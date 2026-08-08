package kubernetes

import (
	"context"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"
)

// namespaceObj construit un namespace nu, sous le nom EXACT qu'on lui donne —
// sans préfixe ajouté. C'est ce qui permet de vérifier que DeleteHostNamespace
// préfixe elle-même, plutôt que de le supposer.
func namespaceObj(name string) *unstructured.Unstructured {
	ns := &unstructured.Unstructured{}
	ns.SetAPIVersion("v1")
	ns.SetKind("Namespace")
	ns.SetName(name)
	return ns
}

func namespaceExists(t *testing.T, s *StatusClient, name string) bool {
	t.Helper()
	_, err := s.client.Resource(namespaceGVR).Get(context.Background(), name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatalf("lecture du namespace %s : %v", name, err)
	}
	return true
}

// Le namespace supprimé est `vcluster-<nom>`, jamais `<nom>` tout court.
//
// Le préfixe n'est pas un détail de nommage ici : l'appelant passe un nom de
// vcluster, et un namespace plat du même nom peut parfaitement exister sur la
// cell — c'est là que vivent les charges de la plateforme. Se tromper de côté du
// préfixe supprime le mauvais namespace, et ce qui est supprimé ici l'est pour
// de bon.
func TestDeleteHostNamespacePrefixesTheName(t *testing.T) {
	s := NewTestStatusClient(namespaceObj("vcluster-demo"), namespaceObj("demo"))

	if err := s.DeleteHostNamespace(context.Background(), "demo"); err != nil {
		t.Fatalf("suppression : %v", err)
	}
	if namespaceExists(t, s, "vcluster-demo") {
		t.Fatal("vcluster-demo est toujours là : la suppression n'a pas porté")
	}
	if !namespaceExists(t, s, "demo") {
		t.Fatal("le namespace `demo` a été supprimé à la place de `vcluster-demo` : " +
			"le préfixe est perdu, et c'est un namespace de la plateforme qui part")
	}
}

// Un namespace déjà absent n'est pas une erreur.
//
// La méthode n'a plus à filtrer une redemande sur un namespace déjà en
// Terminating — c'est désormais l'affaire de l'appelant, qui observe avant
// d'appeler (voir HostNamespaceState). Ce qui reste ici, en défense, c'est le
// cas où le namespace a disparu entre l'observation de l'appelant et cet
// appel : ni erreur, ni panique.
func TestDeleteHostNamespaceAbsentIsNotAnError(t *testing.T) {
	s := NewTestStatusClient()

	if err := s.DeleteHostNamespace(context.Background(), "jamais-monte"); err != nil {
		t.Fatalf("un namespace absent doit rendre nil, pas une erreur : %v", err)
	}
}

// Un refus de l'API server — RBAC sans le verbe `delete`, webhook d'admission —
// remonte comme erreur.
//
// C'est le cas de recette « le ClusterRole n'a pas été redéployé » : l'avaler
// ferait conclure la suppression par observation sur un namespace que personne
// n'a le droit de toucher, et le CR partirait en laissant tout debout.
func TestDeleteHostNamespaceRefusalIsAnError(t *testing.T) {
	refus := apierrors.NewForbidden(
		namespaceGVR.GroupResource(), "vcluster-demo",
		errors.New(`namespaces "vcluster-demo" is forbidden: User "system:serviceaccount:vcluster-manager:operator" cannot delete resource "namespaces"`))

	s := NewTestStatusClientWithReactor(func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.GetVerb() == "delete" && action.GetResource().Resource == "namespaces" {
			return true, nil, refus
		}
		return false, nil, nil
	}, namespaceObj("vcluster-demo"))

	err := s.DeleteHostNamespace(context.Background(), "demo")
	if err == nil {
		t.Fatal("un refus de l'API server rendu nil : la séquence de suppression " +
			"continuerait comme si la demande était partie")
	}
	// Le message est ce qu'un exploitant lit dans la condition NamespaceRemoved
	// quand le CR reste coincé. S'il ne nomme pas le namespace, la condition ne
	// dit pas sur quoi le droit manque.
	if !strings.Contains(err.Error(), "vcluster-demo") {
		t.Fatalf("l'erreur ne nomme pas le namespace concerné : %q", err)
	}
}
