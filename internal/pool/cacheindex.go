package pool

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

// CacheSHA512IndexDirName is the subdirectory of the sha256 cache
// (cache/sha256/.by-sha512/<sha512 hex>) holding the integrity index: one
// small file per cache object, named by the object's upstream sha512 and
// containing its sha256 cache address. It lets Fetch find an object it
// already downloaded when the lock does not yet record the sha256 (a fresh
// resolve, an interrupted update). The index is a lookup hint only; every
// hit is re-verified against the sha512 before it is trusted. gc skips
// subdirectories when collecting objects and prunes this one separately.
const CacheSHA512IndexDirName = ".by-sha512"

// CacheIndexEntryPath returns the index entry path for an object whose
// upstream sha512 digest is sha512Digest, under the sha256 cache cacheDir.
func CacheIndexEntryPath(cacheDir string, sha512Digest []byte) string {
	return filepath.Join(cacheDir, CacheSHA512IndexDirName, hex.EncodeToString(sha512Digest))
}

// WriteCacheIndexEntry records that the object with sha512Digest is cached
// as cacheDir/<sha256Hex>.
func WriteCacheIndexEntry(cacheDir string, sha512Digest []byte, sha256Hex string) error {
	return WriteFileAtomic(CacheIndexEntryPath(cacheDir, sha512Digest), []byte(sha256Hex+"\n"), 0o644)
}

// ReadCacheIndexEntry returns the sha256 cache address an index entry
// names, or false when the entry is unreadable or malformed.
func ReadCacheIndexEntry(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	sha256Hex := strings.TrimSpace(string(data))
	return sha256Hex, isLowerHexOfLen(sha256Hex, 64)
}

// CacheIndexEntries lists the index entry paths under cacheDir; a missing
// index is an empty list.
func CacheIndexEntries(cacheDir string) []string {
	dir := filepath.Join(cacheDir, CacheSHA512IndexDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		paths = append(paths, filepath.Join(dir, e.Name()))
	}
	return paths
}
