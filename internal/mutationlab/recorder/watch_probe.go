/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package recorder

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"

	"github.com/ConfigButler/gitops-reverser/internal/mutationlab"
)

// WatchProbeMode selects the transport behavior a watch probe should capture.
type WatchProbeMode string

const (
	// WatchProbeBookmark captures the bookmark shape from a streaming list watch.
	WatchProbeBookmark WatchProbeMode = "bookmark"
	// WatchProbeExpired captures the Status payload for an expired resourceVersion.
	WatchProbeExpired WatchProbeMode = "expired"
	// WatchProbeReplay captures the full SendInitialEvents replay window — every
	// initial synthetic ADDED plus the terminating initial-events-end BOOKMARK —
	// using the exact transport internal/watch/target_watch.go opens. It exists to
	// document the replay watermark: a create-then-modify performed before the
	// watch opens is delivered as the single collapsed ADDED at the post-modify
	// resourceVersion, not as a distinct CREATE then MODIFIED, and it lands inside
	// the replay window (before the bookmark) where the product files it as an
	// unattributed baseline rather than an attributable per-event commit.
	WatchProbeReplay WatchProbeMode = "replay"
)

// WatchProbeRequest describes one targeted watch transport capture.
type WatchProbeRequest struct {
	Scenario      string
	Mode          WatchProbeMode
	GVR           schema.GroupVersionResource
	Namespace     string
	LabelSelector string
}

// WatchProbe opens short-lived, scenario-scoped watches for transport rows the
// background recorder cannot reliably attribute or trigger.
type WatchProbe struct {
	client dynamic.Interface
}

// NewWatchProbe returns a targeted watch transport prober.
func NewWatchProbe(client dynamic.Interface) *WatchProbe {
	return &WatchProbe{client: client}
}

// Probe captures the requested watch transport event(s). Returned records are
// tagged with req.Scenario because transport-only watch events, especially
// BOOKMARK and ERROR, do not carry scenario labels themselves.
func (p *WatchProbe) Probe(ctx context.Context, req WatchProbeRequest) ([]mutationlab.Record, error) {
	if p == nil || p.client == nil {
		return nil, errors.New("dynamic client is required")
	}
	if req.Scenario == "" {
		return nil, errors.New("scenario is required")
	}
	if req.GVR.Version == "" || req.GVR.Resource == "" {
		return nil, errors.New("resource is required")
	}
	switch req.Mode {
	case WatchProbeBookmark:
		return p.probeBookmark(ctx, req)
	case WatchProbeExpired:
		return p.probeExpired(ctx, req)
	case WatchProbeReplay:
		return p.probeReplay(ctx, req)
	default:
		return nil, fmt.Errorf("unsupported watch probe mode %q", req.Mode)
	}
}

func (p *WatchProbe) probeBookmark(ctx context.Context, req WatchProbeRequest) ([]mutationlab.Record, error) {
	sendInitial := true
	opts := metav1.ListOptions{
		AllowWatchBookmarks:  true,
		LabelSelector:        req.LabelSelector,
		ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
		SendInitialEvents:    &sendInitial,
	}
	return p.captureUntil(ctx, req, opts, func(r mutationlab.Record) bool {
		return r.Summary.WatchType == string(watch.Bookmark)
	}, true)
}

func (p *WatchProbe) probeReplay(ctx context.Context, req WatchProbeRequest) ([]mutationlab.Record, error) {
	sendInitial := true
	opts := metav1.ListOptions{
		AllowWatchBookmarks:  true,
		LabelSelector:        req.LabelSelector,
		ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
		SendInitialEvents:    &sendInitial,
	}
	// onlyTerminal=false keeps every replayed ADDED, not just the boundary — the
	// collapsed observation is the point. The first BOOKMARK is the
	// initial-events-end marker that closes the replay window.
	return p.captureUntil(ctx, req, opts, func(r mutationlab.Record) bool {
		return r.Summary.WatchType == string(watch.Bookmark)
	}, false)
}

func (p *WatchProbe) probeExpired(ctx context.Context, req WatchProbeRequest) ([]mutationlab.Record, error) {
	opts := metav1.ListOptions{
		AllowWatchBookmarks: true,
		LabelSelector:       req.LabelSelector,
		ResourceVersion:     "1",
	}
	return p.captureUntil(ctx, req, opts, func(r mutationlab.Record) bool {
		return r.Summary.WatchType == string(watch.Error)
	}, false)
}

func (p *WatchProbe) captureUntil(
	ctx context.Context,
	req WatchProbeRequest,
	opts metav1.ListOptions,
	done func(mutationlab.Record) bool,
	onlyTerminal bool,
) ([]mutationlab.Record, error) {
	watcher, err := p.resource(req).Watch(ctx, opts)
	if err != nil {
		if status, ok := statusFromWatchError(err); ok {
			rec := taggedWatchRecord(req, watch.Error, status)
			if done(rec) {
				return []mutationlab.Record{rec}, nil
			}
		}
		return nil, fmt.Errorf("open watch: %w", err)
	}
	defer watcher.Stop()

	var records []mutationlab.Record
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("watch probe timed out: %w", ctx.Err())
		case ev, ok := <-watcher.ResultChan():
			if !ok {
				return nil, errors.New("watch closed before the probe event arrived")
			}
			rec := taggedWatchRecord(req, ev.Type, ev.Object)
			if !onlyTerminal || done(rec) {
				records = append(records, rec)
			}
			if done(rec) {
				return records, nil
			}
		}
	}
}

func (p *WatchProbe) resource(req WatchProbeRequest) dynamic.ResourceInterface {
	r := p.client.Resource(req.GVR)
	if req.Namespace != "" {
		return r.Namespace(req.Namespace)
	}
	return r
}

func taggedWatchRecord(req WatchProbeRequest, eventType watch.EventType, obj runtime.Object) mutationlab.Record {
	rec := buildWatchRecord(eventType, obj)
	rec.Scenario = req.Scenario
	rec.Key.Group = req.GVR.Group
	rec.Key.Version = req.GVR.Version
	rec.Key.Resource = req.GVR.Resource
	return rec
}

func statusFromWatchError(err error) (*metav1.Status, bool) {
	var statusErr *apierrors.StatusError
	if !errors.As(err, &statusErr) {
		return nil, false
	}
	status := statusErr.ErrStatus
	return &status, true
}
