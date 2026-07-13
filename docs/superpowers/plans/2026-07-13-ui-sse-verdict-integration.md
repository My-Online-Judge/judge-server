# UI SSE Verdict Integration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** After an async submit, the portal shows the new submission live in the Submissions table and flips its badge from *Judging* to the final verdict in real time via SSE.

**Architecture:** The frontend opens a browser-native `EventSource` to the existing `GET /submissions/{id}/stream` endpoint (auth rides on the `accessToken` cookie), updates the row from the pushed `verdict` payload, and closes the stream. The backend eliminates the verdict-before-subscribe race by replaying a terminal verdict at subscribe time. A frontend safety timeout + one fallback GET covers SSE failure.

**Tech Stack:** Backend — Spring Boot 3 / Java 17 / JUnit 5 + Mockito (existing). Frontend — Vue 3 + Vite 7 + TypeScript + Pinia (existing); Vitest added for the composable.

Design spec: `docs/superpowers/specs/2026-07-13-ui-sse-verdict-integration-design.md`.

## Global Constraints

- **Repos:** backend changes in `/home/tuan/Project/my-oj/judge-api`; frontend changes in `/home/tuan/Project/my-oj/judge-portal`; the E2E runbook lives in `/home/tuan/Project/my-oj/judge-server`. See **Preflight** for branch/baseline setup.
- **Status codes (must match exactly).** Backend enum `vn.thanhtuanle.common.enums.SubmissionResult`: `PENDING(6)`, `JUDGING(7)`; verdict codes `COMPILE_ERROR(-2)`, `WRONG_ANSWER(-1)`, `ACCEPTED(0)`, `CPU_TIME_LIMIT_EXCEEDED(1)`, `REAL_TIME_LIMIT_EXCEEDED(2)`, `MEMORY_LIMIT_EXCEEDED(3)`, `RUNTIME_ERROR(4)`, `SYSTEM_ERROR(5)`, `PARTIALLY_ACCEPTED(8)`. Frontend enum `SubmissionResult` in `src/types/submission.ts` already mirrors these (working-tree, uncommitted). **Terminal** = any status except `PENDING(6)` and `JUDGING(7)`.
- **SSE contract.** Endpoint `GET /api/v1/submissions/{id}/stream`, `text/event-stream`. Server emits a single named event `verdict` whose `data` is JSON of `SubmissionResponseDto` (fields: `id`, `createdAt`, `status`, `result`, `cpuTime`, `memory`, `errorMessage`, `language`, `details`), then completes.
- **Auth.** `JwtAuthenticationFilter` already reads the JWT from the `accessToken` cookie when no `Authorization` header is present. The frontend MUST open the stream with `new EventSource(url, { withCredentials: true })`. CORS already allows credentials for `http://localhost:5173` and `http://localhost`. Do NOT add a fetch/polyfill SSE client.
- **EventSource discipline.** The client MUST call `.close()` after receiving `verdict` (and on error / unmount). A stream left open reconnects and re-subscribes forever.
- **Fallback timeout:** `20000` ms with no event → one `GET /submissions/{id}`; update the row only if the fetched status is terminal.
- **Conventions:** DRY, YAGNI, TDD where a runner exists, frequent commits. Follow existing patterns (Lombok/`@RequiredArgsConstructor`, MapStruct, the `useFetch` composable style, service singleton export).

---

## Preflight (branch & baseline — run once before Task 1)

- [ ] **judge-api** — this feature extends the unmerged Kafka work, so build on the same branch.

```bash
cd /home/tuan/Project/my-oj/judge-api
git branch --show-current    # expect: feature/kafka-judge ; working tree clean
```

- [ ] **judge-portal** — there is pre-existing uncommitted WIP (enum additions, badge colors, router tweak) in files this plan also touches. Commit it as a baseline first, then branch, so this feature is reviewable in isolation.

```bash
cd /home/tuan/Project/my-oj/judge-portal
git add -A && git commit -m "chore: baseline judge-portal WIP before SSE verdict UI"
git checkout -b feature/sse-verdict-ui
```

- [ ] **judge-server** — the runbook update (Task 4) commits here.

```bash
cd /home/tuan/Project/my-oj/judge-server
git branch --show-current    # expect: feature/kafka-judge ; working tree clean
```

---

## Task 1: Backend — replay terminal verdict at subscribe time (judge-api)

Move the SSE subscribe logic into the service and, right after registering the emitter, push the verdict immediately if the submission is already terminal. Registration happens **before** the DB read so a verdict firing in between still finds the emitter.

**Files:**
- Modify: `src/main/java/vn/thanhtuanle/submission/SubmissionService.java`
- Modify: `src/main/java/vn/thanhtuanle/submission/SubmissionController.java`
- Modify: `src/test/java/vn/thanhtuanle/submission/SubmissionServiceSubmitTest.java` (add one mock field so injection stays warning-free)
- Test (create): `src/test/java/vn/thanhtuanle/submission/SubmissionServiceStreamTest.java`

Work from: `/home/tuan/Project/my-oj/judge-api`

**Interfaces:**
- Consumes: `SubmissionSseRegistry.subscribe(String): SseEmitter`, `SubmissionSseRegistry.complete(String, SubmissionResponseDto): void`, `SubmissionRepository.findById(UUID)`, `SubmissionMapper.toDto(Submission): SubmissionResponseDto`, `SubmissionResult.PENDING.getValue()`.
- Produces: `SubmissionService.streamVerdict(String id): SseEmitter` (called by the controller).

- [ ] **Step 1: Write the failing test**

Create `src/test/java/vn/thanhtuanle/submission/SubmissionServiceStreamTest.java`:

```java
package vn.thanhtuanle.submission;

import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.extension.ExtendWith;
import org.mockito.InjectMocks;
import org.mockito.InOrder;
import org.mockito.Mock;
import org.mockito.junit.jupiter.MockitoExtension;
import org.springframework.context.ApplicationEventPublisher;
import org.springframework.web.servlet.mvc.method.annotation.SseEmitter;

import vn.thanhtuanle.common.enums.SubmissionResult;
import vn.thanhtuanle.entity.Submission;
import vn.thanhtuanle.judge.JudgeService;
import vn.thanhtuanle.language.LanguageRepository;
import vn.thanhtuanle.problem.ProblemRepository;
import vn.thanhtuanle.submission.dto.SubmissionResponseDto;
import vn.thanhtuanle.submission.mapper.SubmissionMapper;
import vn.thanhtuanle.user.UserService;

import java.util.Optional;
import java.util.UUID;

import static org.assertj.core.api.Assertions.assertThat;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.Mockito.*;

@ExtendWith(MockitoExtension.class)
class SubmissionServiceStreamTest {

    @Mock SubmissionRepository submissionRepository;
    @Mock JudgeService judgeService;
    @Mock ProblemRepository problemRepository;
    @Mock LanguageRepository languageRepository;
    @Mock SubmissionMapper submissionMapper;
    @Mock UserService userService;
    @Mock ApplicationEventPublisher applicationEventPublisher;
    @Mock SubmissionSseRegistry sseRegistry;

    @InjectMocks SubmissionService submissionService;

    @Test
    void streamVerdict_registersEmitterBeforeReadingDb_andReplaysTerminalVerdict() {
        UUID id = UUID.randomUUID();
        Submission s = new Submission();
        s.setId(id);
        s.setStatus(SubmissionResult.ACCEPTED.getValue());
        SubmissionResponseDto dto = SubmissionResponseDto.builder()
                .status(SubmissionResult.ACCEPTED.getValue()).build();
        SseEmitter emitter = new SseEmitter();
        when(sseRegistry.subscribe(id.toString())).thenReturn(emitter);
        when(submissionRepository.findById(id)).thenReturn(Optional.of(s));
        when(submissionMapper.toDto(s)).thenReturn(dto);

        SseEmitter result = submissionService.streamVerdict(id.toString());

        assertThat(result).isSameAs(emitter);
        // Ordering is the race-safety property: subscribe MUST happen before the DB read.
        InOrder inOrder = inOrder(sseRegistry, submissionRepository);
        inOrder.verify(sseRegistry).subscribe(id.toString());
        inOrder.verify(submissionRepository).findById(id);
        verify(sseRegistry).complete(id.toString(), dto);
    }

    @Test
    void streamVerdict_whenPending_doesNotReplay() {
        UUID id = UUID.randomUUID();
        Submission s = new Submission();
        s.setId(id);
        s.setStatus(SubmissionResult.PENDING.getValue());
        when(sseRegistry.subscribe(id.toString())).thenReturn(new SseEmitter());
        when(submissionRepository.findById(id)).thenReturn(Optional.of(s));

        submissionService.streamVerdict(id.toString());

        verify(sseRegistry, never()).complete(any(), any());
        verify(submissionMapper, never()).toDto(any(Submission.class));
    }

    @Test
    void streamVerdict_whenNotFound_completesEmitterWithoutVerdict() {
        UUID id = UUID.randomUUID();
        SseEmitter emitter = mock(SseEmitter.class);
        when(sseRegistry.subscribe(id.toString())).thenReturn(emitter);
        when(submissionRepository.findById(id)).thenReturn(Optional.empty());

        submissionService.streamVerdict(id.toString());

        verify(emitter).complete();
        verify(sseRegistry, never()).complete(any(), any());
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `./mvnw -q test -Dtest=SubmissionServiceStreamTest`
Expected: FAIL — compilation error `cannot find symbol: method streamVerdict(String)`.

- [ ] **Step 3: Add `streamVerdict` to the service**

In `SubmissionService.java`, add the import and a new field, and the method. Add near the other imports:

```java
import org.springframework.web.servlet.mvc.method.annotation.SseEmitter;
```

Add the field alongside the existing final dependencies (e.g. after `applicationEventPublisher`):

```java
    private final SubmissionSseRegistry sseRegistry;
```

Add the method (e.g. after `getSubmissionById`):

```java
    @Transactional(readOnly = true)
    public SseEmitter streamVerdict(String id) {
        // Register the emitter FIRST: if the verdict fires between here and the DB
        // read, the consumer still finds this emitter. Then, if the submission is
        // already terminal, replay the verdict now so a late subscriber isn't lost.
        SseEmitter emitter = sseRegistry.subscribe(id);
        Submission submission = submissionRepository.findById(UUID.fromString(id)).orElse(null);
        if (submission == null) {
            log.info("SSE subscribe for unknown submission {}, completing", id);
            emitter.complete();
            return emitter;
        }
        if (submission.getStatus() != SubmissionResult.PENDING.getValue()) {
            log.info("Submission {} already terminal (status={}) at subscribe, replaying verdict",
                    id, submission.getStatus());
            sseRegistry.complete(id, submissionMapper.toDto(submission));
        }
        return emitter;
    }
```

- [ ] **Step 4: Point the controller at the service and drop its direct registry use**

In `SubmissionController.java`, remove the `sseRegistry` field and rewrite `stream`. Delete this field:

```java
    private final SubmissionSseRegistry sseRegistry;
```

Replace the `stream` method body:

```java
    @GetMapping(value = "/{id}/stream", produces = MediaType.TEXT_EVENT_STREAM_VALUE)
    @Operation(summary = "Stream the verdict for a submission via SSE")
    public SseEmitter stream(@PathVariable String id) {
        log.info("SSE subscribe for submission {}", id);
        return submissionService.streamVerdict(id);
    }
```

(The `import ...SseEmitter;` and `SubmissionSseRegistry` import: keep `SseEmitter`, remove the now-unused `SubmissionSseRegistry` import from the controller.)

- [ ] **Step 5: Keep the existing submit test warning-free**

In `SubmissionServiceSubmitTest.java`, add the new dependency as a mock so `@InjectMocks` has nothing to null-inject. Add alongside the other `@Mock` fields:

```java
    @Mock SubmissionSseRegistry sseRegistry;
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `./mvnw -q test -Dtest=SubmissionServiceStreamTest,SubmissionServiceSubmitTest`
Expected: PASS — 4 tests, output pristine (no Mockito unnecessary-stubbing or null-injection warnings).

- [ ] **Step 7: Commit**

```bash
git add src/main/java/vn/thanhtuanle/submission/SubmissionService.java \
        src/main/java/vn/thanhtuanle/submission/SubmissionController.java \
        src/test/java/vn/thanhtuanle/submission/SubmissionServiceStreamTest.java \
        src/test/java/vn/thanhtuanle/submission/SubmissionServiceSubmitTest.java
git commit -m "feat: replay terminal verdict at SSE subscribe to kill the race"
```

---

## Task 2: Frontend — Vitest setup + `isTerminalStatus` helper (judge-portal)

Stand up a test runner (none exists) and add the shared terminal-status helper used by the composable and the component.

**Files:**
- Modify: `package.json` (devDependency + scripts)
- Modify: `vite.config.ts` (Vitest `test` block)
- Modify: `src/types/submission.ts` (append `isTerminalStatus`)
- Test (create): `src/types/__tests__/submission.spec.ts`

Work from: `/home/tuan/Project/my-oj/judge-portal`

**Interfaces:**
- Produces: `isTerminalStatus(status: number): boolean` from `@/types/submission`.

- [ ] **Step 1: Install Vitest**

```bash
npm install -D vitest@^3
```

- [ ] **Step 2: Add test scripts to `package.json`**

In the `"scripts"` block add:

```json
    "test": "vitest run",
    "test:watch": "vitest"
```

- [ ] **Step 3: Configure Vitest in `vite.config.ts`**

Change the config import from Vite to Vitest (it re-exports `defineConfig` with the `test` key typed) and add a `test` block. The file becomes:

```ts
import { fileURLToPath, URL } from 'node:url'

import { defineConfig } from 'vitest/config'
import vue from '@vitejs/plugin-vue'
import tailwindcss from '@tailwindcss/vite'

// https://vite.dev/config/
export default defineConfig({
  plugins: [
    vue(),
    tailwindcss(),
  ],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url))
    },
  },
  test: {
    environment: 'node',
    include: ['src/**/__tests__/**/*.spec.ts'],
  },
})
```

- [ ] **Step 4: Write the failing test**

Create `src/types/__tests__/submission.spec.ts`:

```ts
import { describe, it, expect } from 'vitest'
import { isTerminalStatus, SubmissionResult } from '@/types/submission'

describe('isTerminalStatus', () => {
  it('treats PENDING and JUDGING as non-terminal', () => {
    expect(isTerminalStatus(SubmissionResult.PENDING)).toBe(false)
    expect(isTerminalStatus(SubmissionResult.JUDGING)).toBe(false)
  })

  it('treats every verdict code as terminal', () => {
    expect(isTerminalStatus(SubmissionResult.SUCCESS)).toBe(true)
    expect(isTerminalStatus(SubmissionResult.WRONG_ANSWER)).toBe(true)
    expect(isTerminalStatus(SubmissionResult.COMPILE_ERROR)).toBe(true)
    expect(isTerminalStatus(SubmissionResult.SYSTEM_ERROR)).toBe(true)
    expect(isTerminalStatus(SubmissionResult.PARTIALLY_ACCEPTED)).toBe(true)
  })
})
```

- [ ] **Step 5: Run test to verify it fails**

Run: `npm run test`
Expected: FAIL — `isTerminalStatus is not a function` (not yet exported).

- [ ] **Step 6: Implement the helper**

Append to `src/types/submission.ts` (after `getSubmissionStatus`):

```ts
export const isTerminalStatus = (status: number): boolean =>
    status !== SubmissionResult.PENDING && status !== SubmissionResult.JUDGING
```

- [ ] **Step 7: Run test to verify it passes**

Run: `npm run test`
Expected: PASS — 2 tests.

- [ ] **Step 8: Commit**

```bash
git add package.json package-lock.json vite.config.ts \
        src/types/submission.ts src/types/__tests__/submission.spec.ts
git commit -m "test: add vitest and isTerminalStatus helper"
```

---

## Task 3: Frontend — `useSubmissionStream` composable (judge-portal)

The SSE lifecycle: open an `EventSource`, deliver the `verdict` once and close, fall back to a single GET on error or after the safety timeout, and clean up on unmount. Includes the two tiny dependencies it needs (route builders + a by-id fetch).

**Files:**
- Modify: `src/constants/apiPath.ts` (add `SUBMISSIONS.DETAIL`, `SUBMISSIONS.STREAM`)
- Modify: `src/services/submissionService.ts` (add `getSubmissionById`)
- Create: `src/composables/useSubmissionStream.ts`
- Test (create): `src/composables/__tests__/useSubmissionStream.spec.ts`

Work from: `/home/tuan/Project/my-oj/judge-portal`

**Interfaces:**
- Consumes: `submissionService.getSubmissionById(id): Promise<AxiosResponse<ApiResponse<Submission>>>`, `API_ROUTES.SUBMISSIONS.STREAM(id): string`, `isTerminalStatus`.
- Produces: `useSubmissionStream()` returning `{ watch(id: string, onVerdict: (v: Submission) => void): void, stop(id: string): void, stopAll(): void }`.

- [ ] **Step 1: Add the route builders**

In `src/constants/apiPath.ts`, replace the `SUBMISSIONS` block with:

```ts
    SUBMISSIONS: {
        ROOT: '/submissions',
        DETAIL: (id: string) => `/submissions/${id}`,
        STREAM: (id: string) => `/submissions/${id}/stream`,
        BY_USER_PROBLEM: (userId: string | number, problemSlug: string) =>
            `/submissions/user/${userId}/problem/${problemSlug}`,
    },
```

- [ ] **Step 2: Add the by-id fetch to the service**

In `src/services/submissionService.ts`, add this method to the `SubmissionService` class (after `submit`):

```ts
    getSubmissionById(id: string): Promise<AxiosResponse<ApiResponse<Submission>>> {
        return axiosClient.get(API_ROUTES.SUBMISSIONS.DETAIL(id))
    }
```

- [ ] **Step 3: Write the failing test**

Create `src/composables/__tests__/useSubmissionStream.spec.ts`:

```ts
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'

vi.mock('@/services/submissionService', () => ({
  default: { getSubmissionById: vi.fn() },
}))

import submissionService from '@/services/submissionService'
import { useSubmissionStream } from '@/composables/useSubmissionStream'
import { SubmissionResult } from '@/types/submission'

class MockEventSource {
  static instances: MockEventSource[] = []
  url: string
  withCredentials: boolean
  listeners: Record<string, ((ev: any) => void)[]> = {}
  closed = false
  constructor(url: string, opts?: { withCredentials?: boolean }) {
    this.url = url
    this.withCredentials = !!opts?.withCredentials
    MockEventSource.instances.push(this)
  }
  addEventListener(type: string, cb: (ev: any) => void) {
    ;(this.listeners[type] ||= []).push(cb)
  }
  close() { this.closed = true }
  emit(type: string, ev: any) { (this.listeners[type] || []).forEach((cb) => cb(ev)) }
}

const verdictEvent = (status: number) => ({ data: JSON.stringify({ id: 's1', status }) })

beforeEach(() => {
  MockEventSource.instances = []
  vi.stubGlobal('EventSource', MockEventSource as unknown as typeof EventSource)
  vi.mocked(submissionService.getSubmissionById).mockReset()
})
afterEach(() => {
  vi.unstubAllGlobals()
  vi.useRealTimers()
})

describe('useSubmissionStream', () => {
  it('opens a credentialed EventSource to the stream URL', () => {
    useSubmissionStream().watch('s1', () => {})
    expect(MockEventSource.instances).toHaveLength(1)
    expect(MockEventSource.instances[0].url).toContain('/submissions/s1/stream')
    expect(MockEventSource.instances[0].withCredentials).toBe(true)
  })

  it('delivers the verdict once, closes, and ignores a later stream-close error', () => {
    const onVerdict = vi.fn()
    useSubmissionStream().watch('s1', onVerdict)
    const es = MockEventSource.instances[0]
    es.emit('verdict', verdictEvent(SubmissionResult.SUCCESS))
    expect(onVerdict).toHaveBeenCalledTimes(1)
    expect(onVerdict).toHaveBeenCalledWith(expect.objectContaining({ status: SubmissionResult.SUCCESS }))
    expect(es.closed).toBe(true)
    es.emit('error', {})
    expect(onVerdict).toHaveBeenCalledTimes(1)
    expect(submissionService.getSubmissionById).not.toHaveBeenCalled()
  })

  it('on error, closes and performs exactly one fallback GET', async () => {
    vi.mocked(submissionService.getSubmissionById).mockResolvedValue(
      { data: { data: { id: 's1', status: SubmissionResult.WRONG_ANSWER } } } as any,
    )
    const onVerdict = vi.fn()
    useSubmissionStream().watch('s1', onVerdict)
    const es = MockEventSource.instances[0]
    es.emit('error', {})
    es.emit('error', {}) // repeated errors must not re-fetch
    await vi.waitFor(() => expect(onVerdict).toHaveBeenCalledTimes(1))
    expect(submissionService.getSubmissionById).toHaveBeenCalledTimes(1)
    expect(es.closed).toBe(true)
  })

  it('falls back after the 20s safety timeout when no event arrives', async () => {
    vi.useFakeTimers()
    vi.mocked(submissionService.getSubmissionById).mockResolvedValue(
      { data: { data: { id: 's1', status: SubmissionResult.SUCCESS } } } as any,
    )
    const onVerdict = vi.fn()
    useSubmissionStream().watch('s1', onVerdict)
    await vi.advanceTimersByTimeAsync(20_000)
    expect(submissionService.getSubmissionById).toHaveBeenCalledTimes(1)
    expect(onVerdict).toHaveBeenCalledWith(expect.objectContaining({ status: SubmissionResult.SUCCESS }))
  })

  it('stopAll closes the stream and cancels the fallback timer', () => {
    vi.useFakeTimers()
    const { watch, stopAll } = useSubmissionStream()
    watch('s1', () => {})
    const es = MockEventSource.instances[0]
    stopAll()
    expect(es.closed).toBe(true)
    vi.advanceTimersByTime(20_000)
    expect(submissionService.getSubmissionById).not.toHaveBeenCalled()
  })
})
```

- [ ] **Step 4: Run test to verify it fails**

Run: `npm run test`
Expected: FAIL — cannot resolve `@/composables/useSubmissionStream`.

- [ ] **Step 5: Implement the composable**

Create `src/composables/useSubmissionStream.ts`:

```ts
import { getCurrentInstance, onBeforeUnmount } from 'vue'
import submissionService from '@/services/submissionService'
import { API_ROUTES } from '@/constants/apiPath'
import { isTerminalStatus, type Submission } from '@/types/submission'

const BASE_URL = import.meta.env.VITE_API_BASE_URL || 'http://localhost:8080/api/v1'
const FALLBACK_TIMEOUT_MS = 20_000

type VerdictHandler = (verdict: Submission) => void

interface Watcher {
    source: EventSource
    timer: ReturnType<typeof setTimeout>
}

export function useSubmissionStream() {
    const watchers = new Map<string, Watcher>()

    const stop = (id: string) => {
        const w = watchers.get(id)
        if (!w) return
        w.source.close()
        clearTimeout(w.timer)
        watchers.delete(id)
    }

    const stopAll = () => {
        for (const id of Array.from(watchers.keys())) stop(id)
    }

    // Idempotent: claims (closes + removes) the watcher synchronously so repeated
    // triggers (multiple 'error' events, or error racing the timeout) fetch once.
    const fallbackFetch = async (id: string, onVerdict: VerdictHandler) => {
        if (!watchers.has(id)) return
        stop(id)
        try {
            const res = await submissionService.getSubmissionById(id)
            const sub = res.data.data
            if (sub && isTerminalStatus(sub.status)) onVerdict(sub)
        } catch {
            // Nothing more we can do; leave the row as Judging.
        }
    }

    const watch = (id: string, onVerdict: VerdictHandler) => {
        if (watchers.has(id)) return

        const source = new EventSource(`${BASE_URL}${API_ROUTES.SUBMISSIONS.STREAM(id)}`, {
            withCredentials: true,
        })
        const timer = setTimeout(() => fallbackFetch(id, onVerdict), FALLBACK_TIMEOUT_MS)
        watchers.set(id, { source, timer })

        source.addEventListener('verdict', (ev: MessageEvent) => {
            let verdict: Submission
            try {
                verdict = JSON.parse(ev.data)
            } catch {
                fallbackFetch(id, onVerdict) // malformed payload → fetch the real value
                return
            }
            onVerdict(verdict)
            stop(id) // MUST close, or the browser reconnects and re-subscribes
        })

        source.addEventListener('error', () => {
            // EventSource would auto-reconnect; we don't want that. Close and fall back once.
            fallbackFetch(id, onVerdict)
        })
    }

    // Auto-clean when used inside a component; safe to skip in a bare unit test.
    if (getCurrentInstance()) {
        onBeforeUnmount(stopAll)
    }

    return { watch, stop, stopAll }
}
```

- [ ] **Step 6: Run test to verify it passes**

Run: `npm run test`
Expected: PASS — the 5 `useSubmissionStream` tests plus the earlier `isTerminalStatus` tests.

- [ ] **Step 7: Type-check**

Run: `npm run type-check`
Expected: no errors (test files are excluded from the app tsconfig).

- [ ] **Step 8: Commit**

```bash
git add src/constants/apiPath.ts src/services/submissionService.ts \
        src/composables/useSubmissionStream.ts \
        src/composables/__tests__/useSubmissionStream.spec.ts
git commit -m "feat: useSubmissionStream composable for live verdict via SSE"
```

---

## Task 4: Frontend — wire the live row into the UI + runbook (judge-portal, judge-server)

Pass the created submission up from Submit, insert an optimistic *Judging* row, start the stream, and update the row in place on verdict. Verified manually (per design: no component tests). Update the E2E runbook.

**Files:**
- Modify: `src/components/problem/detail/ProblemSubmit.vue`
- Modify: `src/pages/user/problem/ProblemDetailPage.vue`
- Modify: `src/components/problem/detail/ProblemSubmissions.vue`
- Modify: `/home/tuan/Project/my-oj/judge-server/docs/e2e-kafka-runbook.md`

Work from: `/home/tuan/Project/my-oj/judge-portal` (runbook step is in `judge-server`)

**Interfaces:**
- Consumes: `useSubmissionStream().watch`, `isTerminalStatus`, `Submission`, `submissionService.submit` (returns `AxiosResponse<ApiResponse<Submission>>`).
- Produces: `ProblemSubmissions` exposes `addPendingSubmission(sub: Submission): void`; `ProblemSubmit` emits `success` with a `Submission` payload.

- [ ] **Step 1: Emit the created submission from `ProblemSubmit.vue`**

Add the type import to the `<script setup>` imports:

```ts
import type { Submission, SubmissionRequest } from '@/types/submission'
```

Change the emit declaration:

```ts
const emit = defineEmits<{
    (e: 'success', submission: Submission): void
}>()
```

In `handleSubmit`, capture the response and emit it (replace the `await submissionService.submit(data)` / `emit('success')` lines):

```ts
        const res = await submissionService.submit(data)

        triggerToast('Submitted', 'success')
        sourceCode.value = ''

        emit('success', res.data.data)
```

- [ ] **Step 2: Hand the submission to the list in `ProblemDetailPage.vue`**

Add imports/refs in `<script setup>`:

```ts
import type { Submission } from '@/types/submission'
```

Add a template ref and rewrite the handler (replace `handleSubmissionSuccess`):

```ts
const submissionsRef = ref<InstanceType<typeof ProblemSubmissions> | null>(null)

const handleSubmissionSuccess = (submission: Submission) => {
    activeTab.value = 'submissions'
    submissionsRef.value?.addPendingSubmission(submission)
}
```

In the template, attach the ref to the submissions component:

```html
<ProblemSubmissions ref="submissionsRef" :problem-slug="slug" />
```

(The `@success="handleSubmissionSuccess"` binding on `<ProblemSubmit>` already exists and now receives the payload.)

- [ ] **Step 3: Live rows in `ProblemSubmissions.vue` — script**

Add imports:

```ts
import { Loader2 } from 'lucide-vue-next'
import { useSubmissionStream } from '@/composables/useSubmissionStream'
import type { Submission } from '@/types/submission'
import { SubmissionResult, getSubmissionStatus, isTerminalStatus } from '@/types/submission'
```

(Merge with the existing `import { SubmissionResult, getSubmissionStatus } from '@/types/submission'` — replace it with the line above so there is no duplicate import.)

Add state + logic near the other `const`s in `<script setup>`:

```ts
const { watch: watchVerdict } = useSubmissionStream()
const liveRows = ref<Submission[]>([])

// Optimistic + streamed rows shown above the fetched page; dedupe so a row that
// later appears in the fetched page is not rendered twice.
const displayRows = computed(() => {
    const liveIds = new Set(liveRows.value.map((r) => r.id))
    const fetched = (response.value?.data || []).filter((s) => !liveIds.has(s.id))
    return [...liveRows.value, ...fetched]
})

const addPendingSubmission = (sub: Submission) => {
    if (!liveRows.value.some((r) => r.id === sub.id)) {
        liveRows.value.unshift(sub) // status is PENDING(6) from the submit response
    }
    watchVerdict(sub.id, (verdict) => {
        const idx = liveRows.value.findIndex((r) => r.id === verdict.id)
        if (idx !== -1) {
            liveRows.value.splice(idx, 1, { ...liveRows.value[idx], ...verdict })
        }
    })
}

// Once a finished submission shows up in a fresh fetch, drop its live copy so it
// stops being pinned above the paginated list.
watch(response, (r) => {
    const fetchedIds = new Set((r?.data || []).map((s) => s.id))
    liveRows.value = liveRows.value.filter(
        (row) => !(fetchedIds.has(row.id) && isTerminalStatus(row.status)),
    )
})

defineExpose({ addPendingSubmission })
```

- [ ] **Step 4: Live rows in `ProblemSubmissions.vue` — template**

Change the submissions `v-for` to iterate `displayRows` (replace `v-for="sub in submissions"`):

```html
<TableRow v-else v-for="sub in displayRows" :key="sub.id">
```

Change the empty-state check to use `displayRows` (replace `v-else-if="submissions.length === 0"`):

```html
<TableRow v-else-if="displayRows.length === 0">
```

Add a spinner to the status badge for non-terminal rows (replace the `<Badge>` block):

```html
<Badge :class="getStatusClass(sub.status)">
    <Loader2 v-if="!isTerminalStatus(sub.status)" class="h-3 w-3 animate-spin mr-1 inline" />
    {{ getSubmissionStatus(sub.status) }}
</Badge>
```

- [ ] **Step 5: Type-check**

Run: `npm run type-check`
Expected: no errors.

- [ ] **Step 6: Manual verification (dev)**

Start the frontend and backend stack per the runbook, then:

1. Log in, open a problem, go to **Submit**, submit a correct solution.
2. Confirm the app switches to **Submissions** and a new row appears immediately with a spinner + *Pending/Judging*.
3. Within a few seconds the badge flips to the final verdict (AC/WA/…) with time/memory — **without a manual reload**.
4. Submit again and immediately watch the row: confirm the fast-judge case still resolves (the backend replay covers verdicts that land before the stream opens).
5. Open DevTools → Network → the `/stream` request closes right after the verdict (no reconnect loop).

Record the outcome (pass/fail + notes) in the report.

- [ ] **Step 7: Commit the frontend wiring**

```bash
git add src/components/problem/detail/ProblemSubmit.vue \
        src/pages/user/problem/ProblemDetailPage.vue \
        src/components/problem/detail/ProblemSubmissions.vue
git commit -m "feat: live-update submissions table from SSE verdict stream"
```

- [ ] **Step 8: Update and commit the E2E runbook (judge-server repo)**

Append a `## 12. UI verdict (SSE)` section to `/home/tuan/Project/my-oj/judge-server/docs/e2e-kafka-runbook.md` capturing the Step 6 procedure and the success criteria (row appears live; badge flips without reload; stream closes after verdict; fast-judge race handled). Then:

```bash
cd /home/tuan/Project/my-oj/judge-server
git add docs/e2e-kafka-runbook.md
git commit -m "docs: add UI SSE verdict verification to E2E runbook"
```

---

## Self-Review (completed by plan author)

**Spec coverage:** §4 flow → Tasks 3–4; §5 backend replay → Task 1; §6 frontend files → Tasks 2–4 (apiPath/service/composable/components); §7 edge cases → composable (Task 3: verdict-close, error fallback, timeout, unmount, dedupe) + backend replay (Task 1); §8 testing → Task 1 (BE) + Tasks 2–3 (FE Vitest) + Task 4 (manual + runbook); §9 rollout → Task 4 Step 8. No gaps.

**Placeholder scan:** no TBD/TODO; every code step shows full code; commands have expected output.

**Type consistency:** `streamVerdict(String): SseEmitter` (Task 1) matches the controller call and test. `useSubmissionStream()` → `{ watch, stop, stopAll }` (Task 3) matches Task 4 usage (`watch`). `addPendingSubmission(sub: Submission)` exposed (Task 4 script) matches the parent call. `isTerminalStatus(number): boolean` defined in Task 2, consumed in Tasks 3–4. `getSubmissionById(id): Promise<AxiosResponse<ApiResponse<Submission>>>` defined in Task 3 Step 2, consumed in the composable. Frontend enum values match the backend `SubmissionResult` (Global Constraints).
