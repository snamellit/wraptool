package yamledit

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Options configures Edit's behavior beyond the mutation func itself —
// everything that differs between wraptool's node-tree editors.
type Options struct {
	// Kind names the document in error messages, e.g. "config" or
	// "manifest". Defaults to "document" when empty.
	Kind string

	// FileMode is the permission bits applied to the written file (via the
	// temp file, before the rename that replaces path). Required: the zero
	// value would silently write a mode-0000 file, so Edit rejects it
	// rather than guess a default for a path it doesn't own the policy
	// for.
	FileMode os.FileMode

	// SeedIfMissing, when true, seeds Edit with an empty mapping document
	// instead of failing when path does not exist yet — e.g. manifest.yaml
	// bootstraps itself on first `policy set`/`install` rather than
	// needing a separate init step. When false (the default), a missing
	// file is an error.
	SeedIfMissing bool

	// Validate is called with the freshly re-encoded document bytes before
	// they are written. A non-nil error aborts the edit, leaving the file
	// on disk untouched. This is the caller's own semantic validation
	// (e.g. round-tripping through config.Config or pool.ParseManifest) —
	// yamledit never interprets the document itself. Required: both real
	// callers validate every edit, and this is a shared write path onto
	// security-policy files, so Edit rejects a nil Validate rather than
	// silently skip it.
	Validate func([]byte) error

	// Write performs the final atomic write; defaults to AtomicWrite (a
	// temp file created in path's own directory, renamed over path) when
	// nil. A caller that already owns a stricter atomic-write helper with
	// its own locking/fsync discipline (e.g. the harness pool's
	// pool.WriteFileAtomic) can inject it here instead, so Edit still
	// drives load/decode/mutate/encode/validate but the caller's own write
	// semantics are preserved exactly.
	Write func(path string, data []byte, mode os.FileMode) error
}

// Edit reads the YAML document at path, decodes it into a yaml.Node tree
// (preserving comments and key order), hands the document's root mapping
// node to mutate, re-encodes the result, runs opts.Validate over the
// encoded bytes, and — only if that succeeds — writes the result back via
// opts.Write (or AtomicWrite when unset). The file is left untouched if
// mutate or Validate returns an error. Edit itself fails immediately,
// before touching path, when opts.FileMode or opts.Validate is the zero
// value — see their doc comments.
func Edit(path string, opts Options, mutate func(doc *yaml.Node) error) error {
	kind := opts.Kind
	if kind == "" {
		kind = "document"
	}

	// A zero-value Options is a caller bug, not a case to guess a safe
	// default for: this is a shared write path onto security-policy files
	// (config.yaml, manifest.yaml), so a missing FileMode (which would
	// write a mode-0000 file) or a missing Validate (which would write an
	// edit with no semantic check at all) fails loud here rather than
	// risking either silently in production.
	if opts.FileMode == 0 {
		return fmt.Errorf("yamledit: Options.FileMode is required (got 0)")
	}
	if opts.Validate == nil {
		return fmt.Errorf("yamledit: Options.Validate is required (got nil)")
	}

	var root yaml.Node
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if uerr := yaml.Unmarshal(data, &root); uerr != nil {
			return fmt.Errorf("parsing %s: %w", kind, uerr)
		}
	case os.IsNotExist(err) && opts.SeedIfMissing:
		root = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	default:
		return fmt.Errorf("reading %s: %w", kind, err)
	}

	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return fmt.Errorf("%s is not a YAML document", kind)
	}
	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return fmt.Errorf("%s root is not a mapping", kind)
	}

	if err := mutate(doc); err != nil {
		return err
	}

	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return fmt.Errorf("re-encoding %s: %w", kind, err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("re-encoding %s: %w", kind, err)
	}

	// opts.Validate is guaranteed non-nil by the Options check above.
	if err := opts.Validate(out.Bytes()); err != nil {
		return fmt.Errorf("edited %s failed validation: %w", kind, err)
	}

	write := opts.Write
	if write == nil {
		write = AtomicWrite
	}
	return write(path, out.Bytes(), opts.FileMode)
}

// AtomicWrite writes data to a temp file created in path's own directory,
// then renames it over path — the atomic same-filesystem replace pattern:
// a reader (or an fsnotify watcher, e.g. wraptool's own config hot-reload)
// never observes a partially-written file. This is Edit's default Write;
// callers with stricter durability needs (fsync, parent-directory
// creation) supply their own via Options.Write instead.
func AtomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".wraptool-yamledit-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpPath)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		cleanup()
		return fmt.Errorf("setting permissions on temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("renaming temp file over %s: %w", path, err)
	}
	return nil
}
