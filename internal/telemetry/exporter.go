// SPDX-License-Identifier: Apache-2.0

/*
Package telemetry provides the OpenTelemetry-based metrics exporter for GitOps Reverser.
It configures Prometheus-compatible metrics collection for monitoring controller operations.

Every instrument declared here MUST have at least one production recording site. A metric
that is defined but never recorded is a contract the code does not honor; document it in
docs/interpreting-metrics.md only once it actually emits.
*/
package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	otelMeter metric.Meter

	// GitDocumentsTotal is the write-boundary census: one increment per DOCUMENT the writer
	// decided about, labelled by {gittarget_namespace, gittarget_name, group, version, resource,
	// outcome}. outcome is a frozen enum:
	//
	//   written        — the document was created or updated in the working tree.
	//   deleted_live   — removed because a watch DELETE said the object is gone.
	//   deleted_sweep  — removed by a mark-and-sweep resync, which is the path that reconciles a
	//                    deletion nobody was watching for.
	//   unchanged      — the writer diffed it to a no-op, so no file changed.
	//   retained       — a mark-and-sweep would have deleted it and spec.prune.mode kept it. The
	//                    only numeric trace a suppressed drop leaves: it produces no plan action,
	//                    no commit and no ResyncStats entry. Non-zero is configured behaviour,
	//                    never a fault.
	//   refused        — the writer declined to place it, so it is NOT in the mirror.
	//                    placement_refusals_total carries the reason.
	//
	// Tallied on the write batch and published after a successful flush, under the batch's
	// GitTarget: a resync can apply every document and then abort on a precondition, writing
	// nothing at all.
	GitDocumentsTotal metric.Int64Counter
	// CommitRequestsTotal counts CommitRequests reaching a terminal outcome, labelled by
	// {outcome} alone: committed / no_window / window_mismatch / already_present / failed.
	//
	// It is the fleet view resource_condition deliberately refuses to give this kind. A
	// CommitRequest is created once per save, so a gauge per object would churn a series
	// continuously; a counter over terminal outcomes is bounded by the enum instead. The pointer
	// that used to stand in for it, git_commits_total{message_source="commit_request"}, counts
	// only the requests that succeeded AND were used, so the whole failure population had no
	// series at all — and this is the one object a human waits on synchronously. A request that
	// does not land still produces a green, pushed commit carrying a GENERATED message instead of
	// the sentence its author typed, which is user-visible and was un-alertable.
	//
	// window_mismatch is the value that matters most for that reason: it is the silent
	// substitution, not an error anybody sees.
	//
	// COUNTING SEMANTICS, because this counter cannot promise what it looks like it promises. It
	// counts one increment per TERMINAL DECISION ATTEMPT, recorded outside the status-write retry
	// loop. That is not once per CommitRequest: a status write that fails permanently is requeued
	// and re-decides, and restart recovery re-reconciles any request whose terminal status never
	// persisted (documented at CommitRequestReconciler.SetupWithManager as a knowingly-accepted
	// gap). Deduplicating across invocations needs a durable marker on the object, which is an API
	// change rather than an instrument. Read rates and ratios from it, not exact request counts.
	CommitRequestsTotal metric.Int64Counter
	// GitCommitsTotal counts commit batches that REACHED THE REMOTE, labelled by the recording
	// BranchWorker's {provider_namespace, provider_name, branch, author_kind} identity plus
	// message_source. Live, snapshot and resync paths all feed this one counter; message_source
	// (commit_request / live / reconcile) is what tells them apart. It counts commits that USED a
	// request-supplied message, not CommitRequests: a request omitting spec.message renders through
	// liveTemplate and counts as live.
	//
	// It is recorded on a successful push, not on local commit creation. Recorded at creation it
	// claimed a commit the remote might never take, so a dead remote and a healthy one produced the
	// same graph — the product's headline metric climbing while nothing reached Git. Counting at
	// the terminal success is also the only place the accounting is right exactly once: a push
	// cycle that hits a moved remote REBUILDS its commits and pushes again.
	GitCommitsTotal metric.Int64Counter

	// PlacementsTotal counts new-file placements resolved for a resource with no document in
	// Git yet — the only case placement runs for — labelled by {source, disposition,
	// gittarget_namespace, gittarget_name, group, version, resource}. source is which
	// mechanism chose the path (by_type / default / kustomize_root / canonical) and disposition is what
	// it did with it (new_file / appended).
	//
	// It exists because sibling inference was deleted: a
	// repository with a hand-authored layout now needs a placement.byType line, and
	// `source="canonical"` is how its operator learns which type in which target is missing
	// one, WITHOUT reading the folder. The (GitTarget, type) labels are the whole point — a
	// bare "a fall-back happened somewhere" counter is not actionable, which is why the
	// design doc argued against leading with one. Cardinality is bounded by targets ×
	// watched types, and placement fires only for a type/name the target has never written.
	//
	// Every increment is a resource that WAS mirrored; a resource the writer refused is
	// PlacementRefusalsTotal instead, never a value of source here.
	PlacementsTotal metric.Int64Counter
	// PlacementRefusalsTotal counts resources the writer declined to place, labelled by
	// {reason, gittarget_namespace, gittarget_name, group, version, resource}. Every
	// increment is a resource NOT in the mirror: a declared template that escapes spec.path
	// or is not identity-complete, a sensitive resource whose path is already taken, a
	// plaintext resource routed at an encrypted file, or two resources of mixed sensitivity
	// racing onto one brand-new file. The write is retried on the next event or resync, so a
	// steady non-zero rate means a policy that needs fixing rather than a transient.
	//
	// This is the counter that did not exist: a refusal left a log line at the skip site and,
	// on the resync path only, ResyncStats.PlacementSkipped — a field in a summary, not a
	// series anything can alert on.
	PlacementRefusalsTotal metric.Int64Counter
	// PlacementKustomizationEntriesTotal counts attempts to add a new file to the
	// resources: list of the kustomization that governs it, labelled by {outcome,
	// gittarget_namespace, gittarget_name}. outcome is added, no_change, or failed.
	//
	// `failed` is the one to watch, and it is otherwise invisible: the document is committed
	// and the entry is not, so kustomize never builds the file. The object is in Git, looks
	// mirrored, and is not applied by anything.
	PlacementKustomizationEntriesTotal metric.Int64Counter

	// WatchEventsTotal is the ingest census: every event a target watch delivers is counted here
	// exactly once, labelled by {gittarget_namespace, gittarget_name, group, version, resource,
	// outcome}. It is the first stage of the pipeline and, until it existed, the only stage with no
	// instrument at all — "nothing is happening", "the rules filtered everything" and "delivery is
	// failing" were indistinguishable from outside.
	//
	// outcome is a frozen enum, and its values fall into three classes rather than "healthy and
	// unhealthy" — several of these are correct outcomes:
	//
	//   EXPECTED (the pipeline working):
	//     routed              — reached the writer. The number the funnel starts from.
	//     unchanged           — a live UPDATE whose sanitized content equals what Git already
	//                           holds: the /status-churn case, expected in volume.
	//     operation_filtered  — the rule's operation set does not select this verb.
	//     bookmark            — a watch bookmark, carrying cursor progress and no object.
	//     shutdown            — the stream was cancelled while the event waited for its author.
	//                           Not loss: a restart replays and resyncs the object.
	//
	//   DEGRADED (working, but something upstream is odd):
	//     not_object          — the event carried no decodable object.
	//     stream_error        — the API server sent an error frame that is NOT an expired cursor
	//                           (those are classified as a session end, reason=expired, and never
	//                           reach this census). The session dies and the reconnect replays, so
	//                           nothing observed is dropped — but a non-zero rate is an API server
	//                           saying something about this watch that it does not say routinely.
	//
	//   LOSS (an observed change that did not reach Git):
	//     route_failed        — the writer refused it. Nothing retries until the next resync, so
	//                           the mirror is behind for that object.
	//
	// route_failed OVERLAPS git_queue_drops_total: a full worker queue is one of the ways a route
	// fails, so one dropped event can increment both. They are two views of one event — where it
	// was refused, and by what — so a panel may show either, and a sum over both counts that event
	// twice. Neither is a unique-loss total.
	//
	// It carries the GitTarget because "which tenant stopped receiving events" is the question, and
	// deliberately NOT the watch event type (added/modified/deleted): that halves the series
	// budget, and the written-versus-deleted split is answered better at the writer by
	// GitDocumentsTotal.
	WatchEventsTotal metric.Int64Counter
	// WatchEventHandlingSeconds records how long a target watch stream was BUSY on one event,
	// labelled by {group, version, resource}: the whole of routeLiveTargetWatchEvent, attribution
	// wait included.
	//
	// This is the head-of-line signal, and it is a proven failure rather than a theoretical one: a
	// slow attribution resolution blocks the events queued BEHIND it on the same single-threaded
	// stream, which broke a CommitRequest e2e spec and was only ever visible by correlating two
	// log lines by hand. The attribution wait histogram times each resolution in isolation and
	// cannot see the delay one imposes on its neighbours; occupancy can, because a stream that is
	// busy is a stream nothing else is being read from.
	//
	// It measures OCCUPANCY, not queue delay, and the difference is deliberate. Measuring the wait
	// itself needs an arrival timestamp stamped before the blocking consumer, and the events arrive
	// on a client-go watch channel this process does not fill — there is nowhere honest to stamp
	// one. Timing after the dequeue would name a wait it never observed. Read saturation as
	// rate(_sum[5m]), the fraction of wall time the stream spent unavailable; approaching 1 means
	// events are queueing behind it.
	WatchEventHandlingSeconds metric.Float64Histogram
	// WatchSessionsEndedTotal counts watch sessions that ended, labelled by {group, version,
	// resource, reason}: `expired` (the resourceVersion fell out of history — 410 pressure, which
	// forces a full replay), `error`, or `stopped` (the plan retired the stream, which is routine).
	// A restart storm is a rebuild storm, and a rebuild walks the whole type.
	WatchSessionsEndedTotal metric.Int64Counter
	// WatchReplayDurationSeconds records how long an initial-events replay took to reach
	// initial-events-end, labelled by {group, version, resource}. It is what a 410 storm actually
	// charges: the cost of every rebuild it forces.
	WatchReplayDurationSeconds metric.Float64Histogram

	// WatchRecoveryTotal counts completed watch recoveries, labelled by {gittarget_namespace,
	// gittarget_name, group, resource, mode}. mode names WHICH recovery path ran:
	// `cursor_resume` (a durable cursor was still in history, so no replay was needed),
	// `type_reconcile` (a per-type snapshot resync applied on the branch worker), `replay` (a full
	// sendInitialEvents rebuild), or `list_fallback` (the source does not support streaming lists,
	// which is the aggregated-API case and worth knowing about).
	//
	// It was called TargetReconcileCompletedTotal with a `trigger` label whose documented value
	// (`rule_change`) the code never emitted. The name described the caller rather than the event.
	//
	// No `version` label: a recovery covers a CELL, which is keyed by group/resource, and the
	// per-type reconcile path genuinely does not know a served version. An empty version on that
	// arm beside a populated one on the cursor-resume arm would be worse than no label.
	//
	// A counter, not a latched gauge, on purpose: a counter resets to 0 on a fresh pod, so a
	// per-pod `{pod="<new>"} > 0` check after a rollout proves the new pod did its own recovery —
	// robust to the old pod's stale series that a Prometheus pod scrape may still be holding during
	// the rollout, which a latched gauge (or a cross-pod sum-over-baseline) cannot distinguish.
	// The label keys avoid the reserved `namespace`/`name`: a pod scrape with honor_labels=false
	// would overwrite a metric's `namespace` attribute with the scraped pod's own namespace, making
	// a per-GitTarget `namespace` selector silently match nothing. Load-bearing for the
	// restart-reconcile e2e spec; treat the name and labels as a public observability contract.
	WatchRecoveryTotal metric.Int64Counter

	// GitCommitFailuresTotal counts work that was routed to a worker and then died before it
	// could ever be pushed, labelled by {provider_namespace, provider_name, branch, kind, reason}.
	// kind is `window` (an ordinary live commit window) or `atomic` (a snapshot/resync request);
	// reason is `refused` (the acceptance gate or a write-boundary precondition rejected the plan,
	// which needs a human to fix the Git path) or `error` (a write fault).
	//
	// This was the largest remaining hole. A commit failure drops the whole window — the events are
	// already lost to the failed flush — and it happens AFTER routing and BEFORE pushing, so
	// neither GitQueueDropsTotal nor GitPushesTotal sees it. The mirror silently falls behind for
	// every object in that window until the next resync re-derives them, and until now the only
	// trace was a log line (or, for a refusal, a GitTarget condition nobody is alerting on).
	GitCommitFailuresTotal metric.Int64Counter
	// GitPushesTotal counts push CYCLES at their terminal end, labelled by {provider_namespace,
	// provider_name, branch, outcome} where outcome is `pushed` or `failed`. A cycle that exhausts
	// its replay retries was previously a log line and nothing else: the mirror stops advancing and
	// every other metric reads healthy, because commits are still being created locally.
	GitPushesTotal metric.Int64Counter
	// GitPushRetriesTotal counts replay rounds inside a push cycle, labelled by
	// {provider_namespace, provider_name, branch, reason}. reason has one value, `remote_moved`:
	// the remote branch moved, so the pending writes are rebuilt on the new head and re-pushed.
	// Every other rejection ends the cycle rather than retrying it, and is a `failed` outcome on
	// GitPushesTotal. A retry is not a terminal outcome, which is why it is its own counter rather
	// than a third value there; rate(retries)/rate(pushes) is the contention signal.
	GitPushRetriesTotal metric.Int64Counter
	// GitPushDurationSeconds records one push cycle's wall time, labelled by
	// {provider_namespace, provider_name, branch}. Retries are inside the measurement on purpose:
	// what an operator wants is how long it took the mirror to accept the work, not how fast one
	// attempt was.
	GitPushDurationSeconds metric.Float64Histogram
	// GitQueueDropsTotal counts work the branch worker threw away because its queue was full,
	// labelled by {provider_namespace, provider_name, branch, kind} where kind is `write`,
	// `attach` or `resync`. Every increment is lost work: a write is recovered only by the next
	// resync, and until then the mirror is behind for that object with no other trace.
	//
	// The queue-depth gauge said the queue was deep. Nothing said anything had been dropped, which
	// is the one thing an operator needs to know, and a saturating queue is exactly when it
	// happens.
	GitQueueDropsTotal metric.Int64Counter

	// GitResyncFailuresTotal counts rule-change resyncs whose apply failed or
	// timed out at the worker AFTER being enqueued. Delivery is marked on enqueue (the
	// resync is fire-and-forget to avoid an unbounded re-gather loop — see
	// Manager.recordWatchRecovery), so a failed background apply is otherwise
	// only logged. This counter makes those failures observable/alertable without
	// triggering an immediate re-gather. Labelled by {gittarget_namespace,
	// gittarget_name}; a sustained increase means snapshots are not committing and the
	// folder is relying on steady-state events to catch up.
	GitResyncFailuresTotal metric.Int64Counter

	// AuditEventsTotal is the single per-event census: every successfully decoded, converted, and
	// validated audit event increments it exactly once, labelled by {outcome, category, group,
	// version, resource, verb}. Audit is attribution-only — it names the author of a watch-observed
	// change; it never carries object state. Liveness = sum(...) > 0; the e2e invariant gates on
	// category="error" == 0.
	AuditEventsTotal metric.Int64Counter
	// AuditEventListDurationSeconds times every request at /audit-webhook, labelled by bounded
	// outcome: bad_method, bad_path and bare_endpoint_disabled for requests refused at the door,
	// then processed, empty, decode_error and process_error.
	//
	// Its _count series IS the request counter — a histogram ships its own observation count — so
	// there is no separate one. The per-event census is AuditEventsTotal.
	AuditEventListDurationSeconds metric.Float64Histogram
	// AttributionResolutionsTotal counts watch-event attribution resolver outcomes, labelled by
	// {tier, actor_kind, group, version, resource}. tier names WHICH evidence answered
	// (delete_sticky/exact/deletecollection_body_uid/latest/name/deletecollection_scope/
	// resource_version/absent) and actor_kind
	// names WHO it named (user/serviceaccount/none) — two orthogonal questions, so they are two
	// labels. Match coverage is tier!="absent"; anything narrower reads the collection and name
	// tiers as misses.
	AttributionResolutionsTotal metric.Int64Counter
	// AttributionFactsTotal counts attribution fact lifecycle events, labelled by bounded op:
	// "written" is one fact appended to the fact log, "matched" is one joined by a watch event.
	// Together they say how much of what is published is ever used. They are NOT subtractable:
	// written counts every type, matched only the streams this process follows.
	AttributionFactsTotal metric.Int64Counter
	// AttributionResolutionWaitSeconds records resolver wait time by {tier, event_kind, group,
	// version, resource}. event_kind is write or removal, and the split is load-bearing: a removal
	// holds a fallback and keeps waiting where a write does not, so the removal wait is the number
	// --author-attribution-grace is tuned from.
	AttributionResolutionWaitSeconds metric.Float64Histogram
	// AttributionFactIndexEntries gauges the entries the in-memory fact index currently holds across
	// every scope and match structure. Read against the eviction counter it says whether the caps
	// are binding.
	AttributionFactIndexEntries metric.Int64Gauge
	// The three fact-loss counters below all mean "attribution that will never happen", and they
	// are deliberately NOT one counter, because they do not count the same THING:
	//
	//   an eviction    is one FACT, known exactly.
	//   a trim gap     is one OCCURRENCE, spanning an unknown number of entries.
	//   a decode error is one ENTRY, and an entry carries a whole audit batch's facts.
	//
	// Adding them produces a number in no unit at all. They were briefly merged into a single
	// `attribution_facts_lost_total{reason}` on the argument that an operator reads them together
	// and wants one alert; that is true, and it is what a recording rule is for. A metric's name
	// has to be true about what it counts before it is convenient.

	// AttributionFactIndexEvictionsTotal counts FACTS dropped from the in-memory fact index because
	// it was full, labelled by bounded reason (per_type/total). An attribution lost to a full index
	// has to look different from one that was never published, or a burst is silently absorbed.
	//
	// It is also the removal pointer's only horizon: every other index entry expires on the fact
	// TTL. Note it does not prove the fact went unused — a fact may have been matched already and
	// then evicted — so read it as pressure on the caps rather than as a count of lost joins.
	AttributionFactIndexEvictionsTotal metric.Int64Counter
	// AttributionFactStreamGapsTotal counts OCCASIONS a fact stream was trimmed past this process's
	// follower, labelled by stream. Every gap is facts lost for good, and how many is unknowable:
	// the entries are gone. It is the one loss a log transport can see at all.
	AttributionFactStreamGapsTotal metric.Int64Counter
	// AttributionFactStreamDecodeErrorsTotal counts fact-stream ENTRIES the follower could not
	// decode, labelled by transport. Such an entry is skipped and its position passed, so the whole
	// audit batch it carried is lost — and unlike a trim gap the loss leaves no other trace, which
	// is why this is the loss path that most needed a counter.
	AttributionFactStreamDecodeErrorsTotal metric.Int64Counter
	// AttributionCollectionWithoutUIDSetTotal counts collection facts published without the uid set
	// the precise join would have used, labelled by bounded reason (uid_cap/no_uids). The scope
	// fallback is already correct, so this says how often the precise path was available — not that
	// anything broke.
	AttributionCollectionWithoutUIDSetTotal metric.Int64Counter
	// AttributionFactFollowerErrorsTotal counts fact-follower read failures, labelled by transport.
	// The follower retries with a backoff rather than returning, so the errors are otherwise only a
	// log line.
	AttributionFactFollowerErrorsTotal metric.Int64Counter
	// AttributionFactFollowerLastSuccessTimestampSeconds gauges the Unix time of the follower's last
	// successful read, idle rounds included. It matters more than the error counter: only it
	// separates "erroring occasionally while making progress" from "has read nothing in ten
	// minutes", and only the second is an outage. Read it as time() - <gauge>.
	AttributionFactFollowerLastSuccessTimestampSeconds metric.Int64Gauge
	// AttributionTransportInfo is an info gauge, always 1, labelled by the fact transport in force
	// (redis/memory). It is interpretive metadata rather than a signal: a burst of unresolved
	// commits after a restart is EXPECTED under the in-process transport, which drops every fact
	// with the process, and a bug under Redis.
	AttributionTransportInfo metric.Int64Gauge

	// APICatalogResources gauges the count of served top-level resources in the catalog,
	// split by the default-watch-policy allowed/excluded state.
	APICatalogResources metric.Int64Gauge
	// APICatalogGroupVersions gauges discovered group/versions, split into trusted vs degraded.
	APICatalogGroupVersions metric.Int64Gauge
	// APICatalogRefreshTotal counts API resource catalog refreshes by outcome.
	APICatalogRefreshTotal metric.Int64Counter
	// APICatalogRefreshDurationSeconds records the wall time of one catalog refresh.
	APICatalogRefreshDurationSeconds metric.Float64Histogram

	// The watch-plane owner is a queue, and a queue that grows silently is what makes a stall
	// hard to see. These are the queue's instrument panel; see
	// docs/design/watch-manager-ownership.md. Its two gauges — dirty depth and the timestamp the
	// oldest dirty target went dirty — are OBSERVABLE and live in gauges.go, because a gauge this
	// loop pushes stops moving exactly when the loop stops.

	// WatchPlanPassesTotal counts plan passes by {outcome, gittarget_namespace,
	// gittarget_name}, where outcome is completed, failed, or timed_out. It separates "not
	// running" from "running and failing", and timed_out from a real error because the two have
	// different causes.
	WatchPlanPassesTotal metric.Int64Counter
	// WatchPlanPassDurationSeconds records the wall time of one plan pass. It is the input to
	// choosing the per-target deadline.
	WatchPlanPassDurationSeconds metric.Float64Histogram
	// WatchPlanTriggersTotal counts triggers by {reason, coalesced}. reason is declare,
	// rule_change, shared_refresh or periodic, and shows which source is noisy. coalesced is
	// "true" when the trigger landed on a GitTarget that was ALREADY dirty, which is the proof
	// that the silence window does what it claims: one `kubectl apply` of a GitTarget and four
	// WatchRules should show four coalesced triggers and one pass.
	//
	// coalesced is a label rather than the second counter it used to be, so the ratio is one
	// metric's business instead of a division across two.
	WatchPlanTriggersTotal metric.Int64Counter

	// SecretEncryptionsTotal counts Secret encryption decisions, labelled by bounded outcome:
	// "encrypted" (the encryptor ran and produced ciphertext), "failed" (it ran and errored, and
	// the write is rejected), or "cached" (already-encrypted content was reused because the
	// sensitive marker was unchanged).
	//
	// It replaces five counters over two populations. attempts was incremented immediately before
	// Encrypt, so it was exactly success + failures; and cache_hits and marker_skips were
	// incremented on consecutive lines of the same branch, on every path, so they could never
	// differ. The documented "cache effectiveness" ratio was cache_hits / attempts, which divided
	// over disjoint populations and could exceed 1 — with one counter it is a share of one total.
	SecretEncryptionsTotal metric.Int64Counter
)

// cardinalityLimit caps the data points ONE instrument may publish per collection. Past it the SDK
// collapses the rest into a single otel.metric.overflow=true point, losing the labels that identify
// them — a metric that reads plausible and names nothing.
//
// The SDK's own default is 2000, which three families exceed at the install size
// docs/design/metrics-observability-plan.md §7.1 models: watch_events_total (3,600),
// git_documents_total (3,000) and placements_total (4,800 ceiling). This is roughly 3x the largest
// of those, so an install several times the model still names its objects instead of truncating
// them. It is PER INSTRUMENT and says nothing about the process total, which is the sum over every
// instrument; §7.1 owns that estimate. For a histogram it is more conservative than it looks, since
// one data point exports bucket count + 2 series — which is why §7.1 keeps histogram label sets
// small rather than relying on this.
//
// Raised rather than removed, so an unbounded label set overflows visibly instead of growing
// without bound. Overflow is only a signal if someone watches for it: alert on
// otel_metric_overflow="true".
const cardinalityLimit = 15000

// InitOTLPExporter initializes the OTLP-to-Prometheus bridge.
func InitOTLPExporter(_ context.Context) (func(context.Context) error, error) {
	fmt.Println("Initializing OTLP exporter")

	// Create a Prometheus exporter that bridges OTLP metrics to Prometheus
	// Configure it to use the controller-runtime registry.
	exporter, err := prometheus.New(
		prometheus.WithRegisterer(metrics.Registry),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create Prometheus exporter: %w", err)
	}

	// Create a meter provider with the Prometheus exporter.
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(exporter),
		sdkmetric.WithCardinalityLimit(cardinalityLimit),
	)
	otel.SetMeterProvider(provider)

	// Get the meter from the new provider.
	otelMeter = provider.Meter("gitops-reverser")

	if err := registerInstruments(); err != nil {
		return nil, err
	}

	return func(_ context.Context) error {
		fmt.Println("Shutting down OTLP exporter")
		return nil
	}, nil
}

// InitTestExporter wires the global instruments to a meter provider backed by a
// manual reader, so unit tests can collect and assert recorded metric values.
// It returns the reader to collect from.
func InitTestExporter() (*sdkmetric.ManualReader, error) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithCardinalityLimit(cardinalityLimit),
	)
	otel.SetMeterProvider(provider)
	otelMeter = provider.Meter("gitops-reverser")
	if err := registerInstruments(); err != nil {
		return nil, err
	}
	return reader, nil
}

// Instrument registration spec types. Each pairs a metric name with the
// package-level variable that receives the created instrument.
type (
	cSpec struct {
		name string
		dest *metric.Int64Counter
	}
	hSpec struct {
		name    string
		dest    *metric.Float64Histogram
		buckets []float64
	}
	gSpec struct {
		name string
		dest *metric.Int64Gauge
	}
)

// registerInstruments creates every metric instrument against the current
// otelMeter and stores it in its package-level variable, one kind at a time.
func registerInstruments() error {
	if err := registerCounters(); err != nil {
		return err
	}
	if err := registerHistograms(); err != nil {
		return err
	}
	return registerGauges()
}

func registerCounters() error {
	counters := []cSpec{
		{"gitopsreverser_git_documents_total", &GitDocumentsTotal},
		{"gitopsreverser_git_commits_total", &GitCommitsTotal},
		{"gitopsreverser_commit_requests_total", &CommitRequestsTotal},
		{"gitopsreverser_git_commit_failures_total", &GitCommitFailuresTotal},
		{"gitopsreverser_git_pushes_total", &GitPushesTotal},
		{"gitopsreverser_git_push_retries_total", &GitPushRetriesTotal},
		{"gitopsreverser_git_queue_drops_total", &GitQueueDropsTotal},
		{"gitopsreverser_placements_total", &PlacementsTotal},
		{"gitopsreverser_placement_refusals_total", &PlacementRefusalsTotal},
		{
			"gitopsreverser_placement_kustomization_entries_total",
			&PlacementKustomizationEntriesTotal,
		},
		{"gitopsreverser_watch_events_total", &WatchEventsTotal},
		{"gitopsreverser_watch_sessions_ended_total", &WatchSessionsEndedTotal},
		{"gitopsreverser_watch_recovery_total", &WatchRecoveryTotal},
		{"gitopsreverser_git_resync_failures_total", &GitResyncFailuresTotal},
		{"gitopsreverser_audit_events_total", &AuditEventsTotal},
		{"gitopsreverser_attribution_resolutions_total", &AttributionResolutionsTotal},
		{"gitopsreverser_attribution_facts_total", &AttributionFactsTotal},
		{"gitopsreverser_attribution_fact_index_evictions_total", &AttributionFactIndexEvictionsTotal},
		{"gitopsreverser_attribution_fact_stream_gaps_total", &AttributionFactStreamGapsTotal},
		{
			"gitopsreverser_attribution_fact_stream_decode_errors_total",
			&AttributionFactStreamDecodeErrorsTotal,
		},
		{
			"gitopsreverser_attribution_collection_without_uidset_total",
			&AttributionCollectionWithoutUIDSetTotal,
		},
		{"gitopsreverser_attribution_fact_follower_errors_total", &AttributionFactFollowerErrorsTotal},
		{"gitopsreverser_api_catalog_refresh_total", &APICatalogRefreshTotal},
		{"gitopsreverser_secret_encryptions_total", &SecretEncryptionsTotal},
		{"gitopsreverser_watch_plan_passes_total", &WatchPlanPassesTotal},
		{"gitopsreverser_watch_plan_triggers_total", &WatchPlanTriggersTotal},
	}
	for _, s := range counters {
		v, err := otelMeter.Int64Counter(s.name)
		if err != nil {
			return err
		}
		*s.dest = v
	}
	return nil
}

func registerHistograms() error {
	// eventListDurationBuckets span the webhook's EventList answer time: sub-millisecond decode
	// up through a slow request, plus headroom for an attribution lookup wait.
	eventListDurationBuckets := []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 5, 30, 300}
	// catalogRefreshBuckets span discovery latency: two cached GETs on an aggregated
	// apiserver (sub-second) up to a slow per-group fallback (seconds).
	catalogRefreshBuckets := []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
	// attributionWaitBuckets span zero-wait hits up through the default grace window
	// and slower configured waits.
	attributionWaitBuckets := []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 3, 5, 10}
	// watchPlanPassBuckets span one target's plan pass: in-memory replanning (sub-millisecond)
	// up through a first observation of a cluster and on to the per-target deadline.
	watchPlanPassBuckets := []float64{0.0005, 0.001, 0.005, 0.025, 0.1, 0.5, 1, 5, 15, 30}
	// gitPushBuckets span a push to a healthy nearby remote (tens of milliseconds) up through a
	// contended one that replays, and on to a remote that is timing out.
	gitPushBuckets := []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}
	// watchHandlingBuckets span an event routed immediately through one that sat out the whole
	// attribution grace window, and past it.
	watchHandlingBuckets := []float64{0.0005, 0.001, 0.005, 0.025, 0.1, 0.5, 1, 3, 10, 30}
	// watchReplayBuckets span a replay of a handful of objects through a full walk of a busy type.
	watchReplayBuckets := []float64{0.05, 0.1, 0.5, 1, 5, 15, 30, 60, 300}
	hists := []hSpec{
		{"gitopsreverser_git_push_duration_seconds", &GitPushDurationSeconds, gitPushBuckets},
		{"gitopsreverser_watch_event_handling_seconds", &WatchEventHandlingSeconds, watchHandlingBuckets},
		{"gitopsreverser_watch_replay_duration_seconds", &WatchReplayDurationSeconds, watchReplayBuckets},
		{
			"gitopsreverser_watch_plan_pass_duration_seconds",
			&WatchPlanPassDurationSeconds,
			watchPlanPassBuckets,
		},
		{"gitopsreverser_audit_eventlist_duration_seconds", &AuditEventListDurationSeconds, eventListDurationBuckets},
		{
			"gitopsreverser_attribution_resolution_wait_seconds",
			&AttributionResolutionWaitSeconds,
			attributionWaitBuckets,
		},
		{
			"gitopsreverser_api_catalog_refresh_duration_seconds",
			&APICatalogRefreshDurationSeconds,
			catalogRefreshBuckets,
		},
	}
	for _, s := range hists {
		opts := []metric.Float64HistogramOption{}
		if len(s.buckets) > 0 {
			opts = append(opts, metric.WithExplicitBucketBoundaries(s.buckets...))
		}
		v, err := otelMeter.Float64Histogram(s.name, opts...)
		if err != nil {
			return err
		}
		*s.dest = v
	}
	return nil
}

func registerGauges() error {
	// Synchronous gauges: each is stamped by the event it describes, so there is no loop between
	// the state and the value and nothing for a callback to improve. The follower timestamp is the
	// worked example — it SHOULD stop advancing when the follower wedges, because that is the
	// signal.
	gauges := []gSpec{
		{"gitopsreverser_api_catalog_resources", &APICatalogResources},
		{"gitopsreverser_api_catalog_group_versions", &APICatalogGroupVersions},
		{"gitopsreverser_attribution_fact_index_entries", &AttributionFactIndexEntries},
		{
			"gitopsreverser_attribution_fact_follower_last_success_timestamp_seconds",
			&AttributionFactFollowerLastSuccessTimestampSeconds,
		},
		{"gitopsreverser_attribution_transport_info", &AttributionTransportInfo},
	}
	for _, s := range gauges {
		v, err := otelMeter.Int64Gauge(s.name)
		if err != nil {
			return err
		}
		*s.dest = v
	}
	return registerObservableGauges()
}

// registerObservableGauges creates the gauges whose value is READ at scrape time from a source a
// producer installs with SetGaugeSource. See gauges.go for why these in particular cannot be
// pushed. Most of them measure saturation of a loop, so publishing from inside that loop freezes
// the value during the stall it exists to report. The two keyed on config objects —
// resource_condition and git_branch_targets — are here for the other reason: they need series that
// STOP when the object is deleted, which a pushed gauge cannot do.
func registerObservableGauges() error {
	observable := []struct {
		name   string
		source string
	}{
		{"gitopsreverser_git_queue_depth", GaugeGitQueueDepth},
		{"gitopsreverser_watch_types", GaugeWatchTypes},
		{"gitopsreverser_watch_streams_open", GaugeWatchStreamsOpen},
		{"gitopsreverser_watch_plan_dirty_targets", GaugeWatchPlanDirtyTargets},
		{
			"gitopsreverser_watch_plan_oldest_dirty_since_timestamp_seconds",
			GaugeWatchPlanOldestDirtySince,
		},
		{"gitopsreverser_resource_condition", GaugeResourceCondition},
		{"gitopsreverser_git_branch_targets", GaugeGitBranchTargets},
	}
	for _, o := range observable {
		if _, err := otelMeter.Int64ObservableGauge(
			o.name,
			metric.WithInt64Callback(observeGauge(o.source)),
		); err != nil {
			return err
		}
	}

	// resource_condition is the one observable gauge whose state this package owns, so it installs
	// its own source here rather than waiting for a producer to call SetGaugeSource. The registry
	// it reads is package state that is always safe to read and empty until a reconcile publishes,
	// so there is no lifecycle to manage and nothing for the source to outlive.
	SetGaugeSource(GaugeResourceCondition, resourceConditionSamples)
	// git_branch_targets is the same case: package state, always safe to read, empty until a
	// GitTarget reconcile publishes into it.
	SetGaugeSource(GaugeGitBranchTargets, branchTargetSamples)
	return nil
}
