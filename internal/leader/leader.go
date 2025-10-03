/*
Package leader provides leader election functionality for the GitOps Reverser controller.
It manages pod labeling to identify the active leader instance in a multi-replica deployment.
*/
package leader

import (
	"context"
	"os"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	leaderLabelKey   = "role"
	leaderLabelValue = "leader"
)

// PodLabeler is a Runnable that adds a label to the pod when it becomes the leader
// and removes it when it stops being the leader.
// It implements the LeaderElectionRunnable interface so it only runs on the leader.
type PodLabeler struct {
	Client    client.Client
	Log       logr.Logger
	PodName   string
	Namespace string
}

// NeedLeaderElection implements the LeaderElectionRunnable interface.
// This ensures the PodLabeler only runs on the elected leader.
func (p *PodLabeler) NeedLeaderElection() bool {
	return true
}

// Start adds the leader label to the pod and blocks until the context is canceled.
// This method is only called on the elected leader pod when NeedLeaderElection returns true.
func (p *PodLabeler) Start(ctx context.Context) error {
	log := p.Log.WithValues("pod", p.PodName, "namespace", p.Namespace)
	log.Info("🎯 PodLabeler.Start() called - This pod is the leader, adding leader label.")

	if err := p.addLabel(ctx, log); err != nil {
		log.Error(err, "❌ Failed to add leader label")
		return err
	}

	log.Info("✅ Leader label added successfully")

	// The context is canceled when the manager stops.
	<-ctx.Done()

	log.Info("Leader is shutting down, removing leader label.")
	// Use a new context for the cleanup operation.
	if err := p.removeLabel(context.Background(), log); err != nil {
		log.Error(err, "failed to remove leader label on shutdown")
		// Don't return error on shutdown, just log it.
	}
	return nil
}

func (p *PodLabeler) addLabel(ctx context.Context, log logr.Logger) error {
	pod, err := p.getPod(ctx)
	if err != nil {
		return err
	}

	if pod.Labels == nil {
		pod.Labels = make(map[string]string)
	}

	if val, ok := pod.Labels[leaderLabelKey]; ok && val == leaderLabelValue {
		log.Info("Pod already has leader label")
		return nil
	}

	pod.Labels[leaderLabelKey] = leaderLabelValue
	return p.Client.Update(ctx, pod)
}

func (p *PodLabeler) removeLabel(ctx context.Context, log logr.Logger) error {
	pod, err := p.getPod(ctx)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("Pod not found, cannot remove leader label.")
			return nil
		}
		return err
	}

	if _, ok := pod.Labels[leaderLabelKey]; !ok {
		log.Info("Pod does not have leader label, nothing to remove.")
		return nil
	}

	delete(pod.Labels, leaderLabelKey)
	return p.Client.Update(ctx, pod)
}

func (p *PodLabeler) getPod(ctx context.Context) (*corev1.Pod, error) {
	pod := &corev1.Pod{}
	key := types.NamespacedName{Name: p.PodName, Namespace: p.Namespace}
	err := p.Client.Get(ctx, key, pod)
	return pod, err
}

// GetPodName returns the pod name from the POD_NAME environment variable.
func GetPodName() string {
	return os.Getenv("POD_NAME")
}

// GetPodNamespace returns the pod namespace from the POD_NAMESPACE environment variable.
func GetPodNamespace() string {
	return os.Getenv("POD_NAMESPACE")
}
