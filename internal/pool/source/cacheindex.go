package source

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/pti/wraptool/internal/pool"
)

// cacheIndex finds a cache object by its upstream sha512 when the lock does
// not record the object's sha256 cache address yet: right after a fresh
// resolve, every npm package's sha256 is empty, so without this index an
// interrupted update re-downloads everything it had already cached.
//
// Index entries live under cacheDir/pool.CacheSHA512IndexDirName. An entry
// is only a hint: lookup re-hashes the object it points at and returns it
// only if both digests match, so a stale, truncated, or tampered entry can
// never admit bytes that did not pass the upstream integrity check.
//
// Objects cached before the index existed have no entries. The first miss
// in a cacheDir indexes them once (hashing each unindexed object); later
// misses go straight to the download. The zero value is ready to use.
type cacheIndex struct {
	mu      sync.Mutex
	scanned map[string]bool // cacheDir -> backfill already ran in this process
}

// lookup returns the sha256 address of the cached object whose sha512 is
// want, or "" when there is none.
func (ix *cacheIndex) lookup(cacheDir string, want []byte) (string, error) {
	if sha256Hex := verifiedEntry(cacheDir, want); sha256Hex != "" {
		return sha256Hex, nil
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.scanned[cacheDir] {
		return "", nil
	}
	if err := backfillIndex(cacheDir); err != nil {
		return "", err
	}
	if ix.scanned == nil {
		ix.scanned = map[string]bool{}
	}
	ix.scanned[cacheDir] = true
	return verifiedEntry(cacheDir, want), nil
}

// verifiedEntry reads want's index entry and returns the sha256 it names if
// the object exists and re-hashes to both digests. A bad entry is removed
// so the next lookup does not re-check it.
func verifiedEntry(cacheDir string, want []byte) string {
	entryPath := indexEntryPath(cacheDir, want)
	data, err := os.ReadFile(entryPath)
	if err != nil {
		return ""
	}
	sha256Hex := strings.TrimSpace(string(data))
	if validSHA256Hex(sha256Hex) {
		got512, got256, err := hashFile(filepath.Join(cacheDir, sha256Hex))
		if err == nil && bytes.Equal(got512, want) && got256 == sha256Hex {
			return sha256Hex
		}
	}
	_ = os.Remove(entryPath)
	return ""
}

// recordIndex writes the index entry for an object just admitted to
// cacheDir. Best-effort: the index only saves re-downloads, so a failure to
// write it must not fail the fetch that produced a valid object.
func recordIndex(cacheDir string, sha512Digest []byte, sha256Hex string) {
	_ = pool.WriteFileAtomic(indexEntryPath(cacheDir, sha512Digest), []byte(sha256Hex+"\n"), 0o644)
}

// backfillIndex hashes every cache object that no index entry names yet and
// records it. Objects whose content does not match their sha256 name are
// skipped (they are corrupt, and verifiedEntry would reject them anyway).
func backfillIndex(cacheDir string) error {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("listing cache %s: %w", cacheDir, err)
	}
	indexed := map[string]bool{}
	if idx, err := os.ReadDir(filepath.Join(cacheDir, pool.CacheSHA512IndexDirName)); err == nil {
		for _, e := range idx {
			if data, err := os.ReadFile(filepath.Join(cacheDir, pool.CacheSHA512IndexDirName, e.Name())); err == nil {
				indexed[strings.TrimSpace(string(data))] = true
			}
		}
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !validSHA256Hex(name) || indexed[name] {
			continue
		}
		got512, got256, err := hashFile(filepath.Join(cacheDir, name))
		if err != nil || got256 != name {
			continue
		}
		recordIndex(cacheDir, got512, name)
	}
	return nil
}

func indexEntryPath(cacheDir string, sha512Digest []byte) string {
	return filepath.Join(cacheDir, pool.CacheSHA512IndexDirName, hex.EncodeToString(sha512Digest))
}

// hashFile returns path's sha512 digest and hex sha256 digest.
func hashFile(path string) ([]byte, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	h512, h256 := sha512.New(), sha256.New()
	if _, err := io.Copy(io.MultiWriter(h512, h256), f); err != nil {
		return nil, "", err
	}
	return h512.Sum(nil), hex.EncodeToString(h256.Sum(nil)), nil
}
