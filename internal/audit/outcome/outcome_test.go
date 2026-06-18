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

package outcome

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOutcomeCategory(t *testing.T) {
	cases := map[Outcome]Category{
		Queued:                   Stored,
		Parked:                   Held,
		NotNeeded:                Dropped,
		NilEvent:                 Dropped,
		Stage:                    Dropped,
		ReadOnlyOrUnknownVerb:    Dropped,
		FailedRequest:            Dropped,
		DryRun:                   Dropped,
		UnchangedResourceVersion: Dropped,
		MalformedAdditional:      Dropped,
		NonScaleSubresource:      Dropped,
		ShallowDropped:           Dropped,
		RVLessEmptyHighWater:     Dropped,
		OlderThanHighWater:       Dropped,
		NonNumericRV:             Dropped,
		WriteError:               Error,
	}
	for o, want := range cases {
		assert.Equalf(t, want, o.Category(), "category of %s", o)
	}

	// An unknown/unmapped outcome is treated as Error so a new outcome added without a
	// category mapping trips the category="error" invariant rather than passing silently.
	assert.Equal(t, Error, Outcome("mystery").Category())
}

func TestRecordNilGuard(t *testing.T) {
	// With telemetry uninitialized (AuditEventsTotal == nil) Record is a no-op and must not panic.
	assert.NotPanics(t, func() {
		Record(context.Background(), nil, Queued)
	})
}
