# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
make build                 # go build -o bin/podsmedic ./cmd/podsmedic
make test                  # go test ./...
make vet
make fmt                   # gofmt -w ./cmd ./internal
make run                   # PODSMEDIC_DRY_RUN=true go run ./cmd/podsmedic (current kubectl context, stdout sink)
make docker / make deploy

go test ./internal/heal -run TestValidateRejectsShrink -v   # single test
go test ./internal/detect -v                                # single package
```

Go 1.26, installed at `/usr/local/go/bin` (not on the default `PATH` — export it first).
Every test is a pure unit test — no cluster, no network, no `testdata`. `go test ./...`
runs offline in seconds; if a change makes that untrue, the change is in the wrong package.

Running locally needs an API key (`ANTHROPIC_API_KEY`, or `DEEPSEEK_API_KEY`/`PODSMEDIC_API_KEY`
for the OpenAI-compatible path) and a working kubeconfig. `.env.example` documents every setting.

## Architecture

One poll loop, one sweep per `PODSMEDIC_INTERVAL`:

```
ListPods → detect → predict → incident.Observe → Collect evidence (+ cluster capacity,
           workload load) → playbook? / LLM → heal.Validate → Execute → verify/rollback → notify

Telegram inbound (optional) → chat.Bot authorises → agent.Answer → local state or llm.Answer
```

`internal/agent/agent.go` is the **only** orchestrator and the only place where cluster I/O, the
LLM, and state persistence meet. It holds nearly all the sequencing and has no tests by design:
every rule worth asserting lives in a pure package it calls (`detect`, `heal.Validate`,
`capacity`, `incident`, `breaker`, `playbook`, `predict`, `dedupe`, `chat` policy). When adding behaviour, put the decision in
a pure function with a table test and let `agent.go` do only the wiring.

### Two things bound a heal, and they are different

`heal.Validate` bounds *policy* (may we?), `internal/capacity` bounds *physics* (will it fit?).
Both are pure and both must stay that way. Capacity is gathered once per sweep by
`k8s.ClusterCapacity`, attached to the bundle by `agent.enrich`, and read by the validator —
the validator itself never calls the cluster.

The capacity rules that are load-bearing and easy to break by "simplifying": headroom is
bin-packed **per node** (a pod fits on one node, so cluster-wide sums overcount); **pod slots**
bound the fit alongside CPU/memory (pod count is what kills a kubelet); a **reserve** is held
back from every node; cordoned/NotReady nodes contribute nothing. A missing snapshot makes
`scale_replicas` **refuse** — do not "helpfully" fall back to a default cap.

An **HPA on the workload forbids `scale_replicas` outright** — checked before anything is
derived, because two controllers writing `spec.replicas` overwrite each other. The index is
built once per sweep (`k8s.ListAutoscalers`) and attached by `agent.enrich`, so the validator
stays pure. It is deliberately fail-*open* where the capacity gate is fail-closed; the reason
is written out on `ListAutoscalers`. Only scaling is blocked — an HPA does not own resource
limits, images, or probes.

Replica counts are **derived**, never taken from the model: `capacity.TargetReplicas` applies
the HPA utilisation formula to measured load, and the model's `replicas` may only lower the
result. A *derived* target that breaches a policy bound is clamped; a *model-proposed* one is
refused. Keep that asymmetry — it is the difference between our arithmetic and untrusted input.

### Storage admits exactly two recoveries

`detect.Kind.Storage()` marks `PVCPending` and `VolumeMountFailed`. `heal.Validate` refuses
every action on them *above* the `AllowedKinds` allowlist except two, so `PODSMEDIC_HEAL_KINDS`
cannot widen the set:

- `restart_workload`, and only when `allClaimsBound(b)` — the storage recovered and the pod is
  holding a stale mount. Touches no storage object.
- `create_pvc`, and only for a claim reported `Missing` — additive, so the worst case is an
  unused volume rather than lost data.

`create_pvc` is the only `create` verb podsmedic has, and its RBAC (in `rbac-autoheal.yaml`) is
create-only on purpose: no update, patch, or delete. Its size/class/access mode come from
`heal.Options`, never from the model — same asymmetry as derived replica counts. It requires a
non-empty `AllowNamespaces` (stricter than everything else, where empty means "all"), refuses
more than one missing claim, and refuses `Replicas > 1` because the access mode is then
ambiguous. It gets no rollback record: undoing a create means deleting a claim that may hold
data. Do not add an editing, resizing, detaching, or deleting storage action.

`PVCPending` replaces `Unschedulable` rather than joining it: both share the scheduler's
`Unschedulable` reason, and the message text is the only signal that separates them.
Evidence lives in `k8s.claims` — the *claim's* events, not the pod's, are where the reason
an unbound volume will not bind actually appears.

### Heal limits come in two scopes, and the wide one is newer

`heal.Validate`, the cooldown (`healSeen`), `breaker.Breaker`, and the playbook are all
**per workload**. `breaker.Surge` and `breaker.Budget` are **cluster-wide**, and exist because
N distinct workloads failing at once passes every per-workload limit — the gap that let a
single node failure patch dozens of controllers in one sweep. Both live on `sweepState` and are
consulted at the top of `maybeHeal`, before the per-workload checks.

`Surge` counts distinct *workloads*, never pods: one Deployment with thirty crashing replicas
must stay one failure. If you add another heal entry point, route it through `maybeHeal` or it
will bypass both brakes.

### The trust boundary

Pod logs reach the model verbatim, so **anything the LLM returns is untrusted input**, including
its proposed `heal.Action`. `heal.Validate` (`internal/heal/validate.go`) is the security boundary:
a pure function, no cluster calls, exhaustively unit-tested in `validate_test.go`. It re-derives a
bounded `Plan` from the current cluster state or refuses with `ErrNoSafeAction`. Its invariants —
values only ever increase, absolute caps, multiplier caps, image repo/registry pinned (tag-only
change), probes only loosened, replicas only scaled up, namespace deny/allow, kind allowlist,
confidence gate — are load-bearing. Never move a check out of `Validate` into `agent.go`, and never
add an action kind without the matching refusal tests.

Execution is separately gated by two switches (`PODSMEDIC_AUTOHEAL`, then `PODSMEDIC_HEAL_APPLY`),
so the middle state is a real server-side dry run. `internal/heal/executor.go` talks to the cluster
through the narrow `heal.Cluster` interface, which is what makes the executor testable with a fake.

### The live view is the only thing that watches

`internal/live` is pure — event model, bounded fan-out stream, and `Transitions`,
which decides what counts as a real pod change. `internal/ui` serves one embedded
HTML file plus SSE. `k8s.WatchPods` is the only informer in the codebase and exists
solely for this: the sweep stays on its interval because healing is deliberately
paced, and only the display is live.

Load-bearing details: `Stream.Publish` must never block (it is called from the sweep
*and* an informer callback — a browser on a sleeping laptop cannot be allowed to
stall either), a nil `*Stream` is a valid no-op so call sites stay unconditional,
and `Transitions` must stay strict about what it emits or the display flickers
constantly. The page fetches nothing external, and there is no Service or Ingress
for the UI port on purpose — it has no authentication.

### Durable state lives in ConfigMaps

Four ConfigMaps in podsmedic's own namespace (`k8s.Namespace()`), all read/written through
`k8s.GetConfigMap`/`PutConfigMap`, all bounded, all reloaded on startup:

| ConfigMap | Package | Holds |
| --- | --- | --- |
| `podsmedic-heal-state` | `heal.ConfigMapStore` | pending heals + prior values, for verify/rollback |
| `podsmedic-audit` | `audit` | append-only trail, oldest dropped past the cap |
| `podsmedic-playbook` | `playbook` | verified remedies, replayed with no LLM call |
| `podsmedic-incident-state` | `incident` (via agent `Snapshot`/`Restore`) | open incidents + pending heal proposals |

A missing ConfigMap is a normal first run, not an error. Writes are non-fatal — an absent RBAC rule
degrades a feature, it must not stop the sweep. Each store exposes `Dirty()`/`ClearDirty()` so a
sweep only writes when something changed.

Circuit-breaker state (`internal/breaker`) and the dedupe caches are deliberately **in-memory** —
they clear on restart. Combined with the in-memory dedupe, this is why podsmedic must run
**exactly one replica**.

### Provider abstraction

`llm.Client` is `Diagnose` (schema-constrained JSON, for the sweep) plus `Answer` (prose, for
chat). `anthropic.go` uses the SDK with `output_config.format` schema
enforcement and a `cache_control`-marked static `systemPrompt`; `openai.go` is `net/http`-only,
asks for `json_object`, spells the schema out in the prompt, and validates shape on parse. The
`systemPrompt` const in `llm.go` must stay byte-identical across calls or Anthropic prompt caching
breaks — per-pod content belongs in the user turn.

## Conventions

- **Dependency stance.** stdlib + `client-go` + `anthropic-sdk-go`, nothing else. The Prometheus
  registry in `internal/metrics/metrics.go` is hand-rolled to avoid `client_golang`; the
  OpenAI-compatible backend and both notifiers are plain `net/http`. Adding a dependency needs a
  real justification.
- **Config.** Every setting is a `PODSMEDIC_*` env var read in `config.Load` via the `envString` /
  `envBool` / `envDuration` / `envList` helpers. A new setting means: a `Config` field, a line in
  `Load`, an entry in `.env.example`, and the README section that describes it. Use
  `envStringAllowEmpty` when empty must mean "disabled" rather than "use the default".
- **Metrics** are package-level vars declared in `internal/metrics/podsmedic.go`; declare there,
  increment at the call site.
- **RBAC.** `deploy/rbac.yaml` is read-only; anything that mutates goes in `deploy/rbac-autoheal.yaml`.
  A new API call means updating one of them, and making the failure path degrade gracefully.
  Verify a change with `kubectl auth can-i <verb> <resource>
  --as=system:serviceaccount:podsmedic:podsmedic` — the boundaries that must stay `no` are
  delete-anything, `get secrets`, `update`/`delete persistentvolumeclaims`, and any write to nodes.
- **Manifests.** `deploy/config.yaml` holds the namespace, ConfigMap, and secret placeholder;
  `deploy/deployment.yaml` holds only the workload. Keep them apart — merging them means an
  image rollout silently resets every tuned setting. `deploy/local-registry.yaml` is optional
  and single-node only.
- **Tests** are table-driven with hand-built `corev1.Pod` / `k8s.Bundle` literals. Collaborators are
  faked against the narrow interfaces (`heal.Cluster`, `heal.ConfigMapAPI`, `audit.Log`), not mocked.
- **Failure handling.** One bad pod must never abort a sweep: log, meter, continue. `heal.Validate`
  declining a proposal is the normal case and is logged at info, not error.
- **The chat path is read-only.** `internal/chat` and `agent.Answer` may read state and call
  `llm.Answer`; they must never reach the healer. `llm.Answer` returns prose with no action
  field, which is what makes that structural rather than a convention. Adding a chat command
  that mutates anything would break the property the feature is documented on.
- **Per-sweep state.** `sweepState` is built before handlers run and never mutated, so the
  concurrent handlers in `handleAll` can read it lock-free. The chat path reads
  `Agent.lastSweep` instead, which *is* mutex-guarded because it crosses sweeps.
- **Evidence redaction.** `k8s.Collect` sends env var *names* only, never values. Keep it that way.

## Extension points

- New detector: add a `detect.Kind` + case in `internal/detect/detect.go`, plus a `detect_test.go` case.
- New heal action: `ActionKind` in `heal/action.go`, a `validateX` in `validate.go` with its caps in
  `heal.Options`, an execute branch in `executor.go`, a `k8s` patch method, and RBAC.
- New sink: implement `notify.Notifier` (Notify, Notice, Check, Name), append in `buildNotifier` in
  `cmd/podsmedic/main.go`.
- New LLM provider: implement `llm.Client` (`Diagnose` + `Answer`), wire into `llm.New`.
- New chat command: a `Command` and `Parse` case in `internal/chat/policy.go` plus a test, a line
  in `chat.HelpText`, then a branch in `agent.Answer` (`internal/agent/ask.go`). Answer from state
  the agent already holds so it costs no tokens, and put any non-trivial formatting in the package
  that owns the data (e.g. `heal.Action.Describe`) so it gets a test. A command that returns a file
  sets `chat.Reply.Document`; the bot uploads it via multipart `sendDocument`.
- New export format: a renderer in `internal/report` plus a `ParseFormat` case. Rendering is a pure
  function of `report.Input` — never call the LLM or the cluster from there, or a study document
  could describe a remedy that was never learned. HTML must stay self-contained (no external
  fetches) so it prints to PDF offline; there is deliberately no PDF library.
