# Admin Roles & Permissions — Design

**Date:** 2026-07-18
**Target repos:** `judge-api` (backend), `judge-portal` (frontend)
**Builds on:** `2026-07-16-admin-ui-design.md` (Phase 3: Roles ↔ Permissions), `2026-07-16-permission-based-authz-design.md`

## Goal

Let an admin **view all roles + the permission catalog**, and **manage which permissions each role holds**. Every capability is gated by a specific permission (never a role name), enforced on both router and backend.

## Scope

**In:**
- Roles admin page: list roles (name, description, permissions), and assign/unassign permissions to a role.
- Permissions admin page: read-only catalog of all permissions (name, description), grouped by resource.
- Backend: `PUT /roles/{id}/permissions` (replace-all), new `role:update` permission seeded to ADMIN.

**Out (unchanged from prior specs):**
- Creating/deleting roles or permissions themselves — the catalog is fixed/seeded.
- User↔role management (the Users page stays a stub — separate future work).

## Chosen approach (approved)

**Approach A** — permission assignment via a modal dialog listing the full catalog grouped by resource, checkboxes + per-group select-all; Save replaces the role's whole permission set. Replace-all `PUT` semantics (idempotent). The `ADMIN` role is immutable (locked, no edit) to prevent self-lockout.

## Backend (judge-api)

### Flyway `V7__seed_role_update_permission.sql`
- Insert permission `role:update` (`description`: "Update a role's permissions") into `t_permissions` — `ON CONFLICT DO NOTHING`.
- Grant it to the `ADMIN` role in `t_roles_permissions` (`SELECT` the ADMIN role id + the new permission id) — `ON CONFLICT DO NOTHING`. Idempotent.

### `PUT /api/v1/roles/{id}/permissions`
- `@PreAuthorize("hasAuthority('role:update')")`.
- Request DTO `UpdateRolePermissionsRequest { Set<String> permissions }` — permission **names**.
- `RoleService.updatePermissions(UUID roleId, Set<String> permissionNames)`:
  - Role must exist → else `ROLE_NOT_EXISTED` (404).
  - **ADMIN guard:** if `role.name == "ADMIN"` → `ROLE_IMMUTABLE` (403/409). ADMIN always holds every permission (managed by migrations).
  - Every requested name must exist in the catalog → else `PERMISSION_NOT_EXISTED` (400), naming the unknown one(s).
  - Replace the role's `permissions` with the resolved `Permission` entities; save.
  - Return the updated `RoleResponse`.
- New `ErrorCode`s: `ROLE_NOT_EXISTED`, `ROLE_IMMUTABLE`, `PERMISSION_NOT_EXISTED` (reuse existing if present).

### Existing (unchanged)
- `GET /roles` (`role:read`) → `RoleResponse[] { id, name, description, permissions: Set<String> }`.
- `GET /permissions` (`permission:read`) → `PermissionResponse[] { id, name, description }`.

### Tests
- `RoleServiceUpdatePermissionsTest` (Mockito): valid replace persists resolved set; unknown role → ROLE_NOT_EXISTED; ADMIN role → ROLE_IMMUTABLE, never saves; unknown permission name → PERMISSION_NOT_EXISTED, never saves; empty set clears permissions on a non-ADMIN role.
- Keep existing controller security tests green; add a case that `role:update` is required for the PUT.

## Frontend (judge-portal)

### Data layer
- `types/role.ts` `Role { id: string; name: string; description?: string; permissions: string[] }`.
- `types/permission.ts` `Permission { id: string; name: string; description?: string }`.
- `services/roleService.ts` `getRoles()`, `updatePermissions(id, permissions: string[])`.
- `services/permissionService.ts` `getPermissions()`.
- `constants/apiPath.ts` add `ROLES: { ROOT: '/roles', PERMISSIONS: (id) => '/roles/${id}/permissions' }`, `PERMISSIONS: { ROOT: '/permissions' }`.
- `lib/permissionDisplay.ts` `groupByResource(perms)` (split on `:` → `problem`, `judgeserver`, `role`, `permission`, …), `resourceLabel(prefix)` + vitest spec.

### Pages
- `pages/admin/AdminRolesPage.vue` (replace stub): fetch roles via `useFetch`. Table: role name, description, permission badges (count + chips). Per row an **Edit permissions** button (`v-if hasPermission('role:update')`), hidden/disabled for the `ADMIN` role. Dialog (shadcn `Dialog`): catalog grouped by resource, checkbox per permission + a group-level select-all, prefilled from the role's current set; Save → `roleService.updatePermissions` → refetch + toast; loading/error/empty states.
- `pages/admin/AdminPermissionsPage.vue` (new): read-only catalog via `useFetch`, table grouped by resource (name mono, description).

### Wiring
- `constants/routePath.ts` add `ADMIN_PERMISSIONS = '/admin/permissions'` (`ADMIN_ROLES` already exists).
- `router/index.ts` add `{ path: 'permissions', name: 'AdminPermissions', component: AdminPermissionsPage, meta: { permissions: ['permission:read'] } }`.
- `pages/admin/AdminLayout.vue` Access group: add `{ label: 'Permissions', to: ROUTE_PATH.ADMIN_PERMISSIONS, icon: KeyRound, show: can('permission:read') }`; add its breadcrumb.
- Reuse the existing shadcn `Dialog`, `Table`, `Checkbox`, `Badge`, `Button` components (add via shadcn-vue if any is missing).

### Tests
- `permissionDisplay.spec.ts` (grouping/labels).
- Type-check clean; existing vitest green.

## Data flow

`AdminRolesPage` → `GET /roles` + `GET /permissions` on mount → render table. Edit → dialog prefilled from `role.permissions` → user toggles → Save → `PUT /roles/{id}/permissions {permissions}` → on success refetch roles + toast. `AdminPermissionsPage` → `GET /permissions` → grouped read-only table.

## Error handling
- Backend returns `AppException` → `GlobalExceptionHandler` → HTTP status + message; the dialog surfaces `resp.data.message`.
- Router `beforeEach` already gates `/admin/roles` (`role:read`) and `/admin/permissions` (`permission:read`); the Edit action is additionally gated by `role:update`.

## Deploy & verify
Rebuild `judge-api` (V7 applies `role:update` to `oj-db`) + `judge-portal`. Verify: `PUT /roles/{USER id}/permissions` toggles a permission; ADMIN role edit blocked; `/admin/roles` + `/admin/permissions` render live.
