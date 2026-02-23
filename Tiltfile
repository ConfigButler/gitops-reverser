# Tilt inner loop for gitops-reverser on Kind (DOOD-friendly).
# This keeps the current Makefile flow and automates rebuild/load/restart.

kind_cluster = os.getenv('KIND_CLUSTER', 'gitops-reverser-test-e2e')
expected_context = 'kind-' + kind_cluster

allow_k8s_contexts(expected_context)

current_context = str(k8s_context())
if current_context != expected_context:
    fail(
        'Tilt expects kube context "%s", but current context is "%s". Set KIND_CLUSTER or switch context before running tilt.'
        % (expected_context, current_context),
    )

update_settings(max_parallel_updates=1)

docker_build(
    'gitops-reverser:e2e-local',
    '.',
    dockerfile='Dockerfile',
)

local_resource(
    'cluster-prereqs',
    cmd='KIND_CLUSTER=%s make setup-cluster cleanup-webhook setup-e2e check-cert-manager' % kind_cluster,
    trigger_mode=TRIGGER_MODE_MANUAL,
    auto_init=True,
)

k8s_yaml(kustomize('config'))

k8s_resource(
    'gitops-reverser',
    resource_deps=['cluster-prereqs'],
    port_forwards=[8443, 9443, 9444],
)

local_resource(
    'port-forwards',
    cmd='bash test/e2e/scripts/setup-port-forwards.sh',
    trigger_mode=TRIGGER_MODE_MANUAL,
    resource_deps=['cluster-prereqs'],
)

local_resource(
    'quickstart-smoke',
    cmd='KIND_CLUSTER=%s PROJECT_IMAGE=gitops-reverser:e2e-local make test-e2e-quickstart-helm' % kind_cluster,
    trigger_mode=TRIGGER_MODE_MANUAL,
    resource_deps=['gitops-reverser'],
)
