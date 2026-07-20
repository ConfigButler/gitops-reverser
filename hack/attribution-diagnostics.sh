#!/usr/bin/env bash
# Post-run diagnostics for the author-attribution path.
#
# When no audit fact matches within the grace window (watch.DefaultAttributionGraceWindow,
# 3s), the commit is visibly authored as `unknown (attribution unresolved)` rather than as
# the configured committer. For a live change that should be attributable, that author is a
# concrete signal to check audit-attribution configuration and delivery.
#
# It answers the one question that splits the diagnosis in two:
#
#   Is the fact ABSENT from Redis (the apiserver never delivered it — batching, queue
#   overflow, a routing error), or is it PRESENT but was written too late for the 3s
#   grace window (the resolver gave up first)?
#
# Fact keys carry a 10-minute TTL, so the remaining TTL back-computes when each fact
# landed: age = ttl_total - ttl_remaining. Comparing that against the fact's own
# stageTimestamp gives the apiserver→Redis delivery lag directly.
#
# Usage:  hack/attribution-diagnostics.sh [name-substring]
# Run it IMMEDIATELY after an e2e run — facts expire after 10 minutes.
set -euo pipefail

CTX="${CTX:-k3d-gitops-reverser-test-e2e}"
VALKEY_NS="${VALKEY_NS:-valkey-e2e}"
PROM_NS="${PROM_NS:-prometheus-operator}"
FACT_TTL_SECONDS="${FACT_TTL_SECONDS:-600}"
FILTER="${1:-}"

kc() { kubectl --context "${CTX}" "$@"; }

echo "== attribution resolution outcomes (Prometheus) =="
echo "   absent = Git author was unknown (attribution unresolved); no actor was named."
prom_pod_port=9097
kc -n "${PROM_NS}" port-forward svc/prometheus-operated ${prom_pod_port}:9090 >/dev/null 2>&1 &
pf=$!
trap 'kill ${pf} 2>/dev/null || true' EXIT
sleep 5
curl -sG "http://localhost:${prom_pod_port}/api/v1/query" \
  --data-urlencode 'query=sum by (result) (gitopsreverser_attribution_resolutions_total)' |
  python3 -c '
import json,sys
d=json.load(sys.stdin)
rows=[(r["metric"].get("result","?"), float(r["value"][1])) for r in d.get("data",{}).get("result",[])]
total=sum(v for _,v in rows) or 1
for k,v in sorted(rows, key=lambda x:-x[1]):
    print(f"   {k:32s} {v:8.0f}  ({100*v/total:5.1f}%)")
print(f"   {'TOTAL':32s} {total:8.0f}")
absent=dict(rows).get("absent",0)
if absent:
    print(f"\n   >> {absent:.0f} resolution(s) produced the explicit unresolved Git author.")
    print( "   >> For a change that should be attributable, inspect the audit policy, route,")
    print( "   >> source identity, and Redis delivery below.")
' || echo "   (query failed)"

echo
echo "== apiserver-side audit delivery =="
curl -sG "http://localhost:${prom_pod_port}/api/v1/query" \
  --data-urlencode 'query=sum(apiserver_audit_requests_rejected_total) or vector(0)' |
  python3 -c 'import json,sys; d=json.load(sys.stdin); r=d.get("data",{}).get("result",[]); print("   apiserver_audit_requests_rejected_total =", r[0]["value"][1] if r else "n/a (not scraped)")' \
  || echo "   (query failed)"
curl -sG "http://localhost:${prom_pod_port}/api/v1/query" \
  --data-urlencode 'query=sum(gitopsreverser_audit_eventlist_events_total) or vector(0)' |
  python3 -c 'import json,sys; d=json.load(sys.stdin); r=d.get("data",{}).get("result",[]); print("   audit events INGESTED by us          =", r[0]["value"][1] if r else "0")' \
  || echo "   (query failed)"
kill ${pf} 2>/dev/null || true

echo
echo "== attribution facts present in Valkey =="
pw="$(kc -n "${VALKEY_NS}" get secret valkey-auth -o jsonpath='{.data.*}' | head -1 | base64 -d)"
pod="$(kc -n "${VALKEY_NS}" get pods -o jsonpath='{.items[0].metadata.name}')"
vc() { kc -n "${VALKEY_NS}" exec "${pod}" -c valkey -- valkey-cli -a "${pw}" --no-auth-warning "$@" 2>/dev/null; }

keys="$(vc --scan --pattern '*author:v1:audit:*' | { [ -n "${FILTER}" ] && grep -F "${FILTER}" || cat; } | head -40)"
if [ -z "${keys}" ]; then
  echo "   no facts matching ${FILTER:-<all>}"
  echo "   >> If a commit has the unresolved author AND no fact exists, check audit delivery:"
  echo "   >> check audit-webhook-batch-max-wait/-size and the /audit-webhook/<provider> route."
  exit 0
fi

echo "   (fact age is back-computed from the remaining TTL against a ${FACT_TTL_SECONDS}s TTL)"
for k in ${keys}; do
  val="$(vc GET "${k}")"
  ttl="$(vc TTL "${k}")"
  python3 - "${k}" "${val}" "${ttl}" "${FACT_TTL_SECONDS}" <<'PY'
import json,sys
key,val,ttl,total = sys.argv[1],sys.argv[2],sys.argv[3],int(sys.argv[4])
try:
    f=json.loads(val)
except Exception:
    print(f"   {key}  <unparseable>"); raise SystemExit
try:
    age=total-int(ttl)
except ValueError:
    age="?"
ns=f.get("namespace",""); name=f.get("name","")
print(f"   {ns}/{name}  verb={f.get('verb','?')}  author={f.get('author','?')}")
print(f"      stageTimestamp={f.get('stageTimestamp','?')}  written_to_redis≈{age}s ago")
PY
done

echo
echo "   Reading this: a fact PRESENT here for an object whose commit has the unresolved author means"
echo "   the fact arrived, just not within the 3s grace — the grace window (or the audit"
echo "   batching delay ahead of it) is the problem, not fact delivery."
