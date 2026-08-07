package kubernetes

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clienttesting "k8s.io/client-go/testing"
)

func testNamespace(name string, annotations map[string]string) *unstructured.Unstructured {
	ns := &unstructured.Unstructured{}
	ns.SetAPIVersion("v1")
	ns.SetKind("Namespace")
	ns.SetName("vcluster-" + name)
	if annotations != nil {
		ns.SetAnnotations(annotations)
	}
	return ns
}

func TestGetNamespaceProtection_AnnotationSet(t *testing.T) {
	s := NewTestStatusClient(testNamespace("demo", map[string]string{"protect-deletion": "true"}))
	protected, err := s.GetNamespaceProtection(context.Background(), "demo")
	if err != nil {
		t.Fatalf("erreur inattendue : %v", err)
	}
	if !protected {
		t.Fatal("attendu protected=true, l'annotation est posée")
	}
}

func TestGetNamespaceProtection_NoAnnotation(t *testing.T) {
	s := NewTestStatusClient(testNamespace("demo", nil))
	protected, err := s.GetNamespaceProtection(context.Background(), "demo")
	if err != nil {
		t.Fatalf("erreur inattendue : %v", err)
	}
	if protected {
		t.Fatal("attendu protected=false, aucune annotation")
	}
}

func TestGetNamespaceProtection_NamespaceMissingIsNotAnError(t *testing.T) {
	// Aucun namespace seedé : le namespace n'existe pas, ce qui est un fait
	// connu (pas de protection possible sur un namespace absent), pas une
	// lecture ratée.
	s := NewTestStatusClient()
	protected, err := s.GetNamespaceProtection(context.Background(), "demo")
	if err != nil {
		t.Fatalf("un namespace absent doit rendre (false, nil), pas une erreur : %v", err)
	}
	if protected {
		t.Fatal("attendu protected=false pour un namespace inexistant")
	}
}

func TestGetNamespaceProtection_ReadFailureIsAnError(t *testing.T) {
	// Reactor qui fait échouer le Get sur le namespace avec autre chose qu'un
	// NotFound : ça doit remonter comme une erreur, pas comme protected=false.
	readErr := errors.New("etcd is on fire")
	s := NewTestStatusClientWithReactor(func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.GetVerb() == "get" && action.GetResource().Resource == "namespaces" {
			return true, nil, readErr
		}
		return false, nil, nil
	}, testNamespace("demo", map[string]string{"protect-deletion": "true"}))

	protected, err := s.GetNamespaceProtection(context.Background(), "demo")
	if err == nil {
		t.Fatal("une lecture qui échoue doit rendre une erreur, pas (false, nil)")
	}
	if protected {
		t.Fatal("protected doit rester false sur une lecture ratée")
	}
}

func TestGetNamespaceProtection_NotFoundStillDistinguishable(t *testing.T) {
	// Vérifie que la fonction utilise bien apierrors.IsNotFound et pas une
	// simple comparaison "err != nil" — un NotFound explicite via reactor.
	s := NewTestStatusClientWithReactor(func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.GetVerb() == "get" && action.GetResource().Resource == "namespaces" {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, "vcluster-demo")
		}
		return false, nil, nil
	})
	protected, err := s.GetNamespaceProtection(context.Background(), "demo")
	if err != nil {
		t.Fatalf("un NotFound explicite doit rester (false, nil) : %v", err)
	}
	if protected {
		t.Fatal("attendu protected=false")
	}
}
