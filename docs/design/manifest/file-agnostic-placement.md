The whole cool thing of KRM and the Kubernetes API is that the exact content of the YAML file contains everything needed to reflect the actual resource inside the Kuberntes API. The actual location of the file doesnt matter.

At this moment gitops-reverser has a fixed convention on where it expects/places files. Obviously the placement of new files is something that always needs some rule (either hardcoded or fixed). But why would it be important to start workin? For normal GitOps application like ArgoCD or Flux it's also not important (ok, if configured well offcourse).

There is some interesting problems to this:
* It's normal/allowed in YAML files to place multiple resources in one file, the `/n---/n` trick is used as seperator.
* For some resources it would be very beneficial to NOT write the namespace, actually all my examples until now have the problem that it's not entirly logical to also place the namespace. It might be a nice option on a GitTarget to drop it.
* Most examples also would have benefitted from having a simple folder based set of 'bootstrap' files. I already do this for the .sops.yaml file, but why not for a simple kustomization.yaml? Which allows to easily hook it up to normal GitOps tools and deploy it into a different namespace?
* It would be very logical for people to hook up a cloud version of this to an existing Gitrepo: I will never be able to support everyhing but to be so strict in where files are to placed is madness, then it will certainly not work.

Requirements:
* Parse an existing folder and parse all yaml/KRM that can be found in it. 
    * Have a notion / index of this (can be in memory) so that I can write back changes or updates at the right place.
* Recognize a kustomization / the current structure and be able to not write the namespace.
* Be able to (or even require GitOps tooling!) to stream the initial set of yamls into some location so that we can start looking for changes. Not sure how to cope with this one, it's again a form if bidirectional GitOps.
* Detect helm shizzle and ignore it with a good error message.
* Detect Kustomize and only support the very basic constructs in both directions (what would that exactly look like).

The dream would to be able to point gitops-reverser (potenially combined with flux for example) at a folder and to be able to provide a GitOps API for it without even thinking. People would have insigt in which objects are detected, they can edit them and a pull request is automatically created out of it.

Boundries:
* I don't believe that gitops-reverser should get knowledge on things like GitHub (so creating that PR is a respnsiblity for another layer, but pushing changes to a branch is fine).
* We really shouldnt get into the details of kustomize or helm to soon: we can also start with a clean folder of yaml manifests.

Follow-up investigation:
* [contextual-namespace-and-kustomize-folder-editing.md](contextual-namespace-and-kustomize-folder-editing.md)
