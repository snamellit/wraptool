;; Development environment for wraptool.
;;
;; `go` and `git` come straight from Guix.  golangci-lint is not (yet)
;; packaged in Guix, so we install the official pre-built release binary —
;; version-pinned to match CI (.gitlab-ci.yml / .forgejo/workflows/lint.yml,
;; currently v2.12.2).  The binary is a statically linked Go executable, so
;; no dynamic-linker patching is required.
;;
;; To bump the version: change %golangci-lint-version, then update the
;; hash with e.g.
;;   guix download https://github.com/golangci/golangci-lint/releases/download/v<VER>/golangci-lint-<VER>-linux-amd64.tar.gz

(use-modules (guix packages)
             (guix download)
             (guix build-system copy)
             ((guix licenses) #:prefix license:))

(define %golangci-lint-version "2.12.2")

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
    (description
     "golangci-lint runs a configurable set of Go linters in parallel.  This
package installs the upstream pre-built release binary rather than building
from source, since golangci-lint is not packaged in Guix.")
    (license license:gpl3+)))

(concatenate-manifests
 (list (specifications->manifest '("go" "git"))
       (packages->manifest (list golangci-lint))))
