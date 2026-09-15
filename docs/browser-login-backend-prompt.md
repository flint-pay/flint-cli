# Backend agent prompt: standard OAuth device login for Flint CLI

Implement the website and backend support for `flint auth login` in the Flint application repository. **This supersedes the earlier custom device-flow/API-key issuance proposal. Do not implement `/v1/cli/auth/device`, `/v1/cli/auth/token`, or return `secret_key` for browser login.** The CLI now implements OAuth access tokens, rotating refresh tokens, and grant revocation. API-key import remains a separate supported path for manual setup and CI.

The corresponding client code is in `internal/cli/login.go` and `internal/cli/oauth.go` in flint-cli. These new capabilities are not in its current embedded public OpenAPI snapshot. Match the contract below and coordinate any changes with the CLI agent.

## Standards and public client

Implement the OAuth 2.0 Device Authorization Grant ([RFC 8628](https://www.rfc-editor.org/rfc/rfc8628.html)), refresh-token grant and token responses ([RFC 6749](https://www.rfc-editor.org/rfc/rfc6749.html)), and revocation ([RFC 7009](https://www.rfc-editor.org/rfc/rfc7009.html)). Apply [OAuth security best practices](https://www.rfc-editor.org/info/rfc9700/), including refresh-token rotation with reuse detection for this public client.

Register `client_id=flint-cli` as a first-party **public** client with no client secret and no HTTP Basic authentication. Do not embed a client secret in the CLI. Reuse the existing authorization/token infrastructure where appropriate, but keep partner clients, grants, and audiences isolated. The existing `/v1/oauth/token` partner flows must remain compatible.

OAuth endpoint requests below use `application/x-www-form-urlencoded`. Success and error responses are flat OAuth JSON, **without** a Flint `data` or nested `error` envelope. Send `Cache-Control: no-store` and `Pragma: no-cache` on credential responses. Tokens may be opaque or JWTs; the CLI treats them as opaque and trusts authenticated server context, not decoded JWT claims.

## 1. Device authorization

`POST /v1/oauth/device/authorize`

Form fields:

- `client_id=flint-cli`
- `scope`: space-separated scopes from the CLI's existing `initialCLIScopes` list.
- `environment=sandbox` by default or `live` when the user explicitly supplies `--live`.
- Optional `merchant_id` and `sandbox_id`: constraints from the selected CLI profile/project.

The environment and tenant fields are Flint extensions to the standard device authorization request. Validate them, the client, and permitted scope vocabulary. They constrain the user's eventual choice; they never confer authority.

Example HTTP 200 response:

```json
{
  "device_code": "<high-entropy-opaque-secret>",
  "user_code": "ABCD-EFGH",
  "verification_uri": "https://app.withflintpay.com/cli/activate",
  "verification_uri_complete": "https://app.withflintpay.com/cli/activate?user_code=ABCD-EFGH",
  "expires_in": 600,
  "interval": 5
}
```

Client contract:

- `device_code`: nonempty, at most 4096 characters. Use strong cryptographic randomness; never expose it to the browser or user.
- `user_code`: 4–32 printable non-whitespace ASCII characters. Prefer an easy-to-read uppercase alphabet with hyphen separators. Rate-limit brute-force attempts.
- `expires_in`: integer seconds, 1–1800; recommend 600.
- `interval`: integer seconds, 1–60; omitted or zero defaults to 5 in this client.
- `verification_uri_complete` is optional. The CLI opens the complete URI when supplied, otherwise opens the base URI and the user enters the displayed code. The client does not invent a query parameter when the complete URI is absent.
- Verification URLs must have no embedded credentials or fragment. Production supports HTTPS on `app.withflintpay.com`; the configured API origin is also allowed for local development. HTTP is allowed only on loopback. Complete and base verification URLs must have the same origin. Coordinate any separate staging website origin with the CLI agent.

## 2. Website approval

Implement `/cli/activate` with both manual user-code entry and the complete-URI shortcut.

1. Use the existing Flint browser session, or sign in and resume this exact authorization request.
2. Show the short code and require the user to confirm that it matches their terminal. Never approve on GET or automatically because a user is already signed in.
3. Let the user choose an authorized organization, merchant, and sandbox, subject to the CLI request's constraints. Show the requested permissions and explain persistent CLI access. Clearly identify live production access. Provide Approve and Deny.
4. Check CSRF protection, session validity, tenant access, and permission delegation using the normal application authorization rules. The public client ID, scope list, and merchant IDs are not proof of authorization.
5. Approval binds a grant to the user, client, resource audience, exact environment, merchant, sandbox, and permitted scopes. Denial and expiry are terminal. Display success and tell the user to return to the terminal.

No API key creation, browser callback listener, clipboard transfer, or secret token in browser URLs/HTML/storage is involved. Keep short user codes out of browser analytics and referrers where possible.

## 3. Token exchange

Extend `POST /v1/oauth/token` to support this public client and standard form requests, preserving existing partner flows.

Initial token request:

```text
grant_type=urn:ietf:params:oauth:grant-type:device_code
client_id=flint-cli
device_code=<secret-from-device-response>
```

Before approval return HTTP 400 with `{"error":"authorization_pending"}`. For polling too quickly return HTTP 400 with `{"error":"slow_down"}`. The CLI adds five seconds to every subsequent polling interval for that authorization. Return `access_denied` for denial and `expired_token` for expiration or consumed device codes. Implement the other standard OAuth error conditions as appropriate. Never echo tokens or codes in `error_description`.

On approval and successful redemption, return HTTP 200:

```json
{
  "access_token": "<opaque-access-token>",
  "token_type": "Bearer",
  "expires_in": 900,
  "refresh_token": "<opaque-refresh-token>",
  "scope": "payments.payment_intents.read"
}
```

The example scope list is abbreviated. The CLI requires nonempty access and refresh tokens, case-insensitive Bearer token type, and integer `expires_in` from 1 through 86400 seconds. Use short-lived access tokens; 15 minutes is a proposed server policy, not hardcoded in the client. Define and document an appropriate maximum and idle refresh-session lifetime and show the relevant policy during consent. A refresh token is required for this CLI integration even though it is optional in OAuth generally.

Redeem device codes atomically; concurrent successful exchanges must not create multiple grants. Recheck the approving user's permissions and session state before issuance. Prefer minting credentials on redemption rather than approval so abandoned authorizations do not issue unused credentials. The CLI does **not** use the previous proposal's JSON envelopes or custom idempotency-response caching.

## 4. Token refresh and revocation

Refresh request to `/v1/oauth/token`:

```text
grant_type=refresh_token
client_id=flint-cli
refresh_token=<current-refresh-token>
```

Return the same standard token response fields as initial issuance. **Rotate the refresh token on every successful refresh** and return the replacement. Reuse detection must revoke the compromised grant/family. Enforce expiry, user access removal, grant revocation, client binding, audience, and tenant constraints. Refresh must never broaden scopes or change grant/environment/merchant/sandbox identity. Scope reductions are allowed, subject to a nonempty usable scope set for this client.

The CLI refreshes within 30 seconds of access-token expiry. It serializes refresh with logout and credential replacement using a cross-process lock and re-reads Keychain after acquiring the lock. It immediately persists the rotated record in Keychain with verification pending, then validates the access token against the original grant context before permitting business requests. A temporary context-endpoint failure retains the new tokens for a later verification attempt instead of revoking the session. Refresh failures return an actionable error; `invalid_grant` requires a fresh browser login.

The CLI sends each refresh request once, without generic transport retries or an idempotency header. If a response is lost after rotation, a subsequent use of the old refresh token may require reauthentication and family revocation. Do not disable reuse detection to conceal this failure. Test and document this behavior.

Revocation: `POST /v1/oauth/revoke`, standard form:

```text
client_id=flint-cli
token=<refresh-token>
token_type_hint=refresh_token
```

Return HTTP 200 with an empty body for successful or already-invalid token revocation. The CLI treats other success statuses, including 202 and 204, as unconfirmed revocation and retains the local credential. Revoking a refresh token must revoke its full CLI grant/family, including issued access tokens. Keep sufficient hashed family linkage for a previously rotated refresh token to revoke its own family; the CLI may only have the old token after a malformed refresh response. Never allow one client or token family to revoke another.

`flint auth logout --confirm` revokes first, then deletes the local credential and profile context. If revocation fails, the CLI retains the local record so logout can be retried and reports failure. Existing API-key logout remains local-only. Browser-login validation/storage failures and terminal refresh failures attempt bounded grant revocation; transient refresh-verification or verification-state-save failures retain the staged tokens for retry; if cleanup fails the CLI directs the user to revoke the session on the website. Add a website view to list and revoke CLI sessions. Replacing a local credential does not automatically revoke other previously created sessions; expose them clearly for management.

## 5. API authorization and authoritative session context

OAuth access tokens for this client must authorize the same intended merchant APIs as the approved CLI scopes, including webhook listening. Enforce permissions and tenant/audience/environment boundaries independently on every endpoint. Support this grant without changing existing API-key, partner-token, or checkout-session behavior.

Extend `GET /v1/developer/auth-context` to accept these access tokens. This endpoint keeps its existing Flint envelope:

```json
{
  "data": {
    "auth_type": "oauth",
    "oauth_grant_id": "grant_123",
    "environment": "sandbox",
    "merchant_id": "mer_123",
    "sandbox_id": "sandbox_123",
    "scopes": ["payments.payment_intents.read"]
  },
  "meta": {"api_version": "<current-api-version>"}
}
```

Required for CLI OAuth sessions: `auth_type=oauth`, nonempty stable `oauth_grant_id`, nonempty merchant ID and scope list, environment `sandbox` or `live`, and nonempty sandbox ID in sandbox mode. `api_key_id` must not be required for OAuth. Grant ID and tenant/environment bindings remain stable across token refresh; logout/relogin creates a distinct grant. This stable grant identity isolates command history without breaking history references on refresh.

The CLI validates this context at login, refresh, and normal command authentication. It enforces `--live` and explicit merchant/sandbox guards against the server result. Ordinary requests refresh tokens before sending, including pagination and webhook reconnects. Long-lived webhook streams should close when the access token/grant is no longer valid, allowing the CLI to reconnect with a refreshed token. The CLI deliberately does not replay API writes on HTTP 401, avoiding duplicate business operations.

## 6. Storage, security, and deployment

- Use durable shared authorization/grant storage with TTLs and atomic state transitions across API instances. Hash device/user codes and refresh tokens where lookup permits; keep refresh-token-family lineage for reuse detection and revocation.
- Rate-limit authorization creation, code lookup, approval attempts, token polling, and refresh. Audit approval/denial, issuance, refresh/reuse detection, and revocation without recording secrets.
- Exclude OAuth request and response bodies from logging, tracing, analytics, and error reports. Tokens and device codes never belong in URLs. Verification links may contain only browser authorization context, never device or token secrets.
- No secrets in CLI config files. The client stores a versioned OAuth token record in Keychain; existing raw API-key records remain compatible. Stored OAuth credentials are bound to their issuing API base URL; a conflicting `FLINT_BASE_URL` is rejected before sending tokens.
- Inspect existing auth/session/partner/token middleware and reuse appropriate primitives. Add migrations, storage, endpoints, website UI, permissions, and tests. Do not weaken tenant or scope checks to make login pass.
- Update the canonical OpenAPI source with OAuth form endpoints/responses and OAuth-aware auth-context schemas, regenerate public artifacts, and give the CLI agent the updated snapshot. Do not hand-edit flint-cli's embedded schema to pretend the server is implemented.

Acceptance tests: full sandbox and explicit live login; logged-out resume; code confirmation; deny/expire/slow-down; invalid client/scope/code; CSRF; cross-tenant denial; concurrent redemption; refresh rotation and reuse; lost refresh responses; revoked or removed users; session expiry; API audience/scopes; auth-context identity stability; old-family-token revocation; no credential logging; API-key/partner-flow compatibility; and revoked stream reconnection behavior.

Finally run the built CLI against the deployed backend: `auth login`, `auth status`, `doctor`, a scoped API read, forced token expiry/refresh, concurrent CLI commands, webhook reconnect, and `auth logout --confirm`. Confirm validation/storage-failure cleanup revokes only the new session. Report tests, migration/deployment order, and remaining integration gaps.

## Documentation rollout

Make `flint auth login` the recommended local-development path in the hosted CLI guide, command reference, onboarding pages, and dashboard setup instructions. Quickstarts should show `flint auth login` then `flint doctor`. Explain browser approval, automatic refresh, `--no-open`, profiles, explicit `--live`, and OAuth session revocation on logout. Retain `flint auth import` for manual keys, `FLINT_API_KEY` for CI, and `flint signup` for terminal account creation. Remove the pending-backend notices from both CLI READMEs only after end-to-end verification against the deployed service.
