// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// GitTargetConditionLayoutResolved reports what the last scan resolved about the folder's shape:
// which kustomize render root governs new documents, or that there is none, or that there are
// several. It is an OBSERVATION and writes no part of the kstatus trio — a folder with no
// kustomization at all is a perfectly healthy folder, and most of them are.
const GitTargetConditionLayoutResolved = "LayoutResolved"

const (
	// GitTargetReasonAmbiguousLayout is a GitTarget path covering more than one kustomize render
	// root. The string must stay in sync with the watch package's gitPathRefusalReason, which
	// maps the corresponding write refusal onto GitPathAccepted.
	GitTargetReasonAmbiguousLayout = "AmbiguousLayout"
	// GitTargetReasonLayoutNotScanned is the pre-scan state: the target has not read its folder
	// yet, so nothing is known about its layout. Distinct from a folder that resolved to nothing.
	GitTargetReasonLayoutNotScanned = "NotScanned"
)

// observeLayout reads the layout the data plane last resolved for this GitTarget.
//
// Absent means no scan has reported yet, which is genuinely different from a folder that
// resolved to no root: the first is Unknown and the second is a definite answer with reason
// None, and collapsing them would make a target that has never read its folder indistinguishable
// from one that read it and found a plain directory.
func (r *GitTargetReconciler) observeLayout(
	target *configbutleraiv1alpha3.GitTarget,
) (git.LayoutReport, bool) {
	if r.EventRouter == nil || r.EventRouter.WatchManager == nil {
		return git.LayoutReport{}, false
	}
	return r.EventRouter.WatchManager.LayoutForGitTarget(
		types.NewResourceReference(target.Name, target.Namespace))
}

// publishLayout writes status.placement and the LayoutResolved condition.
//
// It runs before every gate, so a target held unready still shows what its folder resolved to.
// That ordering is the point rather than a convenience: the stanza's whole reason for existing is
// to be readable BEFORE the target is doing anything, and a projection that only ran on the happy
// path would be missing exactly when it is wanted.
func publishLayout(
	st *reconcileStatus,
	target *configbutleraiv1alpha3.GitTarget,
	report git.LayoutReport,
	scanned bool,
) {
	if !scanned {
		st.set(GitTargetConditionLayoutResolved, metav1.ConditionUnknown,
			GitTargetReasonLayoutNotScanned,
			"the GitTarget folder has not been scanned yet; layout is unknown")
		return
	}

	target.Status.Placement = placementStatus(report)
	value, message := layoutCondition(report)
	st.set(GitTargetConditionLayoutResolved, value, string(report.Reason), message)
}

// layoutCondition maps a resolution onto the condition's status and message.
//
// Only Ambiguous is False. None is a definite, healthy answer — the folder has no kustomization
// and new documents take a declared template or the canonical path — and reporting it as False
// would train operators to ignore the condition on the majority of folders, which is the same
// mistake status.retention was designed not to make.
func layoutCondition(report git.LayoutReport) (metav1.ConditionStatus, string) {
	switch report.Reason {
	case manifestanalyzer.LayoutSingleKustomization:
		return metav1.ConditionTrue,
			fmt.Sprintf("render root %q governs new files", report.RenderRoot)
	case manifestanalyzer.LayoutNone:
		return metav1.ConditionTrue,
			"no kustomization governs this folder; new files take the declared or canonical path"
	case manifestanalyzer.LayoutAmbiguous:
		return metav1.ConditionFalse, fmt.Sprintf(
			"the GitTarget path covers %d kustomize render roots (%s); point it at one of them, so "+
				"one target is one write partition",
			len(report.RenderRoots), strings.Join(report.RenderRoots, ", "))
	default:
		return metav1.ConditionUnknown, "the folder's layout could not be resolved"
	}
}

// placementStatus projects a report onto the status stanza.
func placementStatus(report git.LayoutReport) *configbutleraiv1alpha3.GitTargetPlacementStatus {
	status := &configbutleraiv1alpha3.GitTargetPlacementStatus{
		RenderRoot:       report.RenderRoot,
		ByTypeEntries:    int32(report.ByTypeEntries), //nolint:gosec // a byType map has no unbounded size
		ObservedRevision: report.Revision,
	}
	if report.SerializeNamespace != nil {
		resolved := *report.SerializeNamespace
		status.SerializeNamespace = &resolved
	}
	if !report.ObservedTime.IsZero() {
		observed := metav1.NewTime(report.ObservedTime)
		status.ObservedTime = &observed
	}
	for _, example := range report.Examples {
		status.Examples = append(status.Examples, configbutleraiv1alpha3.GitTargetPlacementExample{
			Type:   example.Type,
			Path:   example.Path,
			Source: string(example.Source),
		})
	}
	return status
}
