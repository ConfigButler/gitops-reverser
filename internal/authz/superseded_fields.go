// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"fmt"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// ReasonSupersededFieldStored is the terminal reason for an object that still carries a field this
// release removed or renamed.
//
// It is deliberately ONE reason across three kinds. From an operator's point of view the fact that
// matters is the same in every case — this object was written against the previous API and has not
// been migrated — and the Message names the exact field and its replacement.
const ReasonSupersededFieldStored = "SupersededFieldStored"

// SupersededFieldRefusal reports the stored superseded field on obj, or "" when it carries none.
//
// # Why a stored value is refused rather than ignored
//
// Each of these fields is retained in the schema and rejected at admission, so a manifest that
// still sets one fails to apply. That covers the WRITE path and nothing else: an object written by
// an earlier release keeps its value in etcd and is never re-admitted, so admission alone leaves
// exactly the population that most needs telling.
//
// Ignoring a stored value is silent reinterpretation, and for each of these it is silent in a way
// that changes behavior:
//
//   - GitTarget.spec.allowedSourceNamespaces bounded which source namespaces could reach a folder.
//     Ignored, it becomes a field that READS like a fence and enforces nothing — and the moment its
//     ClusterProvider is migrated to allowAnySourceNamespace, a `sourceNamespace: "*"` rule under it
//     widens from that declared set to every namespace the credential can read, with the stale
//     field still sitting there describing the old bound.
//   - GitProvider.spec.push.commitWindow and spec.commit.message shaped every commit. Ignored, the
//     folder silently starts committing at the default cadence and under the default wording.
//   - ClusterProvider.spec.allowedNamespaces is deny-by-default, so ignoring it does not fail open:
//     accessFrom is absent, nothing is admitted, and every GitTarget through the provider stalls
//     with a message blaming its namespace rather than naming the rename. Refusing here is what
//     turns a confusing failure into a legible one.
//   - ClusterProvider.spec.allowSourceNamespaceOverride is the sharpest of the four: ignoring a
//     stored `true` REVOKES a delegation a platform admin granted, stalling every cross-namespace
//     WatchRule through that provider.
//
// # A DEFAULTED field is refused only at its meaningful value
//
// allowSourceNamespaceOverride carried `+kubebuilder:default=false` before this release, so the
// apiserver wrote it into EVERY stored ClusterProvider whether or not anyone asked for it — the
// chart-owned `default` provider included. Refusing every non-nil value would therefore refuse
// every install that never used the feature, and the operator could not fix it: `kubectl apply`
// does not remove a field the server defaulted, because it was never in the user's manifest to be
// removed. That is an unfixable upgrade, and it is why only `true` is refused here.
//
// A stored `false` is refused by nothing because it means nothing: it grants no delegation, and
// allowAnySourceNamespace defaults false too, so ignoring it reinterprets no behavior and loses no
// intent. This is the same rule ClusterWatchRuleSpec.DeclaresNamespacedScope already applies to its
// own retained field, which refuses a stored "Namespaced" and lets the default "Cluster" through.
//
// The other three fields carry no default, so any stored value is one a user wrote on purpose and
// every one of them is refused.
//
// The refusal is terminal and clears the moment the field is removed, which is an ordinary edit the
// message spells out.
func SupersededFieldRefusal(obj any) string {
	switch o := obj.(type) {
	case *configv1alpha3.GitTarget:
		return gitTargetSupersededField(o)
	case *configv1alpha3.GitProvider:
		return gitProviderSupersededField(o)
	case *configv1alpha3.ClusterProvider:
		return clusterProviderSupersededField(o)
	default:
		return ""
	}
}

func gitTargetSupersededField(target *configv1alpha3.GitTarget) string {
	//nolint:staticcheck // reading the removed field is the point: it must be refused, not ignored.
	if target.Spec.AllowedSourceNamespaces == nil {
		return ""
	}
	return fmt.Sprintf(
		"GitTarget %s/%s still sets spec.allowedSourceNamespaces, which this release removed. It is "+
			"refused rather than ignored, because ignoring it would leave a field that reads like a "+
			"bound on which source namespaces reach this folder while enforcing nothing. Source-cluster "+
			"RBAC now bounds what may be read and ClusterProvider.spec.accessFrom bounds which "+
			"namespaces may wield it; a rules[].sourceNamespace other than a WatchRule's own namespace "+
			"needs only ClusterProvider.spec.allowAnySourceNamespace. Note that \"*\" no longer means "+
			"\"every namespace this GitTarget admits\": it is every namespace the credential can read. "+
			"Delete spec.allowedSourceNamespaces to clear this",
		target.Namespace, target.Name)
}

func gitProviderSupersededField(provider *configv1alpha3.GitProvider) string {
	//nolint:staticcheck // reading the relocated fields is the point: they must be refused.
	push := provider.Spec.Push != nil
	//nolint:staticcheck // reading the relocated field is the point: it must be refused.
	message := provider.Spec.Commit != nil && provider.Spec.Commit.Message != nil
	if !push && !message {
		return ""
	}

	field := "spec.push"
	replacement := "GitTarget.spec.commit.window"
	switch {
	case push && message:
		field = "spec.push and spec.commit.message"
		replacement = "GitTarget.spec.commit.window and GitTarget.spec.commit.message"
	case message:
		field = "spec.commit.message"
		replacement = "GitTarget.spec.commit.message"
	}
	return fmt.Sprintf(
		"GitProvider %s/%s still sets %s, which moved to %s in this release. Writes through this "+
			"provider are STOPPED rather than made at the default cadence and wording, because a "+
			"folder committing on settings nobody chose is worse than one that is not committing. "+
			"Set the values on each GitTarget that needs them, then remove them here",
		provider.Namespace, provider.Name, field, replacement)
}

func clusterProviderSupersededField(provider *configv1alpha3.ClusterProvider) string {
	//nolint:staticcheck // reading the renamed fields is the point: they must be refused.
	renamedPolicy := provider.Spec.AllowedNamespaces != nil
	// Only a stored `true` is refused: see "A DEFAULTED field is refused only at its meaningful
	// value" on SupersededFieldRefusal. A `false` is the old schema's default, present on every
	// stored provider, and it grants nothing.
	//nolint:staticcheck // reading the renamed field is the point: it must be refused.
	renamedFlag := provider.Spec.AllowSourceNamespaceOverride != nil &&
		*provider.Spec.AllowSourceNamespaceOverride
	if !renamedPolicy && !renamedFlag {
		return ""
	}

	field := "spec.allowedNamespaces"
	replacement := "spec.accessFrom"
	switch {
	case renamedPolicy && renamedFlag:
		field = "spec.allowedNamespaces and spec.allowSourceNamespaceOverride"
		replacement = "spec.accessFrom and spec.allowAnySourceNamespace"
	case renamedFlag:
		field = "spec.allowSourceNamespaceOverride"
		replacement = "spec.allowAnySourceNamespace"
	}
	return fmt.Sprintf(
		"ClusterProvider %q still sets %s, renamed to %s in this release. Same shape, same default, "+
			"same semantics: rename the key. It is refused rather than ignored because ignoring it "+
			"would revoke what it grants without saying so",
		provider.Name, field, replacement)
}
