/*
Copyright 2025.

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

package v1alpha1

import (
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var gitrepoconfiglog = logf.Log.WithName("gitrepoconfig-webhook")

// SetupWebhookWithManager registers the webhook with the manager.
func (r *GitRepoConfig) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(r).
		Complete()
}

// TODO: Re-enable webhook marker after fixing controller-runtime registration
// +kubebuilder:webhook:path=/validate-configbutler-ai-v1alpha1-gitrepoconfig,mutating=false,failurePolicy=fail,sideEffects=None,groups=configbutler.ai,resources=gitrepoconfigs,verbs=create;update,versions=v1alpha1,name=vgitrepoconfig.kb.io,admissionReviewVersions=v1

// ValidateCreate implements webhook.Validator.
func (r *GitRepoConfig) ValidateCreate() (admission.Warnings, error) {
	gitrepoconfiglog.Info("validate create", "name", r.Name)
	return nil, r.validateAccessPolicy()
}

// ValidateUpdate implements webhook.Validator.
func (r *GitRepoConfig) ValidateUpdate(_ runtime.Object) (admission.Warnings, error) {
	gitrepoconfiglog.Info("validate update", "name", r.Name)
	return nil, r.validateAccessPolicy()
}

// ValidateDelete implements webhook.Validator.
func (r *GitRepoConfig) ValidateDelete() (admission.Warnings, error) {
	gitrepoconfiglog.Info("validate delete", "name", r.Name)
	// No validation needed for delete
	return nil, nil
}

// validateAccessPolicy validates the accessPolicy field.
func (r *GitRepoConfig) validateAccessPolicy() error {
	if r.Spec.AccessPolicy == nil {
		return nil // No access policy = use defaults (valid)
	}

	namespacedRules := r.Spec.AccessPolicy.NamespacedRules
	if namespacedRules == nil {
		return nil // No namespaced rules = use defaults (valid)
	}

	// Validate: namespaceSelector requires mode=FromSelector
	if namespacedRules.NamespaceSelector != nil {
		if namespacedRules.Mode != AccessPolicyModeFromSelector {
			return fmt.Errorf(
				"namespaceSelector can only be set when mode is 'FromSelector', got mode '%s'",
				namespacedRules.Mode,
			)
		}
	}

	// Validate: mode=FromSelector requires namespaceSelector
	if namespacedRules.Mode == AccessPolicyModeFromSelector {
		if namespacedRules.NamespaceSelector == nil {
			return errors.New("namespaceSelector is required when mode is 'FromSelector'")
		}

		// Validate label selector is well-formed
		_, err := metav1.LabelSelectorAsSelector(namespacedRules.NamespaceSelector)
		if err != nil {
			return fmt.Errorf("invalid namespaceSelector: %w", err)
		}
	}

	return nil
}
