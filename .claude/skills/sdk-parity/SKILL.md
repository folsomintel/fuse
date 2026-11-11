---
name: sdk-parity
description: Land an api-surface change across all four fuse sdks (go, typescript, python, rust) with a test in each, plus openapi and the sdk reference docs. Use when adding or changing a request/response field, endpoint, error code or sdk method, when one sdk got a change the others didn't, or when asked to "check sdk parity".
---

Repo rule (CLAUDE.md): the four sdks move together. A change to one sdk's api surface lands in all four in the same PR, with a test in each. A method missing in another sdk is a gap to close, not a reason to skip.

## where things live

| layer | path |
|---|---|
| wire structs | `internal/api/types.go` (+ `exec.go`, `computer.go`, `interventions.go`) |
| spec | `internal/api/openapi.yaml` |
| errors | `internal/api/errors.go` (codes), sdk error types below |
| go sdk | `sdks/go/types.go`, `sdks/go/<area>.go`, tests `sdks/go/*_test.go` |
| ts sdk | `sdks/typescript/src/types.ts`, `src/<area>.ts`, `src/index.ts` (exports), tests next to src (vitest) |
| python sdk | `sdks/python/src/fuse/types.py`, `src/fuse/_services/`, `src/fuse/__init__.py` (exports), tests `sdks/python/tests/` |
| rust sdk | `sdks/rust/src/types.rs`, `src/<area>.rs`, `src/lib.rs` (exports), tests `sdks/rust/tests/` |
| reference docs | `docs/content/reference/{go,typescript,python}/*.mdx` (no rust pages yet), `docs/content/reference/api/` |

## steps

1. **List the surface delta** from `git diff origin/main...HEAD -- internal/api`: every new/renamed json field (with optional/required and type), endpoint, status code and error code.
2. **Build a matrix** with one row per item and columns go / ts / py / rust / openapi / docs. Fill it by reading each sdk, not by assuming. Grep the json name in each sdk (`grep -rn '"field_name"\|field_name' sdks/`).
3. **Close every gap.** Match each sdk's existing idiom rather than copying go:
   - go: pointer for optional, `omitempty` tags
   - ts: optional `?:` in `types.ts`, export from `index.ts`
   - python: typed dataclass/TypedDict in `types.py`, export from `__init__.py`
   - rust: `Option<T>` with `#[serde(skip_serializing_if = "Option::is_none")]`. adding a pub field to a struct people build with literals breaks them, so say so in the PR (precedent: `ExecRequest`, `ComputerAction`)
   - behavior (timeouts, 202 waits, retries, error mapping) needs parity as much as types do (b9bc805 fixed the exec timeout in go only)
4. **Tests, one per sdk, at minimum:** request serialization includes the new field, and the response decodes it. Use each sdk's existing mock-transport tests as the template.
5. **Openapi:** same field names, the same required list, and enums match the go constants.
6. **Docs:** update the reference page for each sdk that documents that area, and the api reference. If an sdk lacks a capability for a structural reason (go-only stream/upgrade support), say so in the docs and the PR's "Choices worth a look" section. Never leave it out silently.
7. **Run the four suites** (the CI commands, using bun locally for ts):
   ```bash
   (cd sdks/go && GOWORK=off go build ./... && go vet ./... && go test ./... -race -count=1)
   (cd sdks/typescript && bun install && bun run lint && bun run typecheck && bun run test && bun run build)
   (cd sdks/python && uv sync && uv run ruff check src tests && uv run mypy && uv run pytest)
   (cd sdks/rust && cargo fmt --check && cargo clippy --all-targets -- -D warnings && cargo test)
   ```
8. Show the finished matrix in your reply, and summarize it in the PR body under **What changed** ("all four SDKs, with a test in each").
