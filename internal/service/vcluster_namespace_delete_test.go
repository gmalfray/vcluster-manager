package service

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"

	"github.com/gmalfray/vcluster-manager/internal/kubernetes"
	"github.com/gmalfray/vcluster-manager/internal/models"
)

// Les gardes de DeleteHostNamespace, testées à part du finalizer qui l'appelle.
//
// C'est la seule méthode du service qui supprime un namespace entier, et le
// contrôleur l'appelle en SystemActor — donc admin, toujours. Ce qui reste
// devant elle, c'est le nom : tout ce qui suit le concatène dans un `vcluster-`,
// et ce qui part ici ne revient pas.

// hostNamespaceFor construit le namespace hôte tel que le client dynamique de
// test l'attend.
func hostNamespaceFor(name string) *unstructured.Unstructured {
	ns := &unstructured.Unstructured{}
	ns.SetAPIVersion("v1")
	ns.SetKind("Namespace")
	ns.SetName("vcluster-" + name)
	return ns
}

// terminatingNamespaceFor est le même namespace, déjà condamné : c'est l'état
// dans lequel le finalizer retrouve sa cible à chaque tour après le premier.
func terminatingNamespaceFor(name string) *unstructured.Unstructured {
	ns := hostNamespaceFor(name)
	ns.SetDeletionTimestamp(&metav1.Time{Time: time.Now()})
	return ns
}

func serviceWithCluster(env string, k8s *kubernetes.StatusClient) *Service {
	var mu sync.RWMutex
	return New(Deps{K8sClients: map[string]*kubernetes.StatusClient{env: k8s}, K8sClientsMu: &mu})
}

// captureAudit détourne le logger par défaut, que audit.LogActor utilise, et
// rend le tampon où les lignes atterrissent.
func captureAudit(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

// auditLines rend les lignes d'audit portant cette action.
func auditLines(buf *bytes.Buffer, action string) []string {
	var out []string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, `"action":"`+action+`"`) {
			out = append(out, line)
		}
	}
	return out
}

const nsDeleteAction = "vcluster-namespace-delete"

var operatorActor = models.Actor{Username: "vcluster-operator", IsAdmin: true}

func TestDeleteHostNamespaceRefusesANonAdmin(t *testing.T) {
	s := serviceWithCluster("preprod", kubernetes.NewTestStatusClient(hostNamespaceFor("demo")))

	err := s.DeleteHostNamespace(context.Background(), models.Actor{Username: "bob"}, "demo", "preprod")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("DeleteHostNamespace pour un lecteur = %v, attendu ErrForbidden", err)
	}
}

// Le nom est la dernière barrière avant une concaténation de namespace.
//
// `manager` est le cas qui fait mal : `vcluster-manager` est le namespace de
// l'application elle-même, et validName le réserve précisément pour ça. Sans le
// contrôle, un CR nommé `manager` ferait supprimer par l'opérateur le namespace
// où il tourne.
func TestDeleteHostNamespaceRefusesANameItShouldNotTouch(t *testing.T) {
	ctx := context.Background()
	k8s := kubernetes.NewTestStatusClient(hostNamespaceFor("manager"), hostNamespaceFor("demo"))
	s := serviceWithCluster("preprod", k8s)

	for _, nom := range []string{"manager", "../kube-system", "", "Demo"} {
		if err := s.DeleteHostNamespace(ctx, operatorActor, nom, "preprod"); !errors.Is(err, ErrInvalidName) {
			t.Errorf("DeleteHostNamespace(%q) = %v, attendu ErrInvalidName", nom, err)
		}
	}
	// Et le refus doit être un refus, pas un détour : rien n'a été supprimé. La
	// vérification passe par le client, pas par HostNamespaceState — qui refuse
	// le même nom et rendrait « inconnu » quoi qu'il arrive.
	if st, _ := k8s.HostNamespaceState(ctx, "manager"); !st.Exists {
		t.Fatal("le namespace vcluster-manager n'est plus là : un nom refusé a quand même " +
			"atteint le cluster, et c'est le namespace de l'application qui est parti")
	}
}

// Pas de client pour cette cell : on le dit, on ne fait pas semblant.
func TestDeleteHostNamespaceWithoutAClientSaysSo(t *testing.T) {
	s := newTestService() // aucun client k8s

	err := s.DeleteHostNamespace(context.Background(), operatorActor, "demo", "preprod")
	if !errors.Is(err, ErrK8sUnavailable) {
		t.Fatalf("DeleteHostNamespace sans client = %v, attendu ErrK8sUnavailable : un nil "+
			"rendu ici ferait conclure au finalizer que la demande est partie", err)
	}
}

// Une suppression réelle laisse une ligne d'audit, et une seule.
//
// C'est la seule trace nommée qu'il reste : l'objet qui portait le status
// disparaît juste après, et le namespace supprimé ne se relit pas.
func TestDeleteHostNamespaceAuditsWhatItActuallyDeleted(t *testing.T) {
	ctx := context.Background()
	k8s := kubernetes.NewTestStatusClient(hostNamespaceFor("demo"))
	s := serviceWithCluster("preprod", k8s)
	buf := captureAudit(t)

	if err := s.DeleteHostNamespace(ctx, operatorActor, "demo", "preprod"); err != nil {
		t.Fatalf("suppression : %v", err)
	}

	lignes := auditLines(buf, nsDeleteAction)
	if len(lignes) != 1 {
		t.Fatalf("%d ligne(s) d'audit %s, attendu 1 — journal : %s", len(lignes), nsDeleteAction, buf.String())
	}
	for _, attendu := range []string{`"vcluster":"demo"`, `"env":"preprod"`, `"user":"vcluster-operator"`} {
		if !strings.Contains(lignes[0], attendu) {
			t.Errorf("la ligne d'audit ne porte pas %s : %s", attendu, lignes[0])
		}
	}
	if st := s.HostNamespaceState(ctx, "demo", "preprod"); !st.Known || st.Exists {
		t.Fatalf("namespace toujours là après la suppression (%+v)", st)
	}
}

// Un namespace absent n'est pas une erreur.
//
// Ce n'est PLUS un test d'audit : depuis que DeleteHostNamespace n'a plus de
// Get préalable, elle ne peut plus distinguer « je viens de le condamner » de
// « il n'a jamais existé » sans relire l'objet — exactement le Get que le
// point 3 retire. C'est l'appelant qui porte cette garantie désormais (le
// finalizer n'invoque cette méthode QUE sur un namespace qu'il vient
// d'observer vivant), pas cette méthode prise isolément. Ce qui reste à
// couvrir ici, en défense : un appel direct sur un namespace absent ne doit ni
// planter ni renvoyer une erreur — seule la ligne d'audit n'est plus garantie
// dans ce cas précis, et TestDeleteHostNamespaceAuditsWhatItActuallyDeleted
// couvre déjà le cas qui compte (une suppression réelle en laisse une).
func TestDeleteHostNamespaceOnAnAbsentNamespaceIsNotAnError(t *testing.T) {
	ctx := context.Background()
	s := serviceWithCluster("preprod", kubernetes.NewTestStatusClient())

	if err := s.DeleteHostNamespace(ctx, operatorActor, "jamais-monte", "preprod"); err != nil {
		t.Fatalf("suppression d'un namespace absent : %v", err)
	}
}

// Un env vide vaut preprod, avant le choix du client ET avant la ligne d'audit.
// Une ligne d'audit sans environnement ne dit pas sur quelle cell le namespace
// est parti.
func TestDeleteHostNamespaceDefaultsTheEnvBeforeAuditing(t *testing.T) {
	ctx := context.Background()
	s := serviceWithCluster("preprod", kubernetes.NewTestStatusClient(hostNamespaceFor("demo")))
	buf := captureAudit(t)

	if err := s.DeleteHostNamespace(ctx, operatorActor, "demo", ""); err != nil {
		t.Fatalf("suppression : %v", err)
	}
	lignes := auditLines(buf, nsDeleteAction)
	if len(lignes) != 1 {
		t.Fatalf("%d ligne(s) d'audit, attendu 1 — journal : %s", len(lignes), buf.String())
	}
	if !strings.Contains(lignes[0], `"env":"preprod"`) {
		t.Fatalf("l'environnement n'a pas été normalisé avant l'audit : %s", lignes[0])
	}
}

// Une erreur du cluster remonte telle quelle, et ne se journalise pas comme une
// suppression.
func TestDeleteHostNamespaceSurfacesTheClusterError(t *testing.T) {
	panne := errors.New("etcd is on fire")
	k8s := kubernetes.NewTestStatusClientWithReactor(func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.GetVerb() == "delete" && action.GetResource().Resource == "namespaces" {
			return true, nil, panne
		}
		return false, nil, nil
	}, hostNamespaceFor("demo"))
	s := serviceWithCluster("preprod", k8s)
	buf := captureAudit(t)

	err := s.DeleteHostNamespace(context.Background(), operatorActor, "demo", "preprod")
	if err == nil {
		t.Fatal("suppression refusée par le cluster rendue sans erreur : le finalizer " +
			"enchaînerait sur l'observation comme si la demande était partie")
	}
	if !strings.Contains(err.Error(), panne.Error()) {
		t.Fatalf("l'erreur du cluster a été remplacée par autre chose : %v", err)
	}
	if lignes := auditLines(buf, nsDeleteAction); len(lignes) != 0 {
		t.Fatalf("ligne d'audit écrite malgré l'échec : %v", lignes)
	}
}

// --- HostNamespaceState -------------------------------------------------

// Ce qui distingue désormais un namespace vivant d'un namespace en cours de
// suppression : DeletionTimestamp, un fait posé par l'apiserver, pas une
// condition écrite par l'opérateur. C'est cette propriété que le finalizer
// utilise pour décider s'il y a encore quelque chose à supprimer et, sinon,
// depuis quand attendre.
func TestHostNamespaceStateReportsTheDeletionTimestamp(t *testing.T) {
	ctx := context.Background()
	s := serviceWithCluster("preprod", kubernetes.NewTestStatusClient(terminatingNamespaceFor("tenace")))

	st := s.HostNamespaceState(ctx, "tenace", "preprod")
	if !st.Known || !st.Exists {
		t.Fatalf("attendu known=true, exists=true : %+v", st)
	}
	if st.DeletionTimestamp.IsZero() {
		t.Fatal("un namespace en Terminating doit porter un deletionTimestamp non nul")
	}
}

func TestHostNamespaceStateOnALiveNamespaceHasNoDeletionTimestamp(t *testing.T) {
	ctx := context.Background()
	s := serviceWithCluster("preprod", kubernetes.NewTestStatusClient(hostNamespaceFor("vivant")))

	st := s.HostNamespaceState(ctx, "vivant", "preprod")
	if !st.DeletionTimestamp.IsZero() {
		t.Fatalf("un namespace pas encore condamné ne doit pas porter de deletionTimestamp : %v", st.DeletionTimestamp)
	}
}

// Un nom refusé (garde-fou avant toute lecture cluster) rend Known=false plutôt
// que de laisser croire que le namespace de l'application, par exemple, a été
// consulté pour de vrai.
func TestHostNamespaceStateRefusesAnInvalidName(t *testing.T) {
	ctx := context.Background()
	s := serviceWithCluster("preprod", kubernetes.NewTestStatusClient(hostNamespaceFor("manager")))

	st := s.HostNamespaceState(ctx, "manager", "preprod")
	if st.Known || st.Exists {
		t.Fatalf("attendu l'état zéro sur un nom refusé, eu %+v", st)
	}
}
