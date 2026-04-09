https://kubernetes.io/docs/reference/command-line-tools-reference/kube-apiserver/

--audit-log-batch-buffer-size int     Default: 10000
The size of the buffer to store events before batching and writing. Only used in batch mode.

--audit-log-batch-max-size int     Default: 1
The maximum size of a batch. Only used in batch mode.

--audit-log-batch-max-wait duration
The amount of time to wait before force writing the batch that hadn't reached the max size. Only used in batch mode.

--audit-log-batch-throttle-burst int
Maximum number of requests sent at the same moment if ThrottleQPS was not utilized before. Only used in batch mode.

--audit-log-batch-throttle-enable
Whether batching throttling is enabled. Only used in batch mode.

--audit-log-batch-throttle-qps float
Maximum average number of batches per second. Only used in batch mode.

--audit-log-compress
If set, the rotated log files will be compressed using gzip.

--audit-log-format string     Default: "json"
Format of saved audits. "legacy" indicates 1-line text format for each event. "json" indicates structured json format. Known formats are legacy,json.

--audit-log-maxage int
The maximum number of days to retain old audit log files based on the timestamp encoded in their filename.

--audit-log-maxbackup int
The maximum number of old audit log files to retain. Setting a value of 0 will mean there's no restriction on the number of files.

--audit-log-maxsize int
The maximum size in megabytes of the audit log file before it gets rotated.

--audit-log-mode string     Default: "blocking"
Strategy for sending audit events. Blocking indicates sending events should block server responses. Batch causes the backend to buffer and write events asynchronously. Known modes are batch,blocking,blocking-strict.

--audit-log-path string
If set, all requests coming to the apiserver will be logged to this file. '-' means standard out.

--audit-log-truncate-enabled
Whether event and batch truncating is enabled.

--audit-log-truncate-max-batch-size int     Default: 10485760
Maximum size of the batch sent to the underlying backend. Actual serialized size can be several hundreds of bytes greater. If a batch exceeds this limit, it is split into several batches of smaller size.

--audit-log-truncate-max-event-size int     Default: 102400
Maximum size of the audit event sent to the underlying backend. If the size of an event is greater than this number, first request and response are removed, and if this doesn't reduce the size enough, event is discarded.

--audit-log-version string     Default: "audit.k8s.io/v1"
API group and version used for serializing audit events written to log.

--audit-policy-file string
Path to the file that defines the audit policy configuration.

--audit-webhook-batch-buffer-size int     Default: 10000
The size of the buffer to store events before batching and writing. Only used in batch mode.

--audit-webhook-batch-max-size int     Default: 400
The maximum size of a batch. Only used in batch mode.

--audit-webhook-batch-max-wait duration     Default: 30s
The amount of time to wait before force writing the batch that hadn't reached the max size. Only used in batch mode.

--audit-webhook-batch-throttle-burst int     Default: 15
Maximum number of requests sent at the same moment if ThrottleQPS was not utilized before. Only used in batch mode.

--audit-webhook-batch-throttle-enable     Default: true
Whether batching throttling is enabled. Only used in batch mode.

--audit-webhook-batch-throttle-qps float     Default: 10
Maximum average number of batches per second. Only used in batch mode.

--audit-webhook-config-file string
Path to a kubeconfig formatted file that defines the audit webhook configuration.

--audit-webhook-initial-backoff duration     Default: 10s
The amount of time to wait before retrying the first failed request.

--audit-webhook-mode string     Default: "batch"
Strategy for sending audit events. Blocking indicates sending events should block server responses. Batch causes the backend to buffer and write events asynchronously. Known modes are batch,blocking,blocking-strict.

--audit-webhook-truncate-enabled
Whether event and batch truncating is enabled.

--audit-webhook-truncate-max-batch-size int     Default: 10485760
Maximum size of the batch sent to the underlying backend. Actual serialized size can be several hundreds of bytes greater. If a batch exceeds this limit, it is split into several batches of smaller size.

--audit-webhook-truncate-max-event-size int     Default: 102400
Maximum size of the audit event sent to the underlying backend. If the size of an event is greater than this number, first request and response are removed, and if this doesn't reduce the size enough, event is discarded.


---

The today here is that we should come up with some advise on how to self host these: which values matter? And why or why not set them? Especially focus on not getting to many errors during setup I guess :-)
