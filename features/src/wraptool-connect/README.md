# wraptool-connect (Dev Container Feature) — SUNSET

This feature's source has been retired. It is **not republished** from this
directory — there is deliberately no `devcontainer-feature.json` here, so
`devcontainer features publish ./features/src` skips this directory (warns
and continues) rather than erroring or touching the registry on its behalf.
This README exists only as a tombstone: it replaces the source this
directory used to hold, so links to it (including the `:0`/`0.1.x`
artifacts' own `documentationURL`) land somewhere useful instead of 404ing.

`wraptool-connect`'s wiring-only job was folded into the merged, wiring-only
`wraptool` feature (major `:1`). See
[Dev Containers for AI harnesses](https://pti.pages.snamellit.com/wraptool/devcontainers.html)
for the current feature and its migration notes, and the
[feature definition](https://forge.snamellit.com/pti/wraptool/src/branch/main/features/src/wraptool/devcontainer-feature.json)
for every option.

Already-published registry artifacts
(`forge.snamellit.com/pti/wraptool/wraptool-connect:0`, `0.1.x`) are
untouched and keep working for existing configs indefinitely — only the
source is retired. New setups should reference
`forge.snamellit.com/pti/wraptool/wraptool:1` instead.
