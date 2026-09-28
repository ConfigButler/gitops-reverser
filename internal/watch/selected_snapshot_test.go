// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"errors"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// Under prune.mode Always a selected snapshot removes every document it does not return, so only
// a COMPLETE observation may reach the sweep. These drive one selected stream through each way an
// observation can end and report whether it tried to enqueue its snapshot: the router's client holds
// no GitTarget, so an attempted enqueue surfaces as errGitTargetGone and nothing else does.
func TestSelectedSnapshot_OnlyACompleteObservationReachesTheSweep(t *testing.T) {
	forbidden := errors.New("configmaps is forbidden: User cannot list resource")
	initialEventsEnd := func() watch.Event {
		bookmark := &unstructured.Unstructured{}
		bookmark.SetResourceVersion("11")
		bookmark.SetAnnotations(map[string]string{metav1.InitialEventsAnnotationKey: "true"})
		return watch.Event{Type: watch.Bookmark, Object: bookmark}
	}
	cases := []struct {
		name      string
		events    []watch.Event
		openErr   error
		listErr   error
		wantSweep bool
	}{
		{name: "complete empty snapshot", events: []watch.Event{initialEventsEnd()}, wantSweep: true},
		{name: "complete snapshot", events: []watch.Event{
			{Type: watch.Added, Object: configMapObject("10")}, initialEventsEnd(),
		}, wantSweep: true},
		{name: "initialization interrupted before initial-events-end", events: []watch.Event{
			{Type: watch.Added, Object: configMapObject("10")},
		}},
		{name: "denied open", openErr: forbidden},
		{name: "failed fallback LIST", openErr: errors.New("sendInitialEvents: Forbidden"), listErr: forbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, configv1alpha3.AddToScheme(scheme))
			manager := &Manager{
				Log: logr.Discard(),
				EventRouter: &EventRouter{
					Log:    logr.Discard(),
					Client: fake.NewClientBuilder().WithScheme(scheme).Build(),
				},
				targetWatchOpen: func(
					_ context.Context, _ schema.GroupVersionResource, _ string, opts metav1.ListOptions,
				) (watch.Interface, error) {
					if opts.SendInitialEvents != nil && tc.openErr != nil {
						return nil, tc.openErr
					}
					fw := watch.NewFakeWithChanSize(len(tc.events), false)
					for _, ev := range tc.events {
						fw.Action(ev.Type, ev.Object)
					}
					fw.Stop()
					return fw, nil
				},
				targetWatchList: func(
					context.Context, schema.GroupVersionResource, string, metav1.ListOptions,
				) (*unstructured.UnstructuredList, error) {
					return nil, tc.listErr
				},
			}
			key := targetWatchKey{GVR: configmapsGVR, Namespace: "apps", LabelSelector: teamA}

			err := manager.targetWatchReplayAndStream(context.Background(), logr.Discard(),
				types.NewResourceReference("target", "default"), testStream(key), false)

			require.Error(t, err)
			assert.Equal(t, tc.wantSweep, errors.Is(err, errGitTargetGone), "err: %v", err)
		})
	}
}
