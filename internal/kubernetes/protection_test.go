package kubernetes

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// --- HostNamespaceState -----------------------------------------------------

func TestHostNamespaceState_Absent(t *testing.T) {
	s := NewTestStatusClient()
	st, known := s.HostNamespaceState(context.Background(), "demo")
	if !known {
		t.Fatal("un namespace absent est un fait connu, pas une lecture ratée")
	}
	if st.Exists {
		t.Fatal("attendu Exists=false")
	}
}

func TestHostNamespaceState_LiveNamespaceHasNoDeletionTimestamp(t *testing.T) {
	s := NewTestStatusClient(testNamespace("demo", nil))
	st, known := s.HostNamespaceState(context.Background(), "demo")
	if !known || !st.Exists {
		t.Fatalf("attendu known=true, Exists=true ; eu known=%v, %+v", known, st)
	}
	if !st.DeletionTimestamp.IsZero() {
		t.Fatalf("un namespace vivant ne porte pas de deletionTimestamp, eu %v", st.DeletionTimestamp)
	}
}

// Le deletionTimestamp est un fait posé par l'apiserver, pas par l'opérateur :
// c'est précisément ce qui rend cette borne-là fiable pendant un refus de
// suppression, où il n'existe tout simplement pas encore.
func TestHostNamespaceState_ReadsTheDeletionTimestamp(t *testing.T) {
	ns := testNamespace("demo", nil)
	// Tronqué à la seconde : un objet non typé sérialise deletionTimestamp en
	// RFC3339 (SetDeletionTimestamp l'écrit comme une chaîne), qui ne porte pas
	// la précision infra-seconde de time.Now().
	when := metav1.NewTime(time.Now().Add(-3 * time.Minute).Truncate(time.Second))
	ns.SetDeletionTimestamp(&when)

	s := NewTestStatusClient(ns)
	st, known := s.HostNamespaceState(context.Background(), "demo")
	if !known || !st.Exists {
		t.Fatalf("attendu known=true, Exists=true ; eu known=%v, %+v", known, st)
	}
	if !st.DeletionTimestamp.Equal(when.Time) {
		t.Fatalf("deletionTimestamp = %v, attendu %v", st.DeletionTimestamp, when.Time)
	}
}

// status.conditions doit être lu tel quel — c'est ce qui nomme le finalizer ou
// la ressource qui retient un namespace, au lieu de le laisser deviner par
// l'appelant.
func TestHostNamespaceState_ReadsTheConditions(t *testing.T) {
	ns := testNamespace("demo", nil)
	if err := unstructured.SetNestedSlice(ns.Object, []interface{}{
		map[string]interface{}{
			"type":    "NamespaceFinalizersRemaining",
			"status":  "True",
			"reason":  "SomeFinalizersRemain",
			"message": "some finalizers remain: recette.local/bloque",
		},
	}, "status", "conditions"); err != nil {
		t.Fatalf("seed conditions: %v", err)
	}

	s := NewTestStatusClient(ns)
	st, known := s.HostNamespaceState(context.Background(), "demo")
	if !known {
		t.Fatal("attendu known=true")
	}
	if len(st.Conditions) != 1 {
		t.Fatalf("%d condition(s), attendu 1 : %+v", len(st.Conditions), st.Conditions)
	}
	c := st.Conditions[0]
	if c.Type != corev1.NamespaceConditionType("NamespaceFinalizersRemaining") ||
		c.Status != corev1.ConditionTrue ||
		c.Message != "some finalizers remain: recette.local/bloque" {
		t.Fatalf("condition mal lue : %+v", c)
	}
}

func TestHostNamespaceState_ReadFailureIsUnknown(t *testing.T) {
	readErr := errors.New("etcd is on fire")
	s := NewTestStatusClientWithReactor(func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.GetVerb() == "get" && action.GetResource().Resource == "namespaces" {
			return true, nil, readErr
		}
		return false, nil, nil
	}, testNamespace("demo", nil))

	st, known := s.HostNamespaceState(context.Background(), "demo")
	if known {
		t.Fatal("une lecture qui échoue doit rendre known=false, pas un état affirmé")
	}
	if st.Exists {
		t.Fatal("Exists doit rester false sur une lecture ratée")
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
