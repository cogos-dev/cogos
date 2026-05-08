# ui/

Top-level directory for CogOS web UI surfaces.

## Convention

Each UI surface is its own Go package. The package owns:

- The static HTML file (no build step, no external dependencies)
- An `embed.go` wrapper that embeds the HTML via `//go:embed`
- A small public API: `Bytes() []byte` and `Handler() http.Handler`

The kernel's `serve.go` imports the package and mounts `Handler()` at the
appropriate route — no handler logic lives inside `serve.go` itself.

## Surfaces

| Package | Route | Description |
|---------|-------|-------------|
| `ui/dashboard` | `GET /` | Main web dashboard |
| `ui/canvas` | `GET /canvas` | Infinite-canvas spatial interface |

## Why separate packages?

Go's `//go:embed` directive is package-local: it can only embed files in or
below the package directory. Moving HTML assets to a top-level `ui/` directory
while keeping the embed in `internal/engine/` would require relative path
traversal, which `//go:embed` does not support. Making each surface its own
package keeps the embed, the HTML, and the handler co-located and importable
from anywhere in the module.

## Out of scope

- Build pipelines (Vite, npm, etc.) — these are single-file static surfaces.
- The `cogfield` panel: consumed via HTTP from `myrgic/cogfield`, not embedded here.
- API-explorer / manifest-driven panel discovery: tracked in issue #98.
