# Wintermute FAQ

Configuration and the questions that come up when wiring this thing to real
hardware. Everything here is also in `deploy/`, which is the copy systemd
actually reads.

## The model repository

### Where do the weights live?

On one directory the server owns, named by `WINTERMUTE_MODEL_REPO`. In practice
that is the mount point of a drive with room for tens of gigabytes.

```
WINTERMUTE_MODEL_REPO=/mnt/usb-drive/wintermute
```

Unset, the Repository screen reports itself as not configured and nothing can be
downloaded or assigned.

### I set the path and downloads fail with permission denied

The shipped systemd unit runs under `ProtectSystem=strict`, which makes the
entire filesystem read-only apart from `StateDirectory`. A repository outside
`/var/lib/wintermute` needs to be named explicitly:

```
ReadWritePaths=/mnt/usb-drive/wintermute
```

This is the single most confusing failure in the whole setup, because the
directory is plainly writable from a shell and only the service cannot touch it.
If the drive is mounted by systemd, add `RequiresMountsFor=/mnt/usb-drive` too.

### Initialise says the repository is not writable

The server can see the directory but cannot write to it, and there are only two
causes — both outside this server, and the message names which one applies by
reporting the uid it runs as against the directory's owner and mode.

Either the directory is not owned by the service user:

```
sudo chown -R wintermute:wintermute /mnt/usb-drive/wintermute
```

or systemd is making it read-only. The shipped unit uses `ProtectSystem=strict`,
so anything outside `StateDirectory` has to be named explicitly:

```
ReadWritePaths=/mnt/usb-drive/wintermute
```

followed by `sudo systemctl daemon-reload` and a restart.

### Something failed and all I get is "internal error"

That is deliberate — the server never puts internal detail in an HTTP response.
The detail is kept though, and **Admin → Status** shows a *Recent errors* panel
with what the server was actually doing when it failed.

You do not usually have to go looking: when a request comes back as "internal
error", the UI fetches the recorded detail and shows that instead. The list is
in memory and bounded to the last 50, so restarting the server clears it. It is
a diagnostic aid, not an audit trail — the audit trail is muninn, in the
database.

### Why does it refuse to write until I press Initialise?

Because an unmounted drive leaves its mount point behind as an ordinary empty
directory. Without a check, a download would fill the server's root filesystem
while reporting success.

Pressing **Initialise** in Huginn → Repository writes a `.wintermute-repo` marker
onto the drive itself. No marker, no writing. Deleting the marker does not delete
any weights; it just stops the server writing there.

### Can I copy GGUF files onto the drive by hand?

Yes, and over a Samba or NFS share if that is how you reach it. The listing walks
the directory rather than trusting its index, so anything you drop in appears.

What it cannot know is where a hand-copied file came from, so it shows as
unverified and its parameter count and quantisation are inferred from the
filename and marked as estimates.

### What does "verified" mean on a model?

That the bytes were checked against a digest Hugging Face published. That is only
possible for files stored in LFS — and note that on Xet-backed repositories the
CDN's `ETag` is a Xet hash, not a content hash, so the real digest is read from
the `X-Linked-Etag` header on the first hop. Anything unverified is recorded
honestly as unverified rather than given a hash that proves nothing.

### A download was interrupted

It resumes. Bytes land in a `.part` file and only become a model once the
transfer is complete and checked, so an interrupted download can never be
mistaken for a usable one. Start the same download again and it continues from
where it stopped — across a server restart as well.

## Fleet nodes

### Nothing from my node reaches the server

Check `WINTERMUTE_METRICS_DB` is set:

```
WINTERMUTE_METRICS_DB=/var/lib/wintermute/metrics.db
```

Without it, `POST /api/v1/nodes/report` answers 503 and every agent logs a
failure. The node model stores ride on the same report, so an unset value also
means no node ever learns what it has been assigned — which looks like a broken
agent rather than a missing variable.

### Does a node need to mount the repository share?

**No.** The agent fetches weights over HTTP from the server, resumably,
authenticating with its own token. It never mounts anything, and it would ignore
a share if you gave it one.

The share is for you — dragging files onto the drive from a desktop.

### How do I add a node?

One command on the server, which prints the one to run on the machine:

```
sudo scripts/add-node.sh rig-01
```

The name is how that machine is identified everywhere. It is never taken from
the hostname the agent reports, because a node that could name itself could
write telemetry attributed to another.

The full walkthrough, with what to do when it goes wrong, is under
**Utilities → Guides → Adding a node**.

### What do I set on a node?

Nothing, to start with. The installer writes `/etc/wintermute/node.env` with the
server and the token, and leaves that file alone on every later update.

You edit it to change what the agent does beyond reporting. An Ollama host that
should also hold weights:

```
WINTERMUTE_NODE_STORE=/srv/models
WINTERMUTE_NODE_RUNTIME=ollama
```

or a llama.cpp host:

```
WINTERMUTE_NODE_STORE=/srv/models
WINTERMUTE_NODE_RUNTIME=llamacpp
WINTERMUTE_NODE_LLAMA_SWAP_CONFIG=/etc/llama-swap/wintermute.yaml
WINTERMUTE_NODE_LLAMA_SERVER_ARGS=--n-gpu-layers 99
```

Passing `--store` and `--runtime` to the installer writes those two for you, and
`--store` also grants the service write access to the path — without which the
directory is invisible to the agent however writable it looks from a shell.
Restart with `systemctl restart wintermute-node` after editing by hand.

Every setting has a command-line flag too — run `wintermute-node -h`.

### How do I update a node?

Rebuild on the server with `sudo ./update.sh`, then on the node:

```
sudo wintermute-node-update --check
sudo wintermute-node-update
```

`--check` says whether a newer build is waiting and stops. Updating replaces the
agent and restarts the service, leaving `node.env` and the model store alone.

### Can I run an agent that only reports metrics?

Yes, and that is the default. Leave `WINTERMUTE_NODE_STORE` unset and the agent
reports host state and nothing else. Older agents that predate the model store
keep working against a new server unchanged, so a fleet can be upgraded one
machine at a time.

### How do I put a model on a node?

Huginn → Fleet, then the store panel on that node's card. Assigning records intent
only — nothing is transferred and nothing connects to the node. The agent notices
on its next report and fetches for itself, usually within a minute.

### Why does the server not just push the file?

Because it cannot. Nodes sit behind NAT and get addresses from DHCP, so there is
no route to push over — which is exactly why the fleet was built around the host
dialling out in the first place.

It also keeps a property worth keeping. What a node reads back is a list of
names, not a list of actions: an assignment carries a repository-relative path, a
size and a digest, and there is no field in which a command or a local path could
arrive. The worst a compromised server can do to a node is make it download a
file it already had permission to download.

### What do the states in the store panel mean?

| State | Meaning |
|---|---|
| `held` | On the host and ready to serve. |
| `pending` | Assigned but not fetched yet. The agent gets it on its next report. |
| `fetching` | A transfer is part-way through. It resumes rather than restarting. |
| `unimported` | The file is there, but the runtime cannot serve it yet. |

### Un-assigning did not free any disk

By design. Dropping an assignment stops the node being expected to hold the
model; it never deletes anything on the host. An agent that erases weights on
instruction is a worse thing to own than a disk that fills up visibly. Free the
space on the node itself.

### Why does an Ollama node use twice the disk?

Ollama will not read a loose GGUF. It imports into its own content-addressed blob
store, so the model exists once in your store and once inside Ollama. A llama.cpp
node keeps one copy, because `llama-server` takes the path and reads the file
where it lies.

The agent says so at startup rather than leaving it to be found from a full disk.

### Does the agent run commands?

No. It collects from `/proc`, sends, and — with a store configured — fetches
files and talks to the local runtime's HTTP API. Ollama imports go through
`/api/blobs` and `/api/create` rather than by running the `ollama` binary,
because a process that shells out is one step from a process that can be made
to shell out.

### The agent rewrote my llama-swap config

It owns that file entirely and regenerates it whenever the store changes; it does
not merge. Point `WINTERMUTE_NODE_LLAMA_SWAP_CONFIG` at a file only the agent
uses, keep your hand-written models in your own config, and run llama-swap with
`--watch-config` so it reloads when the agent writes.

## Models and backends

### Which backends can load and unload on demand?

Ollama and hailo-ollama. llama.cpp serves whatever it was started with and needs
its process restarting, or llama-swap in front of it; vLLM is one model per
process; Anthropic is someone else's hardware. The UI shows a plain label rather
than a button where control is not possible.

### Loading a model is not offered to the assistant as a tool

Deliberately. Every server-side tool here is read-only by construction, which is
what lets the agent loop auto-approve them without asking anyone. Evicting a
model can take VRAM out from under a turn another conversation is mid-way
through, so giving the assistant that power is a separate decision from giving
the operator a button.

## Memory and backups

### Turning recording off mid-conversation

An off-the-record conversation writes no rows at all, and turning recording off
part-way deletes what was already written in the same transaction. The audit log
keeps recording throughout: it holds what was *done*, not what was said.

### Changing the embedder

It is pinned. Its name and vector width are written on first index and compared
at every startup; a mismatch refuses to start rather than retrieving against
another model's vector space, which fails silently. Changing it is a deliberate
`wintermuted -reindex-memory`.
