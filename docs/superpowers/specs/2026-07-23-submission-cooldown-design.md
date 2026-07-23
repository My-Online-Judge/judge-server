# Spec D — Submission Cooldown (design)

Date: 2026-07-23
Target repos: judge-api, judge-portal
Depends on: Spec C (Redis limiter idiom, metrics, fail-open convention — merged to main)

## Goal

Stop submission spam: every user must wait **10 seconds** between two submissions
(any problem, any language). Protects the judge queue and the workers without
punishing normal use.

## Decisions (user-approved)

1. **Policy**: fixed cooldown between submissions, per user. Chosen over
   window quotas and pending-count limits for predictable UX.
2. **Duration**: **10 seconds** (user corrected down from the proposed 30).
   Configurable via `oj.submission.cooldown-seconds` in `application.yml`
   (default 10) so ops can tune it without a code change.
3. **Exemptions**: none — admins and SYS_ROOT wait like everyone else.
4. **Frontend UX**: proactive countdown — the submit button disables itself for
   10s after a successful submit; a 429 (second tab, refresh) seeds the counter
   from the response's `Retry-After` header. Normal users never see the error.

## Backend (judge-api) — no migration needed

No new tables, no new permissions.

- **`SubmissionRateLimiter`** (new, `vn.thanhtuanle.security`, same idiom as
  `LoginRateLimiter`): atomic Redis gate
  `SET oj:rl:submit:{userId} NX EX <cooldown>`.
  - Set succeeded → submission allowed (cooldown starts now).
  - Key already present → read remaining TTL and throw
    `RateLimitedException(SUBMISSION_RATE_LIMITED, remainingSeconds)`.
  - Redis down → **fail open** (log + allow), consistent with Spec C.
  - Increments counter `oj.submission.rate_limited` when it blocks.
- **`RateLimitedException extends AppException`** (new): carries
  `retryAfterSeconds`. `GlobalExceptionHandler` gets a dedicated
  `@ExceptionHandler(RateLimitedException.class)` that sets a **dynamic**
  `Retry-After` header (the real remaining seconds). The existing login
  `RATE_LIMITED` path (hardcoded 900) is left untouched.
- **New ErrorCode**: `SUBMISSION_RATE_LIMITED("You are submitting too fast.
  Try again in a moment", 429 TOO_MANY_REQUESTS)`.
- **Insertion point**: in `SubmissionService.submit()` AFTER problem, language,
  and current user resolve, immediately BEFORE `createPendingSubmission`.
  A 404 (bad slug/language) therefore does not burn the cooldown; only real
  submissions arm it. The atomic NX still kills double-click races.

## Frontend (judge-portal)

- **`useCooldown()`** composable (new, `src/composables/useCooldown.ts`):
  `start(seconds)`, `remaining` (ref, seconds left), `active` (computed).
  1-second interval; testable in the node-env vitest suite with fake timers.
- **Submit button** on the problem detail page: on successful submit call
  `start(10)`; while `active`, the button is disabled and shows
  `Submit (Ns)`. On a 429 from the submit call, read
  `error.response.headers['retry-after']`, `start(that)`, and toast the
  cooldown message.

## Error contract

`POST /api/v1/submissions` when throttled:
- Status `429`, header `Retry-After: <remaining seconds, 1..cooldown>`
- Body: standard `ApiResponse.error` with the SUBMISSION_RATE_LIMITED message.

## Testing

- Backend unit: limiter allows when NX succeeds; blocks with correct remaining
  TTL when key exists; fail-open on Redis exceptions; metric incremented on
  block. Handler test: dynamic Retry-After equals the exception's seconds.
  SubmissionService test: limiter consulted with the current user's id before
  the submission is persisted; not consulted when the problem doesn't resolve.
- Frontend unit: `useCooldown` counts down and deactivates with fake timers;
  service/param specs as applicable.
- Live verification: two immediate submissions → second gets 429 with
  `Retry-After ≤ 10`; after waiting it succeeds; the UI button counts down
  after a successful submit.

## Out of scope

- Window quotas / pending-submission caps (revisit if worker queue pressure
  becomes real).
- Ban integration (Spec C's AccessBanFilter already covers submissions).
- Per-problem or per-contest cooldown tuning.
