package handlers

import (
	"fmt"
	"net/url"
	"regexp"
)

// These fields end up interpolated into YAML (and, for FluxCDRepoURL/Branch/Path,
// into a shell command) committed to fluxprod through internal/gitops/generator.go,
// which uses text/template — unlike html/template, it does not escape anything. A
// quote or a newline in one of them breaks the surrounding YAML string, injects a
// sibling key, or — for the flux-bootstrap fields — runs as part of the "flux
// bootstrap git ..." command inside the manifest. Restricting the accepted
// charset up front is what keeps the templates safe without turning
// generator.go into a YAML marshaller for every field.
var (
	bucketRegex   = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	quantityRegex = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(m|k|M|G|T|P|Ki|Mi|Gi|Ti|Pi)?$`)
	versionRegex  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,62}$`)
	// FluxCDRepoURL is documented as an SSH git URL in the UI ("URL du repo Git
	// (SSH)"), either the full ssh:// form or the scp-like shorthand.
	fluxRepoSSHRegex = regexp.MustCompile(`^ssh://[A-Za-z0-9_.-]+@[A-Za-z0-9_.-]+(:[0-9]+)?/[A-Za-z0-9_./-]+$`)
	fluxRepoSCPRegex = regexp.MustCompile(`^[A-Za-z0-9_.-]+@[A-Za-z0-9_.-]+:[A-Za-z0-9_./-]+$`)
	branchPathRegex  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,254}$`)
	veleroHourRegex  = regexp.MustCompile(`^([01]?[0-9]|2[0-3]):[0-5][0-9]$`)
)

// fieldError formats a rejected-field message the same way everywhere, so the
// toast reads "cpu : quantité Kubernetes invalide (...)".
func fieldError(field, reason string) error {
	return fmt.Errorf("%s : %s", field, reason)
}

// firstValidationError returns the first non-nil error in errs, or nil if
// every field passed. Lets a handler run a batch of field validations and
// report just the first failure, same as the sequential if-chain it replaces.
func firstValidationError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// validateBucket checks an S3 bucket name against the DNS-safe charset S3
// itself requires. Empty is valid: it means "leave this environment's BSL
// alone" (see UpdateVeleroConfig).
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

// validateQuantity checks a Kubernetes resource quantity (CPU/memory/storage).
// Empty is valid: it means "use the configured default".
func validateQuantity(field, value string) error {
	if value == "" {
		return nil
	}
	if !quantityRegex.MatchString(value) {
		return fieldError(field, "quantité Kubernetes invalide (ex: 8, 500m, 32Gi)")
	}
	return nil
}

// validateVersion checks a K8s/ArgoCD version or tag string.
func validateVersion(field, value string) error {
	if value == "" {
		return nil
	}
	if !versionRegex.MatchString(value) {
		return fieldError(field, "version invalide")
	}
	return nil
}

// validateFluxRepoURL checks a FluxCD bootstrap repo URL (SSH form only, see
// the regexes above).
func validateFluxRepoURL(field, value string) error {
	if value == "" {
		return nil
	}
	if !fluxRepoSSHRegex.MatchString(value) && !fluxRepoSCPRegex.MatchString(value) {
		return fieldError(field, "URL SSH invalide (ex: ssh://git@host:port/chemin.git)")
	}
	return nil
}

// validateBranchOrPath checks a FluxCD branch or path: no newlines, no shell
// or YAML metacharacters, nothing a path-traversal attempt would need.
func validateBranchOrPath(field, value string) error {
	if value == "" {
		return nil
	}
	if !branchPathRegex.MatchString(value) {
		return fieldError(field, "caractères non autorisés")
	}
	return nil
}

// validateVeleroHour checks the "HH:MM" backup time. Empty is valid: Velero
// stays on its current schedule.
func validateVeleroHour(field, value string) error {
	if value == "" {
		return nil
	}
	if !veleroHourRegex.MatchString(value) {
		return fieldError(field, "format attendu HH:MM (ex: 03:00)")
	}
	return nil
}
