In deploy-controller.sh -> shoudl we also enfroce image pull policy always -> otherwise it's never going to be the right version


    Actually, Always would break local k3d — it forces the kubelet to contact a registry, and gitops-reverser:e2e-local doesn't exist in any registry. You'd get ImagePullBackOff on every rollout restart.

The current IfNotPresent is already correct for the rollout restart approach:

load-image.sh imports new content into containerd under gitops-reverser:e2e-local (replacing old content)
rollout restart kills the old pod, schedules a new one
New pod starts: kubelet checks "is gitops-reverser:e2e-local in containerd?" → yes → uses it
Containerd only has one version of that tag — the one just imported — so the new pod gets the new content
Always would only help if Kubernetes needed to re-pull from a registry to pick up new content. With load-image.sh having already replaced the image in containerd, the re-pull step isn't needed — and would fail since there's no registry to pull from.

Never is another valid option (explicitly says "use what's in containerd, never pull"), but IfNotPresent is fine since the tag is always present after the load step.

So no change needed here — the existing IfNotPresent is the right policy for this flow.


We should really have the endpoint to validate the build date and commit sha (not sure yet but it proibabbly helps somehow
)

Another idea from kubecon
https://www.youtube.com/watch?v=2_nbkiI_UJU

Forgejo perhaps has beter API management? To solve the problems with git repo creation etc?
https://github.com/trailofbits/skills/tree/main/plugins/devcontainer-setup/skills/devcontainer-setup
https://github.com/afonsograca/devcontainers-best-practices/blob/HEAD/skills/devcontainers-best-practices/references/vscode-containers.md
https://code.visualstudio.com/remote/advancedcontainers/add-nonroot-user
    Oef get it to work inside github again?!


---

* Validate e2e with actual git commands: we have this thing locally checked out, so it should be relivtelvy easy to do that.
* Would also be very cool to create a test that 'builds' the trust with the real ssh-keygen commands -> instead of letting it create it's own key (both paths should be validated!).
* Would also be very interesting to think about these kind of combined things as tests that also could run in parallel if you wanted to. It's way to slow now!
* We also should be more carefull with full cluster deletes: that's taking way to much valuable time away for iterating (it just can't be above the 5 minutes if you throw enough processing power at it!)
