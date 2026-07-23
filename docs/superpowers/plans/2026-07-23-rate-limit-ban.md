# Login Rate-limit & Access Ban (Spec C) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Automatically lock out brute-force login attempts (per username/ip/device_hash) and let admins manually ban an ip or device_hash across ALL requests, with a login-attempt audit log and an admin Security page.

**Architecture:** A Redis `INCR+EXPIRE` failure counter (`LoginRateLimiter`) checked inside `AuthService`; a servlet filter (`AccessBanFilter`) registered BEFORE the Spring Security chain that consults a Redis mirror of the `t_access_bans` table; every password/Google login writes `t_login_attempts`. New `/api/v1/security` endpoints + an `/admin/security` page (2 tabs) in the portal.

**Tech Stack:** Spring Boot 3 (Maven — `./mvnw`, never gradle), Spring Data JPA + Flyway (V11, `ddl-auto: validate`), `StringRedisTemplate`, Micrometer; Vue 3 + shadcn-vue + Pinia + vitest (node env, no DOM).

## Global Constraints

- Branch `feature/rate-limit-ban` off **`main`** in BOTH repos (`judge-api`, `judge-portal`). Repos live at `/home/tuan/Project/my-oj/`.
- judge-api builds with **`./mvnw`**. Run scoped tests as: `./mvnw -q test -Dtest='<Class>' -Dsurefire.failIfNoSpecifiedTests=false`.
- **No Jakarta Bean Validation provider** on the classpath — `@Valid` is a no-op; services validate explicitly and throw `AppException(ErrorCode.X)`.
- `ddl-auto: validate` — every mapped column MUST exist in the migration, including BaseEntity's `created_at, updated_at, created_by, updated_by` (updated_at NOT NULL).
- List endpoints return `ApiResponse.success(PageResponse.of(page))` — the `ApiResponse<List<T>>` overload with `pagination`.
- Optional filters use **JPA Specification** (the `(:p IS NULL OR col=:p)` JPQL pattern breaks on Postgres).
- Frontend vitest env is `node`: stub browser globals with `vi.stubGlobal`; mock axios methods with `vi.hoisted`.
- Redis failures **fail open** (log + allow) in both limiter and ban mirror — availability over enforcement.
- Metric names use dots (`oj.login.rate_limited`) — Prometheus renders `oj_login_rate_limited_total`.
- Commits end with the Co-Authored-By + Claude-Session trailer used throughout this repo.

---

### Task 1: judge-api — branch, Flyway V11, ErrorCodes, Route

**Files:**
- Create: `judge-api/src/main/resources/db/migration/V11__login_attempts_and_access_bans.sql`
- Modify: `judge-api/src/main/java/vn/thanhtuanle/common/exception/ErrorCode.java` (before `UNCATEGORIZED_EXCEPTION`)
- Modify: `judge-api/src/main/java/vn/thanhtuanle/common/constant/Routes.java`

**Interfaces:**
- Produces: `Routes.SECURITY = "/api/v1/security"`; ErrorCodes `RATE_LIMITED(429)`, `ACCESS_BANNED(403)`, `BAN_EXISTED(409)`, `BAN_SELF(409)`, `BAN_INVALID(400)`, `BAN_NOT_FOUND(404)`; tables `t_login_attempts`, `t_access_bans`; permissions `ban:read/create/delete` seeded to ADMIN + SYS_ROOT.

- [ ] **Step 1: Branch**

```bash
cd /home/tuan/Project/my-oj/judge-api && git checkout -b feature/rate-limit-ban main
```

- [ ] **Step 2: Write V11 migration**

```sql
-- V11: login-attempt audit log + manual access bans + ban:* permissions (Spec C).
-- Additive + idempotent — safe on fresh DBs, baselined DBs, and re-runs.

CREATE TABLE IF NOT EXISTS public.t_login_attempts (
    id          UUID PRIMARY KEY,
    username    VARCHAR(64),
    ip          VARCHAR(45),
    device_hash VARCHAR(128),
    user_agent  VARCHAR(512),
    success     BOOLEAN NOT NULL,
    error_code  VARCHAR(64),
    created_at  TIMESTAMP NOT NULL,
    updated_at  TIMESTAMP NOT NULL,
    created_by  VARCHAR(255),
    updated_by  VARCHAR(255)
);
CREATE INDEX IF NOT EXISTS idx_login_attempts_ip_created ON public.t_login_attempts (ip, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_login_attempts_username_created ON public.t_login_attempts (username, created_at DESC);

CREATE TABLE IF NOT EXISTS public.t_access_bans (
    id         UUID PRIMARY KEY,
    type       VARCHAR(10) NOT NULL,
    value      VARCHAR(128) NOT NULL,
    reason     VARCHAR(255),
    expires_at TIMESTAMP,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    created_by VARCHAR(255),
    updated_by VARCHAR(255),
    CONSTRAINT uq_access_bans_type_value UNIQUE (type, value)
);

-- ban:* permissions. Per the V9 convention: grant to BOTH ADMIN and SYS_ROOT.
INSERT INTO public.t_permissions (id, created_at, created_by, updated_at, updated_by, description, name) VALUES
    ('a3000000-0000-0000-0000-000000000001', '2026-07-23 00:00:00', 'SYS', '2026-07-23 00:00:00', 'SYS', 'View login attempts and bans', 'ban:read'),
    ('a3000000-0000-0000-0000-000000000002', '2026-07-23 00:00:00', 'SYS', '2026-07-23 00:00:00', 'SYS', 'Ban an IP or device', 'ban:create'),
    ('a3000000-0000-0000-0000-000000000003', '2026-07-23 00:00:00', 'SYS', '2026-07-23 00:00:00', 'SYS', 'Remove a ban', 'ban:delete')
ON CONFLICT (id) DO NOTHING;

INSERT INTO public.t_roles_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM public.t_roles r, public.t_permissions p
WHERE r.name IN ('ADMIN', 'SYS_ROOT')
  AND p.id IN ('a3000000-0000-0000-0000-000000000001',
               'a3000000-0000-0000-0000-000000000002',
               'a3000000-0000-0000-0000-000000000003')
ON CONFLICT (role_id, permission_id) DO NOTHING;
```

- [ ] **Step 3: Add ErrorCodes** — insert before `UNCATEGORIZED_EXCEPTION` in the enum:

```java
    RATE_LIMITED("Too many failed login attempts. Try again later", HttpStatus.TOO_MANY_REQUESTS),
    ACCESS_BANNED("Access from this address or device is banned", HttpStatus.FORBIDDEN),
    BAN_EXISTED("This value is already banned", HttpStatus.CONFLICT),
    BAN_SELF("You cannot ban your own current IP or device", HttpStatus.CONFLICT),
    BAN_INVALID("Ban type must be IP or DEVICE and value must not be blank", HttpStatus.BAD_REQUEST),
    BAN_NOT_FOUND("Ban not found", HttpStatus.NOT_FOUND),
```

- [ ] **Step 4: Add route** — in `Routes.java` after `USERS`:

```java
    public static final String SECURITY = BASE_API + "/security";
```

- [ ] **Step 5: Compile + commit**

```bash
./mvnw -q compile && git add -A && git commit -m "feat(security): V11 migration, error codes, route for rate-limit + bans"
```
Expected: compile silent, commit created.

---

### Task 2: judge-api — entities + repositories

**Files:**
- Create: `judge-api/src/main/java/vn/thanhtuanle/common/enums/BanType.java`
- Create: `judge-api/src/main/java/vn/thanhtuanle/entity/LoginAttempt.java`
- Create: `judge-api/src/main/java/vn/thanhtuanle/entity/AccessBan.java`
- Create: `judge-api/src/main/java/vn/thanhtuanle/security/LoginAttemptRepository.java`
- Create: `judge-api/src/main/java/vn/thanhtuanle/security/AccessBanRepository.java`

**Interfaces:**
- Consumes: `BaseEntity` (UUID id + audit columns), Task 1 tables.
- Produces: `BanType { IP, DEVICE }`; `LoginAttempt(username, ip, deviceHash, userAgent, success, errorCode)`; `AccessBan(type, value, reason, expiresAt)`; `LoginAttemptRepository extends JpaRepository<LoginAttempt, UUID>, JpaSpecificationExecutor<LoginAttempt>` with `long deleteByCreatedAtBefore(LocalDateTime cutoff)`; `AccessBanRepository extends JpaRepository<AccessBan, UUID>` with `boolean existsByTypeAndValue(BanType type, String value)` and `long deleteByExpiresAtBefore(LocalDateTime cutoff)`.

- [ ] **Step 1: BanType enum**

```java
package vn.thanhtuanle.common.enums;

public enum BanType { IP, DEVICE }
```

- [ ] **Step 2: LoginAttempt entity**

```java
package vn.thanhtuanle.entity;

import jakarta.persistence.Column;
import jakarta.persistence.Entity;
import jakarta.persistence.Table;
import lombok.*;
import lombok.experimental.SuperBuilder;

@Entity
@Table(name = "t_login_attempts")
@Getter
@Setter
@NoArgsConstructor
@AllArgsConstructor
@SuperBuilder
@EqualsAndHashCode(callSuper = true)
public class LoginAttempt extends BaseEntity {

    @Column(length = 64)
    private String username;

    @Column(length = 45)
    private String ip;

    @Column(name = "device_hash", length = 128)
    private String deviceHash;

    @Column(name = "user_agent", length = 512)
    private String userAgent;

    @Column(nullable = false)
    private boolean success;

    @Column(name = "error_code", length = 64)
    private String errorCode;
}
```

- [ ] **Step 3: AccessBan entity**

```java
package vn.thanhtuanle.entity;

import jakarta.persistence.*;
import lombok.*;
import lombok.experimental.SuperBuilder;
import vn.thanhtuanle.common.enums.BanType;

import java.time.LocalDateTime;

@Entity
@Table(name = "t_access_bans")
@Getter
@Setter
@NoArgsConstructor
@AllArgsConstructor
@SuperBuilder
@EqualsAndHashCode(callSuper = true)
public class AccessBan extends BaseEntity {

    @Enumerated(EnumType.STRING)
    @Column(nullable = false, length = 10)
    private BanType type;

    @Column(nullable = false, length = 128)
    private String value;

    @Column(length = 255)
    private String reason;

    /** null = permanent. */
    @Column(name = "expires_at")
    private LocalDateTime expiresAt;
}
```

- [ ] **Step 4: Repositories**

```java
package vn.thanhtuanle.security;

import org.springframework.data.jpa.repository.JpaRepository;
import org.springframework.data.jpa.repository.JpaSpecificationExecutor;
import vn.thanhtuanle.entity.LoginAttempt;

import java.time.LocalDateTime;
import java.util.UUID;

public interface LoginAttemptRepository extends JpaRepository<LoginAttempt, UUID>, JpaSpecificationExecutor<LoginAttempt> {
    long deleteByCreatedAtBefore(LocalDateTime cutoff);
}
```

```java
package vn.thanhtuanle.security;

import org.springframework.data.jpa.repository.JpaRepository;
import vn.thanhtuanle.common.enums.BanType;
import vn.thanhtuanle.entity.AccessBan;

import java.time.LocalDateTime;
import java.util.UUID;

public interface AccessBanRepository extends JpaRepository<AccessBan, UUID> {
    boolean existsByTypeAndValue(BanType type, String value);
    long deleteByExpiresAtBefore(LocalDateTime cutoff);
}
```

- [ ] **Step 5: Compile + commit**

```bash
./mvnw -q compile && git add -A && git commit -m "feat(security): LoginAttempt + AccessBan entities and repositories"
```

---

### Task 3: judge-api — LoginRateLimiter (TDD)

**Files:**
- Create: `judge-api/src/main/java/vn/thanhtuanle/security/LoginRateLimiter.java`
- Test: `judge-api/src/test/java/vn/thanhtuanle/security/LoginRateLimiterTest.java`

**Interfaces:**
- Consumes: `StringRedisTemplate`, `MeterRegistry`, `ClientMeta(ip, deviceHash, userAgent)`, `AppException`, `ErrorCode.RATE_LIMITED`.
- Produces: `assertAllowed(String username, ClientMeta meta)` (throws `AppException(RATE_LIMITED)`), `recordFailure(String username, ClientMeta meta)`, `clear(String username)`, public constants `LOCK_SECONDS = 900`.

- [ ] **Step 1: Write the failing test**

```java
package vn.thanhtuanle.security;

import io.micrometer.core.instrument.simple.SimpleMeterRegistry;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.extension.ExtendWith;
import org.mockito.Mock;
import org.mockito.junit.jupiter.MockitoExtension;
import org.mockito.junit.jupiter.MockitoSettings;
import org.mockito.quality.Strictness;
import org.springframework.data.redis.core.StringRedisTemplate;
import org.springframework.data.redis.core.ValueOperations;
import vn.thanhtuanle.common.exception.AppException;
import vn.thanhtuanle.common.exception.ErrorCode;
import vn.thanhtuanle.common.util.ClientMeta;

import java.time.Duration;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatCode;
import static org.assertj.core.api.Assertions.assertThatThrownBy;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.anyString;
import static org.mockito.ArgumentMatchers.eq;
import static org.mockito.Mockito.lenient;
import static org.mockito.Mockito.never;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

@ExtendWith(MockitoExtension.class)
@MockitoSettings(strictness = Strictness.LENIENT)
class LoginRateLimiterTest {

    private static final ClientMeta META = new ClientMeta("1.2.3.4", "dev-1", "probe/1.0");

    @Mock StringRedisTemplate redis;
    @Mock ValueOperations<String, String> valueOps;

    LoginRateLimiter limiter;

    @BeforeEach
    void setUp() {
        when(redis.opsForValue()).thenReturn(valueOps);
        when(redis.hasKey(anyString())).thenReturn(false);
        limiter = new LoginRateLimiter(redis, new SimpleMeterRegistry());
    }

    @Test
    void allowed_whenNoLockExists() {
        assertThatCode(() -> limiter.assertAllowed("alice", META)).doesNotThrowAnyException();
    }

    @Test
    void blocked_whenUsernameLocked() {
        when(redis.hasKey("oj:rl:lock:username:alice")).thenReturn(true);
        assertThatThrownBy(() -> limiter.assertAllowed("alice", META))
                .isInstanceOf(AppException.class)
                .extracting(e -> ((AppException) e).getErrorCode())
                .isEqualTo(ErrorCode.RATE_LIMITED);
    }

    @Test
    void blocked_whenIpLocked() {
        when(redis.hasKey("oj:rl:lock:ip:1.2.3.4")).thenReturn(true);
        assertThatThrownBy(() -> limiter.assertAllowed("bob", META)).isInstanceOf(AppException.class);
    }

    @Test
    void failure_incrementsAllThreeCounters_andSetsWindowOnFirst() {
        when(valueOps.increment(anyString())).thenReturn(1L);
        limiter.recordFailure("alice", META);
        verify(valueOps).increment("oj:rl:login:username:alice");
        verify(valueOps).increment("oj:rl:login:ip:1.2.3.4");
        verify(valueOps).increment("oj:rl:login:device:dev-1");
        verify(redis).expire(eq("oj:rl:login:username:alice"), eq(Duration.ofMinutes(5)));
    }

    @Test
    void fifthUsernameFailure_locksUsername() {
        when(valueOps.increment(anyString())).thenReturn(5L);
        limiter.recordFailure("alice", META);
        verify(valueOps).set(eq("oj:rl:lock:username:alice"), eq("1"), eq(Duration.ofMinutes(15)));
    }

    @Test
    void fifthFailure_doesNotLockIpOrDevice_thresholdIsTen() {
        when(valueOps.increment(anyString())).thenReturn(5L);
        limiter.recordFailure("alice", META);
        verify(valueOps, never()).set(eq("oj:rl:lock:ip:1.2.3.4"), any(), any(Duration.class));
        verify(valueOps, never()).set(eq("oj:rl:lock:device:dev-1"), any(), any(Duration.class));
    }

    @Test
    void nullDeviceHash_isSkipped() {
        when(valueOps.increment(anyString())).thenReturn(1L);
        limiter.recordFailure("alice", new ClientMeta("1.2.3.4", null, null));
        verify(valueOps, never()).increment("oj:rl:login:device:null");
    }

    @Test
    void clear_deletesTheUsernameCounter() {
        limiter.clear("alice");
        verify(redis).delete("oj:rl:login:username:alice");
    }

    @Test
    void redisDown_failsOpen() {
        when(redis.hasKey(anyString())).thenThrow(new RuntimeException("redis down"));
        assertThatCode(() -> limiter.assertAllowed("alice", META)).doesNotThrowAnyException();
    }

    @Test
    void lockSecondsConstant_matchesLockTtl() {
        assertThat(LoginRateLimiter.LOCK_SECONDS).isEqualTo(900);
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
./mvnw -q test -Dtest='LoginRateLimiterTest' -Dsurefire.failIfNoSpecifiedTests=false
```
Expected: COMPILATION ERROR (LoginRateLimiter does not exist).

- [ ] **Step 3: Implement**

```java
package vn.thanhtuanle.security;

import io.micrometer.core.instrument.MeterRegistry;
import lombok.extern.slf4j.Slf4j;
import org.springframework.data.redis.core.StringRedisTemplate;
import org.springframework.stereotype.Component;
import vn.thanhtuanle.common.exception.AppException;
import vn.thanhtuanle.common.exception.ErrorCode;
import vn.thanhtuanle.common.util.ClientMeta;

import java.time.Duration;

/**
 * Redis-backed login brute-force lockout, modeled on the TokenBlocklist idiom.
 *
 * <p>Counts FAILED logins per username / ip / device_hash in a 5-minute window
 * (oj:rl:login:*). Crossing a threshold (5/username, 10/ip, 10/device) parks a lock key
 * (oj:rl:lock:*) with a 15-minute TTL; while it exists, logins for that key answer 429.
 * A successful login clears the username counter. Redis being down fails OPEN — login
 * availability beats enforcement here.
 */
@Component
@Slf4j
public class LoginRateLimiter {

    static final String COUNT_PREFIX = "oj:rl:login:";
    static final String LOCK_PREFIX = "oj:rl:lock:";
    static final Duration WINDOW = Duration.ofMinutes(5);
    static final Duration LOCK = Duration.ofMinutes(15);
    static final int USERNAME_LIMIT = 5;
    static final int IP_LIMIT = 10;
    static final int DEVICE_LIMIT = 10;
    /** Public for the Retry-After header in GlobalExceptionHandler. */
    public static final long LOCK_SECONDS = LOCK.toSeconds();

    private final StringRedisTemplate redis;
    private final MeterRegistry registry;

    public LoginRateLimiter(StringRedisTemplate redis, MeterRegistry registry) {
        this.redis = redis;
        this.registry = registry;
    }

    /** Throws 429 RATE_LIMITED when any dimension of this attempt is locked. */
    public void assertAllowed(String username, ClientMeta meta) {
        boolean locked;
        try {
            locked = isLocked("username", username) || isLocked("ip", meta.ip()) || isLocked("device", meta.deviceHash());
        } catch (Exception e) {
            log.warn("Rate limiter unavailable, allowing login: {}", e.getMessage());
            return;
        }
        if (locked) {
            registry.counter("oj.login.rate_limited").increment();
            throw new AppException(ErrorCode.RATE_LIMITED);
        }
    }

    /** Bump all three failure counters; lock any dimension that crosses its threshold. */
    public void recordFailure(String username, ClientMeta meta) {
        try {
            bump("username", username, USERNAME_LIMIT);
            bump("ip", meta.ip(), IP_LIMIT);
            bump("device", meta.deviceHash(), DEVICE_LIMIT);
        } catch (Exception e) {
            log.warn("Rate limiter unavailable, failure not counted: {}", e.getMessage());
        }
    }

    /** A successful login forgives that username's earlier typos. ip/device keep counting. */
    public void clear(String username) {
        try {
            redis.delete(COUNT_PREFIX + "username:" + username);
        } catch (Exception e) {
            log.warn("Rate limiter unavailable, counter not cleared: {}", e.getMessage());
        }
    }

    private boolean isLocked(String dimension, String value) {
        return value != null && !value.isBlank()
                && Boolean.TRUE.equals(redis.hasKey(LOCK_PREFIX + dimension + ":" + value));
    }

    private void bump(String dimension, String value, int limit) {
        if (value == null || value.isBlank()) {
            return;
        }
        String key = COUNT_PREFIX + dimension + ":" + value;
        Long count = redis.opsForValue().increment(key);
        if (count != null && count == 1L) {
            redis.expire(key, WINDOW);
        }
        if (count != null && count >= limit) {
            redis.opsForValue().set(LOCK_PREFIX + dimension + ":" + value, "1", LOCK);
        }
    }
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
./mvnw -q test -Dtest='LoginRateLimiterTest' -Dsurefire.failIfNoSpecifiedTests=false
```
Expected: `Tests run: 10, Failures: 0`.

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(security): Redis login rate limiter (5/username, 10/ip, 10/device per 5m, 15m lock)"
```

---

### Task 4: judge-api — LoginAttemptService (record + prune) (TDD)

**Files:**
- Create: `judge-api/src/main/java/vn/thanhtuanle/security/LoginAttemptService.java`
- Test: `judge-api/src/test/java/vn/thanhtuanle/security/LoginAttemptServiceTest.java`

**Interfaces:**
- Consumes: `LoginAttemptRepository`, `ClientMeta`.
- Produces: `record(String username, ClientMeta meta, boolean success, String errorCode)` (never throws), scheduled `prune()` deleting rows older than 30 days. `@EnableScheduling` is already on `BackendServiceApplication` — do NOT add it again.

- [ ] **Step 1: Write the failing test**

```java
package vn.thanhtuanle.security;

import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.extension.ExtendWith;
import org.mockito.ArgumentCaptor;
import org.mockito.InjectMocks;
import org.mockito.Mock;
import org.mockito.junit.jupiter.MockitoExtension;
import vn.thanhtuanle.common.util.ClientMeta;
import vn.thanhtuanle.entity.LoginAttempt;

import java.time.LocalDateTime;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatCode;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

@ExtendWith(MockitoExtension.class)
class LoginAttemptServiceTest {

    private static final ClientMeta META = new ClientMeta("1.2.3.4", "dev-1", "probe/1.0");

    @Mock LoginAttemptRepository repository;
    @InjectMocks LoginAttemptService service;

    @Test
    void record_persistsAllFields() {
        service.record("alice", META, false, "USER_NOT_EXISTED");

        ArgumentCaptor<LoginAttempt> captor = ArgumentCaptor.forClass(LoginAttempt.class);
        verify(repository).save(captor.capture());
        LoginAttempt saved = captor.getValue();
        assertThat(saved.getUsername()).isEqualTo("alice");
        assertThat(saved.getIp()).isEqualTo("1.2.3.4");
        assertThat(saved.getDeviceHash()).isEqualTo("dev-1");
        assertThat(saved.getUserAgent()).isEqualTo("probe/1.0");
        assertThat(saved.isSuccess()).isFalse();
        assertThat(saved.getErrorCode()).isEqualTo("USER_NOT_EXISTED");
    }

    @Test
    void record_neverThrows_evenWhenRepositoryFails() {
        when(repository.save(any())).thenThrow(new RuntimeException("db down"));
        assertThatCode(() -> service.record("alice", META, true, null)).doesNotThrowAnyException();
    }

    @Test
    void prune_deletesRowsOlderThan30Days() {
        when(repository.deleteByCreatedAtBefore(any())).thenReturn(7L);
        service.prune();
        ArgumentCaptor<LocalDateTime> captor = ArgumentCaptor.forClass(LocalDateTime.class);
        verify(repository).deleteByCreatedAtBefore(captor.capture());
        assertThat(captor.getValue()).isBefore(LocalDateTime.now().minusDays(29));
    }
}
```

- [ ] **Step 2: Run to verify FAIL** (compilation error), same `-Dtest` pattern as Task 3.

- [ ] **Step 3: Implement**

```java
package vn.thanhtuanle.security;

import lombok.RequiredArgsConstructor;
import lombok.extern.slf4j.Slf4j;
import org.springframework.scheduling.annotation.Scheduled;
import org.springframework.stereotype.Service;
import org.springframework.transaction.annotation.Transactional;
import vn.thanhtuanle.common.util.ClientMeta;
import vn.thanhtuanle.entity.LoginAttempt;

import java.time.LocalDateTime;

/** Audit log of password/Google login attempts. Refresh-token calls are NOT logged (volume). */
@Service
@RequiredArgsConstructor
@Slf4j
public class LoginAttemptService {

    static final int RETENTION_DAYS = 30;

    private final LoginAttemptRepository repository;

    /** Never throws — the audit trail must not break login itself. */
    public void record(String username, ClientMeta meta, boolean success, String errorCode) {
        try {
            repository.save(LoginAttempt.builder()
                    .username(username)
                    .ip(meta.ip())
                    .deviceHash(meta.deviceHash())
                    .userAgent(meta.userAgent())
                    .success(success)
                    .errorCode(errorCode)
                    .build());
        } catch (Exception e) {
            log.warn("Failed to record login attempt for {}: {}", username, e.getMessage());
        }
    }

    /** Nightly retention sweep (03:30 VN time — the app pins Asia/Ho_Chi_Minh). */
    @Scheduled(cron = "0 30 3 * * *")
    @Transactional
    public void prune() {
        long removed = repository.deleteByCreatedAtBefore(LocalDateTime.now().minusDays(RETENTION_DAYS));
        if (removed > 0) {
            log.info("Pruned {} login attempts older than {} days", removed, RETENTION_DAYS);
        }
    }
}
```

- [ ] **Step 4: Run to verify PASS** (3 tests).

- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(security): login-attempt audit log with 30-day retention"`

---

### Task 5: judge-api — wire limiter + audit into AuthService, Retry-After header (TDD)

**Files:**
- Modify: `judge-api/src/main/java/vn/thanhtuanle/auth/AuthService.java` (`authenticateWithPassword`, `authenticateGoogleUser`)
- Modify: `judge-api/src/main/java/vn/thanhtuanle/common/exception/GlobalExceptionHandler.java` (`handleAppException`)
- Test: modify `judge-api/src/test/java/vn/thanhtuanle/auth/AuthServicePasswordLoginTest.java` (add mocks + new cases)

**Interfaces:**
- Consumes: `LoginRateLimiter.assertAllowed/recordFailure/clear`, `LoginAttemptService.record`, `LoginRateLimiter.LOCK_SECONDS`.
- Produces: password login that is limiter-guarded and audited; Google login audited (NOT rate-limited — the OAuth code exchange is its own guard); refresh untouched; 429 responses carry `Retry-After: 900`.

- [ ] **Step 1: Add failing tests** — in `AuthServicePasswordLoginTest`, add `@Mock LoginRateLimiter loginRateLimiter;` and `@Mock LoginAttemptService loginAttemptService;` fields (Mockito injects them via `@InjectMocks`), then add:

```java
    @Test
    void login_checksLimiter_recordsSuccess_andClearsCounter() {
        // arrange a successful login exactly like the existing happy-path test, then:
        authService.authenticateWithPassword("admin", "Admin@12345", META);
        verify(loginRateLimiter).assertAllowed(eq("admin"), any(ClientMeta.class));
        verify(loginRateLimiter).clear("admin");
        verify(loginAttemptService).record(eq("admin"), any(ClientMeta.class), eq(true), isNull());
    }

    @Test
    void badPassword_recordsFailure_andAuditsIt() {
        // arrange the existing wrong-password case, then:
        assertThatThrownBy(() -> authService.authenticateWithPassword("admin", "wrong", META))
                .isInstanceOf(AppException.class);
        verify(loginRateLimiter).recordFailure(eq("admin"), any(ClientMeta.class));
        verify(loginAttemptService).record(eq("admin"), any(ClientMeta.class), eq(false), eq("USER_NOT_EXISTED"));
    }

    @Test
    void lockedKey_rejectsBeforeTouchingTheRepository() {
        doThrow(new AppException(ErrorCode.RATE_LIMITED))
                .when(loginRateLimiter).assertAllowed(eq("admin"), any(ClientMeta.class));
        assertThatThrownBy(() -> authService.authenticateWithPassword("admin", "Admin@12345", META))
                .extracting(e -> ((AppException) e).getErrorCode())
                .isEqualTo(ErrorCode.RATE_LIMITED);
        verifyNoInteractions(userRepository);
    }
```
(Reuse the test class's existing arrange helpers/mocks; `META` already exists there from Spec B.)

- [ ] **Step 2: Run to verify FAIL**

```bash
./mvnw -q test -Dtest='AuthServicePasswordLoginTest' -Dsurefire.failIfNoSpecifiedTests=false
```

- [ ] **Step 3: Implement in AuthService** — inject two new finals `private final LoginRateLimiter loginRateLimiter;` and `private final LoginAttemptService loginAttemptService;`. Rewrite `authenticateWithPassword`:

```java
    public AuthResponse authenticateWithPassword(String username, String password, ClientMeta meta) {
        loginRateLimiter.assertAllowed(username, meta);

        User user;
        try {
            user = userRepository.findByUsername(username)
                    .filter(u -> u.getPassword() != null && passwordEncoder.matches(password, u.getPassword()))
                    .orElseThrow(() -> new AppException(ErrorCode.USER_NOT_EXISTED));

            if (user.getStatus() == null || user.getStatus() != CommonStatus.ACTIVE.getValue()) {
                throw new AppException(ErrorCode.USER_BLOCKED);
            }
        } catch (AppException e) {
            loginRateLimiter.recordFailure(username, meta);
            loginAttemptService.record(username, meta, false, e.getErrorCode().name());
            throw e;
        }

        String accessToken = jwtUtil.generateToken(user);
        String refreshToken = jwtUtil.generateRefreshToken(user);

        revokeAllUserTokens(user);
        savedUserToken(user, accessToken, TokenType.ACCESS, meta);
        savedUserToken(user, refreshToken, TokenType.REFRESH, meta);

        user.setLastLogin(LocalDateTime.now());
        user.setLastLoginIp(meta.ip());
        user.setLastLoginDeviceHash(meta.deviceHash());
        userRepository.save(user);

        loginRateLimiter.clear(username);
        loginAttemptService.record(username, meta, true, null);

        log.info("Password login for user: {}", username);
        return AuthResponse.builder()
                .accessToken(accessToken)
                .refreshToken(refreshToken)
                .build();
    }
```

In `authenticateGoogleUser`, right where the USER_BLOCKED check happens (around line 170), wrap it the same way — record a failed attempt with the user's email as username and rethrow; after the success bookkeeping (`user.setLastLogin...save`) add `loginAttemptService.record(user.getEmail(), meta, true, null);`. No limiter calls in the Google flow.

- [ ] **Step 4: Retry-After header** — in `GlobalExceptionHandler.handleAppException` replace the return with:

```java
        ResponseEntity.BodyBuilder builder = ResponseEntity.status(errorCode.getStatusCode());
        if (errorCode == ErrorCode.RATE_LIMITED) {
            builder.header(org.springframework.http.HttpHeaders.RETRY_AFTER,
                    String.valueOf(vn.thanhtuanle.security.LoginRateLimiter.LOCK_SECONDS));
        }
        return builder.body(ApiResponse.error(errorCode.getStatusCode().value(), errorCode.getMessage()));
```

- [ ] **Step 5: Run to verify PASS**, then run the full auth package:

```bash
./mvnw -q test -Dtest='AuthService*,AuthController*' -Dsurefire.failIfNoSpecifiedTests=false
```
Expected: all pass (Google-callback tests unaffected — service is mocked there).

- [ ] **Step 6: Commit** — `git add -A && git commit -m "feat(security): guard password login with rate limiter + audit both login flows"`

---

### Task 6: judge-api — AccessBanMirror + AccessBanService (TDD)

**Files:**
- Create: `judge-api/src/main/java/vn/thanhtuanle/security/AccessBanMirror.java`
- Create: `judge-api/src/main/java/vn/thanhtuanle/security/AccessBanService.java`
- Create: `judge-api/src/main/java/vn/thanhtuanle/security/dto/CreateBanRequest.java`
- Create: `judge-api/src/main/java/vn/thanhtuanle/security/dto/BanResponse.java`
- Test: `judge-api/src/test/java/vn/thanhtuanle/security/AccessBanServiceTest.java`

**Interfaces:**
- Consumes: `AccessBanRepository`, `StringRedisTemplate`, `ClientMeta`, ErrorCodes from Task 1.
- Produces: `AccessBanMirror.isBanned(BanType, String)` (fail-open false), `.add(AccessBan)`, `.remove(BanType, String)`; `AccessBanService.create(CreateBanRequest, ClientMeta caller)`, `.list(int page, int size)`, `.delete(UUID id)`, `.rebuildMirror()` on `ApplicationReadyEvent`, scheduled `pruneExpired()`. `CreateBanRequest{String type; String value; String reason; Integer durationHours}` — **durationHours instead of the spec's raw expiresAt** (null = permanent; avoids ISO/`Z`-suffix vs LocalDateTime parsing traps; the UI offers 1h/24h/7d/permanent anyway).

- [ ] **Step 1: DTOs**

```java
package vn.thanhtuanle.security.dto;

import lombok.Data;

@Data
public class CreateBanRequest {
    private String type;          // "IP" | "DEVICE"
    private String value;
    private String reason;
    private Integer durationHours; // null = permanent
}
```

```java
package vn.thanhtuanle.security.dto;

import lombok.Builder;
import lombok.Data;
import vn.thanhtuanle.common.enums.BanType;

import java.time.LocalDateTime;
import java.util.UUID;

@Data
@Builder
public class BanResponse {
    private UUID id;
    private BanType type;
    private String value;
    private String reason;
    private LocalDateTime expiresAt;
    private String createdBy;
    private LocalDateTime createdAt;
}
```

- [ ] **Step 2: Write the failing test**

```java
package vn.thanhtuanle.security;

import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.extension.ExtendWith;
import org.mockito.ArgumentCaptor;
import org.mockito.InjectMocks;
import org.mockito.Mock;
import org.mockito.junit.jupiter.MockitoExtension;
import vn.thanhtuanle.common.enums.BanType;
import vn.thanhtuanle.common.exception.AppException;
import vn.thanhtuanle.common.exception.ErrorCode;
import vn.thanhtuanle.common.util.ClientMeta;
import vn.thanhtuanle.entity.AccessBan;
import vn.thanhtuanle.security.dto.CreateBanRequest;

import java.time.LocalDateTime;
import java.util.Optional;
import java.util.UUID;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatThrownBy;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

@ExtendWith(MockitoExtension.class)
class AccessBanServiceTest {

    private static final ClientMeta CALLER = new ClientMeta("9.9.9.9", "my-dev", "ua");

    @Mock AccessBanRepository repository;
    @Mock AccessBanMirror mirror;
    @InjectMocks AccessBanService service;

    private static CreateBanRequest req(String type, String value, Integer hours) {
        CreateBanRequest r = new CreateBanRequest();
        r.setType(type);
        r.setValue(value);
        r.setReason("spam");
        r.setDurationHours(hours);
        return r;
    }

    @Test
    void create_persists_andMirrors() {
        when(repository.existsByTypeAndValue(BanType.IP, "1.2.3.4")).thenReturn(false);
        when(repository.save(any())).thenAnswer(inv -> inv.getArgument(0));

        service.create(req("IP", "1.2.3.4", 24), CALLER);

        ArgumentCaptor<AccessBan> captor = ArgumentCaptor.forClass(AccessBan.class);
        verify(repository).save(captor.capture());
        assertThat(captor.getValue().getType()).isEqualTo(BanType.IP);
        assertThat(captor.getValue().getExpiresAt()).isAfter(LocalDateTime.now().plusHours(23));
        verify(mirror).add(captor.getValue());
    }

    @Test
    void create_permanent_hasNullExpiry() {
        when(repository.existsByTypeAndValue(BanType.DEVICE, "bad-dev")).thenReturn(false);
        when(repository.save(any())).thenAnswer(inv -> inv.getArgument(0));
        service.create(req("DEVICE", "bad-dev", null), CALLER);
        ArgumentCaptor<AccessBan> captor = ArgumentCaptor.forClass(AccessBan.class);
        verify(repository).save(captor.capture());
        assertThat(captor.getValue().getExpiresAt()).isNull();
    }

    @Test
    void duplicate_is409() {
        when(repository.existsByTypeAndValue(BanType.IP, "1.2.3.4")).thenReturn(true);
        assertThatThrownBy(() -> service.create(req("IP", "1.2.3.4", null), CALLER))
                .extracting(e -> ((AppException) e).getErrorCode()).isEqualTo(ErrorCode.BAN_EXISTED);
    }

    @Test
    void banningYourOwnIp_is409() {
        assertThatThrownBy(() -> service.create(req("IP", "9.9.9.9", null), CALLER))
                .extracting(e -> ((AppException) e).getErrorCode()).isEqualTo(ErrorCode.BAN_SELF);
    }

    @Test
    void banningYourOwnDevice_is409() {
        assertThatThrownBy(() -> service.create(req("DEVICE", "my-dev", null), CALLER))
                .extracting(e -> ((AppException) e).getErrorCode()).isEqualTo(ErrorCode.BAN_SELF);
    }

    @Test
    void badType_orBlankValue_is400() {
        assertThatThrownBy(() -> service.create(req("MAC", "x", null), CALLER))
                .extracting(e -> ((AppException) e).getErrorCode()).isEqualTo(ErrorCode.BAN_INVALID);
        assertThatThrownBy(() -> service.create(req("IP", "  ", null), CALLER))
                .extracting(e -> ((AppException) e).getErrorCode()).isEqualTo(ErrorCode.BAN_INVALID);
    }

    @Test
    void delete_removesRow_andMirrorEntry() {
        UUID id = UUID.randomUUID();
        AccessBan ban = AccessBan.builder().type(BanType.IP).value("1.2.3.4").build();
        when(repository.findById(id)).thenReturn(Optional.of(ban));
        service.delete(id);
        verify(repository).delete(ban);
        verify(mirror).remove(BanType.IP, "1.2.3.4");
    }

    @Test
    void delete_missing_is404() {
        UUID id = UUID.randomUUID();
        when(repository.findById(id)).thenReturn(Optional.empty());
        assertThatThrownBy(() -> service.delete(id))
                .extracting(e -> ((AppException) e).getErrorCode()).isEqualTo(ErrorCode.BAN_NOT_FOUND);
    }
}
```

- [ ] **Step 3: Run to verify FAIL**, then implement the mirror:

```java
package vn.thanhtuanle.security;

import lombok.RequiredArgsConstructor;
import lombok.extern.slf4j.Slf4j;
import org.springframework.data.redis.core.StringRedisTemplate;
import org.springframework.stereotype.Component;
import vn.thanhtuanle.common.enums.BanType;
import vn.thanhtuanle.entity.AccessBan;

import java.time.Duration;
import java.time.LocalDateTime;

/**
 * Redis mirror of t_access_bans for O(1) lookup on every request (multi-instance safe).
 * One key per ban (oj:ban:ip:<v> / oj:ban:device:<v>) — a temporary ban's key carries a
 * matching TTL so it self-evicts. Postgres stays the source of truth; the mirror is
 * rebuilt at boot and updated synchronously on CRUD. Redis being down fails OPEN.
 */
@Component
@RequiredArgsConstructor
@Slf4j
public class AccessBanMirror {

    static final String KEY_PREFIX = "oj:ban:";

    private final StringRedisTemplate redis;

    public void add(AccessBan ban) {
        Duration ttl = ban.getExpiresAt() == null ? null
                : Duration.between(LocalDateTime.now(), ban.getExpiresAt());
        if (ttl != null && (ttl.isZero() || ttl.isNegative())) {
            return; // already expired — nothing to enforce
        }
        String key = key(ban.getType(), ban.getValue());
        try {
            if (ttl == null) {
                redis.opsForValue().set(key, "1");
            } else {
                redis.opsForValue().set(key, "1", ttl);
            }
        } catch (Exception e) {
            log.warn("Ban mirror unavailable, {} not mirrored: {}", key, e.getMessage());
        }
    }

    public void remove(BanType type, String value) {
        try {
            redis.delete(key(type, value));
        } catch (Exception e) {
            log.warn("Ban mirror unavailable, {} not removed: {}", key(type, value), e.getMessage());
        }
    }

    /** Fail-open: if Redis is down nobody is considered banned. */
    public boolean isBanned(BanType type, String value) {
        if (value == null || value.isBlank()) {
            return false;
        }
        try {
            return Boolean.TRUE.equals(redis.hasKey(key(type, value)));
        } catch (Exception e) {
            log.warn("Ban mirror unavailable, allowing request: {}", e.getMessage());
            return false;
        }
    }

    static String key(BanType type, String value) {
        return KEY_PREFIX + type.name().toLowerCase() + ":" + value;
    }
}
```

Then the service:

```java
package vn.thanhtuanle.security;

import lombok.RequiredArgsConstructor;
import lombok.extern.slf4j.Slf4j;
import org.springframework.context.event.EventListener;
import org.springframework.boot.context.event.ApplicationReadyEvent;
import org.springframework.data.domain.Page;
import org.springframework.data.domain.PageRequest;
import org.springframework.data.domain.Sort;
import org.springframework.scheduling.annotation.Scheduled;
import org.springframework.stereotype.Service;
import org.springframework.transaction.annotation.Transactional;
import vn.thanhtuanle.common.enums.BanType;
import vn.thanhtuanle.common.exception.AppException;
import vn.thanhtuanle.common.exception.ErrorCode;
import vn.thanhtuanle.common.payload.PageResponse;
import vn.thanhtuanle.common.util.ClientMeta;
import vn.thanhtuanle.entity.AccessBan;
import vn.thanhtuanle.security.dto.BanResponse;
import vn.thanhtuanle.security.dto.CreateBanRequest;

import java.time.LocalDateTime;
import java.util.UUID;

@Service
@RequiredArgsConstructor
@Slf4j
public class AccessBanService {

    private final AccessBanRepository repository;
    private final AccessBanMirror mirror;

    public BanResponse create(CreateBanRequest request, ClientMeta caller) {
        BanType type = parseType(request.getType());
        String value = request.getValue() == null ? "" : request.getValue().trim();
        if (value.isBlank() || value.length() > 128) {
            throw new AppException(ErrorCode.BAN_INVALID);
        }
        // Guardrail: don't let an admin saw off the branch they're sitting on.
        if ((type == BanType.IP && value.equals(caller.ip()))
                || (type == BanType.DEVICE && value.equals(caller.deviceHash()))) {
            throw new AppException(ErrorCode.BAN_SELF);
        }
        if (repository.existsByTypeAndValue(type, value)) {
            throw new AppException(ErrorCode.BAN_EXISTED);
        }
        LocalDateTime expiresAt = request.getDurationHours() == null ? null
                : LocalDateTime.now().plusHours(request.getDurationHours());
        AccessBan ban = repository.save(AccessBan.builder()
                .type(type).value(value).reason(request.getReason()).expiresAt(expiresAt)
                .build());
        mirror.add(ban);
        log.info("Banned {} {} until {}", type, value, expiresAt == null ? "forever" : expiresAt);
        return toResponse(ban);
    }

    public PageResponse<BanResponse> list(int page, int size) {
        Page<BanResponse> dtoPage = repository
                .findAll(PageRequest.of(page, size, Sort.by("createdAt").descending()))
                .map(AccessBanService::toResponse);
        return PageResponse.of(dtoPage);
    }

    public void delete(UUID id) {
        AccessBan ban = repository.findById(id)
                .orElseThrow(() -> new AppException(ErrorCode.BAN_NOT_FOUND));
        repository.delete(ban);
        mirror.remove(ban.getType(), ban.getValue());
        log.info("Unbanned {} {}", ban.getType(), ban.getValue());
    }

    /** Boot-time rebuild so the mirror survives Redis restarts/flushes. */
    @EventListener(ApplicationReadyEvent.class)
    public void rebuildMirror() {
        try {
            repository.findAll().forEach(mirror::add); // add() skips already-expired bans
            log.info("Access-ban mirror rebuilt ({} bans in DB)", repository.count());
        } catch (Exception e) {
            log.warn("Could not rebuild ban mirror: {}", e.getMessage());
        }
    }

    /** Nightly sweep of expired ban rows (mirror keys already self-evicted via TTL). */
    @Scheduled(cron = "0 40 3 * * *")
    @Transactional
    public void pruneExpired() {
        long removed = repository.deleteByExpiresAtBefore(LocalDateTime.now());
        if (removed > 0) {
            log.info("Pruned {} expired bans", removed);
        }
    }

    private static BanType parseType(String raw) {
        try {
            return BanType.valueOf(raw == null ? "" : raw.trim().toUpperCase());
        } catch (IllegalArgumentException e) {
            throw new AppException(ErrorCode.BAN_INVALID);
        }
    }

    static BanResponse toResponse(AccessBan ban) {
        return BanResponse.builder()
                .id(ban.getId()).type(ban.getType()).value(ban.getValue())
                .reason(ban.getReason()).expiresAt(ban.getExpiresAt())
                .createdBy(ban.getCreatedBy()).createdAt(ban.getCreatedAt())
                .build();
    }
}
```

- [ ] **Step 4: Run to verify PASS** (`AccessBanServiceTest`, 8 tests).

- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(security): access-ban service with Redis mirror, self-ban guard, boot rebuild"`

---

### Task 7: judge-api — AccessBanFilter registered before the security chain (TDD)

**Files:**
- Create: `judge-api/src/main/java/vn/thanhtuanle/security/AccessBanFilter.java`
- Modify: `judge-api/src/main/java/vn/thanhtuanle/config/SecurityConfig.java` (add the registration bean)
- Test: `judge-api/src/test/java/vn/thanhtuanle/security/AccessBanFilterTest.java`

**Interfaces:**
- Consumes: `AccessBanMirror.isBanned`, `ClientMeta.from(HttpServletRequest)`, `ApiResponse.error`, `MeterRegistry`.
- Produces: a filter that answers `403` JSON for banned ip/device on ANY path (including permitAll ones) and passes everything else through; registered at order `-200` — before `springSecurityFilterChain` (order -100). NOTE: Boot skips auto-registration of a Filter bean when a `FilterRegistrationBean` for it exists, so it runs exactly once.

- [ ] **Step 1: Write the failing test**

```java
package vn.thanhtuanle.security;

import com.fasterxml.jackson.databind.ObjectMapper;
import io.micrometer.core.instrument.simple.SimpleMeterRegistry;
import jakarta.servlet.FilterChain;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.extension.ExtendWith;
import org.mockito.Mock;
import org.mockito.junit.jupiter.MockitoExtension;
import org.springframework.mock.web.MockHttpServletRequest;
import org.springframework.mock.web.MockHttpServletResponse;
import vn.thanhtuanle.common.enums.BanType;

import static org.assertj.core.api.Assertions.assertThat;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.eq;
import static org.mockito.Mockito.never;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

@ExtendWith(MockitoExtension.class)
class AccessBanFilterTest {

    @Mock AccessBanMirror mirror;
    @Mock FilterChain chain;

    AccessBanFilter filter;
    MockHttpServletRequest request;
    MockHttpServletResponse response;

    @BeforeEach
    void setUp() {
        filter = new AccessBanFilter(mirror, new ObjectMapper(), new SimpleMeterRegistry());
        request = new MockHttpServletRequest("GET", "/api/v1/problems");
        request.setRemoteAddr("1.2.3.4");
        request.addHeader("X-Device-Id", "dev-1");
        request.addHeader("User-Agent", "probe/1.0");
        response = new MockHttpServletResponse();
    }

    @Test
    void cleanRequest_passesThrough() throws Exception {
        when(mirror.isBanned(any(), any())).thenReturn(false);
        filter.doFilter(request, response, chain);
        verify(chain).doFilter(request, response);
    }

    @Test
    void bannedIp_gets403Json_andChainStops() throws Exception {
        when(mirror.isBanned(eq(BanType.IP), eq("1.2.3.4"))).thenReturn(true);
        filter.doFilter(request, response, chain);
        verify(chain, never()).doFilter(any(), any());
        assertThat(response.getStatus()).isEqualTo(403);
        assertThat(response.getContentType()).contains("application/json");
        assertThat(response.getContentAsString()).contains("banned");
    }

    @Test
    void bannedDevice_gets403() throws Exception {
        when(mirror.isBanned(eq(BanType.IP), any())).thenReturn(false);
        when(mirror.isBanned(eq(BanType.DEVICE), eq("dev-1"))).thenReturn(true);
        filter.doFilter(request, response, chain);
        assertThat(response.getStatus()).isEqualTo(403);
    }
}
```

- [ ] **Step 2: Run to verify FAIL**, then implement:

```java
package vn.thanhtuanle.security;

import com.fasterxml.jackson.databind.ObjectMapper;
import io.micrometer.core.instrument.MeterRegistry;
import jakarta.servlet.FilterChain;
import jakarta.servlet.ServletException;
import jakarta.servlet.http.HttpServletRequest;
import jakarta.servlet.http.HttpServletResponse;
import lombok.RequiredArgsConstructor;
import org.springframework.http.HttpStatus;
import org.springframework.http.MediaType;
import org.springframework.stereotype.Component;
import org.springframework.web.filter.OncePerRequestFilter;
import vn.thanhtuanle.common.enums.BanType;
import vn.thanhtuanle.common.exception.ErrorCode;
import vn.thanhtuanle.common.payload.ApiResponse;
import vn.thanhtuanle.common.util.ClientMeta;

import java.io.IOException;

/**
 * Hard access ban by ip or device_hash, enforced BEFORE the Spring Security chain (see the
 * FilterRegistrationBean in SecurityConfig) so bans also cover permitAll endpoints.
 * Runs outside @ControllerAdvice, so it writes its own 403 JSON body.
 */
@Component
@RequiredArgsConstructor
public class AccessBanFilter extends OncePerRequestFilter {

    private final AccessBanMirror mirror;
    private final ObjectMapper objectMapper;
    private final MeterRegistry registry;

    @Override
    protected void doFilterInternal(HttpServletRequest request, HttpServletResponse response,
                                    FilterChain filterChain) throws ServletException, IOException {
        ClientMeta meta = ClientMeta.from(request);
        if (mirror.isBanned(BanType.IP, meta.ip()) || mirror.isBanned(BanType.DEVICE, meta.deviceHash())) {
            registry.counter("oj.request.banned").increment();
            response.setStatus(HttpStatus.FORBIDDEN.value());
            response.setContentType(MediaType.APPLICATION_JSON_VALUE);
            objectMapper.writeValue(response.getWriter(),
                    ApiResponse.error(HttpStatus.FORBIDDEN.value(), ErrorCode.ACCESS_BANNED.getMessage()));
            return;
        }
        filterChain.doFilter(request, response);
    }
}
```

- [ ] **Step 3: Register before the security chain** — add to `SecurityConfig`:

```java
    // Runs at order -200 — before springSecurityFilterChain (order -100) — so a ban covers
    // every endpoint, including permitAll ones. Having this registration bean also stops
    // Boot from auto-registering the @Component filter a second time.
    @Bean
    public org.springframework.boot.web.servlet.FilterRegistrationBean<vn.thanhtuanle.security.AccessBanFilter>
            accessBanFilterRegistration(vn.thanhtuanle.security.AccessBanFilter filter) {
        var registration = new org.springframework.boot.web.servlet.FilterRegistrationBean<>(filter);
        registration.setOrder(-200);
        registration.addUrlPatterns("/*");
        return registration;
    }
```

- [ ] **Step 4: Run to verify PASS** (`AccessBanFilterTest`, 3 tests).

- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(security): access-ban filter enforced before the security chain"`

---

### Task 8: judge-api — attempts query + SecurityController + full suite

**Files:**
- Create: `judge-api/src/main/java/vn/thanhtuanle/security/LoginAttemptSpecifications.java`
- Create: `judge-api/src/main/java/vn/thanhtuanle/security/dto/AttemptResponse.java`
- Create: `judge-api/src/main/java/vn/thanhtuanle/security/SecurityController.java`
- Modify: `judge-api/src/main/java/vn/thanhtuanle/security/LoginAttemptService.java` (add `list`)
- Test: `judge-api/src/test/java/vn/thanhtuanle/security/LoginAttemptListTest.java`

**Interfaces:**
- Consumes: everything above; `ApiResponse.success(PageResponse<T>)`; `@PreAuthorize("hasAuthority('ban:...')")`; `@DateTimeFormat(iso = DateTimeFormat.ISO.DATE) LocalDate`.
- Produces: `GET /api/v1/security/attempts?page&size&ip&username&success&createdFrom&createdTo`, `GET /api/v1/security/bans?page&size`, `POST /api/v1/security/bans`, `DELETE /api/v1/security/bans/{id}`.

- [ ] **Step 1: Specifications** (mirror of `UserSpecifications`):

```java
package vn.thanhtuanle.security;

import jakarta.persistence.criteria.Predicate;
import org.springframework.data.jpa.domain.Specification;
import vn.thanhtuanle.entity.LoginAttempt;

import java.time.LocalDate;
import java.time.LocalDateTime;
import java.util.ArrayList;
import java.util.List;

public final class LoginAttemptSpecifications {

    private LoginAttemptSpecifications() {
    }

    public static Specification<LoginAttempt> filter(String ip, String username, Boolean success,
                                                     LocalDate createdFrom, LocalDate createdTo) {
        return (root, query, cb) -> {
            List<Predicate> predicates = new ArrayList<>();
            if (ip != null && !ip.isBlank()) {
                predicates.add(cb.equal(root.get("ip"), ip.trim()));
            }
            if (username != null && !username.isBlank()) {
                predicates.add(cb.like(cb.lower(root.get("username")),
                        "%" + username.trim().toLowerCase() + "%"));
            }
            if (success != null) {
                predicates.add(cb.equal(root.get("success"), success));
            }
            if (createdFrom != null) {
                predicates.add(cb.greaterThanOrEqualTo(root.<LocalDateTime>get("createdAt"),
                        createdFrom.atStartOfDay()));
            }
            if (createdTo != null) {
                predicates.add(cb.lessThan(root.<LocalDateTime>get("createdAt"),
                        createdTo.plusDays(1).atStartOfDay()));
            }
            return cb.and(predicates.toArray(new Predicate[0]));
        };
    }
}
```

- [ ] **Step 2: AttemptResponse + list method** — DTO:

```java
package vn.thanhtuanle.security.dto;

import lombok.Builder;
import lombok.Data;

import java.time.LocalDateTime;
import java.util.UUID;

@Data
@Builder
public class AttemptResponse {
    private UUID id;
    private String username;
    private String ip;
    private String deviceHash;
    private String userAgent;
    private boolean success;
    private String errorCode;
    private LocalDateTime createdAt;
}
```

Add to `LoginAttemptService` (plus imports for `Page/PageRequest/Sort/Specification/PageResponse/AttemptResponse/LocalDate`):

```java
    public PageResponse<AttemptResponse> list(int page, int size, String ip, String username,
                                              Boolean success, LocalDate createdFrom, LocalDate createdTo) {
        Page<AttemptResponse> dtoPage = repository
                .findAll(LoginAttemptSpecifications.filter(ip, username, success, createdFrom, createdTo),
                        PageRequest.of(page, size, Sort.by("createdAt").descending()))
                .map(a -> AttemptResponse.builder()
                        .id(a.getId()).username(a.getUsername()).ip(a.getIp())
                        .deviceHash(a.getDeviceHash()).userAgent(a.getUserAgent())
                        .success(a.isSuccess()).errorCode(a.getErrorCode())
                        .createdAt(a.getCreatedAt())
                        .build());
        return PageResponse.of(dtoPage);
    }
```

- [ ] **Step 3: Failing test for the list**

```java
package vn.thanhtuanle.security;

import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.extension.ExtendWith;
import org.mockito.InjectMocks;
import org.mockito.Mock;
import org.mockito.junit.jupiter.MockitoExtension;
import org.springframework.data.domain.Page;
import org.springframework.data.domain.PageImpl;
import org.springframework.data.domain.PageRequest;
import org.springframework.data.domain.Pageable;
import org.springframework.data.jpa.domain.Specification;
import vn.thanhtuanle.common.payload.PageResponse;
import vn.thanhtuanle.entity.LoginAttempt;
import vn.thanhtuanle.security.dto.AttemptResponse;

import java.util.List;

import static org.assertj.core.api.Assertions.assertThat;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.Mockito.when;

@ExtendWith(MockitoExtension.class)
class LoginAttemptListTest {

    @Mock LoginAttemptRepository repository;
    @InjectMocks LoginAttemptService service;

    @Test
    void list_mapsEntitiesToDtos_withPagination() {
        LoginAttempt attempt = LoginAttempt.builder()
                .username("alice").ip("1.2.3.4").deviceHash("dev-1").success(false).errorCode("USER_NOT_EXISTED")
                .build();
        Page<LoginAttempt> page = new PageImpl<>(List.of(attempt), PageRequest.of(0, 20), 1);
        when(repository.findAll(any(Specification.class), any(Pageable.class))).thenReturn(page);

        PageResponse<AttemptResponse> result = service.list(0, 20, null, null, null, null, null);

        assertThat(result.getData()).hasSize(1);
        assertThat(result.getData().get(0).getUsername()).isEqualTo("alice");
        assertThat(result.getData().get(0).isSuccess()).isFalse();
    }
}
```

Run FAIL → implement Step 2 → run PASS.

- [ ] **Step 4: Controller**

```java
package vn.thanhtuanle.security;

import jakarta.servlet.http.HttpServletRequest;
import lombok.RequiredArgsConstructor;
import org.springframework.format.annotation.DateTimeFormat;
import org.springframework.security.access.prepost.PreAuthorize;
import org.springframework.web.bind.annotation.*;
import vn.thanhtuanle.common.constant.Routes;
import vn.thanhtuanle.common.payload.ApiResponse;
import vn.thanhtuanle.common.util.ClientMeta;
import vn.thanhtuanle.security.dto.AttemptResponse;
import vn.thanhtuanle.security.dto.BanResponse;
import vn.thanhtuanle.security.dto.CreateBanRequest;

import java.time.LocalDate;
import java.util.List;
import java.util.UUID;

@RestController
@RequestMapping(Routes.SECURITY)
@RequiredArgsConstructor
public class SecurityController {

    private final LoginAttemptService loginAttemptService;
    private final AccessBanService accessBanService;

    @GetMapping("/attempts")
    @PreAuthorize("hasAuthority('ban:read')")
    public ApiResponse<List<AttemptResponse>> listAttempts(
            @RequestParam(defaultValue = "0") int page,
            @RequestParam(defaultValue = "20") int size,
            @RequestParam(required = false) String ip,
            @RequestParam(required = false) String username,
            @RequestParam(required = false) Boolean success,
            @RequestParam(required = false) @DateTimeFormat(iso = DateTimeFormat.ISO.DATE) LocalDate createdFrom,
            @RequestParam(required = false) @DateTimeFormat(iso = DateTimeFormat.ISO.DATE) LocalDate createdTo) {
        return ApiResponse.success(loginAttemptService.list(page, size, ip, username, success, createdFrom, createdTo));
    }

    @GetMapping("/bans")
    @PreAuthorize("hasAuthority('ban:read')")
    public ApiResponse<List<BanResponse>> listBans(
            @RequestParam(defaultValue = "0") int page,
            @RequestParam(defaultValue = "20") int size) {
        return ApiResponse.success(accessBanService.list(page, size));
    }

    @PostMapping("/bans")
    @PreAuthorize("hasAuthority('ban:create')")
    public ApiResponse<BanResponse> createBan(@RequestBody CreateBanRequest request,
                                              HttpServletRequest httpRequest) {
        return ApiResponse.success(accessBanService.create(request, ClientMeta.from(httpRequest)));
    }

    @DeleteMapping("/bans/{id}")
    @PreAuthorize("hasAuthority('ban:delete')")
    public ApiResponse<Void> deleteBan(@PathVariable UUID id) {
        accessBanService.delete(id);
        return ApiResponse.success();
    }
}
```

- [ ] **Step 5: Full backend suite + commit**

```bash
./mvnw -q test 2>&1 | tail -5
```
Expected: 0 failures (127 existing + ~25 new).

```bash
git add -A && git commit -m "feat(security): /security endpoints — attempts list + ban CRUD"
```

---

### Task 9: judge-portal — branch, apiPath, types, securityService (TDD)

**Files:**
- Modify: `judge-portal/src/constants/apiPath.ts`
- Create: `judge-portal/src/types/security.ts`
- Create: `judge-portal/src/services/securityService.ts`
- Test: `judge-portal/src/services/__tests__/securityService.spec.ts`

**Interfaces:**
- Consumes: `axiosClient`, `ApiResponse<T>` from `@/types/common`.
- Produces: `securityService.listAttempts(signal?, params?)`, `.listBans(signal?, params?)`, `.createBan(payload)`, `.deleteBan(id)`; types `LoginAttempt`, `AccessBan`, `AttemptListParams`, `CreateBanPayload`.

- [ ] **Step 1: Branch**

```bash
cd /home/tuan/Project/my-oj/judge-portal && git checkout -b feature/rate-limit-ban main
```

- [ ] **Step 2: apiPath** — add to `API_ROUTES` after `ROLES`:

```ts
    SECURITY: {
        ATTEMPTS: '/security/attempts',
        BANS: '/security/bans',
        BAN: (id: string) => `/security/bans/${id}`,
    },
```

- [ ] **Step 3: types/security.ts**

```ts
export interface LoginAttempt {
    id: string
    username: string | null
    ip: string | null
    deviceHash: string | null
    userAgent: string | null
    success: boolean
    errorCode: string | null
    createdAt: string
}

export type BanType = 'IP' | 'DEVICE'

export interface AccessBan {
    id: string
    type: BanType
    value: string
    reason: string | null
    expiresAt: string | null
    createdBy: string | null
    createdAt: string
}

export interface AttemptListParams {
    page?: number
    size?: number
    ip?: string
    username?: string
    success?: boolean
    createdFrom?: string
    createdTo?: string
}

export interface CreateBanPayload {
    type: BanType
    value: string
    reason?: string
    /** hours; omit/null = permanent */
    durationHours?: number | null
}
```

- [ ] **Step 4: Failing service spec** (vi.hoisted idiom, same as `userService.spec.ts`):

```ts
import { describe, it, expect, vi, beforeEach } from 'vitest'
import securityService from '@/services/securityService'

const { get, post, del } = vi.hoisted(() => ({
    get: vi.fn(),
    post: vi.fn(),
    del: vi.fn(),
}))

vi.mock('@/api/axiosClient', () => ({
    default: { get, post, delete: del },
}))

beforeEach(() => {
    get.mockReset().mockResolvedValue({ data: {} })
    post.mockReset().mockResolvedValue({ data: {} })
    del.mockReset().mockResolvedValue({ data: {} })
})

describe('securityService', () => {
    it('listAttempts passes params + signal', async () => {
        const ctrl = new AbortController()
        await securityService.listAttempts(ctrl.signal, { page: 1, ip: '1.2.3.4', success: false })
        expect(get).toHaveBeenCalledWith('/security/attempts', {
            params: { page: 1, ip: '1.2.3.4', success: false },
            signal: ctrl.signal,
        })
    })

    it('listBans hits /security/bans', async () => {
        await securityService.listBans(undefined, { page: 0, size: 20 })
        expect(get).toHaveBeenCalledWith('/security/bans', { params: { page: 0, size: 20 }, signal: undefined })
    })

    it('createBan posts the payload', async () => {
        await securityService.createBan({ type: 'IP', value: '1.2.3.4', reason: 'spam', durationHours: 24 })
        expect(post).toHaveBeenCalledWith('/security/bans', {
            type: 'IP', value: '1.2.3.4', reason: 'spam', durationHours: 24,
        })
    })

    it('deleteBan targets the id', async () => {
        await securityService.deleteBan('b-1')
        expect(del).toHaveBeenCalledWith('/security/bans/b-1')
    })
})
```

Run: `npm run test -- securityService` → FAIL (module missing).

- [ ] **Step 5: Implement securityService.ts**

```ts
import axiosClient from '@/api/axiosClient'
import type { AxiosResponse } from 'axios'
import type { ApiResponse } from '@/types/common'
import type { LoginAttempt, AccessBan, AttemptListParams, CreateBanPayload } from '@/types/security'
import { API_ROUTES } from '@/constants/apiPath'

class SecurityService {
    listAttempts(signal?: AbortSignal, params?: AttemptListParams): Promise<AxiosResponse<ApiResponse<LoginAttempt[]>>> {
        return axiosClient.get<ApiResponse<LoginAttempt[]>>(API_ROUTES.SECURITY.ATTEMPTS, { params, signal })
    }

    listBans(signal?: AbortSignal, params?: { page?: number; size?: number }): Promise<AxiosResponse<ApiResponse<AccessBan[]>>> {
        return axiosClient.get<ApiResponse<AccessBan[]>>(API_ROUTES.SECURITY.BANS, { params, signal })
    }

    createBan(payload: CreateBanPayload): Promise<AxiosResponse<ApiResponse<AccessBan>>> {
        return axiosClient.post<ApiResponse<AccessBan>>(API_ROUTES.SECURITY.BANS, payload)
    }

    deleteBan(id: string): Promise<AxiosResponse<ApiResponse<void>>> {
        return axiosClient.delete<ApiResponse<void>>(API_ROUTES.SECURITY.BAN(id))
    }
}

export default new SecurityService()
```

- [ ] **Step 6: Run PASS + commit**

```bash
npm run test -- securityService && git add -A && git commit -m "feat(security): security service + types for attempts and bans"
```

---

### Task 10: judge-portal — AdminSecurityPage + route + nav

**Files:**
- Create: `judge-portal/src/pages/admin/AdminSecurityPage.vue`
- Modify: `judge-portal/src/router/index.ts` (import + child route)
- Modify: `judge-portal/src/constants/routePath.ts` (add `ADMIN_SECURITY`)
- Modify: `judge-portal/src/pages/admin/AdminLayout.vue` (nav item + breadcrumb)

**Interfaces:**
- Consumes: `securityService`, `useFetch(serviceFn, { immediate })` where serviceFn is `(signal, ...args)`, `useToast().triggerToast(msg, 'success'|'error')`, `getErrorMessage(err, fallback)`, `authStore.hasPermission`, shadcn-vue components already used by `AdminUsersPage.vue` (`Button`, `Input`, `Select…`, `Dialog…`, `Badge`, `DropdownMenu…`).
- Produces: `/admin/security` — tab **Attempts** (filters: ip, username, result; per-row Ban IP / Ban device) and tab **Bans** (list, create, unban).

- [ ] **Step 1: routePath + router + nav**

`routePath.ts`: add `ADMIN_SECURITY: '/admin/security',` next to the other ADMIN_* constants.

`router/index.ts`: `import AdminSecurityPage from '@/pages/admin/AdminSecurityPage.vue'` and add child route after `permissions`:

```ts
        { path: 'security', name: 'AdminSecurity', component: AdminSecurityPage, meta: { permissions: ['ban:read'] } },
```

`AdminLayout.vue`: import `ShieldAlert` from `lucide-vue-next`; in the `Access` nav group append:

```ts
                { label: 'Security', to: ROUTE_PATH.ADMIN_SECURITY, icon: ShieldAlert, show: can('ban:read') },
```

and register the breadcrumb entry alongside the existing ones: `AdminSecurity: [adminCrumb, { label: 'Security' }],` (match the file's existing crumb-map shape).

- [ ] **Step 2: AdminSecurityPage.vue** — follow `AdminUsersPage.vue` conventions exactly (script setup + useFetch + debounced filters). Complete component:

```vue
<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useFetch } from '@/composables/useFetch'
import { useToast } from '@/composables/useToast'
import { getErrorMessage } from '@/lib/error'
import securityService from '@/services/securityService'
import type { AccessBan, BanType, CreateBanPayload, LoginAttempt } from '@/types/security'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Badge } from '@/components/ui/badge'
import {
    Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from '@/components/ui/dialog'
import {
    Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from '@/components/ui/select'

const { triggerToast } = useToast()

const tab = ref<'attempts' | 'bans'>('attempts')

// ---------- Attempts tab ----------
const page = ref(0)
const size = 20
const fIp = ref('')
const fUsername = ref('')
const fResult = ref<'all' | 'ok' | 'fail'>('all')
const fFrom = ref('') // yyyy-MM-dd (native date input)
const fTo = ref('')

const attemptParams = computed(() => ({
    page: page.value,
    size,
    ip: fIp.value.trim() || undefined,
    username: fUsername.value.trim() || undefined,
    success: fResult.value === 'all' ? undefined : fResult.value === 'ok',
    createdFrom: fFrom.value || undefined,
    createdTo: fTo.value || undefined,
}))

const {
    data: attemptsRes,
    isLoading: attemptsLoading,
    execute: loadAttempts,
} = useFetch((signal: AbortSignal) => securityService.listAttempts(signal, attemptParams.value), { immediate: true })

const attempts = computed<LoginAttempt[]>(() => attemptsRes.value?.data?.data ?? [])
const pagination = computed(() => attemptsRes.value?.data?.pagination)

let debounce: ReturnType<typeof setTimeout> | undefined
watch([fIp, fUsername, fResult, fFrom, fTo], () => {
    clearTimeout(debounce)
    debounce = setTimeout(() => {
        page.value = 0
        loadAttempts()
    }, 300)
})
watch(page, () => loadAttempts())

// ---------- Bans tab ----------
const bansPage = ref(0)
const {
    data: bansRes,
    isLoading: bansLoading,
    execute: loadBans,
} = useFetch((signal: AbortSignal) => securityService.listBans(signal, { page: bansPage.value, size }), { immediate: true })
const bans = computed<AccessBan[]>(() => bansRes.value?.data?.data ?? [])
watch(bansPage, () => loadBans())

// ---------- Ban dialog (shared by both tabs) ----------
const banDialogOpen = ref(false)
const banForm = ref<{ type: BanType; value: string; reason: string; duration: string }>({
    type: 'IP', value: '', reason: '', duration: 'permanent',
})
const banSaving = ref(false)

function openBanDialog(type: BanType, value: string) {
    banForm.value = { type, value, reason: '', duration: 'permanent' }
    banDialogOpen.value = true
}

const DURATIONS: Record<string, number | null> = { '1h': 1, '24h': 24, '7d': 168, permanent: null }

async function submitBan() {
    if (!banForm.value.value.trim()) {
        triggerToast('Value must not be empty', 'error')
        return
    }
    banSaving.value = true
    try {
        const payload: CreateBanPayload = {
            type: banForm.value.type,
            value: banForm.value.value.trim(),
            reason: banForm.value.reason.trim() || undefined,
            durationHours: DURATIONS[banForm.value.duration],
        }
        await securityService.createBan(payload)
        triggerToast(`Banned ${payload.type} ${payload.value}`, 'success')
        banDialogOpen.value = false
        loadBans()
    } catch (err) {
        triggerToast(getErrorMessage(err, 'Could not create ban'), 'error')
    } finally {
        banSaving.value = false
    }
}

// ---------- Unban ----------
const unbanTarget = ref<AccessBan | null>(null)
const unbanning = ref(false)
async function confirmUnban() {
    if (!unbanTarget.value) return
    unbanning.value = true
    try {
        await securityService.deleteBan(unbanTarget.value.id)
        triggerToast(`Unbanned ${unbanTarget.value.value}`, 'success')
        unbanTarget.value = null
        loadBans()
    } catch (err) {
        triggerToast(getErrorMessage(err, 'Could not remove ban'), 'error')
    } finally {
        unbanning.value = false
    }
}

const fmt = (iso: string | null) => (iso ? new Date(iso).toLocaleString() : '—')
</script>

<template>
    <div class="space-y-4">
        <div class="flex items-center justify-between">
            <h1 class="text-xl font-semibold">Security</h1>
            <Button v-if="tab === 'bans'" size="sm" @click="openBanDialog('IP', '')">New ban</Button>
        </div>

        <div class="flex gap-1 border-b">
            <button
                v-for="t in (['attempts', 'bans'] as const)" :key="t"
                class="px-3 py-1.5 text-sm capitalize -mb-px border-b-2"
                :class="tab === t ? 'border-primary font-medium' : 'border-transparent text-muted-foreground'"
                @click="tab = t"
            >
                {{ t === 'attempts' ? 'Login attempts' : 'Bans' }}
            </button>
        </div>

        <!-- Attempts tab -->
        <template v-if="tab === 'attempts'">
            <div class="flex flex-wrap items-center gap-2">
                <Input v-model="fIp" placeholder="Filter by IP" class="h-8 w-40" />
                <Input v-model="fUsername" placeholder="Filter by username" class="h-8 w-44" />
                <Select v-model="fResult">
                    <SelectTrigger class="h-8 w-32"><SelectValue /></SelectTrigger>
                    <SelectContent>
                        <SelectItem value="all">All results</SelectItem>
                        <SelectItem value="ok">Success</SelectItem>
                        <SelectItem value="fail">Failed</SelectItem>
                    </SelectContent>
                </Select>
                <Input v-model="fFrom" type="date" class="h-8 w-36" aria-label="From date" />
                <Input v-model="fTo" type="date" class="h-8 w-36" aria-label="To date" />
            </div>

            <div class="overflow-x-auto rounded-md border">
                <table class="w-full text-sm">
                    <thead class="bg-muted/50 text-left">
                        <tr>
                            <th class="px-3 py-2">Time</th>
                            <th class="px-3 py-2">Username</th>
                            <th class="px-3 py-2">IP</th>
                            <th class="px-3 py-2">Device</th>
                            <th class="px-3 py-2">Result</th>
                            <th class="px-3 py-2 text-right">Actions</th>
                        </tr>
                    </thead>
                    <tbody>
                        <tr v-if="attemptsLoading"><td colspan="6" class="px-3 py-6 text-center text-muted-foreground">Loading…</td></tr>
                        <tr v-else-if="!attempts.length"><td colspan="6" class="px-3 py-6 text-center text-muted-foreground">No login attempts match these filters.</td></tr>
                        <tr v-for="a in attempts" :key="a.id" class="border-t">
                            <td class="whitespace-nowrap px-3 py-2">{{ fmt(a.createdAt) }}</td>
                            <td class="px-3 py-2">{{ a.username ?? '—' }}</td>
                            <td class="px-3 py-2 font-mono text-xs">{{ a.ip ?? '—' }}</td>
                            <td class="max-w-40 truncate px-3 py-2 font-mono text-xs" :title="a.deviceHash ?? ''">{{ a.deviceHash ?? '—' }}</td>
                            <td class="px-3 py-2">
                                <Badge :variant="a.success ? 'secondary' : 'destructive'">
                                    {{ a.success ? 'Success' : (a.errorCode ?? 'Failed') }}
                                </Badge>
                            </td>
                            <td class="px-3 py-2 text-right">
                                <Button v-if="a.ip" variant="ghost" size="sm" @click="openBanDialog('IP', a.ip)">Ban IP</Button>
                                <Button v-if="a.deviceHash" variant="ghost" size="sm" @click="openBanDialog('DEVICE', a.deviceHash)">Ban device</Button>
                            </td>
                        </tr>
                    </tbody>
                </table>
            </div>

            <div class="flex items-center justify-end gap-2">
                <Button variant="outline" size="sm" :disabled="!pagination?.hasPrev" @click="page--">Previous</Button>
                <Button variant="outline" size="sm" :disabled="!pagination?.hasNext" @click="page++">Next</Button>
            </div>
        </template>

        <!-- Bans tab -->
        <template v-else>
            <div class="overflow-x-auto rounded-md border">
                <table class="w-full text-sm">
                    <thead class="bg-muted/50 text-left">
                        <tr>
                            <th class="px-3 py-2">Type</th>
                            <th class="px-3 py-2">Value</th>
                            <th class="px-3 py-2">Reason</th>
                            <th class="px-3 py-2">Expires</th>
                            <th class="px-3 py-2">By</th>
                            <th class="px-3 py-2 text-right">Actions</th>
                        </tr>
                    </thead>
                    <tbody>
                        <tr v-if="bansLoading"><td colspan="6" class="px-3 py-6 text-center text-muted-foreground">Loading…</td></tr>
                        <tr v-else-if="!bans.length"><td colspan="6" class="px-3 py-6 text-center text-muted-foreground">No active bans. Use “New ban” or the Attempts tab.</td></tr>
                        <tr v-for="b in bans" :key="b.id" class="border-t">
                            <td class="px-3 py-2"><Badge variant="outline">{{ b.type }}</Badge></td>
                            <td class="px-3 py-2 font-mono text-xs">{{ b.value }}</td>
                            <td class="max-w-52 truncate px-3 py-2" :title="b.reason ?? ''">{{ b.reason ?? '—' }}</td>
                            <td class="whitespace-nowrap px-3 py-2">{{ b.expiresAt ? fmt(b.expiresAt) : 'Permanent' }}</td>
                            <td class="px-3 py-2">{{ b.createdBy ?? '—' }}</td>
                            <td class="px-3 py-2 text-right">
                                <Button variant="ghost" size="sm" class="text-destructive" @click="unbanTarget = b">Unban</Button>
                            </td>
                        </tr>
                    </tbody>
                </table>
            </div>
        </template>

        <!-- Create-ban dialog -->
        <Dialog v-model:open="banDialogOpen">
            <DialogContent class="sm:max-w-md">
                <DialogHeader>
                    <DialogTitle>Ban {{ banForm.type === 'IP' ? 'IP address' : 'device' }}</DialogTitle>
                    <DialogDescription>Blocked clients receive 403 on every request until the ban expires or is removed.</DialogDescription>
                </DialogHeader>
                <div class="space-y-3">
                    <Select v-model="banForm.type">
                        <SelectTrigger class="w-full"><SelectValue /></SelectTrigger>
                        <SelectContent>
                            <SelectItem value="IP">IP</SelectItem>
                            <SelectItem value="DEVICE">Device</SelectItem>
                        </SelectContent>
                    </Select>
                    <Input v-model="banForm.value" :placeholder="banForm.type === 'IP' ? 'e.g. 203.0.113.7' : 'device hash'" />
                    <Input v-model="banForm.reason" placeholder="Reason (optional)" />
                    <Select v-model="banForm.duration">
                        <SelectTrigger class="w-full"><SelectValue /></SelectTrigger>
                        <SelectContent>
                            <SelectItem value="1h">1 hour</SelectItem>
                            <SelectItem value="24h">24 hours</SelectItem>
                            <SelectItem value="7d">7 days</SelectItem>
                            <SelectItem value="permanent">Permanent</SelectItem>
                        </SelectContent>
                    </Select>
                </div>
                <DialogFooter>
                    <Button variant="outline" @click="banDialogOpen = false">Cancel</Button>
                    <Button :disabled="banSaving" @click="submitBan">Ban</Button>
                </DialogFooter>
            </DialogContent>
        </Dialog>

        <!-- Unban confirm -->
        <Dialog :open="!!unbanTarget" @update:open="(v: boolean) => { if (!v) unbanTarget = null }">
            <DialogContent class="sm:max-w-sm">
                <DialogHeader>
                    <DialogTitle>Remove this ban?</DialogTitle>
                    <DialogDescription>
                        {{ unbanTarget?.type }} <span class="font-mono">{{ unbanTarget?.value }}</span> will immediately regain access.
                    </DialogDescription>
                </DialogHeader>
                <DialogFooter>
                    <Button variant="outline" @click="unbanTarget = null">Cancel</Button>
                    <Button variant="destructive" :disabled="unbanning" @click="confirmUnban">Unban</Button>
                </DialogFooter>
            </DialogContent>
        </Dialog>
    </div>
</template>
```

Adjust imports to the exact component paths/composables the file's neighbors use (`AdminUsersPage.vue` is the source of truth — e.g. if `useFetch`/`getErrorMessage` live elsewhere, mirror it).

- [ ] **Step 3: Verify + commit**

```bash
npm run type-check && npm run lint && npm run test
```
Expected: all clean (any lint auto-fixes are fine — `--fix` is in the script).

```bash
git add -A && git commit -m "feat(security): admin Security page — login attempts + bans"
```

---

### Task 11: Deploy + live verification

**Files:** none (operational).

- [ ] **Step 1: Rebuild the two code services** (skip prometheus — AppArmor quirk):

```bash
cd /home/tuan/Project/my-oj/judge-deployment && docker compose up -d --build judge-api judge-portal
```

- [ ] **Step 2: Flyway check**

```bash
docker exec oj-db psql -U postgres -d my_oj -t -c "select max(version::int) from flyway_schema_history;"
```
Expected: `11`.

- [ ] **Step 3: Rate-limit scenario** — 6 bad passwords then observe 429:

```bash
for i in $(seq 1 6); do curl -s -o /dev/null -w "try $i: %{http_code}\n" \
  -X POST http://localhost:8000/api/v1/auth/login -H 'Content-Type: application/json' \
  -H 'X-Device-Id: spec-c-probe' -d '{"username":"ratelimit-probe","password":"wrong"}'; done
```
Expected: five `401` then `429` (username threshold = 5). Also check the header:

```bash
curl -s -D - -o /dev/null -X POST http://localhost:8000/api/v1/auth/login \
  -H 'Content-Type: application/json' -H 'X-Device-Id: spec-c-probe' \
  -d '{"username":"ratelimit-probe","password":"wrong"}' | grep -i retry-after
```
Expected: `Retry-After: 900`.

- [ ] **Step 4: Attempts + ban scenario** — login as admin (from a DIFFERENT device id so the self-ban guard doesn't fire on the probe), list attempts, ban the probe device, verify 403, unban:

```bash
cd /tmp && curl -s -c /tmp/specc.txt -X POST http://localhost:8000/api/v1/auth/login \
  -H 'Content-Type: application/json' -H 'X-Device-Id: admin-console' \
  -d '{"username":"admin","password":"Admin@12345"}' -o /dev/null -w 'admin login: %{http_code}\n'
curl -s -b /tmp/specc.txt 'http://localhost:8000/api/v1/security/attempts?username=ratelimit-probe&size=3' | head -c 400; echo
curl -s -b /tmp/specc.txt -X POST http://localhost:8000/api/v1/security/bans \
  -H 'Content-Type: application/json' \
  -d '{"type":"DEVICE","value":"spec-c-probe","reason":"live verify","durationHours":1}' -o /dev/null -w 'ban: %{http_code}\n'
curl -s -o /dev/null -w 'banned probe request: %{http_code}\n' \
  http://localhost:8000/api/v1/problems -H 'X-Device-Id: spec-c-probe'
```
Expected: attempts JSON shows the 6 failures; `ban: 200`; banned probe gets `403`. Then unban via `DELETE /security/bans/{id}` (id from the ban response or `GET /security/bans`) and confirm the probe gets `200` again. Also verify self-ban guard: banning `admin-console` from the admin session → `409`.

- [ ] **Step 5: UI smoke** — `http://localhost/admin/security` renders both tabs; metrics visible:

```bash
curl -s http://localhost:8000/actuator/prometheus | grep -E 'oj_login_rate_limited|oj_request_banned'
```

- [ ] **Step 6: Push both branches**

```bash
git -C /home/tuan/Project/my-oj/judge-api push -u origin feature/rate-limit-ban
git -C /home/tuan/Project/my-oj/judge-portal push -u origin feature/rate-limit-ban
```
Then open ONE PR per repo with base **`main`** (no stacking this time — main is complete).
