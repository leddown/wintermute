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

## 2. The live blocker: the deployed server is from 13 August

**This supersedes yesterday's diagnosis, which was wrong.** Initialise did not
fail because of `ProtectSystem=strict`. It failed because the endpoint is not
in the running binary:

```
$ ls -l /usr/local/bin/wintermuted
-rwxr-xr-x 1 root root 23008316 Aug 13 15:59 /usr/local/bin/wintermuted
$ strings /usr/local/bin/wintermuted | grep -cF /api/v1/repo/init
0
```

Zero for `/api/v1/repo/init`, `repo/download` and `WINTERMUTE_MODEL_REPO`. The
deployed build predates the model repository entirely, and since the browser UI
is embedded in the binary, the served JS and CSS are that old too. Everything
from `a6f72e8` onward — eight commits — is unreleased. **Nothing in this repo
can be reviewed in the running UI until `./update.sh` is run.**

That script rebuilds from the working tree and restarts the service; it was
offered and explicitly declined on 24 August, so the stale deploy is a known
state, not an oversight.

### What is still expected to bite after deploying

Both of these are unproven — they were reasoned from the unit file and the
filesystem, not observed against a build that has the feature.

1. **The unit still has no writable path outside its state directory:**

   ```
   ProtectSystem=strict
   ReadWritePaths=          <-- empty
   StateDirectory=wintermute
   ```

   So the EROFS reasoning probably does apply once the endpoint exists. The
   fix, unchanged:

   ```bash
   sudo systemctl edit wintermuted
   ```
   ```ini
   [Service]
   ReadWritePaths=/mnt/usb-drive/wintermute
   RequiresMountsFor=/mnt/usb-drive
   ```

2. **`/mnt` is empty on this host.** Whatever `WINTERMUTE_MODEL_REPO` points
   at, `/mnt/usb-drive` is not mounted, so the write has nowhere to land
   whatever the unit permits. Check `findmnt --target` before blaming systemd
   a second time.

`15f9b92` means the distinction will now be reported in the browser rather
than guessed at: EROFS names `ReadWritePaths`, EACCES names `chown`.

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

## 3b. Text brightness in Admin → Appearance

Added 24 August (`8cf3889`). A slider lifting `--text` and `--muted` towards
white, 100–175, per browser, on all four themes at once.

- `--text` and `--muted` are now **derived** in `style.css` from `--text-base`
  and `--muted-base`, which is what each palette sets. A new theme must define
  the `-base` pair, not `--text`/`--muted` directly, or the lift will not
  reach it.
- The derivation is inside `@supports (color: color-mix(...))` with a plain
  fallback before it. This is not defensive habit: a custom property that
  fails to compute does not fall back to the palette, it invalidates every
  colour depending on it, and on these backgrounds that is an unreadable page.
- Applied in `theme-init.js` before first paint, so the clamp is duplicated
  there. If the range changes, both files move.
- Verified in headless Chrome rather than by eye: at 175 the body colour goes
  from `#e6e8ec` to `#f9f9fa` and mean text-pixel brightness across the pane
  from (115,129,152) to (157,166,180); junk and out-of-range input clamp to
  100/175; the pre-paint path and the module path produce identical colours.

Not checked: how the lift reads on the Matrix and 40K palettes on a real
screen. Both mix towards white, so at the top of the range the Matrix green
pales and the 40K brass loses some of its warmth. That is the trade the
setting exists to offer, but nobody has looked at it.

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
