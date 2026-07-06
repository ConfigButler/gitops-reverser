// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"strings"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
)

// coreSecretsTypeKey is the placement byType key for core Kubernetes Secrets — the
// one resource type that is always sensitive (types.SensitiveResourcePolicy), and
// therefore the only sensitive type this static, spec-only gate can name without
// the operator's runtime sensitive-resource configuration. Additional configured
// sensitive types are caught by the write-time co-mingle guards instead.
const coreSecretsTypeKey = "v1/secrets"

// validatePlacementPolicy statically validates a GitTarget's declared placement
// policy (F4, Option B2:
// docs/design/manifest/version2/gittarget-new-file-placement-rules.md) against the
// spec alone — no repository scan is needed, so this runs as part of the Validated
// gate, the same spec-well-formedness check that already covers provider/branch
// resolution and path-overlap conflicts. A nil spec (no declared policy) is always
// valid.
//
// B2 has one placement map for every resource, so sensitivity is not a separate
// block to validate; it is a write-safety property enforced at write time. What
// this gate still owns is the one part of that property it can prove statically:
// core Secrets (always sensitive) must never be routed to a path that could collide
// two of them onto one file.
func validatePlacementPolicy(spec *configbutleraiv1alpha3.GitTargetPlacementSpec) (bool, string) {
	if spec == nil {
		return true, ""
	}
	for key, tmpl := range spec.ByType {
		if !validPlacementTypeKeySyntax(key) {
			return false, fmt.Sprintf(
				"placement byType key %q is not a valid \"[group/]version/resource\" type key", key,
			)
		}
		if msg, bad := validatePlacementTemplate(tmpl); bad {
			return false, fmt.Sprintf("placement byType[%q]: %s", key, msg)
		}
	}
	if strings.TrimSpace(spec.Default) != "" {
		if msg, bad := validatePlacementTemplate(spec.Default); bad {
			return false, fmt.Sprintf("placement default: %s", msg)
		}
	}
	return validateSecretSafety(spec)
}

// validateSecretSafety enforces, statically, that core Secrets can never be placed
// where two of them would collide onto one file:
//   - an explicit byType["v1/secrets"] route must be identity-complete; and
//   - a bundling default (one that is not itself identity-complete, e.g. "all.yaml")
//     is rejected unless such an explicit, identity-complete Secret route exists, so
//     a Secret with no byType entry can never fall through to the bundle.
//
// Additional operator-configured sensitive types are not knowable here; the
// write-time guards (finishPlacement, createNew) keep those safe.
func validateSecretSafety(spec *configbutleraiv1alpha3.GitTargetPlacementSpec) (bool, string) {
	secretTmpl := strings.TrimSpace(spec.ByType[coreSecretsTypeKey])
	secretRouteComplete := secretTmpl != "" &&
		manifestanalyzer.IdentityCompletePlacementTemplate(secretTmpl, true)

	if secretTmpl != "" && !secretRouteComplete {
		return false, fmt.Sprintf(
			"placement byType[%q] %q must be identity-complete (include {name} and "+
				"{namespace}/{namespaceOrCluster}) because Secrets are sensitive and must never share a file",
			coreSecretsTypeKey, secretTmpl,
		)
	}

	def := strings.TrimSpace(spec.Default)
	if def == "" || manifestanalyzer.IdentityCompletePlacementTemplate(def, false) {
		return true, ""
	}
	if !secretRouteComplete {
		return false, fmt.Sprintf(
			"placement default %q is a bundling path that could place a Secret in a shared file; add an "+
				"identity-complete byType[%q] entry so sensitive resources are routed to their own files",
			def, coreSecretsTypeKey,
		)
	}
	return true, ""
}

// validatePlacementTemplate checks one template string against the two purely
// structural rules every placement template must satisfy: its variables are all
// known (ValidPlacementTemplateSyntax) and its literal text cannot escape the
// GitTarget's spec.path or carry the wrong file suffix (ValidPlacementTemplatePath).
// Secret-specific identity-completeness is handled once, centrally, in
// validateSecretSafety.
func validatePlacementTemplate(tmpl string) (string, bool) {
	if err := manifestanalyzer.ValidPlacementTemplateSyntax(tmpl); err != nil {
		return err.Error(), true
	}
	if err := manifestanalyzer.ValidPlacementTemplatePath(tmpl); err != nil {
		return err.Error(), true
	}
	return "", false
}

// validPlacementTypeKeySyntax reports whether key has the shape
// "[group/]version/resource" — two or three non-empty, slash-separated segments.
// It checks syntax only; whether the type is actually served/watched is a
// repository-independent question this static gate cannot answer.
func validPlacementTypeKeySyntax(key string) bool {
	parts := strings.Split(key, "/")
	if len(parts) != 2 && len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if strings.TrimSpace(p) == "" {
			return false
		}
	}
	return true
}
