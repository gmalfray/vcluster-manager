package service

import (
	"errors"
	"strings"
	"testing"
)

// Le nom `manager` n'est pas interdit par prudence : "vcluster-" + "manager"
// EST le namespace de l'app et de l'opérateur. Sans ce refus, la garde de
// placement l'accepte — ses deux règles coïncident sur ce nom-là — et un backup
// Velero de ce « vcluster » exporte les secrets de l'app dans le bucket S3.
func TestNameThatResolvesToTheOperatorNamespaceIsRefused(t *testing.T) {
	reserve := strings.TrimPrefix(OperatorNamespace, "vcluster-")
	if validName(reserve) {
		t.Fatalf("le nom %q est accepté : son namespace dérivé est %q, celui de l'app", reserve, OperatorNamespace)
	}
	if ValidName(reserve) {
		t.Fatalf("ValidName accepte %q — c'est la porte qu'utilisent les routes HTTP", reserve)
	}
	// Le pendant : la liste ne doit pas mordre sur des noms légitimes voisins.
	for _, ok := range []string{"manager2", "gestionnaire", "man", "managers"} {
		if !validName(ok) {
			t.Errorf("nom légitime %q refusé", ok)
		}
	}
}

// Refuser, pas assainir : une ligne de policy.csv est un droit d'accès, et un
// groupe discrètement écarté donne un accès qui n'existe pas.
func TestRBACGroupsAreRefusedNotSanitised(t *testing.T) {
	hostiles := map[string]string{
		"saut de ligne + sortie du bloc YAML": "team\nkey: injecté",
		"règle policy.csv arbitraire":         "team, role:admin\n    p, role:x, applications, *, */*, allow",
		"virgule (sépare les champs csv)":     "team, role:admin",
		"espace":                              "mon groupe",
		"retour chariot seul":                 "team\rautre",
	}
	for nom, g := range hostiles {
		t.Run(nom, func(t *testing.T) {
			err := ValidateRBACGroups([]string{"legitime", g})
			if err == nil {
				t.Fatalf("groupe hostile accepté : %q", g)
			}
			if !errors.Is(err, ErrInvalidRBACGroup) {
				t.Fatalf("err = %v, attendu ErrInvalidRBACGroup", err)
			}
		})
	}

	// Les formes réelles d'un groupe OIDC doivent passer, sinon le contrôle
	// casse la fonctionnalité qu'il protège.
	legitimes := []string{"team-a", "ops.platform", "role:admin", "svc@example.com", "grp/sous-grp", "a_b"}
	if err := ValidateRBACGroups(legitimes); err != nil {
		t.Fatalf("groupe légitime refusé : %v", err)
	}
}
