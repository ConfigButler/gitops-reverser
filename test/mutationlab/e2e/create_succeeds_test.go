//go:build mutationlab_e2e

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

package e2e

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestCreateSucceeds is the M0 proof of the loop: capture -> normalize -> write
// -> diff on a single ConfigMap create. It expects three moments for the
// scenario — a watch ADDED, an audit create, and an admission create — and
// commits them as corpus/configmap/create-succeeds/.
func TestCreateSucceeds(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.newScenario(ctx, t, "create-succeeds")

	cm := &corev1.ConfigMap{ObjectMeta: s.meta("cm-a"), Data: map[string]string{"key": "value"}}
	if _, err := h.kube.CoreV1().ConfigMaps(s.ns).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create configmap: %v", err)
	}

	records := h.drain(t, s.id, drainSpec{minCount: 3, settle: 2 * time.Second, timeout: 60 * time.Second})
	h.syncCorpus(t, "configmap/create-succeeds", records)
}
