// SPDX-License-Identifier: Apache-2.0

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
