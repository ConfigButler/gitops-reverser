// SPDX-License-Identifier: Apache-2.0

package applier

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// argoCDGuide is the Argo CD user guide. Its "A complete example" section is the configuration
// this file holds to the code: a reader copies it, so it must be accepted as written.
const argoCDGuide = "../../docs/argocd.md"

// TestArgoCDGuide_TheCompleteExampleIsAccepted reads the guide's complete example and checks it
// against the code that validates and drives an Argo CD applier: the GitOps Reverser objects decode
// into the API types with no unknown field, Configured comes out True for the Application as
// written, the trigger's source binds the GitTarget, and a request sets a sync pinned to the commit. It also holds the example to the guide's
// own advice: automated sync off, prune kept, and a recursive directory source.
func TestArgoCDGuide_TheCompleteExampleIsAccepted(t *testing.T) {
	objects := guideExampleObjects(t, argoCDGuide)

	var provider v1alpha3.GitProvider
	var target v1alpha3.GitTarget
	var trigger v1alpha3.ArgoCDTrigger
	var secret corev1.Secret
	decodeStrict(t, objects, "GitProvider", &provider)
	decodeStrict(t, objects, "GitTarget", &target)
	decodeStrict(t, objects, v1alpha3.ApplyTriggerKindArgoCD, &trigger)
	decodeStrict(t, objects, "Secret", &secret)
	app := one(t, objects, kindApplication)

	// The GitOps Reverser objects are wired to each other.
	assert.Equal(t, provider.Name, target.Spec.GitProviderRef.Name)
	assert.Equal(t, provider.Namespace, target.Namespace, "a GitTarget names a GitProvider in its own namespace")
	require.NotNil(t, provider.Spec.Receiver, "the example serves a push receiver")
	assert.Equal(t, secret.Name, provider.Spec.Receiver.SecretRef.Name)
	assert.NotEmpty(t, secret.StringData[v1alpha3.ReceiverTokenKey], "the receiver reads the token key")
	require.NotNil(t, target.Spec.ApplyTriggerRef)
	assert.Equal(t, v1alpha3.ApplyTriggerReference{Kind: v1alpha3.ApplyTriggerKindArgoCD, Name: trigger.Name},
		*target.Spec.ApplyTriggerRef)
	assert.Equal(t, target.Namespace, trigger.Namespace, "a GitTarget names a trigger in its own namespace")
	assert.Equal(t, v1alpha3.RefusalActionReapply, target.Spec.OnRefusal)

	// The Application follows the guide's advice.
	assert.False(t, automatedSyncEnabled(app), "the standard is automated sync off")
	assert.True(t, automatedPrune(app), "the off switch keeps prune for the syncs GitOps Reverser starts")
	recurse, _, _ := unstructured.NestedBool(app.Object, "spec", "source", "directory", "recurse")
	assert.True(t, recurse, "GitOps Reverser writes nested folders")

	// The applier, as written, is accepted, binds the GitTarget, and is driven.
	ref := trigger.Applier()
	assert.Equal(t, types.NamespacedName{Namespace: app.GetNamespace(), Name: app.GetName()}, ref)
	want := Binding{RepositoryURL: provider.Spec.URL, Branch: target.Spec.Branch}
	client := newSourceClient(app)
	argo := pinnedArgo(client)
	verdict := argo.Validate(context.Background(), ref)
	assert.True(t, verdict.OK, "Configured for the guide's example: %s: %s", verdict.Reason, verdict.Message)
	require.NotNil(t, verdict.Source)
	trigger.Status.Source = &v1alpha3.ArgoCDTriggerSource{
		RepoURL: verdict.Source.URL, TargetRevision: verdict.Source.Revision,
	}
	assert.Empty(t, BindingMismatch(&trigger, want), "the trigger's Application syncs what the GitTarget writes")

	started, err := argo.Request(context.Background(), ref, Ask{
		Want: want, Commit: testCommit, Reason: v1alpha3.ApplyReasonForeignCommit, Token: requestToken(stampTime),
	})
	require.NoError(t, err)
	assert.Equal(t, OutcomeSyncRequested, started.Outcome)
	written, err := client.Resource(applicationGVR()).Namespace(ref.Namespace).
		Get(context.Background(), ref.Name, metav1.GetOptions{})
	require.NoError(t, err)
	revision, _, _ := unstructured.NestedString(written.Object, "operation", "sync", "revision")
	prune, _, _ := unstructured.NestedBool(written.Object, "operation", "sync", "prune")
	assert.Equal(t, testCommit, revision)
	assert.True(t, prune)
}
