(specifications->manifest
  (module
    (make-modules "go/go-build"
      (build-options
        (go-build "go build -o wraptool ." "")
        )))
