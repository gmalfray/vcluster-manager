package service

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"

	"github.com/gmalfray/vcluster-manager/internal/audit"
	"github.com/gmalfray/vcluster-manager/internal/gitops"
	"github.com/gmalfray/vcluster-manager/internal/models"
	"gopkg.in/yaml.v3"
)

// bucketRegex is the DNS-safe charset S3 itself requires for a bucket name.
var bucketRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// validateBucket checks an S3 bucket name. Empty is valid: it means "leave
// this environment's backup storage location alone" (see UpdateVeleroConfig).
func validateBucket(field, value string) error {
	if value == "" {
		return nil
	}
	if !bucketRegex.MatchString(value) {
		return fieldError(field, "nom de bucket invalide (minuscules, chiffres, points, tirets)")
	}
	return nil
}

// validateS3URL checks that a Velero S3 endpoint, when given, is a proper
// http(s) URL.
func validateS3URL(field, value string) error {
	if value == "" {
		return nil
	}
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fieldError(field, "URL invalide (http:// ou https://)")
	}
	return nil
}

// VeleroConfigInput is the platform-wide Velero configuration. TTL is already
// the Go duration string (parseTTLText in the handler), not the short form.
type VeleroConfigInput struct {
	TTL           string
	S3URL         string
	BucketPreprod string
	BucketProd    string
}

// UpdateVeleroConfig saves the global Velero settings and commits the backup
// storage location of each environment to fluxprod. Admin only.
//
// The generator is rebuilt because it bakes the default backup TTL into every
// values.yaml it produces — swapping it here (rather than in the adapter) is
// what keeps vcluster creations and settings updates on the new retention.
func (s *Service) UpdateVeleroConfig(ctx context.Context, actor models.Actor, in VeleroConfigInput) error {
	if !actor.IsAdmin {
		return ErrForbidden
	}
	if err := validateS3URL("velero_s3_url", in.S3URL); err != nil {
		return err
	}
	if err := validateBucket("velero_bucket_preprod", in.BucketPreprod); err != nil {
		return err
	}
	if err := validateBucket("velero_bucket_prod", in.BucketProd); err != nil {
		return err
	}

	s.cfg.SetVeleroConfig(in.TTL, in.S3URL, in.BucketPreprod, in.BucketProd)

	s.generator = gitops.NewGenerator(gitops.GeneratorConfig{
		BaseDomainPreprod:   s.cfg.BaseDomainPreprod,
		BaseDomainProd:      s.cfg.BaseDomainProd,
		TLSSecretPreprod:    s.cfg.TLSSecretPreprod,
		TLSSecretProd:       s.cfg.TLSSecretProd,
		OIDCIssuer:          s.cfg.KeycloakURL + "/auth/realms/" + s.cfg.KeycloakRealm,
		GitLabSSHURL:        s.cfg.GitLabSSHURL,
		GitLabArgoCDPath:    s.cfg.GitLabArgoCDPath,
		DefaultCPU:          s.cfg.DefaultCPU,
		DefaultMemory:       s.cfg.DefaultMemory,
		DefaultStorage:      s.cfg.DefaultStorage,
		VeleroTimezone:      s.cfg.VeleroTimezone,
		VeleroDefaultTTL:    s.cfg.VeleroDefaultTTL,
		VClusterPodSecurity: s.cfg.VClusterPodSecurity,
		ArgoCDDefaultPolicy: s.cfg.ArgoCDDefaultPolicy,
	})

	audit.LogActor(actor.Username, "update-velero-config", "", "global")

	if s.gitlab == nil || (s.cfg.VeleroS3URL == "" && s.cfg.VeleroBucketPreprod == "" && s.cfg.VeleroBucketProd == "") {
		return nil
	}

	var actions []gitops.CommitAction
	for _, env := range []string{"preprod", "prod"} {
		bucket := s.cfg.VeleroBucketPreprod
		if env == "prod" {
			bucket = s.cfg.VeleroBucketProd
		}
		if bucket == "" {
			continue
		}
		path := fmt.Sprintf("%s/%s/velero/values.yaml", s.cfg.FluxprodClustersPath, env)
		// GitLab rejects an "update" on a file that doesn't exist yet.
		action := "update"
		if _, err := s.gitlab.GetFile(ctx, gitops.SourceBranch, path); err != nil {
			action = "create"
		}
		actions = append(actions, gitops.CommitAction{
			Action:  action,
			Path:    path,
			Content: veleroValuesYAML(bucket, s.cfg.VeleroS3URL),
		})
	}
	if len(actions) == 0 {
		return nil
	}

	if err := s.gitlab.Commit(ctx, gitops.SourceBranch, "chore: update Velero BSL configuration", actions); err != nil {
		slog.Error("UpdateVeleroConfig: commit failed", "err", err)
		return &CommitError{Err: err}
	}
	return nil
}

// veleroValuesConfig / veleroValuesLocation / veleroValues mirror the
// values.yaml shape veleroValuesYAML used to build by hand with fmt.Sprintf.
// Marshaling a typed struct instead of interpolating strings means bucket and
// s3URL can never break out of their YAML string — no quote or newline in
// either field can reach the committed file unescaped.
type veleroValuesConfig struct {
	S3URL             string `yaml:"s3Url,omitempty"`
	S3ForcePathStyle  string `yaml:"s3ForcePathStyle"`
	ChecksumAlgorithm string `yaml:"checksumAlgorithm"`
}

type veleroValuesLocation struct {
	Name     string             `yaml:"name"`
	Provider string             `yaml:"provider"`
	Bucket   string             `yaml:"bucket"`
	Config   veleroValuesConfig `yaml:"config"`
}

type veleroValues struct {
	Configuration struct {
		BackupStorageLocation []veleroValuesLocation `yaml:"backupStorageLocation"`
	} `yaml:"configuration"`
}

// veleroValuesYAML renders the Velero backup storage location values for one
// environment.
func veleroValuesYAML(bucket, s3URL string) string {
	var v veleroValues
	v.Configuration.BackupStorageLocation = []veleroValuesLocation{{
		Name:     "default",
		Provider: "aws",
		Bucket:   bucket,
		Config: veleroValuesConfig{
			S3URL:            s3URL,
			S3ForcePathStyle: "true",
		},
	}}
	// Marshal cannot fail on a plain struct of strings — safe to drop the error.
	out, _ := yaml.Marshal(v)
	return string(out)
}
