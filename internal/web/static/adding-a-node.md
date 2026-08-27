# Adding a machine to the fleet

A **node** is any Linux machine on your network that reports what it is doing
back to this server: its CPU, its memory, its graphics cards and how full they
are. Once a machine is reporting, the Fleet screen shows it, and this server can
work out whether a given model will actually fit on it rather than guessing from
its own hardware.

Adding one takes two commands. One here, on the server. One on the machine
being added.

## Before you start

You need three things.

- **The machine runs Linux and uses systemd.** The agent reads `/proc`, so it is
  Linux-only, and it installs itself as a systemd service.
- **Shell access to it, with `sudo`.** The install writes to `/usr/local/bin`
  and `/etc`, and starts a service.
- **The machine can reach this server.** It makes the connection, always — this
  server never reaches into a machine uninvited. So the node needs a route to
  `{{server}}`, but this server needs no route to the node, and the node can sit
  behind NAT on a DHCP address without anything special.

You do **not** need a Go toolchain on the node, or to copy any files by hand.
The server builds the agent and hands it over.

## Step 1 — check the agent has been built here

On the server:

```
grep NODE_AGENT /etc/wintermute/wintermute.env
ls /var/lib/wintermute/node-agent/
```

You want `WINTERMUTE_NODE_AGENT_DIR` to be set, and that directory to hold five
files: the agent for both architectures, the service unit, the env template and
a `SHA256SUMS`.

If either is missing, add the setting to the env file and rebuild:

```
WINTERMUTE_NODE_AGENT_DIR=/var/lib/wintermute/node-agent
```

```
sudo ./update.sh
```

`update.sh` cross-compiles the agent for `linux/amd64` and `linux/arm64` on the
same pass that builds the server, so a node always installs an agent built from
the same commit as the server it reports to. Skip this step and step 3 fails on
the far machine with a **503**.

## Step 2 — register the machine, here

Still on the server, in the checkout:

```
sudo scripts/add-node.sh tycho
```

Use whatever name you want the machine known by. That name is how it is
identified everywhere afterwards: on the Fleet screen, in model assignments, and
in the `node` field of a backend declaration. It is never taken from the
hostname the machine reports — a node that could name itself could file
telemetry under another one's name.

The script checks step 1 for you before it issues anything, then prints the
exact command to run on the node, with the address and the token already filled
in. Copy that command.

If the address it guesses is not one the node can reach, say so:

```
sudo scripts/add-node.sh tycho --server {{server}}
```

And if the machine will also hold model weights:

```
sudo scripts/add-node.sh tycho --store /srv/models --runtime ollama
```

`--store` matters more than it looks: it also grants the service write access to
that path. Without it the directory is invisible to the agent even though it is
plainly writable from a shell.

**The command it prints contains a credential.** The token is shown once and
kept here only as a hash. If it gets away from you, revoke it and start again
with `sudo scripts/clients.sh revoke tycho`.

## Step 3 — install the agent, on the node

Paste the command from step 2. It looks like this:

```
curl -fsSL -H "Authorization: Bearer wm_..." \
  {{server}}/api/v1/node-agent/install.sh \
  | sudo sh -s -- --token "wm_..."
```

The installer creates a service user, downloads the agent for that machine's
architecture, checks it against the server's checksums, writes
`/etc/wintermute/node.env`, installs the systemd unit and starts it.

## Step 4 — check it worked

On the node:

```
systemctl status wintermute-node
journalctl -u wintermute-node -f
```

Then open **Fleet** here. The machine should appear within a minute or so — the
agent takes a reading every 15 seconds and delivers a batch every minute.

## Step 5 — if the machine serves models

Reporting hardware and serving models are separate things. If the new machine
also runs Ollama or llama.cpp, name it in that backend's declaration so fit
verdicts are worked out against its cards rather than this server's:

```
{ "name": "tycho", "kind": "ollama", "base_url": "http://tycho.lan:11434", "node": "tycho" }
```

## Updating a node later

The install leaves a puller behind, so there is nothing to reconstruct:

```
sudo wintermute-node-update --check
sudo wintermute-node-update
```

`--check` says whether this server is holding a newer build and stops. It exits
0 when the node is current and 10 when an update is waiting, so a loop over
several hosts can skip the ones with nothing to do.

Updating replaces the agent and restarts the service. It leaves `node.env`
alone, so the token and that machine's settings survive, and it leaves anything
in the model store alone.

Rebuild here first — `sudo ./update.sh` — or the nodes will pull the same
version they already have.

## When it goes wrong

| What you see | What it means | What to do |
| --- | --- | --- |
| `Connection refused` | Nothing is listening at that address. A wrong port is the usual reason. | Check the address, including the port. This server is at `{{server}}`. |
| `401` with `invalid token` | The token is not in the database this server reads. | Issue a new one with step 2. Do not run `wintermuted -add-client` from a checkout with `WINTERMUTE_DB` unset — see below. |
| `401` with `missing bearer token` | The header did not arrive. Something between the node and here is stripping it. | Check any reverse proxy in front of this server forwards `Authorization`. |
| `503` about the agent distribution | The server has no agent to hand over. | Step 1. |
| `404` naming a file | The distribution directory is incomplete. | Run `sudo ./update.sh` here. |
| Permission denied on the model store | `ProtectSystem=strict` hides paths outside the state directory. | Re-run the installer with `--store /your/path`. |
| The node never appears on Fleet | The agent is running but cannot deliver. | `journalctl -u wintermute-node -f` on the node. |

### About that `invalid token`

`wintermuted -add-client` used to default to a **relative** `wintermute.db`. Run
from a checkout, it wrote a second database in that directory and registered the
client there — a real token, in a file the server never opens. The only symptom
was `invalid token` arriving later from somewhere else.

It now reads the database out of `/etc/wintermute/wintermute.env`, says which
one it used, and refuses to create a new one. If you are on an older build, this
is almost certainly what happened.

## What the agent does, and does not do

It reports. That is all it does. It cannot be told to run anything, and there is
no command channel from this server to a node — a fleet of agents that execute
what they are told is a fleet of remote shells. Loading and unloading models is
done through the backend's own API instead, which needs no access to the host at
all.

The agent also never fetches its own executable. `wintermute-node-update` is run
by a person standing at the keyboard; nothing in the agent's reporting loop can
reach it. **Putting it on a timer changes that** — it would make this server
able to replace the binary running as a service on every machine in the fleet,
unattended, and therefore root on all of them the moment this server was
compromised. That is a real decision, not a convenience. Take it knowingly.

Telemetry is kept in its own database, apart from your conversations. Raw
readings live two hours and are then folded into minute, hour and day summaries.
