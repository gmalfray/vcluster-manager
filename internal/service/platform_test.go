package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gmalfray/vcluster-manager/internal/models"
)

func TestUpdateVeleroConfig_ForbiddenForNonAdmin(t *testing.T) {
	s := newTestService()
	err := s.UpdateVeleroConfig(context.Background(), models.Actor{Username: "bob", IsAdmin: false}, VeleroConfigInput{})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden for non-admin, got %v", err)
	}
}

// TestUpdateVeleroConfig_RejectsInjectionInBucket checks the same
// anti-injection guard as UpdateSettings' field validation, on the field this
// domain owns: the bucket name lands unescaped in a values.yaml committed to
// fluxprod (see veleroValuesYAML — it marshals a typed struct exactly to
// avoid this, but the field must never reach it in the first place with a
// broken-out value).
func TestUpdateVeleroConfig_RejectsInjectionInBucket(t *testing.T) {
	s := newTestService()
	err := s.UpdateVeleroConfig(context.Background(), models.Actor{Username: "alice", IsAdmin: true}, VeleroConfigInput{
		BucketPreprod: "bucket\n      s3Url: http://evil",
	})
	if err == nil || !strings.Contains(err.Error(), "velero_bucket_preprod :") {
		t.Fatalf("expected a velero_bucket_preprod validation error, got %v", err)
	}
}

func TestUpdateVeleroConfig_RejectsQuoteInBucket(t *testing.T) {
	s := newTestService()
	err := s.UpdateVeleroConfig(context.Background(), models.Actor{Username: "alice", IsAdmin: true}, VeleroConfigInput{
		BucketProd: `bucket"`,
	})
	if err == nil || !strings.Contains(err.Error(), "velero_bucket_prod :") {
		t.Fatalf("expected a velero_bucket_prod validation error, got %v", err)
	}
}

func TestUpdateVeleroConfig_RejectsInvalidS3URL(t *testing.T) {
	s := newTestService()
	err := s.UpdateVeleroConfig(context.Background(), models.Actor{Username: "alice", IsAdmin: true}, VeleroConfigInput{
		S3URL: "not a url",
	})
	if err == nil || !strings.Contains(err.Error(), "velero_s3_url :") {
		t.Fatalf("expected a velero_s3_url validation error, got %v", err)
	}
}

func TestVeleroValuesYAML(t *testing.T) {
	out := veleroValuesYAML("my-bucket", "https://s3.example.com")
	if !strings.Contains(out, "bucket: my-bucket") {
		t.Errorf("expected bucket in output, got %q", out)
	}
	if !strings.Contains(out, "s3Url: https://s3.example.com") {
		t.Errorf("expected s3Url in output, got %q", out)
	}

	// A newline in the bucket must not be able to inject a sibling YAML key —
	// yaml.Marshal quotes/escapes it instead of splicing it in raw. This is
	// the same property validateBucket enforces earlier; here it's the
	// second, independent line of defense (defense in depth on the render
	// side, in case a bucket ever reaches this function unvalidated).
	injected := veleroValuesYAML("bucket\nevil: true", "")
	if strings.Contains(injected, "\nevil: true\n") {
		t.Errorf("bucket value escaped out of its YAML string: %q", injected)
	}
}
