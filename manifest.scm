;; Development environment for wraptool.
;;
;; `go` and `git` come straight from Guix.  golangci-lint is not (yet)
;; packaged in Guix, so we install the official pre-built release binary
;; (a statically linked Go executable, so no dynamic-linker patching is
;; required).  This manifest is the single source of truth for the pinned
;; toolchain: CI consumes it directly (Forgejo `guix shell`) or via an image
;; packed from it (GitLab — see .forgejo/workflows/publish-ci-image.yml).
;;
;; To bump golangci-lint: change %golangci-lint-version, then update the
;; hash with e.g.
;;   guix download https://github.com/golangci/golangci-lint/releases/download/v<VER>/golangci-lint-<VER>-linux-amd64.tar.gz

(use-modules (guix packages)
             (guix download)
             (guix build-system copy)
             ((guix licenses) #:prefix license:))

(define %golangci-lint-version "2.12.2")

;; Base toolchain — what a developer needs from `guix shell -m manifest.scm`.
(define %base-specs '("go" "git"))

;; Extra packages folded in only when WRAPTOOL_CI_IMAGE is set — for packing
;; the CI container image (recipe: .forgejo/workflows/publish-ci-image.yml):
;;   bash + coreutils  — GitLab's Docker runner needs /bin/sh and coreutils
;;                       (mkdir/mv/…) for the job's prep script.
;;   nss-certs         — a CA bundle so `go`/`golangci-lint` can fetch modules
;;                       over HTTPS.
;; A plain `guix shell` for local dev is unaffected (the host already has
;; these), so the interactive profile stays lean — but note the toggle is
;; ambient: a stray WRAPTOOL_CI_IMAGE in your env silently fattens the profile.
(define %ci-image-specs '("bash" "coreutils" "nss-certs"))

(define golangci-lint
  (package
    (name "golangci-lint")
    (version %golangci-lint-version)
    (source
     (origin
       (method url-fetch)
       (uri (string-append
             "https://github.com/golangci/golangci-lint/releases/download/v"
             version "/golangci-lint-" version "-linux-amd64.tar.gz"))
       (sha256
        (base32 "0lx5jcw99qrag9f5y8b5wrsz53dgk5q51h5ahjlqzv8gcz981xcd"))))
    (build-system copy-build-system)
    (arguments
     (list #:install-plan
           #~'(("golangci-lint" "bin/"))))
    (home-page "https://golangci-lint.run")
    (synopsis "Fast linters runner for Go")
    (description "Upstream pre-built golangci-lint release binary.")
    (license license:gpl3+)))

(concatenate-manifests
 (list (specifications->manifest
        (if (getenv "WRAPTOOL_CI_IMAGE")
            (append %base-specs %ci-image-specs)
            %base-specs))
       (packages->manifest (list golangci-lint))))
