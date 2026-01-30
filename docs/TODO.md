[ ] **QUEUE COLLISION ISSUE**: Currently, each GitRepoConfig gets its own queue (keyed by namespace/name). If two different GitRepoConfigs point to the same Git repository URL, they will have separate queues and may cause write collisions when pushing simultaneously. Consider:
    - Option 1: Use repository URL as queue key (requires fetching GitRepoConfig on dispatch)
    - Option 2: Add validation webhook to prevent multiple GitRepoConfigs with same repo URL
    - Option 3: Document as known limitation and recommend using single GitRepoConfig per repo
    - See `internal/git/worker.go:dispatchEvent()` for current implementation

[ ] Combine edits of the same person in the same minute (make that configurable): it doesnt make sense to have lot's of commits for one action. This is a hard one to get right, when does this stop? After x actions or x seconds of inactivity. Or if two persons change something in the same resource, that shouls also be immediatly be comitted. Can you check that effeciently on every incomming event?
[ ] See if we can get more out of: https://github.com/RichardoC/kube-audit-rest?tab=readme-ov-file#known-limitations-and-warnings (since it's maintained and gives some exampels on how to maintain such an open tool).

---

New questions:

* If the gitops-reverser starts: it should itterate all kubernetes objects to see if files need to be adjusted/deleted. It should take ownership of a context/namespace/folder whatever: it could have missed changes. The cluster dictates/writes the source of truth: at this moment configbutler can't do syncing in two ways (would it become an option someday? What if we just hooked up flux or argocd? -> it would need to detect that the file already is there in the exact state, or almost exact state, the ethernal syncing loop would be stopped then).
* Perhaps we should have a owner file in the root of that context/ns/folder -> the current pod name that is leading, the last change etc. -> it would be an unimportant file for the user, but would allow us to prevent two pods from fighting over the state.
* If the AccessPolicy is adjusted on the GitRepoConfig, are the existing watchrules also re-evaluated (if they can send in events).
* Is there to much code duplication between clusterwatchrule and watchrule?
* Add a default business rule that Config resources are not written to disk: these should never be in git. Have an example on the frontpage on how to use sealedSecrets for now: that's a nice start and will just make sure that it's safe (perhaps something better later). We could add an exception as a commandline flag: people that want to do bad should not be blocked in doing so. :-)
* Improve README.m
  * Better explaination of configuration of this tool: one GitRepoConfig per repo, security considerations (namespace or non namespace etc), storeRawConfigmaps (default false).
* There is no time in the admission request: we should add the time received as soon as possible and also put that as commit time (if we can override that).
* Would it be nice to add a [on-behalf-of](https://docs.github.com/en/pull-requests/committing-changes-to-your-project/creating-and-editing-commits/creating-a-commit-on-behalf-of-an-organization) notice in the commit message? That would also implicate that we can deduce this from the admission webhook.
* Can we be more carefull with for example comments? If we edit a file in git? Also keep the same ordering? That is going to be a hard, so not for now.


---

* Can we create the race condition if we have the same GitDestionation in a test: there is a very small chance that we run into that despite the validatingwebhook. Apprently we also should be checking the current status for it according to best prqctices
* Also implement the check on repo level: it's very annoying if two git objects are pushing to the same repo (I think that I had troubles with that today).
  * How can I now if it's the same repo? Only be adding a file? There should be a lock of some kind.
* I would like better metrics and a visual of the current queues / how full they are. Also more tests on high load.
* Improve seeding as written here: [](docs/seed-removal-and-reconciler-triggers-plan.md), potenially also send the hash in the event so that we don't need to do it on multiple places
* The clenup of gitdestination in memory is not really working as expected (I still logs in the end2end test end).
* https://www.bestpractices.dev/en
  * Analyse better what porch was doing, and mention it as alternative: 

---

Recent tests shown:

* It's really hard to get the username for the rescaling: I can't get it to work properly on k3s -> I had to removed the advancd queue stuff, since it's useless if multiple calls are done anyway.
  * So it's really time to replace the mechanisms
* The gitdestination is not handling the first sync very well: I still notice an error the first time new events have to be pushed (tehnically it's designed to work and not loose things, but it would be nicer to do the right init).
* We need a nice way to filder the configmap that k8s creates in every namespace: the public key thingy. Or should we meak the neat functonality better for this? Detect specials that are not cluster independent.
* Think about a mechanism to influence the folder where the resources are written; also support multiple resources per file?
* Create a single commit for the first reconcile: e.g. when the repo has been disconnected from realiy for a while.
