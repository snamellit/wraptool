# WebAuthn Admin Authentication

Status: proposed

## Problem

The administrator can review capability requests and edit the policy through
the Web UI. Those actions can grant an AI access to credential-bearing host
tools, so the administrator must be authenticated independently of the AI.

Guix containers run with `guix shell --network`, which shares the host network
namespace. A loopback-only listener is therefore reachable by the untrusted
container. The existing tokenless Web UI mode cannot be an authorization
boundary: a container can POST directly to an approval or rule-edit endpoint,
then the server hot-reloads the modified policy.

## Goals

- Require an administrator identity for every enabled Web UI.
- Use platform passkeys where available: Touch ID, Windows Hello, desktop
  passkeys, or FIDO2 security keys.
- Keep the private credential in the authenticator; wraptool stores only the
  public WebAuthn credential material.
- Keep MCP authentication and administrator authentication separate. An MCP
  token forwarded to a container must never authorize Web UI operations.
- Offer a low-friction first-run setup and a deliberate recovery process.
- Work with local development (`wraptool.localhost`) and an HTTPS deployment.

## Non-goals

- Direct use of PAM, macOS LocalAuthentication, or Windows Hello APIs. Those
  are platform-specific and would not provide a portable server implementation.
- Treating a local source address, process UID, or container marker as proof of
  administrator identity.
- Replacing MCP authentication with WebAuthn. MCP clients need a non-browser
  credential; WebAuthn protects the human-only administration plane.

## Threat Model

The AI client and its container workload are untrusted. They may make arbitrary
HTTP requests to listeners reachable from the container and possess the MCP
token injected by `wraptool up`. They must not be able to approve a request,
create an allow rule, modify a tool, rotate credentials, or enroll a new
administrator credential.

The local OS account is trusted during explicit CLI setup and recovery. A local
same-UID hostile process is not a primary isolation boundary; token files and
WebAuthn still reduce accidental exposure and browser-origin attacks.

## Authentication Model

### Transport and origins

The Web UI is an admin-only listener. Its default local origin is
`http://wraptool.localhost:<port>` and its WebAuthn relying-party ID is
`wraptool.localhost`. The server validates Host and Origin against configured
origins. A remote deployment must use HTTPS and a configured DNS name.

`localhost` is treated as a secure context by major browsers. The stable
hostname, rather than a loopback IP literal, is used as the relying-party ID.

### Credentials

Each enrolled operator credential contains:

- a stable operator ID and display name;
- WebAuthn credential ID and public key;
- signature counter, transports, and backup eligibility/state;
- creation and last-used timestamps.

This metadata is stored in `$XDG_DATA_HOME/wraptool/admin-credentials.json`
with mode `0600`, in a `0700` parent directory. Private keys never leave the
browser authenticator.

WebAuthn requests require user verification. The server sends
`userVerification: "required"` for registration and assertion. Consequently,
key possession alone is insufficient when the platform supports PIN, biometric,
or local device-unlock verification.

### Sessions and step-up

After a verified assertion, wraptool issues an opaque, random, server-side
session with a short idle and absolute expiry. The cookie is `HttpOnly`,
`SameSite=Strict`, and `Secure` when served over HTTPS.

Read-only pages require a valid session. Every policy-mutating endpoint
(approval, denial, rule/tool/constraint CRUD, credential enrollment/removal,
and recovery-token rotation) requires a fresh WebAuthn assertion. This limits
the impact of a stolen or unattended browser session.

Every POST also has CSRF protection and Host/Origin validation. WebAuthn is an
identity check, not a replacement for request-origin defenses.

## Bootstrap and Recovery

`wraptool setup` creates a safe configuration, generates a separate MCP token
and admin recovery token, and opens a one-time enrollment URL. The URL carries
a short-lived, single-use bootstrap secret in its fragment, not its query,
so it is not included in HTTP requests or server logs. The enrollment page
exchanges it for an enrollment session and asks the local operator to create a
passkey.

The recovery token is stored at
`$XDG_CONFIG_HOME/wraptool/tokens/admin-recovery`, mode `0600`. It is never
forwarded to a container and is not an ordinary Web UI login credential.
`wraptool admin recover` requires an interactive host terminal confirmation,
starts a short-lived enrollment session, and opens the browser. It supports
lost-device recovery without leaving a permanent token-authenticated admin UI.

## Configuration

The transitional token foundation requires a separate Web UI token:

```yaml
mcp:
  transport: sse
  listen: 127.0.0.1:8717
  auth_token_file: ~/.config/wraptool/tokens/mcp
admin:
  listen: 127.0.0.1:8718
  auth_token_file: ~/.config/wraptool/tokens/admin-bootstrap
```

An enabled Web UI must have an auth token file during the transition. It must
not refer to the MCP token file, including through symlinks. `wraptool up`
creates configured missing token files securely. The WebAuthn implementation
replaces ordinary token login after enrollment; the separate token remains only
for bootstrap and recovery.

Guix startup additionally rejects a config file under the writable workspace.
Otherwise a container could alter the YAML directly and rely on hot reload.

## Implementation Phases

1. Remove tokenless Web UI mode. Require a distinct Web UI token, provision it
   with `up`, and reject Guix workspaces containing the active config. Add
   regression tests that an MCP token cannot approve or add rules.
2. Add `wraptool setup`, secure token-file paths, and one-time bootstrap state.
   Keep token-authenticated Web UI access only for enrollment/recovery while
   preserving a documented migration path for existing operators.
3. Add WebAuthn registration and assertion using `github.com/go-webauthn/webauthn`.
   Persist public credential metadata and server-side challenges/sessions.
4. Require passkey authentication for Web UI access and fresh assertions for
   mutations. Add CSRF, Host/Origin, and security-header middleware.
5. Remove ordinary admin token login, retaining only the explicit recovery
   command and its short-lived enrollment flow.

## Test Plan

- Config validation rejects an enabled Web UI without a token and rejects MCP/
  Web UI token aliasing, including symlink aliases.
- `up` creates separate token files at `0600` and refuses a Guix workspace that
  contains the active config, including a symlinked config path.
- Unauthenticated requests and requests bearing the MCP token cannot approve a
  request or create an allow rule; config bytes and request state remain
  unchanged. The distinct Web UI credential can perform the operation.
- A Guix integration test attempts both endpoints through host loopback using
  its injected MCP token and verifies that both fail.
- WebAuthn tests cover registration, assertion, challenge expiry/replay,
  origin/RP-ID mismatch, invalid signatures, session expiry, and per-mutation
  step-up requirements.
