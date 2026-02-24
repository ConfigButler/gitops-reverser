Testing with pre-requisites

I'm intregued how fast you can test and build stuff with AI: but it's also very hard to get the prerequisites right.

How about that we really learn something about Makefiles, Kubernets and combining the two?

A Makefile is a smart dependency tree: one can define for each task which other tasks needs to be done first. There is a special '.phony'. command to always run a task when it's requested. In normal circumstances you would include the actual file names that a task depends on. As long as these files are not touched the step is not ran again. This is very logical for compiles and linkers, which is why this tool has been so popular for years in the C years. 

In the Kubernetes world this is a bit harder: but we could save a little 'state' file per make step. The special cleanup cluster make file step would also remove the other 'state' files.

This will allow us to define better when and how we should be waiting on things: especially the port forwarder task/script is an interesting example: it really does need a running pod. It's hard to guarantee that a pod is running, but we can guarantee that something at least has been defined as deployment (by defining the spec).

golang source files -> create image -> deploy image -> create port forward
                    -> run compiler
                    -> run linter

golang test files   -> run compiler -> run tests

kustomize config manifests -> replace image version -> apply kubectl -f

Now the question is where this breaks: I believe that make will only allows us to work backwards. A change in a golang source file shouldnt/can't trigger a state file change? Or should it? And can we then include that as a dependency as well?

image-state (source files changed, digest?)

Which actions would qualify to move to make:

create-cluster
delete-cluster

setup-gitea
setup-prometheus (run both the operator and prometheus instance)
setup-cert-manager
forward-ports (interesting case: can we also check if they are alive somehow?)
create-age-secret
create-image
deploy-config
deploy-helm

lint
test
test-e2e



----

Testing with prerequisites: using Make for Kubernetes operator dev loops

AI makes it tempting to “just run tests” all the time. But the real speed comes from something more boring: not rerunning steps that don’t need to run.

That’s why Make is still useful. It can turn your build-and-test workflow into a small dependency graph where steps only rerun when their inputs changed.

For Kubernetes operators, this gets tricky because part of your workflow happens in a live cluster. The good news: you can still make it work if you treat “cluster state” as something you prove locally using small stamp files.

What Make actually does

A Makefile is a set of rules. Each rule looks like this:

target: prerequisites
	recipe

target is usually a file you want to create.

prerequisites are files (or other targets) that must be up-to-date first.

recipe is the shell commands to run.

The key behavior:

If the target file doesn’t exist, Make runs the recipe.

If any prerequisite file is newer than the target, Make runs the recipe.

Otherwise, Make skips the recipe.

That’s it. No magic, just file timestamps.

A tiny example (real file target)
build/app: main.c
	cc -o build/app main.c

If main.c didn’t change since last time, build/app is “up-to-date” and Make won’t relink.

.PHONY targets (always run)

Some targets are not files. They’re just convenient names like test or lint. For those you use .PHONY:

.PHONY: test
test:
	go test ./...

This tells Make: “don’t look for a file named test; always run the commands when someone asks for make test.”

The Kubernetes problem

In operator development, a lot of steps don’t naturally create a local file:

“Create a kind cluster”

“Apply CRDs”

“Deploy controller”

“Wait for rollout”

“Port-forward to the service”

Make can’t “see” cluster state. If there’s no file target, it can’t decide what’s up-to-date.

The bridge pattern: stamp files

The trick is to create a small local file that means:

“This step completed successfully for these inputs.”

Example:

.stamps/cluster/kind-myop/ready

.stamps/cluster/kind-myop/crds.applied

.stamps/cluster/kind-myop/operator.applied

.stamps/cluster/kind-myop/e2e.passed

Now each Kubernetes step becomes a normal Make target that produces a normal file. Make can reason about it.

Why stamps work

If you only touch the stamp after a real verification (like kubectl wait), then:

stamp present → step already succeeded

stamp older than inputs → rerun step

Example Makefile: operator dev loop with stamps

Below is a minimal pattern that works well for kind + kustomize-based operators.

1) Define your “phony entrypoint”
.PHONY: e2e
e2e: .stamps/cluster/kind-gitops-reverser/e2e.passed

make e2e doesn’t run tests directly. It asks Make to produce the file e2e.passed. Make then works backwards and builds everything it needs.

That’s how “sub-steps” work in Make: they’re not nested actions, they’re dependencies.

2) Build an image and write a digest file

This is the “digest trick”.

Problem: tags like :dev are mutable. You can’t reliably tell if the image changed.

Solution: write an immutable identifier (digest) to a file.

SOURCES := $(shell find cmd internal pkg -type f -name '*.go') go.mod go.sum
IMG_REPO ?= ghcr.io/you/gitops-reverser
IMG_TAG  ?= dev
IMG      := $(IMG_REPO):$(IMG_TAG)

.stamps/image/controller.digest: $(SOURCES) Dockerfile
	mkdir -p .stamps/image
	docker build -t $(IMG) .
	docker inspect --format='{{index .RepoDigests 0}}' $(IMG) > $@

What this gives you:

If Go code changes → this target reruns → digest file updates.

If nothing changed → digest file is up-to-date → Make skips the build.

And because the digest is immutable, it’s also a perfect input for deployment.

3) Ensure the cluster exists
KIND_CLUSTER ?= gitops-reverser
KUBECONTEXT  ?= kind-$(KIND_CLUSTER)
CLUSTER_STAMPS := .stamps/cluster/$(KUBECONTEXT)

$(CLUSTER_STAMPS)/ready: hack/kind-up.sh
	mkdir -p $(CLUSTER_STAMPS)
	KIND_CLUSTER=$(KIND_CLUSTER) ./hack/kind-up.sh
	kubectl --context $(KUBECONTEXT) get ns >/dev/null
	touch $@

Note the pattern: create cluster, then verify (kubectl get ns), then touch the stamp.

4) Apply CRDs and verify they’re established
$(CLUSTER_STAMPS)/crds.applied: $(CLUSTER_STAMPS)/ready $(shell find config/crd -type f)
	kubectl --context $(KUBECONTEXT) apply -k config/crd
	kubectl --context $(KUBECONTEXT) wait --for=condition=Established crd --all --timeout=120s
	touch $@

If you change any CRD manifest file, Make sees it and reruns this step.

5) Deploy the controller using the digest

This is where the digest trick pays off: deploy by digest, not tag.

$(CLUSTER_STAMPS)/operator.applied: \
  $(CLUSTER_STAMPS)/crds.applied \
  .stamps/image/controller.digest \
  $(shell find config/manager config/rbac config/default -type f)

	IMG_DIGEST=$$(cat .stamps/image/controller.digest) ; \
	kustomize build config/default | \
	  sed "s#$(IMG_REPO):.*#$$IMG_DIGEST#g" | \
	  kubectl --context $(KUBECONTEXT) apply -f -

	kubectl --context $(KUBECONTEXT) -n system rollout status deploy/controller-manager --timeout=180s
	touch $@

Now deployment reruns when:

the image digest changes (code changed)

or manifests changed (RBAC/manager/config)

or CRDs changed (because it depends on crds.applied)

6) Run E2E tests and stamp success
$(CLUSTER_STAMPS)/e2e.passed: $(CLUSTER_STAMPS)/operator.applied test/e2e/*.go
	KUBECONTEXT=$(KUBECONTEXT) go test ./test/e2e -v
	touch $@

Now make e2e is incremental:

Change Go code → image rebuild → redeploy → rerun tests

Change only tests → rerun tests

Change only CRDs → reapply CRDs → redeploy → rerun tests

Change nothing → everything skipped (fast!)

What about port-forward?

Port-forward is a special case because it’s not a stable build artifact. It’s a long-running process.

Two approaches:

Option A: keep it .PHONY (simple)

Always start it when asked:

.PHONY: port-forward
port-forward: $(CLUSTER_STAMPS)/operator.applied
	kubectl --context $(KUBECONTEXT) -n system port-forward deploy/controller-manager 8080:8080
Option B: stamp + PID file (more complex)

Write a PID file and verify the process is still alive before claiming it’s up-to-date. This is doable, but for most teams option A is enough.

Where this approach breaks

Stamps assume the world doesn’t change behind Make’s back. Kubernetes can drift:

someone deletes the cluster manually

the deployment is edited manually

the controller crashes after the stamp was written

kubecontext changes

You can mitigate this in two ways:

Namespace stamps by kubecontext (as shown)

Only write stamps after real verification (kubectl wait, rollout status)

If you need extra safety, add a quick “is it still true?” check before skipping (for example: confirm the deployed image digest matches your digest file)

The main takeaway

Make is not “tasks”. It’s a dependency graph over artifacts.

For Kubernetes operator workflows, you can still use Make effectively if you:

make each cluster step produce a local stamp file

make stamps depend on the real inputs (manifests, tests, scripts)

use an image digest file as the stable bridge between “built code” and “deployed controller”

That gives you fast, predictable inner loops: change one thing, rerun only what truly depends on it.

If you want, paste your current Make targets (image build + kind + deploy + e2e), and I’ll adapt this pattern to your repo structure (operator-sdk layout vs custom, kind vs k3d, kustomize vs helm) without adding more complexity than necessary.

O
I'm very curious to how you would do that PID trick with a port forwarding. What construct would you need to pull that off?

You can do it, but you have to accept one hard truth: Make isn’t a process supervisor. So the “PID trick” is basically: start port-forward in the background, record its PID, and on the next make port-forward check if that PID is still alive (and ideally still the right command). If it’s alive, skip; if not, start again.

There are two workable constructs:

A stamp file + PID file (most common)

A stamp file whose recipe validates and only re-runs if validation fails (cleaner, but you still need the PID)

Below is a solid pattern using a PID file, plus an optional health-check.

Pattern: target file .stamps/.../portforward.running + .stamps/.../portforward.pid
Makefile snippet
# Config
KIND_CLUSTER ?= gitops-reverser
KUBECONTEXT  ?= kind-$(KIND_CLUSTER)
NAMESPACE    ?= system

PF_LOCAL_PORT  ?= 8080
PF_REMOTE_PORT ?= 8080

CLUSTER_STAMPS := .stamps/cluster/$(KUBECONTEXT)
PF_PID   := $(CLUSTER_STAMPS)/portforward.pid
PF_LOG   := $(CLUSTER_STAMPS)/portforward.log

.PHONY: port-forward port-forward-stop
port-forward: $(CLUSTER_STAMPS)/portforward.running

# Start port-forward only if not already running
$(CLUSTER_STAMPS)/portforward.running: $(CLUSTER_STAMPS)/operator.applied
	@mkdir -p $(CLUSTER_STAMPS)
	@set -eu; \
	# If PID file exists and process is alive, consider it running and refresh the stamp.
	if [ -f "$(PF_PID)" ] && kill -0 "$$(cat "$(PF_PID)")" 2>/dev/null; then \
	  echo "port-forward already running (pid=$$(cat "$(PF_PID)"))"; \
	  touch "$@"; \
	  exit 0; \
	fi; \
	# Clean up stale pid file
	rm -f "$(PF_PID)"; \
	echo "starting port-forward on localhost:$(PF_LOCAL_PORT) -> $(PF_REMOTE_PORT)"; \
	# Start in background, capture PID, write logs
	( kubectl --context "$(KUBECONTEXT)" -n "$(NAMESPACE)" \
	    port-forward deploy/controller-manager "$(PF_LOCAL_PORT):$(PF_REMOTE_PORT)" \
	    >"$(PF_LOG)" 2>&1 & echo $$! >"$(PF_PID)" ); \
	# Give it a moment and verify process is alive
	sleep 0.5; \
	kill -0 "$$(cat "$(PF_PID)")" 2>/dev/null || (echo "port-forward failed, see $(PF_LOG)"; exit 1); \
	touch "$@"

# Stop it explicitly
port-forward-stop:
	@set -eu; \
	if [ -f "$(PF_PID)" ]; then \
	  PID="$$(cat "$(PF_PID)")"; \
	  if kill -0 "$$PID" 2>/dev/null; then \
	    echo "stopping port-forward (pid=$$PID)"; \
	    kill "$$PID"; \
	  fi; \
	  rm -f "$(PF_PID)" "$(CLUSTER_STAMPS)/portforward.running"; \
	else \
	  echo "no port-forward pid file found"; \
	fi
What this gives you

make port-forward:

If the PID file exists and the process is alive → do nothing.

Otherwise start a new kubectl port-forward and record PID.

make port-forward-stop kills it and removes files.

Make it safer: verify it’s actually listening on the port

A PID being alive doesn’t guarantee the forward is working (pods can restart, tunnels can break). Add a lightweight check:

Option A: check the local TCP port is open (Linux)

Add this in the “already running” branch:

if [ -f "$(PF_PID)" ] && kill -0 "$(cat "$(PF_PID)")" 2>/dev/null; then
  if ss -ltn | grep -q ":$(PF_LOCAL_PORT) "; then
    echo "port-forward already running and listening"
    touch "$@"
    exit 0
  fi
fi

(You could use lsof -iTCP:PORT -sTCP:LISTEN too, but ss is usually present.)

Option B: HTTP health probe (if your port exposes it)

If your operator exposes /healthz:

if curl -fsS "http://127.0.0.1:$(PF_LOCAL_PORT)/healthz" >/dev/null; then
  touch "$@"
  exit 0
fi

This is the best check if you have it.

Why this works in Make terms

The target is a file artifact: portforward.running.

If it exists and is newer than prerequisites, Make would normally skip.

But we’re also doing an extra runtime validation in the recipe (PID alive / port listening).

If validation fails, we restart and then touch the target.

This is a slight “abuse” of Make (because runtime state isn’t purely captured by mtimes), but it’s practical.

Two gotchas (don’t ignore these)

Killing PID might not kill kubectl’s children in some shells
Usually it does, but if you see orphaned processes, you can start kubectl in its own process group and kill the group. That’s a bit more bash-y.

PID reuse
It’s rare but possible: a PID file could point to a new unrelated process after reboot. That’s why adding a port check or command-line check is good.

If you want command-line verification, you can do:

ps -p "$PID" -o args= | grep -q "kubectl .*port-forward"

If you tell me your environment (Linux/macOS, bash vs sh, kind vs k3d) and what you port-forward to (svc/deploy/pod + which ports), I’ll tailor the snippet to be robust and minimal for your setup.