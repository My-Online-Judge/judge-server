# Admin Users CRUD + SYS_ROOT — Design Spec

- **Date:** 2026-07-19
- **Status:** Approved (design). Ready for implementation plan.
- **Repos:** `judge-api` (Spring Boot 3), `judge-portal` (Vue 3 SPA)
- **Depends on:** the permission-based authz + admin-roles-crud work already merged.
- **Follow-up:** Spec B — Auth telemetry (`ip`/`device_hash` on `t_users`/`t_tokens`) as groundwork for rate-limiting. Out of scope here.

## 1. Motivation

`/admin/users` is a stub. Admins can manage roles and permissions but cannot manage the
users those roles are assigned to. This closes the loop: an admin can see every account,
create accounts, edit them, reset passwords, enable/disable, soft-delete, and assign/remove
roles per user. Assigning roles per user is also what lets an admin free a role that is still
in use before deleting it (the `ROLE_IN_USE` 409 from role-delete).

The route `/admin/users` already exists and is guarded by `meta.permissions: ['user:read']`,
but `user:read` is not seeded anywhere, so the page is currently unreachable by anyone. The
V9 migration below seeds it and unblocks the route.

## 2. Scope

**In scope (Spec A):**

- List users — server-side pagination + filters (search, status, role, created-date range).
- View user detail.
- Create user — local account (username + name + email + password + status + roles).
- Edit user — name / email / status (username is immutable).
- Reset password — admin sets a new password for a user.
- Disable / enable — toggle status.
- Soft-delete — mark `DELETED`; the record and its submissions are retained.
- Assign/remove roles — a "Manage roles" dialog writes the full role set at once.
- `SYS_ROOT` protected super-user role + authz guardrails.

**Out of scope:**

- Telemetry columns (`ip`, `device_hash`, …) and rate-limiting → **Spec B**, built right after.
- Hard-delete and cascade of a user's submissions.
- Self-service password change for the root account (a separate, non-admin flow).
- MFA administration.

## 3. Decisions log

Captured from the brainstorming session (2026-07-19):

| Decision | Choice |
|---|---|
| Feature scope | Full CRUD (create / edit / delete / disable / roles / reset-password). |
| Role assignment UX | Checkbox dialog; `PUT /users/{id}/roles` replaces the whole set (atomic). |
| Self-lockout guard | An admin cannot change **their own** roles / status, nor delete themselves. |
| Delete semantics | **Soft-delete** (status → `DELETED`). Root cannot disable/delete itself. |
| Create | Local account with a BCrypt-hashed password. |
| Edit | name / email / status only; username immutable; password via separate reset. |
| List filters | search (username/email/name), status, role, **created-date range**. |
| SYS_ROOT model | **Immutable super-user**: has every permission; any account holding it cannot be modified via the API (by anyone, including itself). ADMIN keeps managing roles/users as today. |
| Telemetry | Separate Spec B, immediately after A. |

## 4. Data model & migration `V9`

New Flyway file `V9__seed_user_permissions_and_sys_root.sql`, mirroring the V6/V8 style
(additive + idempotent, `ON CONFLICT DO NOTHING`, fixed UUIDs, `'SYS'` audit actor).

### 4.1 Permissions (fixed UUIDs continue the `a1000000-…` family)

| Name | UUID | Meaning |
|---|---|---|
| `user:read` | `a1000000-0000-0000-0000-00000000000a` | list + detail |
| `user:create` | `a1000000-0000-0000-0000-00000000000b` | create account |
| `user:update` | `a1000000-0000-0000-0000-00000000000c` | edit profile, change status, reset password, manage roles |
| `user:delete` | `a1000000-0000-0000-0000-00000000000d` | soft-delete |

`user:update` deliberately covers every *modify* action on a user (no separate
`user:reset-password` permission) to keep the catalog small; ADMIN gets all four.

### 4.2 `SYS_ROOT` role

- Fixed role UUID: `a2000000-0000-0000-0000-000000000001`.
- Granted **every** permission via `INSERT … SELECT p.id FROM t_permissions p` (same trick
  V6 used for ADMIN) — so it holds all present permissions including the four new `user:*`.
- Assigned to the seeded root user `d0000000-0000-0000-0000-000000000001` (keeps its existing
  ADMIN grant too; harmless and avoids a destructive delete in the migration).

**Convention going forward:** every future permission-seed migration must grant the new
permission to **both** ADMIN and SYS_ROOT (SYS_ROOT is not auto-updated by later migrations).

### 4.3 `UserStatus`

Introduce a user-specific enum (the shared `CommonStatus{ACTIVE(1), INACTIVE(0)}` stays as is
for other entities):

```
UserStatus: ACTIVE(1), DISABLED(0), DELETED(2)
```

- Existing rows (root admin has `status = 1`) map to `ACTIVE` — backward compatible.
- **Disable** → `DISABLED`. **Soft-delete** → `DELETED`.
- **Login must reject any non-ACTIVE user** — the password auth path currently does not check
  status; add the check in `AuthService` (both password and Google paths).
- The default list hides `DELETED`; pass `status=DELETED` explicitly to view them.

No schema change to `t_users` — `status` stays `Integer`; only the set of meaningful values grows.

## 5. Backend API

Add `Routes.USERS = BASE_API + "/users"`. New `UserController` mirrors `RoleController`
conventions (`@PreAuthorize("hasAuthority('…')")`, `ApiResponse.success/created/success()`,
`@Valid @RequestBody` kept for convention even though no Bean Validation provider is on the
classpath — services validate explicitly).

| Method | Path | Authority | Body / Params | Returns |
|---|---|---|---|---|
| GET | `/users` | `user:read` | `page,size,search,status,roleId,createdFrom,createdTo` | `PageResponse<UserResponse>` |
| GET | `/users/{id}` | `user:read` | — | `UserResponse` |
| POST | `/users` | `user:create` | `CreateUserRequest` | `created(UserResponse)` |
| PUT | `/users/{id}` | `user:update` | `UpdateUserRequest{name,email,status}` | `UserResponse` |
| PATCH | `/users/{id}/status` | `user:update` | `UpdateUserStatusRequest{status}` | `UserResponse` |
| PUT | `/users/{id}/roles` | `user:update` | `UpdateUserRolesRequest{roleIds: UUID[]}` | `UserResponse` |
| POST | `/users/{id}/reset-password` | `user:update` | `ResetPasswordRequest{newPassword}` | `success()` |
| DELETE | `/users/{id}` | `user:delete` | — | `success()` (soft-delete) |

The Edit dialog uses `PUT /users/{id}`; the quick **Disable/Enable** toggle uses the lighter
`PATCH /users/{id}/status` so it never has to resend name/email. **Neither `PUT` nor `PATCH`
may set `DELETED`** — reaching the `DELETED` state is `DELETE`'s job (gated by `user:delete`);
both status-writing paths accept only `ACTIVE` / `DISABLED`.

**List query.** One JPQL `@Query` with optional params (the `findProblemsWithStats` pattern),
each guarded `:param IS NULL OR …`:

- `search` → `LOWER(username|email|name) LIKE %search%`
- `status` → `u.status = :status`; when `status` is null, exclude `DELETED`
- `roleId` → `EXISTS (SELECT 1 FROM u.roles r WHERE r.id = :roleId)` — **EXISTS, not JOIN
  FETCH**, so pagination stays correct against the EAGER `roles` collection
- `createdFrom`/`createdTo` → `LocalDate` params; `createdAt >= from.atStartOfDay()` and
  `createdAt < to.plusDays(1).atStartOfDay()` (inclusive day range)

Sort by `createdAt` descending (default). Wrap the `Page` with the existing `PageResponse<T>`.

**DTOs.**

- `CreateUserRequest{ username, name, email, password, status?, roleIds? }`
- `UpdateUserRequest{ name, email, status }`
- `UpdateUserStatusRequest{ status }`
- `UpdateUserRolesRequest{ roleIds: List<UUID> }`
- `ResetPasswordRequest{ newPassword }`
- `UserResponse{ id, username, name, email, status, enabledMfa, lastLogin, avatar, googleId,
  createdAt, roles: [{id, name}] }` — reuse/extend the existing user response DTO used by
  `/me`; the list variant needs `roles` but not the flattened `permissions[]`.

## 6. Authorization & guardrails (the core)

Enforced in `UserService` (not just annotations), throwing `AppException(ErrorCode.…)`:

1. **SYS_ROOT is immutable.** Before any *modify* op (update, roles, reset-password,
   disable via update, soft-delete), if the **target** user holds `SYS_ROOT` →
   `USER_PROTECTED` (409). Applies to everyone, including the root acting on itself.
2. **No privilege escalation.** `PUT /users/{id}/roles`: if the incoming set contains
   `SYS_ROOT` → `ROLE_NOT_ASSIGNABLE` (409). `SYS_ROOT` is seed-only. The Manage-roles dialog
   also omits it from the checkbox list. Add `SYS_ROOT` to `RoleService.SYSTEM_ROLES` so it
   cannot be deleted through the role CRUD.
3. **No self-lockout.** If the **target** is the current user, reject **manage-roles**,
   **disable** (status change on self), and **soft-delete** → `USER_SELF_MODIFY` (409).
   Editing one's own name/email is still allowed.
4. **Create validation** (service-side, since `@Valid` is a no-op):
   - `username` — required, unique, `^[a-zA-Z0-9_]{3,32}$` → `USERNAME_INVALID` / `USERNAME_EXISTED`
   - `email` — required, unique, basic email shape → `EMAIL_INVALID` / `EMAIL_EXISTED`
   - `password` — required, min length 8 (+ at least letters & digits) → `PASSWORD_INVALID`
   - `roleIds` — each must exist and must not be `SYS_ROOT`
   - password stored via the existing `BCryptPasswordEncoder` bean; new users default `ACTIVE`.
5. **Edit validation.** email uniqueness re-checked when changed; status must be a valid
   `UserStatus`.

**New `ErrorCode` entries:** `USER_NOT_EXISTED` (404), `USERNAME_EXISTED` (409),
`EMAIL_EXISTED` (409), `USER_PROTECTED` (409), `ROLE_NOT_ASSIGNABLE` (409),
`USER_SELF_MODIFY` (409), `USERNAME_INVALID` (400), `PASSWORD_INVALID` (400),
`EMAIL_INVALID` (400). (`USER_NOT_EXISTED` only if not already present.)

**Repository.** `UserRepository` gains the paginated finder (JPQL above),
`existsByUsername`, `existsByEmail`, and `existsByEmailAndIdNot` (edit). `countByRoles_Id`
already exists (used by role-delete). Assign/remove roles by mutating `user.getRoles()` and
saving — `User` owns the `t_users_roles` join table.

## 7. Frontend (`/admin/users`)

Everything mirrors `AdminRolesPage.vue`: `useFetch`, `authStore.hasPermission`, `useToast`
+ `getErrorMessage`, shadcn-vue dialogs.

- **`services/userService.ts`** (new): `list(params)`, `get(id)`, `create(payload)`,
  `update(id, payload)`, `updateStatus(id, status)`, `updateRoles(id, roleIds)`,
  `resetPassword(id, newPassword)`, `remove(id)`. Extend `apiPath.USERS` with `STATUS(id)`,
  `ROLES(id)`, `RESET_PASSWORD(id)`.
- **`AdminUsersPage.vue`** replaces the stub:
  - Toolbar: search input, status `Select`, role `Select`, created-date range (two date
    inputs), all driving the server query; `PaginationFooter` at the bottom.
  - Table columns: username · name · email · role badges · status badge · created date ·
    actions.
  - Row actions, each gated by permission: **Edit** (`user:update`), **Manage roles**
    (`user:update`), **Reset password** (`user:update`), **Disable/Enable** (`user:update`),
    **Delete** (`user:delete`). **Create user** button gated `user:create`.
  - Rows whose user holds `SYS_ROOT`: actions hidden/disabled (immutable).
  - The current user's own row: hide Manage-roles / Disable / Delete (self-lockout guard),
    surfaced from `authStore` user id.
  - Dialogs: Create, Edit, Manage-roles (checkbox list of assignable roles, SYS_ROOT omitted),
    Reset-password.
- **Types.** Fix `types/user.ts` `Role.id` to `string` (it is a UUID, currently typed
  `number`); add a `UserStatus` union/const and request payload types.
- **Validators.** `lib/userValidators.ts` (`isValidUsername`, `isValidPassword`) in the style
  of `lib/roleName.ts`, with matching front-end Save-disabled behaviour.

## 8. Testing

- **Backend** (Mockito, like `RoleServiceCreateTest`): create happy-path + each validation +
  duplicate username/email; soft-delete sets `DELETED`; SYS_ROOT-protected on every modify op;
  self-modify guard; SYS_ROOT-not-assignable on `updateRoles`; reset-password stores a hash;
  `updateRoles` replaces the whole set; list filters (status/role/date/search) build the right
  query. Aim to keep the full user suite green alongside the existing role suite.
- **Frontend** (vitest): `userService` request shapes; `userValidators`.

## 9. Migration & rollout notes

- After V9, ADMIN gains `user:*` and the `/admin/users` route becomes reachable.
- Deploy is the usual `docker compose up -d --build judge-api judge-portal` against the
  working tree; Flyway applies V9 on boot (`ddl-auto: validate`, baseline-on-migrate).
- The seeded root account (`admin` / `Admin@12345`) becomes immutable via the admin UI once it
  holds SYS_ROOT — its password must still be changed through the normal self-service flow, not
  this panel.
