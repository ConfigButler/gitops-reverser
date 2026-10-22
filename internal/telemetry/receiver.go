// SPDX-License-Identifier: Apache-2.0

package telemetry

import "go.opentelemetry.io/otel/metric"

// ReceiverDeliveriesTotal counts the deliveries the push receiver answered for a GitReceiver,
// labelled by {receiver_namespace, receiver_name, type, outcome}. type is the receiver type
// (github, gitlab, gitea, generic-hmac). outcome is a frozen enum:
//
//	notified            — verified, named a repository and a branch, and at least one branch
//	                      worker for them was told to look.
//	unreferenced        — verified, but no GitProvider the receiver admits references it. Still a
//	                      202; the receiver serves nobody.
//	unknown_repository  — verified, but no GitProvider on the receiver has the repository it names.
//	                      Still a 202, and routine on an organization or GitHub App hook, which
//	                      delivers for every repository.
//	unknown_branch      — verified and for a repository a GitProvider on the receiver has, but no
//	                      branch worker writes the branch for it. Still a 202.
//	ignored             — verified and deliberately not acted on: a ping, an event other than a
//	                      push, or a tag or other non-branch ref.
//	rejected            — refused before anything in it was read: the signature or token was absent
//	                      or wrong, or the body was over the size cap.
//	invalid             — verified but not parseable, or a required field was missing (a 400).
//
// It cannot say whether the push was somebody else's: the receiver only hands the branch to its
// workers, and each worker decides foreign versus own by reading the remote. A delivery is a hint,
// so "notified" means "looked", never "found a foreign commit".
//
// Requests that never reach a GitReceiver (an unknown route, a wrong method, a missing token
// Secret) are not counted: there is no receiver to label them with, and the route and method are
// unbounded inputs from whoever can reach the listener.
//
// Registered in registerCounters (exporter.go) with every other counter.
var ReceiverDeliveriesTotal metric.Int64Counter
