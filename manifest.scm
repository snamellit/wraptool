;; Single source of truth for the wraptool toolchain — one flat manifest used
;; verbatim everywhere:
;;   * local dev            guix shell -m manifest.scm
;;   * CI lint              guix shell -m manifest.scm -- ... (Forgejo)
;;   * CI container image   guix pack -f docker -m manifest.scm (GitLab)
;;   * isolated dev box     guix shell --container -m manifest.scm (wraptool up)
;;
;; No build-time toggles: the container that the AI harness runs in gets the
;; same toolchain a developer does. Credential isolation is the mount/user
;; namespace (host $HOME and secrets are never shared), not the package set —
;; so privileged/remote git (push, private fetch) still has to go through
;; wraptool's MCP git tool, which holds the host credentials.
;;
;; golangci-lint is not packaged in Guix, so we install the official pre-built
;; release binary (statically linked; no dynamic-linker patching needed). To
;; bump it: change %golangci-lint-version, then refresh the hash with e.g.
;;   guix download https://github.com/golangci/golangci-lint/releases/download/v<VER>/golangci-lint-<VER>-linux-amd64.tar.gz

(use-modules (guix packages)
             (guix download)
             (guix build-system copy)
             ((guix licenses) #:prefix license:))

(define %golangci-lint-version "2.12.2")

;; Runtime plumbing every self-contained use (image / --container) needs: a
;; shell + coreutils for job/prep scripts, and CA roots for HTTPS dependency
;; pulls (npm/pip/cargo/go modules). Named so the toolchain list below reads as
;; project intent, not boilerplate. Harmless for local dev (the host has these).
(define %plumbing '("bash" "coreutils" "nss-certs"))

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
        (append '("go" "git" "node")   ; toolchain + AI harness runtime
                %plumbing))
       (packages->manifest (list golangci-lint))))
