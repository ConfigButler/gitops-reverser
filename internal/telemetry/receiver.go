// SPDX-License-Identifier: Apache-2.0

package telemetry

import "go.opentelemetry.io/otel/metric"

// ReceiverDeliveriesTotal counts the deliveries the push receiver answered for a GitProvider that
// has spec.receiver, labelled by {type, outcome}. type is the receiver type (github, gitlab,
// gitea, generic-hmac). outcome is a frozen enum:
//
//	notified   — verified, named a branch, and a branch worker for it was told to look.
//	unmatched  — verified and named a branch, but no branch worker writes it. Still a 202.
//	ignored    — verified and deliberately not acted on: a ping, an event other than a push, a
//	             tag or other non-branch ref, or a generic-hmac body naming another repository.
//	rejected   — refused before anything in it was read: the signature or token was absent or
//	             wrong, or the body was over the size cap.
//	invalid    — verified but not parseable, or a required field was missing (a 400).
//
// It cannot say whether the push was somebody else's: the receiver only hands the branch to its
// worker, and the worker decides foreign versus own by reading the remote. A delivery is a hint,
// so "notified" means "looked", never "found a foreign commit".
//
// Requests that never reach a provider with a receiver (an unknown route, a wrong method, a
// missing token Secret) are not counted: there is no type to label them with, and the route and
// method are unbounded inputs from whoever can reach the listener.
//
// Registered in registerCounters (exporter.go) with every other counter.
var ReceiverDeliveriesTotal metric.Int64Counter
