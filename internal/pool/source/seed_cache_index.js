// seed-cache-index.js — seeds npm's make-fetch-happen URL cache index for a
// shrinkwrapped harness (see realize.go's seedShrinkwrapCacheIndex doc
// comment for the full "why").
//
// This file is STATIC, checked-in content, embedded verbatim into the
// wraptool binary (go:embed) and copied byte for byte into every
// realization's .build scratch dir — it never has any per-harness data
// interpolated into it. All variable data (URLs/integrity/sizes, which
// ultimately derive from npm lockfile content) travels through the
// separate seed-manifest.json file, read here with JSON.parse, never
// string-interpolated into this script's own source.
//
// Usage (see realize.go for the exact argv):
//   node seed-cache-index.js <manifest.json> <npm-cache-dir>
//
// <npm-cache-dir> is the SAME directory realize.go passes to npm's own
// `--cache` flag (a plain directory, not the `_cacache` cacache store
// itself) — this script appends "_cacache" itself, mirroring pacote's own
// lib/util/cache-dir.js convention (confirmed empirically: content `npm
// cache add`s under `<cache>/_cacache/content-v2/...`, and
// `cacache.get.hasContent` only finds it there, not under `<cache>` bare).
//
// <manifest.json> shape: { "<resolved tarball URL>": { "integrity":
// "sha512-...", "size": <tarball byte size> }, ... } — see
// buildSeedManifest in realize.go.
'use strict'

const fs = require('fs')
const path = require('path')
const { execFileSync } = require('child_process')

const [, , manifestPath, npmCacheDir] = process.argv
if (!manifestPath || !npmCacheDir) {
  console.error('usage: node seed-cache-index.js <manifest.json> <npm-cache-dir>')
  process.exit(2)
}

// Locate npm's own bundled cacache module rather than depending on it being
// a resolvable dependency from this script's own location (it has none —
// this script is a standalone file dropped into a scratch dir, not part of
// any node_modules tree). `npm root -g` is the one anchor point that is
// true of any npm install regardless of how node/npm were provisioned (a
// system package, nodejs.org's own tarball, or Guix's node+npm packages);
// npm's own package lives at "<npm root -g>/npm", with cacache bundled
// under its node_modules. execFileSync (not execSync/a shell string) avoids
// spawning a shell for this static, argument-free invocation.
function findCacache () {
  const candidates = []
  try {
    const globalRoot = execFileSync('npm', ['root', '-g'], { encoding: 'utf8' }).trim()
    candidates.push(path.join(globalRoot, 'npm', 'node_modules', 'cacache'))
  } catch (err) {
    // fall through to the process.execPath-relative candidate below
  }
  // Fallback for a layout `npm root -g` cannot see (e.g. a node binary
  // invoked outside its own installation's PATH): nodejs.org's own tarball
  // layout ships npm at "<node prefix>/lib/node_modules/npm".
  candidates.push(path.join(path.dirname(process.execPath), '..', 'lib', 'node_modules', 'npm', 'node_modules', 'cacache'))

  const errors = []
  for (const candidate of candidates) {
    try {
      return require(candidate)
    } catch (err) {
      errors.push(`${candidate}: ${err.message}`)
    }
  }
  throw new Error(`could not locate npm's bundled cacache module; tried:\n${errors.join('\n')}`)
}

async function main () {
  const cacache = findCacache()
  const cachePath = path.join(npmCacheDir, '_cacache')
  const manifest = JSON.parse(fs.readFileSync(manifestPath, 'utf8'))

  let count = 0
  for (const [url, { integrity, size }] of Object.entries(manifest)) {
    // Defense in depth: the content this index entry will point at must
    // already exist (realize.go's `npm cache add` step runs strictly before
    // this one) — an index entry over missing content would let `npm ci
    // --offline` "succeed" at finding an entry and then fail with a much
    // more confusing EINTEGRITY/ENOENT deep inside cacache's own read path.
    const has = await cacache.get.hasContent(cachePath, integrity)
    if (!has) {
      throw new Error(`seed-cache-index: no cache content for ${url} (integrity ${integrity}); npm cache add must run before seeding`)
    }

    // The make-fetch-happen cache key format and the minimal metadata shape
    // a cache hit needs are both npm-internals: see
    // node_modules/make-fetch-happen/lib/cache/key.js (cacheKey) and
    // lib/cache/entry.js (getMetadata) in npm's own bundled copy. Empirically
    // confirmed sufficient (2026-08 diagnosis, reproduced against a real
    // @earendil-works/pi-coding-agent lockfile+shrinkwrap): npm's own
    // `--offline` flag maps to fetch option `cache: 'only-if-cached'`
    // (npm-registry-fetch/lib/index.js getCacheMode), which — per
    // make-fetch-happen/lib/cache/index.js's cacheFetch — returns straight
    // from ANY matching index entry without ever checking staleness, so
    // empty req/resHeaders and a fixed `time` are tolerated; a missing
    // "status" defaults to 200 (make-fetch-happen/lib/cache/entry.js's
    // response getter). If a future npm changes any of this, the
    // integration test this script exists for breaks loudly rather than
    // this comment silently going stale.
    const key = `make-fetch-happen:request-cache:${url}`
    await cacache.index.insert(cachePath, key, integrity, {
      size,
      metadata: {
        time: Date.now(),
        url,
        reqHeaders: {},
        resHeaders: {},
        options: { compress: true },
      },
    })
    count++
  }
  process.stderr.write(`seed-cache-index: seeded ${count} make-fetch-happen index entries\n`)
}

main().catch((err) => {
  console.error(err.stack || String(err))
  process.exit(1)
})
