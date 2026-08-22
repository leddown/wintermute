-- What the operator thinks of a model, as opposed to what it reports about
-- itself.
--
-- The catalog in 0003 records what each backend says it has: id, family,
-- parameter count, quantisation, context length, whether it is resident. All of
-- it is probed, and Catalog.Refresh rewrites those rows on every sweep — it is
-- a cache of an external truth, not a place to keep an opinion.
--
-- Judgements are the opposite. "This is the one that actually writes decent Go"
-- is learned slowly, across weeks of use, and would be destroyed by the next
-- probe if it lived in the catalog. So it lives here, in tables nothing
-- refreshes, keyed by model id rather than by catalog row.
--
-- The shape follows the split model registries settled on years ago, because it
-- keeps three genuinely different things apart:
--
--   * an annotation — free prose about a model, which is `model_notes`;
--   * an alias — a moving pointer such as "champion", which is
--     `model_champions`, one model per task;
--   * a tag — a key/value label. Not built yet. Champions cover both of the
--     things actually asked for, and a tag vocabulary invented before anything
--     needs it is a vocabulary nobody uses.

-- One note per model.
--
-- The key is the model id lowercased, not a normalised name. Normalising across
-- engines is tempting — Ollama's `qwen3:8b` and vLLM's `Qwen3-8B-Instruct` are
-- the same weights — but every rule that merges them is a heuristic, and a
-- wrong merge silently attaches a judgement to a different model, which is a
-- worse failure than keeping two notes. Lowercasing alone still does the useful
-- thing: the same model served by four Ollama hosts shares one id, so it shares
-- one note.
CREATE TABLE IF NOT EXISTS model_notes (
    model_id   TEXT PRIMARY KEY,
    note       TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMP NOT NULL
);

-- The champion for each task: the one model the operator would reach for.
--
-- Keyed by task rather than by model, which is what makes the pointer move.
-- Naming a new champion for "coding" replaces the previous one in a single
-- statement, so there is never a moment with two, and never a stale flag left
-- behind on a model that has been superseded.
--
-- The task vocabulary is models.AllTasks — general, agent, documents, coding,
-- long_context, vision, reasoning, embedding — the same eight classes the
-- planner and the seed catalog already use. Reusing it means a champion can be
-- compared against the curated score for the same task, rather than being an
-- opinion floating free of everything else the server knows.
--
-- No foreign key onto the catalog. A champion should survive its backend being
-- switched off for a week, and pointing at a model the catalog cannot currently
-- see is a normal state worth displaying, not an integrity error.
CREATE TABLE IF NOT EXISTS model_champions (
    task       TEXT PRIMARY KEY,
    model_id   TEXT NOT NULL,
    updated_at TIMESTAMP NOT NULL
);
