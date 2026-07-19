# Auth Telemetry (rate-limit groundwork) — Design Spec

- **Date:** 2026-07-19
- **Status:** Approved (design). Ready for implementation plan.
- **Repos:** `judge-api` (Spring Boot 3), `judge-portal` (Vue 3 SPA)
- **Follow-up (Spec C, out of scope):** rate-limit enforcement — Redis counters, block-by-ip/device, a login-attempts audit log, and a "your sessions" UI.

## 1. Motivation

Record where each login/token comes from (IP, device, user-agent) so a later feature can
throttle or block abusive IPs/devices. This spec only **captures** the data as columns; it does
not enforce anything.

## 2. Scope

**In scope:** add telemetry columns to `t_tokens` + `t_users`, capture them on successful
login (password + Google) and refresh, and have the SPA send a stable device id.

**Out of scope:** rate-limit enforcement, blocking, a `t_login_attempts` table (would be needed
to track *failed* attempts — deferred to Spec C), and any session-management UI.

Note: `revokeAllUserTokens` deletes prior tokens on each login, so `t_tokens` holds only the
*current* session's telemetry — fine for "where is this user logged in from", not a substitute
for the attempt log that real spam-blocking (Spec C) will need.

## 3. Decisions log

| Decision | Choice |
|---|---|
| device_hash source | Client sends `X-Device-Id` (a UUID the SPA stores in localStorage); server falls back to `sha256(User-Agent)` when the header is absent. |
| Scope | Columns only on `t_users` + `t_tokens`; capture on success. No attempts table. |
| Capture points | Every token issue (login + refresh) sets token telemetry; `last_login_*` on `t_users` updates on login only. |

## 4. Migration `V10`

`V10__add_auth_telemetry_columns.sql` — additive + idempotent (`ADD COLUMN IF NOT EXISTS`),
all nullable so existing rows are unaffected. `ddl-auto: validate`, so the entities gain matching
fields in the same change.

```sql
ALTER TABLE public.t_tokens ADD COLUMN IF NOT EXISTS ip VARCHAR(45);
ALTER TABLE public.t_tokens ADD COLUMN IF NOT EXISTS device_hash VARCHAR(128);
ALTER TABLE public.t_tokens ADD COLUMN IF NOT EXISTS user_agent VARCHAR(512);
ALTER TABLE public.t_users  ADD COLUMN IF NOT EXISTS last_login_ip VARCHAR(45);
ALTER TABLE public.t_users  ADD COLUMN IF NOT EXISTS last_login_device_hash VARCHAR(128);
```

## 5. Backend

### 5.1 Entities
- `Token`: `@Column(length=45) String ip`, `@Column(name="device_hash", length=128) String deviceHash`,
  `@Column(name="user_agent", length=512) String userAgent`.
- `User`: `@Column(name="last_login_ip", length=45) String lastLoginIp`,
  `@Column(name="last_login_device_hash", length=128) String lastLoginDeviceHash`.

### 5.2 Client metadata
- `ClientIpResolver.resolve(HttpServletRequest)` — first IP of `X-Forwarded-For`, else
  `getRemoteAddr()`. **Refactor** the duplicated logic in `JudgeServerController` to call this.
- `ClientMeta` (record: `ip`, `deviceHash`, `userAgent`) + `ClientMeta.from(HttpServletRequest)`:
  - `ip` = `ClientIpResolver.resolve(request)`
  - `userAgent` = `request.getHeader("User-Agent")`, truncated to 512
  - `deviceHash` = `X-Device-Id` header (trimmed, truncated to 128) if present and non-blank;
    otherwise `sha256Hex(userAgent)` truncated to 128 (null if no UA)

### 5.3 Capture wiring
- Controllers own the servlet request. Add `HttpServletRequest request` to `AuthController.login`,
  `refresh`, and `googleCallback`; build `ClientMeta.from(request)` and pass it down.
- `AuthService`:
  - `authenticateWithPassword(username, password, ClientMeta meta)`
  - `authenticateGoogleUser(code, ClientMeta meta)`
  - `refreshAccessToken(refreshToken, ClientMeta meta)`
  - `savedUserToken(user, jwt, type, ClientMeta meta)` — sets `token.ip/deviceHash/userAgent`.
  - The two **login** flows also set `user.lastLoginIp` / `user.lastLoginDeviceHash` next to the
    existing `setLastLogin(now())`. Refresh does **not** touch `last_login_*`.

## 6. Frontend

- `lib/deviceId.ts`: `getDeviceId()` returns `localStorage['deviceId']`, generating and storing a
  `crypto.randomUUID()` on first call (stable per browser).
- `api/axiosClient.ts`: request interceptor adds `config.headers['X-Device-Id'] = getDeviceId()`.

## 7. Testing

- **Backend (Mockito + a mock/stub `HttpServletRequest`):**
  - `ClientIpResolver`: XFF present (takes first, trims) / XFF absent (falls back to remote addr).
  - `ClientMeta.from`: uses `X-Device-Id` when present; falls back to a UA hash when absent;
    truncates over-long UA / device id.
- **Frontend (vitest):** `getDeviceId` returns a stable value and persists to localStorage.
- **Live:** log in, then `SELECT ip, device_hash, user_agent FROM t_tokens` and
  `SELECT last_login_ip, last_login_device_hash FROM t_users` show the captured values; with the
  SPA sending `X-Device-Id`, `device_hash` equals that id.

## 8. Rollout

- Stacks on `feature/admin-users-crud`. New branch `feature/auth-telemetry`.
- Flyway applies `V10` on boot; columns are nullable so no backfill needed.
- No API contract change for clients other than the new (optional) `X-Device-Id` request header.
