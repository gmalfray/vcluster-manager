package gitops

import (
	"fmt"
	"testing"
)

// --- fakeFileProvider ---

// fakeFileProvider implements fileProvider with an in-memory map.
// Keys are "branch:path". Missing entries return an error.
type fakeFileProvider struct {
	files map[string]string   // "branch:path" -> content
	dirs  map[string][]string // "branch:path" -> list of file paths under that prefix
}

func newFakeProvider() *fakeFileProvider {
	return &fakeFileProvider{
		files: map[string]string{},
		dirs:  map[string][]string{},
	}
}

func (f *fakeFileProvider) set(branch, path, content string) {
	f.files[branch+":"+path] = content
}

// addDir registers file paths returned by ListFiles for a given branch+path prefix.
func (f *fakeFileProvider) addDir(branch, path string, filePaths []string) {
	f.dirs[branch+":"+path] = filePaths
}

func (f *fakeFileProvider) GetFile(branch, path string) (string, error) {
	if v, ok := f.files[branch+":"+path]; ok {
		return v, nil
	}
	return "", fmt.Errorf("file not found: %s on %s", path, branch)
}

func (f *fakeFileProvider) ListFiles(branch, path string) ([]string, error) {
	if v, ok := f.dirs[branch+":"+path]; ok {
		return v, nil
	}
	return nil, fmt.Errorf("dir not found: %s on %s", path, branch)
}

// newTestParser creates a Parser backed by the fake provider.
func newTestParser(fp *fakeFileProvider) *Parser {
	return &Parser{gitlab: fp, branch: "preprod"}
}

// --- parseVeleroCron ---

func TestParseVeleroCron(t *testing.T) {
	tests := []struct {
		schedule   string
		wantHour   int
		wantMinute int
	}{
		{"CRON_TZ=Europe/Paris 30 2 * * *", 2, 30},
		{"0 3 * * *", 3, 0},
		{"15 22 * * *", 22, 15},
		{"", 0, 0},
		{"invalid", 0, 0},
	}
	for _, tt := range tests {
		h, m := parseVeleroCron(tt.schedule)
		if h != tt.wantHour || m != tt.wantMinute {
			t.Errorf("parseVeleroCron(%q) = (%d, %d), want (%d, %d)", tt.schedule, h, m, tt.wantHour, tt.wantMinute)
		}
	}
}

// --- listDirs ---

func TestListDirs(t *testing.T) {
	fp := newFakeProvider()
	fp.addDir("preprod", "clusters/preprod/vclusters", []string{
		"clusters/preprod/vclusters/platform/kustomization.yaml",
		"clusters/preprod/vclusters/platform/values.yaml",
		"clusters/preprod/vclusters/demos/kustomization.yaml",
		"clusters/preprod/vclusters/demos/tenant/kustomization.yaml",
	})
	p := newTestParser(fp)

	dirs, err := p.listDirs("clusters/preprod/vclusters")
	if err != nil {
		t.Fatalf("listDirs error: %v", err)
	}
	if len(dirs) != 2 {
		t.Errorf("expected 2 dirs, got %d: %v", len(dirs), dirs)
	}
	// Check both names are present (order may vary)
	found := map[string]bool{}
	for _, d := range dirs {
		found[d] = true
	}
	if !found["platform"] || !found["demos"] {
		t.Errorf("expected platform and demos, got %v", dirs)
	}
}

func TestListDirs_NoDuplicates(t *testing.T) {
	fp := newFakeProvider()
	fp.addDir("preprod", "clusters/preprod/vclusters", []string{
		"clusters/preprod/vclusters/myvc/kustomization.yaml",
		"clusters/preprod/vclusters/myvc/values.yaml",
		"clusters/preprod/vclusters/myvc/tenant/kustomization.yaml",
	})
	p := newTestParser(fp)

	dirs, err := p.listDirs("clusters/preprod/vclusters")
	if err != nil {
		t.Fatalf("listDirs error: %v", err)
	}
	if len(dirs) != 1 || dirs[0] != "myvc" {
		t.Errorf("expected [myvc], got %v", dirs)
	}
}

// --- parseValues ---

func valuesYAML(extra string) string {
	return `veleroBackup:
  enabled: true
  schedule: "CRON_TZ=Europe/Paris 30 2 * * *"
vcluster:
  controlPlane:
    ingress:
      host: "myvc.api.preprod.example.com"
  policies:
    resourceQuota:
      quota:
        requests.cpu: "4"
        requests.memory: 16Gi
        requests.storage: "200Gi"
` + extra
}

func TestParseValues_Velero(t *testing.T) {
	fp := newFakeProvider()
	fp.set("preprod", "clusters/preprod/vclusters/myvc/values.yaml", valuesYAML(""))
	p := newTestParser(fp)

	vc, err := p.parseVClusterEnv("preprod", "myvc")
	if err != nil {
		t.Fatalf("parseVClusterEnv error: %v", err)
	}
	if !vc.Velero.Enabled {
		t.Error("expected Velero.Enabled = true")
	}
	if vc.Velero.Hour != 2 || vc.Velero.Minute != 30 {
		t.Errorf("expected velero 02:30, got %02d:%02d", vc.Velero.Hour, vc.Velero.Minute)
	}
}

func TestParseValues_VeleroDisabled(t *testing.T) {
	content := `veleroBackup:
  enabled: false
vcluster:
  policies:
    resourceQuota:
      quota:
        requests.cpu: "4"
        requests.memory: 8Gi
        requests.storage: "100Gi"
`
	fp := newFakeProvider()
	fp.set("preprod", "clusters/preprod/vclusters/myvc/values.yaml", content)
	p := newTestParser(fp)

	vc, err := p.parseVClusterEnv("preprod", "myvc")
	if err != nil {
		t.Fatalf("parseVClusterEnv error: %v", err)
	}
	if vc.Velero.Enabled {
		t.Error("expected Velero.Enabled = false")
	}
	if vc.Velero.Hour != 0 || vc.Velero.Minute != 0 {
		t.Errorf("expected velero 00:00, got %02d:%02d", vc.Velero.Hour, vc.Velero.Minute)
	}
}

func TestParseValues_Quotas(t *testing.T) {
	fp := newFakeProvider()
	fp.set("preprod", "clusters/preprod/vclusters/myvc/values.yaml", valuesYAML(""))
	p := newTestParser(fp)

	vc, err := p.parseVClusterEnv("preprod", "myvc")
	if err != nil {
		t.Fatalf("parseVClusterEnv error: %v", err)
	}
	if vc.NoQuotas {
		t.Error("expected NoQuotas = false")
	}
	if vc.Quotas.CPU != "4" {
		t.Errorf("CPU = %q, want 4", vc.Quotas.CPU)
	}
	if vc.Quotas.Memory != "16Gi" {
		t.Errorf("Memory = %q, want 16Gi", vc.Quotas.Memory)
	}
	if vc.Quotas.Storage != "200Gi" {
		t.Errorf("Storage = %q, want 200Gi", vc.Quotas.Storage)
	}
}

func TestParseValues_NoQuotas(t *testing.T) {
	content := `veleroBackup:
  enabled: false
vcluster:
  policies:
    resourceQuota:
      enabled: false
    limitRange:
      enabled: false
`
	fp := newFakeProvider()
	fp.set("preprod", "clusters/preprod/vclusters/myvc/values.yaml", content)
	p := newTestParser(fp)

	vc, err := p.parseVClusterEnv("preprod", "myvc")
	if err != nil {
		t.Fatalf("parseVClusterEnv error: %v", err)
	}
	if !vc.NoQuotas {
		t.Error("expected NoQuotas = true")
	}
}

func TestParseValues_K8sVersion(t *testing.T) {
	content := `veleroBackup:
  enabled: false
vcluster:
  controlPlane:
    distro:
      k8s:
        version: "1.31.0"
  policies:
    resourceQuota:
      quota:
        requests.cpu: "4"
        requests.memory: 8Gi
        requests.storage: "100Gi"
`
	fp := newFakeProvider()
	fp.set("preprod", "clusters/preprod/vclusters/myvc/values.yaml", content)
	p := newTestParser(fp)

	vc, err := p.parseVClusterEnv("preprod", "myvc")
	if err != nil {
		t.Fatalf("parseVClusterEnv error: %v", err)
	}
	if vc.K8sVersionConfig != "1.31.0" {
		t.Errorf("K8sVersionConfig = %q, want 1.31.0", vc.K8sVersionConfig)
	}
}

func TestParseValues_FluxCD(t *testing.T) {
	content := `veleroBackup:
  enabled: false
vcluster:
  policies:
    resourceQuota:
      quota:
        requests.cpu: "4"
        requests.memory: 8Gi
        requests.storage: "100Gi"
fluxcd:
  enabled: true
  repoURL: "ssh://git@gitlab.example.com:22226/ops/myrepo.git"
  branch: "master"
  path: "clusters/pra2"
`
	fp := newFakeProvider()
	fp.set("preprod", "clusters/preprod/vclusters/myvc/values.yaml", content)
	p := newTestParser(fp)

	vc, err := p.parseVClusterEnv("preprod", "myvc")
	if err != nil {
		t.Fatalf("parseVClusterEnv error: %v", err)
	}
	if !vc.FluxCD.Enabled {
		t.Error("expected FluxCD.Enabled = true")
	}
	if vc.FluxCD.RepoURL != "ssh://git@gitlab.example.com:22226/ops/myrepo.git" {
		t.Errorf("FluxCD.RepoURL = %q", vc.FluxCD.RepoURL)
	}
	if vc.FluxCD.Branch != "master" {
		t.Errorf("FluxCD.Branch = %q, want master", vc.FluxCD.Branch)
	}
	if vc.FluxCD.Path != "clusters/pra2" {
		t.Errorf("FluxCD.Path = %q, want clusters/pra2", vc.FluxCD.Path)
	}
}

// --- parseRBACGroups ---

func rbacCM(policyCSV string) string {
	return "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: argocd-rbac-cm\ndata:\n  policy.csv: |\n" +
		indentLines(policyCSV, "    ")
}

func indentLines(s, prefix string) string {
	lines := ""
	for _, l := range splitLines(s) {
		lines += prefix + l + "\n"
	}
	return lines
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, c := range s {
		if c == '\n' {
			out = append(out, cur)
			cur = ""
		} else {
			cur += string(c)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func TestParseRBACGroups(t *testing.T) {
	policyCSV := "g, team-alpha, role:admin\ng, team-beta, role:admin\np, role:readonly, applications, get, */*, allow\n"
	fp := newFakeProvider()
	fp.set("preprod", "clusters/preprod/vclusters/myvc/values.yaml", valuesYAML(""))
	fp.set("preprod", "clusters/preprod/vclusters/myvc/tenant/argocd/argocd-rbac-cm.yaml", rbacCM(policyCSV))
	// ArgoCD detection: pathExists checks via ListFiles
	fp.addDir("preprod", "clusters/preprod/vclusters/myvc/tenant/argocd", []string{
		"clusters/preprod/vclusters/myvc/tenant/argocd/kustomization.yaml",
		"clusters/preprod/vclusters/myvc/tenant/argocd/argocd-rbac-cm.yaml",
	})
	fp.set("preprod", "clusters/preprod/vclusters/myvc/tenant/argocd/kustomization.yaml", "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: []\n")

	p := newTestParser(fp)
	vc, err := p.parseVClusterEnv("preprod", "myvc")
	if err != nil {
		t.Fatalf("parseVClusterEnv error: %v", err)
	}

	if !vc.ArgoCD {
		t.Error("expected ArgoCD = true")
	}
	if len(vc.RBACGroups) != 2 {
		t.Errorf("expected 2 RBAC groups, got %d: %v", len(vc.RBACGroups), vc.RBACGroups)
	}
	found := map[string]bool{}
	for _, g := range vc.RBACGroups {
		found[g] = true
	}
	if !found["team-alpha"] || !found["team-beta"] {
		t.Errorf("expected team-alpha and team-beta in %v", vc.RBACGroups)
	}
}

func TestParseRBACGroups_Empty(t *testing.T) {
	fp := newFakeProvider()
	fp.set("preprod", "clusters/preprod/vclusters/myvc/values.yaml", valuesYAML(""))
	fp.set("preprod", "clusters/preprod/vclusters/myvc/tenant/argocd/argocd-rbac-cm.yaml",
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: argocd-rbac-cm\ndata:\n  policy.csv: |\n    p, role:readonly, applications, get, */*, allow\n")
	fp.addDir("preprod", "clusters/preprod/vclusters/myvc/tenant/argocd", []string{
		"clusters/preprod/vclusters/myvc/tenant/argocd/kustomization.yaml",
	})
	fp.set("preprod", "clusters/preprod/vclusters/myvc/tenant/argocd/kustomization.yaml", "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: []\n")

	p := newTestParser(fp)
	vc, err := p.parseVClusterEnv("preprod", "myvc")
	if err != nil {
		t.Fatalf("parseVClusterEnv error: %v", err)
	}
	if len(vc.RBACGroups) != 0 {
		t.Errorf("expected no RBAC groups, got %v", vc.RBACGroups)
	}
}

// --- parseArgoCDVersion ---

func argoCDKustomization(version string) string {
	if version == "" {
		return "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - ../../base\n"
	}
	return `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../../base
images:
  - name: quay.io/argoproj/argocd
    newTag: ` + version + "\n"
}

func TestParseArgoCDVersion_WithPin(t *testing.T) {
	fp := newFakeProvider()
	fp.set("preprod", "clusters/preprod/vclusters/myvc/values.yaml", valuesYAML(""))
	fp.set("preprod", "clusters/preprod/vclusters/myvc/tenant/argocd/kustomization.yaml", argoCDKustomization("v2.9.3"))
	fp.set("preprod", "clusters/preprod/vclusters/myvc/tenant/argocd/argocd-rbac-cm.yaml",
		"apiVersion: v1\nkind: ConfigMap\ndata:\n  policy.csv: \"\"\n")
	fp.addDir("preprod", "clusters/preprod/vclusters/myvc/tenant/argocd", []string{
		"clusters/preprod/vclusters/myvc/tenant/argocd/kustomization.yaml",
	})

	p := newTestParser(fp)
	vc, err := p.parseVClusterEnv("preprod", "myvc")
	if err != nil {
		t.Fatalf("parseVClusterEnv error: %v", err)
	}
	if vc.ArgoCDVersion != "v2.9.3" {
		t.Errorf("ArgoCDVersion = %q, want v2.9.3", vc.ArgoCDVersion)
	}
}

func TestParseArgoCDVersion_NoPin(t *testing.T) {
	fp := newFakeProvider()
	fp.set("preprod", "clusters/preprod/vclusters/myvc/values.yaml", valuesYAML(""))
	fp.set("preprod", "clusters/preprod/vclusters/myvc/tenant/argocd/kustomization.yaml", argoCDKustomization(""))
	fp.set("preprod", "clusters/preprod/vclusters/myvc/tenant/argocd/argocd-rbac-cm.yaml",
		"apiVersion: v1\nkind: ConfigMap\ndata:\n  policy.csv: \"\"\n")
	fp.addDir("preprod", "clusters/preprod/vclusters/myvc/tenant/argocd", []string{
		"clusters/preprod/vclusters/myvc/tenant/argocd/kustomization.yaml",
	})

	p := newTestParser(fp)
	vc, err := p.parseVClusterEnv("preprod", "myvc")
	if err != nil {
		t.Fatalf("parseVClusterEnv error: %v", err)
	}
	if vc.ArgoCDVersion != "" {
		t.Errorf("ArgoCDVersion = %q, want empty (no pin)", vc.ArgoCDVersion)
	}
}

// --- parseVClusterEnv: ArgoCD detection ---

func TestParseVClusterEnv_ArgoCDDisabled(t *testing.T) {
	fp := newFakeProvider()
	fp.set("preprod", "clusters/preprod/vclusters/myvc/values.yaml", valuesYAML(""))
	// No argocd dir → ArgoCD = false
	// pathExists calls ListFiles; return error to signal "not found"
	p := newTestParser(fp)

	vc, err := p.parseVClusterEnv("preprod", "myvc")
	if err != nil {
		t.Fatalf("parseVClusterEnv error: %v", err)
	}
	if vc.ArgoCD {
		t.Error("expected ArgoCD = false when argocd dir does not exist")
	}
}

func TestParseVClusterEnv_NameAndEnv(t *testing.T) {
	fp := newFakeProvider()
	fp.set("preprod", "clusters/preprod/vclusters/myvc/values.yaml", valuesYAML(""))
	p := newTestParser(fp)

	vc, err := p.parseVClusterEnv("preprod", "myvc")
	if err != nil {
		t.Fatalf("parseVClusterEnv error: %v", err)
	}
	if vc.Name != "myvc" {
		t.Errorf("Name = %q, want myvc", vc.Name)
	}
	if vc.Env != "preprod" {
		t.Errorf("Env = %q, want preprod", vc.Env)
	}
}

// --- ListVClusters ---

func TestListVClusters(t *testing.T) {
	fp := newFakeProvider()
	// vclusters directory listing
	fp.addDir("preprod", "clusters/preprod/vclusters", []string{
		"clusters/preprod/vclusters/platform/kustomization.yaml",
		"clusters/preprod/vclusters/platform/values.yaml",
		"clusters/preprod/vclusters/demos/kustomization.yaml",
		"clusters/preprod/vclusters/demos/values.yaml",
	})
	// values.yaml for each
	fp.set("preprod", "clusters/preprod/vclusters/platform/values.yaml", valuesYAML(""))
	fp.set("preprod", "clusters/preprod/vclusters/demos/values.yaml", valuesYAML(""))

	p := newTestParser(fp)
	vclusters, err := p.ListVClusters("preprod")
	if err != nil {
		t.Fatalf("ListVClusters error: %v", err)
	}
	if len(vclusters) != 2 {
		t.Errorf("expected 2 vclusters, got %d", len(vclusters))
	}
}

func TestListVClusters_SkipsParseError(t *testing.T) {
	fp := newFakeProvider()
	fp.addDir("preprod", "clusters/preprod/vclusters", []string{
		"clusters/preprod/vclusters/good/values.yaml",
		"clusters/preprod/vclusters/bad/values.yaml",
	})
	fp.set("preprod", "clusters/preprod/vclusters/good/values.yaml", valuesYAML(""))
	fp.set("preprod", "clusters/preprod/vclusters/bad/values.yaml", "not: valid: yaml: [")

	p := newTestParser(fp)
	// Should return only "good" and log a warning for "bad"
	vclusters, err := p.ListVClusters("preprod")
	if err != nil {
		t.Fatalf("ListVClusters error: %v", err)
	}
	if len(vclusters) != 1 {
		t.Errorf("expected 1 vcluster (bad one skipped), got %d", len(vclusters))
	}
	if vclusters[0].Name != "good" {
		t.Errorf("expected vcluster 'good', got %q", vclusters[0].Name)
	}
}
