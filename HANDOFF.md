# Session handoff — 2026-08-24

Working notes for picking this back up. Not project documentation: delete it
when it stops being useful, or commit it if you'd rather keep the trail.

---

## 1. Where the code stands

`main` is at **`6dd10aa`**, **not pushed** — three commits ahead of `origin/main`.
The tree is clean apart from this file.

| Commit | What |
|---|---|
| `15f9b92` | EROFS told apart from EACCES in `writeFailure()` (yesterday's uncommitted work) |
| `c69e4fa` | Split GGUFs fetched whole, or refused |
| `6dd10aa` | Hugging Face search asks for the facts the cards were missing |

`go build`, `go vet`, `gofmt -l .`, `go test ./...`, the Windows cross-compile
and `node --check` are all clean.

Verified against the live Hub and a throwaway server on 127.0.0.1:18099, not
just against tests:

- 17 quantisations now listed for `unsloth/Qwen3-Coder-30B-A3B-Instruct-GGUF`
  where the old dedupe showed about 13.
- `BF16` reports 61.10 GB across its two shards; it previously offered shard
  one alone at 49 GB and called it the model.
- A 15-result search: 143 KB from the Hub, **14.5 KB** onward to the browser
  once the chat templates are dropped.

**Not yet done: nobody has looked at the rendered page.** The JavaScript is
syntax-checked and the JSON behind it was read field by field, but the card
layout itself is unreviewed in a browser. That is the first thing to do.

## 2. The live blocker: Initialise fails on the server

**Symptom:** Admin → Repository → Initialise returns `internal error`.

**Diagnosed cause:** not permissions. The directory is already
`drwxr-xr-x wintermute` — the service user owns it with `rwx`. The unit has:

```
User=wintermute
ProtectSystem=strict
ReadWritePaths=          <-- empty
```

`ProtectSystem=strict` remounts everything outside `StateDirectory` read-only
*for the process*, so the write fails no matter who owns the directory. Adding
the user to a group will not help.

**Fix, on the server:**

```bash
sudo systemctl edit wintermuted
```
```ini
[Service]
ReadWritePaths=/mnt/usb-drive/wintermute
RequiresMountsFor=/mnt/usb-drive
```
```bash
sudo systemctl daemon-reload
sudo systemctl restart wintermuted
```

**Still to verify on the actual server** (it is *not* this machine —
`/mnt/usb-drive` does not exist here and `/mnt` is empty):

```bash
systemctl show -p ReadWritePaths -p ProtectSystem wintermuted
findmnt --target /mnt/usb-drive            # mounted? read-only? CIFS?
strings /usr/local/bin/wintermuted | grep -c wintermute-repo
```

That last one matters: the binary installed on *this* box returns **0**, i.e.
it predates the repository feature entirely. If the server is running a build
that old, `/api/v1/repo/init` does not exist there and the whole diagnosis is
moot — deploy a current build first.

If the drive turns out to be a **CIFS/SMB mount** rather than a local disk that
Samba exports, group membership and `chown` both do nothing: permissions on
CIFS are synthetic and set at mount time via `uid=`, `gid=`, `file_mode=`,
`dir_mode=` in `/etc/fstab`.

---

## 3. The Hugging Face search work — done, with notes

Both complaints are addressed. `expand[]` on the search endpoint carries the
real parameter count, architecture, context length, tool support, licence,
base model, all-time downloads and quantisation count; results are graded for
fit on the way past; the download is a `Download…` button rather than a text
link called `Files`. Sizes come from `?blobs=true` on the detail request.

Findings from yesterday's research that are now settled and need no revisiting:
`expand[]=gguf` works on search, `usedStorage` is not expandable, and the Hub
returns the full valid list in the body of a 400 if a bad one is passed.

Three things worth knowing about what landed:

- **The split-file fix went in separately** (`c69e4fa`), as a correctness bug
  rather than a display one. It also fixed a second case of the same mistake:
  `Q2_K_L` and `UD-Q2_K_XL` both infer to the label `Q2_K`, and deduping on
  the label was dropping all but the first. Grouping is now by file name.
- **A consequence of that:** several rows can share a label, so each quant row
  shows its file name as well. `inferQuant` still does not know `_L`, `_XL` or
  `UD-` variants, and they are graded for fit at the base label's bits per
  weight — a small under-estimate, not a wrong one. Teaching `quantBPW` those
  variants would be the honest fix if it ever matters.
- **A shard set is several jobs, started together.** The job registry is
  per-file and that was left alone. The progress panel therefore shows two
  bars for one BF16 download, which is accurate but may read oddly.

## 4. Other things left open

- **Ollama ingest on a node is untested against a real Ollama.** The
  `llamacpp` path was verified end to end against a live agent — fetch, config
  generation, inventory reporting. The Ollama path (`/api/blobs` push then
  `/api/create`) is covered by unit tests and the API docs only. First thing to
  watch when a real Ollama node is pointed at it.
- **llama-swap `-watch-config` reload over NFS is unverified.** Irrelevant to
  the current design — the agent writes to local disk, where inotify works —
  but it is why config generation happens on the node rather than the server
  writing one config onto a share.
- The user does not care about the **Ollama double-disk cost**, so the
  Ollama-registry idea is closed. The agent still prints a one-line note about
  it at startup; drop that if it becomes noise.

---

## 5. Environment notes

- **Do not `pkill -f wintermuted`.** There is a production server running on
  this machine as PID **1361** (`/usr/local/bin/wintermuted`, user
  `wintermute`, since 08:53). A broad pkill matches it. Kill test servers by
  exact PID, or match on the scratchpad path:

  ```bash
  pgrep -f wintermuted | while read p; do
    case "$(readlink /proc/$p/exe)" in "$SCRATCH"*) kill "$p";; esac
  done
  ```

- Test servers were run on **127.0.0.1:18099** to stay clear of anything real.
- Scratchpad paths are session-specific; assume any named here is gone.
- The production server now runs as **PIDs 1347 and 1361** (they change on
  restart — check `readlink /proc/<pid>/exe` and the owning user before
  killing anything).
- Starting a throwaway server needs `WINTERMUTE_DB`, `WINTERMUTE_ADDR`,
  `WINTERMUTE_LLM_PROVIDER=ollama`, `WINTERMUTE_LLM_BASE_URL`,
  `WINTERMUTE_LLM_MODEL`, and `WINTERMUTE_METRICS_DB` if the fleet is involved.

---

## 6. Verification worth repeating after changes

```bash
go build ./... && go vet ./... && gofmt -l . && go test ./...
GOOS=windows GOARCH=amd64 go build -o /dev/null ./cmd/wintermute   # client must cross-compile
node --check internal/web/static/app.js                            # no JS test runner here
```

The Repository and fleet work was verified against a real server and a real
agent, not just tests — a genuine Hugging Face download (sha256 matched the
published digest byte for byte) and a real node fetch (byte-identical, exactly
one fetch across ~15 reports). Worth doing again for anything that touches
those paths; the Xet digest bug in `a6f72e8` was invisible to unit tests and
only showed up against the live API.
