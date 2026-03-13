package gitops

import (
	"strings"
	"testing"

	"github.com/gmalfray/vcluster-manager/internal/models"
)

// testConfig returns a GeneratorConfig with realistic but fake values for tests.
func testConfig() GeneratorConfig {
	return GeneratorConfig{
		BaseDomainPreprod:   "preprod.example.com",
		BaseDomainProd:      "example.com",
		TLSSecretPreprod:    "wildcard-preprod-example-com-tls",
		TLSSecretProd:       "wildcard-example-com-tls",
		OIDCIssuer:          "https://keycloak.example.com/auth/realms/myrealm",
		GitLabSSHURL:        "ssh://git@gitlab.example.com:22226",
		GitLabArgoCDPath:    "ops/argocd",
		DefaultCPU:          "8",
		DefaultMemory:       "32Gi",
		DefaultStorage:      "500Gi",
		VeleroTimezone:      "Europe/Paris",
		VClusterPodSecurity: "privileged",
		ArgoCDDefaultPolicy: "role:readonly",
	}
}

// --- parseVeleroHour ---

func TestParseVeleroHour(t *testing.T) {
	tests := []struct {
		input      string
		wantHour   int
		wantMinute int
	}{
		{"02:30", 2, 30},
		{"00:00", 0, 0},
		{"23:59", 23, 59},
		{"10:05", 10, 5},
		{"", 0, 0},
		{"invalid", 0, 0},
	}
	for _, tt := range tests {
		h, m := parseVeleroHour(tt.input)
		if h != tt.wantHour || m != tt.wantMinute {
			t.Errorf("parseVeleroHour(%q) = (%d, %d), want (%d, %d)", tt.input, h, m, tt.wantHour, tt.wantMinute)
		}
	}
}

// --- UpdateKustomization ---

func TestUpdateKustomization_Add(t *testing.T) {
	existing := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ./vclusters/platform
  - ./vclusters/demos
`
	result := UpdateKustomization(existing, "myvc", true)
	if !strings.Contains(result, "  - ./vclusters/myvc") {
		t.Error("expected entry to be added")
	}
	// Should appear after last vclusters entry
	demoIdx := strings.Index(result, "./vclusters/demos")
	myvcIdx := strings.Index(result, "./vclusters/myvc")
	if myvcIdx <= demoIdx {
		t.Error("new entry should appear after the last existing vcluster entry")
	}
}

func TestUpdateKustomization_AddNoDuplicate(t *testing.T) {
	existing := `resources:
  - ./vclusters/platform
  - ./vclusters/demos
`
	result := UpdateKustomization(existing, "demos", true)
	count := strings.Count(result, "./vclusters/demos")
	if count != 1 {
		t.Errorf("expected exactly 1 occurrence of demos, got %d", count)
	}
}

func TestUpdateKustomization_Remove(t *testing.T) {
	existing := `resources:
  - ./vclusters/platform
  - ./vclusters/demos
  - ./vclusters/delivery
`
	result := UpdateKustomization(existing, "demos", false)
	if strings.Contains(result, "./vclusters/demos") {
		t.Error("expected demos entry to be removed")
	}
	if !strings.Contains(result, "./vclusters/platform") {
		t.Error("platform entry should remain")
	}
	if !strings.Contains(result, "./vclusters/delivery") {
		t.Error("delivery entry should remain")
	}
}

func TestUpdateKustomization_RemoveNonExistent(t *testing.T) {
	existing := `resources:
  - ./vclusters/platform
`
	result := UpdateKustomization(existing, "nothere", false)
	if result != existing {
		t.Error("removing a non-existent entry should not change the content")
	}
}

// --- GenerateVCluster: file count ---

func TestGenerateVCluster_FileCountWithoutArgoCD(t *testing.T) {
	g := NewGenerator(testConfig())
	req := &models.CreateRequest{
		Name:          "myvc",
		ArgoCD:        false,
		VeleroEnabled: true,
		VeleroHour:    "03:00",
		CPU:           "4",
		Memory:        "16Gi",
		Storage:       "200Gi",
	}
	files := g.GenerateVCluster(req, "preprod")
	// Base: kustomization, values, tenant_flux, tenant/kustomization,
	//       tenant/cert-manager_kustomization, tenant/cert-manager-config_kustomization,
	//       tenant/vault-webhook_kustomization, tenant/cert-manager/kustomization,
	//       tenant/vault-webhook/kustomization
	const wantCount = 9
	if len(files) != wantCount {
		t.Errorf("expected %d files without ArgoCD, got %d", wantCount, len(files))
	}
}

func TestGenerateVCluster_FileCountWithArgoCD(t *testing.T) {
	g := NewGenerator(testConfig())
	req := &models.CreateRequest{
		Name:   "myvc",
		ArgoCD: true,
	}
	files := g.GenerateVCluster(req, "preprod")
	// Base 9 + argocd_kustomization, argocd/kustomization, argocd/argo-cd-cm,
	//          argocd/argocd-rbac-cm, navlink_kustomization, navlink/kustomization
	const wantCount = 15
	if len(files) != wantCount {
		t.Errorf("expected %d files with ArgoCD, got %d", wantCount, len(files))
	}
}

func TestGenerateVCluster_FileCountWithFluxCD(t *testing.T) {
	g := NewGenerator(testConfig())
	req := &models.CreateRequest{
		Name:          "myvc",
		FluxCDEnabled: true,
		FluxCDRepoURL: "ssh://git@gitlab.example.com:22226/ops/myrepo.git",
		FluxCDBranch:  "master",
		FluxCDPath:    "clusters/pra2",
	}
	files := g.GenerateVCluster(req, "preprod")
	// Base 9 + flux-bootstrap_kustomization, flux-bootstrap/kustomization
	const wantCount = 11
	if len(files) != wantCount {
		t.Errorf("expected %d files with FluxCD, got %d", wantCount, len(files))
	}
}

func TestGenerateVCluster_FileCountWithArgoCDAndFluxCD(t *testing.T) {
	g := NewGenerator(testConfig())
	req := &models.CreateRequest{
		Name:          "myvc",
		ArgoCD:        true,
		FluxCDEnabled: true,
		FluxCDRepoURL: "ssh://git@gitlab.example.com:22226/ops/myrepo.git",
		FluxCDBranch:  "master",
		FluxCDPath:    "clusters/pra2",
	}
	files := g.GenerateVCluster(req, "preprod")
	// Base 9 + 6 ArgoCD + 2 FluxCD
	const wantCount = 17
	if len(files) != wantCount {
		t.Errorf("expected %d files with ArgoCD+FluxCD, got %d", wantCount, len(files))
	}
}

// --- GenerateVCluster: file paths ---

func TestGenerateVCluster_FilePaths(t *testing.T) {
	g := NewGenerator(testConfig())
	req := &models.CreateRequest{Name: "myvc", ArgoCD: true}
	files := g.GenerateVCluster(req, "preprod")

	expected := []string{
		"clusters/preprod/vclusters/myvc/kustomization.yaml",
		"clusters/preprod/vclusters/myvc/values.yaml",
		"clusters/preprod/vclusters/myvc/tenant_flux.yaml",
		"clusters/preprod/vclusters/myvc/tenant/kustomization.yaml",
		"clusters/preprod/vclusters/myvc/tenant/cert-manager_kustomization.yaml",
		"clusters/preprod/vclusters/myvc/tenant/cert-manager-config_kustomization.yaml",
		"clusters/preprod/vclusters/myvc/tenant/vault-webhook_kustomization.yaml",
		"clusters/preprod/vclusters/myvc/tenant/cert-manager/kustomization.yaml",
		"clusters/preprod/vclusters/myvc/tenant/vault-webhook/kustomization.yaml",
		"clusters/preprod/vclusters/myvc/tenant/argocd_kustomization.yaml",
		"clusters/preprod/vclusters/myvc/tenant/argocd/kustomization.yaml",
		"clusters/preprod/vclusters/myvc/tenant/argocd/argo-cd-cm.yaml",
		"clusters/preprod/vclusters/myvc/tenant/argocd/argocd-rbac-cm.yaml",
		"clusters/preprod/vclusters/myvc/tenant/navlink_kustomization.yaml",
		"clusters/preprod/vclusters/myvc/tenant/navlink/kustomization.yaml",
	}

	byPath := make(map[string]string, len(files))
	for _, f := range files {
		byPath[f.Path] = f.Content
	}
	for _, p := range expected {
		if _, ok := byPath[p]; !ok {
			t.Errorf("missing expected file: %s", p)
		}
	}
}

// --- buildData: derived field computation ---

func TestBuildData_PreprodDomains(t *testing.T) {
	g := NewGenerator(testConfig())
	req := &models.CreateRequest{Name: "myvc", ArgoCD: true}
	data := g.buildData("myvc", "preprod", req, "")

	if data.APIHost != "myvc.api.preprod.example.com" {
		t.Errorf("APIHost = %q, want %q", data.APIHost, "myvc.api.preprod.example.com")
	}
	if data.Domain != "myvc.preprod.example.com" {
		t.Errorf("Domain = %q, want %q", data.Domain, "myvc.preprod.example.com")
	}
	if data.WildcardSecret != "wildcard-myvc-preprod-example-com-tls" {
		t.Errorf("WildcardSecret = %q, want %q", data.WildcardSecret, "wildcard-myvc-preprod-example-com-tls")
	}
	if data.ArgoCDClientID != "argocd-k8s-myvc-preprod" {
		t.Errorf("ArgoCDClientID = %q, want %q", data.ArgoCDClientID, "argocd-k8s-myvc-preprod")
	}
	if data.TargetRevision != "preprod" {
		t.Errorf("TargetRevision = %q, want preprod", data.TargetRevision)
	}
	if data.TLSSecret != "wildcard-preprod-example-com-tls" {
		t.Errorf("TLSSecret = %q, want %q", data.TLSSecret, "wildcard-preprod-example-com-tls")
	}
}

func TestBuildData_ProdDomains(t *testing.T) {
	g := NewGenerator(testConfig())
	req := &models.CreateRequest{Name: "myvc", ArgoCD: true}
	data := g.buildData("myvc", "prod", req, "")

	if data.APIHost != "myvc.api.example.com" {
		t.Errorf("APIHost = %q, want %q", data.APIHost, "myvc.api.example.com")
	}
	if data.ArgoCDClientID != "argocd-k8s-myvc" {
		t.Errorf("ArgoCDClientID = %q, want %q", data.ArgoCDClientID, "argocd-k8s-myvc")
	}
	if data.TargetRevision != "master" {
		t.Errorf("TargetRevision = %q, want master", data.TargetRevision)
	}
	if data.TLSSecret != "wildcard-example-com-tls" {
		t.Errorf("TLSSecret = %q, want %q", data.TLSSecret, "wildcard-example-com-tls")
	}
}

func TestBuildData_DefaultQuotas(t *testing.T) {
	g := NewGenerator(testConfig())
	req := &models.CreateRequest{Name: "myvc"} // no CPU/Memory/Storage set
	data := g.buildData("myvc", "preprod", req, "")

	if data.CPU != "8" {
		t.Errorf("CPU = %q, want default 8", data.CPU)
	}
	if data.Memory != "32Gi" {
		t.Errorf("Memory = %q, want default 32Gi", data.Memory)
	}
	if data.Storage != "500Gi" {
		t.Errorf("Storage = %q, want default 500Gi", data.Storage)
	}
}

func TestBuildData_CustomQuotas(t *testing.T) {
	g := NewGenerator(testConfig())
	req := &models.CreateRequest{Name: "myvc", CPU: "2", Memory: "8Gi", Storage: "100Gi"}
	data := g.buildData("myvc", "preprod", req, "")

	if data.CPU != "2" {
		t.Errorf("CPU = %q, want 2", data.CPU)
	}
	if data.Memory != "8Gi" {
		t.Errorf("Memory = %q, want 8Gi", data.Memory)
	}
	if data.Storage != "100Gi" {
		t.Errorf("Storage = %q, want 100Gi", data.Storage)
	}
}

func TestBuildData_VeleroSchedule(t *testing.T) {
	g := NewGenerator(testConfig())
	req := &models.CreateRequest{Name: "myvc", VeleroEnabled: true, VeleroHour: "02:30"}
	data := g.buildData("myvc", "preprod", req, "")

	want := "CRON_TZ=Europe/Paris 30 2 * * *"
	if data.VeleroSchedule != want {
		t.Errorf("VeleroSchedule = %q, want %q", data.VeleroSchedule, want)
	}
}

func TestBuildData_VeleroDisabled(t *testing.T) {
	g := NewGenerator(testConfig())
	req := &models.CreateRequest{Name: "myvc", VeleroEnabled: false}
	data := g.buildData("myvc", "preprod", req, "")

	if data.VeleroSchedule != "" {
		t.Errorf("VeleroSchedule should be empty when disabled, got %q", data.VeleroSchedule)
	}
}

func TestBuildData_RBACGroups(t *testing.T) {
	g := NewGenerator(testConfig())
	req := &models.CreateRequest{
		Name:       "myvc",
		RBACGroups: []string{"team-alpha", "team-beta", "  trimmed  "},
	}
	data := g.buildData("myvc", "preprod", req, "")

	if !strings.Contains(data.PolicyLines, "g, team-alpha, role:admin") {
		t.Error("PolicyLines should contain team-alpha")
	}
	if !strings.Contains(data.PolicyLines, "g, team-beta, role:admin") {
		t.Error("PolicyLines should contain team-beta")
	}
	if !strings.Contains(data.PolicyLines, "g, trimmed, role:admin") {
		t.Error("PolicyLines should contain trimmed group (with spaces stripped)")
	}
}

func TestBuildData_GitLabSSHBase(t *testing.T) {
	g := NewGenerator(testConfig())
	req := &models.CreateRequest{Name: "myvc"}
	data := g.buildData("myvc", "preprod", req, "")

	want := "ssh://git@gitlab.example.com:22226/ops/argocd"
	if data.GitLabSSHBase != want {
		t.Errorf("GitLabSSHBase = %q, want %q", data.GitLabSSHBase, want)
	}
}

// --- Template content validation ---

func TestGenerateVCluster_ValuesContainsName(t *testing.T) {
	g := NewGenerator(testConfig())
	req := &models.CreateRequest{Name: "myvc", VeleroEnabled: true, VeleroHour: "03:00"}
	files := g.GenerateVCluster(req, "preprod")

	var values string
	for _, f := range files {
		if strings.HasSuffix(f.Path, "/values.yaml") {
			values = f.Content
			break
		}
	}
	if values == "" {
		t.Fatal("values.yaml not found in generated files")
	}
	if !strings.Contains(values, "vcluster-myvc") {
		t.Error("values.yaml should reference vcluster-myvc")
	}
	if !strings.Contains(values, "myvc.api.preprod.example.com") {
		t.Error("values.yaml should contain the API host")
	}
	if !strings.Contains(values, "CRON_TZ=Europe/Paris") {
		t.Error("values.yaml should contain velero schedule")
	}
}

func TestGenerateVCluster_ValuesVeleroDisabled(t *testing.T) {
	g := NewGenerator(testConfig())
	req := &models.CreateRequest{Name: "myvc", VeleroEnabled: false}
	files := g.GenerateVCluster(req, "preprod")

	var values string
	for _, f := range files {
		if strings.HasSuffix(f.Path, "/values.yaml") {
			values = f.Content
			break
		}
	}
	if !strings.Contains(values, "enabled: false") {
		t.Error("values.yaml should have veleroBackup enabled: false")
	}
	if strings.Contains(values, "schedule:") {
		t.Error("values.yaml should not contain schedule when velero is disabled")
	}
}

func TestGenerateVCluster_ValuesNoQuotas(t *testing.T) {
	g := NewGenerator(testConfig())
	req := &models.CreateRequest{Name: "myvc", NoQuotas: true}
	files := g.GenerateVCluster(req, "preprod")

	var values string
	for _, f := range files {
		if strings.HasSuffix(f.Path, "/values.yaml") {
			values = f.Content
			break
		}
	}
	if !strings.Contains(values, "resourceQuota:\n      enabled: false") {
		t.Error("values.yaml should disable resourceQuota when NoQuotas=true")
	}
}

func TestGenerateVCluster_ArgoCDVersionPin(t *testing.T) {
	g := NewGenerator(testConfig())
	req := &models.CreateRequest{Name: "myvc", ArgoCD: true, ArgoCDVersion: "v2.9.3"}
	files := g.GenerateVCluster(req, "preprod")

	var kustContent string
	for _, f := range files {
		if f.Path == "clusters/preprod/vclusters/myvc/tenant/argocd/kustomization.yaml" {
			kustContent = f.Content
			break
		}
	}
	if kustContent == "" {
		t.Fatal("argocd kustomization.yaml not found")
	}
	if !strings.Contains(kustContent, "newTag: v2.9.3") {
		t.Errorf("argocd kustomization should pin version v2.9.3, got:\n%s", kustContent)
	}
}

func TestGenerateVCluster_ArgoCDNoVersionPin(t *testing.T) {
	g := NewGenerator(testConfig())
	req := &models.CreateRequest{Name: "myvc", ArgoCD: true, ArgoCDVersion: ""}
	files := g.GenerateVCluster(req, "preprod")

	var kustContent string
	for _, f := range files {
		if f.Path == "clusters/preprod/vclusters/myvc/tenant/argocd/kustomization.yaml" {
			kustContent = f.Content
			break
		}
	}
	if strings.Contains(kustContent, "images:") {
		t.Error("argocd kustomization should not contain images: section when ArgoCDVersion is empty")
	}
}

func TestGenerateVCluster_ArgoCDPathPreprod(t *testing.T) {
	g := NewGenerator(testConfig())
	req := &models.CreateRequest{Name: "myvc", ArgoCD: true}
	files := g.GenerateVCluster(req, "preprod")

	var kustContent string
	for _, f := range files {
		if f.Path == "clusters/preprod/vclusters/myvc/tenant/argocd/kustomization.yaml" {
			kustContent = f.Content
			break
		}
	}
	if kustContent == "" {
		t.Fatal("argocd kustomization.yaml not found")
	}
	if !strings.Contains(kustContent, "value: preprod") {
		t.Errorf("argocd kustomization should set path to 'preprod', got:\n%s", kustContent)
	}
	if !strings.Contains(kustContent, "value: master") {
		t.Errorf("argocd kustomization should set targetRevision to 'master', got:\n%s", kustContent)
	}
}

func TestGenerateVCluster_ArgoCDPathProd(t *testing.T) {
	g := NewGenerator(testConfig())
	req := &models.CreateRequest{Name: "myvc", ArgoCD: true}
	files := g.GenerateVCluster(req, "prod")

	var kustContent string
	for _, f := range files {
		if f.Path == "clusters/prod/vclusters/myvc/tenant/argocd/kustomization.yaml" {
			kustContent = f.Content
			break
		}
	}
	if kustContent == "" {
		t.Fatal("argocd kustomization.yaml not found")
	}
	if !strings.Contains(kustContent, "value: prod") {
		t.Errorf("argocd kustomization should set path to 'prod', got:\n%s", kustContent)
	}
	if !strings.Contains(kustContent, "value: master") {
		t.Errorf("argocd kustomization should set targetRevision to 'master', got:\n%s", kustContent)
	}
}

func TestGenerateVCluster_K8sVersionInValues(t *testing.T) {
	g := NewGenerator(testConfig())
	req := &models.CreateRequest{Name: "myvc"}
	// K8s version is passed via buildData's k8sVersion param (used by GenerateUpdatedValues)
	// GenerateVCluster always passes "" for k8sVersion
	files := g.GenerateVCluster(req, "preprod")

	var values string
	for _, f := range files {
		if strings.HasSuffix(f.Path, "/values.yaml") {
			values = f.Content
			break
		}
	}
	if strings.Contains(values, "distro:") {
		t.Error("values.yaml should not contain distro section when k8sVersion is empty")
	}
}

func TestGenerateUpdatedValues_WithK8sVersion(t *testing.T) {
	g := NewGenerator(testConfig())
	req := &models.UpdateRequest{K8sVersion: "1.31.0"}
	f := g.GenerateUpdatedValues("myvc", "preprod", req)

	if !strings.Contains(f.Content, "version: \"1.31.0\"") {
		t.Errorf("updated values should contain k8s version, got:\n%s", f.Content)
	}
}
