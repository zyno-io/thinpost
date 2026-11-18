---
title: Operator authentication
description: Connect the Thinpost operator console and admin API to any OpenID Connect provider, with claim mappings for Microsoft Entra, Keycloak, Auth0 and Okta.
---

# Operator authentication

Operators sign in to the [console](/guide/console) with OpenID Connect. The control plane verifies the resulting **JWT access token** on every admin API call. Thinpost keeps no operator passwords and no local accounts: an operator is whoever your identity provider says holds the administrator role.

Any provider that issues signed JWT access tokens works. This page lists what Thinpost requires, the settings, and worked examples for Microsoft Entra, Keycloak, Auth0 and Okta. The configuration lives in the `thinpost-operator-config` ConfigMap; see [Helm: control](/deploy/helm-control).

An app API key is never accepted as an operator identity.

## Requirements

The token verifier checks every one of these. A token that fails any of them is rejected.

| Check | Requirement |
| --- | --- |
| Issuer | The `iss` claim equals `THINPOST_OIDC_ISSUER` exactly, character for character. The issuer must be an HTTPS URL with no query or fragment. |
| JWKS | `THINPOST_OIDC_JWKS_URL` is an HTTPS URL. Keys are fetched from it and refreshed every five minutes. Redirects are refused. Only keys whose `use` is `sig` or absent are used. |
| Audience | The `aud` claim contains `THINPOST_OIDC_AUDIENCE`. This must name the API, **not** the browser client: startup fails if `THINPOST_OIDC_AUDIENCE` equals `THINPOST_OIDC_CLIENT_ID`, so an ID token cannot pass. |
| Algorithm | `RS256`, `ES256`, `EdDSA` or `PS256`, from `THINPOST_OIDC_ALGORITHMS`. The default is `RS256`. |
| Times | `exp` and `iat` are present. 30 seconds of clock leeway is allowed. |
| Subject | `sub` is present and non-empty. |
| Role | The configured roles claim contains `THINPOST_OIDC_ADMIN_ROLE`. |
| Scope | The token carries `THINPOST_OIDC_ADMIN_SCOPE`, unless you opt in to role-only mode. |

The token must be at most 8192 bytes.

The identity provider must put the role or group claim in the **access token**, not only in the ID token, and must issue the API audience.

## Settings

| Variable | Required | Meaning |
| --- | --- | --- |
| `THINPOST_OIDC_ISSUER` | Yes | Exact issuer URL. |
| `THINPOST_OIDC_JWKS_URL` | Yes | HTTPS URL of the signing keys. |
| `THINPOST_OIDC_AUDIENCE` | Yes | The API audience the access token is issued for. |
| `THINPOST_OIDC_ADMIN_ROLE` | Yes | The role or group value that marks an administrator. |
| `THINPOST_OIDC_ROLES_CLAIM` | No | Claim that carries roles or groups. Default `roles`. See below. |
| `THINPOST_OIDC_ADMIN_SCOPE` | One of the two | The scope an administrator token must carry. |
| `THINPOST_OIDC_ADMIN_SCOPE_OPTIONAL` | One of the two | Set to exactly `true` for role-only mode. |
| `THINPOST_OIDC_ALGORITHMS` | No | Accepted algorithms, comma- or whitespace-separated. Default `RS256`. |
| `THINPOST_OIDC_CLIENT_ID` | Yes | The browser client's public ID, shown to the console. |
| `THINPOST_OIDC_SCOPES` | Yes | Space-separated scopes the console requests. Must include `openid`. |
| `THINPOST_CONSOLE_CONNECT_ORIGINS` | No | Extra origins the console may call; see [Console policy](#console-content-security-policy). |

Startup fails with a generic configuration error when a required value is missing or invalid.

### How claims are read

- **Scopes.** The token's scopes are the union of two claims. `scope` is the standard space-delimited string (RFC 8693 and 9068); an array there counts as no scopes. `scp` may be a space-delimited string or an array of strings. The required scope may be in either claim. Other types count as no scopes.
- **Roles.** `THINPOST_OIDC_ROLES_CLAIM` is a claim name or a dotted path into nested objects, such as `realm_access.roles`. The full string is looked up first as a top-level claim, so names that contain dots, such as `https://example.com/roles`, work. Only if that claim is absent is the dotted path walked. The value may be an array of strings or a single string, which is one role and is never split. Any other type means no roles. The value must be at most 256 characters, contain no whitespace and have no empty path segment.
- **Algorithms.** Each entry must be one of the four algorithms. Duplicate and empty entries fail startup with an error that names the variable but not its value.
- **Administrator.** A principal is an administrator when the token carries the admin role in the roles claim and the admin scope in its scopes.

### Scope or role-only

By default you set `THINPOST_OIDC_ADMIN_SCOPE` and both the scope and the role are required. If your provider does not give you a convenient scope to require, set `THINPOST_OIDC_ADMIN_SCOPE_OPTIONAL=true` and leave `THINPOST_OIDC_ADMIN_SCOPE` unset or empty. Setting both fails startup, and any value other than exactly `true` fails startup.

In role-only mode the role decides, but the token must still carry a non-empty `scope` or `scp` claim. That excludes ID tokens and Entra app-only tokens. Client-credentials tokens from Keycloak, Auth0 and Okta do carry scopes, so in role-only mode **any client that holds the admin role is an administrator**. Never grant the admin role to a service account.

## Provider examples

Each example gives the access-token claims Thinpost expects and the matching settings. Provider consoles change; use these as a map of which setting corresponds to which claim, and check your provider's documentation for the exact clicks. Decode a real access token (for example with `jwt.io` on a non-production token, or `jq` on the payload) and compare `iss`, `aud`, the scopes and the roles claim with your settings before you go live.

In every example the settings go into `thinpost-operator-config`. Replace the placeholders with your own values.

### Microsoft Entra ID

Create two app registrations: an **API** registration that defines the audience, a scope and an app role, and a **single-page application** registration for the console.

On the API registration:

1. Expose a scope named `access`.
2. Add an app role with the value `Thinpost.Admin`, allowed for users, and assign it to your operators or an operator group.
3. In the manifest, set `accessTokenAcceptedVersion` to `2` so tokens carry the v2 issuer and the API's client ID as the audience.

On the SPA registration, add the redirect URI `https://thinpost.example.com/auth/callback`, grant it the `access` scope and enable the authorization code flow with PKCE.

Access-token claims:

```json
{
  "iss": "https://login.microsoftonline.com/<tenant-id>/v2.0",
  "aud": "<api-application-client-id>",
  "scp": "access",
  "roles": ["Thinpost.Admin"]
}
```

Settings:

```yaml
THINPOST_OIDC_ISSUER: https://login.microsoftonline.com/<tenant-id>/v2.0
THINPOST_OIDC_JWKS_URL: https://login.microsoftonline.com/<tenant-id>/discovery/v2.0/keys
THINPOST_OIDC_AUDIENCE: <api-application-client-id>
THINPOST_OIDC_ADMIN_SCOPE: access
THINPOST_OIDC_ADMIN_ROLE: Thinpost.Admin
THINPOST_OIDC_CLIENT_ID: <spa-application-client-id>
THINPOST_OIDC_SCOPES: "openid api://<api-application-client-id>/access"
```

`THINPOST_OIDC_ROLES_CLAIM` keeps its default `roles`.

### Keycloak

Use a realm, an OIDC client for the console (public, authorization code with PKCE) and a client scope for the API.

1. Create a realm role `thinpost-admin` and assign it to your operators.
2. Create a client scope `thinpost-admin` and add it to the console client as an optional or default scope.
3. Add an **Audience** mapper to that scope, targeting the API audience, with "Add to access token" on.
4. The default roles mapper puts realm roles in `realm_access.roles` in the access token.

Access-token claims:

```json
{
  "iss": "https://keycloak.example.com/realms/<realm>",
  "aud": "thinpost-api",
  "scope": "openid thinpost-admin",
  "realm_access": { "roles": ["thinpost-admin"] }
}
```

Settings:

```yaml
THINPOST_OIDC_ISSUER: https://keycloak.example.com/realms/<realm>
THINPOST_OIDC_JWKS_URL: https://keycloak.example.com/realms/<realm>/protocol/openid-connect/certs
THINPOST_OIDC_AUDIENCE: thinpost-api
THINPOST_OIDC_ADMIN_SCOPE: thinpost-admin
THINPOST_OIDC_ROLES_CLAIM: realm_access.roles
THINPOST_OIDC_ADMIN_ROLE: thinpost-admin
THINPOST_OIDC_CLIENT_ID: thinpost-console
THINPOST_OIDC_SCOPES: "openid thinpost-admin"
```

### Auth0

Create an **API** whose identifier is the audience, and a **single-page application** for the console. Auth0 issues a JWT access token only when the request names a custom API audience. The console does not send an `audience` parameter, so set your API as the tenant's **default audience** in the tenant settings.

Auth0 does not put roles in access tokens by itself. Add a post-login Action that writes them under a namespaced claim, for example `https://example.com/roles`, and assign operators a role named `admin`.

Access-token claims:

```json
{
  "iss": "https://example.us.auth0.com/",
  "aud": "https://thinpost.example.com/api",
  "scope": "openid",
  "https://example.com/roles": ["admin"]
}
```

Settings (role-only, see above):

```yaml
THINPOST_OIDC_ISSUER: https://example.us.auth0.com/
THINPOST_OIDC_JWKS_URL: https://example.us.auth0.com/.well-known/jwks.json
THINPOST_OIDC_AUDIENCE: https://thinpost.example.com/api
THINPOST_OIDC_ADMIN_SCOPE_OPTIONAL: "true"
THINPOST_OIDC_ROLES_CLAIM: https://example.com/roles
THINPOST_OIDC_ADMIN_ROLE: admin
THINPOST_OIDC_CLIENT_ID: <spa-client-id>
THINPOST_OIDC_SCOPES: "openid"
```

The Auth0 issuer ends in a slash, and Thinpost compares it exactly. Copy the `iss` value from a decoded token.

### Okta

Use an Okta **custom authorization server** (the org authorization server does not issue access tokens that carry your own audience), an OIDC single-page application for the console and a group for administrators.

1. On the authorization server, set the audience to your API identifier.
2. Add a **groups claim** to the access token, filtered to your admin group, named `groups`.
3. Assign operators to the group `thinpost-admins`.

Access-token claims:

```json
{
  "iss": "https://example.okta.com/oauth2/<server-id>",
  "aud": "api://thinpost",
  "scp": ["openid"],
  "groups": ["thinpost-admins"]
}
```

Settings (role-only):

```yaml
THINPOST_OIDC_ISSUER: https://example.okta.com/oauth2/<server-id>
THINPOST_OIDC_JWKS_URL: https://example.okta.com/oauth2/<server-id>/v1/keys
THINPOST_OIDC_AUDIENCE: api://thinpost
THINPOST_OIDC_ADMIN_SCOPE_OPTIONAL: "true"
THINPOST_OIDC_ROLES_CLAIM: groups
THINPOST_OIDC_ADMIN_ROLE: thinpost-admins
THINPOST_OIDC_CLIENT_ID: <spa-client-id>
THINPOST_OIDC_SCOPES: "openid"
```

## The console's sign-in settings

The console is a single-page application served from the same origin as the API. It reads its sign-in configuration from the public endpoint `GET /api/v1/config`:

```json
{
  "oidc": {
    "issuer": "https://keycloak.example.com/realms/<realm>",
    "client_id": "thinpost-console",
    "scopes": ["openid", "thinpost-admin"]
  }
}
```

The endpoint needs no authentication and contains no secret. It answers `503` until the issuer, client ID and scopes form a valid configuration: an HTTPS issuer without a query or fragment, a client ID of at most 256 characters, and one to 16 scopes (no whitespace inside a scope) that include `openid`. The console then shows that sign-in is not set up.

Register these URLs with your identity provider:

| Setting | Value |
| --- | --- |
| Redirect URI | `https://thinpost.example.com/auth/callback` |
| Post-logout redirect URI | `https://thinpost.example.com/` |
| Flow | Authorization code with PKCE. The client is public and has no secret. |

The console keeps the access token in the browser's local storage and does not use refresh tokens or silent renewal. When the token expires, the session ends and the operator signs in again, so a short access-token lifetime means frequent sign-ins. Removing an operator's role stops them at their next token, not mid-session.

## Console content security policy

Every console response carries a strict `Content-Security-Policy`:

```text
default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline';
img-src 'self' data: blob:; font-src 'self'; connect-src 'self' <issuer-origin>;
frame-src 'none'; child-src 'none'; worker-src 'none'; object-src 'none';
base-uri 'none'; form-action 'none'; frame-ancestors 'self'; manifest-src 'self'
```

(Shown wrapped; it is one header.) The only external connect target is the origin of `THINPOST_OIDC_ISSUER`, used for OIDC discovery and token requests. There is no `unsafe-eval`, and `img-src` allows no remote images, so remote images and external CSS in rendered email are blocked. Inline styles are allowed because email HTML needs them.

If your provider's token or discovery endpoints sit on a different origin than the issuer, add that origin to `THINPOST_CONSOLE_CONNECT_ORIGINS`, a comma-separated list. Each entry must be a bare `https://host` or `https://host:port`. Paths, queries, user information, wildcards and other schemes fail startup.

```yaml
THINPOST_CONSOLE_CONNECT_ORIGINS: https://login.example.net,https://tokens.example.net
```

The console files and the policy come with the control image. The operator API itself answers with `Content-Security-Policy: default-src 'none'; frame-ancestors 'none'; sandbox`, `X-Content-Type-Options: nosniff` and `Referrer-Policy: no-referrer`.

## What is checked at startup and at runtime

- Startup fails when the OIDC settings are incomplete or invalid. The admin API otherwise fails closed: without a working verifier every call returns an error.
- The control plane downloads the signing keys at start and fails to start if it cannot, so the JWKS endpoint must be reachable from the pods. After that, keys refresh every five minutes in the background. `/operator-readyz` reports recent database checks. It does not report identity-provider health, so monitor sign-in separately.
- Operator actions are audited. See [Data handling](/security/data-handling).

## Next

- [Helm: control](/deploy/helm-control)
- [Operator console](/guide/console)
- [Threat model](/security/threat-model)
