/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package webhook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	configbutleraiv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

// GitTargetValidator validates GitTarget resources to prevent duplicates.
type GitTargetValidator struct {
	Client client.Client
}

// SetupGitTargetValidatorWebhook registers the GitTarget validator webhook with the manager.
func SetupGitTargetValidatorWebhook(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &configbutleraiv1alpha1.GitTarget{}).
		WithValidator(&GitTargetValidator{Client: mgr.GetClient()}).
		Complete()
}

// ValidateCreate validates creation of a GitTarget.

func (v *GitTargetValidator) ValidateCreate(
	ctx context.Context,
	target *configbutleraiv1alpha1.GitTarget,
) (admission.Warnings, error) {
	log := logf.FromContext(ctx).WithName("GitTargetValidator")
	if target == nil {
		return nil, errors.New("GitTarget cannot be nil")
	}

	log.Info("Validating GitTarget creation",
		"name", target.Name,
		"namespace", target.Namespace,
		"providerRef", target.Spec.ProviderRef.Name,
		"branch", target.Spec.Branch,
		"path", target.Spec.Path)

	return v.validateUniqueness(ctx, target, nil)
}

// ValidateUpdate validates update of a GitTarget.
func (v *GitTargetValidator) ValidateUpdate(
	ctx context.Context,
	oldTarget, newTarget *configbutleraiv1alpha1.GitTarget,
) (admission.Warnings, error) {
	log := logf.FromContext(ctx).WithName("GitTargetValidator")
	if oldTarget == nil || newTarget == nil {
		return nil, errors.New("GitTarget cannot be nil")
	}

	log.Info("Validating GitTarget update",
		"name", newTarget.Name,
		"namespace", newTarget.Namespace,
		"providerRef", newTarget.Spec.ProviderRef.Name,
		"branch", newTarget.Spec.Branch,
		"path", newTarget.Spec.Path)

	return v.validateUniqueness(ctx, newTarget, oldTarget)
}

// ValidateDelete validates deletion of a GitTarget (always allowed).
func (v *GitTargetValidator) ValidateDelete(
	_ context.Context,
	target *configbutleraiv1alpha1.GitTarget,
) (admission.Warnings, error) {
	if target == nil {
		return nil, errors.New("GitTarget cannot be nil")
	}
	// Deletion is always allowed
	return nil, nil
}

// validateUniqueness checks if the GitTarget conflicts with existing ones.
// oldTarget is provided for updates to exclude the current resource from conflict checking.
func (v *GitTargetValidator) validateUniqueness(
	ctx context.Context,
	target *configbutleraiv1alpha1.GitTarget,
	oldTarget *configbutleraiv1alpha1.GitTarget,
) (admission.Warnings, error) {
	log := logf.FromContext(ctx).WithName("validateUniqueness")

	// 1. Resolve Provider to get the actual repository URL
	repoURL, err := v.getRepoURL(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve provider '%s/%s': %w",
			target.Namespace, // Provider is always in same namespace
			target.Spec.ProviderRef.Name,
			err)
	}

	// 2. Create identifier for this target
	normalizedURL := normalizeRepoURL(repoURL)
	targetIdentifier := createTargetIdentifier(normalizedURL, target.Spec.Branch, target.Spec.Path)

	log.V(1).Info("Created target identifier",
		"repoURL", normalizedURL,
		"branch", target.Spec.Branch,
		"path", target.Spec.Path,
		"identifier", targetIdentifier)

	// 3. List all GitTargets in the cluster
	var allTargets configbutleraiv1alpha1.GitTargetList
	if err := v.Client.List(ctx, &allTargets); err != nil {
		return nil, fmt.Errorf("failed to list GitTargets: %w", err)
	}

	// 4. Check each target for conflicts
	for i := range allTargets.Items {
		existing := &allTargets.Items[i]

		// Skip self (same namespace and name)
		if existing.Namespace == target.Namespace && existing.Name == target.Name {
			continue
		}

		// For updates, skip the old version if checking against ourselves
		if oldTarget != nil && existing.Namespace == oldTarget.Namespace && existing.Name == oldTarget.Name {
			continue
		}

		// Resolve the existing target's Provider
		existingRepoURL, err := v.getRepoURL(ctx, existing)
		if err != nil {
			log.V(1).Info("Skipping target with unresolvable Provider",
				"target", fmt.Sprintf("%s/%s", existing.Namespace, existing.Name),
				"error", err.Error())
			continue
		}

		// Create identifier for existing target
		existingNormalizedURL := normalizeRepoURL(existingRepoURL)
		existingIdentifier := createTargetIdentifier(
			existingNormalizedURL,
			existing.Spec.Branch,
			existing.Spec.Path,
		)

		// Check for conflict
		if existingIdentifier == targetIdentifier {
			return nil, fmt.Errorf(
				"GitTarget conflict detected - another target already uses this location:\n"+
					"  Repository: %s\n"+
					"  Branch: %s\n"+
					"  Path: %s\n"+
					"  Conflicting Resource: %s/%s\n\n"+
					"Suggestion: Use a different path (e.g., '%s/%s') to avoid conflicts",
				normalizedURL,
				target.Spec.Branch,
				target.Spec.Path,
				existing.Namespace,
				existing.Name,
				target.Spec.Path,
				target.Namespace,
			)
		}
	}

	log.Info("GitTarget uniqueness validation passed",
		"name", target.Name,
		"namespace", target.Namespace,
		"identifier", targetIdentifier)

	return nil, nil
}

// getRepoURL retrieves the URL from the referenced Provider (GitProvider or Flux GitRepository).
func (v *GitTargetValidator) getRepoURL(
	ctx context.Context,
	target *configbutleraiv1alpha1.GitTarget,
) (string, error) {
	providerRef := target.Spec.ProviderRef
	namespace := target.Namespace // Provider must be in same namespace

	// Default Kind to GitProvider if not specified
	kind := providerRef.Kind
	if kind == "" {
		kind = "GitProvider"
	}

	switch kind {
	case "GitProvider":
		return v.getGitProviderURL(ctx, namespace, providerRef.Name)
	case "GitRepository":
		return v.getFluxGitRepositoryURL(ctx, namespace, providerRef.Name)
	default:
		return "", fmt.Errorf("unsupported provider kind: %s", kind)
	}
}

func (v *GitTargetValidator) getGitProviderURL(ctx context.Context, namespace, name string) (string, error) {
	var provider configbutleraiv1alpha1.GitProvider
	key := types.NamespacedName{Namespace: namespace, Name: name}
	if err := v.Client.Get(ctx, key, &provider); err != nil {
		return "", err
	}
	return provider.Spec.URL, nil
}

func (v *GitTargetValidator) getFluxGitRepositoryURL(ctx context.Context, namespace, name string) (string, error) {
	// Handle Flux GitRepository using unstructured
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "source.toolkit.fluxcd.io",
		Version: "v1", // Assuming v1, but could be v1beta1 or v1beta2
		Kind:    "GitRepository",
	})
	key := types.NamespacedName{Namespace: namespace, Name: name}
	if err := v.Client.Get(ctx, key, u); err != nil {
		return "", err
	}

	url, found, err := unstructured.NestedString(u.Object, "spec", "url")
	if err != nil {
		return "", fmt.Errorf("failed to read spec.url from GitRepository: %w", err)
	}
	if !found {
		return "", errors.New("spec.url not found in GitRepository")
	}
	return url, nil
}

// normalizeRepoURL normalizes a Git repository URL for comparison.
// Handles: .git suffix, trailing slashes, http/https/ssh protocols.
func normalizeRepoURL(rawURL string) string {
	// Remove trailing slash first (before .git)
	rawURL = strings.TrimSuffix(rawURL, "/")

	// Remove trailing .git if present
	rawURL = strings.TrimSuffix(rawURL, ".git")

	// Parse URL to normalize
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		// If parsing fails, just clean up basic things
		return strings.ToLower(rawURL)
	}

	// Normalize scheme to lowercase
	parsedURL.Scheme = strings.ToLower(parsedURL.Scheme)

	// Normalize host to lowercase
	parsedURL.Host = strings.ToLower(parsedURL.Host)

	// Normalize path to lowercase
	parsedURL.Path = strings.ToLower(parsedURL.Path)

	return parsedURL.String()
}

// createTargetIdentifier creates a unique identifier for a target.
// Uses SHA256 hash of: normalized_repo_url + branch + path.
func createTargetIdentifier(normalizedRepoURL, branch, path string) string {
	// Create deterministic string
	data := fmt.Sprintf("%s:%s:%s", normalizedRepoURL, branch, path)

	// Hash it for consistent identifier
	hash := sha256.Sum256([]byte(data))
	return hex.EncodeToString(hash[:])
}
