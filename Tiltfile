#
# Thin Tilt loop for normal controller development.
#
# Assumptions:
# - Tilt bootstraps the shared e2e cluster via a local resource on startup
# - kubectl usage in this file always relies on the active kube context
# - install mode is config-dir
# - Task owns cluster/bootstrap work; Tilt owns the live loop
#
# Start Tilt by running:
#   tilt up

allow_k8s_contexts('k3d-gitops-reverser-test-e2e')

update_settings(
    max_parallel_updates=1,
    k8s_upsert_timeout_secs=180,
)

local_resource(
    'prepare-e2e',
    cmd='task prepare-e2e',
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
    resource_deps=['gitops-reverser'],
    labels=['controller'],
)

k8s_yaml(kustomize('config'))
k8s_yaml(kustomize('test/playground'))

k8s_resource(
    new_name='gitops-reverser-cluster',
    objects=[
        'gitops-reverser:namespace',
        'clusterwatchrules.configbutler.ai:customresourcedefinition',
        'gitproviders.configbutler.ai:customresourcedefinition',
        'gittargets.configbutler.ai:customresourcedefinition',
        'watchrules.configbutler.ai:customresourcedefinition',
        'gitops-reverser:serviceaccount:gitops-reverser',
        'gitops-reverser:clusterrole',
        'gitops-reverser:clusterrolebinding',
        'gitops-reverser-demo-jane-access:clusterrolebinding',
        'gitops-reverser-audit-client-cert:certificate:gitops-reverser',
        'gitops-reverser-audit-root-ca:certificate:gitops-reverser',
        'gitops-reverser-audit-server-cert:certificate:gitops-reverser',
        'gitops-reverser-audit-ca-issuer:issuer:gitops-reverser',
        'gitops-reverser-selfsigned-issuer:issuer:gitops-reverser',
    ],
    resource_deps=['prepare-e2e'],
    labels=['setup'],
)

k8s_resource(
    'gitops-reverser',
    resource_deps=['gitops-reverser-cluster'],
    port_forwards=[
        port_forward(8443, name='metrics'),
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
    'playground-bootstrap',
    cmd='task playground-bootstrap',
    trigger_mode=TRIGGER_MODE_MANUAL,
    auto_init=True,
    resource_deps=['gitops-reverser'],
    labels=['playground'],
)

k8s_resource(
    new_name='playground',
    objects=[
        'playground-provider:gitprovider:tilt-playground',
        'playground-target:gittarget:tilt-playground',
        'playground-watchrule:watchrule:tilt-playground',
        'playground-sample-config:configmap:tilt-playground',
    ],
    resource_deps=['playground-bootstrap'],
    labels=['playground'],
)

local_resource(
    'playground-status',
    cmd='task playground-status',
    trigger_mode=TRIGGER_MODE_MANUAL,
    auto_init=False,
    resource_deps=['playground'],
    labels=['playground'],
)

local_resource(
    'playground-cleanup',
    cmd='task playground-cleanup',
    trigger_mode=TRIGGER_MODE_MANUAL,
    auto_init=False,
    labels=['playground'],
)

local_resource(
    'playground-upsert-configmap',
    cmd='task playground-upsert-configmap',
    trigger_mode=TRIGGER_MODE_MANUAL,
    auto_init=False,
    resource_deps=['playground'],
    labels=['playground'],
)

local_resource(
    'playground-delete-configmap',
    cmd='task playground-delete-configmap',
    trigger_mode=TRIGGER_MODE_MANUAL,
    auto_init=False,
    resource_deps=['playground'],
    labels=['playground'],
)

local_resource(
    'playground-upsert-secret',
    cmd='task playground-upsert-secret',
    trigger_mode=TRIGGER_MODE_MANUAL,
    auto_init=False,
    resource_deps=['playground'],
    labels=['playground'],
)

local_resource(
    'playground-delete-secret',
    cmd='task playground-delete-secret',
    trigger_mode=TRIGGER_MODE_MANUAL,
    auto_init=False,
    resource_deps=['playground'],
    labels=['playground'],
)

local_resource(
    'create-random-configmap',
    cmd="""bash -ceu '
ns="${TILT_CONFIGMAP_NAMESPACE:-}"
if [ -z "$ns" ]; then
  ns="$(kubectl get watchrules -A -o jsonpath="{range .items[*]}{.metadata.namespace}{\"\\t\"}{.spec.rules[*].resources}{\"\\n\"}{end}" 2>/dev/null | awk '\''$0 ~ /configmaps|\\*/ { print $1; exit }'\'')"
fi
if [ -z "$ns" ]; then
  ns="default"
  echo "No WatchRule mentioning configmaps found; falling back to namespace: $ns"
fi
name="tilt-smoke-$(date +%%s)-$RANDOM"
value="tilt-$(date -u +%%Y%%m%%dT%%H%%M%%SZ)-$RANDOM"
kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "$ns" create configmap "$name" \
  --from-literal=key="$value" \
  --from-literal=createdBy=tilt \
  --from-literal=createdAt="$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)"
echo "Created ConfigMap: ${ns}/${name}"
echo "Value: ${value}"
'""",
    trigger_mode=TRIGGER_MODE_MANUAL,
    auto_init=False,
    resource_deps=['gitops-reverser'],
    labels=['ops', 'playground'],
)
