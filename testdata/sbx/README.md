# sbx fixtures

These files are **transcribed from Docker's documentation and CLI reference
data** (`design/harness/docker-sandbox-runtime.md` §15), not captured from a
running `sbx`. Nothing here has been verified against real output; the
table spacing, the `Reason:` text and the version line are illustrative.
Phase 0 of the design replaces them with captures (R11). The secret values
are fake.

| File | Source |
|---|---|
| `secret_ls_global.txt` | the documented `SCOPE TYPE NAME SECRET` table; the `github` row is the documented example, the others are invented rows of the same shape (including a sandbox-scoped one) |
| `version.txt`, `version_old.txt` | issue-report shape `sbx version: v0.45.0 <commit>` |
| `ls_quiet.txt` | `sbx ls --quiet`: "Only display sandbox names", one per line |
| `policy_check_allowed.txt`, `policy_check_denied.txt` | documented `Allowed:`/`Denied:` plus a `Reason:` line |
