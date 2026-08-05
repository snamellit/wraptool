;; Package set for the wraptool harness-pool PROVISIONING image (§11.2 of
;; design/harness/unified-harness-pool.md): the minimal toolchain the OCI
;; backend (docker/podman) needs to run the pool's resolve and realize
;; phases on hosts without Guix (macOS, Windows, non-Guix Linux).
;;
;; This is deliberately the same contract as the guix backend's package set
;; (cmd/harness.go defaultResolveBackend): node+npm for resolve/npm-ci,
;; tar+xz for the node-runtime unpack, coreutils for the `env HOME=` argv
;; prefix, bash for npm lifecycle scripts (claude/opencode postinstall are
;; load-bearing), and CA certs for the resolve phase's registry HTTPS.
;; Keeping the two sets in lockstep is what makes §17's cross-backend
;; receipt-equality test meaningful.
;;
;; Built by .forgejo/workflows/publish-provisioning-image.yml via
;; `guix pack -f docker` under channels.scm's pinned revisions, so the
;; image is reproducible from this file + the channel lock. The published
;; image's DIGEST is pinned in cmd/harness.go and folded into every npm
;; recipe_revision (§8) — changing this file produces a new digest and
;; therefore invalidates locks, deliberately.
(specifications->manifest
 (list "node"
       "tar"
       "xz"
       "coreutils"
       "bash"
       "nss-certs"))
