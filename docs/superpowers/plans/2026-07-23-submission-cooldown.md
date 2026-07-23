# Submission Cooldown (Spec D) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every user waits 10 seconds between two submissions; the submit button counts the wait down proactively.

**Architecture:** An atomic Redis `SET NX EX` gate (`SubmissionRateLimiter.acquire(userId)`) called inside `SubmissionService.submit()` after problem/language/user resolve. A new `RateLimitedException` carries the remaining seconds so `GlobalExceptionHandler` can emit a dynamic `Retry-After`. Frontend: a `useCooldown()` composable drives the submit button countdown.

**Tech Stack:** Spring Boot 3 (Maven `./mvnw`), `StringRedisTemplate`, Micrometer; Vue 3 + vitest (node env, fake timers).

## Global Constraints

- Branch `feature/submission-cooldown` off `main` in BOTH repos (`/home/tuan/Project/my-oj/judge-api`, `.../judge-portal`).
- judge-api tests: `./mvnw -q test -Dtest='<Class>' -Dsurefire.failIfNoSpecifiedTests=false`.
- No migration — no new tables or permissions in this spec.
- Cooldown default **10s**, configurable `oj.submission.cooldown-seconds` (`@Value` default + explicit yml entry).
- Redis down → **fail open** (log + allow). Metric on block: `oj.submission.rate_limited`.
- Login `RATE_LIMITED` path (hardcoded Retry-After 900) stays untouched.
- New non-@Component filters/beans NOT needed here; `SubmissionRateLimiter` is a plain `@Component` (no Filter interface → safe for @WebMvcTest slices).
- Commits end with the repo's Co-Authored-By + Claude-Session trailer.

---

### Task 1: judge-api — ErrorCode, RateLimitedException, dynamic Retry-After (TDD)

**Files:**
- Modify: `judge-api/src/main/java/vn/thanhtuanle/common/exception/ErrorCode.java`
- Create: `judge-api/src/main/java/vn/thanhtuanle/common/exception/RateLimitedException.java`
- Modify: `judge-api/src/main/java/vn/thanhtuanle/common/exception/GlobalExceptionHandler.java`
- Test: `judge-api/src/test/java/vn/thanhtuanle/common/exception/GlobalExceptionHandlerRetryAfterTest.java`

**Interfaces:**
- Produces: `ErrorCode.SUBMISSION_RATE_LIMITED (429)`; `RateLimitedException(ErrorCode, long retryAfterSeconds)` with getter `getRetryAfterSeconds()`; handler answers 429 + `Retry-After: <seconds>`.

- [ ] **Step 1: Branch**

```bash
cd /home/tuan/Project/my-oj/judge-api && git checkout -b feature/submission-cooldown main
```

- [ ] **Step 2: Failing handler test**

```java
package vn.thanhtuanle.common.exception;

import org.junit.jupiter.api.Test;
import org.springframework.http.ResponseEntity;
import vn.thanhtuanle.common.payload.ApiResponse;

import static org.assertj.core.api.Assertions.assertThat;

class GlobalExceptionHandlerRetryAfterTest {

    private final GlobalExceptionHandler handler = new GlobalExceptionHandler();

    @Test
    void rateLimitedException_carriesDynamicRetryAfter() {
        RateLimitedException ex = new RateLimitedException(ErrorCode.SUBMISSION_RATE_LIMITED, 7);

        ResponseEntity<ApiResponse<Object>> res = handler.handleRateLimitedException(ex);

        assertThat(res.getStatusCode().value()).isEqualTo(429);
        assertThat(res.getHeaders().getFirst("Retry-After")).isEqualTo("7");
        assertThat(res.getBody().getMessage()).contains("submitting too fast");
    }

    @Test
    void loginRateLimit_keepsItsFixed900() {
        ResponseEntity<ApiResponse<Object>> res =
                handler.handleAppException(new AppException(ErrorCode.RATE_LIMITED));
        assertThat(res.getHeaders().getFirst("Retry-After")).isEqualTo("900");
    }
}
```

- [ ] **Step 3: Run to verify FAIL** (compilation: `SUBMISSION_RATE_LIMITED`, `RateLimitedException`, `handleRateLimitedException` missing).

Run: `./mvnw -q test -Dtest='GlobalExceptionHandlerRetryAfterTest' -Dsurefire.failIfNoSpecifiedTests=false`

- [ ] **Step 4: Implement** — ErrorCode (before `UNCATEGORIZED_EXCEPTION`):

```java
    SUBMISSION_RATE_LIMITED("You are submitting too fast. Try again in a moment", HttpStatus.TOO_MANY_REQUESTS),
```

`RateLimitedException.java`:

```java
package vn.thanhtuanle.common.exception;

import lombok.Getter;

/** An AppException that knows how long the caller must wait — mapped to a dynamic Retry-After. */
@Getter
public class RateLimitedException extends AppException {

    private final long retryAfterSeconds;

    public RateLimitedException(ErrorCode errorCode, long retryAfterSeconds) {
        super(errorCode);
        this.retryAfterSeconds = retryAfterSeconds;
    }
}
```

`GlobalExceptionHandler` — add ABOVE `handleAppException` (Spring picks the most specific handler, so RateLimitedException lands here, not in the AppException one):

```java
    @ExceptionHandler(RateLimitedException.class)
    public ResponseEntity<ApiResponse<Object>> handleRateLimitedException(RateLimitedException ex) {
        ErrorCode errorCode = ex.getErrorCode();
        return ResponseEntity
                .status(errorCode.getStatusCode())
                .header(org.springframework.http.HttpHeaders.RETRY_AFTER, String.valueOf(ex.getRetryAfterSeconds()))
                .body(ApiResponse.error(errorCode.getStatusCode().value(), errorCode.getMessage()));
    }
```

- [ ] **Step 5: Run to verify PASS** (2 tests), then commit:

```bash
git add -A && git commit -m "feat(security): RateLimitedException with dynamic Retry-After + submission error code"
```

---

### Task 2: judge-api — SubmissionRateLimiter (TDD)

**Files:**
- Create: `judge-api/src/main/java/vn/thanhtuanle/security/SubmissionRateLimiter.java`
- Modify: `judge-api/src/main/resources/application.yml` (add `oj.submission.cooldown-seconds`)
- Test: `judge-api/src/test/java/vn/thanhtuanle/security/SubmissionRateLimiterTest.java`

**Interfaces:**
- Consumes: `StringRedisTemplate.opsForValue().setIfAbsent(key, "1", Duration)`, `redis.getExpire(key, TimeUnit.SECONDS)`, `MeterRegistry`, Task 1's `RateLimitedException`.
- Produces: `void acquire(UUID userId)` — arms the cooldown atomically or throws `RateLimitedException(SUBMISSION_RATE_LIMITED, remaining)`.

- [ ] **Step 1: Failing test**

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
import vn.thanhtuanle.common.exception.RateLimitedException;

import java.time.Duration;
import java.util.UUID;
import java.util.concurrent.TimeUnit;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatCode;
import static org.assertj.core.api.Assertions.catchThrowableOfType;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.anyString;
import static org.mockito.ArgumentMatchers.eq;
import static org.mockito.Mockito.when;

@ExtendWith(MockitoExtension.class)
@MockitoSettings(strictness = Strictness.LENIENT)
class SubmissionRateLimiterTest {

    private static final UUID USER = UUID.fromString("d0000000-0000-0000-0000-000000000001");

    @Mock StringRedisTemplate redis;
    @Mock ValueOperations<String, String> valueOps;

    SubmissionRateLimiter limiter;

    @BeforeEach
    void setUp() {
        when(redis.opsForValue()).thenReturn(valueOps);
        limiter = new SubmissionRateLimiter(redis, new SimpleMeterRegistry(), 10);
    }

    @Test
    void firstSubmission_armsCooldown_andPasses() {
        when(valueOps.setIfAbsent(eq("oj:rl:submit:" + USER), eq("1"), eq(Duration.ofSeconds(10)))).thenReturn(true);
        assertThatCode(() -> limiter.acquire(USER)).doesNotThrowAnyException();
    }

    @Test
    void withinCooldown_throwsWithRemainingSeconds() {
        when(valueOps.setIfAbsent(anyString(), anyString(), any(Duration.class))).thenReturn(false);
        when(redis.getExpire("oj:rl:submit:" + USER, TimeUnit.SECONDS)).thenReturn(7L);

        RateLimitedException ex = catchThrowableOfType(() -> limiter.acquire(USER), RateLimitedException.class);

        assertThat(ex).isNotNull();
        assertThat(ex.getRetryAfterSeconds()).isEqualTo(7);
    }

    @Test
    void expiredButPresentKey_clampsRetryAfterToAtLeast1() {
        when(valueOps.setIfAbsent(anyString(), anyString(), any(Duration.class))).thenReturn(false);
        when(redis.getExpire(anyString(), any(TimeUnit.class))).thenReturn(0L);

        RateLimitedException ex = catchThrowableOfType(() -> limiter.acquire(USER), RateLimitedException.class);

        assertThat(ex.getRetryAfterSeconds()).isEqualTo(1);
    }

    @Test
    void redisDown_failsOpen() {
        when(valueOps.setIfAbsent(anyString(), anyString(), any(Duration.class))).thenThrow(new RuntimeException("down"));
        assertThatCode(() -> limiter.acquire(USER)).doesNotThrowAnyException();
    }
}
```

- [ ] **Step 2: Run to verify FAIL** (class missing), same `-Dtest` pattern.

- [ ] **Step 3: Implement**

```java
package vn.thanhtuanle.security;

import io.micrometer.core.instrument.MeterRegistry;
import lombok.extern.slf4j.Slf4j;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.data.redis.core.StringRedisTemplate;
import org.springframework.stereotype.Component;
import vn.thanhtuanle.common.exception.ErrorCode;
import vn.thanhtuanle.common.exception.RateLimitedException;

import java.time.Duration;
import java.util.UUID;
import java.util.concurrent.TimeUnit;

/**
 * Per-user submission cooldown: one atomic SET NX EX per submit. If the key is still
 * there, the user is inside the cooldown and gets a 429 whose Retry-After is the key's
 * remaining TTL. Redis being down fails OPEN — submissions beat enforcement.
 */
@Component
@Slf4j
public class SubmissionRateLimiter {

    static final String KEY_PREFIX = "oj:rl:submit:";

    private final StringRedisTemplate redis;
    private final MeterRegistry registry;
    private final long cooldownSeconds;

    public SubmissionRateLimiter(StringRedisTemplate redis, MeterRegistry registry,
                                 @Value("${oj.submission.cooldown-seconds:10}") long cooldownSeconds) {
        this.redis = redis;
        this.registry = registry;
        this.cooldownSeconds = cooldownSeconds;
    }

    /** Arms the cooldown atomically; throws 429 with the remaining seconds if still cooling down. */
    public void acquire(UUID userId) {
        String key = KEY_PREFIX + userId;
        Long remaining;
        try {
            Boolean armed = redis.opsForValue().setIfAbsent(key, "1", Duration.ofSeconds(cooldownSeconds));
            if (!Boolean.FALSE.equals(armed)) {
                return; // armed now (TRUE) — or a null answer from a flaky proxy: fail open
            }
            remaining = redis.getExpire(key, TimeUnit.SECONDS);
        } catch (Exception e) {
            log.warn("Submission limiter unavailable, allowing: {}", e.getMessage());
            return;
        }
        long retryAfter = (remaining == null || remaining < 1) ? 1 : remaining;
        registry.counter("oj.submission.rate_limited").increment();
        throw new RateLimitedException(ErrorCode.SUBMISSION_RATE_LIMITED, retryAfter);
    }
}
```

`application.yml` — add a top-level block (next to the existing `minio:` block):

```yaml
oj:
  submission:
    cooldown-seconds: ${SUBMISSION_COOLDOWN_SECONDS:10}
```

- [ ] **Step 4: Run to verify PASS** (4 tests), commit:

```bash
git add -A && git commit -m "feat(security): per-user submission cooldown limiter (Redis SET NX EX)"
```

---

### Task 3: judge-api — wire into SubmissionService + full suite

**Files:**
- Modify: `judge-api/src/main/java/vn/thanhtuanle/submission/SubmissionService.java`
- Test: `judge-api/src/test/java/vn/thanhtuanle/submission/SubmissionServiceCooldownTest.java`

**Interfaces:**
- Consumes: `SubmissionRateLimiter.acquire(UUID)`; existing `submit()` flow (`judgeService.validate`, repos, `userService.getCurrentUser()`).
- Produces: `submit()` that consults the limiter AFTER problem/language/user resolve and BEFORE persisting.

- [ ] **Step 1: Failing test**

```java
package vn.thanhtuanle.submission;

import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.extension.ExtendWith;
import org.mockito.InjectMocks;
import org.mockito.Mock;
import org.mockito.junit.jupiter.MockitoExtension;
import org.mockito.junit.jupiter.MockitoSettings;
import org.mockito.quality.Strictness;
import org.springframework.context.ApplicationEventPublisher;
import vn.thanhtuanle.common.exception.ErrorCode;
import vn.thanhtuanle.common.exception.RateLimitedException;
import vn.thanhtuanle.common.exception.ResourceNotFoundException;
import vn.thanhtuanle.entity.Language;
import vn.thanhtuanle.entity.Problem;
import vn.thanhtuanle.entity.Submission;
import vn.thanhtuanle.entity.User;
import vn.thanhtuanle.judge.JudgeService;
import vn.thanhtuanle.language.LanguageRepository;
import vn.thanhtuanle.messaging.event.SubmissionRequestedEvent;
import vn.thanhtuanle.problem.ProblemRepository;
import vn.thanhtuanle.security.SubmissionRateLimiter;
import vn.thanhtuanle.submission.dto.SubmissionRequestDto;
import vn.thanhtuanle.submission.dto.SubmissionResponseDto;
import vn.thanhtuanle.user.UserService;

import java.util.Optional;
import java.util.UUID;

import static org.assertj.core.api.Assertions.assertThatCode;
import static org.assertj.core.api.Assertions.assertThatThrownBy;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.anyString;
import static org.mockito.Mockito.doThrow;
import static org.mockito.Mockito.never;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

@ExtendWith(MockitoExtension.class)
@MockitoSettings(strictness = Strictness.LENIENT)
class SubmissionServiceCooldownTest {

    @Mock SubmissionRepository submissionRepository;
    @Mock JudgeService judgeService;
    @Mock ProblemRepository problemRepository;
    @Mock LanguageRepository languageRepository;
    @Mock SubmissionMapper submissionMapper;
    @Mock UserService userService;
    @Mock ApplicationEventPublisher applicationEventPublisher;
    @Mock SubmissionSseRegistry sseRegistry;
    @Mock SubmissionRateLimiter submissionRateLimiter;

    @InjectMocks SubmissionService service;

    private final UUID userId = UUID.randomUUID();
    private SubmissionRequestDto req;

    @BeforeEach
    void setUp() {
        req = new SubmissionRequestDto();
        req.setSourceCode("print(1)");
        req.setLanguageIdentifier("python3");
        req.setProblemSlug("simple-a-plus-b");

        User user = User.builder().username("u").build();
        user.setId(userId);
        when(problemRepository.findByProblemSlug("simple-a-plus-b")).thenReturn(Optional.of(new Problem()));
        when(languageRepository.findByIdentifier("python3")).thenReturn(Optional.of(new Language()));
        when(userService.getCurrentUser()).thenReturn(user);
        when(submissionRepository.save(any())).thenAnswer(inv -> {
            Submission s = inv.getArgument(0);
            s.setId(UUID.randomUUID());
            return s;
        });
        when(judgeService.buildRequestedEvent(anyString(), anyString(), any(), any()))
                .thenReturn(SubmissionRequestedEvent.builder().build());
        when(submissionMapper.toDto(any())).thenReturn(new SubmissionResponseDto());
    }

    @Test
    void submit_acquiresCooldown_forTheCurrentUser() {
        assertThatCode(() -> service.submit(req)).doesNotThrowAnyException();
        verify(submissionRateLimiter).acquire(userId);
    }

    @Test
    void unknownProblem_doesNotBurnTheCooldown() {
        when(problemRepository.findByProblemSlug("simple-a-plus-b")).thenReturn(Optional.empty());
        assertThatThrownBy(() -> service.submit(req)).isInstanceOf(ResourceNotFoundException.class);
        verify(submissionRateLimiter, never()).acquire(any());
    }

    @Test
    void withinCooldown_nothingIsPersisted() {
        doThrow(new RateLimitedException(ErrorCode.SUBMISSION_RATE_LIMITED, 7))
                .when(submissionRateLimiter).acquire(userId);
        assertThatThrownBy(() -> service.submit(req)).isInstanceOf(RateLimitedException.class);
        verify(submissionRepository, never()).save(any());
    }
}
```

NOTE for the implementer: check the real package/type names before compiling —
`LanguageRepository`'s package, `SubmissionMapper`, `SubmissionSseRegistry`,
`SubmissionRequestedEvent.builder()`, and `ResourceNotFoundException`'s package
are taken from `SubmissionService`'s own imports; mirror whatever that file uses.

- [ ] **Step 2: Run to verify FAIL** (limiter not injected / not called).

- [ ] **Step 3: Implement** — in `SubmissionService`: add `private final SubmissionRateLimiter submissionRateLimiter;` (import `vn.thanhtuanle.security.SubmissionRateLimiter`) and insert after `User currentUser = userService.getCurrentUser();`:

```java
        // Cooldown gate: sits after problem/language/user resolve so a 404 never burns it,
        // and before the row exists so a throttled submit leaves no trace.
        submissionRateLimiter.acquire(currentUser.getId());
```

- [ ] **Step 4: Run to verify PASS**, then the FULL suite:

```bash
./mvnw test 2>&1 | grep -E 'Tests run: [0-9]+, Failures: [0-9]+, Errors: [0-9]+, Skipped: [0-9]+$|BUILD' | tail -2
```
Expected: `BUILD SUCCESS`, 0 failures (≈189 tests).

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(submission): enforce per-user cooldown on submit"
```

---

### Task 4: judge-portal — useCooldown composable (TDD)

**Files:**
- Create: `judge-portal/src/composables/useCooldown.ts`
- Test: `judge-portal/src/composables/__tests__/useCooldown.spec.ts`

**Interfaces:**
- Produces: `useCooldown()` → `{ remaining: Ref<number>, active: ComputedRef<boolean>, start(seconds: number): void, stop(): void }`.

- [ ] **Step 1: Branch**

```bash
cd /home/tuan/Project/my-oj/judge-portal && git checkout -b feature/submission-cooldown main
```

- [ ] **Step 2: Failing spec**

```ts
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { useCooldown } from '@/composables/useCooldown'

beforeEach(() => vi.useFakeTimers())
afterEach(() => vi.useRealTimers())

describe('useCooldown', () => {
    it('starts inactive', () => {
        const cd = useCooldown()
        expect(cd.active.value).toBe(false)
        expect(cd.remaining.value).toBe(0)
    })

    it('counts down once per second and deactivates at zero', () => {
        const cd = useCooldown()
        cd.start(3)
        expect(cd.active.value).toBe(true)
        expect(cd.remaining.value).toBe(3)
        vi.advanceTimersByTime(1000)
        expect(cd.remaining.value).toBe(2)
        vi.advanceTimersByTime(2000)
        expect(cd.remaining.value).toBe(0)
        expect(cd.active.value).toBe(false)
    })

    it('restart replaces the previous countdown', () => {
        const cd = useCooldown()
        cd.start(30)
        cd.start(5)
        vi.advanceTimersByTime(5000)
        expect(cd.remaining.value).toBe(0)
        expect(cd.active.value).toBe(false)
    })

    it('non-positive input is a no-op', () => {
        const cd = useCooldown()
        cd.start(0)
        expect(cd.active.value).toBe(false)
    })
})
```

Run: `npx vitest run src/composables/__tests__/useCooldown.spec.ts` → FAIL (module missing).

- [ ] **Step 3: Implement**

```ts
import { computed, ref } from 'vue'

/**
 * A simple seconds countdown for cooldown UX (e.g. the submit button).
 * start(n) (re)starts it; `active` flips false when it reaches zero.
 */
export function useCooldown() {
    const remaining = ref(0)
    let timer: ReturnType<typeof setInterval> | undefined

    function stop() {
        if (timer) {
            clearInterval(timer)
            timer = undefined
        }
    }

    function start(seconds: number) {
        stop()
        remaining.value = Math.max(0, Math.ceil(seconds))
        if (remaining.value === 0) return
        timer = setInterval(() => {
            remaining.value -= 1
            if (remaining.value <= 0) {
                remaining.value = 0
                stop()
            }
        }, 1000)
    }

    const active = computed(() => remaining.value > 0)

    return { remaining, active, start, stop }
}
```

- [ ] **Step 4: Run to verify PASS** (4 tests), commit:

```bash
git add -A && git commit -m "feat(submission): useCooldown composable for countdown UX"
```

---

### Task 5: judge-portal — submit button countdown + checks

**Files:**
- Modify: `judge-portal/src/components/problem/detail/ProblemSubmit.vue` (button ~line 76-80, handleSubmit ~line 170-198)

**Interfaces:**
- Consumes: `useCooldown()` from Task 4; existing `isSubmitting`, `triggerToast`, `getErrorMessage`, `submissionService.submit`.
- Produces: submit button that disables + counts down 10s after success; 429 seeds the counter from `Retry-After`.

- [ ] **Step 1: Wire the composable** — in the `<script setup>` imports add:

```ts
import { isAxiosError } from 'axios'
import { useCooldown } from '@/composables/useCooldown'
```

after `const isSubmitting = ref(false)` add:

```ts
// Matches the API's oj.submission.cooldown-seconds (10s); a 429's Retry-After overrides it.
const COOLDOWN_SECONDS = 10
const { remaining: cooldownRemaining, active: cooldownActive, start: startCooldown } = useCooldown()
```

In `handleSubmit`, right after the success toast (`triggerToast('Submitted', 'success')`) add:

```ts
        startCooldown(COOLDOWN_SECONDS)
```

Replace the catch body (currently `triggerToast(getErrorMessage(error, 'An error occurred'), 'error')`) with:

```ts
        if (isAxiosError(error) && error.response?.status === 429) {
            const ra = Number(error.response.headers?.['retry-after'])
            startCooldown(Number.isFinite(ra) && ra > 0 ? ra : COOLDOWN_SECONDS)
            triggerToast(getErrorMessage(error, 'You are submitting too fast'), 'error')
        } else {
            triggerToast(getErrorMessage(error, 'An error occurred'), 'error')
        }
```

- [ ] **Step 2: Button** — change the submit `<Button>` to:

```vue
                    <Button class="cursor-pointer px-6" @click="handleSubmit"
                        :disabled="isSubmitting || cooldownActive || sourceCode.trim() === ''">
                        <!-- spinner markup unchanged -->
                        {{ isSubmitting ? 'Submitting...' : cooldownActive ? `Submit (${cooldownRemaining}s)` : 'Submit' }}
                    </Button>
```

(Keep whatever spinner/icon markup already sits inside the button; only the `:disabled` expression and the label text change.)

- [ ] **Step 3: Verify + commit**

```bash
npm run type-check && npm run lint && npm run test
```
Expected: clean, 60+ tests pass.

```bash
git add -A && git commit -m "feat(submission): submit button cooldown countdown (10s, Retry-After aware)"
```

---

### Task 6: Deploy + live verification + push

**Files:** none (operational).

- [ ] **Step 1: Rebuild** (never touch prometheus — AppArmor quirk):

```bash
cd /home/tuan/Project/my-oj/judge-deployment && docker compose up -d --build judge-api judge-portal
```

- [ ] **Step 2: Live cooldown check** — login, submit twice fast:

```bash
cd /tmp/claude-1000/-home-tuan-Project-my-oj-judge-server/bcd67cce-adf9-4742-8abc-cfeb24934aee/scratchpad
curl -s -c cd2.txt -X POST http://localhost:8000/api/v1/auth/login -H 'Content-Type: application/json' \
  -H 'X-Device-Id: specd-verify' -d '{"username":"admin","password":"Admin@12345"}' -o /dev/null -w 'login: %{http_code}\n'
BODY='{"sourceCode":"a,b=map(int,input().split())\nprint(a+b)\n","languageIdentifier":"python3","problemSlug":"simple-a-plus-b"}'
curl -s -b cd2.txt -X POST http://localhost:8000/api/v1/submissions -H 'Content-Type: application/json' -d "$BODY" -o /dev/null -w 'submit 1: %{http_code}\n'
curl -s -b cd2.txt -D - -o /dev/null -X POST http://localhost:8000/api/v1/submissions -H 'Content-Type: application/json' -d "$BODY" | grep -iE 'HTTP|retry-after'
sleep 11
curl -s -b cd2.txt -X POST http://localhost:8000/api/v1/submissions -H 'Content-Type: application/json' -d "$BODY" -o /dev/null -w 'submit after wait: %{http_code}\n'
```
Expected: `submit 1: 200`; second shows `HTTP/1.1 429` + `Retry-After: ≤10`; after wait: `200`.

- [ ] **Step 3: Metric**

```bash
curl -s http://localhost:8000/actuator/prometheus | grep '^oj_submission_rate_limited'
```
Expected: `oj_submission_rate_limited_total 1.0`.

- [ ] **Step 4: Push + PRs** (base `main`):

```bash
git -C /home/tuan/Project/my-oj/judge-api push -u origin feature/submission-cooldown
git -C /home/tuan/Project/my-oj/judge-portal push -u origin feature/submission-cooldown
```
