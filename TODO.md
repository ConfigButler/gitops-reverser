[ ] **QUEUE COLLISION ISSUE**: Currently, each GitRepoConfig gets its own queue (keyed by namespace/name). If two different GitRepoConfigs point to the same Git repository URL, they will have separate queues and may cause write collisions when pushing simultaneously. Consider:
    - Option 1: Use repository URL as queue key (requires fetching GitRepoConfig on dispatch)
    - Option 2: Add validation webhook to prevent multiple GitRepoConfigs with same repo URL
    - Option 3: Document as known limitation and recommend using single GitRepoConfig per repo
    - See `internal/git/worker.go:dispatchEvent()` for current implementation

[ ] Combine edits of the same person in the same minute (make that configurable): it doesnt make sense to have lot's of commits for one action. This is a hard one to get right, when does this stop? After x actions or x seconds of inactivity. Or if two persons change something in the same resource, that shouls also be immediatly be comitted. Can you check that effeciently on every incomming event?
[ ] Do we really need to pull before each commit? That's not what was in my head before we started the whole conversation -> it should do a push/pull once a minute. Or perhaps a pull the first time an event is created? I would like to have a timeline, please let's be carefull with pushes and pulls
[ ] See if we can get more out of: https://github.com/RichardoC/kube-audit-rest?tab=readme-ov-file#known-limitations-and-warnings (since it's maintained and gives some exampels on how to maintain such an open tool).
[ ] Should we also do a full reconicile on the folders? As in: check if all the yaml files are still usefull?
    -> This last line is where it gets interesting: who wins? I guess we just push a new commit and throw away the files that don't exist in the cluster. Should we do a full reconcile every x minutes? How many resources can we handle before it gets tricky?
[ ] Should the repo config be namespaced or clustered? All that duplication is also ugly, how does flux do that part?



For tomorrow I should grow my understanding on the nice step that is decribed here: 
https://golangci-lint.run/docs/welcome/install/#ci-installation
https://github.com/golangci/golangci-lint-action

And I should try to only have the test runner use that slim ci-dev-container.

Are there best practices written down for this? Could I do something on this? It will be very usefull to have a deeper understanding of docker and images if I'm going to want to have my configuration as image succesful at some point.


This is what I had:
      - name: Set up KinD
        uses: helm/kind-action@v1.12.0
        with:
          cluster_name: gitops-reverser-test-e2e
          version: v0.30.0

---

Get the RBAC updates in the helm chart automated
Validate the git credentials every 10 minutes: only do a pull on the existing repo, don't clone the whole thing again
Add tests for CRs"

Somehow the trigger/filtering is not correct at the moment: I need to investigate better why my crd endpointchecker stuff is not safed, and how I would reference the k8s types in the correct way.

There should also be some examples in the unit tests somewhow: I should be able to hunt down some of these things that come in, and why I'm not 'getting' them at this moment (so there is still some human work involved luckily enough).

I'm not to happy with all the sanitize stuff: It's throwing away some metadata fields that could prove valuable as well. Isn't it going to be good enough to just throw away the status field? We don't want things to be to big, but also not be to small...

Will we allow to keep status as well? Is it perhaps usefull for some usecases? It also depends where the API server will run: people could also choose the run this concept in their own cluster. It would be a backup.
