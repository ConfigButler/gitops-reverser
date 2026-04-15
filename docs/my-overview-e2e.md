The goal of an e2e test is to get a reliabele 'program' to validate if the software is behaving like we expect.

I'm now learning how I want all high quality components to work together. What I currently have in my stack:
* Ginkgo: the e2e framework that allows you to organise the test code in tdd style
    * Also should help in deciding how I want to run my tests, and also how I want to run them in parallel
    * I just learned that the support from vscode is not really big, and I learned that I should have installed my ginkgo cli tool long time ago. Since it's the way to get a beter insight in how things work, and also is the only way to run the tests in parallel.
    * I also learned that I'm not using half of the things they all could be supporting
    * I also learned that my setup and tear down probably requires some real work
* Gomega: the library to write easy assertions
* Task: replaces my Makefile by yaml files, and reads a lot easier in 'reusing' steps and defining the dependecies. We want to re-use certain things (like kubernetes cluster step) so that we can become much quicker.
    * Is starting to look pretty good at this moment.
    * I already feel that this the solid move to make for now, it's way easier to reason and also is running by default in parallel.
    * I do feel that some of my steps/script actually have outgrown what it really should be
    * I would like to k now if task also have usefull linting for doing weird stuff.

For how it's working now:
* We one _suite_test.go -> Which is now setting up the cluster. In the new 'spirit' of all what I learned this should be it. We do want the other files to be ran indepdent. This should be possible, it might be hard and interesting, but it should work. Creating the git repo on the _suite_test.go is therefore not what we should do anymore. It's really something to do within the actual test. If the test needs it. Most tests do, but we also have a test now that is testing if task is behaving like it should.

I actually would like every test file to run it's own set of simple tests: it should have a clear goal. And we should be able to run the whole thing in parallel. So having multiple git repos, and handling the events in multiple paths. Let's get in that direciton.

My current guess is that having an ordered container with specs is not really wrong: but it's not a guarantee that nothing else is running. I'm also in doubt if it should matter how you actually install the tool.

Also adding a readme that I created with chatgtp:

A spec is one independently runnable example. In practice, a spec has one subject node such as It or Specify, plus zero or more setup nodes that apply to it. Specify is just an alias for It; there is no semantic difference. By default Ginkgo wants specs to be independent, which is why it cares so much about setup and fresh state.

A container node such as Describe, Context, or When is mostly for structure and language. Conceptually, yes, those are the same kind of thing: they organize and label the tree so the resulting spec names read well. They are not where assertions belong, because container closures run during tree construction, not during spec execution. The usual rule is: declare variables in containers, initialize them in setup nodes.

A setup node such as BeforeEach, AfterEach, or JustBeforeEach runs around specs. BeforeEach is for preparing fresh state per spec. JustBeforeEach is the “do the actual creation last” hook: it lets you configure state in one or more BeforeEach blocks and then perform the final action right before the subject runs. The docs describe it as decoupling configuration from creation. It is powerful, but easy to overuse.

By is a bit different from all of those. It is not a node in the tree and it does not create sub-specs. It is just narrative markup for long workflows. Also, it is not limited to subject nodes — the docs show it inside BeforeEach as well as inside It. So your instinct was close, but the more precise rule is: use By inside executing code paths when you want the spec timeline to read like steps. For actual guaranteed ordering between multiple specs, By is not the tool; Ordered containers are.

On BeforeSuite and AfterSuite: yes, they are suite-level setup and cleanup. They must be top-level, cannot be nested in containers, and there can be at most one of each per suite. The docs say it is idiomatic to put them in the bootstrap suite file that ginkgo bootstrap generates, but the key idea is not the filename magic — it is that they are suite-level top-level nodes. Ginkgo’s bootstrap command generates a PACKAGE_suite_test.go file with the normal TestXxx function that calls RunSpecs(...).

One very useful thing you did not mention is assertions in setup nodes. Those are allowed, and the docs explicitly call that a common pattern. So “no assertions in container nodes” is true, but “assertions only in It” is not. Assertions can live in BeforeEach, JustBeforeEach, AfterEach, BeforeSuite, and so on when that is the right place.

Another thing worth knowing is Ordered containers. By default, Ginkgo wants spec independence so it can shuffle order and parallelize. When you truly need a sequence, Ordered is the escape hatch: specs in that container run in the order written and on the same parallel process. That also unlocks BeforeAll and AfterAll, which only exist inside ordered containers. This is the feature to reach for when you really have a staged workflow, not By.

So the short cheat sheet is:

Describe, Context, When = containers that shape the story
BeforeEach, AfterEach, JustBeforeEach = setup/teardown around each spec
It, Specify = the actual spec subject
By = step narration inside running code
BeforeSuite, AfterSuite = whole-suite setup/cleanup
Ordered = real ordering across specs
BeforeAll, AfterAll = only for ordered containers

The mantra I would keep in your head is this:

Containers describe. Setup prepares. Subjects prove. By narrates.
And underneath all of it: declare in containers, initialize in setup nodes. That guideline exists because container code runs once when building the tree, while setup code runs per spec.

A couple of places where people often make mistakes:

Initializing mutable objects directly in a Describe closure instead of a BeforeEach
Using By as if it creates real test structure
Writing dependent specs without Ordered
Stuffing too much logic into nested JustBeforeEach
Treating _suite_test.go as magical instead of as the conventional bootstrap location

Here is the summary I’d use for myself:

Ginkgo builds a tree from container nodes, then runs flattened specs.
Each spec gets one subject node and whatever setup nodes apply to it.
Containers are for structure, not assertions.
Setup nodes create fresh state.
Subject nodes check behavior.
By documents long flows.
Ordered is for real sequence dependence.
Suite hooks live at the top level.

You are not missing much on the vocabulary side. The main thing to internalize now is the tree construction vs run phase distinction. Once that lands, most of the “why is Ginkgo so picky?” rules stop feeling arbitrary.

I can also turn this into a one-page “Ginkgo mental model” note with tiny code examples for each node type.