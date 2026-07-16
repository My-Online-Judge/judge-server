# Design Spec: Admin UI (full)

**Date:** 2026-07-16
**Status:** Approved
**Target repos:** `judge-portal` (Vue 3 frontend), `judge-api` (Spring Boot backend)
**Builds on:** `2026-07-16-permission-based-authz-design.md`, `2026-07-16-admin-ui-permission-groundwork-design.md`

## Problem & Goal

judge-portal is contestant-only: no admin section, no route guards, no admin data reads/writes
beyond what a user needs. With permission-based authz + the `/me` permissions contract + read
endpoints now in place, build a dedicated **`/admin` section** that lets an administrator manage
problems, view judge servers, manage role↔permission mappings, and manage user↔role
assignments — every capability gated by a specific **permission** (never a role), enforced on
both the router (redirect) and the backend (403).

## Scope

**In scope (delivered in phases, each independently shippable):**
- FE: `/admin` section with `AdminLayout` (sidebar), a permission-aware `router.beforeEach`
  guard, a `/403` page, and an "Admin" header entry shown only when the user has an admin
  permission.
- FE pages: Problems (list + create[multipart] + edit + delete), Judge servers (list + health),
  Roles (view + assign permissions), Users (list + assign roles).
- BE: new write/read endpoints that don't exist yet — `PUT /roles/{id}/permissions`,
  `GET /users`, `PUT /users/{id}/roles` — plus a `UserController`.
- BE: three new permissions `role:update`, `user:read`, `user:update` seeded to ADMIN (Flyway V5).
- Self-lockout guard: an admin cannot remove the `ADMIN` role from their own account.

**Out of scope:** creating/deleting roles or permissions themselves (catalog is fixed/seeded);
editing user profile fields; test-case re-upload on problem edit (metadata only, matching the
existing `UpdateProblemDto`); i18n; bulk operations; audit log UI.

## Current State (verified)

- **FE stack:** Vue 3 `<script setup>`, Pinia setup-stores, vue-router 4, Tailwind v4 +
  **shadcn-vue** (reka-ui) primitives in `src/components/ui/*` (button, input, label, textarea,
  select, switch, dialog, table, card, badge, pagination, toast, …), axios client
  (`src/api/axiosClient.ts`, `withCredentials`, Bearer-from-cookie), `ApiResponse<T>` envelope
  (`src/types/common.ts`), `useFetch` composable (service fn's first arg is `AbortSignal`),
  `useToast().triggerToast(msg, type)`. Services are singleton classes in `src/services/`.
- **Router:** `src/router/index.ts` has 3 flat routes, **no layouts, no `beforeEach`, eager
  imports**. Paths in `src/constants/routePath.ts`; API paths in `src/constants/apiPath.ts`.
- **App shell:** hardcoded in `App.vue` (Header + `<RouterView>` + footer + `<Toaster/>`). No
  layout system. Sidebar design tokens (`bg-sidebar`, …) already exist in `src/assets/main.css`.
- **Auth bootstrap (critical):** `authStore.fetchUser()` is called **un-awaited** in
  `Header.vue onMounted` — so a guard cannot assume permissions are loaded. `authStore` already
  exposes `user`, `isAuthenticated`, `login`, `logout`, `fetchUser`, `hasPermission(perm)`;
  `UserResponse` has `permissions: string[]` + `roles: Role[]`.
- **Tests:** Vitest `environment: 'node'`, includes only `src/**/__tests__/**/*.spec.ts`. **No
  component testing** (`@vue/test-utils`/jsdom absent) — only stores/composables/pure functions
  are unit-testable. Confirm-dialog utility: none (build from shadcn `Dialog`). File upload:
  no existing pattern.
- **BE:** permission model live; `GET /problems` (public, paginated), `POST/PUT/DELETE /problems`
  (gated `problem:create/update/delete`), `GET /judge-servers` (gated `judgeserver:read`),
  `GET /permissions` (`permission:read`), `GET /roles` (`role:read`) exist. **No** `UserController`,
  no user list, no role/user assignment writes. `UserRepository` exists (`user/`).
  `CreateProblemDto` = title, subject, description, timeLimit, memoryLimit, hardnessLevel,
  problemSlug, inputDescription, outputDescription, sampleInput, sampleOutput, hint, status;
  create is multipart (`data` JSON part + test-case `file` part). `UpdateProblemDto` = same minus
  slug. Migrations V1-V4 released → new seed in **V5**.

## Design

### A. Frontend foundation (Phase 0)

- **Routes** (`routePath.ts`): `ADMIN='/admin'`, `ADMIN_PROBLEMS='/admin/problems'`,
  `ADMIN_JUDGE_SERVERS='/admin/judge-servers'`, `ADMIN_ROLES='/admin/roles'`,
  `ADMIN_USERS='/admin/users'`, `FORBIDDEN='/403'`.
- **Nested route:** `{ path:'/admin', component: AdminLayout, children:[ …pages with
  meta.permissions:string[] ] }` + top-level `/403 → ForbiddenPage`. `AdminLayout` = full-height
  shell with a sidebar (`bg-sidebar`) whose links `v-if="hasPermission(...)"`, and its own
  `<RouterView/>`.
- **Guard readiness:** add `authStore.ensureLoaded(): Promise<void>` that memoizes a single
  `fetchUser()` call (so repeated guard runs await the same promise). Also call awaited
  `ensureLoaded()` in `main.ts` before mount is optional; the guard is the source of truth.
- **Guard logic (pure + testable):** factor the decision into
  `resolveAdminAccess(meta, { isAuthenticated, hasPermission }) → 'allow' | 'login' | 'forbidden'`
  in `src/router/adminGuard.ts`. `router.beforeEach` calls `await authStore.ensureLoaded()`
  when `user` is null, then applies `resolveAdminAccess`: no `meta.permissions` → allow;
  unauthenticated → `login` (redirect home, open login); has none of `meta.permissions` →
  `forbidden` (`/403`); else allow. Unit-test `resolveAdminAccess`.
- **Header:** add an "Admin" link `v-if` the user has any admin permission
  (`['problem:create','problem:update','problem:delete','judgeserver:read','role:read','user:read']
  .some(p => authStore.hasPermission(p))`).

### B. Problems management (Phase 1) — backend already exists

- Extend `problemService.ts`: `create(body: FormData)`, `update(slug, dto)`, `remove(slug)`
  (mutations, no signal; `create` sets `Content-Type: multipart/form-data`, appends `data` as a
  JSON `Blob` + the test-case `file`). Add `PROBLEMS` write paths to `apiPath.ts` (reuse ROOT +
  DETAIL(slug)).
- `AdminProblemsPage`: list via `useFetch(problemService.getProblems)` (reuse the public list);
  a "Create" button (`v-if hasPermission('problem:create')`) → `ProblemFormDialog` (create mode,
  multipart); per-row "Edit" (`problem:update`) → dialog (metadata) and "Delete"
  (`problem:delete`) → confirm `Dialog`. `triggerToast` on success/error; refetch after mutations.

### C. Judge servers (Phase 2) — backend already exists

- New `judgeServerService.ts` `getJudgeServers(signal)` → `GET /judge-servers`; `apiPath.ts`
  `JUDGE_SERVERS.ROOT='/judge-servers'`; `types/judgeServer.ts` mirroring `JudgeServerResponseDto`
  (hostname, ip, cpuCore, cpuUsage, memoryUsage, serviceUrl, lastHeartbeat, taskNumber, disabled,
  alive). `AdminJudgeServersPage`: `useFetch` list, a table with an alive/health `Badge`.

### D. Roles ↔ Permissions (Phase 3) — new backend write

- **BE:** `PUT /api/v1/roles/{id}/permissions`, body `{ permissions: Set<String> }` (permission
  **names**, matching `RoleResponse.permissions`); replaces the role's permission set (resolve
  names → `Permission` entities via `PermissionRepository`; unknown name → 400
  `AppException`). Gate `@PreAuthorize("hasAuthority('role:update')")`. Returns updated
  `RoleResponse`. Implement in `RoleService.updatePermissions(id, names)` + `RoleRepository`
  already has `findById`.
- **FE:** `roleService.ts` `getRoles(signal)` + `updatePermissions(id, names)`;
  `permissionService.ts` `getPermissions(signal)`; `apiPath.ts` `ROLES`/`PERMISSIONS` groups.
  `AdminRolesPage`: list roles (`role:read`); per role an "Edit permissions" dialog
  (`role:update`) with a checkbox per catalog permission, pre-checked from the role; save → PUT →
  toast + refetch.

### E. Users ↔ Roles (Phase 4) — new backend

- **BE:** new `UserController` — `GET /api/v1/users` (paginated, gate `user:read`, returns
  `PageResponse<UserResponse>` via the existing paging pattern + `UserMapper`) and
  `PUT /api/v1/users/{id}/roles`, body `{ roles: Set<String> }` (role names) replacing the user's
  role set (resolve via `RoleRepository`; unknown → 400). Gate `user:update`. **Self-lockout
  guard:** if `id` == current user and the current user currently has `ADMIN` but the new set
  omits it → 400 `AppException` ("cannot remove your own admin role"). Returns updated
  `UserResponse`.
- **FE:** `userService.ts` `getUsers(signal, params)` + `updateRoles(id, names)`; `apiPath.ts`
  `USERS` (ROOT exists) + `ROLES(id)`. `AdminUsersPage`: paginated table (`user:read`) showing
  username/email/roles; per user an "Edit roles" dialog (`user:update`) with a checkbox per role
  (from `GET /roles`), pre-checked; save → PUT → toast + refetch. Disable removing ADMIN from
  self in the UI too (defense-in-depth with the backend guard).

### F. New permissions + seed (Flyway V5)

- Catalog additions: `role:update` (`…007`), `user:read` (`…008`), `user:update` (`…009`) — UUIDs
  `a1000000-0000-0000-0000-00000000000{7,8,9}`. `V5__seed_admin_permissions.sql` (idempotent)
  inserts them + maps to ADMIN (`626c2558-…`). Append the same to `initdb.sql`.

### G. Testing strategy

- **Backend (TDD):** security slices (`@WebMvcTest` + `@WithMockUser`, the established pattern) for
  the 3 new endpoints — allowed with the right permission, 403 without. Service unit tests
  (Mockito) for `RoleService.updatePermissions` (replace + unknown-name→exception),
  `UserService.updateRoles` (replace + **self-lockout** exception + unknown-name). Migration V5
  verified on a disposable Postgres (idempotent; ADMIN → 9 permissions).
- **Frontend (node-env unit tests):** `resolveAdminAccess` guard function (allow/login/forbidden
  matrix); `authStore.ensureLoaded` memoization (calls `fetchUser` once); optionally service URL
  construction. `.vue` pages/dialogs are **not** unit-testable in the current setup
  (`environment: 'node'`, no `@vue/test-utils`) → each phase carries a **manual verification
  checklist** (run app, log in as ADMIN, exercise the flow). Adding `@vue/test-utils` + jsdom is
  possible but out of scope.

### H. Phased delivery (each phase = one shippable increment + review)

- **Phase 0:** FE foundation — routePath, AdminLayout, guard (`ensureLoaded` +
  `resolveAdminAccess` + `beforeEach`), `/403`, Header "Admin" link. Ships an empty gated section.
- **Phase 1:** Problems management (FE only; backend exists).
- **Phase 2:** Judge servers (FE only; backend exists).
- **Phase 3:** Roles↔Permissions (BE `PUT /roles/{id}/permissions` + `role:update` [part of V5] +
  FE page).
- **Phase 4:** Users↔Roles (BE `UserController` + `user:read/update` [V5] + FE page) + self-lockout.
- V5 migration lands with Phase 3 (first phase needing new permissions), seeding all three at once.

## Success Criteria

- A user with admin permissions sees the "Admin" link and can reach `/admin/*`; a user without a
  given permission is redirected to `/403` for that route and never sees its nav link; direct URL
  entry is blocked by the guard; backend returns 403 independently.
- Problems can be created (multipart), edited, and deleted from `/admin/problems`.
- Judge servers list with health shows at `/admin/judge-servers`.
- A role's permissions can be edited at `/admin/roles`; ADMIN gains `role:update` etc. via V5.
- Users can be listed and have roles reassigned at `/admin/users`; an admin cannot strip their own
  ADMIN role (backend 400 + UI prevents it).
- New + existing backend tests pass (except the known offline `contextLoads`); FE unit tests +
  `type-check` pass.
