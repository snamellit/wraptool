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
	"sync"

	"github.com/pti/wraptool/internal/pool"
)

// The integrity index (pool.CacheSHA512IndexDirName) finds a cache object
// by its upstream sha512 when the lock does not record the object's sha256
// cache address yet: right after a fresh resolve, every npm package's
// sha256 is empty, so without the index an interrupted update re-downloads
// everything it had already cached.
//
// An entry is only a hint: findCached re-hashes the object it points at and
// returns it only if both digests match, so a stale, truncated, or tampered
// entry can never admit bytes that did not pass the upstream integrity
// check.
//
// Objects cached before the index existed have no entries. The first miss
// for a cacheDir in this process indexes them (hashing each unindexed
// object); later misses go straight to the download. The memo is
// process-wide because cmd builds a fresh Source per harness, and every one
// of them shares the same pool cache.
var (
	backfillMu   sync.Mutex
	backfillDone = map[string]bool{}
)

// findCached returns the sha256 address of the cached object whose sha512
// is want, or "" when there is none. It returns "" without looking when
// knownSHA256 is set (the caller's own cache-hit check already covers that
// case) or when want is nil (no upstream sha512 to verify against).
func findCached(cacheDir string, want []byte, knownSHA256 string) (string, error) {
	if knownSHA256 != "" || want == nil {
		return "", nil
	}
	if sha256Hex := verifiedEntry(cacheDir, want); sha256Hex != "" {
		return sha256Hex, nil
	}
	backfillMu.Lock()
	defer backfillMu.Unlock()
	if backfillDone[cacheDir] {
		return "", nil
	}
	hit, err := backfillIndex(cacheDir, want)
	if err != nil {
		return "", err
	}
	backfillDone[cacheDir] = true
	return hit, nil
}

// recordCached writes the index entry for an object just admitted to
// cacheDir. Best-effort: the index only saves re-downloads, so a failure to
// write it must not fail the fetch that produced a valid object. A nil
// sha512Digest (no upstream sha512) records nothing.
func recordCached(cacheDir string, sha512Digest []byte, sha256Hex string) {
	if sha512Digest != nil {
		_ = pool.WriteCacheIndexEntry(cacheDir, sha512Digest, sha256Hex)
	}
}

// verifiedEntry reads want's index entry and returns the sha256 it names if
// the object exists and re-hashes to both digests. A bad entry is removed
// so the next lookup does not re-check it.
func verifiedEntry(cacheDir string, want []byte) string {
	entryPath := pool.CacheIndexEntryPath(cacheDir, want)
	sha256Hex, ok := pool.ReadCacheIndexEntry(entryPath)
	if !ok {
		return ""
	}
	got512, got256, err := hashFile(filepath.Join(cacheDir, sha256Hex))
	if err == nil && bytes.Equal(got512, want) && got256 == sha256Hex {
		return sha256Hex
	}
	_ = os.Remove(entryPath)
	return ""
}

// backfillIndex hashes every cache object that no index entry names yet,
// records it, and returns the sha256 of the one whose sha512 is want (""
// if none). Objects whose content does not match their sha256 name are
// skipped: they are corrupt, and verifiedEntry would reject them anyway.
func backfillIndex(cacheDir string, want []byte) (string, error) {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("listing cache %s: %w", cacheDir, err)
	}
	indexed := map[string]bool{}
	for _, p := range pool.CacheIndexEntries(cacheDir) {
		if sha256Hex, ok := pool.ReadCacheIndexEntry(p); ok {
			indexed[sha256Hex] = true
		}
	}
	hit := ""
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !validSHA256Hex(name) || indexed[name] {
			continue
		}
		got512, got256, err := hashFile(filepath.Join(cacheDir, name))
		if err != nil || got256 != name {
			continue
		}
		recordCached(cacheDir, got512, name)
		if bytes.Equal(got512, want) {
			hit = name
		}
	}
	return hit, nil
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
