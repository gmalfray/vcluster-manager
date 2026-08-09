package gitops

import (
	"fmt"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/gmalfray/vcluster-manager/internal/models"
)

// The tree a vcluster expands to has two kinds of documents in it, and telling
// them apart is what lets the operator apply the same thing the repo commits.
//
//   - Kubernetes objects: the Flux Kustomizations, and the values ConfigMap that
//     kustomize's configMapGenerator builds out of values.yaml. An API server
//     accepts these as they are.
//   - kustomize plumbing: the kustomization.yaml overlays and the two ArgoCD
//     ConfigMaps used as patch fragments. They are inputs to `kustomize build`,
//     read from Git, and mean nothing to an API server.
//
// vclusterDocs below is the single list both readers walk: GenerateVCluster
// writes the tree to Git, ClusterObjects reports which part of it an API server
// would accept. One list, so the two answers cannot drift.

// vclusterDoc is one document of a vcluster's tree.
type vclusterDoc struct {
	// path is relative to clusters/<env>/vclusters/<name>/.
	path     string
	template string

	// when reports whether the document belongs in the tree for this request.
	// Nil means always.
	when func(*models.CreateRequest) bool

	// object turns the rendered text into the object to apply. Nil marks the
	// document as kustomize plumbing.
	object func(name, rendered string) (*unstructured.Unstructured, error)
}

func withArgoCD(r *models.CreateRequest) bool { return r.ArgoCD }
func withFluxCD(r *models.CreateRequest) bool { return r.FluxCDEnabled }

// vclusterDocs is the tree, in the order it is written.
func vclusterDocs() []vclusterDoc {
	return []vclusterDoc{
		{path: "kustomization.yaml", template: "kustomization.yaml.tmpl"},
		{path: "values.yaml", template: "values.yaml.tmpl", object: valuesConfigMap},
		{path: "tenant_flux.yaml", template: "tenant_flux.yaml.tmpl", object: parseObject},
		{path: "tenant/kustomization.yaml", template: "tenant/kustomization.yaml.tmpl"},
		{path: "tenant/cert-manager_kustomization.yaml", template: "tenant/cert-manager_kustomization.yaml.tmpl", object: parseObject},
		{path: "tenant/cert-manager-config_kustomization.yaml", template: "tenant/cert-manager-config_kustomization.yaml.tmpl", object: parseObject},
		{path: "tenant/vault-webhook_kustomization.yaml", template: "tenant/vault-webhook_kustomization.yaml.tmpl", object: parseObject},
		{path: "tenant/cert-manager/kustomization.yaml", template: "tenant/cert-manager/kustomization.yaml.tmpl"},
		{path: "tenant/vault-webhook/kustomization.yaml", template: "tenant/vault-webhook/kustomization.yaml.tmpl"},

		{path: "tenant/argocd_kustomization.yaml", template: "tenant/argocd_kustomization.yaml.tmpl", when: withArgoCD, object: parseObject},
		{path: "tenant/argocd/kustomization.yaml", template: "tenant/argocd/kustomization.yaml.tmpl", when: withArgoCD},
		{path: "tenant/argocd/argo-cd-cm.yaml", template: "tenant/argocd/argo-cd-cm.yaml.tmpl", when: withArgoCD},
		{path: "tenant/argocd/argocd-rbac-cm.yaml", template: "tenant/argocd/argocd-rbac-cm.yaml.tmpl", when: withArgoCD},
		// Pas d'overlay tenant/navlink/ : bascule sur ./lib partagé + substitution
		// (la démonstration du patron, cf. examples/fluxprod/).
		{path: "tenant/navlink_kustomization.yaml", template: "tenant/navlink_kustomization.yaml.tmpl", when: withArgoCD, object: parseObject},

		{path: "tenant/flux-bootstrap_kustomization.yaml", template: "tenant/flux-bootstrap_kustomization.yaml.tmpl", when: withFluxCD, object: parseObject},
		{path: "tenant/flux-bootstrap/kustomization.yaml", template: "tenant/flux-bootstrap/kustomization.yaml.tmpl", when: withFluxCD},
	}
}

// GenerateVCluster produces all files for a vcluster in a given env.
func (g *Generator) GenerateVCluster(req *models.CreateRequest, env string) []GeneratedFile {
	base := fmt.Sprintf("clusters/%s/vclusters/%s/", env, req.Name)
	data := g.buildData(req.Name, env, req, "")

	var files []GeneratedFile
	for _, doc := range vclusterDocs() {
		if doc.when != nil && !doc.when(req) {
			continue
		}
		files = append(files, GeneratedFile{
			Path:    base + doc.path,
			Content: g.render(doc.template, data),
		})
	}
	return files
}

// ClusterObjects renders the parts of the tree that are Kubernetes objects.
//
// It is the inventory, not what the operator applies. The operator applies two
// objects and no more (see Service.RenderVClusterSubstitutions): applying these
// would put a second writer on objects Flux already owns, without removing a
// single committed file, since most of them are Kustomizations whose `path`
// points back at a per-vcluster overlay in Git.
//
// What it is for: measuring how much of the tree could stop being Git's
// business, which is what tells you whether a template is worth migrating to
// ./lib + substitution. The test on it is the record of that measurement.
//
// k8sVersion is separate from req because CreateRequest has no field for it —
// the same reason buildData takes it apart (it arrives on UpdateRequest).
func (g *Generator) ClusterObjects(req *models.CreateRequest, env, k8sVersion string) ([]*unstructured.Unstructured, error) {
	data := g.buildData(req.Name, env, req, k8sVersion)

	// The host namespace comes first: everything else in the list lives in it.
	// It exists in clusters/<env>/base too, where the root kustomization renames
	// it; applying it here only claims its name, so whatever labels the base
	// carries stay owned by whoever applies the base.
	objects := []*unstructured.Unstructured{hostNamespace(req.Name)}

	for _, doc := range vclusterDocs() {
		if doc.object == nil {
			continue
		}
		if doc.when != nil && !doc.when(req) {
			continue
		}
		obj, err := doc.object(req.Name, g.render(doc.template, data))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", doc.path, err)
		}
		objects = append(objects, obj)
	}
	return objects, nil
}

// Substitutions is everything a vcluster derives from its name, its cell and
// the operator's own configuration, flattened into the envsubst variables Flux
// reads through postBuild.substituteFrom.
//
// It is built from the same buildData the templates render from, so a derived
// value cannot mean one thing in a committed file and another in the ConfigMap.
//
// The key set is the same whatever the spec says: a disabled feature gets empty
// values rather than a missing key, because Flux substitutes what it finds and
// leaves the rest of the placeholder in place — a missing key would ship
// "${ARGOCD_URL}" into a live object instead of an empty string.
func (g *Generator) Substitutions(req *models.CreateRequest, env, k8sVersion string) map[string]string {
	d := g.buildData(req.Name, env, req, k8sVersion)

	subs := map[string]string{
		"VCLUSTER_NAME":              d.Name,
		"VCLUSTER_CELL":              d.Env,
		"VCLUSTER_NAMESPACE":         "vcluster-" + d.Name,
		"VCLUSTER_API_HOST":          d.APIHost,
		"VCLUSTER_DOMAIN":            d.Domain,
		"VCLUSTER_WILDCARD_SECRET":   d.WildcardSecret,
		"VCLUSTER_TLS_SECRET":        d.TLSSecret,
		"VCLUSTER_TARGET_REVISION":   d.TargetRevision,
		"VCLUSTER_POD_SECURITY":      d.PodSecurity,
		"VCLUSTER_K8S_VERSION":       d.K8sVersion,
		"VCLUSTER_KUBECONFIG_SECRET": "vc-vcluster-" + d.Name + "-int",

		// Already spelled this way in the cert-manager-config Kustomization's
		// inline substitute block; same name here so that block becomes a
		// deletion rather than a rename.
		"VAULT_KUBERNETES_AUTH_PATH": "kubernetes-vcluster-" + d.Name + "-" + d.Env,

		"QUOTAS_ENABLED": boolText(!d.NoQuotas),
		"QUOTA_CPU":      d.CPU,
		"QUOTA_MEMORY":   d.Memory,
		"QUOTA_STORAGE":  d.Storage,

		// buildData fills these in whether the feature is on or not, because the
		// templates that read them are only rendered when it is. Here there is no
		// such guard — one ConfigMap carries the whole set — so an off feature has
		// to be blanked explicitly. Leaking a live ArgoCD URL for a vcluster that
		// has no ArgoCD is how a placeholder ends up substituted with something
		// plausible and wrong.
		"VELERO_ENABLED":  boolText(d.VeleroEnabled),
		"VELERO_SCHEDULE": onlyIf(d.VeleroEnabled, d.VeleroSchedule),
		"VELERO_TTL":      onlyIf(d.VeleroEnabled, d.VeleroTTL),

		"ARGOCD_ENABLED":            boolText(d.ArgoCD),
		"ARGOCD_VERSION":            onlyIf(d.ArgoCD, d.ArgoCDVersion),
		"ARGOCD_CLIENT_ID":          onlyIf(d.ArgoCD, d.ArgoCDClientID),
		"ARGOCD_URL":                onlyIf(d.ArgoCD, d.ArgoCDURL),
		"ARGOCD_HOST":               onlyIf(d.ArgoCD, d.ArgoCDHost),
		"ARGOCD_OIDC_ISSUER":        onlyIf(d.ArgoCD, d.OIDCIssuer),
		"ARGOCD_DEFAULT_POLICY":     onlyIf(d.ArgoCD, d.DefaultPolicy),
		"ARGOCD_NAVLINK_LABEL":      onlyIf(d.ArgoCD, d.ArgoCDNavlinkLabel),
		"ARGOCD_APP_MANIFESTS_REPO": onlyIf(d.ArgoCD, d.GitLabSSHBase+"/app-manifests-"+d.Name+".git"),

		// The one multi-line value in the set, and envsubst is a plain string
		// replacement: it only works where the placeholder starts a line whose
		// indentation matches what is generated here. That is the known cost of
		// migrating the ArgoCD overlay; none of the others have this shape.
		"ARGOCD_RBAC_POLICY": onlyIf(d.ArgoCD, d.PolicyLines),

		"FLUXCD_ENABLED":  boolText(d.FluxCD),
		"FLUXCD_REPO_URL": d.FluxCDRepoURL,
		"FLUXCD_BRANCH":   d.FluxCDBranch,
		"FLUXCD_PATH":     d.FluxCDPath,
	}

	return subs
}

// onlyIf keeps a derived value when its feature is on, and blanks it otherwise.
func onlyIf(on bool, value string) string {
	if on {
		return value
	}
	return ""
}

// SubstitutionConfigMap is the single object the operator owns on the host
// cluster: the derived values Flux substitutes into the shared tenant templates.
//
// Sole owner on purpose. Flux never writes it, so there is no field-manager
// contest (crd-vcluster.md §7, inconnue 1), and nothing to prune when a feature
// is turned off (inconnue 2) — the key stays, its value goes empty.
func (g *Generator) SubstitutionConfigMap(req *models.CreateRequest, env, k8sVersion string) *unstructured.Unstructured {
	data := map[string]any{}
	for k, v := range g.Substitutions(req, env, k8sVersion) {
		data[k] = v
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      "vcluster-" + req.Name + "-substitutions",
			"namespace": "vcluster-" + req.Name,
		},
		"data": data,
	}}
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// HostNamespace is the vcluster's namespace on the host cluster.
//
// The caller must have checked the name first: everything here concatenates it
// into a namespace, and a namespace is the only boundary between two tenants.
func HostNamespace(name string) *unstructured.Unstructured {
	return hostNamespace(name)
}

// hostNamespace is the vcluster's namespace on the host cluster.
//
// The label marks a namespace the operator actually manages — one it created
// and will delete itself, as opposed to a same-named namespace left over from
// before the operator existed. Server-Side Apply means the operator owns this
// field the moment it applies it, so it stays put across reconciles instead of
// getting reset by whoever applies the rest of the object.
//
// It exists for the ValidatingAdmissionPolicy in
// deploy/base/operator-admission-policy.yaml, which is the actual boundary on
// the operator's `delete namespaces` right: RBAC only bounds by resource type,
// not by name, on a cluster-scoped resource. The VAP reads oldObject, i.e. the
// namespace as it stood *before* the request being evaluated — never the
// request's own payload — so an operator write cannot both attach the label
// and act on the object in the same call. A namespace only starts passing
// once something outside that write path has labeled it first.
func hostNamespace(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]any{
			"name":   "vcluster-" + name,
			"labels": map[string]any{"vcluster.rebuild-it.fr/managed-namespace": "true"},
		},
	}}
}

// valuesConfigMap wraps values.yaml the way kustomize's configMapGenerator does
// in the committed tree: same name, same key, and no hash suffix, because the
// root kustomization sets disableNameSuffixHash. The name has to stay put across
// reconciles — the HelmRelease references it by name through valuesFrom.
func valuesConfigMap(name, rendered string) (*unstructured.Unstructured, error) {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      "vcluster-" + name + "-values",
			"namespace": "vcluster-" + name,
		},
		"data": map[string]any{"values.yaml": rendered},
	}}, nil
}

// parseObject reads a rendered document as a Kubernetes object.
func parseObject(_, rendered string) (*unstructured.Unstructured, error) {
	obj := &unstructured.Unstructured{Object: map[string]any{}}
	dec := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(rendered), 4096)
	if err := dec.Decode(&obj.Object); err != nil {
		return nil, fmt.Errorf("lecture du document rendu: %w", err)
	}
	if obj.GetKind() == "" || obj.GetAPIVersion() == "" {
		return nil, fmt.Errorf("document sans apiVersion/kind")
	}
	return obj, nil
}

// VeleroTTLFromShort turns the short retention form a human writes in a CR or a
// form ("30j", "12h", "90m") into the Go duration string Velero wants
// ("720h0m0s"). Empty on anything it cannot read.
//
// It lives next to parseVeleroHour because it does the same job on the other
// half of the backup policy: the readable form is what gets reviewed, the raw
// form is what the machine consumes.
func VeleroTTLFromShort(text string) string {
	text = strings.TrimSpace(strings.ToLower(text))
	if text == "" {
		return ""
	}
	unit := text[len(text)-1]
	n, err := strconv.Atoi(text[:len(text)-1])
	if err != nil || n <= 0 {
		return ""
	}
	switch unit {
	case 'j':
		return fmt.Sprintf("%dh0m0s", n*24)
	case 'h':
		return fmt.Sprintf("%dh0m0s", n)
	case 'm':
		return fmt.Sprintf("0h%dm0s", n)
	}
	return ""
}
