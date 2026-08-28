# Agents

An **agent** is a named configuration of the assistant: a prompt, an optional
model pin, the sources it may consult, and a library of documents uploaded to
it. Pick one when you start a chat, or name one from another application, and
the conversation can read that material and nothing else.

It is not a second assistant. The agent loop, the transcript store and the
approval model are the same ones described in the [README](../README.md); an
agent narrows what a turn can reach.

## Why

Two problems, one answer.

The first is separation. A conversation about one client's engagement should
reach that client's documents; a conversation about another client, or about
the firm's own finances, should not. Before agents every session saw every tool
and no documents at all.

The second is that a model with no access to your material answers as though
you had asked it to imagine your material. Asked *"how many Security NFRs are
focused on network segmentation?"*, it explains what it would need in order to
answer and offers to go through the list if you paste it in. It cannot know
that the list is one HTTP call away. Give the agent the catalog and the same
question gets a real answer, with references.

## Sources

| Source | Tools | What it reaches |
|---|---|---|
| `documents` | `list_documents`, `search_documents`, `read_document` | The library uploaded to this agent |
| `grc` | `grc_overview`, `grc_list_nfrs`, `grc_search`, `grc_get` | A GRC application's Security NFRs, 800-53 controls, regulation coverage, policies and risk register |
| `web` | `web_search`, `fetch_url` | Your SearXNG instance, and one page at a time |

An agent with no sources is a plain assistant. The workspace tools — tasks,
CRM, accounting — are available to every session regardless;
sources govern *knowledge*, not the firm's own tools.

The document tools are registered per session and bound to the session's agent.
There is no agent argument for a model to change, so one agent cannot read
another's library by naming it.

## Making one

In the browser UI, **Workspace → Agents → New**. Give it a name (the id is derived from it),
say what it is for, tick its sources, and optionally add instructions and a
model pin. Then upload documents to it: PDF, text, markdown, HTML, CSV, JSON or
YAML, up to 25 MiB each.

A typical set:

- **GRC** — sources `documents`, `grc`, `web`. The compliance engagement.
- **Acme Bank** — source `documents`. That client's policies and contracts, and
  nothing else.
- **Finance** — no sources, or `documents` for the accountant's letters.

Deleting an agent deletes its library. Conversations it held are kept and
become unscoped, because deleting a configuration should not delete a
transcript.

## Documents

Uploads are extracted, split at headings, and searched lexically (BM25). The
heading is scored with the body, so a section titled "Incident reporting" is
found by that phrase even when the body says "notify the authority".

PDFs need `pdftotext` (`apt install poppler-utils`). There is no pure-Go
fallback: poppler's layout mode preserves the heading structure the chunker
splits on, and a PDF parser would be this program's third dependency for a job
it would do worse. A server without it says so and asks for text or markdown.

Only the extracted text is stored, not the original bytes — nothing here serves
the file back, and keeping it would grow the database with material it never
reads.

## Configuration

All optional. An unconfigured source is not offered to the model, rather than
offered and failing when it is used.

```sh
# A GRC application's read-only knowledge API.
GRC_URL=https://grc.internal:8080
GRC_KNOWLEDGE_TOKEN=…            # that installation's KNOWLEDGE_TOKEN

# Your own SearXNG instance, for web_search and fetch_url.
SEARXNG_URL=http://192.168.1.20:8080
SEARXNG_CATEGORIES=general       # optional
SEARXNG_LANGUAGE=en              # optional
```

SearXNG must have the JSON format enabled — add `json` to `search.formats` in
its `settings.yml`, or every search returns HTTP 403. The tool says so when it
happens.

`fetch_url` refuses private address space, and the check runs in the dialer
rather than on the hostname, so a name that resolves to a public address at
validation time and to `169.254.169.254` a moment later is still refused.

An agent that declares a source this server has not configured still works: the
tools are absent, and the model is told to say so rather than quietly lacking
them.

## Connecting the GRC application

The GRC application asks its questions through wintermuted rather than growing
an agent of its own — one loop, one transcript, one place to look when a model
does something surprising.

1. Here: create an agent with the `grc` source. Give the GRC installation's
   `KNOWLEDGE_TOKEN` to this server as `GRC_KNOWLEDGE_TOKEN`, and its URL as
   `GRC_URL`.
2. There: **Settings → AI providers**, choose Wintermute, enter this server's
   URL and client token, then pick the agent from the list. Its Settings page
   and AI pages link back here for document upload.

Questions asked anywhere in that application then run against that agent, and
the same agent is available in this UI.

## Testing a backend

**Admin → Backends → Send a test question** sends one prompt to one backend and
shows the reply, the model that served it, the elapsed time and the token
counts.

It creates no session, offers no tools, and — importantly — does not fall back
to another backend. A probe tells you a backend answers HTTP; this tells you it
answers questions, which is the fact worth knowing before pointing an agent at
it. A failure is reported as a result with its timing, not as a server error.
