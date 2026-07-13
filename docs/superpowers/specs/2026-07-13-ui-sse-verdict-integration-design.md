# Design — UI integration with SSE verdict reception

**Date:** 2026-07-13
**Repos touched:** `judge-portal` (primary), `judge-api` (small)
**Depends on:** the Kafka async judge migration (`2026-07-12-judge-kafka-microservice-design.md`), which made `POST /submissions` return `PENDING` immediately and added `GET /submissions/{id}/stream` (SSE, event name `verdict`, payload = `SubmissionResponseDto`).

## 1. Problem

After the Kafka migration, `POST /submissions` no longer blocks on judging — it returns `status = 6` (PENDING) and the verdict arrives asynchronously (Kafka → `JudgeResultConsumer` → DB + SSE push). The frontend was written for the old synchronous flow:

- `ProblemSubmit.vue` submits, shows a toast, and emits `success`.
- `ProblemDetailPage.vue` reacts by switching to the Submissions tab and **refetching the problem** (not the submissions list).
- `ProblemSubmissions.vue` fetches the submissions list once, with **no polling and no real-time update**.

Result today: a new submission either does not appear, or appears stuck on `Pending` until the user manually reloads. There is a working SSE endpoint on the backend that nothing consumes.

## 2. Goals / Non-goals

**Goals**
- After submit, the user sees the new submission immediately as `Judging`, and its status flips to the final verdict (AC / WA / TLE / …) in real time, without a manual reload.
- Use the existing browser-native SSE endpoint; no polling as the primary mechanism.
- Be robust to the two real failure modes: the verdict arriving **before** the client subscribes (fast judging, 1–3 s), and the SSE connection failing.

**Non-goals**
- Multi-instance SSE fan-out (the registry is single-instance; unchanged — noted in the existing code).
- Resuming SSE for in-flight submissions across a full page reload (YAGNI for now).
- Live-updating submissions created in *other* browser tabs/sessions.
- Reworking auth, pagination, or the submissions table layout beyond what this feature needs.

## 3. Key technical facts (verified)

- **Auth works with native `EventSource`.** `JwtAuthenticationFilter` accepts the JWT from **either** the `Authorization: Bearer` header **or** the `accessToken` cookie. The frontend stores `accessToken` as a first-party, non-HttpOnly cookie (`document.cookie`, domain `localhost`, `SameSite=Lax`). `EventSource(url, { withCredentials: true })` cannot set headers but *does* send that cookie, and CORS already allows credentials for the frontend origins (`allowCredentials(true)`, origins `http://localhost:5173`, `http://localhost`). So no fetch-based SSE polyfill is needed.
- **`EventSource` auto-reconnects** when the stream closes. The client MUST call `.close()` after receiving the `verdict` event, otherwise the browser reopens the stream and re-subscribes indefinitely (and each reconnect hits an already-finished submission with no emitter, hanging until the 5-minute server timeout).
- **The verdict can land before subscribe.** `SubmissionSseRegistry.complete()` removes and pushes to whatever emitter is registered *at that moment*; if none is registered yet, the verdict is delivered to the DB but lost to SSE, and a later subscriber's emitter simply times out after 5 minutes.
- **The SSE payload is self-contained** (`SubmissionResponseDto` with status/result/cpuTime/memory/errorMessage/details). The frontend can update its row directly from the payload and never needs a follow-up GET on the happy path. (This also means the existing in-transaction `complete()` call is acceptable: the client trusts the pushed payload, not a re-read.)

## 4. Architecture & flow

```
User clicks Submit
  │  POST /submissions ──▶ 200 { id, status: 6 (PENDING) }     (returns immediately)
  │
  ├─ FE: insert optimistic row (Judging) at top of the list + switch to Submissions tab
  │
  └─ FE opens EventSource GET /submissions/{id}/stream   (withCredentials → accessToken cookie)
        ├─ event "verdict" { SubmissionResponseDto }  ──▶ update row from payload, es.close()
        ├─ (backend) if already terminal at subscribe time → push verdict immediately (kills the race)
        └─ FE safety timeout 20 s with no event ──▶ one GET /submissions/{id}; update if terminal, then give up
```

## 5. Backend changes — `judge-api` (small, with a test)

Eliminate the verdict-before-subscribe race at the source.

- **`SubmissionSseRegistry`**: keep `subscribe(id)` returning the emitter and registering it in the map **first**. Add nothing that reads the DB here (the registry stays persistence-free).
- **`SubmissionService.streamVerdict(id)`** (new): the controller delegates here so the controller stays thin.
  1. `emitter = sseRegistry.subscribe(id)` — registers the emitter **before** any DB read (ordering matters: if the verdict fires between registration and the read, the consumer still finds the emitter).
  2. Load the submission. If it does not exist, complete the emitter immediately (client falls back / closes).
  3. If `submission.status != PENDING` (already terminal), call `sseRegistry.complete(id, mapper.toDto(submission))` to push the verdict now.
  4. Return the emitter.
- **`SubmissionController.stream(id)`**: `return submissionService.streamVerdict(id);`

**Race correctness.** Because the emitter is registered before the DB read, at most one verdict is ever delivered: whichever of the consumer's `complete()` and the subscribe-time `complete()` runs first sends and removes the emitter; the other sees `ConcurrentHashMap.remove(...) == null` and no-ops. No lost verdict, no double send.

No change to Kafka topics, the consumer, or idempotency logic.

## 6. Frontend changes — `judge-portal`

- **`constants/apiPath.ts`**: add
  - `SUBMISSIONS.DETAIL: (id) => \`/submissions/${id}\``
  - `SUBMISSIONS.STREAM: (id) => \`/submissions/${id}/stream\``
- **`services/submissionService.ts`**: add `getSubmissionById(id)` (fallback GET). SSE lifecycle lives in the composable, not the service, because `EventSource` is not an axios call.
- **`composables/useSubmissionStream.ts`** (new, mirrors the `useFetch` composable pattern):
  - `watch(id, onVerdict)`: opens `new EventSource(\`${baseURL}/submissions/${id}/stream\`, { withCredentials: true })`, listens for the `verdict` event, parses JSON → `onVerdict(dto)` → `close()`.
  - On `error`: `close()`, then one fallback `getSubmissionById(id)`; if terminal → `onVerdict`, else leave as Judging.
  - Safety timeout (20 s) with no event → same fallback GET.
  - Tracks a `Map<id, EventSource>` so several rapid submits each get their own stream; `onBeforeUnmount` closes all streams and clears all timers.
  - Terminal check via a shared helper `isTerminalStatus(status)` (anything except PENDING=6 / JUDGING=7).
- **`ProblemSubmit.vue`**: `handleSubmit` captures the created submission from `res.data.data` and does `emit('success', submission)` (currently emits with no payload).
- **`ProblemDetailPage.vue`**: `handleSubmissionSuccess(sub)` switches to the Submissions tab and hands `sub` to `ProblemSubmissions` via a template ref call `addPendingSubmission(sub)` (drop the current `fetchProblem(slug)` refetch, which was the wrong target).
- **`ProblemSubmissions.vue`**:
  - Hold a reactive `liveRows: Map<id, Submission>` for optimistic + streamed rows, exposed via `defineExpose({ addPendingSubmission })`.
  - Displayed rows = `liveRows` (newest first) + fetched page rows whose id is not in `liveRows` (dedupe so the optimistic row is not duplicated when the server page later includes it).
  - `addPendingSubmission(sub)`: put a PENDING row in `liveRows`, then `useSubmissionStream.watch(sub.id, verdict => update that row in place)`.
  - Badge/`getSubmissionStatus`: render PENDING(6)/JUDGING(7) with a spinner (⟳) to signal "in progress".

## 7. Edge cases

| Case | Handling |
| --- | --- |
| Verdict lands before subscribe | Backend subscribe-time replay (§5) |
| SSE connection error / 401 | `onerror` → close → one fallback GET |
| No event within 20 s | Safety-timeout fallback GET |
| Component unmount / navigate away | `onBeforeUnmount` closes all `EventSource`, clears timers |
| Reconnect loop | Always `close()` on verdict and on error → browser never reopens |
| Rapid multiple submits | One `EventSource` per id in the `Map` |
| Optimistic row vs later server page | Dedupe displayed rows by id |
| Unknown/bad submission id | Backend completes emitter with no verdict; FE fallback GET 404s → row stays Judging (rare) |

## 8. Testing

- **Backend (`judge-api`, JUnit — infra exists):** unit-test the subscribe-time replay.
  - Given a **terminal** submission, `streamVerdict(id)` → the returned emitter receives the `verdict` event immediately.
  - Given a **PENDING** submission, `streamVerdict(id)` → no event yet; a subsequent `complete(id, dto)` delivers exactly one.
- **Frontend (`judge-portal`, add Vitest — no runner today):** unit-test `useSubmissionStream` against a **mock `EventSource`**. This is the riskiest logic.
  - Receives `verdict` → calls `onVerdict` once and `close()` once (no reconnect).
  - `error` → closes and performs exactly one fallback GET.
  - 20 s timeout with no event → one fallback GET (fake timers).
  - Unmount → closes the stream and clears the timer.
  - Visual wiring (optimistic row appears, badge flips, dedupe) is verified manually and appended to the E2E runbook.

## 9. Rollout / verification

- Extend `docs/e2e-kafka-runbook.md` with a UI step: submit from the portal, confirm the row appears as Judging and flips to the final verdict live; then submit and immediately watch to confirm the fast-judge race is handled.
- No schema change, no config change. Frontend and backend deploy independently; the backend replay change is backward-compatible (a client that never subscribes is unaffected).

## 10. Open risks

- **Single-instance SSE only** — unchanged and explicitly out of scope; documented in `SubmissionSseRegistry`.
- **In-transaction SSE push** — `complete()` runs inside the consumer's `@Transactional` before commit. Accepted because the client trusts the self-contained payload and does not re-read. Not changed here.
