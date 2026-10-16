// SPDX-License-Identifier: Apache-2.0

package applier

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

const (
	applyRequestsMetric = "gitopsreverser_apply_requests_total"
	targetNS            = "team-a"
)

var testBranchKey = git.BranchKey{RepoNamespace: targetNS, RepoName: "platform", Branch: testBranch}

// gitTarget builds a GitTarget on testBranchKey; a nil kustomizationRef declares no applier.
func gitTarget(name string, kustomizationRef *fluxmeta.NamespacedObjectReference, cluster string) *v1alpha3.GitTarget {
	target := &v1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Namespace: targetNS, Name: name, UID: uid(name)},
		Spec: v1alpha3.GitTargetSpec{
			GitProviderRef: fluxmeta.LocalObjectReference{Name: testBranchKey.RepoName},
			Branch:         testBranch,
			Path:           name,
		},
	}
	if kustomizationRef != nil {
		target.Spec.Applier = &v1alpha3.GitTargetApplier{KustomizationRef: *kustomizationRef}
	}
	if cluster != "" {
		target.Spec.ClusterProviderRef = &fluxmeta.LocalObjectReference{Name: cluster}
	}
	return target
}

// reapplying makes a GitTarget consent to a Refusal request.
func reapplying(target *v1alpha3.GitTarget) *v1alpha3.GitTarget {
	target.Spec.OnRefusal = v1alpha3.RefusalActionReapply
	return target
}

// quietFor sets a GitTarget's quiet period, which is also how long its requests are retried.
func quietFor(target *v1alpha3.GitTarget, quietPeriod time.Duration) *v1alpha3.GitTarget {
	target.Spec.Applier.QuietPeriod = &metav1.Duration{Duration: quietPeriod}
	return target
}

// clusterProvider admits the GitTargets in the named namespaces.
func clusterProvider(name string, admits ...string) *v1alpha3.ClusterProvider {
	return &v1alpha3.ClusterProvider{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha3.ClusterProviderSpec{AccessFrom: &v1alpha3.NamespaceMatcher{Names: admits}},
	}
}

// controlPlane is what every dispatcher test reads besides its GitTargets: the GitProvider of
// testBranchKey, and the ClusterProviders the tests name, admitting targetNS.
func controlPlane() []client.Object {
	return []client.Object{
		&v1alpha3.GitProvider{
			ObjectMeta: metav1.ObjectMeta{Namespace: testBranchKey.RepoNamespace, Name: testBranchKey.RepoName},
			Spec:       v1alpha3.GitProviderSpec{URL: testRepoURL},
		},
		clusterProvider(v1alpha3.DefaultClusterProviderName, targetNS),
		clusterProvider("prod-eu-1", targetNS),
	}
}

func ksRef(namespace, name string) *fluxmeta.NamespacedObjectReference {
	return &fluxmeta.NamespacedObjectReference{Namespace: namespace, Name: name}
}

func ref(name string) itypes.ResourceReference { return itypes.NewResourceReference(name, targetNS) }

// uid is the uid gitTarget gives the GitTarget of that name.
func uid(name string) types.UID { return types.UID("uid-" + name) }

// sourceStub hands out one fake source cluster's client, failing the first failFirst calls (or
// every call, with failFirst < 0), and remembers which cluster ids it was asked for.
type sourceStub struct {
	client    *dynamicfake.FakeDynamicClient
	mu        sync.Mutex
	failFirst int
	calls     int
	clusters  []string
}

func (s *sourceStub) get(_ context.Context, clusterID string) (dynamic.Interface, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.clusters = append(s.clusters, clusterID)
	if s.failFirst < 0 || s.calls <= s.failFirst {
		return nil, fmt.Errorf("source cluster unreachable (call %d)", s.calls)
	}
	return s.client, nil
}

func (s *sourceStub) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type dispatcherFixture struct {
	dispatcher *Dispatcher
	control    client.Client
	source     *sourceStub
	recorder   *record.FakeRecorder
	reader     *sdkmetric.ManualReader
	handBacks  *handBackRecorder
}

// handBackRecorder records the requests the dispatcher hands back.
type handBackRecorder struct {
	mu       sync.Mutex
	requests []git.ApplyRequest
}

func (h *handBackRecorder) HandBackApply(req git.ApplyRequest) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.requests = append(h.requests, req)
}

func (h *handBackRecorder) got() []git.ApplyRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]git.ApplyRequest(nil), h.requests...)
}

func newDispatcherFixture(t *testing.T, targets []client.Object, sourceObjs ...runtime.Object) *dispatcherFixture {
	t.Helper()
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, v1alpha3.AddToScheme(scheme))
	controlClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(append(controlPlane(), targets...)...).Build()

	source := &sourceStub{client: newSourceClient(sourceObjs...)}
	recorder := record.NewFakeRecorder(64)
	handBacks := &handBackRecorder{}
	d := NewDispatcher(controlClient, source.get, recorder, logr.Discard())
	d.Backoff = Backoff{Initial: time.Millisecond, Max: 4 * time.Millisecond}
	d.Now = func() time.Time { return stampTime }
	d.HandBack = handBacks
	return &dispatcherFixture{
		dispatcher: d, control: controlClient, source: source, recorder: recorder, reader: reader, handBacks: handBacks,
	}
}

// start runs the dispatcher until the test ends.
func (f *dispatcherFixture) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = f.dispatcher.Start(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func (f *dispatcherFixture) count(reason v1alpha3.ApplyReason, outcome string) int64 {
	value, _ := telemetry.CollectInt64Sum(f.reader, applyRequestsMetric, map[string]string{
		"applier_kind": applierKindKustomization, "reason": string(reason), "outcome": outcome,
	})
	return value
}

// waitForCount waits until the counter reaches want.
func (f *dispatcherFixture) waitForCount(t *testing.T, reason v1alpha3.ApplyReason, outcome string, want int64) {
	t.Helper()
	require.Eventually(t, func() bool { return f.count(reason, outcome) == want },
		5*time.Second, time.Millisecond, "apply_requests_total{reason=%s,outcome=%s} never reached %d (at %d)",
		reason, outcome, want, f.count(reason, outcome))
}

// events drains the recorded Events.
func (f *dispatcherFixture) events() []string {
	var out []string
	for {
		select {
		case event := <-f.recorder.Events:
			out = append(out, event)
		default:
			return out
		}
	}
}

func (f *dispatcherFixture) patches() []string {
	var out []string
	for _, action := range f.source.client.Actions() {
		if action.GetVerb() == "patch" {
			out = append(out, action.GetResource().Resource+" "+action.GetNamespace())
		}
	}
	return out
}

// TestDispatcher_AsksEachApplierOnce: targets sharing an applier ask it once; a target with no
// applier, or one that is gone, asks nothing; an omitted kustomizationRef namespace is the target's.
func TestDispatcher_AsksEachApplierOnce(t *testing.T) {
	f := newDispatcherFixture(t,
		[]client.Object{
			gitTarget("a", ksRef(fluxSystemNS, "checkout"), ""),
			gitTarget("b", ksRef(fluxSystemNS, "checkout"), ""),
			gitTarget("c", ksRef("", "own"), "prod-eu-1"),
			gitTarget("no-applier", nil, ""),
		},
		kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform")),
		gitRepositoryObj(fluxSystemNS, "platform", testRepoURL, branchRef(testBranch), "main@sha1:"+olderCommit),
		kustomizationObj(targetNS, "own", gitRepoSourceRef("own")),
		gitRepositoryObj(targetNS, "own", testRepoURL, branchRef(testBranch), "main@sha1:"+olderCommit),
	)
	f.start(t)

	f.dispatcher.RequestApply(git.ApplyRequest{
		Branch: testBranchKey, Reason: v1alpha3.ApplyReasonForeignCommit, Commit: testCommit,
		Targets: []itypes.ResourceReference{ref("a"), ref("b"), ref("c"), ref("no-applier"), ref("deleted")},
	})
	f.waitForCount(t, v1alpha3.ApplyReasonForeignCommit, outcomeSent, 2)

	assert.ElementsMatch(t, []string{"gitrepositories " + fluxSystemNS, "gitrepositories " + targetNS}, f.patches(),
		"one source stamp per applier, however many targets share it")
	assert.ElementsMatch(t, []string{v1alpha3.DefaultClusterProviderName, "prod-eu-1"}, f.source.clusters,
		"each applier is looked up in its target's source cluster")

	want := v1alpha3.GitTargetApplierStatus{
		RequestedCommit: testCommit,
		RequestedAt:     &metav1.Time{Time: stampTime},
		Reason:          v1alpha3.ApplyReasonForeignCommit,
	}
	for _, name := range []string{"a", "b", "c"} {
		got, ok := f.dispatcher.LastRequest(ref(name), uid(name))
		require.True(t, ok, name)
		assert.True(t, got.RequestedAt.Equal(want.RequestedAt), name)
		assert.Equal(t, want.RequestedCommit, got.RequestedCommit, name)
		assert.Equal(t, want.Reason, got.Reason, name)
	}
	_, ok := f.dispatcher.LastRequest(ref("no-applier"), uid("no-applier"))
	assert.False(t, ok, "a target without an applier records no request")

	assert.ElementsMatch(t, []string{
		"Normal ApplyRequested asked flux-system/checkout to apply 4f2c1ab9 (ForeignCommit)",
		"Normal ApplyRequested asked flux-system/checkout to apply 4f2c1ab9 (ForeignCommit)",
		"Normal ApplyRequested asked team-a/own to apply 4f2c1ab9 (ForeignCommit)",
	}, f.events())
}

// TestDispatcher_RetriesThenDelivers: a failed attempt is retried on the backoff, and only the
// delivery counts.
func TestDispatcher_RetriesThenDelivers(t *testing.T) {
	f := newDispatcherFixture(t,
		[]client.Object{reapplying(gitTarget("a", ksRef(fluxSystemNS, "checkout"), ""))},
		kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform")),
		gitRepositoryObj(fluxSystemNS, "platform", testRepoURL, branchRef(testBranch), "main@sha1:"+testCommit),
	)
	f.source.failFirst = 2
	f.start(t)

	f.dispatcher.RequestApply(git.ApplyRequest{
		Branch: testBranchKey, Reason: v1alpha3.ApplyReasonRefusal, Commit: testCommit,
		Targets: []itypes.ResourceReference{ref("a")},
	})
	f.waitForCount(t, v1alpha3.ApplyReasonRefusal, outcomeSent, 1)

	assert.Equal(t, 3, f.source.callCount(), "two failures, then the attempt that landed")
	assert.Equal(t, int64(0), f.count(v1alpha3.ApplyReasonRefusal, outcomeFailed))
	assert.Equal(t, []string{"kustomizations " + fluxSystemNS}, f.patches(),
		"a refusal at the artifact's commit stamps the kustomization")
	got, ok := f.dispatcher.LastRequest(ref("a"), uid("a"))
	require.True(t, ok)
	assert.Equal(t, v1alpha3.ApplyReasonRefusal, got.Reason)
}

// TestDispatcher_TheLedgerIsPerIncarnation: a GitTarget deleted and recreated under the same name
// does not inherit its predecessor's last request, and a forgotten one holds nothing.
func TestDispatcher_TheLedgerIsPerIncarnation(t *testing.T) {
	f := newDispatcherFixture(t,
		[]client.Object{gitTarget("a", ksRef(fluxSystemNS, "checkout"), "")},
		kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform")),
		gitRepositoryObj(fluxSystemNS, "platform", testRepoURL, branchRef(testBranch), "main@sha1:"+olderCommit),
	)
	f.start(t)
	f.dispatcher.RequestApply(git.ApplyRequest{
		Branch: testBranchKey, Reason: v1alpha3.ApplyReasonForeignCommit, Commit: testCommit,
		Targets: []itypes.ResourceReference{ref("a")},
	})
	f.waitForCount(t, v1alpha3.ApplyReasonForeignCommit, outcomeSent, 1)

	_, ok := f.dispatcher.LastRequest(ref("a"), uid("a"))
	assert.True(t, ok)
	_, ok = f.dispatcher.LastRequest(ref("a"), "the-recreated-one")
	assert.False(t, ok, "a recreated GitTarget does not inherit the last request")
	f.dispatcher.Forget(ref("a"))
	_, ok = f.dispatcher.LastRequest(ref("a"), uid("a"))
	assert.False(t, ok)
}

// TestDispatcher_HandsBackAfterItsRetryBudget is the review's finding 4: a request that kept
// failing was given up on, while the worker had already cleared its debt, so nothing asked again
// until an unrelated commit. It is now retried for about one quiet period and handed back to the
// worker, and the failure is reported once, not again when the worker's next request fails too.
func TestDispatcher_HandsBackAfterItsRetryBudget(t *testing.T) {
	f := newDispatcherFixture(t, []client.Object{
		quietFor(gitTarget("a", ksRef(fluxSystemNS, "checkout"), ""), 20*time.Millisecond),
		gitTarget("b", ksRef(fluxSystemNS, "checkout"), ""),
	})
	f.source.failFirst = -1
	f.start(t)

	f.dispatcher.RequestApply(git.ApplyRequest{
		Branch: testBranchKey, Reason: v1alpha3.ApplyReasonForeignCommit, Commit: testCommit,
		Targets: []itypes.ResourceReference{ref("a"), ref("b")},
	})
	f.waitForCount(t, v1alpha3.ApplyReasonForeignCommit, outcomeFailed, 1)

	assert.Greater(t, f.source.callCount(), 1, "retried within the budget before handing back")
	require.Eventually(t, func() bool { return len(f.handBacks.got()) == 1 }, 5*time.Second, time.Millisecond)
	assert.Equal(t, git.ApplyRequest{
		Branch: testBranchKey, Reason: v1alpha3.ApplyReasonForeignCommit, Commit: testCommit,
		Targets: []itypes.ResourceReference{ref("a"), ref("b")}, HandBacks: 1,
	}, f.handBacks.got()[0], "every GitTarget goes back, with its reason and one more hand-back")
	_, ok := f.dispatcher.LastRequest(ref("a"), uid("a"))
	assert.False(t, ok, "nothing was delivered")
	events := f.events()
	require.Len(t, events, 2, "one Warning per GitTarget")
	const want = "Warning ApplyRequestFailed could not ask flux-system/checkout to apply 4f2c1ab9 (ForeignCommit)"
	assert.True(t, strings.HasPrefix(events[0], want), events[0])

	// The worker sends it again, and it fails again.
	f.dispatcher.RequestApply(git.ApplyRequest{
		Branch: testBranchKey, Reason: v1alpha3.ApplyReasonForeignCommit, Commit: testCommit,
		Targets: []itypes.ResourceReference{ref("a"), ref("b")}, HandBacks: 1,
	})
	require.Eventually(t, func() bool { return len(f.handBacks.got()) == 2 }, 5*time.Second, time.Millisecond)
	assert.Equal(t, 2, f.handBacks.got()[1].HandBacks)
	assert.Empty(t, f.events(), "a failure already reported is not reported again")
}

// TestDispatcher_PermanentFailureIsHandedBackAtOnce: a Kustomization a request cannot work with is
// not retried; the worker asks again later, in case it was fixed.
func TestDispatcher_PermanentFailureIsHandedBackAtOnce(t *testing.T) {
	f := newDispatcherFixture(t,
		[]client.Object{gitTarget("a", ksRef(fluxSystemNS, "checkout"), "")},
		kustomizationObj(fluxSystemNS, "checkout", map[string]any{"kind": "Bucket", "name": "platform"}),
	)
	f.start(t)

	f.dispatcher.RequestApply(git.ApplyRequest{
		Branch: testBranchKey, Reason: v1alpha3.ApplyReasonForeignCommit, Commit: testCommit,
		Targets: []itypes.ResourceReference{ref("a")},
	})
	f.waitForCount(t, v1alpha3.ApplyReasonForeignCommit, outcomeFailed, 1)
	assert.Equal(t, 1, f.source.callCount())
	require.Eventually(t, func() bool { return len(f.handBacks.got()) == 1 }, 5*time.Second, time.Millisecond)
}

// TestDispatcher_ASourceMismatchIsNeverStamped is the review's finding 2: ApplierConfigured said
// SourceMismatch, and a foreign commit stamped the GitRepository of the other branch anyway.
func TestDispatcher_ASourceMismatchIsNeverStamped(t *testing.T) {
	f := newDispatcherFixture(t,
		[]client.Object{gitTarget("a", ksRef(fluxSystemNS, "checkout"), "")},
		kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform")),
		gitRepositoryObj(fluxSystemNS, "platform", testRepoURL, branchRef("release"), "release@sha1:"+olderCommit),
	)
	f.start(t)

	f.dispatcher.RequestApply(git.ApplyRequest{
		Branch: testBranchKey, Reason: v1alpha3.ApplyReasonForeignCommit, Commit: testCommit,
		Targets: []itypes.ResourceReference{ref("a")},
	})
	f.waitForCount(t, v1alpha3.ApplyReasonForeignCommit, outcomeFailed, 1)
	assert.Empty(t, f.patches(), "a source that follows another branch is never stamped")
	assert.Equal(t, 1, f.source.callCount(), "and a mismatch is not retried")
}

// TestDispatcher_ATargetItsClusterProviderDeniesAsksNothing is the review's finding 1: the source
// client is shared by every GitTarget that declared the cluster, so a GitTarget its ClusterProvider
// does not admit drove the applier on another GitTarget's admission.
func TestDispatcher_ATargetItsClusterProviderDeniesAsksNothing(t *testing.T) {
	f := newDispatcherFixture(t,
		[]client.Object{
			gitTarget("admitted", ksRef(fluxSystemNS, "checkout"), ""),
			gitTarget("denied", ksRef(fluxSystemNS, "restricted"), "restricted"),
			clusterProvider("restricted", "some-other-namespace"),
		},
		kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform")),
		kustomizationObj(fluxSystemNS, "restricted", gitRepoSourceRef("platform")),
		gitRepositoryObj(fluxSystemNS, "platform", testRepoURL, branchRef(testBranch), "main@sha1:"+testCommit),
	)
	f.start(t)

	f.dispatcher.RequestApply(git.ApplyRequest{
		Branch: testBranchKey, Reason: v1alpha3.ApplyReasonForeignCommit, Commit: testCommit,
		Targets: []itypes.ResourceReference{ref("admitted"), ref("denied")},
	})
	f.waitForCount(t, v1alpha3.ApplyReasonForeignCommit, outcomeSent, 1)
	f.waitForCount(t, v1alpha3.ApplyReasonForeignCommit, outcomeDropped, 1)

	assert.Equal(t, []string{v1alpha3.DefaultClusterProviderName}, f.source.clusters,
		"the denied GitTarget's cluster client is never even asked for")
	_, ok := f.dispatcher.LastRequest(ref("denied"), uid("denied"))
	assert.False(t, ok)
}

// TestDispatcher_EveryRetryChecksAgain: a GitTarget admitted when its request was decided, and
// denied before a retry, is not asked for on the retry. The same holds for a GitTarget that moved
// to another branch.
func TestDispatcher_EveryRetryChecksAgain(t *testing.T) {
	for name, change := range map[string]func(ctx context.Context, c client.Client) error{
		"its ClusterProvider stops admitting it": func(ctx context.Context, c client.Client) error {
			provider := &v1alpha3.ClusterProvider{}
			if err := c.Get(ctx, client.ObjectKey{Name: v1alpha3.DefaultClusterProviderName}, provider); err != nil {
				return err
			}
			provider.Spec.AccessFrom.Names = []string{"some-other-namespace"}
			return c.Update(ctx, provider)
		},
		"it moves to another branch": func(ctx context.Context, c client.Client) error {
			target := &v1alpha3.GitTarget{}
			if err := c.Get(ctx, client.ObjectKey{Namespace: targetNS, Name: "a"}, target); err != nil {
				return err
			}
			target.Spec.Branch = "release"
			return c.Update(ctx, target)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newDispatcherFixture(
				t,
				[]client.Object{gitTarget("a", ksRef(fluxSystemNS, "checkout"), "")},
				kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform")),
				gitRepositoryObj(
					fluxSystemNS,
					"platform",
					testRepoURL,
					branchRef(testBranch),
					"main@sha1:"+olderCommit,
				),
			)
			f.dispatcher.Backoff = Backoff{Initial: 50 * time.Millisecond, Max: 50 * time.Millisecond}
			f.source.failFirst = 1
			f.start(t)

			f.dispatcher.RequestApply(git.ApplyRequest{
				Branch: testBranchKey, Reason: v1alpha3.ApplyReasonForeignCommit, Commit: testCommit,
				Targets: []itypes.ResourceReference{ref("a")},
			})
			require.Eventually(t, func() bool { return f.source.callCount() == 1 }, 5*time.Second, time.Millisecond)
			require.NoError(t, change(context.Background(), f.control))

			f.waitForCount(t, v1alpha3.ApplyReasonForeignCommit, outcomeDropped, 1)
			assert.Empty(t, f.patches())
		})
	}
}

// TestDispatcher_ARefusalNeedsConsentAtEveryAttempt is the dispatcher's half of the review's
// finding 3: consent is per GitTarget, so a GitTarget suspended after its Refusal was decided is
// asked only a CatchUp, while a sibling on the same applier that still consents keeps its Refusal.
func TestDispatcher_ARefusalNeedsConsentAtEveryAttempt(t *testing.T) {
	suspended := reapplying(gitTarget("suspended", ksRef(fluxSystemNS, "checkout"), ""))
	suspended.Spec.Suspend = true
	f := newDispatcherFixture(t,
		[]client.Object{
			suspended,
			reapplying(gitTarget("consenting", ksRef(fluxSystemNS, "checkout"), "")),
			reapplying(gitTarget("alone", ksRef(fluxSystemNS, "alone"), "")),
		},
		kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform")),
		kustomizationObj(fluxSystemNS, "alone", gitRepoSourceRef("platform")),
		gitRepositoryObj(fluxSystemNS, "platform", testRepoURL, branchRef(testBranch), "main@sha1:"+testCommit),
	)
	f.start(t)

	f.dispatcher.RequestApply(git.ApplyRequest{
		Branch: testBranchKey, Reason: v1alpha3.ApplyReasonRefusal, Commit: testCommit,
		Targets: []itypes.ResourceReference{ref("suspended"), ref("consenting")},
	})
	f.waitForCount(t, v1alpha3.ApplyReasonRefusal, outcomeSent, 1)
	consenting, ok := f.dispatcher.LastRequest(ref("consenting"), uid("consenting"))
	require.True(t, ok)
	assert.Equal(t, v1alpha3.ApplyReasonRefusal, consenting.Reason)
	suspendedStatus, ok := f.dispatcher.LastRequest(ref("suspended"), uid("suspended"))
	require.True(t, ok, "the shared stamp reached its applier too")
	assert.Equal(t, v1alpha3.ApplyReasonCatchUp, suspendedStatus.Reason, "recorded as what it may ask for")

	// A Refusal whose only GitTarget withdrew consent is a CatchUp, which an applier already at the
	// commit satisfies: the only stamp is the one the first request made.
	alone := &v1alpha3.GitTarget{}
	require.NoError(t, f.control.Get(context.Background(), client.ObjectKey{Namespace: targetNS, Name: "alone"}, alone))
	alone.Spec.OnRefusal = v1alpha3.RefusalActionIgnore
	require.NoError(t, f.control.Update(context.Background(), alone))
	f.dispatcher.RequestApply(git.ApplyRequest{
		Branch: testBranchKey, Reason: v1alpha3.ApplyReasonRefusal, Commit: testCommit,
		Targets: []itypes.ResourceReference{ref("alone")},
	})
	f.waitForCount(t, v1alpha3.ApplyReasonCatchUp, outcomeSent, 1)
	assert.Equal(t, []string{"kustomizations " + fluxSystemNS}, f.patches())
}

// TestDispatcher_NewerRequestSupersedes: a request waiting on its retry is replaced by a newer one
// for the same applier, which is sent at once with the newer commit. The newer one is a CatchUp
// and the older a Refusal, so it keeps the Refusal's reason: the artifact already holds the newer
// commit, and a CatchUp would then stamp nothing and drop the revert.
func TestDispatcher_NewerRequestSupersedes(t *testing.T) {
	f := newDispatcherFixture(t,
		[]client.Object{reapplying(gitTarget("a", ksRef(fluxSystemNS, "checkout"), ""))},
		kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform")),
		gitRepositoryObj(fluxSystemNS, "platform", testRepoURL, branchRef(testBranch), "main@sha1:"+testCommit),
	)
	f.dispatcher.Backoff = Backoff{Initial: time.Hour, Max: time.Hour}
	f.source.failFirst = 1
	f.start(t)

	f.dispatcher.RequestApply(git.ApplyRequest{
		Branch: testBranchKey, Reason: v1alpha3.ApplyReasonRefusal, Commit: olderCommit,
		Targets: []itypes.ResourceReference{ref("a")},
	})
	require.Eventually(t, func() bool { return f.source.callCount() == 1 }, 5*time.Second, time.Millisecond)

	f.dispatcher.RequestApply(git.ApplyRequest{
		Branch: testBranchKey, Reason: v1alpha3.ApplyReasonCatchUp, Commit: testCommit,
		Targets: []itypes.ResourceReference{ref("a")},
	})
	f.waitForCount(t, v1alpha3.ApplyReasonRefusal, outcomeSent, 1)

	assert.Equal(t, int64(1), f.count(v1alpha3.ApplyReasonRefusal, outcomeSuperseded),
		"the failed Refusal at the older commit was superseded, not retried")
	assert.Equal(t, int64(0), f.count(v1alpha3.ApplyReasonCatchUp, outcomeSent))
	assert.Equal(t, []string{"kustomizations " + fluxSystemNS}, f.patches())
	got, ok := f.dispatcher.LastRequest(ref("a"), uid("a"))
	require.True(t, ok)
	assert.Equal(t, testCommit, got.RequestedCommit, "the newer commit is the one sent")
	assert.Equal(t, v1alpha3.ApplyReasonRefusal, got.Reason)
}

// TestDispatcher_CatchUpAlreadyCurrent: a CatchUp the applier already satisfies counts as sent and
// records nothing, since nothing was asked of it.
func TestDispatcher_CatchUpAlreadyCurrent(t *testing.T) {
	f := newDispatcherFixture(t,
		[]client.Object{gitTarget("a", ksRef(fluxSystemNS, "checkout"), "")},
		kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform")),
		gitRepositoryObj(fluxSystemNS, "platform", testRepoURL, branchRef(testBranch), "main@sha1:"+testCommit),
	)
	f.start(t)

	f.dispatcher.RequestApply(git.ApplyRequest{
		Branch: testBranchKey, Reason: v1alpha3.ApplyReasonCatchUp, Commit: testCommit,
		Targets: []itypes.ResourceReference{ref("a")},
	})
	f.waitForCount(t, v1alpha3.ApplyReasonCatchUp, outcomeSent, 1)

	assert.Empty(t, f.patches())
	assert.Empty(t, f.events())
	_, ok := f.dispatcher.LastRequest(ref("a"), uid("a"))
	assert.False(t, ok)
}

// TestDispatcher_RequestApplyNeverBlocks: with no loop running, requests queue without blocking; an
// identical scope is replaced in place, and past the bound the oldest is dropped.
func TestDispatcher_RequestApplyNeverBlocks(t *testing.T) {
	f := newDispatcherFixture(t, nil)
	d := f.dispatcher

	same := func(commit string) git.ApplyRequest {
		return git.ApplyRequest{Branch: testBranchKey, Reason: v1alpha3.ApplyReasonForeignCommit, Commit: commit,
			Targets: []itypes.ResourceReference{ref("a"), ref("b")}}
	}
	d.RequestApply(same(olderCommit))
	reordered := same(testCommit)
	reordered.Targets = []itypes.ResourceReference{ref("b"), ref("a")}
	d.RequestApply(reordered)
	d.RequestApply(git.ApplyRequest{Branch: testBranchKey, Reason: v1alpha3.ApplyReasonForeignCommit})

	d.inboxMu.Lock()
	require.Len(t, d.inbox, 1, "an identical scope is replaced, and an empty request is ignored")
	assert.Equal(t, testCommit, d.inbox[0].Commit)
	d.inboxMu.Unlock()

	for i := range maxInbox + 10 {
		d.RequestApply(git.ApplyRequest{Branch: testBranchKey, Reason: v1alpha3.ApplyReasonRefusal,
			Commit: testCommit, Targets: []itypes.ResourceReference{ref(fmt.Sprintf("t%d", i))}})
	}
	d.inboxMu.Lock()
	defer d.inboxMu.Unlock()
	assert.Len(t, d.inbox, maxInbox)
	assert.Equal(t, "t10", d.inbox[0].Targets[0].Name, "the oldest requests were dropped")
}

func TestBackoff_Delay(t *testing.T) {
	b := Backoff{Initial: 2 * time.Second, Max: time.Minute}
	for failures, want := range map[int]time.Duration{
		1: 2 * time.Second, 2: 4 * time.Second, 3: 8 * time.Second, 5: 32 * time.Second, 6: time.Minute, 20: time.Minute,
	} {
		assert.Equal(t, want, b.delay(failures), "after %d failures", failures)
	}
}

func TestStrongerApplyReason(t *testing.T) {
	tests := []struct{ older, newer, want v1alpha3.ApplyReason }{
		{v1alpha3.ApplyReasonRefusal, v1alpha3.ApplyReasonCatchUp, v1alpha3.ApplyReasonRefusal},
		{v1alpha3.ApplyReasonForeignCommit, v1alpha3.ApplyReasonCatchUp, v1alpha3.ApplyReasonForeignCommit},
		{v1alpha3.ApplyReasonCatchUp, v1alpha3.ApplyReasonCatchUp, v1alpha3.ApplyReasonCatchUp},
		{v1alpha3.ApplyReasonCatchUp, v1alpha3.ApplyReasonRefusal, v1alpha3.ApplyReasonRefusal},
		{v1alpha3.ApplyReasonRefusal, v1alpha3.ApplyReasonForeignCommit, v1alpha3.ApplyReasonForeignCommit},
		{v1alpha3.ApplyReasonForeignCommit, v1alpha3.ApplyReasonRefusal, v1alpha3.ApplyReasonForeignCommit},
		{"", v1alpha3.ApplyReasonCatchUp, v1alpha3.ApplyReasonCatchUp},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, git.StrongerApplyReason(tt.older, tt.newer), "%s then %s", tt.older, tt.newer)
	}
}

func TestDescribeCommit(t *testing.T) {
	assert.Equal(t, "4f2c1ab9", describeCommit(testCommit))
	assert.Equal(t, "abc", describeCommit("abc"))
	assert.Equal(t, "the branch, which the remote does not carry", describeCommit(""))
}
