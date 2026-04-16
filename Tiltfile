#
# Thin Tilt loop for normal controller development.
#
# Assumptions:
# - the dev cluster is the standard e2e k3d context
# - install mode is config-dir
# - Task owns cluster/bootstrap work; Tilt owns the live loop
#
# First bootstrap:
#   task prepare-e2e
# Then:
#   tilt up

ctx = os.getenv('CTX', 'k3d-gitops-reverser-test-e2e')

allow_k8s_contexts(ctx)
current_context = str(k8s_context())
if current_context != ctx:
    fail(
        'Tilt expects kube context "%s" but current context is "%s". Bootstrap once with `task prepare-e2e`, then rerun `tilt up`.'
        % (ctx, current_context),
    )

update_settings(
    max_parallel_updates=1,
    k8s_upsert_timeout_secs=180,
)

local_resource(
    'prepare-e2e',
    cmd='task prepare-e2e INSTALL_MODE=config-dir',
    trigger_mode=TRIGGER_MODE_MANUAL,
    auto_init=True,
    labels=['setup'],
)

local_resource(
    'controller-update',
    cmd='task --force _controller-image-id INSTALL_MODE=config-dir && task --force _image-loaded INSTALL_MODE=config-dir && task --force _controller-deployed INSTALL_MODE=config-dir',
    deps=[
        'api',
        'cmd',
        'internal',
        'go.mod',
        'go.sum',
        'Dockerfile',
        'hack/boilerplate.go.txt',
    ],
    ignore=[
        'api/**/*_test.go',
        'api/**/zz_generated.deepcopy.go',
        'cmd/**/*_test.go',
        'cmd/**/zz_generated.deepcopy.go',
        'internal/**/*_test.go',
        'internal/**/zz_generated.deepcopy.go',
    ],
    trigger_mode=TRIGGER_MODE_AUTO,
    auto_init=False,
    resource_deps=['prepare-e2e'],
    labels=['controller'],
)

k8s_yaml(kustomize('config'))

k8s_resource(
    'gitops-reverser',
    resource_deps=['prepare-e2e'],
    port_forwards=[
        port_forward(8443, name='metrics'),
        port_forward(9443, name='webhook-admission'),
        port_forward(9444, name='webhook-audit'),
    ],
    labels=['controller'],
)

test_targets = [
    'test-e2e',
    'test-e2e-full',
    'test-e2e-signing',
    'test-e2e-manager',
    'test-e2e-audit-redis',
    'test-image-refresh',
]
for name in test_targets:
    local_resource(
        name,
        cmd='task ' + name + ' INSTALL_MODE=config-dir',
        trigger_mode=TRIGGER_MODE_MANUAL,
        auto_init=False,
        resource_deps=['gitops-reverser'],
        labels=['tests'],
    )

local_resource(
    'clean-port-forwards',
    cmd='task clean-port-forwards',
    trigger_mode=TRIGGER_MODE_MANUAL,
    auto_init=False,
    labels=['ops'],
)

local_resource(
    'create-random-configmap',
    cmd="""bash -ceu '
ctx="%s"
ns="${TILT_CONFIGMAP_NAMESPACE:-}"
if [ -z "$ns" ]; then
  ns="$(kubectl --context "$ctx" get watchrules -A -o jsonpath="{range .items[*]}{.metadata.namespace}{\"\\t\"}{.spec.rules[*].resources}{\"\\n\"}{end}" 2>/dev/null | awk '\''$0 ~ /configmaps|\\*/ { print $1; exit }'\'')"
fi
if [ -z "$ns" ]; then
  ns="default"
  echo "No WatchRule mentioning configmaps found; falling back to namespace: $ns"
fi
name="tilt-smoke-$(date +%%s)-$RANDOM"
value="tilt-$(date -u +%%Y%%m%%dT%%H%%M%%SZ)-$RANDOM"
kubectl --context "$ctx" create namespace "$ns" --dry-run=client -o yaml | kubectl --context "$ctx" apply -f - >/dev/null
kubectl --context "$ctx" -n "$ns" create configmap "$name" \
  --from-literal=key="$value" \
  --from-literal=createdBy=tilt \
  --from-literal=createdAt="$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)"
echo "Created ConfigMap: ${ns}/${name}"
echo "Value: ${value}"
'""" % ctx,
    trigger_mode=TRIGGER_MODE_MANUAL,
    auto_init=False,
    resource_deps=['gitops-reverser'],
    labels=['ops'],
)
