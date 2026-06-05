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

package mapping

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// StructureOnlyMapper is the no-cluster mapper used by the analyzer default. Its
// lookup returns MappingStructureOnly: it is not a failure, it is the honest
// answer for "this YAML looks like KRM, but no API surface was asked what serves
// it". It never produces watched/unwatched or destructive conclusions.
//
// Its readiness is deliberately not-ready: a structure-only mapper can never gate
// a destructive decision, so a GitTarget that needs watched/unwatched answers must
// hold until a stronger source is wired in.
type StructureOnlyMapper struct{}

// NewStructureOnlyMapper returns the structure-only mapper.
func NewStructureOnlyMapper() StructureOnlyMapper { return StructureOnlyMapper{} }

// Source reports structure-only.
func (StructureOnlyMapper) Source() MapperSource { return MapperSourceStructureOnly }

// Ready reports a deliberately not-ready, non-degraded mapper: it answers every
// call, but never with trusted API data.
func (StructureOnlyMapper) Ready() MapperReadiness {
	return MapperReadiness{Ready: false, Reason: "structure-only: no API source"}
}

// Generation is always 0: there is no refreshable catalog behind it.
func (StructureOnlyMapper) Generation() uint64 { return 0 }

// GVRForGVK always returns MappingStructureOnly, echoing the requested GVK. A
// cancelled context is still honored as an error, so every ResourceMapper reacts
// to cancellation the same way regardless of source.
func (StructureOnlyMapper) GVRForGVK(ctx context.Context, gvk schema.GroupVersionKind) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	return structureOnlyResult(Result{GVK: gvk}), nil
}

func structureOnlyResult(result Result) Result {
	result.Status = MappingStructureOnly
	result.Reason = "no API source consulted"
	return result
}
