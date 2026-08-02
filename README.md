# podsmedic

An SRE agent that watches a Kubernetes cluster for failing pods, gathers the evidence a human
would (`describe`, events, logs, node capacity), asks Claude what went wrong, and posts the
answer to Slack or Telegram in plain language — and answers your questions back, if you
[let it listen](#ask-it-questions-telegram):

> 🔴 **Pod web-7d9f OOMKilled: memory limit too small for workload**
> `api/web-7d9f` container `web` · OOMKilled · confidence high
>
> The container is killed with exit code 137 roughly every 4 minutes because its 128Mi memory
> limit is below the ~210Mi the JVM heap settles at under load. Raise the limit to 256Mi.

## How it works

```
list pods ─▶ detect ─▶ dedupe ─▶ collect evidence ─▶ LLM ─▶ auto-heal? ─▶ Slack / Telegram
  (poll)     (OOM,      (cooldown  (describe+events    (diagnosis +   (validate +
             crashloop)  per fault)  +logs+node       proposal)      capacity gate +
                                     +cluster capacity)               bounded fix)

Telegram ──▶ authorise ─▶ /command or question ─▶ answer      (read-only: never heals)
 (inbound)   (allowlist)   (local state or LLM)
```

| Package | Responsibility |
| --- | --- |
| `internal/detect` | Classifies pod state into concrete problems. No API calls, fully unit-tested. |
| `internal/dedupe` | Suppresses repeat alerts for the same fault within a cooldown window. |
| `internal/k8s` | Lists pods, builds the evidence bundle, and (for auto-heal) resolves owning controllers and patches them. |
| `internal/llm` | Returns a structured diagnosis + a proposed remediation. Two backends behind one `Diagnoser` interface: native Claude Messages API, and any OpenAI-compatible endpoint (DeepSeek, etc.). |
| `internal/heal` | Validates the LLM's proposed action against hard safety caps, executes the bounded result, and persists applied heals so they can be verified and rolled back if they don't hold. The trust boundary for anything that mutates the cluster. |
| `internal/notify` | Delivers to Slack, Telegram, or stdout. |
| `internal/incident` | Correlates a workload's many symptoms into one incident, so one failure is one alert. |
| `internal/audit` | Durable, bounded trail of every heal/verify/rollback with before/after values. |
| `internal/breaker` | Per-workload circuit breaker that suspends healing a workload whose heals keep failing. |
| `internal/playbook` | Memory of verified heals, replayed on recurrence with no LLM diagnosis. |
| `internal/predict` | Flags containers trending toward an OOM from live usage, to heal before the kill. |
| `internal/capacity` | Cluster headroom arithmetic: how many more pods fit, and how many replicas the measured load calls for. Pure, so the bounds a heal obeys are unit-tested. |
| `internal/chat` | The inbound half of Telegram: long-polls for operator questions, authorises them, and replies. |
| `internal/agent` | The poll loop tying the stages together. |
| `internal/metrics` | Dependency-free Prometheus registry and the `/healthz` `/readyz` `/metrics` server. |

### What gets detected

`OOMKilled`, `CrashLoopBackOff`, `ImagePullBackOff` / `ErrImagePull`, `CreateContainerConfigError`,
non-zero container exits (including init containers), `Unschedulable`, `Evicted`, restart storms
above a threshold, pods stuck not-ready past a grace period, and two storage faults —
`PVCPending` and `VolumeMountFailed` (see [Storage faults](#storage-faults)). With prediction enabled it also
raises `MemoryPressure` and `CPUPressure` — forward-looking signals, not current failures — for a
container whose live memory or CPU sits near its limit (see [Predictive heal](#predictive-heal)).

Two rules keep stale history out of the alert stream, and both matter more than they sound:

- A past termination (`lastState`) only counts while the container is **still unhealthy**.
  A container that crashed once and recovered is not an incident.
- A restart storm requires the most recent restart to fall inside `PODSMEDIC_RESTART_WINDOW`
  (default 1h). `restartCount` is cumulative over a pod's entire life, so without the window a
  pod that flapped a month ago alerts forever.

On a real 29-pod cluster these two rules took a first pass from 13 alerts (all stale) to 0.

### What gets sent to Claude

A JSON bundle per problem: trimmed pod description (spec + status, resources, probes, conditions,
owner), up to 25 recent events newest-first, the previous instance's log tail (that is where a
crashed container's real error lives), node allocatable/taints when readable, and — when
metrics-server is installed — each container's live CPU/memory `usage`. Usage is what turns a
memory-limit recommendation from a guess into arithmetic ("using 210Mi against a 128Mi limit →
raise to 256Mi"); without it the model still reasons, just less precisely. It degrades
gracefully: no metrics-server or no `metrics.k8s.io` RBAC simply omits the field.

**Environment variable values are never included** — only the key names. Log contents are sent
as-is, so if your application logs secrets, they will reach the API.

## Choosing an LLM

Two providers, selected with `PODSMEDIC_PROVIDER`:

| Provider | Set | Notes |
| --- | --- | --- |
| `anthropic` (default) | `ANTHROPIC_API_KEY` | Native Messages API. Adaptive thinking, `effort`, `output_config.format` structured output, and a `cache_control`-marked system prompt. |
| `openai` / `deepseek` | `PODSMEDIC_API_KEY` (or `DEEPSEEK_API_KEY` / `OPENAI_API_KEY`) | Any OpenAI-compatible `/chat/completions` endpoint. `deepseek` is an alias that sets the base URL to `https://api.deepseek.com` and the model to `deepseek-chat`. |

DeepSeek in one line:

```bash
PODSMEDIC_PROVIDER=deepseek DEEPSEEK_API_KEY=sk-... PODSMEDIC_DRY_RUN=true \
  go run ./cmd/podsmedic
```

Point it at any other OpenAI-compatible server (self-hosted vLLM, OpenRouter, a
gateway) by setting `PODSMEDIC_PROVIDER=openai`, `PODSMEDIC_BASE_URL`, and
`PODSMEDIC_MODEL`.

**How the two paths differ.** The Anthropic backend gets schema-enforced JSON
via `output_config.format` and a cacheable prompt prefix. The OpenAI path uses
`response_format: {"type":"json_object"}` — which guarantees valid JSON but not
the *shape* — so the schema is spelled out in the prompt and validated on parse
(`internal/llm/openai.go`). `PODSMEDIC_EFFORT` applies to Anthropic only;
`deepseek-reasoner` does its own reasoning without a knob.

## Landing page

`docs/` holds a standalone promotional page — `index.html`, `styles.css`, `app.js`, no
build step and no external requests. Open it directly, or turn it on in
**Settings -> Pages -> Source: main /docs** to publish it.

Its hero is not a screenshot or a video: it is the live view's own mechanic
reimplemented in miniature, so "a failing workload stops orbiting" can be tried rather
than described.

### Serving it with Docker

```bash
docker compose up -d      # http://localhost:8090
WEB_PORT=8091 docker compose up -d   # when 8090 is taken
```

`docker-compose.yml` serves `docs/` and nothing else — there is deliberately no Compose
service for the agent, which needs a Kubernetes API and a ServiceAccount whose narrow
permissions are half the safety argument. The page is mounted read-only, so editing
`docs/` and reloading is enough; there is nothing to rebuild.

## Quick start (local)

```bash
export ANTHROPIC_API_KEY=sk-ant-...
make run          # dry run against your current kubectl context, prints to stdout
```

Then with a real sink:

```bash
export SLACK_WEBHOOK_URL=https://hooks.slack.com/services/...
go run ./cmd/podsmedic
```

See `.env.example` for every setting.

## Deploy to a cluster

```bash
kubectl apply -f deploy/config.yaml       # namespace, settings, secret placeholder
kubectl apply -f deploy/rbac.yaml
kubectl -n podsmedic create secret generic podsmedic \
  --from-literal=ANTHROPIC_API_KEY=sk-ant-... \
  --from-literal=TELEGRAM_BOT_TOKEN=... \
  --from-literal=TELEGRAM_CHAT_ID=... \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f deploy/deployment.yaml   # the workload
```

Settings live in `config.yaml`, the workload in `deployment.yaml`. They are separate files
so that rolling out a new image — the common operation — cannot silently reset every setting
you tuned. Edit the ConfigMap in place and restart; re-apply `config.yaml` only when you mean
to go back to defaults.

### Getting the image to your nodes

`deployment.yaml` refers to an image your nodes must be able to pull. Push it to whatever
registry they already use. If there is no registry — a single-node k3s box, say —
`deploy/local-registry.yaml` runs one inside the cluster:

```bash
kubectl apply -f deploy/local-registry.yaml
kubectl -n registry rollout status deploy/registry
kubectl -n registry port-forward svc/registry 5000:5000 &

docker build -t podsmedic:v1 .
docker tag podsmedic:v1 localhost:5000/podsmedic:v1
docker push localhost:5000/podsmedic:v1        # then set that as the image
```

It avoids touching the node at all. The registry binds `hostPort: 5000` to `hostIP:
127.0.0.1`, so it sits on the node's own loopback, where containerd treats a registry as
insecure by default and pulls over plain HTTP with no `registries.yaml` and no
`--insecure-registry` flag. Loopback rather than `0.0.0.0` also keeps it off your LAN, which
matters because it speaks plain HTTP. Pushing works because `kubectl port-forward`
reaches the pod directly rather than through the host port.

Single-node only: on a multi-node cluster just one node would hold the images.

### RBAC

RBAC is read-only: `get/list/watch` on pods and events, `get` on pods/log, `get/list` on nodes.
The node rule was optional when it only enriched evidence; it now also backs the
[capacity review](#capacity-review), so dropping it means `scale_replicas` refuses rather
than degrades. Everything else still degrades gracefully without it.

Run **one replica**. The dedupe cache is in-memory, so a second replica double-alerts.

## Auto-heal

podsmedic can apply the fix, not just describe it. For an OOMKilled pod it will raise the
memory limit on the owning Deployment/StatefulSet/DaemonSet; the pod restarts with more
headroom and the incident closes itself.

**The LLM proposes; podsmedic disposes.** The model returns a structured `action`, but pod logs
can carry prompt injection, so nothing the model says is trusted. `internal/heal.Validate` — a
pure, fully unit-tested function — re-checks every field against hard caps and produces a bounded
plan, or refuses. That validator, not the model, is the security boundary.

### What it will and won't do

Allowed actions:

- **`patch_resources`** — raise a container's memory/CPU requests or limits on the owning
  controller.
- **`restart_workload`** — rollout restart of a controller-owned workload.
- **`patch_image`** — correct a container image reference for an `ImagePullBackOff` caused by a
  wrong tag. Strictly bounded: the registry and repository must stay identical to the current
  image — **only the tag or digest may change** — and `latest` is refused. A different repository
  or registry is rejected outright, which is what stops a prompt-injected log line from pointing
  the workload at a malicious image. A wrong-but-valid guess is caught by the verify/rollback
  loop. Enable by adding `ImagePullBackOff` to `PODSMEDIC_HEAL_KINDS`.
- **`patch_probe`** — loosen a liveness/readiness probe when an over-aggressive probe (e.g. an
  `initialDelaySeconds` shorter than the app's startup) causes CrashLoopBackOff or a stuck
  not-ready. Only ever **increases** timing (`initialDelaySeconds`, `periodSeconds`,
  `timeoutSeconds`, `failureThreshold`) — never tightens, never disables, never changes the probe
  target — and each field is capped (`PODSMEDIC_HEAL_MAX_PROBE_*`). Enable by adding
  `CrashLoopBackOff` (and/or `RestartStorm`) to `PODSMEDIC_HEAL_KINDS`.
- **`scale_replicas`** — raise a workload's replica count to spread load, for sustained CPU
  pressure. Only ever scales **up**, never down (which would cut availability). The count is
  **computed, not configured** — see [Sizing a scale-up](#sizing-a-scale-up). Typically driven by a
  predicted `CPUPressure` problem — add `CPUPressure` to `PODSMEDIC_HEAL_KINDS`.

Hard rules the validator enforces regardless of the model's output:

- **Only raises, never shrinks** a resource value.
- **Limits by default, requests opt-in.** Raising a *limit* never affects
  scheduling. Raising a *request* can push a pod past node capacity and leave it
  stuck `Pending` — worse than the crash. Request patching is therefore gated
  behind `PODSMEDIC_HEAL_PATCH_REQUESTS=true`, and even then a proposed request
  that exceeds a node's total allocatable is refused as unschedulable.
- **Bounded**: a value may grow at most `PODSMEDIC_HEAL_MAX_MULTIPLIER`× (default 4), and never
  past the absolute cap (`PODSMEDIC_HEAL_MAX_MEMORY` / `_MAX_CPU`, default 4Gi / 4 cores).
- **Fits in the cluster.** Anything that would add a pod (`scale_replicas`) or enlarge one
  (`patch_resources` on requests) is checked against real remaining headroom, with a reserve held
  back — see [Capacity review](#capacity-review). No headroom, no change.
- **Confidence gate**: only heals when the diagnosis confidence is at least
  `PODSMEDIC_HEAL_MIN_CONFIDENCE` (default `high`).
- **Kind allowlist**: only problem kinds in `PODSMEDIC_HEAL_KINDS` (default `OOMKilled`).
- **Namespace guard**: `kube-system`, `kube-public`, `kube-node-lease` are denied by default; an
  optional allowlist narrows further.
- **Cooldown** per workload (`PODSMEDIC_HEAL_COOLDOWN`, default 1h) so a settling pod is not
  re-healed every poll.
- **Cluster-wide brakes**, because every limit above is per *workload*. See
  [Blast radius](#blast-radius).
- **Autoscaler guard.** A workload with a HorizontalPodAutoscaler is never scaled by
  podsmedic. Two controllers writing `spec.replicas` overwrite each other every reconcile, and
  the HPA is the better-informed of the two — it is built for this and runs continuously,
  where podsmedic sees the workload once a sweep. The alert names the HPA and points at the
  real lever: raise its `maxReplicas`. Only scaling is blocked; a memory-limit raise, an image
  fix, or a restart are untouched, since the HPA does not manage those fields.
- **GitOps guard.** A workload reconciled by ArgoCD, Flux, or Helm (detected from its
  controller's labels/annotations) is skipped — an in-cluster patch would be reverted on the
  next sync, or start a fight with the reconciler. The fix belongs in the source repository.
  Override with `PODSMEDIC_HEAL_ALLOW_GITOPS=true`.
- **No delete, ever.** No namespaces, PVCs, pods, or secrets are touched. Only controller
  `patch`. Owning controllers are the only writable resource.

### Blast radius

Every limit described so far is scoped to a single workload: the heal cooldown, the circuit
breaker, the playbook. That leaves a gap worth naming, because it is the one that turns a bad
night into an outage — **fifty distinct workloads failing at once pass all of them**, because
each one is a different workload. `PODSMEDIC_MAX_ALERTS_PER_CYCLE` does not help: it caps new
incidents, and therefore LLM calls, while heal *retries* run for every open incident on every
sweep.

Two cluster-wide brakes close it.

**A per-sweep allowance.** `PODSMEDIC_HEAL_MAX_PER_SWEEP` (default `3`) caps how many heals
execute in one sweep. Anything beyond it is deferred to the next sweep, not dropped, and
metered as `heals_total{result="budget_spent"}`. Even when every individual heal is correct,
rewriting twenty controllers a minute is churn no operator can follow.

**A surge detector.** When `PODSMEDIC_HEAL_SURGE_RATIO` (default `0.25`) of the cluster's
workloads are failing simultaneously, podsmedic suspends healing entirely for that sweep and
sends one notice. The reasoning is not about rate but about *cause*: a quarter of the cluster
does not break at the same instant because a quarter of the cluster is misconfigured. A node
went away, storage stalled, a registry became unreachable. Raising memory limits in response is
noise at best, and at worst it rolls every affected Deployment while the cluster is already
struggling.

It counts **distinct workloads, not pods** — one Deployment with thirty crash-looping replicas
is a single failure, and counting it as thirty would trip the brake on exactly the case
podsmedic handles best. `PODSMEDIC_HEAL_SURGE_MIN_WORKLOADS` (default `8`) keeps the ratio off
small clusters, where one failure out of three is 33% and means nothing.

Suspension is announced once, not once per sweep, and lifts by itself when the failure rate
drops. Alerting and diagnosis continue throughout — only the changes stop. Metrics:
`heal_surge_trips_total`, `heals_total{result="surge_suspended"}`.

### Verification and rollback

An applied heal is not fire-and-forget. When `PODSMEDIC_HEAL_APPLY=true`, every
real resource patch is persisted to a ConfigMap (`podsmedic-heal-state`, in
podsmedic's own namespace) together with the exact prior values. After
`PODSMEDIC_HEAL_VERIFY_AFTER` (default 10m) the next sweep re-checks the
workload:

- **Recovered** — the record is retired and a short "verified" notice is sent.
- **Still failing** — podsmedic rolls the workload back to its recorded prior
  values and alerts that the fix did not hold and needs a human.

Persisting to a ConfigMap (rather than memory) is deliberate: a crash-restart of
podsmedic must not forget a pending heal and leave a bad change in place, nor
re-heal a workload that is still settling. Verification is a no-op under a dry
run — there is nothing applied to check — so it only engages once
`HEAL_APPLY=true`. Set `PODSMEDIC_HEAL_VERIFY=false` to disable it.

One known limit: if a heal *added* a limit where none existed, a strategic-merge
rollback cannot un-set it (it can only lower a value that was already there); the
raised limit is left in place and the alert says so.

### Audit trail

Every heal that changes cluster state — an applied patch, a dry run, a
verification, a rollback — is appended to a durable trail in a ConfigMap
(`PODSMEDIC_AUDIT_CONFIGMAP`, default `podsmedic-audit`, in podsmedic's own
namespace). Each entry records the timestamp, workload, container, action,
outcome, and the exact before/after values, so a change can be reviewed long
after the alert scrolled away:

```json
{
  "time": "2026-07-30T06:59:59Z",
  "namespace": "oom-test",
  "controller": "Deployment/retry-demo",
  "container": "hog",
  "action": "patch_resources",
  "outcome": "applied",
  "old": { "limit.memory": "128Mi" },
  "new": { "limit.memory": "512Mi" },
  "summary": "patch container \"hog\": memory limit 128Mi→512Mi"
}
```

Outcomes are `applied`, `dryrun`, `verified`, `rolledback`, and
`rollback_failed`. The trail is bounded: past `PODSMEDIC_AUDIT_MAX_EVENTS`
(default 500) the oldest entries are dropped, keeping it well under the ConfigMap
size limit. Read it with `kubectl get configmap podsmedic-audit -n podsmedic -o
jsonpath='{.data.events\.json}' | jq`. Set an empty `PODSMEDIC_AUDIT_CONFIGMAP`
to disable it. Unlike the pending-heal state, the trail records dry runs too, so
it is useful even before you flip `HEAL_APPLY=true`.

### Circuit breaker

Some workloads cannot be fixed by a bounded patch — a container that OOMs no
matter the limit, an image that is genuinely broken. Left alone, podsmedic would
heal it, watch verification fail, roll it back, and do it all again on the next
sweep: pure churn that buries the real problem. A per-workload **circuit
breaker** stops that. Keyed by controller (so it spans pod restarts), it counts
recent heals and rollbacks and trips **open** when either crosses a threshold:

- `PODSMEDIC_HEAL_BREAKER_MAX_ROLLBACKS` (default 2) rollbacks in the window — a
  fix that keeps not holding.
- `PODSMEDIC_HEAL_BREAKER_MAX_HEALS` (default 4) heals in the window — a workload
  that recovers and relapses over and over.

While open, healing for that one workload is skipped (the alert says so, metered
as `result="breaker_open"`) for `PODSMEDIC_HEAL_BREAKER_OPEN_FOR` (default 6h),
and a single "breaker OPEN … manual review needed" notice is sent. When the
window elapses the breaker closes with a clean slate. Other workloads are
unaffected. Set `PODSMEDIC_HEAL_BREAKER=false` to disable, or a threshold to `0`
to switch off just that signal. The breaker is in-memory: a podsmedic restart
clears tripped breakers.

### Playbook (learned heals)

Most clusters fail the same handful of ways over and over. The first time a
workload hits a problem, podsmedic diagnoses it with the LLM, applies a bounded
fix, and — crucially — *verifies* it. Once a fix passes verification it is
**learned**: recorded in a playbook ConfigMap
(`PODSMEDIC_PLAYBOOK_CONFIGMAP`, default `podsmedic-playbook`), keyed by workload
controller and problem kind.

The next time that workload hits that same problem, the remembered fix is
**replayed directly — no LLM call, no diagnosis latency**:

```
problem detected
  └─ playbook has a verified remedy for this controller+kind?
       ├─ yes → re-validate the remembered action → execute      (0 tokens)
       └─ no  → diagnose with the LLM → on verify, learn it
```

The replay is not a blind rerun. It passes back through the **same guardrails**:
the pure validator re-checks it against the *current* state (so a fix that no
longer fits is declined and the LLM takes over), the circuit breaker still
applies, and the result is verified like any other heal — if a replayed fix ever
fails to hold, it is **rolled back and evicted** from the playbook, so the book
only ever holds remedies that currently work. Entries are capped
(`PODSMEDIC_PLAYBOOK_MAX_ENTRIES`, default 500, oldest-verified evicted first) and
persisted across restarts.

The effect compounds: the longer podsmedic runs, the more of its healing is
served for free. `podsmedic_playbook_hits_total` against
`podsmedic_llm_requests_total` shows exactly how much diagnosis cost the playbook
is saving. Needs auto-heal enabled and the ConfigMap RBAC from
`rbac-autoheal.yaml`; set an empty name to disable.

### Predictive heal

Everything above is reactive: something breaks, podsmedic fixes it. Prediction
makes it *pre-emptive*. With `PODSMEDIC_PREDICT=true`, every sweep reads live
memory and CPU usage from metrics-server and watches each container against its
limits. A container that stays at or above `PODSMEDIC_PREDICT_MEMORY_RATIO`
(default `0.90`) for `PODSMEDIC_PREDICT_MIN_CHECKS` (default 3) consecutive sweeps
raises a problem *before* the failure:

- **`MemoryPressure`** — memory near the limit → the LLM proposes a bounded
  **limit raise** (`patch_resources`), heading off the OOM kill.
- **`CPUPressure`** — CPU pinned at the limit (heavy throttling) → the LLM
  proposes a bounded **scale-up** (`scale_replicas`) to spread the load.

From there it is an ordinary problem: it correlates into an incident, the LLM
(told the signal is predictive) proposes the fix, the validator and circuit
breaker apply, the heal is verified, and — the payoff — it is **learned into the
playbook**, so the next workload that creeps toward the same wall is fixed for
free. The streak requirement (not a single spike) filters transient churn; a
container that already has a *real* problem this sweep is never shadowed by a
prediction; memory and CPU streaks are tracked independently. A prediction never
triggers a rollback (it is a forecast, not a confirmed failure), so a stale
reading on an old pod mid-rollout can't undo a good heal.

Prediction is **off by default** — it mutates workloads that have not actually
failed yet — and needs metrics-server. To *heal* predictions rather than just
alert, add `MemoryPressure` and/or `CPUPressure` to `PODSMEDIC_HEAL_KINDS`; left
out, you get an early-warning alert and no change. Metrics:
`podsmedic_predictions_total`, `podsmedic_predicted_memory_pressure`.

### Capacity review

Every heal that would **add** a pod or **enlarge** one is bounded by what the cluster
can actually take. Once per sweep podsmedic reads every node's allocatable capacity
and every scheduled pod's requests, and derives the remaining headroom:

```
per node:  free = allocatable − Σ(requests of pods bound here) − reserve
fits       = Σ over schedulable nodes of  min(freeCPU/podCPU, freeMem/podMem, freePodSlots)
```

Four details in that arithmetic are what make it trustworthy rather than decorative:

- **Bin-packed per node, not summed cluster-wide.** A pod has to fit on *one* node.
  Two nodes with 3Gi free each cannot host a 6Gi pod, and aggregate arithmetic would
  cheerfully say they can.
- **Pod slots count as a resource.** A node has a pod cap (typically 110). Filling it
  is what makes a kubelet stop being responsive, long before CPU runs out — so pod
  count bounds the fit even for tiny BestEffort pods that request nothing.
- **A reserve is held back.** `PODSMEDIC_HEAL_CAPACITY_RESERVE` (default `0.20`) keeps
  20% of every node's allocatable off-limits. Requests-based headroom is not the same
  as idle capacity: pods burst above their requests, DaemonSets land, and a drained
  node's workload has to go somewhere. Filling the cluster to the brim because the
  arithmetic said it fit is precisely the failure this prevents.
- **Cordoned and NotReady nodes contribute nothing.** Their free space is not real.

`kube-system` and friends are still counted as *consumers* of capacity even though they
are never healed — capacity is a property of the cluster, so the pod list is read across
all namespaces even when `PODSMEDIC_NAMESPACES` narrows what podsmedic watches.

**Fail closed.** If the node or pod list is denied, there is no snapshot, and
`scale_replicas` is refused outright with that reason rather than proceeding blind.
Request raises fall back to the older single-node check instead of refusing, since
that guard predates the snapshot and losing it silently would be a regression.

This is a *necessary*, not sufficient, condition. Taints, affinity, topology spread and
PVC zoning can still leave a pod Pending; podsmedic makes the outright-impossible
impossible, and leaves the rest to the scheduler.

Metrics: `podsmedic_cluster_cpu_free_millicores`, `podsmedic_cluster_memory_free_bytes`,
`podsmedic_cluster_pod_slots_free`, `podsmedic_cluster_nodes_schedulable` — all already
net of the reserve, so they show what a heal is actually allowed to consume.

### Sizing a scale-up

The replica count is **computed from measurement**, not read from a config value and not
taken from the model. Given the workload's aggregate live CPU across its replicas,
podsmedic applies the same utilisation formula an HPA uses:

```
desired = ceil(current × observedUtilisation / targetUtilisation)
```

Four replicas averaging 95% of their CPU limit, against the 70% target
(`PODSMEDIC_HEAL_SCALE_TARGET_CPU`), gives `ceil(4 × 0.95/0.70)` = **6**. The load is
summed over the whole workload, not just the pod that tripped the detector, so one hot
replica does not drag the count up on its own.

That target is then trimmed by, in order: cluster headroom, the `HEAL_MAX_MULTIPLIER`
step limit, and any hand-set backstop. Whatever survives is what gets applied, and the
alert shows the arithmetic:

```
scale controller owning api/web-7d9f: 4→5 replicas
(CPU at 95% of limit across 4/4 replicas; 70% target needs 6; trimmed to 5 by cluster
headroom (room for 1 more))
```

**The model does not pick the number.** Its `replicas` field can only ever make the
computed target *smaller* — a proposal of 500 is ignored, a proposal of 5 against a
computed 6 is honoured. Pod logs can carry prompt injection, and "how many pods should
exist" is not a question worth trusting them on.

`PODSMEDIC_HEAL_MAX_REPLICAS` is tri-state:

| Value | Behaviour |
| --- | --- |
| `auto` (default) | Derive the count. Capacity and the step multiplier are the only bounds. |
| `0` | Scaling disabled entirely. |
| a number | Still derive the count, but never exceed this hand-set ceiling. |

A derived target that breaches a bound is **clamped** — partial relief now beats nothing,
and the next sweep re-evaluates. A model-*proposed* target that breaches a bound is
**refused**, because an out-of-range number from an untrusted source is a signal, not a
rounding problem.

Both derived scaling and the capacity gate need metrics-server and node reads. Without
them the workload is alerted on, not scaled.

**An HPA wins outright.** If a HorizontalPodAutoscaler targets the workload, none of the above
runs: podsmedic refuses the scale and says which HPA owns it. Unlike the capacity gate this
check is *not* fail-closed — an unreadable HPA list logs a warning and scaling proceeds. The
two differ because the consequences do: scaling into a cluster with no room strands Pending
pods nobody notices, whereas scaling against an HPA is self-limiting and visible — the HPA
wins, verification sees the workload unchanged, the heal rolls back, and the breaker trips if
it repeats. Failing closed would instead disable scaling for every cluster that has no HPAs at
all and never granted the read.

### Turning it on (two deliberate steps)

Enabling is a two-switch commitment so you can preview before you mutate:

| `PODSMEDIC_AUTOHEAL` | `PODSMEDIC_HEAL_APPLY` | Behaviour |
| --- | --- | --- |
| `false` (default) | — | No healing. Alert only. |
| `true` | `false` | **Dry run.** Validate the fix and run it as a server-side dry run — the API server checks it, nothing persists. Alert shows what *would* change. |
| `true` | `true` | Applies the bounded patch for real. |

Grant the extra RBAC (the base role is read-only):

```bash
kubectl apply -f deploy/rbac-autoheal.yaml   # patch on deployments/statefulsets/daemonsets,
                                             # plus a namespaced Role for the state ConfigMap
```

Local dry-run test:

```bash
PODSMEDIC_AUTOHEAL=true PODSMEDIC_HEAL_APPLY=false PODSMEDIC_DRY_RUN=true \
  ANTHROPIC_API_KEY=sk-ant-... go run ./cmd/podsmedic
```

The alert footer then reads e.g. `Auto-heal (dry run) Deployment/web in api: patch container
"web": memory limit 128Mi→256Mi`. Flip `PODSMEDIC_HEAL_APPLY=true` when you trust it.

## Incident correlation

One failing container rarely trips just one detector: an OOMKilled pod is also
CrashLoopBackOff and, soon, a restart storm. Left alone that is three alerts and
three LLM calls for a single failure. podsmedic instead groups them into one
**incident**, keyed by `namespace/pod/container` (a crash-looping pod keeps its
name across in-place restarts, so the incident spans them):

- The first symptom opens the incident and runs the full diagnose + alert.
- Any other symptom seen in the **same sweep** folds into that one alert, which
  reads e.g. *"Also seen on this pod: CrashLoopBackOff, RestartStorm"*.
- A genuinely new symptom in a **later sweep** sends a one-line follow-up notice
  — no second diagnosis, no second LLM call.
- Repeats are suppressed while the incident stays open.
- When the workload goes quiet for `PODSMEDIC_COOLDOWN` (default 30m), the
  incident resolves and a short "Resolved" notice is sent; a later recurrence
  opens a fresh incident.

Because a diagnosis runs only when an incident opens, an incident that could
**not** be healed then — the proposal was declined, GitOps-guarded, or hit a
transient patch error — remembers its validated proposal and **retries the heal
on later sweeps**, against a fresh evidence bundle and with no second LLM call.
So a workload that becomes healable while its incident is still open (e.g. a
GitOps label is removed) heals on the next sweep instead of waiting for the
incident to resolve. The heal cooldown still applies, and a retry that finally
applies sends a *"succeeded on retry"* notice.

Metrics: `podsmedic_incidents_total`, `podsmedic_incident_updates_total`,
`podsmedic_incidents_resolved_total`, `podsmedic_incidents_open`.

**Persistence.** The incident set — including each incident's pending heal
proposal — is written to a ConfigMap (`PODSMEDIC_INCIDENT_STATE_CONFIGMAP`,
default `podsmedic-incident-state`) at the end of every sweep that changed it, and
reloaded on startup. A restart therefore continues open incidents instead of
re-alerting them, and a heal that was waiting to be retried survives the restart
(no re-diagnosis, no second LLM call). It needs the same ConfigMap RBAC as the
heal state (`rbac-autoheal.yaml`); writes are non-fatal if that is absent. Set an
empty name to run in-memory only, in which case a restart re-alerts open
incidents once.

## Storage faults

Two kinds cover the volume layer, and both are **diagnose-only**.

- **`PVCPending`** — the scheduler will not place the pod because a volume will not bind: a
  claim that does not exist, a StorageClass that does not exist, no matching
  PersistentVolume, or a node-affinity conflict on an existing one.
- **`VolumeMountFailed`** — the pod *was* scheduled, but the kubelet cannot attach or mount
  its volumes, so it sits in `ContainerCreating` past
  `PODSMEDIC_VOLUME_MOUNT_GRACE` (default `2m`).

`PVCPending` is raised **instead of** `Unschedulable`, not alongside it. Both look identical
to the scheduler — same `Unschedulable` reason — but "no node has 4 CPUs" and "this claim
will never bind" have nothing in common as fixes, so collapsing them into one kind would
bury the useful half. The distinction comes from the scheduler's message text, which is the
only place the difference appears.

`VolumeMountFailed` requires the pod to mount something mountable — a PVC, secret, configMap,
or CSI volume. A pod stuck in `ContainerCreating` with only an `emptyDir` is almost always a
CNI or image problem, and labelling that a volume failure sends the reader the wrong way.

### The claim's own events are the diagnosis

Pod status only ever says a volume is "unbound". *Why* it is unbound — `ProvisioningFailed`,
no such StorageClass, provisioner quota exhausted, no matching volume — appears only in the
**PVC's** events. So the bundle carries a `volumeClaims` section per claim: phase, storage
class, requested vs actual size, access modes, and those events. For a bound claim it also
reads the PV, chiefly for node affinity, which is what strands a pod when a local or zonal
volume is pinned to a node the pod did not land on.

A claim that does not exist is reported as `missing` — a complete diagnosis by itself. One
that cannot be read is reported as `unreadable` with the reason, so absent RBAC is never
mistaken for a healthy volume.

### Recovery: two actions, and only two

Storage was diagnose-only at first. It now has exactly two recoveries, both narrow enough that
neither can lose data, and both still opt-in through `PODSMEDIC_HEAL_KINDS`.

**`restart_workload` — for the "it crashed and got stuck" case.** A node reboots, a volume stays
attached to the dead node, an attachment goes stale; the storage recovers but the pod stays
wedged. A rollout restart clears it. The precondition is the whole safety argument: **every claim
the pod mounts must already be `Bound`**. If any is still Pending or missing, the restart is
refused — recreating a pod that cannot start is pure churn that buries the real problem. This
action touches no storage object at all.

**`create_pvc` — for the "we forgot to apply the PVC" case.** A Deployment references
`claimName: foo`, nobody ever created `foo`, and the pod sits Pending forever. podsmedic creates
it. This is the only thing podsmedic ever creates, and the only write it has against storage.

Everything else stays refused above the `HEAL_KINDS` allowlist: no editing a claim, no resizing,
no forcing a detach, no deleting a volume. Those are irreversible or destroy data, so listing
`PVCPending` in `HEAL_KINDS` cannot unlock them.

### Bounds on creating a claim

`create_pvc` is off by default (`PODSMEDIC_PVC_AUTOCREATE=false`). It is a real expansion of
what podsmedic can do — the first `create` verb it has ever had — so the bounds are worth
reading before enabling it:

- **Only a claim that does not exist.** A claim that exists is never touched, whatever state it
  is in. A Pending claim is the provisioner's problem, not something to recreate.
- **The model picks the action, not the volume.** Size, storage class, and access mode all come
  from configuration. A provisioned volume costs money and cannot be shrunk once bound, which
  makes it a poor thing to size from evidence that includes untrusted pod logs.
- **`PODSMEDIC_PVC_MAX_SIZE`** (default `10Gi`) is a hard ceiling. A `DEFAULT_SIZE` above it is
  capped, not honoured.
- **An explicit `PODSMEDIC_HEAL_NAMESPACES` is required** — stricter than every other action,
  where an empty allowlist means "all namespaces". Provisioning storage cluster-wide by default
  is not a reasonable reading of any configuration.
- **One missing claim only.** Two or more is ambiguous, and it refuses rather than guessing.
- **Single-replica workloads only.** Several replicas sharing one claim need `ReadWriteMany`,
  which many storage classes do not offer; a `ReadWriteOnce` guess would bind and then block
  every replica but one — worse than the Pending pod it set out to fix.
- **No rollback, by design.** Undoing a create means deleting a claim that may by then hold
  data. Created claims are labelled `app.kubernetes.io/created-by=podsmedic` so they are easy
  to find and remove by hand.

RBAC for reading claims (`get` on `persistentvolumeclaims`, `persistentvolumes`) lives in the
read-only base role. The `create` verb lives in `rbac-autoheal.yaml` and is **create-only** —
no update, patch, or delete. Omit that one rule and auto-creation is impossible regardless of
configuration.

```bash
PODSMEDIC_HEAL_KINDS=OOMKilled,PVCPending,VolumeMountFailed \
PODSMEDIC_PVC_AUTOCREATE=true \
PODSMEDIC_HEAL_NAMESPACES=my-app \
PODSMEDIC_PVC_DEFAULT_SIZE=5Gi
```

## Ask it questions (Telegram)

The Telegram bot is two-way. As well as pushing alerts out, podsmedic can listen for
questions and answer them from the same cluster state its sweep works from:

> **you** — why does checkout keep restarting?
>
> **podsmedic** — `shop/checkout-6b4f` has been CrashLoopBackOff for 34m. The container
> exits immediately with `FATAL: DATABASE_URL is not set`; the Deployment references a
> secret key `db-url` that does not exist in the `shop` namespace. No automated fix
> applies — a missing env var is not something I can patch safely. Create the key, or
> point the envFrom at the right secret.

Turn it on with `PODSMEDIC_TELEGRAM_LISTEN=true` alongside the bot token you already
have. It is **off by default**: an inbound channel exposes cluster state and spends
tokens, so enabling it is a deliberate act rather than a side effect of configuring
alerts.

```bash
PODSMEDIC_TELEGRAM_LISTEN=true \
TELEGRAM_BOT_TOKEN=123456:ABC-DEF... \
TELEGRAM_CHAT_ID=-1001234567890 \
  go run ./cmd/podsmedic
```

### It answers; it does not take orders

This is the boundary that matters. The chat path reads state and calls the model, and
there is **no branch from it into the healer** — the LLM used for answers is given no
action to propose, so an `Answer` has no field that could ever reach `heal.Validate`.
"Restart the api deployment" gets you the kubectl command, not a restart. Healing stays
driven by the sweep and its safety checks, where the validator, cooldowns, circuit
breaker and capacity gate all still apply.

Three more guards:

- **Allowlisted chats only.** A bot's username is discoverable, so anyone could DM it.
  `PODSMEDIC_TELEGRAM_ALLOWED_CHATS` defaults to `TELEGRAM_CHAT_ID`; an empty list
  serves nobody. An unauthorised message gets no reply at all — not even an error, which
  would confirm the bot exists.
- **Rate limited** per chat (`PODSMEDIC_CHAT_MAX_PER_MINUTE`, default 6), because each
  free-form question is an LLM call.
- **Backlog dropped.** Telegram holds undelivered updates for ~24h. A restart after an
  outage would otherwise replay every question at once, each one answering a stale
  cluster. Messages predating the process are discarded.

### Commands are free

Six commands are answered from local state with **no LLM call at all**:

| Command | Answers |
| --- | --- |
| `/status` | What the last sweep found, and whether healing is off / dry-run / applying |
| `/pods` | Pods and their state. Bare, it summarises and lists only what needs attention; add a namespace or name to list those in full, or `all` for everything |
| `/incidents` | Currently open incidents, oldest first |
| `/capacity` | Cluster headroom exactly as the heal validator sees it, reserve included |
| `/heals` | The last 10 audit entries — what changed, and whether it held |
| `/playbook` | Verified remedies that replay without an LLM call, most-used first |
| `/export` | The playbook as a document to study — see below |
| `/help` | The above, plus examples |

Anything else is a question for the model. It gets a JSON snapshot: the last sweep's
results, open incidents, capacity, recent heals, and podsmedic's own configuration. If
the question names a pod podsmedic has seen, that pod's full evidence bundle is attached
too — which is the difference between "web is crashing" and the actual reason.

Pod logs reach the model here as they do during diagnosis, so the same prompt-injection
caveat applies. The blast radius is smaller — the reply is text, and the model is told to
report rather than obey anything embedded in the evidence — but a log line can still
influence what an answer says.

`podsmedic_chat_answers_total{result}` counts model-answered questions, so comparing it
against total traffic shows how much the free commands absorb.

### Exporting the playbook as a study document

`/export` sends the playbook back as a file. It is built entirely from stored state — no LLM
call, no tokens — so it can never describe a remedy that was not actually learned.

| Command | You get |
| --- | --- |
| `/export` | `podsmedic-playbook.md` — Markdown, reads well anywhere |
| `/export html` | `podsmedic-playbook.html` — self-contained, with print CSS |

The document is organised for someone learning how *this* cluster fails, not Kubernetes in
general: remedies grouped by problem kind with the most common first, each workload's fix
alongside **the diagnosis's own reasoning** for why that fix was right, then a change history
showing which fixes held and which were rolled back. The rolled-back rows are the instructive
ones.

**On PDF.** There is no PDF generator, and that is a dependency decision rather than an
oversight: podsmedic runs on stdlib plus `client-go` and the Anthropic SDK, and a PDF library
would be a poor trade for one output format. `/export html` produces a self-contained document
with print styling — page breaks between sections, tables that survive them, no external
fetches — so a browser's **Print → Save as PDF** gives a clean result. `/export pdf` is
accepted as an alias for the HTML form.

A dry-run playbook is labelled as such in the document, so nobody studies a list of changes
believing they were applied when `PODSMEDIC_HEAL_APPLY=false`.

## Live view

A globe with your workloads in a ring around it, and a wire drawn to one each
time something happens to it. Two families of wire, told apart by colour:

- **the cluster changing** — a pod crashed, OOMed, restarted, recovered, vanished
- **podsmedic acting** — diagnosing, healing, verifying, rolling back, declining

The direction of the travelling pulse says which: outward from the globe when
podsmedic reaches for a workload, inward when the cluster reports something. Watching
a red wire arrive and a green one answer it is the whole point.

```bash
PODSMEDIC_UI_ADDR=0.0.0.0:3456 go run ./cmd/podsmedic
# in-cluster:
kubectl -n podsmedic port-forward deploy/podsmedic 3456:3456
```

Binding `0.0.0.0` inside a pod does **not** expose the view outside the cluster: a pod has
its own network namespace, so "all interfaces" means all of *its* interfaces. Reaching it from
elsewhere needs a Service, a NodePort, or a hostPort — none of which this project ships, on
purpose.

**Off by default, and on its own port.** It does not join the metrics server, and
that separation is deliberate: `/metrics` is safe to hand a scraper, whereas this
page lists every workload name, every failure, and every change podsmedic made.

**A non-loopback bind requires a token.** Set `PODSMEDIC_UI_TOKEN` and the page asks
for it once, then carries an in-memory session cookie. Without one, podsmedic
*refuses to start the view* on any address reachable from off the machine — a
refusal rather than a warning, because a warning in a log nobody reads is exactly
how a dashboard like this ends up on a LAN. Sessions clear on restart, guesses are
throttled per source address, and the token is compared in constant time.

It still speaks plain HTTP: there is no certificate to manage for something reached
over a port-forward. On an untrusted network, port-forward remains the intended
route, and there is no Service and no Ingress for it on purpose.

### Reading the orbits

Each namespace is an orbital shell — a tilted ellipse at its own radius, running at its own
speed, some clockwise and some not. Every workload rides its namespace's shell and passes in
front of the globe on one half of the orbit and behind it on the other.

**A workload with an open problem stops.** It holds its position, drops its wake, and gains two
static rings, while everything else on the same shell keeps orbiting past it. That is the
signal the page is built around: colour needs you to be looking at it, whereas one motionless
dot in a field of moving ones is caught by peripheral vision — which is what you actually have
spare while doing something else. The stop is reserved for real failures; a workload that is
merely degraded but still serving keeps moving.

A stopped node never pulses, only the healthy-but-active ones do. Something throbbing reads as
alive, which is precisely the wrong impression for a halt. Namespaces that contain a stopped
workload say so in their caption (`oom-test · 4  ⏸3`).

If your system asks for reduced motion, the orbits hold still. Colour and the halo carry the
state on their own — which is also why the stop is never the *only* cue.

### This is the one thing that watches

Everything podsmedic *does* stays on its sweep interval, because diagnosis, healing
and verification are deliberately paced. Only what it *shows* is live: enabling the
view starts a pod informer so the display reacts in seconds. A once-a-minute
"live" view would be a worse lie than no view at all.

The cost is one watch connection and an informer cache of the pods you watch. On a
large cluster raise the memory limit in `deployment.yaml`; the default 256Mi suits
a few hundred pods.

The bar for drawing something is deliberately high. Kubernetes writes pod status
constantly — probe results, condition timestamps, resource-version churn — so only
real transitions count: a restart that actually incremented, readiness that actually
flipped, a waiting reason that actually changed. `ContainerCreating` and
`PodInitializing` never draw, because every rollout passes through them and they
would bury the failures.

### What it will not show

There are no wires between workloads, because podsmedic cannot see pod-to-pod
traffic. It reads pod status, events, logs and usage metrics — it has no visibility
into connections or service dependencies. That needs eBPF, a service mesh, or
Cilium Hubble, and is a different project.

The page is a single embedded file that fetches nothing: no CDN, no fonts, no
libraries. A globe of dots is projection arithmetic, which does not justify a
dependency in a project that has four.

## Observability

podsmedic serves three endpoints on `PODSMEDIC_METRICS_ADDR` (default `:9090`,
empty to disable):

| Path | Purpose |
| --- | --- |
| `/healthz` | Liveness — 200 while the process is up. |
| `/readyz` | Readiness. |
| `/metrics` | Prometheus exposition. |

Key series (all prefixed `podsmedic_`): `up`, `sweeps_total`, `pods_scanned`,
`problems_detected`, `alerts_total{result}`, `llm_requests_total{provider,result}`,
`llm_latency_seconds` (histogram), `llm_tokens_total{provider,kind}`,
`llm_cost_usd_total{provider}`, `heals_total{result}`,
`heal_verifications_total{result}`, `rollbacks_total{result}`,
`heal_breaker_trips_total`, `heal_breaker_open`,
`playbook_hits_total`, `playbook_records_total`, `playbook_evictions_total`, `playbook_entries`,
`predictions_total`, `predicted_memory_pressure`,
`cluster_cpu_free_millicores`, `cluster_memory_free_bytes`, `cluster_pod_slots_free`,
`cluster_nodes_schedulable`, `chat_answers_total{result}`,
`incidents_total`, `incidents_open`,
`sink_check_failures_total{sink}`. The registry is hand-rolled (no
`client_golang` dependency), matching the project's net/http-only stance.
(`heals_total{result}` includes `result="breaker_open"` for heals skipped by the
[circuit breaker](#circuit-breaker).)

**Sink validation at startup.** Each notifier is checked before the loop
begins, so a bad Slack webhook or Telegram token surfaces immediately in the
logs and in `sink_check_failures_total` — not on the first real incident.
Telegram is verified live (`getMe` + `getChat`); a Slack webhook is validated
structurally (a webhook has no read endpoint, and a live probe would post a
visible message). Checks are non-fatal: a transient blip should not stop the
agent.

The `deploy/deployment.yaml` wires `/healthz` and `/readyz` as liveness/readiness
probes and adds `prometheus.io/scrape` annotations.

## Cost control

Every detected problem is one Claude request, so the guardrails matter:

- `PODSMEDIC_COOLDOWN` (default 30m) — an incident stays open (suppressing repeat diagnoses)
  until its workload has been quiet this long; see [Incident correlation](#incident-correlation).
- `PODSMEDIC_MAX_ALERTS_PER_CYCLE` (default 10) — caps requests during a cluster-wide outage,
  when hundreds of pods fail at once.
- `PODSMEDIC_EFFORT` — drop to `medium` or `low` to spend fewer thinking tokens per diagnosis.
- `PODSMEDIC_CHAT_MAX_PER_MINUTE` (default 6) — caps questions per chat when the Telegram
  listener is on. The `/status`, `/incidents`, `/capacity`, `/heals` and `/playbook` commands
  cost nothing.
- The system prompt is marked with `cache_control`, so it is a cacheable prefix. Note that Opus
  models require a ~4096-token prefix before caching engages; the prompt is currently shorter
  than that, so caching is a no-op until you extend it with your own runbooks. Check
  `usage.cache_read_input_tokens` to confirm.

**Know what you are spending.** Every diagnosis records its token usage
(`podsmedic_llm_tokens_total{provider,kind}`, kind `input`/`output`/`cache_read`)
and logs it per request. Set `PODSMEDIC_LLM_PRICE_INPUT` and
`PODSMEDIC_LLM_PRICE_OUTPUT` (USD per million tokens) to also accumulate
`podsmedic_llm_cost_usd_total{provider}` — a running estimate of spend. Cache
reads are priced at the input rate (a small over-estimate); the per-kind token
counters stay exact, so a precise cost is derivable in PromQL if you need the
cache discount.

## Extending it

**Add your runbooks to the diagnosis.** Append them to `systemPrompt` in `internal/llm/llm.go`
— that is the stable prefix, so a longer prompt also brings prompt caching into range.

**Add a detector.** Add a `Kind` and a case in `internal/detect/detect.go`; the rest of the
pipeline is generic. Add a table case to `detect_test.go` with a hand-built `corev1.Pod`.

**Add a sink.** Implement `notify.Notifier` (Notify, Notice, Check, Name) and append it in
`buildNotifier` in `cmd/podsmedic/main.go`.

**Add an LLM provider.** Implement `llm.Client` — `Diagnose` plus `Answer` — and wire it into
`llm.New` in `internal/llm/llm.go`. The Anthropic and OpenAI-compatible backends are the two
existing examples.

**Add a chat command.** Add a `Command` and a case in `Parse` in `internal/chat/policy.go`
(with a case in `policy_test.go`), then a branch in `Answer` in `internal/agent/ask.go`.
Answer it from state the agent already holds so it stays free.

**Watch instead of poll.** Swap `ListPods` for a shared informer if you need sub-minute
detection on a large cluster; detection itself is already pure and reusable.

## Development

```bash
make test   # unit tests for detection and dedupe
make vet
make build
```

Go 1.26, `client-go` v0.36, `anthropic-sdk-go` v1.59. The OpenAI-compatible backend is
dependency-free (`net/http`).


## Node health

Every other signal in podsmedic starts from a pod, which means it learns a node is sick only
once that node's pods have already fallen over. A node says so first: `DiskPressure` means the
kubelet has stopped admitting pods and has started evicting to reclaim space; `NotReady` means
everything on the node is stranded until the eviction timeout expires.

`/nodes` reports what the last sweep saw. Faults are also pushed to your notifier, once per
fault rather than once per sweep — a node stays NotReady for as long as it stays NotReady, and
repeating that every minute is how a real alert gets muted. Conditions must hold for
`PODSMEDIC_NODE_GRACE` before they count, because a kubelet restart flaps `Ready` for a few
seconds.

podsmedic **never writes to a node.** The base ClusterRole grants `get` and `list` and nothing
more, and that is not an oversight to fix later: cordoning or draining a node has a blast radius
far beyond patching a single workload, and it is precisely the kind of decision that should not
be reachable from a model reading pod logs. The finding goes to you.

## Rightsizing

`/rightsize` reports containers whose declared requests do not match what they actually use, and
`/rightsize html` sends the full document. Three kinds of finding:

- **Oversized** — reserving far more than the measured peak. The waste is indirect but real: the
  reservation is subtracted from every scheduling decision, so the cluster runs out of room while
  the nodes sit idle.
- **Undersized** — the peak exceeds the request. The pod runs on capacity the scheduler never set
  aside, which overcommits the node and makes this pod a likely eviction victim.
- **NoRequests** — no request declared at all. Reported however small the usage, because the harm
  is not the size: the pod is scheduled best-effort, evicted first, and counted as zero by every
  capacity check podsmedic makes — including the one deciding whether a scale-up fits.

**These are never applied.** `heal.Validate` rests on the invariant that a value may only ever
increase; that is what makes acting on an untrusted model's proposal safe, since the worst case is
a workload with too much. Lowering a request is the opposite bet — it moves the scheduling floor
and the eviction priority, so a wrong number gets a pod evicted under pressure it used to survive.
There is no safe automatic version of that, so it stays a document you apply in your manifests.

Suggestions are the measured **peak** times `PODSMEDIC_RIGHTSIZE_HEADROOM`, never the mean: a
container that idles at 10m and spikes to 900m needs the 900m. A container is judged only after
clearing both `MIN_SAMPLES` and `MIN_WINDOW`, because every workload has a quiet ten minutes and
sizing one from that would be worse than saying nothing. The history persists in a ConfigMap, so a
deploy does not restart the observation window.

## Daily digest

Set `PODSMEDIC_DIGEST_AT=09:00` (and optionally `PODSMEDIC_DIGEST_TZ=Asia/Jakarta`) and podsmedic
posts one summary a day: what it swept, what failed, what it changed, what held, what it rolled
back, what it has stopped trying to fix, and what the sizing report currently suggests.

It sends on quiet days too, and that is the entire point. Every other message podsmedic produces
is triggered by something going wrong, which leaves silence ambiguous — "nothing is broken" and
"the agent died three days ago" look identical from the outside. A digest that arrives on a quiet
day turns its own absence into a signal. Alert on `podsmedic_digests_total` going flat.

The schedule is evaluated by looking *backwards* from the current time rather than forwards from
the last send, so a window missed while the process was restarting is caught late instead of
skipped for a day. `/digest` previews the same text without disturbing the daily accounting.

## Running more than one replica

podsmedic has always documented "exactly one replica", because the circuit breaker and the dedupe
caches live in memory: two replicas would each half-enforce the per-workload heal limits that stop
a heal loop. That makes a node failure a total outage of the thing meant to notice node failures.

`PODSMEDIC_LEADER_ELECT=true` fixes that without changing the in-memory design. Only the leader
sweeps, watches pods, and answers Telegram; the standby holds no state because it does nothing at
all. On failover the new leader starts with empty caches — exactly the state a restart already
produces, and what the persisted ConfigMaps exist to soften.

Losing the lease exits the process rather than waiting to win it back. Another replica owns the
cluster now, and this one's caches describe a period it no longer governs; exiting hands the
kubelet a clean restart as a standby. The Telegram listener is leader-only for a second reason
that is not about state at all: two processes long-polling `getUpdates` with the same bot token
fight over the queue, and Telegram answers the loser with a 409.

It needs the `Role` in `deploy/rbac.yaml` granting `get`/`create`/`update` on `leases` in
podsmedic's own namespace. That is the only object podsmedic writes with auto-heal disabled, and
it names only itself. Watch `podsmedic_leader`: summed across pods it must be exactly 1.

## License

Apache License 2.0 — see [LICENSE](LICENSE). Use it, change it, run it commercially;
the licence carries an express patent grant and asks only that you keep the notices
and state what you changed.
