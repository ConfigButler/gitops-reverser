// SPDX-License-Identifier: Apache-2.0

package applier

import (
	"context"
	"errors"
	"testing"
	"time"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

const (
	testCommit   = "4f2c1ab9d0e1f2a3b4c5d6e7f8091a2b3c4d5e6f"
	olderCommit  = "0000000000000000000000000000000000000001"
	testRepoURL  = "https://github.com/acme/platform"
	testBranch   = "main"
	fluxSystemNS = "flux-system"
)

var stampTime = time.Date(2026, 10, 20, 10, 14, 3, 123456789, time.UTC)

// testBinding is the repository and branch the test appliers are expected to apply.
var testBinding = Binding{RepositoryURL: testRepoURL, Branch: testBranch}

func newSourceClient(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			kustomizationGVR(): "KustomizationList",
			gitRepositoryGVR(): "GitRepositoryList",
		}, objs...)
}

// kustomizationObj builds a Kustomization; a nil sourceRef omits spec.sourceRef.
func kustomizationObj(namespace, name string, sourceRef map[string]any) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1",
		"kind":       kindKustomization,
		"metadata":   map[string]any{"namespace": namespace, "name": name},
		"spec":       map[string]any{"path": "./apps"},
	}}
	if sourceRef != nil {
		_ = unstructured.SetNestedMap(obj.Object, sourceRef, "spec", "sourceRef")
	}
	return obj
}

func gitRepoSourceRef(name string) map[string]any {
	return map[string]any{"kind": kindGitRepository, "name": name}
}

// gitRepositoryObj builds a GitRepository; a nil ref omits spec.ref, an empty revision omits the
// artifact.
func gitRepositoryObj(namespace, name, url string, ref map[string]any, revision string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "source.toolkit.fluxcd.io/v1",
		"kind":       kindGitRepository,
		"metadata":   map[string]any{"namespace": namespace, "name": name},
		"spec":       map[string]any{"url": url},
	}}
	if ref != nil {
		_ = unstructured.SetNestedMap(obj.Object, ref, "spec", "ref")
	}
	if revision != "" {
		_ = unstructured.SetNestedField(obj.Object, revision, "status", "artifact", "revision")
	}
	return obj
}

func branchRef(branch string) map[string]any { return map[string]any{"branch": branch} }

// requestedAt returns an object's reconcile.fluxcd.io/requestedAt, or "".
func requestedAt(t *testing.T, client *dynamicfake.FakeDynamicClient, gvr schema.GroupVersionResource,
	namespace, name string) string {
	t.Helper()
	obj, err := client.Resource(gvr).Namespace(namespace).Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	return obj.GetAnnotations()[fluxmeta.ReconcileRequestAnnotation]
}

func pinnedFlux(client *dynamicfake.FakeDynamicClient) *Flux {
	flux := NewFlux(client)
	flux.Now = func() time.Time { return stampTime }
	return flux
}

func TestSplitRevision_Formats(t *testing.T) {
	tests := []struct {
		revision  string
		wantNamed string
		wantHash  string
	}{
		{"main@sha1:" + testCommit, "main", testCommit},
		{"feature/branch@sha1:" + testCommit, "feature/branch", testCommit},
		{"refs/heads/main@sha1:" + testCommit, "refs/heads/main", testCommit},
		{"sha1:" + testCommit, "", testCommit},
		{"main@sha256:abc123", "main", "abc123"},
		{"main/" + testCommit, "main", testCommit},
		{"feature/branch/" + testCommit, "feature/branch", testCommit},
		{"HEAD/" + testCommit, "", testCommit},
		{testCommit, "", testCommit},
		{"", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.revision, func(t *testing.T) {
			named, hash := splitRevision(tt.revision)
			assert.Equal(t, tt.wantNamed, named)
			assert.Equal(t, tt.wantHash, hash)
		})
	}
}

// TestFluxRequest_Outcomes pins which object a request stamps, by whether the artifact already
// holds the commit and by reason.
func TestFluxRequest_Outcomes(t *testing.T) {
	tests := []struct {
		name             string
		revision         string
		commit           string
		reason           v1alpha3.ApplyReason
		want             Outcome
		wantSourceStamp  bool
		wantKustomStamps bool
	}{
		{
			name: "foreign commit not yet fetched stamps the source", revision: "main@sha1:" + olderCommit,
			commit: testCommit, reason: v1alpha3.ApplyReasonForeignCommit,
			want: OutcomeStampedSource, wantSourceStamp: true,
		},
		{
			name: "catch-up behind stamps the source", revision: "main@sha1:" + olderCommit,
			commit: testCommit, reason: v1alpha3.ApplyReasonCatchUp,
			want: OutcomeStampedSource, wantSourceStamp: true,
		},
		{
			name: "no artifact yet stamps the source", revision: "",
			commit: testCommit, reason: v1alpha3.ApplyReasonForeignCommit,
			want: OutcomeStampedSource, wantSourceStamp: true,
		},
		{
			name: "branch absent on the remote stamps the source", revision: "main@sha1:" + testCommit,
			commit: "", reason: v1alpha3.ApplyReasonForeignCommit,
			want: OutcomeStampedSource, wantSourceStamp: true,
		},
		{
			name: "refusal at the artifact's commit stamps the kustomization", revision: "main@sha1:" + testCommit,
			commit: testCommit, reason: v1alpha3.ApplyReasonRefusal,
			want: OutcomeStampedKustomization, wantKustomStamps: true,
		},
		{
			name: "foreign commit already fetched stamps the kustomization", revision: "main/" + testCommit,
			commit: testCommit, reason: v1alpha3.ApplyReasonForeignCommit,
			want: OutcomeStampedKustomization, wantKustomStamps: true,
		},
		{
			name: "catch-up already current stamps nothing", revision: "sha1:" + testCommit,
			commit: testCommit, reason: v1alpha3.ApplyReasonCatchUp,
			want: OutcomeCurrent,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newSourceClient(
				kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform")),
				gitRepositoryObj(fluxSystemNS, "platform", testRepoURL, branchRef(testBranch), tt.revision),
			)
			got, err := pinnedFlux(client).Request(context.Background(),
				types.NamespacedName{Namespace: fluxSystemNS, Name: "checkout"}, testBinding, tt.commit, tt.reason)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)

			wantStamp := func(want bool) string {
				if want {
					return stampTime.Format(time.RFC3339Nano)
				}
				return ""
			}
			assert.Equal(t, wantStamp(tt.wantSourceStamp),
				requestedAt(t, client, gitRepositoryGVR(), fluxSystemNS, "platform"))
			assert.Equal(t, wantStamp(tt.wantKustomStamps),
				requestedAt(t, client, kustomizationGVR(), fluxSystemNS, "checkout"))
		})
	}
}

// TestFluxRequest_SourceRefNamespace: an omitted sourceRef namespace is the Kustomization's own; a
// set one is followed across namespaces.
func TestFluxRequest_SourceRefNamespace(t *testing.T) {
	tests := []struct {
		name          string
		sourceRef     map[string]any
		wantNamespace string
	}{
		{"defaults to the kustomization's", gitRepoSourceRef("platform"), "apps"},
		{
			"follows an explicit one",
			map[string]any{"kind": kindGitRepository, "name": "platform", "namespace": fluxSystemNS},
			fluxSystemNS,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newSourceClient(
				kustomizationObj("apps", "checkout", tt.sourceRef),
				gitRepositoryObj(tt.wantNamespace, "platform", testRepoURL, branchRef(testBranch), ""),
			)
			kustomization := types.NamespacedName{Namespace: "apps", Name: "checkout"}
			got, err := pinnedFlux(client).Request(context.Background(), kustomization, testBinding, testCommit,
				v1alpha3.ApplyReasonForeignCommit)
			require.NoError(t, err)
			assert.Equal(t, OutcomeStampedSource, got)
			assert.NotEmpty(t, requestedAt(t, client, gitRepositoryGVR(), tt.wantNamespace, "platform"))
		})
	}
}

func TestFluxRequest_Failures(t *testing.T) {
	tests := []struct {
		name          string
		objs          []runtime.Object
		wantPermanent bool
	}{
		{name: "kustomization not found"},
		{
			name: "git repository not found",
			objs: []runtime.Object{kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform"))},
		},
		{
			name: "source is not a git repository",
			objs: []runtime.Object{kustomizationObj(fluxSystemNS, "checkout",
				map[string]any{"kind": "Bucket", "name": "platform"})},
			wantPermanent: true,
		},
		{
			name:          "no source ref",
			objs:          []runtime.Object{kustomizationObj(fluxSystemNS, "checkout", nil)},
			wantPermanent: true,
		},
		{
			// The review's reproduction of finding 2: Validate said SourceMismatch, and the
			// request stamped the release source anyway.
			name: "source follows another branch",
			objs: []runtime.Object{
				kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform")),
				gitRepositoryObj(
					fluxSystemNS,
					"platform",
					testRepoURL,
					branchRef("release"),
					"release@sha1:"+olderCommit,
				),
			},
			wantPermanent: true,
		},
		{
			name: "source fetches another repository",
			objs: []runtime.Object{
				kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform")),
				gitRepositoryObj(fluxSystemNS, "platform", "https://github.com/acme/other", branchRef(testBranch), ""),
			},
			wantPermanent: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newSourceClient(tt.objs...)
			_, err := pinnedFlux(client).Request(context.Background(),
				types.NamespacedName{Namespace: fluxSystemNS, Name: "checkout"}, testBinding, testCommit,
				v1alpha3.ApplyReasonForeignCommit)
			require.Error(t, err)
			assert.Equal(t, tt.wantPermanent, isPermanent(err))
			for _, action := range client.Actions() {
				assert.NotEqual(t, "patch", action.GetVerb(), "a failed request stamps nothing")
			}
		})
	}
}

// allowAll and denyOn are AccessCheck fakes.
func allowAll(context.Context, authorizationv1.ResourceAttributes) (bool, error) { return true, nil }

func denyOn(resource string) AccessCheck {
	return func(_ context.Context, attrs authorizationv1.ResourceAttributes) (bool, error) {
		return attrs.Resource != resource, nil
	}
}

// TestFluxValidate_Reasons covers every ApplierConfigured answer.
func TestFluxValidate_Reasons(t *testing.T) {
	healthyKustomization := kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform"))
	repo := func(url string, ref map[string]any) runtime.Object {
		return gitRepositoryObj(fluxSystemNS, "platform", url, ref, "")
	}
	tests := []struct {
		name        string
		objs        []runtime.Object
		reactor     k8stesting.ReactionFunc
		canI        AccessCheck
		wantReason  string
		wantMessage string
		wantErr     bool
	}{
		{
			name:        "configured",
			objs:        []runtime.Object{healthyKustomization, repo(testRepoURL, branchRef(testBranch))},
			wantReason:  ReasonSucceeded,
			wantMessage: "Kustomization flux-system/checkout applies GitRepository flux-system/platform",
		},
		{
			name:        "kustomization missing",
			wantReason:  ReasonApplierNotFound,
			wantMessage: "Kustomization flux-system/checkout not found",
		},
		{
			name: "kind not served, as a RESTMapper says it",
			reactor: func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, &apimeta.NoKindMatchError{
					GroupKind: schema.GroupKind{Group: kustomizationGVR().Group, Kind: kindKustomization},
				}
			},
			wantReason:  ReasonApplierNotFound,
			wantMessage: "does not serve Kustomization (kustomize.toolkit.fluxcd.io/v1)",
		},
		{
			name: "kind not served, as the API server's bare 404 says it",
			reactor: func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewNotFound(kustomizationGVR().GroupResource(), "")
			},
			wantReason:  ReasonApplierNotFound,
			wantMessage: "is Flux installed there?",
		},
		{
			name:        "git repository missing",
			objs:        []runtime.Object{healthyKustomization},
			wantReason:  ReasonApplierNotFound,
			wantMessage: "GitRepository flux-system/platform not found in the source cluster (the sourceRef of",
		},
		{
			name: "source is a bucket",
			objs: []runtime.Object{kustomizationObj(fluxSystemNS, "checkout",
				map[string]any{"kind": "Bucket", "name": "platform"})},
			wantReason:  ReasonSourceMismatch,
			wantMessage: "applies from a Bucket, not a GitRepository",
		},
		{
			name: "another repository",
			objs: []runtime.Object{
				healthyKustomization,
				repo("https://github.com/acme/other", branchRef(testBranch)),
			},
			wantReason:  ReasonSourceMismatch,
			wantMessage: "fetches https://github.com/acme/other, not https://github.com/acme/platform",
		},
		{
			name:        "another branch",
			objs:        []runtime.Object{healthyKustomization, repo(testRepoURL, branchRef("release"))},
			wantReason:  ReasonSourceMismatch,
			wantMessage: `fetches branch "release", not main`,
		},
		{
			name:        "no ref",
			objs:        []runtime.Object{healthyKustomization, repo(testRepoURL, nil)},
			wantReason:  ReasonSourceMismatch,
			wantMessage: "names no spec.ref.branch",
		},
		{
			name:        "pinned tag",
			objs:        []runtime.Object{healthyKustomization, repo(testRepoURL, map[string]any{"tag": "v1.0.0"})},
			wantReason:  ReasonSourceMismatch,
			wantMessage: `pins spec.ref.tag "v1.0.0"`,
		},
		{
			name: "pinned commit beside the branch",
			objs: []runtime.Object{healthyKustomization,
				repo(testRepoURL, map[string]any{"branch": testBranch, "commit": testCommit})},
			wantReason:  ReasonSourceMismatch,
			wantMessage: "pins spec.ref.commit",
		},
		{
			name:        "may not patch the git repository",
			objs:        []runtime.Object{healthyKustomization, repo(testRepoURL, branchRef(testBranch))},
			canI:        denyOn("gitrepositories"),
			wantReason:  ReasonForbidden,
			wantMessage: "may not patch GitRepository flux-system/platform",
		},
		{
			name:        "may not patch the kustomization",
			objs:        []runtime.Object{healthyKustomization, repo(testRepoURL, branchRef(testBranch))},
			canI:        denyOn("kustomizations"),
			wantReason:  ReasonForbidden,
			wantMessage: "may not patch Kustomization flux-system/checkout",
		},
		{
			name: "may not get the kustomization",
			reactor: func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(kustomizationGVR().GroupResource(), "checkout",
					errors.New("denied"))
			},
			wantReason:  ReasonForbidden,
			wantMessage: "may not get Kustomization flux-system/checkout",
		},
		{
			name: "source cluster did not answer",
			reactor: func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewServiceUnavailable("etcd is sulking")
			},
			wantReason: ReasonCheckFailed,
			wantErr:    true,
		},
		{
			name: "access check did not answer",
			objs: []runtime.Object{healthyKustomization, repo(testRepoURL, branchRef(testBranch))},
			canI: func(context.Context, authorizationv1.ResourceAttributes) (bool, error) {
				return false, errors.New("timeout")
			},
			wantReason: ReasonCheckFailed,
			wantErr:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newSourceClient(tt.objs...)
			if tt.reactor != nil {
				client.PrependReactor("get", "kustomizations", tt.reactor)
			}
			flux := pinnedFlux(client)
			flux.CanI = allowAll
			if tt.canI != nil {
				flux.CanI = tt.canI
			}

			got := flux.Validate(context.Background(),
				types.NamespacedName{Namespace: fluxSystemNS, Name: "checkout"}, testBinding)
			assert.Equal(t, tt.wantReason, got.Reason, got.Message)
			assert.Equal(t, tt.wantReason == ReasonSucceeded, got.OK)
			assert.Contains(t, got.Message, tt.wantMessage)
			assert.Equal(t, tt.wantErr, got.Err != nil)
		})
	}
}

// TestFluxValidate_URLEquivalences: the GitRepository's url and the GitProvider's are compared as
// repositories, not as strings.
func TestFluxValidate_URLEquivalences(t *testing.T) {
	tests := []struct {
		name      string
		sourceURL string
		provider  string
		wantOK    bool
	}{
		{"identical", testRepoURL, testRepoURL, true},
		{"trailing .git", testRepoURL + ".git", testRepoURL, true},
		{"scp-style ssh", "git@github.com:acme/platform.git", testRepoURL, true},
		{"ssh url", "ssh://git@github.com/acme/platform", "git@github.com:acme/platform.git", true},
		{"case and trailing slash", "https://GitHub.com/Acme/Platform/", testRepoURL, true},
		{"default port", "https://github.com:443/acme/platform", testRepoURL, true},
		{"another org", "https://github.com/other/platform", testRepoURL, false},
		{"another host", "https://gitlab.com/acme/platform", testRepoURL, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newSourceClient(
				kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform")),
				gitRepositoryObj(fluxSystemNS, "platform", tt.sourceURL, branchRef(testBranch), ""),
			)
			flux := pinnedFlux(client)
			flux.CanI = allowAll
			got := flux.Validate(context.Background(),
				types.NamespacedName{Namespace: fluxSystemNS, Name: "checkout"},
				Binding{RepositoryURL: tt.provider, Branch: testBranch})
			assert.Equal(t, tt.wantOK, got.OK, got.Message)
		})
	}
}

// TestSelfSubjectAccessCheck_AsksTheSourceCluster: the default access check creates a
// SelfSubjectAccessReview with the attributes asked about and answers from its status.
func TestSelfSubjectAccessCheck_AsksTheSourceCluster(t *testing.T) {
	client := newSourceClient()
	var asked []string
	client.PrependReactor("create", "selfsubjectaccessreviews",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			review := action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
			resource, _, _ := unstructured.NestedString(review.Object, "spec", "resourceAttributes", "resource")
			verb, _, _ := unstructured.NestedString(review.Object, "spec", "resourceAttributes", "verb")
			asked = append(asked, verb+" "+resource)
			_ = unstructured.SetNestedField(review.Object, resource == "gitrepositories", "status", "allowed")
			return true, review, nil
		})
	check := SelfSubjectAccessCheck(client)

	allowed, err := check(context.Background(), authorizationv1.ResourceAttributes{
		Verb: "patch", Group: gitRepositoryGVR().Group, Resource: "gitrepositories", Namespace: fluxSystemNS,
	})
	require.NoError(t, err)
	assert.True(t, allowed)

	allowed, err = check(context.Background(), authorizationv1.ResourceAttributes{
		Verb: "patch", Group: kustomizationGVR().Group, Resource: "kustomizations", Namespace: fluxSystemNS,
	})
	require.NoError(t, err)
	assert.False(t, allowed)
	assert.Equal(t, []string{"patch gitrepositories", "patch kustomizations"}, asked)

	client.PrependReactor("create", "selfsubjectaccessreviews",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewServiceUnavailable("down")
		})
	_, err = check(context.Background(), authorizationv1.ResourceAttributes{Verb: "patch"})
	require.Error(t, err)
}
