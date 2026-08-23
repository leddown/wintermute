-- The model repository: weights the operator keeps, as opposed to weights a
-- backend happens to be serving.
--
-- Everything in 0003 and 0016 is about models *some backend reports*. This is
-- about files on a disk — an external drive on the server — which exist whether
-- or not anything is currently serving them, and which survive a backend being
-- switched off, reinstalled or replaced.
--
-- The file on disk is the source of truth, not this table. A listing walks the
-- repository and reconciles what it finds against these rows, so a GGUF copied
-- in by hand appears without ceremony and a row whose file has vanished is
-- reported as missing rather than silently believed. What the table adds is the
-- provenance the filesystem cannot hold: which Hugging Face repository a file
-- came from, what its digest was when it landed, and when.

-- One row per file, keyed by its path *relative to the repository root*.
--
-- Relative, not absolute, and that is the whole point. The repository is a USB
-- drive: it mounts at /mnt/models on one boot and /media/l3d/… on the next, and
-- an absolute key would turn every remount into a repository full of missing
-- files and orphan rows. The root is configuration; only the path within it is
-- identity.
CREATE TABLE IF NOT EXISTS model_repo_files (
    rel_path    TEXT PRIMARY KEY,
    -- Where it came from. Empty for a file the operator copied in themselves,
    -- which is a normal state and not a defect — it just means this server
    -- cannot say anything about its origin.
    hub_id      TEXT NOT NULL DEFAULT '',
    source_url  TEXT NOT NULL DEFAULT '',
    -- What the file claims to be. Recorded at download time from the Hub's
    -- parsed GGUF header, which is authoritative, rather than re-derived from
    -- the filename on every listing, which is a heuristic.
    quant       TEXT NOT NULL DEFAULT '',
    params_b    REAL NOT NULL DEFAULT 0,
    size_bytes  INTEGER NOT NULL DEFAULT 0,
    -- The digest as verified on arrival. Empty means unverified: Hugging Face
    -- only exposes a content hash for files stored in LFS, and claiming to have
    -- checked something that was never checked is worse than admitting it.
    sha256      TEXT NOT NULL DEFAULT '',
    added_at    TIMESTAMP NOT NULL,
    updated_at  TIMESTAMP NOT NULL
);

-- Flat labels, the third of the three things 0016_model_notes.sql kept apart.
--
-- That migration built the annotation (`model_notes`) and the alias
-- (`model_champions`) and deliberately left the tag unbuilt, on the grounds
-- that "a tag vocabulary invented before anything needs it is a vocabulary
-- nobody uses". A repository of files the operator has to sort through is the
-- thing that needs it, so it is built now — and built flat, with no key/value
-- structure, for two reasons. A key/value label such as role=coding would say
-- the same thing as a champion for the coding task, in a second place that
-- drifts from the first. And a free-form tag needs no vocabulary decided in
-- advance, which is the failure 0016 was guarding against.
--
-- The subject is a repository-relative path for a file, matching
-- model_repo_files.rel_path, and lowercased the same way store.NoteKey
-- lowercases a model id. Nothing here constrains it to one or the other: a tag
-- is a label on a name, and the name it labels is the caller's business.
--
-- No foreign key onto model_repo_files. Tags are the operator's own work and
-- should outlive a drive being unplugged, a file being moved, or a row being
-- rebuilt by a rescan — exactly the reasoning that keeps a champion pointing at
-- a model its backend cannot currently see.
CREATE TABLE IF NOT EXISTS model_tags (
    model_id   TEXT NOT NULL,
    tag        TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL,
    PRIMARY KEY (model_id, tag)
);

-- Listing every subject carrying a given tag is the query the repository filter
-- makes; the primary key already covers the other direction.
CREATE INDEX IF NOT EXISTS idx_model_tags_tag ON model_tags(tag);
