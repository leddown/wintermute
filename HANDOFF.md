# Session handoff — 2026-08-28

Working notes for picking this back up. Not project documentation: delete it
when it stops being useful, or commit it if you'd rather keep the trail.

---

## 1. Start here

**The Fleet page fault is found and fixed in the working tree, uncommitted.**
It needs a deploy to reach the browser, and it needs one confirmation.

`nodeCard()` read `s.gpu_util_percent.toFixed(0)` straight off the sample. Every
GPU field on `node.Sample` is `omitempty`, so an **idle** card — nothing
running, 0% util, nothing resident — sends a sample with those keys absent
altogether. `undefined.toFixed()` throws out of `nodeCard`, out of
`renderAdminFleet`, and takes the whole page. An idle GPU is the ordinary case,
not an edge one, which is why this reads as "the Fleet page errors on refresh"
rather than as an intermittent glitch.

Proven, not reasoned: marshalling a `node.Node` with GPU facts and a
zero-valued sample emits

```
"gpus":[{"index":0,"name":"RTX 4090",...}],"latest":{"cpu_percent":3,...}
```

— `gpu_util_percent`, `gpu_mem_used_bytes` and `gpu_mem_total_bytes` all
absent. And `s.gpu_util_percent.toFixed(0)` on that object throws
`TypeError: Cannot read properties of undefined (reading 'toFixed')`.

The fix reads the field through zero, as `bytes()` and `gauge()` on the same
three lines already did. The other four GPU reads were checked and were already
guarded; `bytes()` and `gauge()` both coerce.

**The one thing still unconfirmed** is whether this is the fault that was
reported, because the error text never arrived. It is *a* fault that breaks
exactly that page on exactly that action, but if the Fleet page still errors
after the deploy below, the console text is still the thing to get.

```bash
# on the server
git pull && sudo ./update.sh
```

### Two questions from the last session, both now answered

- **Has the server been rebuilt since `8bfe1b9`? Yes.** `curl
  http://wintermute.l3d.internal/app.js` returns 291,653 bytes, byte-identical
  to the tree at `8bfe1b9`. The UI is embedded in the binary, so a matching
  `app.js` is a matching build — and `app.js` did change in `8bfe1b9`. This is
  a cheap deploy check worth keeping: it needs no token and no ssh.
- **Is the new card code at fault? No.** `agentBuildChip()` and
  `fleetGuideLink()` were both read through. Every value they touch is
  interpolated or null-guarded, and `el()` takes varargs children and treats
  `null` as absent, so the untested `fleetGuideLink()` cannot throw at render —
  only inside its `onclick`. The bug is in older code that today's commits
  merely made newly visible.

Not done, and still worth doing: nobody has looked at the rendered page. No ssh
key to the server from coven (`Permission denied (publickey,password)`), and no
client token on coven either — `~/.config/wintermute/` does not exist — so the
four-endpoint curl check below has not been run.

```bash
TOKEN=wm_...
for p in /api/v1/nodes /api/v1/nodes/assignments /api/v1/models /api/v1/repo; do
  printf '%-32s ' "$p"
  curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $TOKEN" \
    http://wintermute.l3d.internal$p
done
```

Only `/api/v1/nodes` can break the render: it is fetched **without** a
`.catch()` on purpose — it is the point of the screen — while the other three
fall back to empty results. So a failure there takes the whole page and a
failure elsewhere is silent.

## 2. Where the code stands

`main` is at **`8bfe1b9`**, pushed. The tree carries **two uncommitted
changes**: this file, and the one-line GPU guard in `nodeCard()` described
above. Six commits landed on 27 August:

| Commit | What |
|---|---|
| `12613f4` | Tasks and Scratch merged under one **Workspace** tab |
| `61f9102` | `wintermute-node-update`: the node pulls its own builds |
| `ba0ff9e` | `add-node.sh`, and `-add-client` stops writing to the wrong database |
| `30fbdd4` | Utilities → Guides → Adding a node |
| `fa359fd` | Token on its own line; the install address is checked, not guessed |
| `8bfe1b9` | Real agent build identity, shown on the node card (committed by hand) |

`go build`, `go vet`, `gofmt -l .`, `go test ./...`, `shellcheck` on all five
scripts and `node --check` were clean at `8bfe1b9`.

## 3. The two failures this took to find, and what closed them

Both were diagnosed live against the real server, and both are worth
recognising again because they produce identical symptoms to real faults.

**A 401 that was not a bad token.** A 46-character token written into the curl
line twice makes one line long enough for a terminal to wrap. Pasting a wrapped
line brings the wrap back as literal spaces — inside the `Authorization`
header. The server answers `401 invalid token` while working perfectly. Every
place that prints the command now assigns `TOKEN=` on its own line first.

**A connection refused that was not a wrong host.** `add-node.sh` guessed the
port from `WINTERMUTE_ADDR`, which is what wintermuted *binds*. With nginx in
front, that is precisely the port a node cannot reach. It now tries the plain
host first and confirms the choice by fetching the installer with the token it
just issued.

To tell a mangled token from a genuinely wrong one:
`curl -s -H "Authorization: Bearer $TOKEN" http://wintermute.l3d.internal/api/v1/me`

## 4. Adding coven to the fleet — where it got to

Not finished. The last known state:

- The server had **no `WINTERMUTE_NODE_AGENT_DIR`**; the user was editing
  `/etc/wintermute/wintermute.env` to add it. Whether `update.sh` has run since
  is unknown — it is what builds the agent binaries into that directory.
- A valid node token for `coven` was issued and confirmed working
  (`/api/v1/me` → `{"kind":"node","name":"coven"}`). It has been pasted into
  two conversations, so rotate it: `sudo scripts/clients.sh revoke coven`.
- The install command itself was never successfully run on coven.

The whole path, once the server is rebuilt:

```bash
# on the server
sudo scripts/add-node.sh coven --server http://wintermute.l3d.internal
# then the two lines it prints, on coven
```

## 5. Environment notes

Read these before assuming which machine anything is on — half of today went on
getting this wrong.

| | |
|---|---|
| **coven** — `10.232.231.8`, `coven.l3d.local` | The dev box, and the node being added. Checkout at `~/go/wintermute`. Ollama on `:11434`, postgres on `:5432`. |
| **wintermute** — `wintermute.l3d.internal`, `10.232.231.9` | The server. Checkout at `~/code/wintermute`. **nginx 1.24.0 on port 80** in front of wintermuted. |

- **Use `http://wintermute.l3d.internal` with no port.** `:8088` is what
  wintermuted binds behind the proxy; `:8080` is llama.cpp and is not running.
- nginx **does** forward `Authorization` — proven, not assumed: a bad token
  returns wintermuted's own `{"error":"invalid token"}` rather than
  `missing bearer token`.
- **coven runs its own `wintermuted` on `0.0.0.0:80`**, active and enabled,
  logging a failed backend probe every 60s. It looks like a leftover and it is
  what made coven look like the server for most of a session. If coven is only
  ever a node: `sudo systemctl disable --now wintermuted`.
- The service unit is **`wintermuted.service`**, not `wintermute.service`.
- The server's database is `/var/lib/wintermute/wintermute.db`. The
  `wintermute.db` in the coven checkout is a stray with **no clients in it**.
- Deploying is `git pull && sudo ./update.sh` on the server. It restarts the
  service, which is also what makes an env-file change take effect.
- Throwaway servers were run on `127.0.0.1:8199` with `ANTHROPIC_API_KEY=dummy`
  and scratch `WINTERMUTE_DB` / `WINTERMUTE_METRICS_DB`. Add
  `WINTERMUTE_NODE_AGENT_DIR` if the install endpoints are involved, or they
  answer 503.

## 6. Verification worth repeating after changes

```bash
go build ./... && go vet ./... && gofmt -l . && go test ./...
GOOS=windows GOARCH=amd64 go build -o /dev/null ./cmd/wintermute   # client must cross-compile
node --check internal/web/static/app.js                            # no JS test runner here
shellcheck scripts/*.sh update.sh && shellcheck -s sh deploy/wintermute-node-update.sh
```

There is no browser here — the Chrome extension was declined this session — so
**every UI change today is unverified visually.** That includes the Workspace
tab strip, the Scratch pad, the guide page and the node card. The markdown
documents were at least rendered through the real `renderMarkdown` in node with
a stub DOM, which is worth repeating for any doc change: it catches tables and
lists that silently fall through to paragraphs.

`GOOS=windows go build ./...` fails on `internal/modelrepo` and
`internal/utilities` — pre-existing and expected. Only `./cmd/wintermute` is
required to cross-compile.
