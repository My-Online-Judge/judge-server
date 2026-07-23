# Spec C — Login Rate-limit & Access Ban (design)

Date: 2026-07-23
Target repos: judge-api, judge-portal
Depends on: Spec B (auth telemetry — `ClientMeta` ip/device_hash/user_agent, merged to main)

## Goal

Stop login spam/brute-force automatically, and give admins a manual kill-switch to ban an
`ip` or `device_hash` outright. Give admins the visibility (login-attempt log) needed to
decide who to ban.

## Decisions (user-approved)

1. **Scope**: automatic rate-limit on `/auth/login` + manual admin bans enforced on ALL
   requests. Submission throttling is out of scope (future Spec D).
2. **Policy**: count login **failures only** — 5 fails/username, 10 fails/ip,
   10 fails/device_hash within a 5-minute window → lock that key for 15 minutes,
   respond `429` + `Retry-After`. A successful login clears that username's counter.
3. **Ban storage**: `t_access_bans` in Postgres is the source of truth (auditable,
   survives restarts); Redis holds a mirror for O(1) lookup per request
   (multi-instance-safe — the stack already fans out SSE via Redis).
4. **Audit**: every login attempt (password + Google, success and failure) is written to
   `t_login_attempts`; a new admin “Security” page shows attempts (with per-row Ban
   buttons) and current bans.

## Architecture

```
Request → AccessBanFilter (every request; Redis mirror lookup) → 403 ACCESS_BANNED
             ↓
        /auth/login → LoginRateLimiter check → 429 RATE_LIMITED (+ Retry-After)
             ↓ authenticate
        record t_login_attempts (success & failure) + on fail: LoginRateLimiter.recordFailure
```

- **`LoginRateLimiter`** (new component, `vn.thanhtuanle.auth`, modeled on the existing
  `TokenBlocklist` idiom): Redis `INCR` + `EXPIRE`. Keys
  `oj:rl:login:{username|ip|device}:{value}` (5-min TTL counters) and
  `oj:rl:lock:{...}` (15-min TTL locks). Check runs **inside `AuthService`** before
  authentication (username is only known after body parsing; failures are only known
  after authentication) — not in a filter.
- **`AccessBanFilter`** (`OncePerRequestFilter`, registered before `jwtAuthFilter`):
  checks Redis sets `oj:ban:ip` / `oj:ban:device` against `ClientMeta`. Hit → 403
  `ACCESS_BANNED`. Runs before authorization, so bans also cover public
  (`permitAll`) endpoints. WHITE_LIST unchanged.
- **Mirror lifecycle**: rebuilt from `t_access_bans` at boot (`ApplicationReadyEvent`),
  updated synchronously on ban CRUD. Expired bans (`expires_at` past) are skipped at
  rebuild and pruned by the scheduled job; the mirror entry for a temporary ban is
  written with a matching Redis TTL so it self-evicts.

## Database — Flyway V11

- `t_login_attempts`: `id uuid pk, username varchar(64), ip varchar(45),
  device_hash varchar(128), user_agent varchar(512), success boolean not null,
  error_code varchar(64), created_at timestamp not null default now()`.
  Indexes: `(ip, created_at)`, `(username, created_at)`.
  Retention: `@Scheduled` daily prune of rows older than 30 days.
- `t_access_bans`: `id uuid pk, type varchar(10) not null ('IP'|'DEVICE'),
  value varchar(128) not null, reason varchar(255), expires_at timestamp null
  (null = permanent), created_by varchar(64), created_at timestamp not null`.
  Unique `(type, value)`.
- Seed permissions `ban:read`, `ban:create`, `ban:delete` — granted to **both ADMIN and
  SYS_ROOT** (per the V9 convention comment).

## API — `/api/v1/security`

| Endpoint | Permission | Behavior |
|---|---|---|
| `GET /security/attempts` | `ban:read` | paginated; filters: ip, username, success, date range (JPA Specification, same pattern as Users list) |
| `GET /security/bans` | `ban:read` | paginated list of bans (active first) |
| `POST /security/bans` | `ban:create` | body `{type, value, reason, expiresAt?}`; 409 `BAN_EXISTED` on duplicate; **409 `BAN_SELF` when the target equals the caller's current ip/device_hash** (guardrail, same spirit as `USER_SELF_MODIFY`); validates type/value non-blank |
| `DELETE /security/bans/{id}` | `ban:delete` | 404 if missing; removes from mirror immediately |

New `ErrorCode`s: `RATE_LIMITED (429)`, `ACCESS_BANNED (403)`, `BAN_EXISTED (409)`,
`BAN_SELF (409)`, `BAN_INVALID (400)`.

Note: services validate explicitly — no Bean Validation provider on the classpath, so
`@Valid` is a no-op in this codebase.

## Frontend — Admin “Security” page

Route `/admin/security` (permission `ban:read` via `authStore.hasPermission`), two tabs
following the AdminUsersPage patterns (`useFetch`, debounced filters, shadcn-vue table,
`triggerToast`):

- **Attempts tab**: paginated table (time, username, ip, device_hash, result badge,
  error code), filters (ip, username, result, date range). Per-row actions **Ban IP** /
  **Ban device** open a dialog: reason + duration (1h / 24h / 7d / permanent).
- **Bans tab**: table (type, value, reason, created_by, expires_at or “permanent”),
  Unban button with confirm dialog, and a “New ban” dialog for manual entry.

New `securityService.ts` (list attempts, list/create/delete bans) + types; sidebar nav
item gated on `ban:read`.

## Metrics

`OjMetrics` gains `oj_login_rate_limited_total` and `oj_request_banned_total` counters,
incremented by the limiter and the filter respectively.

## Guardrails

- No SYS_ROOT bypass for the **rate limiter** (a 15-minute lock is acceptable; a bypass
  would leave root brute-forceable).
- Self-ban prevention on **manual bans** (above). Unbanning is always possible via
  another admin session or directly in DB.
- The ban filter fails **open** if Redis is down (log + allow) — availability over
  enforcement for this feature; the limiter likewise degrades to allow.

## Testing

- Unit: `LoginRateLimiter` (counting, lock threshold per key type, success reset,
  Redis-down fail-open), `AccessBanFilter` (hit/miss/expired/Redis-down),
  ban service (duplicate, self-ban 409, mirror sync, delete), attempt recording in the
  password and Google flows (refresh is NOT logged and NOT rate-limited — it would flood
  the table — but IS ban-covered like every request).
- Frontend: securityService spec (vi.hoisted mock idiom), validators if any.
- Live verification: 6 failed logins → 429 with Retry-After; correct login on a locked
  username → still 429 until TTL; ban an ip → 403 on any endpoint; unban → restored;
  `t_login_attempts` rows present for both outcomes; metrics visible at
  `/actuator/prometheus`.

## Out of scope

- Submission rate-limiting (Spec D candidate).
- CAPTCHA / progressive backoff.
- CIDR-range bans (single ip values only for now).
