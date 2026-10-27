// SPDX-License-Identifier: Apache-2.0

package controller

import (
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/event"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// KubeProviderConditionCommitRequestsServed reports whether a KubeProvider's cluster's CommitRequests
// are served. It is absent while spec.commitRequests is Ignore, and, like AuditFactsReceived, not part
// of Ready: mirroring works without it.
const KubeProviderConditionCommitRequestsServed = "CommitRequestsServed"

// CommitRequestsReporter is what the KubeProvider reconciler reads to report a provider's CommitRequests.
// *CommitRequestServer satisfies it.
type CommitRequestsReporter interface {
	// CommitRequestsReport is the last report for provider, and false before the first one.
	CommitRequestsReport(provider k8stypes.NamespacedName) (CommitRequestsReport, bool)
	// CommitRequestsEvents names a provider whose report changed.
	CommitRequestsEvents() <-chan event.GenericEvent
}

// applyCommitRequestsServed writes the CommitRequestsServed condition and status.commitRequests. A
// provider that serves no CommitRequests carries neither: a condition that always said "not applicable"
// would be noise, and a True left from before the switch would be wrong. Until the server has looked at
// a provider that serves them, what is there stays.
func (r *KubeProviderReconciler) applyCommitRequestsServed(
	st *reconcileStatus, provider *configbutleraiv1alpha3.KubeProvider,
) {
	if !provider.ServesCommitRequests() || r.CommitRequests == nil {
		st.remove(KubeProviderConditionCommitRequestsServed)
		provider.Status.CommitRequests = nil
		return
	}
	report, ok := r.CommitRequests.CommitRequestsReport(provider.Key())
	if !ok {
		return
	}
	st.set(KubeProviderConditionCommitRequestsServed, report.Status, report.Reason, report.Message)
	provider.Status.CommitRequests = &configbutleraiv1alpha3.KubeProviderCommitRequestsStatus{
		Namespaces: report.Namespaces,
	}
}
