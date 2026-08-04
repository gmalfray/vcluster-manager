package handlers

import "testing"

func TestValidateQuantity(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"empty means use the default", "", false},
		{"plain integer", "8", false},
		{"millicores", "500m", false},
		{"binary suffix", "32Gi", false},
		{"decimal value", "1.5", false},
		{"decimal with suffix", "1.5Gi", false},
		{"yaml injection via newline", "8\n  evil: true", true},
		{"quote breaks the surrounding string", `8"`, true},
		{"unit not recognized", "8xyz", true},
		{"plain word", "banana", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateQuantity("cpu", tt.value)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateQuantity(%q) error = %v, wantErr %v", tt.value, err, tt.wantErr)
			}
		})
	}
}

func TestValidateVersion(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"empty means use the default", "", false},
		{"semver with v prefix", "v1.28.4", false},
		{"plain semver", "1.28.4", false},
		{"with build metadata", "1.28.4+k3s1", false},
		{"newline injection", "1.28.4\nnewTag: evil", true},
		{"quote injection", `1.28.4"`, true},
		{"spaces", "1.28 .4", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateVersion("k8s_version", tt.value)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateVersion(%q) error = %v, wantErr %v", tt.value, err, tt.wantErr)
			}
		})
	}
}

func TestValidateBucket(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"empty means leave the BSL alone", "", false},
		{"typical bucket name", "vcluster-backups-preprod", false},
		{"with dots", "my.bucket.name", false},
		{"uppercase rejected", "MyBucket", true},
		{"newline injection", "bucket\n      s3Url: http://evil", true},
		{"quote injection", `bucket"`, true},
		{"too short", "a", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateBucket("bucket", tt.value)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateBucket(%q) error = %v, wantErr %v", tt.value, err, tt.wantErr)
			}
		})
	}
}

func TestValidateS3URL(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"empty is valid", "", false},
		{"https", "https://s3.example.com", false},
		{"http", "http://minio.internal:9000", false},
		{"missing scheme", "s3.example.com", true},
		{"ftp not accepted", "ftp://s3.example.com", true},
		{"not a URL at all", "not a url", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateS3URL("s3_url", tt.value)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateS3URL(%q) error = %v, wantErr %v", tt.value, err, tt.wantErr)
			}
		})
	}
}

func TestValidateFluxRepoURL(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"empty is valid", "", false},
		{"ssh:// form", "ssh://git@gitlab.example.com:22226/ops/fluxprod.git", false},
		{"scp-like shorthand", "git@gitlab.example.com:ops/fluxprod.git", false},
		{"shell injection via && ", "ssh://git@host/repo.git && rm -rf /", true},
		{"shell injection via backtick", "ssh://git@host/repo.git`whoami`", true},
		{"newline injection", "ssh://git@host/repo.git\nbranch: evil", true},
		{"http not accepted (SSH only)", "https://gitlab.example.com/ops/fluxprod.git", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateFluxRepoURL("fluxcd_repo_url", tt.value)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateFluxRepoURL(%q) error = %v, wantErr %v", tt.value, err, tt.wantErr)
			}
		})
	}
}

func TestValidateBranchOrPath(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"empty is valid", "", false},
		{"plain branch name", "main", false},
		{"path with slashes", "clusters/preprod", false},
		{"shell injection via semicolon", "main; rm -rf /", true},
		{"shell injection via backtick", "main`whoami`", true},
		{"newline injection", "main\n            value: evil", true},
		{"path traversal", "../../etc/passwd", true},
		{"spaces", "my branch", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateBranchOrPath("fluxcd_branch", tt.value)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateBranchOrPath(%q) error = %v, wantErr %v", tt.value, err, tt.wantErr)
			}
		})
	}
}

func TestValidateVeleroHour(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"empty keeps the current schedule", "", false},
		{"midnight", "00:00", false},
		{"typical backup hour", "03:00", false},
		{"last valid minute", "23:59", false},
		{"hour out of range", "24:00", true},
		{"minute out of range", "12:60", true},
		{"missing leading zero is fine", "3:00", false},
		{"not a time at all", "abc", true},
		{"injection attempt", "03:00\n  evil: true", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateVeleroHour("velero_hour", tt.value)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateVeleroHour(%q) error = %v, wantErr %v", tt.value, err, tt.wantErr)
			}
		})
	}
}

func TestFirstValidationError(t *testing.T) {
	if err := firstValidationError(nil, nil, nil); err != nil {
		t.Errorf("expected nil when every check passes, got %v", err)
	}

	boom := fieldError("cpu", "quantité invalide")
	if err := firstValidationError(nil, boom, fieldError("memory", "aussi invalide")); err != boom {
		t.Errorf("expected the first non-nil error, got %v", err)
	}
}

func TestFieldError_Format(t *testing.T) {
	err := fieldError("cpu", "quantité invalide")
	want := "cpu : quantité invalide"
	if err.Error() != want {
		t.Errorf("fieldError message = %q, want %q", err.Error(), want)
	}
}
