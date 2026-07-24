#!/usr/bin/env bash
set -euo pipefail

source_root=$(pwd)
test_root=$(mktemp -d)
trap 'rm -rf "$test_root"' EXIT

mkdir "$test_root/bin"
cat >"$test_root/bin/gpg" <<'EOF'
#!/bin/sh
case " $* " in
  *" --import-options show-only "*)
    echo 'fpr:::::::::C8ABC91CAB2DC195B9763877DFABC43C55F2E63A:'
    ;;
  *" --list-secret-keys "*) exit 0 ;;
  *) echo "unexpected gpg invocation: $*" >&2; exit 1 ;;
esac
EOF
cat >"$test_root/bin/go" <<'EOF'
#!/bin/sh
test "$1" = test
EOF
chmod +x "$test_root/bin/gpg" "$test_root/bin/go"
export PATH="$test_root/bin:$PATH"
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_SYSTEM=/dev/null

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

assert_contains() {
  local output=$1 expected=$2
  [[ "$output" == *"$expected"* ]] || fail "expected output to contain: $expected"
}

expect_failure() {
  local expected=$1
  shift
  local output
  if output=$("$@" 2>&1); then
    fail "command unexpectedly succeeded: $*"
  fi
  assert_contains "$output" "$expected"
}

setup_repo() {
  local name=$1
  local remote="$test_root/$name.git"
  work="$test_root/$name"

  git init --bare -b main "$remote" >/dev/null
  git init -b main "$work" >/dev/null
  cp "$source_root/release-version" "$source_root/release-signing-key.asc" "$work/"
  (
    cd "$work"
    git config user.name "Release Test"
    git config user.email "release-test@example.invalid"
    git add release-version release-signing-key.asc
    git commit -m initial >/dev/null
    git tag wraptool-v1.2.3
    git remote add origin "$remote"
    git push -u origin main --tags >/dev/null 2>&1
  )
}

setup_repo bumps
cd "$work"
for request_and_version in "patch 1.2.4" "minor 1.3.0" "major 2.0.0" "3.4.5 3.4.5"; do
  read -r request expected <<<"$request_and_version"
  output=$(./release-version --dry-run "$request")
  assert_contains "$output" "New release:    $expected"
done
git rev-parse --verify --quiet refs/tags/wraptool-v1.2.4 >/dev/null && \
  fail "--dry-run created a release tag"

setup_repo invalid
cd "$work"
expect_failure "invalid version '01.2.3'" ./release-version --dry-run 01.2.3

setup_repo dirty
cd "$work"
touch uncommitted
expect_failure "working tree is not clean" ./release-version --dry-run patch

setup_repo branch
cd "$work"
git switch -c topic >/dev/null 2>&1
expect_failure "releases must be created from the main branch" ./release-version --dry-run patch

setup_repo unsynchronized
cd "$work"
touch local-only
git add local-only
git commit -m local-only >/dev/null
expect_failure "local main must exactly match origin/main" ./release-version --dry-run patch

setup_repo duplicate
cd "$work"
expect_failure "tag wraptool-v1.2.3 already exists" \
  ./release-version --dry-run 1.2.3

setup_repo confirmation
cd "$work"
expect_failure "non-interactive release requires --yes" \
  ./release-version patch </dev/null

echo "release-version tests passed"
