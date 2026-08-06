# Design: opt-in documentation tooling for development containers

Status: **proposal / future feature**

Scope: make heavyweight documentation tools such as Quarto available when a
developer or coding harness is working on documentation, without installing
them in every development container or the GitLab lint image. Cover both Dev
Container selection and the related Guix/toolchain composition problem. The
publication destinations and deployment mechanisms stay unchanged, but the
proposal does tighten how renderer versions are supplied to those workflows.

## 1. Problem

The repository currently treats `manifest.scm` as one flat package set for
local development, the Guix `wraptool up` container, lint, and the GitLab CI
image. That is intentionally simple and avoids drift, but it assumes every tool
is useful in every environment.

Quarto breaks that assumption:

- It is needed to preview and render `docs/`.
- It is not needed to build, test, lint, or run wraptool.
- The coding harness does not use Quarto unless its current task changes
  documentation and needs to validate the rendered site.
- GitLab's lint job does not use Quarto. Its documentation jobs already use a
  separate Quarto image.
- Forgejo's documentation workflow already requests `quarto-bin` separately.

The size impact is material. The last successfully published Guix CI image,
before Quarto entered the manifest, is
[469 MiB in Forgejo](https://forge.snamellit.com/pti/-/packages/container/wraptool%2Fci-toolchain/latest).
Quarto 1.9.38's
compressed Linux archive alone is 136,466,558 bytes and bundles Pandoc, Deno,
Dart Sass, Typst-related tooling, and other assets. Guix then includes the
package's runtime closure. Because `guix pack -f docker` emits the closure as a
single image layer, subsequent uploads exceed the reverse proxy's body limit
and fail with an Nginx-style HTTP 413 response ([first completed pack/upload
failure](https://forge.snamellit.com/pti/wraptool/actions/runs/148), [latest
retry](https://forge.snamellit.com/pti/wraptool/actions/runs/173)). The exact new
layer size is not currently logged. The archive size comes from the
[upstream Quarto 1.9.38 release](https://github.com/quarto-dev/quarto-cli/releases/tag/v1.9.38).

GnuPG briefly made the image larger during release-workflow development, but it
was not the root cause: removing GnuPG did not fix the upload while Quarto
remained.

## 2. Goals

- Keep the default development and harness environment focused on tools used by
  normal wraptool development.
- Let a developer explicitly request Quarto for a documentation task.
- Let an agent in a documentation-enabled Dev Container run `quarto preview`
  and `quarto render` without host credentials or a host Docker socket.
- Keep tool versions and package definitions centralized even if environments
  compose different capability profiles.
- Preserve reproducible, CI-validated documentation builds.
- Make environment size visible so future growth is detected before registry
  upload.

## 3. Non-goals

- Installing Quarto dynamically into every running container.
- Giving a container the host Docker socket so it can launch a Quarto image.
- Installing unpinned tools from `postCreateCommand`.
- Turning the existing wraptool Dev Container Feature into a general package
  manager.
- Replacing the current Quarto site or its publishing workflows.
- Making documentation tools available without a container rebuild.

## 4. Decision: capability profiles, not one universal output

Within each packaging ecosystem, retain a single source for package names,
versions, and package definitions, but allow multiple derived environments:

| Profile | Consumers | Contents |
|---|---|---|
| Base development | normal Guix container, local Go development | Go, Git, Node, golangci-lint, shell/runtime plumbing |
| Lint image | GitLab lint | base tools needed by `.gitlab-ci.yml`; no documentation renderer |
| Documentation CI | Forgejo/GitLab docs jobs | Quarto plus only the dependencies needed to render the site |
| Documentation development | docs-focused Guix/Dev Container | base development plus Quarto |

This revises the R2 statement in `guix-container-runtime.md` that one flat
manifest must be consumed verbatim everywhere. For Guix, the property worth
preserving is **one definition per tool**, not **one identical package closure
for unrelated jobs**. Guix profiles must be composed from shared definitions;
they must not copy version strings or maintain independent package lists that
can silently drift.

Guix packages, Dev Container Features, and upstream OCI images cannot literally
share one Scheme package object. Across those ecosystems, define one canonical
Quarto version in release/update automation and validate that every selected
artifact resolves to it. Automated synchronization is the consistency model;
identical package definitions are not.

The concrete Scheme layout is an implementation decision. Acceptable shapes
include a shared module exporting base and documentation manifests, with thin
entry manifests selecting or concatenating them. Environment-variable toggles
inside `manifest.scm` remain undesirable because identical source would then
evaluate differently based on hidden process state.

## 5. Dev Container opt-in

Dev Container Features are explicit image-build inputs. A feature listed under
`features` is installed while the container image is built; an absent feature
adds no packages. Adding, removing, or changing a feature requires **Rebuild
Container**. Features are not runtime plugins.

The generated default remains documentation-free:

```jsonc
{
  "features": {
    "forge.snamellit.com/pti/wraptool/wraptool:1": {
      "harness": "claude"
    }
  }
}
```

A documentation-enabled configuration adds an independent Quarto feature:

```jsonc
{
  "features": {
    "forge.snamellit.com/pti/wraptool/wraptool:1": {
      "harness": "claude"
    },
    "<trusted-registry>/<namespace>/quarto:1": {
      "version": "1.9.38"
    }
  }
}
```

The Quarto feature URI above is illustrative until an upstream feature is
selected and reviewed. Prefer a maintained upstream feature with pinned release
inputs. A wraptool-owned feature is a fallback, but it creates an ongoing
packaging, checksum, and update responsibility.

Feature selection must declare and test its compatibility matrix. For the first
version, select a feature that supports both AMD64 and ARM64 and restrict docs
mode to the scaffolder's known Debian and Ubuntu bases. Reject Alpine and
arbitrary `--base IMAGE` values with an actionable error unless that exact base
is added to the tested matrix later. This avoids pretending the scaffolder can
infer libc, distribution, or target architecture from an arbitrary image name.

Do not add a `quarto` boolean to the existing wraptool feature. Provisioning the
harness, wiring MCP, and installing a documentation renderer are independent
concerns. Keeping separate feature entries also avoids Dev Container metadata
that cannot be conditional, notably `containerEnv`.

## 6. User interface

The first implementation should expose the choice while scaffolding:

```bash
wraptool init devcontainer --harness claude --lang go,node --docs quarto
```

Default: no `--docs` value and no Quarto feature.

The command emits the selected feature into a new
`.devcontainer/devcontainer.json`. This fits the current scaffold behavior,
which does not merge an existing file and overwrites only when `--force` is
explicitly supplied. Existing projects opt in by adding the reviewed feature
entry manually.

An alternative is a second configuration such as
`.devcontainer/docs/devcontainer.json`. That is useful when a team frequently
switches between a small normal image and a documentation image, but duplicates
more configuration and requires the editor/user to choose the right file. It is
not required for the first phase.

Dev Container JSON has no general conditional syntax for feature keys. The
generator decides whether to emit the feature; the container does not evaluate
an `if` expression at startup.

## 7. Guix path

Removing Quarto from the base Guix manifest means the normal Guix container can
no longer render documentation. That is intentional for the default profile,
but the Guix runtime needs an explicit equivalent before this design is complete.

Candidate Guix-specific interface:

```bash
wraptool up --runtime=guix --profile docs
```

The docs profile composes the base development manifest with the documentation
manifest before invoking `guix shell --container`. It must not depend on a Guix
daemon or host store socket becoming visible inside the running container; the
complete profile is selected and realized by the trusted host before entry.
Both the profile warm-up command and the interactive container invocation in
`cmd/guixcontainer.go` must consume the same composed manifest.

`--profile docs` is Guix-only in this interface. Explicit `--runtime=guix` never
falls back; if Guix is unavailable, fail normally. Dev Containers opt in through
their generated feature entry and rebuild path (§5-6), not through this flag.

Until that profile exists, developers can render documentation in a host-side,
credential-aware shell:

```bash
guix time-machine -C channels.scm -- shell quarto-bin -- quarto render docs
```

That fallback is suitable for a trusted developer but is not equivalent to
letting the isolated harness validate its own documentation changes.

## 8. CI and publication

- **Forgejo docs:** continue using a dedicated `quarto-bin` shell. Evaluate it
  through the repository's pinned channels rather than the runner's mutable
  channel state.
- **GitLab docs:** continue using a dedicated Quarto image, but pin its digest
  and align its version with the canonical documentation-tool version instead
  of maintaining an unrelated mutable `QUARTO_VERSION` indefinitely.
- **GitLab lint:** consume the base/lint image without Quarto.
- **CI image publication:** print archive and image-layer sizes before upload.
  Keep the reverse proxy limit high enough for intentional growth, but do not
  treat a larger proxy limit as a substitute for profile separation.

Every shared Scheme module and derived manifest that influences the lint image
must appear in `publish-ci-image.yml`'s path filters. Tag each candidate with a
hash of those toolchain inputs (or its manifest digest), not with every source
commit: ordinary commits do not publish an image.

Use two-phase promotion to avoid the Forgejo/GitLab race:

1. A toolchain-change commit publishes the content-addressed candidate while
   GitLab continues using the previous known-good immutable image.
2. After publication succeeds, a separate promotion commit updates GitLab's
   pinned `CI_TOOLCHAIN_TAG` or digest to that candidate.
3. GitLab validates the promoted immutable image on the promotion commit and
   every subsequent ordinary commit.

The reference may be generated into `.gitlab-ci.yml` or another GitLab-visible
CI configuration input, but it must be explicit and reviewable. `latest` may
remain a convenience tag; validation and reproducibility must not depend on it.

The observed response is an Nginx-style HTML HTTP 413 returned during a registry
blob upload, after Guix successfully built the pack. That evidence points to the
reverse proxy in front of Forgejo rather than an Actions execution limit. The
operator should confirm the proxy logs and then increase `client_max_body_size`
for `/v2/` and disable request buffering as appropriate. The profile split is
still valuable because it reduces transfer, storage, and startup costs.

## 9. Security and supply chain

- Feature installers execute as root during the image build. Review the feature
  source and pin immutable inputs or trusted major versions according to the
  repository's update policy.
- Verify downloaded Quarto archives with a pinned SHA-256 checksum if wraptool
  owns the feature.
- Rendering documentation can execute extensions, filters, notebooks, or
  project scripts. It belongs in the same credential-isolated environment as
  other untrusted workspace builds.
- Do not mount the host Docker socket merely to obtain Quarto; that would weaken
  the container boundary far more than the renderer is worth.
- No host GPG, Forgejo, Git, or cloud credentials are needed by the docs feature.

## 10. Phased implementation

### Phase 1: measure and separate CI

1. Log the Guix Docker archive size and layer metadata.
2. Derive a lint image without Quarto from shared package definitions.
3. Add every shared definition/manifest input to the image workflow's path
   filters.
4. Publish a candidate tagged by the complete toolchain-input hash.
5. Promote it in a separate commit that updates GitLab's immutable image
   reference, then confirm lint uses that exact candidate.
6. Keep existing dedicated documentation jobs green.
7. Update README and contributor commands that currently expect Quarto in
   `manifest.scm`.

### Phase 2: Dev Container opt-in

1. Select and review an upstream Quarto feature, or design a minimal pinned
   wraptool-owned feature.
2. Define supported base-image and architecture combinations.
3. Add `--docs none|quarto` to `wraptool init devcontainer` (`none` default).
4. Generate the independent feature entry or reject incompatible inputs.
5. Document that changing the option requires a container rebuild.

### Phase 3: Guix docs profile

1. Introduce shared Scheme package/profile definitions without duplicated
   versions.
2. Add a host-selected docs profile to the Guix runtime.
3. Verify `quarto render docs` from inside the isolated harness container.

### Phase 4: version alignment

Use one reviewed canonical Quarto version in update automation, then verify the
Guix package, selected Dev Container Feature option, and digest-pinned GitLab
docs image all resolve to it. Avoid coupling wraptool releases to Quarto's
release cadence.

## 11. Acceptance criteria

- A default generated Dev Container does not add a Quarto feature (its selected
  base image remains outside wraptool's control).
- A docs-enabled generated Dev Container reports the selected Quarto version and
  successfully runs `quarto render docs`.
- Rebuilding without the docs feature removes Quarto content introduced by that
  feature; an arbitrary user-supplied base may still provide its own renderer.
- The harness has no additional host credentials or Docker socket access.
- GitLab lint uses an image that excludes the Quarto closure.
- Forgejo and GitLab documentation renders remain successful with pinned
  channel/image inputs.
- Quarto's canonical version is not updated manually in multiple ecosystems.
- CI logs the packed image size before upload.
- Every file affecting the derived lint image triggers image publication.
- GitLab validates the explicitly promoted content-addressed image, not mutable
  `latest`; ordinary source commits continue using that known-good reference.
- Tests cover generated JSON with and without the docs option and validate the
  selected feature options, base image, and CPU architecture.

## 12. Open questions

1. Which maintained Quarto Dev Container Feature should be trusted, and does it
   support exact version/checksum pinning?
2. Should the first UI be `--docs quarto`, a more general `--feature quarto`, or
   a project template input? Prefer the narrow option until a second use case
   proves a general feature registry is needed.
3. How should shared Scheme definitions be structured so `manifest.scm`, the
   lint image manifest, and the docs profile remain easy to evaluate with
   `guix time-machine`?
4. Is a docs-specific second Dev Container configuration worth the duplication
   for contributors who switch profiles frequently?
5. What image-size budget should fail CI before the registry proxy does?
