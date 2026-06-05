I have a 'dream' on the initial GitTarget reconcile. It consists out of a few things:

* I believe that we should have a more fixed GKV GKR lookup table per GitTarget, off course it should change, but it should be a consise step.
* I really would like to split the reconcile into every bound GKV, so if we have 5 gkvs that we are tracking: we will have have 5 reconcile actions. One for every gkv. Once a reconicle is done the tracking is starting: that helps in multiple situations. 
    * We can do them one by one, having one reconile commit per type
    * We can immediatly start tracking changes once the initial type is done
    * If new types are added or removed from the GitTarget GKV table then we reconcile that sepeate, without needing a completely new reconicle
    * We might be able to even do one 'reconcile' on the kubernetes side (one LIST or InitialSendEvents) and 'stream' that to all GitTargets that need it.

There is also a technical improvement in the Kubernetes API that makes this more approachale: within a resource type you can now trust the RV to be increasing over time. This is really nice since this also would give the option to even pickup events from during the resync. Especially for longer syncs (bigger sets of resources) that would be a very nice property. I would look at it as merging two datastreams: the initial send (the fake 'added' events from the watch) and when thats finished, picking up the first event that has an higher RV. (you can pick up the earlier events but you should drop them off course).

I see some consequenes:
* More than one commit (which is fine and even more readable I guess?) And perhaps we can also use the normal mechanism to get quick reconciles into one (that we also do for multiple events).
* We should be carefull now with multiple commits / writers (I guess that it still should go on the branchworkers queue so that it all is in order)
* I'm now sketing a scenario with bigger amounts of resources: we might want to create a good e2e test for this as well, and then als add some metrics on that specific situation.
* This gives a bit of breathing room for wobbly types in the Kubernetes API that are 'not reachable' or whatever: we now can just sync the types that are stable. And only have troubles with types that are not stable (which also is understandable and not the fault of gitops-reverser)
* Perhaps: we can even drop the whole hash thing? Since we now can trust the RV to be correct? I guess that all that hashing of content is not really good for our CPU usage... Also the hashing for the whole group etc. I would hope that we can drop some complexity because of this.

Visibility:
* I would love it to have more overview of the actual data inside a GitTarget: which resource types do we exactly follow. Which exact CRD version is behind something, etc. It would be very good to have some API for that, not sure if we could push it to the status since it could become to big?
* I would love to see the sync state and perhaps even a total synced counter per type. Not sure here: it's a lot of data, but it's also a lot of 'darkness' now for a new user if we don't provide anything.
* Off course metrics are also a good place to start for anyone curious: and most admins/devs do that these days as well


