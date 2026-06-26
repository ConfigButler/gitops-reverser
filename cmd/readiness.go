// SPDX-License-Identifier: Apache-2.0
//
// Copyright 2025 ConfigButler
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

// Liveness and readiness wiring for the controller. Liveness (/healthz) stays a bare process
// ping; readiness (/readyz) reflects the audit-serving preconditions so the kube-apiserver — which
// dials this pod through a Service — only routes audit events here once the pod can actually
// receive and enqueue them. See docs/design/audit-readiness-probe-plan.md.

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

const (
	// redisReadinessPingTimeout bounds one PING attempt of the audit-pipeline readiness gate, and
	// redisReadinessRetryInterval is the wait between attempts until the first PING succeeds. The
	// gate is startup-only — it latches ready on the first success and never flips back — so these
	// only govern how quickly a freshly-started pod becomes ready once Redis is reachable.
	redisReadinessPingTimeout   = 2 * time.Second
	redisReadinessRetryInterval = 1 * time.Second
)

// auditReadinessProbe reports whether the audit ingress listener is serving; *auditServerRunnable
// satisfies it. Kept as an interface so the readiness check is unit-testable without a live server.
type auditReadinessProbe interface {
	Serving() bool
}

// redisPinger is the subset of the audit producer the readiness gate needs.
type redisPinger interface {
	Ping(ctx context.Context) error
}

// redisReadinessGate latches ready on the first successful PING of the audit Redis/Valkey and
// never flips back. It is the startup-only Redis half of the readiness probe: a pod that has
// never reached Redis cannot enqueue audit events, so it must not join the audit Service's
// endpoints — but once it has connected, transient Redis blips are handled at the request layer
// (the audit handler returns HTTP 500 and the apiserver retries the batch), NOT by flapping
// readiness, which would yank every replica out of endpoints at once. See
// docs/design/audit-readiness-probe-plan.md §4.
type redisReadinessGate struct {
	pinger   redisPinger
	timeout  time.Duration
	interval time.Duration

	ready   atomic.Bool
	lastErr atomic.Pointer[string]
}

func newRedisReadinessGate(pinger redisPinger) *redisReadinessGate {
	return &redisReadinessGate{
		pinger:   pinger,
		timeout:  redisReadinessPingTimeout,
		interval: redisReadinessRetryInterval,
	}
}

// Start runs as a manager Runnable: it PINGs until the first success (then returns, so it never
// pings again), or until the manager context is cancelled.
func (g *redisReadinessGate) Start(ctx context.Context) error {
	for {
		pingCtx, cancel := context.WithTimeout(ctx, g.timeout)
		err := g.pinger.Ping(pingCtx)
		cancel()
		if err == nil {
			g.ready.Store(true)
			setupLog.Info("audit Redis reachable; readiness gate satisfied")
			return nil
		}
		msg := err.Error()
		g.lastErr.Store(&msg)
		setupLog.Error(err, "audit Redis not yet reachable; pod stays not-ready until first connection")

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(g.interval):
		}
	}
}

// Err returns nil once the gate has latched ready, otherwise a reason describing why the audit
// Redis connection is not yet established.
func (g *redisReadinessGate) Err() error {
	if g.ready.Load() {
		return nil
	}
	if msg := g.lastErr.Load(); msg != nil {
		return fmt.Errorf("audit redis not yet reachable: %s", *msg)
	}
	return errors.New("audit redis connection not yet established")
}

// auditServingReadyCheck fails until the audit ingress listener is bound and accepting.
func auditServingReadyCheck(probe auditReadinessProbe) healthz.Checker {
	if probe == nil {
		return nil
	}
	return func(_ *http.Request) error {
		if !probe.Serving() {
			return errors.New("audit ingress not yet serving")
		}
		return nil
	}
}

// auditCertReadyCheck fails until the audit ingress TLS cert is loaded and parseable. It returns
// nil (no check) when there is no cert watcher, i.e. --audit-insecure.
func auditCertReadyCheck(watcher *certwatcher.CertWatcher) healthz.Checker {
	if watcher == nil {
		return nil
	}
	return func(_ *http.Request) error {
		cert, err := watcher.GetCertificate(nil)
		if err != nil {
			return fmt.Errorf("audit ingress cert not loaded: %w", err)
		}
		if cert == nil {
			return errors.New("audit ingress cert not yet loaded")
		}
		return nil
	}
}

// combineReadyChecks runs the sub-checks in order and returns the first failure, skipping nil
// (not-applicable) checks. It makes the composite readiness probe trivially unit-testable.
func combineReadyChecks(checks ...healthz.Checker) healthz.Checker {
	return func(req *http.Request) error {
		for _, check := range checks {
			if check == nil {
				continue
			}
			if err := check(req); err != nil {
				return err
			}
		}
		return nil
	}
}

// addHealthChecks registers liveness and readiness checks. Liveness stays a bare Ping — a cert or
// Redis problem is not fixed by restarting the pod, so it must not crashloop. Readiness instead
// reflects the locally-checkable audit-serving preconditions (listener up, TLS cert loaded, first
// Redis connection made) so the kube-apiserver, which dials this pod through a Service, only routes
// audit events here once the pod can actually receive and enqueue them. See
// docs/design/audit-readiness-probe-plan.md.
func addHealthChecks(
	mgr ctrl.Manager,
	auditProbe auditReadinessProbe,
	auditCertWatcher *certwatcher.CertWatcher,
	redisGate *redisReadinessGate,
) {
	fatalIfErr(mgr.AddHealthzCheck("healthz", healthz.Ping), "unable to set up health check")

	var redisCheck healthz.Checker
	if redisGate != nil {
		redisCheck = func(_ *http.Request) error { return redisGate.Err() }
	}
	readyz := combineReadyChecks(
		auditServingReadyCheck(auditProbe),
		auditCertReadyCheck(auditCertWatcher),
		redisCheck,
	)
	fatalIfErr(mgr.AddReadyzCheck("readyz", readyz), "unable to set up ready check")
}
