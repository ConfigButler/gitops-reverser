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

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// fluxGuide is the Flux user guide. Its "A complete example" section is the configuration this
// file holds to the code: a reader copies it, so it must be accepted as written.
const fluxGuide = "../../docs/flux.md"

// TestFluxGuide_TheCompleteExampleIsAccepted reads the guide's complete example and checks it
// against the code that validates and drives a Flux applier: the GitOps Reverser objects decode
// into the API types with no unknown field, ApplierConfigured comes out True for the Flux objects
// as written, and a request stamps the GitRepository. It also holds the example to the guide's own
// advice, so the page cannot recommend one thing and show another.
func TestFluxGuide_TheCompleteExampleIsAccepted(t *testing.T) {
	objects := guideExampleObjects(t)

	var provider v1alpha3.GitProvider
	var target v1alpha3.GitTarget
	var secret corev1.Secret
	decodeStrict(t, objects, "GitProvider", &provider)
	decodeStrict(t, objects, "GitTarget", &target)
	decodeStrict(t, objects, "Secret", &secret)
	repository := one(t, objects, "GitRepository")
	kustomization := one(t, objects, "Kustomization")
	assert.NotContains(t, objects, "Receiver", "the guide tells the reader to remove the Flux Receiver")

	// The GitOps Reverser objects are wired to each other.
	assert.Equal(t, provider.Name, target.Spec.GitProviderRef.Name)
	assert.Equal(t, provider.Namespace, target.Namespace, "a GitTarget names a GitProvider in its own namespace")
	require.NotNil(t, provider.Spec.Receiver, "the example serves a push receiver")
	assert.Equal(t, secret.Name, provider.Spec.Receiver.SecretRef.Name)
	assert.Equal(t, provider.Namespace, secret.Namespace)
	assert.NotEmpty(t, secret.StringData[v1alpha3.ReceiverTokenKey], "the receiver reads the token key")
	require.NotNil(t, target.Spec.Applier)
	assert.Equal(t, v1alpha3.RefusalActionReapply, target.Spec.OnRefusal)
	assert.LessOrEqual(t, target.Spec.Applier.EffectiveQuietPeriod(), time.Hour)

	// The Flux objects follow the guide's advice.
	for _, obj := range []*unstructured.Unstructured{repository, kustomization} {
		interval, _, _ := unstructured.NestedString(obj.Object, "spec", "interval")
		d, err := time.ParseDuration(interval)
		require.NoError(t, err, "%s spec.interval", obj.GetKind())
		assert.GreaterOrEqual(
			t,
			d,
			time.Hour,
			"%s: a short interval fetches, and so applies, on its own",
			obj.GetKind(),
		)
	}
	suspended, _, _ := unstructured.NestedBool(kustomization.Object, "spec", "suspend")
	assert.False(t, suspended, "a suspended Kustomization ignores the requests")

	// The applier, as written, is accepted and driven.
	ref := types.NamespacedName{
		Namespace: target.Spec.Applier.KustomizationRef.Namespace, Name: target.Spec.Applier.KustomizationRef.Name,
	}
	if ref.Namespace == "" {
		ref.Namespace = target.Namespace
	}
	want := Binding{RepositoryURL: provider.Spec.URL, Branch: target.Spec.Branch}

	client := newSourceClient(kustomization, repository)
	flux := pinnedFlux(client)
	flux.CanI = allowAll
	verdict := flux.Validate(context.Background(), ref, want)
	assert.True(t, verdict.OK, "ApplierConfigured for the guide's example: %s: %s", verdict.Reason, verdict.Message)

	outcome, err := flux.Request(context.Background(), ref, want, testCommit, v1alpha3.ApplyReasonForeignCommit)
	require.NoError(t, err)
	assert.Equal(t, OutcomeStampedSource, outcome, "a source with no artifact yet is asked to fetch")
	assert.NotEmpty(t, requestedAt(t, client, gitRepositoryGVR(), repository.GetNamespace(), repository.GetName()))
}

// guideExampleObjects returns every object in the yaml blocks of the guide's "A complete example"
// section, by kind.
func guideExampleObjects(t *testing.T) map[string][]*unstructured.Unstructured {
	t.Helper()
	content, err := os.ReadFile(fluxGuide)
	require.NoError(t, err)
	text := string(content)

	const heading = "\n## A complete example\n"
	start := strings.Index(text, heading)
	require.GreaterOrEqual(t, start, 0, "%s must keep its %q section", fluxGuide, strings.TrimSpace(heading))
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
