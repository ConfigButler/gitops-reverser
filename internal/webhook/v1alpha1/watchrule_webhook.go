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
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	configbutleraiv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

// nolint:unused
// log is for logging in this package.
var watchrulelog = logf.Log.WithName("watchrule-resource")

// SetupWatchRuleWebhookWithManager registers the webhook for WatchRule in the manager.
func SetupWatchRuleWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&configbutleraiv1alpha1.WatchRule{}).
		WithValidator(&WatchRuleCustomValidator{}).
		WithDefaulter(&WatchRuleCustomDefaulter{}).
		Complete()
}

// TODO(user): EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!

// +kubebuilder:webhook:path=/mutate-configbutler-ai-configbutler-ai-v1alpha1-watchrule,mutating=true,failurePolicy=fail,sideEffects=None,groups=configbutler.ai.configbutler.ai,resources=watchrules,verbs=create;update,versions=v1alpha1,name=mwatchrule-v1alpha1.kb.io,admissionReviewVersions=v1

// WatchRuleCustomDefaulter struct is responsible for setting default values on the custom resource of the
// Kind WatchRule when those are created or updated.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as it is used only for temporary operations and does not need to be deeply copied.
type WatchRuleCustomDefaulter struct {
	// TODO(user): Add more fields as needed for defaulting
}

var _ webhook.CustomDefaulter = &WatchRuleCustomDefaulter{}

// Default implements webhook.CustomDefaulter so a webhook will be registered for the Kind WatchRule.
func (d *WatchRuleCustomDefaulter) Default(_ context.Context, obj runtime.Object) error {
	watchrule, ok := obj.(*configbutleraiv1alpha1.WatchRule)

	if !ok {
		return fmt.Errorf("expected an WatchRule object but got %T", obj)
	}
	watchrulelog.Info("Defaulting for WatchRule", "name", watchrule.GetName())

	// TODO(user): fill in your defaulting logic.

	return nil
}

// TODO(user): change verbs to "verbs=create;update;delete" if you want to enable deletion validation.
// NOTE: The 'path' attribute must follow a specific pattern and should not be modified directly here.
// Modifying the path for an invalid path can cause API server errors; failing to locate the webhook.
// +kubebuilder:webhook:path=/validate-configbutler-ai-configbutler-ai-v1alpha1-watchrule,mutating=false,failurePolicy=fail,sideEffects=None,groups=configbutler.ai.configbutler.ai,resources=watchrules,verbs=create;update,versions=v1alpha1,name=vwatchrule-v1alpha1.kb.io,admissionReviewVersions=v1

// WatchRuleCustomValidator struct is responsible for validating the WatchRule resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type WatchRuleCustomValidator struct {
	// TODO(user): Add more fields as needed for validation
}

var _ webhook.CustomValidator = &WatchRuleCustomValidator{}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type WatchRule.
func (v *WatchRuleCustomValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	watchrule, ok := obj.(*configbutleraiv1alpha1.WatchRule)
	if !ok {
		return nil, fmt.Errorf("expected a WatchRule object but got %T", obj)
	}
	watchrulelog.Info("Validation for WatchRule upon creation", "name", watchrule.GetName())

	// TODO(user): fill in your validation logic upon object creation.

	return nil, nil
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type WatchRule.
func (v *WatchRuleCustomValidator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	watchrule, ok := newObj.(*configbutleraiv1alpha1.WatchRule)
	if !ok {
		return nil, fmt.Errorf("expected a WatchRule object for the newObj but got %T", newObj)
	}
	watchrulelog.Info("Validation for WatchRule upon update", "name", watchrule.GetName())

	// TODO(user): fill in your validation logic upon object update.

	return nil, nil
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type WatchRule.
func (v *WatchRuleCustomValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	watchrule, ok := obj.(*configbutleraiv1alpha1.WatchRule)
	if !ok {
		return nil, fmt.Errorf("expected a WatchRule object but got %T", obj)
	}
	watchrulelog.Info("Validation for WatchRule upon deletion", "name", watchrule.GetName())

	// TODO(user): fill in your validation logic upon object deletion.

	return nil, nil
}
