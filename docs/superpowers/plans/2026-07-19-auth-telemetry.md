# Auth Telemetry Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Capture per-login/token telemetry (IP, device id, user-agent) on `t_tokens` + `t_users` as groundwork for a later rate-limiter.

**Architecture:** A `V10` migration adds nullable columns; entities gain matching fields. `AuthController` builds a `ClientMeta` from the servlet request and passes it to `AuthService`, which stamps it onto every issued `Token` and updates `t_users.last_login_*` on login. The SPA sends a stable `X-Device-Id` header.

**Tech Stack:** Spring Boot 3, Flyway, JUnit 5 + Mockito; Vue 3 + Vitest; axios.

**Spec:** `docs/superpowers/specs/2026-07-19-auth-telemetry-design.md`

## Global Constraints

- `ddl-auto: validate` — every new DB column has a matching entity field in the same task. New columns are nullable + `ADD COLUMN IF NOT EXISTS` in a new `Vn` migration (never edit an applied one).
- Column sizes: ip `VARCHAR(45)`, device_hash `VARCHAR(128)`, user_agent `VARCHAR(512)`. Entity `@Column(length=…)` must match; truncate values in code before persisting.
- Build/test with Maven: `./mvnw test -Dtest='…'` (NOT gradle). Frontend: `npm run type-check`, `npm run test`, `npm run lint`.
- Branch `feature/auth-telemetry` off `feature/admin-users-crud` in both repos. Frequent commits.

---

# Part A — Backend (`judge-api`)

### Task A1: V10 migration + entity fields

**Files:**
- Create: `src/main/resources/db/migration/V10__add_auth_telemetry_columns.sql`
- Modify: `src/main/java/vn/thanhtuanle/entity/Token.java`
- Modify: `src/main/java/vn/thanhtuanle/entity/User.java`

- [ ] **Step 1:** Write the migration:
```sql
-- V10: auth telemetry columns (IP / device / user-agent). Additive + idempotent, all nullable.
ALTER TABLE public.t_tokens ADD COLUMN IF NOT EXISTS ip VARCHAR(45);
ALTER TABLE public.t_tokens ADD COLUMN IF NOT EXISTS device_hash VARCHAR(128);
ALTER TABLE public.t_tokens ADD COLUMN IF NOT EXISTS user_agent VARCHAR(512);
ALTER TABLE public.t_users  ADD COLUMN IF NOT EXISTS last_login_ip VARCHAR(45);
ALTER TABLE public.t_users  ADD COLUMN IF NOT EXISTS last_login_device_hash VARCHAR(128);
```
- [ ] **Step 2:** Add to `Token` (after `expired`):
```java
@Column(length = 45)
private String ip;

@Column(name = "device_hash", length = 128)
private String deviceHash;

@Column(name = "user_agent", length = 512)
private String userAgent;
```
- [ ] **Step 3:** Add to `User` (near `lastLogin`):
```java
@Column(name = "last_login_ip", length = 45)
private String lastLoginIp;

@Column(name = "last_login_device_hash", length = 128)
private String lastLoginDeviceHash;
```
- [ ] **Step 4:** `./mvnw compile -q` → PASS. Commit `feat(telemetry): V10 columns + entity fields`.

### Task A2: ClientIpResolver + ClientMeta (+ refactor JudgeServerController)

**Files:**
- Create: `src/main/java/vn/thanhtuanle/common/util/ClientIpResolver.java`
- Create: `src/main/java/vn/thanhtuanle/common/util/ClientMeta.java`
- Modify: `src/main/java/vn/thanhtuanle/judgeserver/JudgeServerController.java` (use the resolver)
- Test: `src/test/java/vn/thanhtuanle/common/util/ClientMetaTest.java`

**Interfaces produced:**
- `String ClientIpResolver.resolve(HttpServletRequest)`
- `record ClientMeta(String ip, String deviceHash, String userAgent)` + `static ClientMeta from(HttpServletRequest)`

- [ ] **Step 1 (test):** `ClientMetaTest` with a stubbed `HttpServletRequest` (Mockito `mock(HttpServletRequest.class)`):
  - XFF `"1.2.3.4, 10.0.0.1"` → `resolve` returns `"1.2.3.4"`.
  - XFF null, `getRemoteAddr()` `"9.9.9.9"` → `"9.9.9.9"`.
  - `X-Device-Id: "dev-123"` → `from().deviceHash() == "dev-123"`.
  - `X-Device-Id` blank, `User-Agent: "UA"` → `deviceHash` equals `sha256Hex("UA")` truncated to 128 (non-null, != "UA").
  - over-long UA (600 chars) → `userAgent().length() == 512`.
- [ ] **Step 2:** Run → FAIL.
- [ ] **Step 3:** Implement:
```java
// ClientIpResolver
public final class ClientIpResolver {
    private ClientIpResolver() {}
    public static String resolve(HttpServletRequest request) {
        String xff = request.getHeader("X-Forwarded-For");
        if (xff != null && !xff.isBlank()) return xff.split(",")[0].trim();
        return request.getRemoteAddr();
    }
}
```
```java
// ClientMeta
public record ClientMeta(String ip, String deviceHash, String userAgent) {
    private static final int UA_MAX = 512;
    private static final int DEV_MAX = 128;

    public static ClientMeta from(HttpServletRequest request) {
        String ip = ClientIpResolver.resolve(request);
        String ua = truncate(request.getHeader("User-Agent"), UA_MAX);
        String deviceId = request.getHeader("X-Device-Id");
        String deviceHash = (deviceId != null && !deviceId.isBlank())
                ? truncate(deviceId.trim(), DEV_MAX)
                : (ua != null ? truncate(sha256Hex(ua), DEV_MAX) : null);
        return new ClientMeta(ip, deviceHash, ua);
    }

    private static String truncate(String s, int max) {
        if (s == null) return null;
        return s.length() > max ? s.substring(0, max) : s;
    }

    private static String sha256Hex(String s) {
        try {
            byte[] d = java.security.MessageDigest.getInstance("SHA-256")
                    .digest(s.getBytes(java.nio.charset.StandardCharsets.UTF_8));
            StringBuilder sb = new StringBuilder(d.length * 2);
            for (byte b : d) sb.append(String.format("%02x", b));
            return sb.toString();
        } catch (java.security.NoSuchAlgorithmException e) {
            return null;
        }
    }
}
```
- [ ] **Step 4:** Refactor `JudgeServerController`'s inline XFF logic to `ClientIpResolver.resolve(request)` (remove the duplicated method). `./mvnw compile -q` → PASS.
- [ ] **Step 5:** Run test → PASS. Commit `feat(telemetry): ClientIpResolver + ClientMeta`.

### Task A3: Capture wiring (AuthController + AuthService)

**Files:**
- Modify: `src/main/java/vn/thanhtuanle/auth/AuthController.java`
- Modify: `src/main/java/vn/thanhtuanle/auth/AuthService.java`

**Interfaces produced (AuthService):**
- `authenticateWithPassword(String username, String password, ClientMeta meta)`
- `authenticateGoogleUser(String code, ClientMeta meta)`
- `refreshAccessToken(String refreshToken, ClientMeta meta)`
- `savedUserToken(User user, String jwt, TokenType type, ClientMeta meta)` (private)

- [ ] **Step 1:** `AuthController` — add `HttpServletRequest request` to `login`, `refresh`, `googleCallback`; build `ClientMeta meta = ClientMeta.from(request)` and pass to the matching `authService` call.
- [ ] **Step 2:** `AuthService.savedUserToken` — accept `ClientMeta meta` and set `.ip(meta.ip()).deviceHash(meta.deviceHash()).userAgent(meta.userAgent())` on the `Token.builder()`. Thread `meta` through all three public methods to every `savedUserToken` call.
- [ ] **Step 3:** In `authenticateWithPassword` and `authenticateGoogleUser`, next to `user.setLastLogin(now())`, add `user.setLastLoginIp(meta.ip()); user.setLastLoginDeviceHash(meta.deviceHash());`. `refreshAccessToken` does NOT touch `last_login_*`.
- [ ] **Step 4:** Update any existing `AuthService` tests that call these methods to pass a `ClientMeta` (e.g. `new ClientMeta("t","t","t")`); if none exist, skip. `./mvnw test -Dtest='*Auth*,ClientMetaTest' -Dsurefire.failIfNoSpecifiedTests=false` → PASS.
- [ ] **Step 5:** Commit `feat(telemetry): stamp IP/device/UA on tokens + last_login`.

### Task A4: Backend build + live verification

- [ ] **Step 1:** `./mvnw test` → all green.
- [ ] **Step 2:** `docker compose up -d --build judge-api`; confirm Flyway applied `V10`.
- [ ] **Step 3:** `curl` login with `-H 'X-Device-Id: test-device-1' -H 'User-Agent: probe/1.0'`; then
  `SELECT ip, device_hash, user_agent FROM t_tokens ORDER BY created_at DESC LIMIT 2;` shows the values,
  `device_hash = 'test-device-1'`; `SELECT last_login_ip, last_login_device_hash FROM t_users WHERE username='admin';` populated.
- [ ] **Step 4:** Login again WITHOUT `X-Device-Id` → `device_hash` is a 64-hex UA hash, not null.

---

# Part B — Frontend (`judge-portal`)

### Task B1: deviceId util + axios interceptor

**Files:**
- Create: `src/lib/deviceId.ts`
- Test: `src/lib/__tests__/deviceId.spec.ts`
- Modify: `src/api/axiosClient.ts` (request interceptor)

**Interfaces produced:** `getDeviceId(): string`

- [ ] **Step 1 (test):** `deviceId.spec.ts` — first `getDeviceId()` writes a value to `localStorage['deviceId']`; a second call returns the same value; a pre-seeded value is returned unchanged. (Use jsdom localStorage; if `crypto.randomUUID` is missing in the test env, the util's fallback still returns a stable string.)
- [ ] **Step 2:** Implement:
```ts
const KEY = 'deviceId'
export function getDeviceId(): string {
    let id = localStorage.getItem(KEY)
    if (!id) {
        id = (globalThis.crypto?.randomUUID?.() ?? `dev-${Date.now()}-${Math.random().toString(16).slice(2)}`)
        localStorage.setItem(KEY, id)
    }
    return id
}
```
- [ ] **Step 3:** In `axiosClient.ts`, add a request interceptor:
```ts
axiosClient.interceptors.request.use((config) => {
    config.headers['X-Device-Id'] = getDeviceId()
    return config
})
```
(Import `getDeviceId`. If an interceptor block already exists, add the header there.)
- [ ] **Step 4:** `npm run type-check` + `npx vitest run src/lib/__tests__/deviceId.spec.ts` → PASS. Commit `feat(telemetry): send stable X-Device-Id header`.

### Task B2: Frontend build + live verification

- [ ] **Step 1:** `npm run lint` (0 errors), `npm run type-check`, `npm run test` (all pass).
- [ ] **Step 2:** `docker compose up -d --build judge-portal`; log in through the UI; confirm the browser stores `localStorage.deviceId` and that `t_tokens.device_hash` for that login equals it (matches the value the SPA sends).

---

## Self-review notes (author)

- Every spec section maps to a task: V10+entities (A1), ClientMeta/IpResolver (A2), capture wiring (A3), deviceId+interceptor (B1), verification (A4/B2).
- `ClientMeta.from` truncates to the exact column lengths, satisfying `ddl-auto: validate`.
- `last_login_*` updates only on login (A3 step 3), not refresh — matches the spec.
