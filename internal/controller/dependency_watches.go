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

package controller

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// logDependencyListError records a List failure from inside a cross-kind watch
// map function. Those map functions intentionally degrade to "enqueue nothing"
// on error — the affected resources still recover on their periodic requeue —
// so without this log the failure would be entirely invisible.
func logDependencyListError(ctx context.Context, err error, listKind string, trigger client.Object) {
	logf.FromContext(ctx).Error(err,
		"dependency watch: failed to list "+listKind+" for a dependency event; "+
			"affected resources will recover on the periodic requeue",
		"trigger", client.ObjectKeyFromObject(trigger))
}
