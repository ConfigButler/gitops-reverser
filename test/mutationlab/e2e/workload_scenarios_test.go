//go:build mutationlab_e2e

// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/ConfigButler/gitops-reverser/internal/mutationlab"
)

// Workload scenarios — the moments ConfigMap structurally cannot reach: a
// Deployment for the /status (Row 5) and /scale (Row 6) subresources, and a Pod
// for the two-event graceful delete (Row 7).
//
// The headline finding here is mechanism *silence*. The lab reuses the product's
// audit policy and validating-webhook config verbatim (swap-image model), and
// those deliberately drop exactly these moments from the provenance mechanisms:
//
//   - the audit policy drops apps/*/status and core */status, so a /status write
//     is invisible to audit (Row 5);
//   - the audit policy drops core pods entirely, so a Pod delete is invisible to
//     audit (Row 7);
//   - the validating webhook matches top-level resources only, so neither /status
//     nor /scale ever reaches admission (Rows 5, 6).
//
// So for these rows the *watch* is the only mechanism that sees the state change
// — which is the central "watch is viable for state" hypothesis made concrete.

// pausedDeployment returns a Deployment that the deployment controller will not
// roll out (spec.paused) and that has no replicas, so the only watch events after
// setup are the subresource write under test plus the controller's single
// status follow-up — never rollout/pod churn. This is what makes Rows 5 and 6
// deterministic despite Deployments having an active controller.
func pausedDeployment(s scenario, name string) *appsv1.Deployment {
	replicas := int32(0)
	paused := true
	labels := map[string]string{"app": name}
	return &appsv1.Deployment{
		ObjectMeta: s.meta(name),
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Paused:   paused,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:    "c",
						Image:   "busybox:1.36",
						Command: []string{"sleep", "3600"},
					}},
				},
			},
		},
	}
}

// TestStatusSubresource captures Row 5: a /status subresource update on a
// Deployment. Audit drops apps/*/status and admission never matches a
// subresource, so the only records are watch MODIFIED events — and there are two,
// because the deployment controller owns status and immediately clobbers the
// user's write. That clobber is itself the finding: a user /status write does not
// persist.
func TestStatusSubresource(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.newScenario(ctx, t, "status-update")

	if _, err := h.kube.AppsV1().Deployments(s.ns).Create(ctx, pausedDeployment(s, "d1"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	// create => admission CREATE + audit create + watch ADDED + the controller's
	// status-settle MODIFIEDs; quiesce past all of it, then clear.
	h.quiesceAndClear(t, s.id, 3)

	patch := []byte(`{"status":{"observedGeneration":99}}`)
	if _, err := h.kube.AppsV1().Deployments(s.ns).Patch(
		ctx, "d1", types.MergePatchType, patch, metav1.PatchOptions{}, "status"); err != nil {
		t.Fatalf("status patch: %v", err)
	}

	// The user write (observedGeneration=99) then the controller clobber back to
	// the real generation — two watch MODIFIED, no audit, no admission.
	records := h.drain(t, s.id, drainSpec{minCount: 2, settle: 3 * time.Second, timeout: 60 * time.Second})
	if got := countSource(records, mutationlab.SourceAudit); got != 0 {
		t.Errorf("status update produced %d audit records; want 0 (audit drops */status)", got)
	}
	if got := countSource(records, mutationlab.SourceAdmission); got != 0 {
		t.Errorf("status update produced %d admission records; want 0 (webhook ignores subresources)", got)
	}
	if got := countSource(records, mutationlab.SourceWatch); got < 2 {
		t.Errorf("status update produced %d watch records; want >= 2 (user write + controller clobber)", got)
	}
	h.syncCorpus(t, "deployment/status-update", records)
}

// TestScaleSubresource captures Row 6: a /scale subresource patch on a
// Deployment. Unlike /status, audit keeps scale writes for non-HPA users, so the
// records are one audit `patch` with objectRef.subresource=scale plus the watch
// MODIFIED events; admission still never sees the subresource.
func TestScaleSubresource(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.newScenario(ctx, t, "scale-patch")

	if _, err := h.kube.AppsV1().Deployments(s.ns).Create(ctx, pausedDeployment(s, "d1"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	h.quiesceAndClear(t, s.id, 3)

	patch := []byte(`{"spec":{"replicas":2}}`)
	if _, err := h.kube.AppsV1().Deployments(s.ns).Patch(
		ctx, "d1", types.MergePatchType, patch, metav1.PatchOptions{}, "scale"); err != nil {
		t.Fatalf("scale patch: %v", err)
	}

	// audit patch (scale) + the user scale MODIFIED + the controller's
	// observedGeneration follow-up MODIFIED. The Scale audit body carries no
	// labels, so that event attributes to the namespace, not the scenario id —
	// union both keys (post-clear, the namespace holds only the scale audit).
	records := h.drain(t, s.id, drainSpec{
		minCount: 3, settle: 3 * time.Second, timeout: 60 * time.Second, alsoNamespace: s.ns})
	if got := countSource(records, mutationlab.SourceAudit); got == 0 {
		t.Error("scale patch produced no audit record; want one (scale is audited for non-HPA users)")
	}
	if got := countSource(records, mutationlab.SourceAdmission); got != 0 {
		t.Errorf("scale patch produced %d admission records; want 0 (webhook ignores subresources)", got)
	}
	h.syncCorpus(t, "deployment/scale-patch", records)
}

// gracefulPod returns a Pod that runs until killed, with a short grace period so
// the terminating window is brief, and with the service-account token unmounted
// so no random kube-api-access-XXXXX volume name churns the corpus.
func gracefulPod(s scenario, name string) *corev1.Pod {
	automount := false
	grace := int64(5)
	return &corev1.Pod{
		ObjectMeta: s.meta(name),
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken:  &automount,
			TerminationGracePeriodSeconds: &grace,
			Containers: []corev1.Container{{
				Name:    "c",
				Image:   "busybox:1.36",
				Command: []string{"sleep", "3600"},
			}},
		},
	}
}

// TestGracefulDelete captures Row 7: deleting a running Pod is a two-step removal
// — the apiserver first stamps deletionTimestamp (watch MODIFIED), the object
// lingers while the kubelet terminates the container, and only then does it
// disappear (watch DELETED). A non-graceful delete would skip straight to
// DELETED, so the lingering MODIFIED is the behavior under test.
//
// Pods are dropped from the audit policy entirely, so there is no audit record;
// pods are top-level, so the DELETE does reach the validating webhook. The corpus
// keeps the two semantically load-bearing watch moments (the deletion-pending
// MODIFIED and the terminal DELETED) plus the admission DELETE; the intermediate
// kubelet status writes during termination are timing-dependent, so they are
// asserted as a law over the full drain rather than committed as flaky moments.
func TestGracefulDelete(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.newScenario(ctx, t, "graceful-delete")

	if _, err := h.kube.CoreV1().Pods(s.ns).Create(ctx, gracefulPod(s, "p1"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	h.waitForPodRunning(ctx, t, s.ns, "p1")
	h.quiesceAndClear(t, s.id, 2)

	if err := h.kube.CoreV1().Pods(s.ns).Delete(ctx, "p1", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete pod: %v", err)
	}

	// Drain until the terminal DELETED has arrived and the stream is quiet — the
	// DELETED fires only after the grace period, so a count+settle gate alone
	// would return mid-termination.
	records := h.drain(t, s.id, drainSpec{
		minCount: 3, settle: 3 * time.Second, timeout: 90 * time.Second,
		until: func(rs []mutationlab.Record) bool { return firstWatch(rs, "DELETED") != nil },
	})
	if got := countSource(records, mutationlab.SourceAudit); got != 0 {
		t.Errorf("pod delete produced %d audit records; want 0 (audit drops pods)", got)
	}

	pending := firstDeletionPendingWatch(records)
	deleted := firstWatch(records, "DELETED")
	admission := firstSource(records, mutationlab.SourceAdmission)
	if pending == nil {
		t.Fatal("no watch MODIFIED with a deletionTimestamp; expected a deletion-pending event before DELETED")
	}
	if deleted == nil {
		t.Fatal("no watch DELETED; the pod never disappeared from the watch")
	}
	if admission == nil {
		t.Fatal("no admission DELETE record; the validating webhook should match a top-level pod delete")
	}
	if pending.Key.ResourceVersion >= deleted.Key.ResourceVersion {
		t.Errorf("deletion-pending rv %q should precede terminal DELETED rv %q",
			pending.Key.ResourceVersion, deleted.Key.ResourceVersion)
	}

	h.syncCorpus(t, "pod/graceful-delete", []mutationlab.Record{*admission, *pending, *deleted})
}

// waitForPodRunning blocks until the pod reaches the Running phase, so the
// subsequent delete exercises the graceful (two-step) path rather than the
// immediate removal of an unscheduled pod.
func (h *harness) waitForPodRunning(ctx context.Context, t *testing.T, ns, name string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		p, err := h.kube.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil && p.Status.Phase == corev1.PodRunning {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("pod %s/%s never reached Running", ns, name)
}

// firstDeletionPendingWatch returns the first watch MODIFIED whose object carries
// a deletionTimestamp — the moment the apiserver tombstones the object before the
// kubelet finishes terminating it.
func firstDeletionPendingWatch(records []mutationlab.Record) *mutationlab.Record {
	for i := range records {
		r := &records[i]
		if r.Source == mutationlab.SourceWatch && r.Summary.WatchType == "MODIFIED" && watchHasDeletionTimestamp(r) {
			return r
		}
	}
	return nil
}

func firstWatch(records []mutationlab.Record, watchType string) *mutationlab.Record {
	for i := range records {
		if r := &records[i]; r.Source == mutationlab.SourceWatch && r.Summary.WatchType == watchType {
			return r
		}
	}
	return nil
}

func firstSource(records []mutationlab.Record, src mutationlab.Source) *mutationlab.Record {
	for i := range records {
		if r := &records[i]; r.Source == src {
			return r
		}
	}
	return nil
}

// watchHasDeletionTimestamp reports whether a watch record's object has a
// metadata.deletionTimestamp set.
func watchHasDeletionTimestamp(r *mutationlab.Record) bool {
	var env struct {
		Object struct {
			Metadata struct {
				DeletionTimestamp string `json:"deletionTimestamp"`
			} `json:"metadata"`
		} `json:"object"`
	}
	if err := json.Unmarshal(r.Raw, &env); err != nil {
		return false
	}
	return env.Object.Metadata.DeletionTimestamp != ""
}
