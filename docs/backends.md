# Running several model backends

`wintermuted` doesn't have "the model". It has a set of named **backends** and a
router that picks one per turn. This document covers what that gets you today,
how to configure more than one, and — the question that usually prompts this —
whether adding backends makes anything faster.

The short answer to that last one is: **sometimes, but not in the way people
expect, and never for a single conversation.** The rest of this explains why,
because the failure mode of guessing here is spending a weekend wiring up a
second GPU box and measuring no improvement at all.

- [What exists today](#what-exists-today)
- [Why a second backend doesn't speed up one conversation](#why-a-second-backend-doesnt-speed-up-one-conversation)
- [What actually gets faster](#what-actually-gets-faster)
- [Configuration recipes](#configuration-recipes)
- [Gotchas that will cost you performance](#gotchas-that-will-cost-you-performance)
- [Measure before you build](#measure-before-you-build)
- [Still to build](#still-to-build)

---

## What exists today

| Feature | Status |
|---|---|
| Several backends declared at once | **Works** |
| A conversation pinned to a chosen backend and model | **Works** |
| Switching a conversation's model mid-transcript | **Works** |
| Automatic retry against a fallback when a backend fails | **Works** |
| Concurrent turns in different conversations | **Works** (see caveats below) |
| Probing each backend for what it serves, and whether it fits | **Works** |
| Splitting one turn across backends | Not built — and see below, it wouldn't help |
| Routing steps within a turn to different models by role | **Not built** — [sketched below](#role-based-routing-within-a-turn) |

So "wintermuted can handle multiple AI model backends at the same time" is true
in both senses: different conversations are served from different backends
concurrently, and a batch of independent items is deliberately split across them
to finish sooner.

## Why a second backend doesn't speed up one conversation

Three independent reasons, each sufficient on its own.

**A turn is a sequential loop.** The agent asks the model, gets a tool call,
runs the tool, and asks again *with the result appended*. Step N+1's input
contains step N's output. There is nothing to overlap. Two backends working on
one turn would mean one of them waiting.

**One GPU runs one model at a time.** On the 8 GB card in
[docs/local-models.md](local-models.md) an 8B model at Q4_K_M plus its KV cache
is most of the VRAM. Two backends pointed at the same machine aren't two workers
— they're two queues into one worker, and generation on a Pascal card is
memory-bandwidth-bound, so a second concurrent request mostly divides the
existing tokens/sec rather than adding to it. Batching does help throughput
somewhat (that's what `--parallel` is for), but it is a throughput win, not a
latency win: each individual request gets slower.

**Alternating backends destroys the prompt cache.** This one surprises people.
Inference servers cache the KV state of the prompt prefix, so turn 5 of a
conversation only has to process the tokens added since turn 4. Send turn 5
somewhere else and that server has to process the *entire* transcript from
scratch — which, in an agent loop that replays a growing transcript every
iteration, is the dominant cost. Round-robining a conversation across two
backends can comfortably be **slower** than pinning it to one. This is why a
session pins a backend rather than picking one per turn, and it is the single
most important thing to understand before designing any distribution scheme.

## What actually gets faster

### 1. Different conversations, different hardware

This works now. Two people (or a desktop harness and a browser tab) hold
separate conversations pinned to separate backends, and they don't queue behind
each other.

The gain is entirely in *separate hardware*. Two backends on one GPU share one
GPU. Worth doing when you have:

- a second machine with a GPU,
- a CPU-only box that can slowly run something small,
- a Hailo or other NPU,
- Claude, for the conversation where you want the ceiling raised.

```json
{
  "default": "gpu",
  "backends": [
    { "name": "gpu",  "kind": "llamacpp", "base_url": "http://192.168.1.10:8080/v1",
      "api_key_env": "LLAMA_API_KEY", "model": "qwen3-8b" },
    { "name": "cpu",  "kind": "ollama",   "base_url": "http://192.168.1.11:11434/v1",
      "model": "gemma3:4b" }
  ]
}
```

### 2. Right-sizing the model to the job

Also works now, per conversation. Most of what wintermute does is not hard:
listing a directory, reading back a metadata result, formatting a filename. A 4B
model does that at two to three times the tokens/sec of an 8B one on the same
card. Keep a `fast` backend (or the same backend with a smaller `model`) for
bulk work and a `smart` one for the conversation where the reasoning matters.

`POST /api/v1/models/plan` will rank what you have against a task class, and
`recommend_model` exposes the same thing to the assistant.

### 3. Fanning a batch out over independent items

**This is where the real speedup lives.** Renaming a directory of 300 files is
not one sequential problem — it's 300 nearly independent ones. Each needs a
lookup and a proposed name; only the final approval pass is inherently serial.

That shape parallelises properly across backends, because each item is a *fresh
short prompt* rather than a continuation of a growing transcript — so the prompt
cache objection above doesn't apply. Two machines genuinely halve it. This is
built.

### 4. What doesn't earn its keep

**Racing the same request against two backends and taking the first reply.** It
cuts the tail of the latency distribution and burns double the compute to do it.
On shared home hardware the wasted work slows down everything else you're
running. Not worth it here.

**Sharding a single model across machines** (`--rpc` in llama.cpp, tensor
parallelism in vLLM). This makes a model *fit* that otherwise wouldn't. Over
gigabit Ethernet it is usually slower than running a smaller model that fits on
one card. Consider it only when you truly need a model that doesn't fit
anywhere, and expect single-digit tokens/sec.

## Configuration recipes

### Local default, Claude available on request

The common setup. Local is default; nothing reaches the cloud unless a
conversation explicitly asks.

```json
{
  "default": "local",
  "backends": [
    { "name": "local",  "kind": "llamacpp",  "base_url": "http://127.0.0.1:8080/v1",
      "api_key_env": "LLAMA_API_KEY", "model": "qwen3-8b" },
    { "name": "claude", "kind": "anthropic", "api_key_env": "ANTHROPIC_API_KEY",
      "model": "claude-opus-5" }
  ]
}
```

Note there is no `"fallback"`. Add `"fallback": "claude"` only if you accept
that a local outage sends transcripts off your network. The turn will tell you
it happened (`fell_back_from`), but it will have happened.

### Fast and smart on one host, via llama-swap

llama-swap serves several models behind one address, loading them on demand.

```json
{
  "default": "fast",
  "backends": [
    { "name": "fast",  "kind": "llamacpp", "base_url": "http://127.0.0.1:8080/v1",
      "api_key_env": "LLAMA_API_KEY", "model": "gemma3-4b" },
    { "name": "smart", "kind": "llamacpp", "base_url": "http://127.0.0.1:8080/v1",
      "api_key_env": "LLAMA_API_KEY", "model": "gemma3-12b" }
  ]
}
```

Read [the swap-thrash gotcha](#gotchas-that-will-cost-you-performance) before
using this one — it has a sharp edge.

### Several machines

```json
{
  "default": "workstation",
  "backends": [
    { "name": "workstation", "kind": "llamacpp", "base_url": "http://192.168.1.10:8080/v1",
      "api_key_env": "LLAMA_API_KEY", "model": "qwen3-8b" },
    { "name": "nas",         "kind": "ollama",   "base_url": "http://192.168.1.11:11434/v1",
      "model": "gemma3:4b" },
    { "name": "npu",         "kind": "hailo",    "base_url": "http://192.168.1.12:11434/v1",
      "model": "qwen2.5-1.5b" },
    { "name": "claude",      "kind": "anthropic", "api_key_env": "ANTHROPIC_API_KEY" }
  ]
}
```

Every backend is probed at startup and recorded with its health. One being down
is normal and never blocks the server; `GET /api/v1/backends` shows the state
and `POST /api/v1/backends/refresh` re-checks.

Probing then repeats every `WINTERMUTE_BACKEND_PROBE_INTERVAL` (default one
minute), because a stored status is only evidence about the moment it was
taken — the machine serving a local model can be switched off at any point
after the last probe. Set the interval to `0` to probe only on demand. A result
older than three intervals is reported as `unknown` rather than repeated, so a
prober that has stopped running shows up as lost contact instead of as lasting
good health.

### Running the server away from the GPUs

`wintermuted` never loads a model — every backend is reached over HTTP — so it
does not want a graphics card and is happiest on a small always-on box. The
inference servers stay on the machines with the hardware, and this file is what
points at them. That is the whole configuration; there is no second binary and
nothing to split.

The one thing that does not travel is the hardware probe. `nvidia-smi`,
`/proc/meminfo` and `/proc/cpuinfo` describe *this* host, and once the models
run elsewhere this host is not the one that matters.

So the catalog marks the profile: **if no non-cloud backend has a loopback
`base_url`, `runs_inference` is false**, and everything downstream reports
unknown rather than guessing —

The table below is about *this* host. A backend declared on a fleet node is
graded against that node instead — see below.

| Surface | With a local backend | Without one, and no node declared |
|---|---|---|
| `GET /api/v1/system` | the host's GPUs, VRAM, RAM | same figures, `runs_inference: false`, plus a warning naming what they are not |
| `estimate_model_fit` | a verdict | `unknown`, with the memory footprint still filled in — that is a property of the model and holds anywhere |
| `recommend_model` | ranked against free VRAM | ranked on task suitability only, and says so |
| the Models list | a fit per model | no fit |

The test is **loopback specifically**, not reachability. A backend addressed as
`localhost` is unambiguously here; one addressed by hostname or LAN IP may or
may not be, and settling that would mean matching against every local interface
with DNS in the path — to answer a question whose wrong answer is silent. A host
serving its own models through its external name therefore reads as remote, and
the symptom is fit estimates going *unknown* rather than *wrong*. Point that
backend at `localhost` to get them back.

### Getting real estimates back, across the network

The loopback rule above decides whether *this* host is evidence. It does not
have to be the only host. A machine running `wintermute-node` already reports
its GPUs, their memory and its RAM, and that is everything the fit calculator
needs — so a backend can name the machine it runs on:

```json
{
  "name": "tycho",
  "kind": "llamacpp",
  "base_url": "http://192.168.1.40:8080/v1",
  "node": "tycho"
}
```

`node` is the name the host was registered under with
`wintermuted -add-client <name> -kind node`. Getting the agent onto that host is
one command — see
[Installing the agent](hardware-nodes.md#installing-the-agent). With it declared, every verdict on
the Hub, the Repository and the Models list is computed against *that* machine's
reported hardware, and the badge names it — `fits · tycho`. A model is graded
against every declared machine and the best answer wins, which is the question
actually being asked: not "does this run here", but "does anything I own run
this, and which".

It is **declared, never inferred**. Nothing matches `base_url`'s host against a
node's hostname or address. That would put DNS and NAT in the path of a question
whose wrong answer is silent — the verdict would simply describe another
machine, and look exactly as confident doing it. Declaring a `node` on a cloud
backend is refused outright, since that hardware is nobody's to report.

Two things keep the verdict honest once the link exists:

- **A stale node is not graded.** Past five minutes without a report its free
  VRAM stops being evidence about the present, and it reads `unknown` — a box
  switched off last night still has a plausible-looking profile in the database.
- **Free VRAM arrives as a total across cards.** On a multi-GPU node the split
  is not on the wire, so all reported use is charged to the largest card. That
  under-promises rather than over-promises: a model reported as fitting and then
  failing to load is the failure worth avoiding.

With no backend declared on a node and none on loopback, nothing is graded and
every verdict reads `unknown` — the Hub screen says so in as many words, because
the alternative is a page of grey badges that looks like a broken estimator.

## Gotchas that will cost you performance

**llama-swap thrashing.** Two backends pointing at one llama-swap instance with
*different* models will swap the GPU back and forth on every alternating
request — unloading and reloading multi-gigabyte weights each time. Two
conversations, one on `fast` and one on `smart`, can spend more wall-clock time
loading models than generating. Either keep concurrent conversations on the same
model, give each model its own `llama-server` process on its own port (if VRAM
allows — on 8 GB it usually doesn't), or accept that switching is a
between-jobs operation rather than a concurrent one.

**Context is divided among slots.** `llama-server --parallel 4 --ctx-size 16384`
gives each slot 4096 tokens, not 16384. An agent transcript grows fast; a turn
that silently truncates its history behaves bizarrely. Size context per slot,
then multiply.

**Ollama serialises per model by default** and unloads after five minutes idle.
Set `OLLAMA_KEEP_ALIVE=30m`, or pay a multi-second reload on the first turn
after a coffee break. `OLLAMA_NUM_PARALLEL` controls concurrency.

**A cloud fallback fires on a *timeout*, which a slow local model can trigger.**
`WINTERMUTE_LLM_TIMEOUT` defaults to 10 minutes — generous for an API, not
necessarily generous for a 12B model doing a long tool-using turn on a
partly-offloaded card. If you configure a cloud fallback, set the timeout high
enough that "slow" is never mistaken for "broken".

**Small models fail differently, not just worse.** They lose track of tool
schemas in long transcripts and start describing tool calls in prose. Two of
them don't fix that; one better model does. Check `--jinja` first, then check
whether the model advertises the `tools` capability at all
(`GET /api/v1/models`).

## Measure before you build

Before adding hardware, find out where the time is going. A turn's response
reports the backend, the model and token counts:

```bash
curl -s -X POST http://localhost:8080/api/v1/sessions/$ID/messages \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"text":"list /srv/files/inbox and tell me what is there"}' | jq '{backend, model, usage}'
```

Then work out which of these you have:

| Symptom | Cause | What helps |
|---|---|---|
| High `prompt_tokens`, slow first token | Prompt reprocessing | Pin the session; raise context; don't alternate backends |
| Slow steady output, low GPU utilisation | Layers on CPU | Smaller model or tighter quant — not more backends |
| Fine alone, bad with two users | One GPU, two queues | A second *machine* |
| Many tool round-trips per turn | Task shape | Batch fan-out (below), or a stronger model |
| First turn slow, rest fine | Model load / swap | `OLLAMA_KEEP_ALIVE`, llama-swap `ttl` |

`nvidia-smi dmon` on the model host while a turn runs answers most of this in
about thirty seconds.

## Still to build

### Role-based routing within a turn

Give a backend one or more declared roles, and let the agent pick by role rather
than by name — a small fast model for mechanical steps inside a turn, the
conversation's own model for the reasoning.

```json
{ "name": "fast", "roles": ["summarise", "extract"], ... }
```

Cheap to implement: the router already dispatches by name, so this adds a
role→backend lookup. The payoff is modest, though, because the main loop still
has to run on the model that owns the conversation — the router already
captures the case where the work is genuinely separable.

### Deliberately not planned

**Splitting a conversation across backends** and **racing duplicate requests**.
Both are covered above; neither pays. If a turn is slow, the answer is a better
model or a faster card, not more machines.

## See also

- [docs/local-models.md](local-models.md) — building and tuning a local model
  server, including what fits in 8 GB
- [README](../README.md) — backend configuration reference and the API
