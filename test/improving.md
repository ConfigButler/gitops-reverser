I've learned a lot on the Makefile and running tests: the Tiltfile really got me into this direction and now it makes me wonder how I should 'thank it'.

At this moment I have 3 seperate e2e tests that all create their own cluster.

I think / feel that they could be combined, and even could be ran in parallel.

I find it really important to test my quickstart steps: to show to people that it just works.

But the basics are the same, and they should be able to handle multiple gitops-reversers at the same time:

* Kind cluster
* Gitea (one can have different repos!)
* Prometheus (one can spin up different prometheus instances, or just apply different labels, altough it does become a bit more fuzzy to get that right)
* One namespace with a unqiue name per testrun.
* One Makefile target that would allow us to run the three e2e tests at the same time (in parallel where possible).

What would be needed to get that right? We need to continue our quest for removing repitiont: getting back the essence, properly defining what is really dependent on what.

The first next step used to be analysing `test/e2e/scripts/run-quickstart.sh`, but quickstart smoke assertions now live
in Go (`test/e2e/quickstart_framework_e2e_test.go`) and the shell harness is retired.

It would be really cool to create a new namespace: 
"run-e2e-full-{same-number-as-repo-basedupon-time}"
"run-e2e-quickstart-manifests-{same-number-as-repo-basedupon-time}"
"run-e2e-quickstart-helm-{same-number-as-repo-basedupon-time}"

I'm also wondering if the tests for the quickstart shouldnt be in a go e2e_test.go like file -> in the end we are just doing the same type of assertions.

It would be really awesome if we could redo all tests in the proper way.
