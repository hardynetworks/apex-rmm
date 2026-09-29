# Authentik SSO setup

Hardy RMM uses standard OpenID Connect (authorization code + PKCE). These steps are for Authentik 2024.x/2025.x; menu names may differ slightly in newer versions.

## 1. Groups

**Directory → Groups → Create**

* `RMM Admins` – full access
* `RMM Technicians` – remote control, scripts, tickets
* `RMM Viewers` – read-only (optional)

Add your users to the right group. The names must match `OIDC_ADMIN_GROUPS`, `OIDC_TECH_GROUPS` and `OIDC_VIEWER_GROUPS` in `.env` (comma-separated lists are allowed).

## 2. Provider

**Applications → Providers → Create → OAuth2/OpenID Provider**

| Field | Value |
|---|---|
| Name | `Hardy RMM` |
| Authorization flow | `default-provider-authorization-implicit-consent` |
| Client type | **Confidential** |
| Redirect URIs | `Strict` – `https://remote.hardyvpn.online/auth/callback` |
| Signing key | `authentik Self-signed Certificate` (**required** – RS256) |
| Scopes (advanced) | `openid`, `email`, `profile` (the default *profile* mapping includes the `groups` claim) |
| Subject mode | Based on the User's hashed ID (default) |

Copy the **Client ID** and **Client Secret**.

> If you leave *Signing key* empty, Authentik signs tokens with HS256 and sign-in fails with "invalid token". Pick a certificate.

## 3. Application

**Applications → Applications → Create**

| Field | Value |
|---|---|
| Name | `Hardy RMM` |
| Slug | `hardy-rmm` |
| Provider | `Hardy RMM` |
| Launch URL | `https://remote.hardyvpn.online/` |

Optionally bind a policy so only the RMM groups can see the app (Hardy RMM also refuses users who are in none of the groups unless `OIDC_DEFAULT_ROLE` is set).

## 4. `.env`

```ini
OIDC_ISSUER=https://<your-authentik-host>/application/o/hardy-rmm/
OIDC_CLIENT_ID=<client id>
OIDC_CLIENT_SECRET=<client secret>
OIDC_ADMIN_GROUPS=RMM Admins
OIDC_TECH_GROUPS=RMM Technicians
OIDC_VIEWER_GROUPS=RMM Viewers
```

The issuer must end with the application slug and a trailing slash — it's shown as *OpenID Configuration Issuer* on the provider page.

`docker compose up -d` to apply. The server logs `sso=true` on startup; if discovery fails it retries for ~30 seconds and then exits with the reason.

## Sign-out

Sign-out from Hardy RMM also ends the Authentik session (RP-initiated logout via the provider's end-session endpoint) and returns to the login page.

## Troubleshooting

| Symptom | Fix |
|---|---|
| "Your account is not in an RMM group" | Add the user to a group listed in `OIDC_*_GROUPS`, or set `OIDC_DEFAULT_ROLE`. Group names are case-insensitive. |
| "Could not complete sign-in" | Client secret wrong, or redirect URI mismatch. The URI must exactly equal `PUBLIC_URL` + `/auth/callback`. |
| "invalid token" | Provider has no signing key (see step 2) or the server clock is off. |
| Redirect loop behind a proxy | Make sure `PUBLIC_URL` uses `https://` and your proxy forwards to the container over HTTP. |
