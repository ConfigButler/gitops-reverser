// SPDX-License-Identifier: Apache-2.0

package applier

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/applier/fluxsim"
	"github.com/ConfigButler/gitops-reverser/internal/authz"
)

// fluxGuide is the Flux user guide. Its "A complete example" section is the configuration this
// file holds to the code: a reader copies it, so it must be accepted as written.
const fluxGuide = "../../docs/bi-directional-flux.md"

// TestFluxGuide_TheCompleteExampleIsAccepted reads the guide's complete example and checks it
// against the code that validates and drives a Flux applier: the GitOps Reverser objects decode
// into the API types with no unknown field, Configured comes out True for the Flux objects as
// written, the trigger's source binds the GitTarget, and a request fetches and resumes the
// Kustomization. It also holds the example to the guide's own
// advice, so the page cannot recommend one thing and show another.
func TestFluxGuide_TheCompleteExampleIsAccepted(t *testing.T) {
	objects := guideExampleObjects(t, fluxGuide)

	var provider v1alpha3.GitProvider
	var branch v1alpha3.GitBranch
	var target v1alpha3.GitTarget
	var trigger v1alpha3.FluxTrigger
	var secret corev1.Secret
	decodeStrict(t, objects, "GitProvider", &provider)
	decodeStrict(t, objects, "GitBranch", &branch)
	decodeStrict(t, objects, "GitTarget", &target)
	decodeStrict(t, objects, v1alpha3.ApplyTriggerKindFlux, &trigger)
	decodeStrict(t, objects, "Secret", &secret)
	exampleReachesItsApplier(t, objects, &trigger)
	repository := one(t, objects, "GitRepository")
	kustomization := one(t, objects, "Kustomization")
	assert.NotContains(t, objects, "Receiver", "the guide tells the reader to remove the Flux Receiver")

	// The GitOps Reverser objects are wired to each other.
	assert.Equal(t, provider.Name, branch.Spec.GitProviderRef.Name)
	assert.Equal(t, provider.Namespace, branch.Namespace, "a GitBranch names a GitProvider in its own namespace")
	assert.Equal(t, branch.Name, target.Spec.GitBranchRef.Name)
	assert.Equal(
		t,
		branch.Namespace,
		target.Namespace,
		"the example's GitTarget names a GitBranch in its own namespace",
	)
	require.NotNil(t, provider.Spec.Receiver, "the example serves a push receiver")
	assert.Equal(t, secret.Name, provider.Spec.Receiver.SecretRef.Name)
	assert.Equal(t, provider.Namespace, secret.Namespace)
	assert.NotEmpty(t, secret.StringData[v1alpha3.ReceiverTokenKey], "the receiver reads the token key")
	require.NotNil(t, target.Spec.ApplyTriggerRef)
	assert.Equal(t, v1alpha3.ApplyTriggerReference{Kind: v1alpha3.ApplyTriggerKindFlux, Name: trigger.Name},
		*target.Spec.ApplyTriggerRef)
	assert.Equal(t, target.Namespace, trigger.Namespace, "a GitTarget names a trigger in its own namespace")
	assert.Equal(t, v1alpha3.RefusalActionReapply, target.Spec.OnRefusal)

	// The Flux objects follow the guide's advice.
	interval, _, _ := unstructured.NestedString(repository.Object, "spec", "interval")
	d, err := time.ParseDuration(interval)
	require.NoError(t, err, "GitRepository spec.interval")
	assert.GreaterOrEqual(t, d, time.Hour,
		"a source that fetches often can move the artifact between the recheck and the resume")
	_, setsSuspend, _ := unstructured.NestedFieldNoCopy(kustomization.Object, "spec", "suspend")
	assert.False(t, setsSuspend, "the manifest omits spec.suspend, which GitOps Reverser holds")

	// The applier, as written, is accepted, binds the GitTarget, and is driven.
	ref := trigger.Applier()
	assert.Equal(t, types.NamespacedName{Namespace: kustomization.GetNamespace(), Name: kustomization.GetName()}, ref)
	want := Binding{RepositoryURL: provider.Spec.URL, Branch: branch.Spec.Branch}

	client := newSourceClient(kustomization, repository)
	flux := pinnedFlux(client)
	flux.CanI = allowAll
	verdict := flux.Validate(context.Background(), ref)
	assert.True(t, verdict.OK, "Configured for the guide's example: %s: %s", verdict.Reason, verdict.Message)
	require.NotNil(t, verdict.Source)
	trigger.Status.Source = &v1alpha3.FluxTriggerSource{URL: verdict.Source.URL, Branch: verdict.Source.Revision}
	assert.Empty(t, BindingMismatch(&trigger, want), "the trigger's Kustomization applies what the GitTarget writes")

	sim := fluxsim.New(t, client)
	sim.Fetches(types.NamespacedName{Namespace: repository.GetNamespace(), Name: repository.GetName()},
		branch.Spec.Branch+"@sha1:"+testCommit)
	flux.Now = fluxsim.TickingClock(stampTime)
	started, err := flux.Request(context.Background(), ref, Ask{
		Want: want, Commit: testCommit, Reason: v1alpha3.ApplyReasonForeignCommit, Token: "T1",
	})
	require.NoError(t, err)
	assert.Equal(t, OutcomeResumed, started.Outcome, "a source with no artifact yet fetches, then applies")
	assert.NotEmpty(t, requestedAt(t, client, gitRepositoryGVR(), repository.GetNamespace(), repository.GetName()))
}

// guideExampleObjects returns every object in the yaml blocks of a guide's "A complete example"
// section, by kind.
func guideExampleObjects(t *testing.T, guide string) map[string][]*unstructured.Unstructured {
	t.Helper()
	content, err := os.ReadFile(guide)
	require.NoError(t, err)
	text := string(content)

	const heading = "\n## A complete example\n"
	start := strings.Index(text, heading)
	require.GreaterOrEqual(t, start, 0, "%s must keep its %q section", guide, strings.TrimSpace(heading))
	section := text[start+len(heading):]
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}

	objects := map[string][]*unstructured.Unstructured{}
	blocks := regexp.MustCompile("(?s)```yaml\n(.*?)```").FindAllStringSubmatch(section, -1)
	require.NotEmpty(t, blocks, "the complete example holds yaml blocks")
	for _, block := range blocks {
		decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewBufferString(block[1]), 4096)
		for {
			obj := &unstructured.Unstructured{}
			if err := decoder.Decode(&obj.Object); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				require.NoError(t, err)
			}
			if len(obj.Object) == 0 {
				continue
			}
			objects[obj.GetKind()] = append(objects[obj.GetKind()], obj)
		}
	}
	return objects
}

// one returns the only object of kind in the example.
func one(t *testing.T, objects map[string][]*unstructured.Unstructured, kind string) *unstructured.Unstructured {
	t.Helper()
	require.Len(t, objects[kind], 1, "the complete example holds exactly one %s", kind)
	return objects[kind][0]
}

// decodeStrict decodes the only object of kind into into, failing on a field the type does not have.
func decodeStrict(t *testing.T, objects map[string][]*unstructured.Unstructured, kind string, into runtime.Object) {
	t.Helper()
	encoded, err := json.Marshal(one(t, objects, kind).Object)
	require.NoError(t, err)
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	require.NoError(t, decoder.Decode(into), "the guide's %s must decode with no unknown field", kind)
}

// exampleReachesItsApplier checks that the guide's example KubeProvider admits the trigger's
// namespace and lets it reach its applier, through the admission the trigger reconciler runs.
func exampleReachesItsApplier(
	t *testing.T, objects map[string][]*unstructured.Unstructured, trigger v1alpha3.ApplyTrigger,
) {
	t.Helper()
	var provider v1alpha3.KubeProvider
	decodeStrict(t, objects, "KubeProvider", &provider)
	assert.Equal(t, trigger.KubeProviderKey(), provider.Key(), "the example declares the trigger's KubeProvider")
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, v1alpha3.AddToScheme(scheme))
	control := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&provider).Build()
	decision, err := authz.ApplyTriggerAdmitted(context.Background(), control, trigger)
	require.NoError(t, err)
	assert.True(t, decision.Allowed, "the example's KubeProvider lets the trigger reach its applier: %s: %s",
		decision.Reason, decision.Message)
}
