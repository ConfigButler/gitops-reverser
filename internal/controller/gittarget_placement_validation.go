// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"strings"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
)

// validatePlacementPolicy statically validates a GitTarget's declared placement
// policy (F4: docs/design/manifest/version2/gittarget-new-file-placement-rules.md)
// against the spec alone — no repository scan is needed, so this runs as part of
// the Validated gate, the same spec-well-formedness check that already covers
// provider/branch resolution and path-overlap conflicts. A nil spec (no declared
// policy) is always valid.
func validatePlacementPolicy(spec *configbutleraiv1alpha3.GitTargetPlacementSpec) (bool, string) {
	if spec == nil {
		return true, ""
	}
	if msg, bad := validatePlacementClass(spec.Sensitive, true); bad {
		return false, msg
	}
	normal := configbutleraiv1alpha3.GitTargetPlacementClass{ByType: spec.ByType, Default: spec.Default}
	if msg, bad := validatePlacementClass(normal, false); bad {
		return false, msg
	}
	return true, ""
}

func validatePlacementClass(class configbutleraiv1alpha3.GitTargetPlacementClass, sensitive bool) (string, bool) {
	for key, tmpl := range class.ByType {
		if !validPlacementTypeKeySyntax(key) {
			return fmt.Sprintf(
				"placement byType key %q is not a valid \"[group/]version/resource\" type key", key,
			), true
		}
		if msg, bad := validatePlacementTemplate(tmpl, true, sensitive); bad {
			return fmt.Sprintf("placement byType[%q]: %s", key, msg), true
		}
	}
	if strings.TrimSpace(class.Default) != "" {
		if msg, bad := validatePlacementTemplate(class.Default, false, sensitive); bad {
			return fmt.Sprintf("placement default: %s", msg), true
		}
	}
	return "", false
}

// validatePlacementTemplate checks one template string: its variables are all
// known (ValidPlacementTemplateSyntax), and — for a sensitive template only — that
// it renders a SOPS path and is identity-complete, the structural guarantee
// "Sensitive placement and uniqueness" in the design doc requires. narrowedToOneType
// is true for a ByType entry, whose map key already names one exact type.
func validatePlacementTemplate(tmpl string, narrowedToOneType, sensitive bool) (string, bool) {
	if err := manifestanalyzer.ValidPlacementTemplateSyntax(tmpl); err != nil {
		return err.Error(), true
	}
	if err := manifestanalyzer.ValidPlacementTemplatePath(tmpl); err != nil {
		return err.Error(), true
	}
	if !sensitive {
		return "", false
	}
	if !strings.HasSuffix(tmpl, ".sops.yaml") && !strings.HasSuffix(tmpl, ".sops.yml") {
		return fmt.Sprintf("sensitive template %q must render a .sops.yaml or .sops.yml path", tmpl), true
	}
	if !manifestanalyzer.IdentityCompletePlacementTemplate(tmpl, narrowedToOneType) {
		return fmt.Sprintf(
			"sensitive template %q is not identity-complete: it must include {name} and "+
				"{namespace}/{namespaceOrCluster} (a default template must also include "+
				"{groupPath}/{version}/{resource})", tmpl,
		), true
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
