# Quickstart

From a clean checkout to an assistant that renames a file, in about twenty
minutes. This uses Ollama because it's one command; swap in llama.cpp later
using [local-models.md](local-models.md), which is where the performance is.

There are three machines in this story, and they're often the same machine:

| Role | What runs there |
|---|---|
| **Model host** | Ollama or llama-server, holding the weights and the GPU |
| **Server host** | `wintermuted` — the transcript, the tools, the UI |
| **Desktop** | `wintermute` — the harness, with access to your files |

---

## 1. Get a model answering

On the model host:

```bash
curl -fsSL https://ollama.com/install.sh | sh
ollama pull qwen3:8b
ollama run qwen3:8b "reply with the single word: ready"
```

If that printed anything, you have a model. Confirm it's on the GPU:

```bash
ollama ps        # PROCESSOR must say 100% GPU
```

If it says CPU, it still works — just slowly. [local-models.md](local-models.md)
explains what to do about it.

If the model host isn't the server host, expose it on the LAN (and read
[step 7 of local-models.md](local-models.md#7-network-exposure-and-security)
before you do — Ollama has no authentication of its own):

```bash
sudo systemctl edit ollama       # Environment="OLLAMA_HOST=0.0.0.0:11434"
sudo systemctl restart ollama
```

## 2. Build wintermute

On the server host:

```bash
git clone <your-clone-url> wintermute && cd wintermute
go build -o wintermuted ./cmd/wintermuted
go build -o wintermute  ./cmd/wintermute
```

## 3. Point it at the model

Create `backends.json` next to the binary:

```json
{
  "default": "local",
  "backends": [
    {
      "name": "local",
      "kind": "ollama",
      "base_url": "http://127.0.0.1:11434/v1",
      "model": "qwen3:8b"
    }
  ]
}
```

Change `127.0.0.1` to the model host's address if it's a different machine. Note
the `/v1` — it's Ollama's OpenAI-compatible endpoint, and leaving it off is the
most common setup mistake.

Then create `.env` for everything else.

```bash
# .env
WINTERMUTE_ADDR=:8080
WINTERMUTE_DB=wintermute.db
```

## 4. Create a token and start the server

```bash
./wintermuted -add-client desktop
# prints: wm_… — copy it, it is shown once and stored only as a hash

./wintermuted
```

Check it came up healthy:

```bash
curl -s localhost:8080/api/v1/health
curl -s localhost:8080/api/v1/backends -H "Authorization: Bearer $TOKEN" | jq
```

The backend should report `"status": "ok"`. If it says `unreachable`, the
`base_url` is wrong or Ollama isn't running — those are the only two causes
worth checking first.

While you're here, see what the server thinks of your hardware:

```bash
curl -s localhost:8080/api/v1/system  -H "Authorization: Bearer $TOKEN" | jq
curl -s "localhost:8080/api/v1/models?context=8192" -H "Authorization: Bearer $TOKEN" | jq
```

## 5. Set up the desktop harness

On the desktop — the machine that can see the files you want to work on:

```bash
./wintermute -init          # writes ~/.config/wintermute/config.json, mode 0600
```

Edit it:

```json
{
  "server_url": "http://server-host:8080",
  "token": "wm_…",
  "roots": ["/srv/files"],
  "auto_approve_reads": true,
  "always_allow": [],
  "never_allow": []
}
```

`roots` is the whole of the client's authority. Nothing outside it is reachable,
whatever the model asks for — so start it narrow. One directory of test files is
a good first root.

## 6. Rename something

```bash
./wintermute
```

Then, at the prompt:

```
> list /srv/files/inbox and tell me what's there
```

Reads are auto-approved, so that runs immediately. Now ask for something that
changes a file:

```
> rename report-final-FINAL-v2.pdf to something sensible
```

You'll get a prompt per rename:

```
rename_file  /srv/files/inbox/report-final-FINAL-v2.pdf → 2026-08 Quarterly Report.pdf
[y] yes  [n] no  [a] always  [q] quit turn
```

Answer `n` on the first one deliberately, and watch what happens: the model is
told it was declined and adjusts. That's the property that makes the rest of it
trustworthy — a refusal is reported, not silently swallowed.

## 7. Confirm the audit trail

Every proposal is recorded, approved or not:

```bash
curl -s localhost:8080/api/v1/sessions/$ID/audit \
  -H "Authorization: Bearer $TOKEN" | jq '.entries[] | {tool_name, decision, outcome, is_error}'
```

The transcript is a conversation and can be discarded. This is the record.

---

## Where to go next

**It works but it's slow.** [local-models.md](local-models.md) — build
llama.cpp for your card, pick the right quantisation, quantise the KV cache.
Expect a large multiple over a default Ollama setup on older hardware.

**Tool calls don't fire.** The model describes calling a tool instead of calling
it. On llama.cpp that's a missing `--jinja`; on any backend it can be a model
that was never trained for tool use. Check
`GET /api/v1/models` for the `tools` capability.

**You want a stronger model for hard conversations.** Add an `anthropic`
backend and pin individual sessions to it —
[README](../README.md#choosing-a-model-per-conversation). Local stays the
default; nothing leaves your network unless a conversation asks it to.

**You have more than one machine.** [backends.md](backends.md).

**You want the browser UI.** `./wintermuted -add-client browser -kind browser`,
then open `http://server-host:8080` and paste the token. The browser has no
local action set — it can chat and use server-side lookups, but it can't touch
your disks. That takes the harness.
