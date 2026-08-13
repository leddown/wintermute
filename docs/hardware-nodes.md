# Hardware reporting from remote inference hosts

**Status: planned, not built.** This is the design for restoring real VRAM fit
estimates once `wintermuted` runs somewhere other than the machine with the
GPUs. Nothing here exists yet; the behaviour described under
[What happens today](#what-happens-today) does.

- [Why this is needed](#why-this-is-needed)
- [What happens today](#what-happens-today)
- [The shape](#the-shape)
- [Design decisions](#design-decisions)
- [The invariant](#the-invariant)
- [Build order](#build-order)
- [Security](#security)
- [Open decisions](#open-decisions)

---

## Why this is needed

`wintermuted` never loads a model. Every backend is reached over HTTP, so the
server is happiest on a small always-on box while the inference servers stay on
the machines with the hardware — see
[backends.md](backends.md#running-the-server-away-from-the-gpus).

One thing does not survive that move. The hardware probe shells out to
`nvidia-smi` and reads `/proc/meminfo` and `/proc/cpuinfo`, all of which
describe *this* host. Once the models run elsewhere, this host is not the one
that matters, and four surfaces lose their meaning:

| Surface | What it wants to know |
|---|---|
| `GET /api/v1/system` | the inference host's GPUs, VRAM, RAM |
| `estimate_model_fit` | will this model fit on the card that will run it |
| `recommend_model` | which model to pick, ranked against real free VRAM |
| the Models list | a fit verdict per model |

The point of this work is that each of those describes **the machine that will
actually run the model**, and keeps working when there is more than one.

## What happens today

The gap is closed *honestly* rather than correctly: the catalog marks the
profile with `RunsInference`, false when no non-cloud backend has a loopback
`base_url`, and everything downstream reports unknown rather than computing from
the wrong machine.

`EstimateFit` keeps the memory footprint — weights, KV cache and overhead are
properties of the model and hold anywhere — and drops the verdict, free VRAM and
throughput, which are statements about a particular machine. `VerdictUnknown` is
deliberately distinct from `VerdictNo`: *it will not run* and *nobody looked*
lead to opposite decisions, and collapsing them is how a perfectly usable model
gets ruled out.

So today's answer is not wrong, it is absent. This document is about making it
present again.

## The shape

Three pieces.

**1. A node mode on the inference host.** `wintermuted -node` serves exactly one
route, `GET /api/v1/system`, and nothing else: no store, no browser UI, no agent
loop, no backends file. It is the existing `DetectHardware` behind a token.

**2. A `node_url` per backend.** One new optional field in `backends.json`:

```json
{
  "name": "workstation",
  "kind": "llamacpp",
  "base_url": "http://192.168.1.10:8080/v1",
  "node_url": "http://192.168.1.10:9099",
  "model": "qwen3-8b"
}
```

Absent means no hardware is known for that backend — except a loopback
`base_url`, which keeps probing locally exactly as it does now.

**3. Hardware becomes per-backend.** This is the real change, and most of the
work.

```go
// Today — one profile for the host.
func (c *Catalog) Hardware(ctx context.Context) *Hardware

// Added — the profile of the machine serving a named backend.
func (c *Catalog) BackendHardware(ctx context.Context, backend string) *Hardware
```

`Catalog.Hardware` stays as-is for the admin panel, which is legitimately about
the box you are talking to. `EstimateFit(in, hw)` does not change at all — it
already takes the hardware as an argument, so the work is entirely in callers
passing the right profile. `Fit` gains a field naming the host a verdict is
about, because with several backends "it fits" is no longer a complete sentence.

`Catalog.Models` already knows each row's backend, so threading the right
profile through it is nearly free and is the biggest visible win: a per-model
fit column that is correct across several machines.

## Design decisions

**Reuse `wintermuted` rather than add a binary.** Node mode gets `hardware.go`,
the HTTP plumbing and the systemd unit pattern for free. A third binary would
mean a third build, release and deploy path for what is roughly two hundred
lines. `setup.sh` and `update.sh` already install two binaries; a third is a
real cost.

**Do not use the client-token table for node auth.** Node mode authenticates
against a shared secret from the environment with a constant-time compare, so
there is no SQLite file on the GPU box — nothing to migrate, back up, or get
wrong. Reusing `store.ClientByToken` would drag the whole store onto a machine
whose only job is to answer one read-only question.

**Cache per node, fetch in parallel.** Same 15-second TTL as the local probe
(`hardwareTTL`), because free VRAM moves as models load and unload. Without a
per-node cache, one catalog read becomes N sequential HTTP round-trips on a page
that polls.

**Report the node's own timestamp.** `detected_at` comes from the node, labelled
with which host it came from. Two machines' clocks will disagree slightly and
that is fine; silently restamping it locally would hide a node that has stopped
updating.

## The invariant

**A node that cannot be reached yields no hardware for that backend — never a
fallback to the local host.**

This is the one rule that must not bend. Falling back is precisely the bug that
`RunsInference` exists to prevent, and C is where it would be easy to
reintroduce by accident: the local profile is right there, it is non-nil, and
using it would make a timeout look like a successful probe. An unreachable node
means `VerdictUnknown`, the same as no node at all.

This deserves its own test, asserting that an unreachable node does not produce
a verdict computed from local hardware.

## Build order

Each step is independently deployable and verifiable.

| Step | What | Done when |
|---|---|---|
| 1 | `-node` mode, shared-token auth, systemd unit | `curl` against the GPU host returns its real hardware |
| 2 | `node_url` in `backends.json`, per-backend fetch with TTL, unreachable → nil | `GET /api/v1/backends` shows hardware per backend |
| 3 | Thread per-backend hardware through `Catalog.Models` and `estimate_model_fit` | the Models list shows a correct fit per machine |
| 4 | Multi-host ranking in `recommend_model` | a recommendation names the host it is for |
| 5 | Live telemetry (below) | utilisation and temperature visible per GPU |

**Step 5, the telemetry.** The probe currently collects capacity and current
occupancy — total, used and free VRAM, RAM total and available — which is what
fit estimation needs. It does not collect load. Adding it is small:

- `utilization.gpu`, `temperature.gpu`, `power.draw` appended to the
  `nvidia-smi` query in `detectNvidia`, plus fields on `GPU`
- `/proc/loadavg` for CPU load, plus a field on `Hardware`

Worth doing at the same time: `Hardware.CPUCores` counts `processor` lines in
`/proc/cpuinfo`, so it is logical threads rather than physical cores, and the
name says otherwise.

## Security

The node endpoint is a description of your hardware. It is read-only and cannot
trigger anything, which limits the damage, but it still needs care:

- **A shared token is required**, not optional. An unauthenticated
  `/api/v1/system` on the LAN is free reconnaissance.
- **Bind to the LAN interface, not `0.0.0.0`.** Nothing about this should ever
  be reachable from the internet — the same rule the rest of these docs follow
  for inference servers.
- **Keep `PrivateDevices=false` in the node's unit.** The shipped
  `wintermuted.service` deliberately omits `PrivateDevices=true` because
  `nvidia-smi` needs `/dev/nvidia*`; with it set, the GPU section comes back
  empty and every estimate falls back to unknown, which looks like a missing
  driver rather than a sandbox setting. The node unit inherits that constraint
  and little else — it needs no state directory and no write access at all.

## Open decisions

**How `recommend_model` ranks across hosts.** Either rank across every node and
name the winning host, or take a `backend` argument and answer for one. The
former is the better default — the question is usually "what should I run" and
not "what should I run *there*" — but it makes the summary paragraph harder to
write, since it currently assumes a single GPU.

**Whether `Fit` gaining a host field breaks the browser.** The Models list
renders fit verdicts; adding a field is additive, but it is worth checking
`app.js` rather than assuming.

**Whether a node should report what is loaded.** It would be cheap to include,
but the catalog already gets this from the inference server itself — `/props` on
llama.cpp, `/api/ps` on Ollama. Two sources for one fact is how they start
disagreeing.

## See also

- [backends.md](backends.md#running-the-server-away-from-the-gpus) — the split
  this design completes, and what is honest about it today
- [local-models.md](local-models.md) — setting up the machine that serves models
