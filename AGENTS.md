# AGENTS.md

Go coding standards for agents (and humans) contributing to this repository.

## Module & Layout

- Module path: `wintermute` (see `go.mod`). Keep `go.mod`'s `go` directive in
  sync with the toolchain actually in use; don't bump it speculatively.
- Two entrypoints: `cmd/wintermuted` (server) and `cmd/wintermute` (desktop
  harness). Keep both thin — parse flags, load config, wire dependencies,
  hand off. Business logic belongs in `internal/`.
- `internal/app` is the composition root: the only package allowed to import
  every other one. It opens the store, builds the LLM provider, assembles the
  tool registry, and wires the agent into the HTTP server.
- `internal/tool` is the shared vocabulary between the two processes and must
  not import anything else from this module. Dependencies point *at* it, never
  out of it.
- The client packages (`internal/client/...`) must never import the server's
  storage or agent packages. The harness cross-compiles to a standalone
  binary; keeping that boundary is what keeps it small and cgo-free.
- Package names: short, lowercase, no underscores or stutter.

## The Wire Contract

The client sends `[]tool.Definition` verbatim on every request, and the API
decodes request bodies with `DisallowUnknownFields`. Adding, renaming, or
retagging a field in `internal/tool` therefore **changes the protocol between
the two binaries**, and an older client will start failing with
`unknown field`.

When you touch those structs:

1. Update the corresponding wire struct in `internal/api/turn.go`.
2. Keep the round-trip test in `internal/api/api_test.go` passing — it exists
   precisely because unit tests on each side independently passed while the
   two disagreed.
3. Rebuild *both* binaries before testing; a stale client binary produces
   confusing failures.

## Formatting & Style

- Code must be `gofmt`-clean. Run `gofmt -w .` — never hand-format.
- Run `go vet ./...` and fix everything it flags.
- Follow standard Go naming: `MixedCaps`/`mixedCaps`, no `snake_case`.
  Exported identifiers get doc comments starting with the identifier name;
  unexported ones only need comments when the why isn't obvious from the
  name.
- Keep functions small and single-purpose. Extract a helper when a function
  is doing two distinct things, not preemptively.

## Errors

- Return errors, don't panic, except for truly unrecoverable programmer
  errors (and even then, prefer failing loudly at startup over a runtime
  panic).
- Wrap errors with context using `fmt.Errorf("...: %w", err)` so callers
  can `errors.Is` / `errors.As` through the chain. Don't wrap if you have
  nothing useful to add.
- Check every returned error. Don't use `_` to silently discard an error
  unless it is genuinely safe to ignore.
- Use sentinel errors (`errors.New`) or typed errors when callers need to
  branch on the error; don't encode control flow in error strings.
- **A failing tool is not a failing turn.** Tool handlers report problems to
  the model via `tool.Result` with `IsError` set, so it can recover or
  explain. Reserve a returned `error` for failures the model cannot act on.
- HTTP handlers log the underlying error and return a generic message; don't
  leak internal detail to the client (see `Server.fail`).

## Concurrency

- Don't add goroutines/channels unless concurrency actually buys you
  something.
- Every goroutine you start must have a clear owner for its lifetime and
  error handling.
- Guard shared mutable state with a mutex or avoid sharing it. `tool.Registry`
  is the one shared mutable structure on the server; it is mutex-guarded, and
  per-session tool sets come from `Clone()` rather than mutating the shared
  registry.
- Run `go test -race` on anything concurrent.
- Make goroutines cancelable via `context.Context` when they do I/O or can
  run long.

## Testing

- New behavior gets a test. Use the standard `testing` package; prefer
  table-driven tests for multiple input/output cases.
- Name test functions `TestXxx`, subtests via `t.Run("description", ...)`.
- Use `t.Helper()` in test helper functions.
- **Tests must not need external services.** SQLite tests open a database
  under `t.TempDir()` (see `internal/store/store_test.go`); LLM and metadata
  provider tests use fakes and `httptest`. `go test ./...` must pass on a
  machine with no model running, no network, and no API keys.
- Avoid sleeping in tests to wait for async work; synchronize explicitly.
- Keep tests deterministic — no reliance on wall-clock time, network access,
  or test execution order.
- Path-confinement logic (`internal/client/actions/roots.go`) is security
  boundary code. Any change there needs tests for the escape attempts —
  `..` traversal, absolute paths outside a root, and symlinks pointing out.
- Unit tests on the two sides of the wire can both pass while the two
  disagree. For changes to the request/response shape, exercise the real
  binaries end to end, not just the packages.

## Dependencies

- Prefer the standard library. There are exactly two direct third-party
  dependencies: `modernc.org/sqlite` (pure-Go SQLite, so the binaries stay
  cgo-free and cross-compile) and `github.com/anthropics/anthropic-sdk-go`.
  Everything else in `go.mod` is transitive.
- **Don't introduce a dependency that requires cgo.** It would break Windows
  cross-compilation of the client.
- The model is reached through the official Anthropic Go SDK, and only from
  `internal/llm`. Don't hand-roll the Messages API wire format, and don't let
  the SDK's types leak past the `llm.Provider` interface — the client binary
  must not end up linking it.
- When adding a dependency, run `go mod tidy` and commit the resulting
  `go.mod`/`go.sum` changes together with the code that needs them.
- Don't vendor dependencies.

## Documentation

- Every exported package should have a package doc comment (`// Package x
  ...`) in one file.
- Don't write comments that restate the code. Comment the *why* — a
  non-obvious constraint, a workaround, an invariant a reader could
  otherwise violate.

## Before Considering Go Work Done

1. `gofmt -l .` — empty output.
2. `go vet ./...` — clean.
3. `go build ./...` — succeeds.
4. `go test ./...` — passes (add `-race` if concurrency was touched).
5. If the wire contract or either binary's behavior changed: rebuild both and
   run the loop for real.
