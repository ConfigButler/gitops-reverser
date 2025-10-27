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
	"fmt"
	"net/url"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	configbutleraiv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

// GitDestinationValidator validates GitDestination resources to prevent duplicates.
type GitDestinationValidator struct {
	Client  client.Client
	Decoder *admission.Decoder
}

// SetupGitDestinationValidatorWebhook registers the GitDestination validator webhook with the manager.
func SetupGitDestinationValidatorWebhook(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&configbutleraiv1alpha1.GitDestination{}).
		WithValidator(&GitDestinationValidator{Client: mgr.GetClient()}).
		Complete()
}

// ValidateCreate validates creation of a GitDestination.
func (v *GitDestinationValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	log := logf.FromContext(ctx).WithName("GitDestinationValidator")

	dest, ok := obj.(*configbutleraiv1alpha1.GitDestination)
	if !ok {
		return nil, fmt.Errorf("expected GitDestination but got %T", obj)
	}

	log.Info("Validating GitDestination creation",
		"name", dest.Name,
		"namespace", dest.Namespace,
		"repoRef", dest.Spec.RepoRef.Name,
		"branch", dest.Spec.Branch,
		"baseFolder", dest.Spec.BaseFolder)

	return v.validateUniqueness(ctx, dest, nil)
}

// ValidateUpdate validates update of a GitDestination.
func (v *GitDestinationValidator) ValidateUpdate(
	ctx context.Context,
	oldObj, newObj runtime.Object,
) (admission.Warnings, error) {
	log := logf.FromContext(ctx).WithName("GitDestinationValidator")

	newDest, ok := newObj.(*configbutleraiv1alpha1.GitDestination)
	if !ok {
		return nil, fmt.Errorf("expected GitDestination but got %T", newObj)
	}

	oldDest, ok := oldObj.(*configbutleraiv1alpha1.GitDestination)
	if !ok {
		return nil, fmt.Errorf("expected GitDestination but got %T", oldObj)
	}

	log.Info("Validating GitDestination update",
		"name", newDest.Name,
		"namespace", newDest.Namespace,
		"repoRef", newDest.Spec.RepoRef.Name,
		"branch", newDest.Spec.Branch,
		"baseFolder", newDest.Spec.BaseFolder)

	return v.validateUniqueness(ctx, newDest, oldDest)
}

// ValidateDelete validates deletion of a GitDestination (always allowed).
func (v *GitDestinationValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	// Deletion is always allowed
	return nil, nil
}

// validateUniqueness checks if the GitDestination conflicts with existing ones.
// oldDest is provided for updates to exclude the current resource from conflict checking.
func (v *GitDestinationValidator) validateUniqueness(
	ctx context.Context,
	dest *configbutleraiv1alpha1.GitDestination,
	oldDest *configbutleraiv1alpha1.GitDestination,
) (admission.Warnings, error) {
	log := logf.FromContext(ctx).WithName("validateUniqueness")

	// 1. Resolve GitRepoConfig to get the actual repository URL
	repoConfig, err := v.getGitRepoConfig(ctx, dest)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve GitRepoConfig '%s/%s': %w",
			v.resolveNamespace(dest.Spec.RepoRef.Namespace, dest.Namespace),
			dest.Spec.RepoRef.Name,
			err)
	}

	// 2. Create identifier for this destination
	normalizedURL := normalizeRepoURL(repoConfig.Spec.RepoURL)
	destIdentifier := createDestinationIdentifier(normalizedURL, dest.Spec.Branch, dest.Spec.BaseFolder)

	log.V(1).Info("Created destination identifier",
		"repoURL", normalizedURL,
		"branch", dest.Spec.Branch,
		"baseFolder", dest.Spec.BaseFolder,
		"identifier", destIdentifier)

	// 3. List all GitDestinations in the cluster
	var allDestinations configbutleraiv1alpha1.GitDestinationList
	if err := v.Client.List(ctx, &allDestinations); err != nil {
		return nil, fmt.Errorf("failed to list GitDestinations: %w", err)
	}

	// 4. Check each destination for conflicts
	for i := range allDestinations.Items {
		existing := &allDestinations.Items[i]

		// Skip self (same namespace and name)
		if existing.Namespace == dest.Namespace && existing.Name == dest.Name {
			continue
		}

		// For updates, skip the old version if checking against ourselves
		if oldDest != nil && existing.Namespace == oldDest.Namespace && existing.Name == oldDest.Name {
			continue
		}

		// Resolve the existing destination's GitRepoConfig
		existingRepoConfig, err := v.getGitRepoConfig(ctx, existing)
		if err != nil {
			log.V(1).Info("Skipping destination with unresolvable GitRepoConfig",
				"destination", fmt.Sprintf("%s/%s", existing.Namespace, existing.Name),
				"error", err.Error())
			continue
		}

		// Create identifier for existing destination
		existingNormalizedURL := normalizeRepoURL(existingRepoConfig.Spec.RepoURL)
		existingIdentifier := createDestinationIdentifier(
			existingNormalizedURL,
			existing.Spec.Branch,
			existing.Spec.BaseFolder,
		)

		// Check for conflict
		if existingIdentifier == destIdentifier {
			return nil, fmt.Errorf(
				"GitDestination conflict detected - another destination already uses this location:\n"+
					"  Repository: %s\n"+
					"  Branch: %s\n"+
					"  BaseFolder: %s\n"+
					"  Conflicting Resource: %s/%s\n\n"+
					"Suggestion: Use a different baseFolder (e.g., '%s/%s') to avoid conflicts",
				normalizedURL,
				dest.Spec.Branch,
				dest.Spec.BaseFolder,
				existing.Namespace,
				existing.Name,
				dest.Spec.BaseFolder,
				dest.Namespace,
			)
		}
	}

	log.Info("GitDestination uniqueness validation passed",
		"name", dest.Name,
		"namespace", dest.Namespace,
		"identifier", destIdentifier)

	return nil, nil
}

// getGitRepoConfig retrieves the referenced GitRepoConfig.
func (v *GitDestinationValidator) getGitRepoConfig(
	ctx context.Context,
	dest *configbutleraiv1alpha1.GitDestination,
) (*configbutleraiv1alpha1.GitRepoConfig, error) {
	// Resolve namespace (default to destination's namespace if not specified)
	namespace := v.resolveNamespace(dest.Spec.RepoRef.Namespace, dest.Namespace)

	var repoConfig configbutleraiv1alpha1.GitRepoConfig
	key := types.NamespacedName{
		Namespace: namespace,
		Name:      dest.Spec.RepoRef.Name,
	}

	if err := v.Client.Get(ctx, key, &repoConfig); err != nil {
		return nil, err
	}

	return &repoConfig, nil
}

// resolveNamespace returns the ref namespace if specified, otherwise returns the default namespace.
func (v *GitDestinationValidator) resolveNamespace(refNamespace, defaultNamespace string) string {
	if refNamespace != "" {
		return refNamespace
	}
	return defaultNamespace
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

// createDestinationIdentifier creates a unique identifier for a destination.
// Uses SHA256 hash of: normalized_repo_url + branch + baseFolder.
func createDestinationIdentifier(normalizedRepoURL, branch, baseFolder string) string {
	// Create deterministic string
	data := fmt.Sprintf("%s:%s:%s", normalizedRepoURL, branch, baseFolder)

	// Hash it for consistent identifier
	hash := sha256.Sum256([]byte(data))
	return hex.EncodeToString(hash[:])
}
