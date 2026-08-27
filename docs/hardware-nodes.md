# Hardware reporting from remote inference hosts

**Status: built, by a different route than this document proposed.** Real VRAM
fit estimates across the network work today. What follows is kept as the design
record — its reasoning is still the reasoning, and
[The invariant](#the-invariant) still holds — but read
[What was built](#what-was-built) first, because the mechanism differs.

The short version: this document proposed the server *pull* hardware from a
`/api/v1/system` endpoint on each inference host, addressed by a `node_url` in
`backends.json`. What exists instead reuses the fleet agent, which already
pushes exactly this data, and the link is a **node name** rather than a URL.

- [Installing the agent](#installing-the-agent)
- [Why this is needed](#why-this-is-needed)
- [What was built](#what-was-built)
- [What happens today](#what-happens-today)
- [The shape](#the-shape)
- [Design decisions](#design-decisions)
- [The invariant](#the-invariant)
- [Build order](#build-order)
- [Security](#security)
- [Open decisions](#open-decisions)

---

## Installing the agent

One command on the new host. The server builds the agent and serves it, so
nothing is copied by hand and no Go toolchain is needed on the node:

```bash
# on the server: name the machine, and run the line it prints on that machine
sudo scripts/add-node.sh tycho
```

`add-node.sh` issues the token, checks the agent has actually been built here
before it does, and prints the install command with the address filled in. The
two halves by hand are:

```bash
# on the server
sudo wintermuted -add-client tycho -kind node

# on the node, with the token that printed
TOKEN=wm_...

curl -fsSL -H "Authorization: Bearer $TOKEN" \
  https://wintermute.lan/api/v1/node-agent/install.sh | sudo sh -s -- --token "$TOKEN"
```

The token is assigned on its own line rather than written into the command
twice. Inline, that is one line long enough for a terminal to wrap, and pasting
a wrapped line brings the wrap back as spaces — inside the `Authorization`
header, where the server answers `401 invalid token` while working perfectly.
It is the likeliest cause of that error by some margin.

`add-node.sh` also works the address out rather than assuming it. It tries the
plain host before the port from `WINTERMUTE_ADDR`, because a reverse proxy
publishes 80 or 443 and forwards to the listener — so the listen port is exactly
the address a node cannot reach — and it confirms the choice by fetching the
installer with the token it just issued.

`-add-client` reads the database out of `/etc/wintermute/wintermute.env` and
puts its `-wal`/`-shm` files back under the service account afterwards, so it
does the right thing from a checkout and under `sudo`. It will not create a
database: a token issued into a fresh one is a token the server will reject,
and that failure shows up later, somewhere else, as `invalid token`.

The name given to `-add-client` is how the machine is identified everywhere
after this: in the Fleet view, in model assignments, and in the `node` field of
a backend declaration. It is never taken from the hostname the agent reports,
because a node that could name itself could write telemetry attributed to
another one.

The installer creates the service user, writes `/etc/wintermute/node.env` with
the token, installs the unit, and starts it. Two options are worth passing on a
host that will also hold weights:

```bash
  ... | sudo sh -s -- --token "$TOKEN" --store /srv/models --runtime llamacpp
```

`--store` also writes a systemd drop-in granting write access to that path,
which is otherwise the first thing to go wrong: `ProtectSystem=strict` hides a
store outside the state directory, and the symptom is permission denied on a
directory that is plainly writable from a shell.

### Updating a host

The install leaves a puller behind, so a later update is one command on the
node with no arguments and no token to find again:

```bash
sudo wintermute-node-update --check    # is the server holding a newer build?
sudo wintermute-node-update            # take it
```

`--check` compares the installed binary against the server's `SHA256SUMS` and
exits 0 when the node is current, 10 when an update is waiting — so a loop over
hosts can skip the ones with nothing to do. It carries no copy of the install
steps: it reads the address and token out of `/etc/wintermute/node.env`, asks
the server for `install.sh` and runs that, so a node updating always runs the
*current* installer rather than whichever one shipped with the version it is on.

Each agent reports the commit it was built from — recorded by `go build` from
the checkout rather than set by a linker flag some build path would eventually
forget. The server and the agents it hands out come off one pass over one tree,
so the Fleet view can mark a host whose build is not the one on offer. That is a
glance; `--check` compares checksums on the host itself and is the answer that
counts, since it cannot be misled by a server rebuilt without its agent.

The original `curl … | sudo sh` line still works and does the same thing. Three
things are deliberately left alone by either route:

- `node.env`, which holds the token and the choices made about this machine.
- A unit file that differs from the shipped one, since a local edit is usually
  the reason it differs. The new one is written beside it as `.service.new` and
  the installer says so.
- Anything in the model store.

### What this is not

It is not an update channel. The agent never fetches its own executable, and
nothing in the install path is reachable from its reporting loop — an operator
runs the script, the same way they would run any installer. That includes
`wintermute-node-update`: it is a convenience for the person standing at the
keyboard, not a thing the fleet does to itself. **Putting it on a systemd timer
converts it into an update channel** — a server that could replace the binary
running as a service on every node, unattended, would be root on the whole
fleet the moment it was compromised. That is a decision to take knowingly.

That is the same line [Design decisions](#design-decisions) draws around the
agent generally. The server can already make a node download a *model file* it
had permission to download; a server that could replace the binary running as a
service on every node would be a categorically different thing, and it would be
root on the whole fleet the moment the server was compromised. Updating a fleet
is therefore a loop over hosts, which is a small price for not having built that
channel.

The endpoints themselves are authenticated like everything else, serve a fixed
list of four files by exact name, and refuse to write an installer at all for a
`Host` header that would not survive being pasted into a shell script.

### On the server

`scripts/setup.sh` and `update.sh` cross-compile the agent for `linux/amd64` and
`linux/arm64` on the same pass that builds the server, into
`WINTERMUTE_NODE_AGENT_DIR`, alongside the unit file, the env template and the
puller the nodes install. A node therefore installs an agent built from the
commit the server is running, which is the only version pairing worth having.
No cgo anywhere in the tree, so both architectures are a pair of environment
variables — the same property that makes the Windows client a single build.

Unset the variable and the install endpoints report themselves as not
configured, naming the setting, rather than returning a 404 that reads like a
wrong URL.

## What was built

`cmd/wintermute-node` was already reporting each host's cards, their memory and
its RAM, on an interval, for the Fleet screen. That is everything the fit
calculator needs, so no second probe and no second endpoint were built: a
backend names the machine it runs on, and `models.HardwareFromNode` turns that
host's own reports into the same `Hardware` a local probe produces.

```json
{ "name": "tycho", "kind": "llamacpp",
  "base_url": "http://192.168.1.40:8080/v1", "node": "tycho" }
```

Four differences from the design below, each of which turned out to matter:

- **The host pushes; the server never pulls.** No `node_url`, no per-backend
  fetch, no TTL, and nothing to time out — which removes the failure mode
  [The invariant](#the-invariant) was written to guard, since there is no probe
  to fall back from. Staleness replaces unreachability: past five minutes
  without a report a node reads `unknown`, and the invariant holds in the same
  words.
- **The link is a name, not an address.** `node` matches the client the agent
  authenticates as. An address would have invited inferring the link from
  `base_url`, which is exactly the DNS-in-the-path guess this codebase refuses
  to make.
- **No new authentication.** The agent already holds a node token. The design's
  shared-token endpoint would have been a second credential guarding the same
  data.
- **Every host is graded, not one per backend.** A model is estimated against
  each declared machine and the best verdict wins, carrying the name of the
  machine that earned it — `fits · tycho`.

Free VRAM crosses the wire as a total across a host's cards, because that is
what a fleet chart plots. With one card that is exact. With several, the split
is not on the wire, and `nodeGPUs` charges all reported use to the largest card
rather than inventing a distribution — under-promising, which is the safe
direction.

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

So the answer is not wrong, it is absent — and it stays absent exactly as
described above when nothing has been declared as a machine that runs models.
Declare `node` on the backend serving them and the answer comes back, computed
from that machine's own reports. This section describes the floor, not the
ceiling.

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

**Superseded.** Steps 1-4 were delivered by the fleet agent and the declared
`node` link described above; step 5's telemetry was built first, as the Fleet
screen, and is what made the rest cheap. Kept for the record.

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

**How `recommend_model` ranks across hosts.** *Decided: rank across every
machine.* A plan recommends one model to run, so it is graded against one
host — `Catalog.PrimaryHost`, the best-equipped declared machine — which keeps
the summary paragraph's single-GPU assumption intact. The fit surfaces, which
have no such constraint, grade against all of them and name the winner.

**Whether `Fit` gaining a host field breaks the browser.** *Decided: it does
not, and `app.js` was checked rather than assumed.* Every verdict now renders
through one `fitBadge` helper, which appends the host name only when there is
one — so a single-machine server's badges read exactly as they did.

**Whether a node should report what is loaded.** It would be cheap to include,
but the catalog already gets this from the inference server itself — `/props` on
llama.cpp, `/api/ps` on Ollama. Two sources for one fact is how they start
disagreeing.

## See also

- [backends.md](backends.md#running-the-server-away-from-the-gpus) — the split
  this design completes, and what is honest about it today
- [local-models.md](local-models.md) — setting up the machine that serves models
