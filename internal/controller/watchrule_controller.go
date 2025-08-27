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

package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	configbutleraiv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
)

// WatchRuleReconciler reconciles a WatchRule object
type WatchRuleReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	RuleStore *rulestore.RuleStore
}

// +kubebuilder:rbac:groups=configbutler.ai,resources=watchrules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=configbutler.ai,resources=watchrules/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=configbutler.ai,resources=watchrules/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *WatchRuleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	log.V(1).Info("Reconciling WatchRule")

	// Fetch the WatchRule instance
	var watchRule configbutleraiv1alpha1.WatchRule
	if err := r.Get(ctx, req.NamespacedName, &watchRule); err != nil {
		if client.IgnoreNotFound(err) != nil {
			log.Error(err, "unable to fetch WatchRule")
			return ctrl.Result{}, err
		}
		// Resource was deleted. Remove it from the store.
		r.RuleStore.Delete(req.NamespacedName)
		log.Info("WatchRule deleted, removed from store", "name", req.Name, "namespace", req.Namespace)
		return ctrl.Result{}, nil
	}

	// Add or update the rule in the store
	r.RuleStore.AddOrUpdate(watchRule)
	log.Info("Successfully reconciled WatchRule", "name", watchRule.Name, "namespace", watchRule.Namespace)

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *WatchRuleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&configbutleraiv1alpha1.WatchRule{}).
		Named("watchrule").
		Complete(r)
}
