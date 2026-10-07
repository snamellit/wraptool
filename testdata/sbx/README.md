# sbx fixtures

Two kinds of file live here, and the distinction matters: a **captured** file
is real output from a running `sbx` (version noted), and a **transcribed**
file is reconstructed from Docker's documentation and CLI reference data
(`design/harness/docker-sandbox-runtime.md` §15) with nothing verified. The
design's Phase 0 replaces every transcribed file with a capture (R11). Secret
values in transcribed files are fake.

| File | Kind | Content |
|---|---|---|
| `secret_ls_global.txt` | captured, sbx 0.47.0 | `sbx secret ls --global` with no global secret: a prose line, not an empty table |
| `secret_ls_global_table.txt` | transcribed | the documented `SCOPE TYPE NAME SECRET` table; the `github` row is the documented example, the others are invented rows of the same shape |
| `ls_quiet.txt` | captured, sbx 0.47.0 | `sbx ls --quiet`: one name per line; stopped sandboxes are listed |
| `ls.txt` | captured, sbx 0.47.0 | `sbx ls`: the table behind it (not parsed by wraptool) |
| `mcp_ls_quiet.txt` | captured, sbx 0.47.0 | `sbx mcp ls --quiet` with nothing registered: empty |
| `version.txt`, `version_old.txt` | transcribed | issue-report shape `sbx version: v0.45.0 <commit>` |
| `policy_check_allowed.txt`, `policy_check_denied.txt` | transcribed | documented `Allowed:`/`Denied:` plus a `Reason:` line |
