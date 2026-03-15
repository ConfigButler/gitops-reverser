I'm trying to get bi-direction gitops to work. I'm now at an interesting point.

I managed to 'get' to the point where the difference are important.


This is what the test comitted (gitops):
```
apiVersion: shop.example.com/v1
kind: IceCreamOrder
metadata:
  name: bi-alice-order-1773601028969607066
  namespace: gitops-reverser
spec:
  customerName: Alice
  container: Cone
  scoops:
    - flavor: Vanilla
      quantity: 2
  toppings:
    - Sprinkles
```

This is what reverse gitops wanted to change it into (reverse-gitops). Now the interesting problem arrisses that the comparison apperently is not to intelligent since it wants to replace with:
```yaml
apiVersion: shop.example.com/v1
kind: IceCreamOrder
metadata:
  labels:
    kustomize.toolkit.fluxcd.io/name: bi-live-1773601028969607066
    kustomize.toolkit.fluxcd.io/namespace: flux-system
  name: bi-alice-order-1773601028969607066
  namespace: gitops-reverser
spec:
  container: Cone
  customerName: Alice
  scoops:
  - flavor: Vanilla
    quantity: 2
  toppings:
  - Sprinkles
```

So the whitespace (might be!) a problem and we have the labels that are added.

A quick solution that I would like to see is to filter all labels that start with `kustomize.toolkit.fluxcd.io` -> just hardcoded to start with. Can you investigate if that is enough?


So flux is doing a reconicle every x seconds -> even when the gitsource / artifact is stil the old one. Is there a way to prevent that? Can we or should we stop the flux reconicle? Or should we enfforce 'zero' and do it by hand? And optimize for API usage? -> This is hard condition. We now have two loops and we should be carefull who we will define as authoriative.

There is a 'special' in the current way we push from the gitops-reverser -> if a new commit is there we fail and we do it again. It would make a lot of sense to suspend the kustmize controller and to require a refresh when this situion occurs. We also should respond to a webhook, then it's fully predictable again. And then it becomes a choice (and something we will log!) when we have a race condition. Would that be realistic / happening often?
