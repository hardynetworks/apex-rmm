# Authentik SSO setup

Hardy RMM uses standard OpenID Connect (authorization code + PKCE). These steps are for Authentik 2024.x/2025.x; menu names may differ slightly in newer versions.

## 1. Groups

**Directory → Groups → Create**

* `RMM Admins` – full access
* `RMM Technicians` – remote control, scripts, tickets
* `RMM Viewers` – read-only (optional)

Add your users to the right group. The names must match the group fields in Settings → General → Single sign-on (comma-separated lists are allowed).

## 2. Provider

**Applications → Providers → Create → OAuth2/OpenID Provider**

| Field | Value |
|---|---|
| Name | `Hardy RMM` |
| Authorization flow | `default-provider-authorization-implicit-consent` |
| Client type | **Confidential** |
| Redirect URIs | `Strict` – `https://<your-hardy-rmm-host>/auth/callback` (copy it from Settings → General) |
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
| Launch URL | `https://<your-hardy-rmm-host>/` |

Optionally bind a policy so only the RMM groups can see the app (Hardy RMM also refuses users who are in none of the groups unless "Users in no group" is set to a role).

## 4. Hardy RMM

Sign in with your local admin account and open **Settings → General → Single sign-on**:

1. Copy the **Redirect URI** shown there into the provider from step 2, if you haven't already.
2. **Issuer URL:** `https://<your-authentik-host>/application/o/hardy-rmm/`. Authentik shows it as *OpenID Configuration Issuer* on the provider page. Click **Test** to confirm Hardy RMM can reach it.
3. **Client ID** and **Client secret** from step 2.
4. Group names for admin / technician / viewer, if yours differ from the defaults.
5. **Save settings.** The badge turns **Active** and a *Sign in with single sign-on* button appears on the login page; no restart is needed.

Test in a private window before relying on it. Your local admin login keeps working either way.

Prefer config files? The same values can be set as `OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`, `OIDC_ADMIN_GROUPS`, `OIDC_TECH_GROUPS` and `OIDC_VIEWER_GROUPS` in `.env`; they then show as locked in Settings.

## Sign-out

Sign-out from Hardy RMM also ends the Authentik session (RP-initiated logout via the provider's end-session endpoint) and returns to the login page.

## Troubleshooting

| Symptom | Fix |
|---|---|
| "Your account is not in an RMM group" | Add the user to one of the groups configured in Settings, or choose a role for "Users in no group". Group names are case-insensitive. |
| "Could not complete sign-in" | Client secret wrong, or redirect URI mismatch. It must exactly match the Redirect URI shown in Settings (Public URL + `/auth/callback`). |
| "invalid token" | Provider has no signing key (see step 2) or the server clock is off. |
| Redirect loop behind a proxy | Make sure the Public URL in Settings uses `https://` and your proxy forwards to the container over HTTP. |
