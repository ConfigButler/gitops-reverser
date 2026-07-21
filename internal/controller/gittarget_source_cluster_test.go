// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/watch"
)

func TestNamespaceToGitTargets(t *testing.T) {
	gt := func(name, ns string) *configbutleraiv1alpha3.GitTarget {
		return &configbutleraiv1alpha3.GitTarget{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	}
	cl := fake.NewClientBuilder().WithScheme(scScheme(t)).
		WithObjects(gt("a", "team-a"), gt("b", "team-a"), gt("c", "team-b")).Build()
	r := &GitTargetReconciler{Client: cl}

	reqs := r.namespaceToGitTargets(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}})
	// A label change on team-a re-enqueues exactly the GitTargets IN team-a, so a selector-based
	// authorization revocation converges without waiting for the periodic reconcile.
	names := []string{}
	for _, req := range reqs {
		assert.Equal(t, "team-a", req.Namespace)
		names = append(names, req.Name)
	}
	assert.ElementsMatch(t, []string{"a", "b"}, names)
}

func TestClusterProviderReadyOrSpecChanged(t *testing.T) {
	p := clusterProviderReadyOrSpecChanged()
	cp := func(gen int64, ready metav1.ConditionStatus) *configbutleraiv1alpha3.ClusterProvider {
		c := &configbutleraiv1alpha3.ClusterProvider{
			ObjectMeta: metav1.ObjectMeta{Name: "default", Generation: gen},
		}
		if ready != "" {
			c.Status.Conditions = []metav1.Condition{{Type: ConditionTypeReady, Status: ready}}
		}
		return c
	}

	assert.True(t, p.Update(event.UpdateEvent{
		ObjectOld: cp(1, metav1.ConditionUnknown), ObjectNew: cp(1, metav1.ConditionTrue),
	}), "a Ready flip (status-only, same generation) must fire")
	assert.False(t, p.Update(event.UpdateEvent{
		ObjectOld: cp(1, metav1.ConditionTrue), ObjectNew: cp(1, metav1.ConditionTrue),
	}), "no spec/Ready change must not fire")
	assert.True(t, p.Update(event.UpdateEvent{
		ObjectOld: cp(1, metav1.ConditionTrue), ObjectNew: cp(2, metav1.ConditionTrue),
	}), "a spec (generation) change must fire")
	assert.True(t, p.Create(event.CreateEvent{}))
	assert.True(t, p.Delete(event.DeleteEvent{}))
}

const scValidKubeConfig = `apiVersion: v1
kind: Config
clusters:
- {name: c, cluster: {server: https://192.0.2.1:6443, certificate-authority-data: dGVzdA==}}
contexts:
- {name: c, context: {cluster: c, user: u}}
current-context: c
users:
- {name: u, user: {token: t}}
`

const scExecKubeConfig = `apiVersion: v1
kind: Config
clusters:
- {name: c, cluster: {server: https://192.0.2.1:6443}}
contexts:
- {name: c, context: {cluster: c, user: u}}
current-context: c
users:
- name: u
  user:
    exec: {apiVersion: client.authentication.k8s.io/v1, command: /bin/echo, interactiveMode: Never}
`

const scInsecureKubeConfig = `apiVersion: v1
kind: Config
clusters:
- {name: c, cluster: {server: https://192.0.2.1:6443, insecure-skip-tls-verify: true}}
contexts:
- {name: c, context: {cluster: c, user: u}}
current-context: c
users:
- {name: u, user: {token: t}}
`

func scScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, configbutleraiv1alpha3.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

func TestCheckSourceAuthorization(t *testing.T) {
	provider := func(policy *configbutleraiv1alpha3.NamespaceMatcher) *configbutleraiv1alpha3.ClusterProvider {
		return &configbutleraiv1alpha3.ClusterProvider{
			ObjectMeta: metav1.ObjectMeta{Name: "prod-eu-1"},
			Spec:       configbutleraiv1alpha3.ClusterProviderSpec{AllowedNamespaces: policy},
		}
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a", Labels: map[string]string{"tier": "trusted"}}}
	target := func(providerName string) *configbutleraiv1alpha3.GitTarget {
		return &configbutleraiv1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "gt", Namespace: "team-a"},
			Spec: configbutleraiv1alpha3.GitTargetSpec{
				ClusterProviderRef: &configbutleraiv1alpha3.ClusterProviderReference{Name: providerName},
			},
		}
	}

	tests := []struct {
		name           string
		objects        []client.Object
		providerRef    string
		wantAuthorized bool
		wantReason     string
	}{
		{
			name:           "provider not found -> hard gate (NotReady, no mirroring)",
			objects:        []client.Object{ns},
			providerRef:    "absent",
			wantAuthorized: false,
			wantReason:     GitTargetReasonClusterProviderNotFound,
		},
		{
			name: "provider allows the namespace by name",
			objects: []client.Object{
				provider(&configbutleraiv1alpha3.NamespaceMatcher{Names: []string{"team-a"}}), ns,
			},
			providerRef: "prod-eu-1", wantAuthorized: true,
		},
		{
			name: "provider does not allow the namespace -> refused",
			objects: []client.Object{
				provider(&configbutleraiv1alpha3.NamespaceMatcher{Names: []string{"team-b"}}), ns,
			},
			providerRef: "prod-eu-1", wantAuthorized: false, wantReason: GitTargetReasonNamespaceNotAuthorized,
		},
		{
			name: "provider allows the namespace by label selector",
			objects: []client.Object{
				provider(&configbutleraiv1alpha3.NamespaceMatcher{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "trusted"}},
				}), ns,
			},
			providerRef: "prod-eu-1", wantAuthorized: true,
		},
		{
			// A selector the API accepted but that cannot compile must FAIL CLOSED. Treating an
			// unevaluatable policy as "allow" would hand a namespace access it was never granted.
			name: "invalid allowedNamespaces selector -> refused, not allowed",
			objects: []client.Object{
				provider(&configbutleraiv1alpha3.NamespaceMatcher{
					Selector: &metav1.LabelSelector{
						MatchExpressions: []metav1.LabelSelectorRequirement{
							{Key: "tier", Operator: "BogusOperator", Values: []string{"trusted"}},
						},
					},
				}), ns,
			},
			providerRef: "prod-eu-1", wantAuthorized: false, wantReason: GitTargetReasonNamespaceNotAuthorized,
		},
		{
			// A missing Namespace object is not an error: the policy is still evaluated, just with
			// no labels. A name-based allow still works; a selector-only policy then denies.
			name: "namespace object absent -> evaluated with no labels, name allow still holds",
			objects: []client.Object{
				provider(&configbutleraiv1alpha3.NamespaceMatcher{Names: []string{"team-a"}}),
			},
			providerRef: "prod-eu-1", wantAuthorized: true,
		},
		{
			name: "namespace object absent -> selector-only policy denies (no labels to match)",
			objects: []client.Object{
				provider(&configbutleraiv1alpha3.NamespaceMatcher{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "trusted"}},
				}),
			},
			providerRef: "prod-eu-1", wantAuthorized: false, wantReason: GitTargetReasonNamespaceNotAuthorized,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cl := fake.NewClientBuilder().WithScheme(scScheme(t)).WithObjects(tc.objects...).Build()
			r := &GitTargetReconciler{Client: cl}
			ok, reason, msg, err := r.checkSourceAuthorization(context.Background(), target(tc.providerRef))
			require.NoError(t, err)
			assert.Equal(t, tc.wantAuthorized, ok)
			if !tc.wantAuthorized {
				assert.Equal(t, tc.wantReason, reason)
				assert.NotEmpty(t, msg)
			}
		})
	}
}

// TestCheckSourceAuthorization_ReadErrorsRequeue pins the fail-closed contract for TRANSIENT
// failures. A read error is not a verdict: the gate must return an error so the reconcile requeues,
// rather than returning authorized=false (which would look like a policy denial and flap the
// GitTarget's status) or authorized=true (which would open a watch on an unevaluated policy).
func TestCheckSourceAuthorization_ReadErrorsRequeue(t *testing.T) {
	target := &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "gt", Namespace: "team-a"},
		Spec: configbutleraiv1alpha3.GitTargetSpec{
			ClusterProviderRef: &configbutleraiv1alpha3.ClusterProviderReference{Name: "prod-eu-1"},
		},
	}
	provider := &configbutleraiv1alpha3.ClusterProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-eu-1"},
		Spec: configbutleraiv1alpha3.ClusterProviderSpec{
			AllowedNamespaces: &configbutleraiv1alpha3.NamespaceMatcher{Names: []string{"team-a"}},
		},
	}

	tests := []struct {
		name    string
		failOn  func(client.Object) bool
		wantErr string
	}{
		{
			name:    "ClusterProvider read fails",
			failOn:  func(o client.Object) bool { _, ok := o.(*configbutleraiv1alpha3.ClusterProvider); return ok },
			wantErr: "read ClusterProvider",
		},
		{
			name:    "Namespace read fails",
			failOn:  func(o client.Object) bool { _, ok := o.(*corev1.Namespace); return ok },
			wantErr: "read namespace",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cl := fake.NewClientBuilder().
				WithScheme(scScheme(t)).
				WithObjects(provider).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(
						ctx context.Context,
						c client.WithWatch,
						key client.ObjectKey,
						obj client.Object,
						opts ...client.GetOption,
					) error {
						if tc.failOn(obj) {
							return errors.New("api server unavailable")
						}
						return c.Get(ctx, key, obj, opts...)
					},
				}).
				Build()
			r := &GitTargetReconciler{Client: cl}

			ok, reason, _, err := r.checkSourceAuthorization(context.Background(), target)
			require.Error(t, err, "a transient read failure must requeue, not decide the policy")
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.False(t, ok)
			assert.Empty(t, reason, "an error carries no verdict reason")
		})
	}
}

// TestConditionStatusFromString maps the watch layer's stringly-typed state onto the API type.
// Anything unrecognised must land on Unknown rather than being read as a False (which would
// downgrade a GitTarget on a state the data plane never actually reported).
func TestConditionStatusFromString(t *testing.T) {
	tests := []struct {
		state string
		want  metav1.ConditionStatus
	}{
		{"True", metav1.ConditionTrue},
		{"False", metav1.ConditionFalse},
		{"Unknown", metav1.ConditionUnknown},
		{"", metav1.ConditionUnknown},
		{"true", metav1.ConditionUnknown},
		{"garbage", metav1.ConditionUnknown},
	}
	for _, tc := range tests {
		t.Run("state="+tc.state, func(t *testing.T) {
			assert.Equal(t, tc.want, conditionStatusFromString(tc.state))
		})
	}
}

// TestDescribeKubeConfigKey checks the "key not found" hint names the value→value.yaml fallback
// when the spec omitted a key, so the message tells the user what was actually tried.
func TestDescribeKubeConfigKey(t *testing.T) {
	assert.Equal(t, "value or value.yaml", describeKubeConfigKey(""))
	assert.Equal(t, "kubeconfig", describeKubeConfigKey("kubeconfig"))
}

// TestReconcile_UnauthorizedNamespaceStartsNoWatch pins the whole security property in one place,
// through the REAL Reconcile rather than checkSourceAuthorization in isolation: namespace
// authorization is enforced at reconcile only (there is no admission webhook for it), so this path
// is the entire boundary. A GitTarget whose namespace its ClusterProvider does not admit must end
// up Validated=False/NamespaceNotAuthorized AND must never reach DeclareForGitTarget — no watch is
// started, so nothing is ever routed to a branch worker and nothing is written to Git.
func TestReconcile_UnauthorizedNamespaceStartsNoWatch(t *testing.T) {
	const ns, providerName = "team-a", "prod-eu-1"

	// The provider admits team-b only; the GitTarget lives in team-a.
	provider := &configbutleraiv1alpha3.ClusterProvider{
		ObjectMeta: metav1.ObjectMeta{Name: providerName},
		Spec: configbutleraiv1alpha3.ClusterProviderSpec{
			AllowedNamespaces: &configbutleraiv1alpha3.NamespaceMatcher{Names: []string{"team-b"}},
		},
	}
	gitProvider := &configbutleraiv1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "gp", Namespace: ns},
		Spec:       configbutleraiv1alpha3.GitProviderSpec{AllowedBranches: []string{"main"}},
	}
	target := &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "gt", Namespace: ns, UID: "gt-uid"},
		Spec: configbutleraiv1alpha3.GitTargetSpec{
			ProviderRef:        configbutleraiv1alpha3.GitProviderReference{Name: "gp"},
			Branch:             "main",
			Path:               "apps",
			ClusterProviderRef: &configbutleraiv1alpha3.ClusterProviderReference{Name: providerName},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scScheme(t)).
		WithObjects(provider, gitProvider, target,
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}).
		WithStatusSubresource(&configbutleraiv1alpha3.GitTarget{}).
		Build()

	watchManager := &watch.Manager{Client: cl, Log: logr.Discard()}
	r := &GitTargetReconciler{
		Client:      cl,
		EventRouter: &watch.EventRouter{WatchManager: watchManager},
	}

	_, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: k8stypes.NamespacedName{Name: "gt", Namespace: ns}})
	require.NoError(t, err)

	var got configbutleraiv1alpha3.GitTarget
	require.NoError(t, cl.Get(context.Background(),
		k8stypes.NamespacedName{Name: "gt", Namespace: ns}, &got))

	validated := apimeta.FindStatusCondition(got.Status.Conditions, GitTargetConditionValidated)
	require.NotNil(t, validated, "Validated condition must be set")
	assert.Equal(t, metav1.ConditionFalse, validated.Status)
	assert.Equal(t, GitTargetReasonNamespaceNotAuthorized, validated.Reason)
	assert.Contains(t, validated.Message, "not permitted to reference")

	// The data plane is reported blocked, not merely un-ready...
	streams := apimeta.FindStatusCondition(got.Status.Conditions, GitTargetConditionStreamsRunning)
	require.NotNil(t, streams)
	assert.Equal(t, metav1.ConditionUnknown, streams.Status)

	// ...and, the point of the test, no declaration ever reached the watch manager. The Validated
	// gate returns before DeclareForGitTarget, so the capture-on-Declare map stays empty for it.
	gitDest := types.NewResourceReference("gt", ns).WithUID("gt-uid")
	_, declared := watchManager.DeclaredSourceCluster(gitDest)
	assert.False(t, declared, "a refused GitTarget must never declare watches against its source cluster")

	// Positive control, through the real Declare path: the same manager DOES record a declaration,
	// so the assertion above cannot pass vacuously. DeclareForGitTarget captures the source cluster
	// before it opens any watch, so it records even though opening watches fails here (no discovery
	// client is wired) — which is exactly the capture a refused GitTarget must not produce.
	other := types.NewResourceReference("authorized", ns).WithUID("other-uid")
	_ = watchManager.DeclareForGitTarget(
		context.Background(), other, providerName, providerName, configbutleraiv1alpha3.PruneOnEvent)
	id, declaredOther := watchManager.DeclaredSourceCluster(other)
	require.True(t, declaredOther, "the positive control must declare, or the assertion above proves nothing")
	assert.Equal(t, providerName, id)
}

func TestClusterProviderReadiness_AllScenarios(t *testing.T) {
	provider := func(conds []metav1.Condition) *configbutleraiv1alpha3.ClusterProvider {
		return &configbutleraiv1alpha3.ClusterProvider{
			ObjectMeta: metav1.ObjectMeta{Name: "prod-eu-1"},
			Status:     configbutleraiv1alpha3.ClusterProviderStatus{Conditions: conds},
		}
	}
	ready := metav1.Condition{Type: ConditionTypeReady, Status: metav1.ConditionTrue, Reason: "OK"}
	notReady := metav1.Condition{
		Type:    ConditionTypeReady,
		Status:  metav1.ConditionFalse,
		Reason:  "Bad",
		Message: "nope",
	}

	unknownReady := metav1.Condition{
		Type:    ConditionTypeReady,
		Status:  metav1.ConditionUnknown,
		Reason:  "Checking",
		Message: "validating",
	}

	tests := []struct {
		name string
		cp   *configbutleraiv1alpha3.ClusterProvider
		want metav1.ConditionStatus
	}{
		{"ready", provider([]metav1.Condition{ready}), metav1.ConditionTrue},
		{"not ready -> False (downgrades)", provider([]metav1.Condition{notReady}), metav1.ConditionFalse},
		{"no condition -> Unknown (does not downgrade)", provider(nil), metav1.ConditionUnknown},
		{"absent provider -> Unknown", nil, metav1.ConditionUnknown},
		// Only an EXPLICIT Ready=False downgrades. A provider reporting Ready=Unknown is mid-flight,
		// not broken, so it must not hold its GitTargets down.
		{
			"explicit Ready=Unknown -> Unknown (does not downgrade)",
			provider([]metav1.Condition{unknownReady}),
			metav1.ConditionUnknown,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			builder := fake.NewClientBuilder().WithScheme(scScheme(t))
			if tc.cp != nil {
				builder = builder.WithObjects(tc.cp)
			}
			r := &GitTargetReconciler{Client: builder.Build()}
			target := &configbutleraiv1alpha3.GitTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "gt", Namespace: "team-a"},
				Spec: configbutleraiv1alpha3.GitTargetSpec{
					ClusterProviderRef: &configbutleraiv1alpha3.ClusterProviderReference{Name: "prod-eu-1"},
				},
			}
			status, _, msg := r.clusterProviderReadiness(context.Background(), target)
			assert.Equal(t, tc.want, status)
			assert.NotEmpty(t, msg)
		})
	}
}

func TestGitProviderReadiness_AllScenarios(t *testing.T) {
	provider := func(conds []metav1.Condition) *configbutleraiv1alpha3.GitProvider {
		return &configbutleraiv1alpha3.GitProvider{
			ObjectMeta: metav1.ObjectMeta{Name: "prov", Namespace: "team-a"},
			Status:     configbutleraiv1alpha3.GitProviderStatus{Conditions: conds},
		}
	}
	ready := metav1.Condition{Type: ConditionTypeReady, Status: metav1.ConditionTrue, Reason: "OK"}
	notReady := metav1.Condition{
		Type:    ConditionTypeReady,
		Status:  metav1.ConditionFalse,
		Reason:  "BadRepo",
		Message: "no repo",
	}

	tests := []struct {
		name string
		gp   *configbutleraiv1alpha3.GitProvider
		want metav1.ConditionStatus
	}{
		{"ready", provider([]metav1.Condition{ready}), metav1.ConditionTrue},
		{"not ready -> False (downgrades)", provider([]metav1.Condition{notReady}), metav1.ConditionFalse},
		{"no condition -> Unknown (does not downgrade)", provider(nil), metav1.ConditionUnknown},
		{"absent provider -> Unknown", nil, metav1.ConditionUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			builder := fake.NewClientBuilder().WithScheme(scScheme(t))
			if tc.gp != nil {
				builder = builder.WithObjects(tc.gp)
			}
			r := &GitTargetReconciler{Client: builder.Build()}
			target := &configbutleraiv1alpha3.GitTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "gt", Namespace: "team-a"},
				Spec: configbutleraiv1alpha3.GitTargetSpec{
					ProviderRef: configbutleraiv1alpha3.GitProviderReference{Name: "prov"},
				},
			}
			status, _, msg := r.gitProviderReadiness(context.Background(), target, "team-a")
			assert.Equal(t, tc.want, status)
			assert.NotEmpty(t, msg)
		})
	}
}
