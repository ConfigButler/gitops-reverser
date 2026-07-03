// SPDX-License-Identifier: Apache-2.0

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
