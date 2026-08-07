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

	requested, err := s.DeleteHostNamespace(context.Background(), "demo")
	if err != nil {
		t.Fatalf("suppression : %v", err)
	}
	if !requested {
		t.Fatal("requested=false alors que le namespace était bien là : l'audit ne dira " +
			"jamais qu'un namespace a été supprimé")
	}
	if namespaceExists(t, s, "vcluster-demo") {
		t.Fatal("vcluster-demo est toujours là : la suppression n'a pas porté")
	}
	if !namespaceExists(t, s, "demo") {
		t.Fatal("le namespace `demo` a été supprimé à la place de `vcluster-demo` : " +
			"le préfixe est perdu, et c'est un namespace de la plateforme qui part")
	}
}

// Un namespace déjà absent n'est pas une erreur, et ne se journalise pas.
//
// Les deux moitiés comptent. Si l'absence remontait en erreur, le finalizer —
// qui rejoue cette étape à chaque tour — repartirait en boucle d'échecs sur une
// suppression qui a réussi. Et si `requested` valait true, chaque tour
// écrirait une ligne d'audit « namespace supprimé » pour un namespace qui
// n'était plus là depuis longtemps.
func TestDeleteHostNamespaceAbsentIsNeitherAnErrorNorAnAuditLine(t *testing.T) {
	s := NewTestStatusClient()

	requested, err := s.DeleteHostNamespace(context.Background(), "jamais-monte")
	if err != nil {
		t.Fatalf("un namespace absent doit rendre (false, nil), pas une erreur : %v", err)
	}
	if requested {
		t.Fatal("requested=true sur un namespace qui n'existait pas : l'audit annoncerait " +
			"une suppression qui n'a rien supprimé")
	}
}

// L'appel est rejoué à chaque tour du finalizer tant que la disparition n'est
// pas constatée. Le deuxième appel doit donc être aussi calme que le premier :
// pas d'erreur, et plus rien à journaliser.
func TestDeleteHostNamespaceIsReplayable(t *testing.T) {
	ctx := context.Background()
	s := NewTestStatusClient(namespaceObj("vcluster-demo"))

	if _, err := s.DeleteHostNamespace(ctx, "demo"); err != nil {
		t.Fatalf("premier appel : %v", err)
	}
	requested, err := s.DeleteHostNamespace(ctx, "demo")
	if err != nil {
		t.Fatalf("rejouer la suppression doit rester sans erreur, sinon le finalizer boucle "+
			"sur un échec permanent : %v", err)
	}
	if requested {
		t.Fatal("requested=true au deuxième appel : une ligne d'audit par tour de reconcile " +
			"pour une seule suppression réelle")
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

	requested, err := s.DeleteHostNamespace(context.Background(), "demo")
	if err == nil {
		t.Fatal("un refus de l'API server rendu (false, nil) : la séquence de suppression " +
			"continuerait comme si la demande était partie")
	}
	if requested {
		t.Fatal("requested=true sur un refus : l'audit annoncerait une suppression refusée")
	}
	// Le message est ce qu'un exploitant lit dans la condition NamespaceRemoved
	// quand le CR reste coincé. S'il ne nomme pas le namespace, la condition ne
	// dit pas sur quoi le droit manque.
	if !strings.Contains(err.Error(), "vcluster-demo") {
		t.Fatalf("l'erreur ne nomme pas le namespace concerné : %q", err)
	}
}

// Le refus peut tomber sur la LECTURE, pas seulement sur la suppression — c'est
// même le cas le plus probable d'un RBAC incomplet, `get` et `delete` étant deux
// verbes distincts.
//
// Cette lecture existe pour distinguer « je viens de le condamner » de « il
// l'était déjà ». Quand elle échoue, on ne sait ni l'un ni l'autre : rendre
// (false, nil) dirait « il n'y avait rien à supprimer » sur un hoquet d'API, et
// le finalizer conclurait la séquence sur cette phrase-là.
func TestDeleteHostNamespaceReadRefusalIsAnError(t *testing.T) {
	refus := apierrors.NewForbidden(
		namespaceGVR.GroupResource(), "vcluster-demo",
		errors.New(`namespaces "vcluster-demo" is forbidden: User "system:serviceaccount:vcluster-manager:operator" cannot get resource "namespaces"`))

	s := NewTestStatusClientWithReactor(func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.GetVerb() == "get" && action.GetResource().Resource == "namespaces" {
			return true, nil, refus
		}
		return false, nil, nil
	}, namespaceObj("vcluster-demo"))

	requested, err := s.DeleteHostNamespace(context.Background(), "demo")
	if err == nil {
		t.Fatal("lecture refusée rendue (false, nil) : « je n'ai pas pu regarder » " +
			"passerait pour « il n'y avait rien à supprimer »")
	}
	if requested {
		t.Fatal("requested=true alors que rien n'a pu être lu ni supprimé")
	}
	if !strings.Contains(err.Error(), "vcluster-demo") {
		t.Fatalf("l'erreur ne nomme pas le namespace concerné : %q", err)
	}
}
