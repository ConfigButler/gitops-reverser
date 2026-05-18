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

package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	configbutleraiv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

// ExplicitCommitReconciler reconciles ExplicitCommit objects. Its only job is
// to stamp the initial WaitingForAuditEvent phase on freshly-created objects;
// the terminal phase is written by the audit consumer once the object's own
// audit event has been processed.
type ExplicitCommitReconciler struct {
	client.Client

	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=configbutler.ai,resources=explicitcommits,verbs=get;list;watch
// +kubebuilder:rbac:groups=configbutler.ai,resources=explicitcommits/status,verbs=get;update;patch

// Reconcile stamps the initial phase on an ExplicitCommit. It deliberately
// does no further work: finalizing the commit window is driven by the audit
// event, not by the API create.
func (r *ExplicitCommitReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("ExplicitCommitReconciler")

	var explicitCommit configbutleraiv1alpha1.ExplicitCommit
	if err := r.Get(ctx, req.NamespacedName, &explicitCommit); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Only stamp the initial phase. Once any phase is set — the initial
	// WaitingForAuditEvent or a terminal phase written by the audit consumer —
	// this controller has nothing left to do.
	if explicitCommit.Status.Phase != "" {
		return ctrl.Result{}, nil
	}

	explicitCommit.Status.Phase = configbutleraiv1alpha1.ExplicitCommitPhaseWaitingForAuditEvent
	if err := r.Status().Update(ctx, &explicitCommit); err != nil {
		return ctrl.Result{}, err
	}

	log.V(1).Info("Stamped ExplicitCommit as WaitingForAuditEvent", "name", req.NamespacedName)
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ExplicitCommitReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&configbutleraiv1alpha1.ExplicitCommit{}).
		Named("explicitcommit").
		Complete(r)
}
