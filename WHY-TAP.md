# Why TAP

People are asking AI agents to investigate issues, prepare customer reviews,
check releases, and get work done across company systems. Those requests can
involve source control, ticketing, CRM, support, billing, and many other tools.
Different people need the work done for different projects or accounts, and
many of the procedures are repeated across an organization.

We want agents to build their own internal tools for that work, much like
engineers build software for people. Once an agent has worked out a procedure,
it should be able to put the repeatable parts in code and use them again.
The next request supplies new inputs. The agent can spend its reasoning on
the decisions the task needs.

TAP stands for Trusted Agent Primitives. A primitive is a reusable block of
code with defined inputs, outputs, and the tools and access it needs. We expect
agents to write most of these primitives as they build their own tooling.
Humans can write them too. Useful code should be something people and their
agents can inspect, improve, and use again.

## What belongs in a primitive

A primitive can handle a sequence of calls, pass results from one step into
the next, transform data, and use loops or branches where the procedure needs
them. It can be a useful part of a larger task. The aim is to make those parts
reusable when building more complex work.

Consider collecting the facts for a customer account review. A primitive
could resolve the customer's identifiers across CRM, support, billing, and
product systems; retrieve records for a date range; follow pages of tickets;
match related records; and calculate agreed measures. It returns the data,
calculations, gaps, and source references. The agent can then interpret what
those facts mean and prepare the review. Another account gives the code new
inputs without changing that procedure.

Options belong in the interface when they have a clear purpose. A defined
choice can select a branch in the code. Missing inputs and failures should
be reported so the calling agent can decide how to continue.

Where a procedure needs reasoning, we want that step to be targeted: a
specific decision with defined inputs and outputs. The parts whose logic is
already known can stay in code.

## Using existing tools

A primitive uses the tool connections supplied by the host it runs through.
The package describes the access it needs and carries the code to execute.
Connected-tool credentials stay with the client's connections. An explicitly
configured direct MCP backend instead uses the owner's local header file.
This lets agents create internal tools around the systems they already use.

That access needs to be inspectable. TAP Runtime runs code in a WebAssembly
sandbox and checks requests against the package's declarations. Supported
clients provide approval paths for changes; changes without authorization
are refused. The runner records requests and outcomes. The
[compatibility table](README.md#where-it-runs) explains which clients support
connected-tool execution and approval.

## Keeping useful work

Models, tools, and systems change. We want the useful code agents create to
last through those changes. Defined interfaces and modular procedures give
people and agents something they can test, update, and build on. When a tool
changes, the affected code can be revised and checked.

We also want people to retain control over what they build. TAP Runtime is
open source under the MIT license and works without a Telara account, registry,
or service. A primitive's portability depends on a compatible host and the
tools it requires. Its requirements should be explicit enough to understand
what needs to be provided in another environment.

## Working across a team

We built TAP at [Telara](https://telara.dev/), where we're working on security,
sovereignty, and longevity for AI systems. Reusable code helps with that work:
it gives people something concrete to inspect and maintain, with declared
access and an interface that can be used across compatible systems.

Telara is the enterprise AI operating layer. It provides company context to
connected AI clients, applies the organization's identity, scope, and policy
to mediated actions, and retains the record of that work. We're extending
the platform with verification, versioning, admin approval, and distribution
for TAP primitives. A useful tool created by one person's agent should be
something colleagues' agents can use under company policy.

If you want to build a primitive, start with the
[authoring guide](docs/writing-a-primitive.md). If your team needs to review
and share this work across company systems, [take a look at Telara](https://telara.dev/).
