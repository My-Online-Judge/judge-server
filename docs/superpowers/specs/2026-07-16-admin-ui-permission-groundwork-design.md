# Design Spec: Permission Groundwork for Admin UI

**Date:** 2026-07-16
**Status:** Approved
**Target repos:** `judge-api` (backend), `judge-portal` (frontend)
**Builds on:** `2026-07-16-permission-based-authz-design.md`

## Problem & Goal

The backend now authorizes by permission, but the client can't see permissions and there are no
endpoints to read the role/permission catalog. Before an admin UI can be planned, we need the
foundational **contract + read APIs + frontend seam**:

1. `GET /auth/me` must expose the current user's effective **permissions** (flat), so the UI can
   gate by `hasPermission('problem:create')` — never by role.
2. Read endpoints `GET /api/v1/permissions` and `GET /api/v1/roles` so an admin UI can list the
   catalog and each role's permissions.
3. Frontend plumbing: the `permissions` field on the user type + a `hasPermission()` store getter.

This is **groundwork only** — it unblocks planning the admin UI. It does NOT build the admin UI
pages, nor any write/CRUD endpoints (create permission, assign role↔permission, assign
user↔role): those belong to the admin-UI feature itself.

## Scope

**In scope:**
- Backend: `permissions` in `UserResponse` (`/auth/me`); `GET /permissions`; `GET /roles`;
  two new gate permissions `role:read`, `permission:read` seeded to ADMIN (Flyway V4 + initdb).
- Frontend: `UserResponse.permissions: string[]`; `authStore.hasPermission(perm)`.
- Tests: mapper flatten (unit), MockMvc security slices for the two endpoints, vitest for
  `hasPermission`.

**Out of scope (belongs to the admin-UI feature):**
- Any admin UI pages/routes/guards/navigation.
- Write APIs: create/edit/delete permissions or roles; assign role↔permission; assign
  user↔role.
- Pagination/filtering on the catalog endpoints (small fixed catalog — return all).

## Current State (verified)

- `GET /auth/me` → `AuthService.me()` → `UserMapper.toResponse(user)` → `UserResponse`.
- `UserResponse` (`user/dto/UserResponse.java`) has `Set<Role> roles` (the **entity** `Role`).
  Because `Role` now has an eager `permissions` collection (from the prior change), `/me` currently
  serializes `roles[].permissions[]` nested — leaking entity internals. We will suppress that.
- `UserMapper` is MapStruct (`@Mapper(componentModel = "spring")`), single method `toResponse`.
- `Routes` constants: `BASE_API = "/api/v1"`; has `PROBLEMS`, `JUDGE_SERVERS`, etc. No
  `PERMISSIONS` / `ROLES` yet.
- `RoleRepository` (`user/RoleRepository.java`) is `JpaRepository<Role, UUID>` — `findAll()` free.
  No `PermissionRepository` yet.
- `BaseResponse` = `{id, createdAt, updatedAt, createdBy, updatedBy}`.
- Method security is on (`@EnableMethodSecurity`); authorities are un-prefixed → use
  `hasAuthority(...)`. Endpoints not whitelisted require a JWT; `@PreAuthorize` adds the
  permission check. `t_permissions` / `t_roles_permissions` exist; ADMIN mapped to the 4 prior
  permissions. Prior migration `V3` is released (immutable) → new seed goes in `V4`.
- Frontend `stores/auth.ts` holds `user` + `isAuthenticated` (no permission helpers).
  `types/user.ts` `UserResponse` has `roles: Role[]`, no `permissions`. Vitest is configured.

## Design

### 1. `/auth/me` exposes permissions

- `UserResponse` gains `private Set<String> permissions;`.
- `UserMapper` flattens across the user's roles → distinct, sorted permission names:
  ```java
  @Mapping(target = "permissions", source = "roles", qualifiedByName = "rolesToPermissionNames")
  UserResponse toResponse(User user);

  @Named("rolesToPermissionNames")
  default Set<String> rolesToPermissionNames(Set<Role> roles) {
      if (roles == null) return Set.of();
      return roles.stream()
          .filter(r -> r.getPermissions() != null)
          .flatMap(r -> r.getPermissions().stream())
          .map(Permission::getName)
          .collect(Collectors.toCollection(TreeSet::new));
  }
  ```
- `Role.permissions` gets `@JsonIgnore` so `/me` `roles[]` stays `{id,name,description,audit}` as
  before (no nested permissions leak). Server-side authorization is unaffected (it uses
  `getAuthorities()`, not JSON). `RoleResponse` (below) is a separate DTO and still exposes
  permission names deliberately.

### 2. `GET /api/v1/permissions` — list the catalog

New feature package `permission/`:
- `PermissionRepository extends JpaRepository<Permission, UUID>`.
- `dto/PermissionResponse` = `{UUID id, String name, String description}`.
- `PermissionMapper` (MapStruct) `toResponse(Permission)`, `toResponses(List)`.
- `PermissionService#listPermissions()` → `List<PermissionResponse>` (all, sorted by name).
- `PermissionController` `@GetMapping(Routes.PERMISSIONS)`,
  `@PreAuthorize("hasAuthority('permission:read')")`, returns `ApiResponse<List<PermissionResponse>>`.
- `Routes.PERMISSIONS = BASE_API + "/permissions"`.

### 3. `GET /api/v1/roles` — list roles with their permissions

New feature package `role/`:
- `dto/RoleResponse` = `{UUID id, String name, String description, Set<String> permissions}`.
- `RoleMapper` (MapStruct) `toResponse(Role)` with a `permissionsToNames` flatten of the single
  role's `permissions` → sorted names; `toResponses(List)`.
- `RoleService#listRoles()` → `List<RoleResponse>` (reuses `user.RoleRepository.findAll()`,
  sorted by name).
- `RoleController` `@GetMapping(Routes.ROLES)`,
  `@PreAuthorize("hasAuthority('role:read')")`, returns `ApiResponse<List<RoleResponse>>`.
- `Routes.ROLES = BASE_API + "/roles"`.

### 4. New gate permissions + seed (Flyway V4)

- Catalog additions: `role:read`, `permission:read`.
- Fixed UUIDs: `role:read` → `a1000000-0000-0000-0000-000000000005`,
  `permission:read` → `a1000000-0000-0000-0000-000000000006`.
- `V4__seed_read_permissions.sql` (idempotent): insert the 2 permissions, map both to ADMIN
  (`626c2558-a7d5-4fa4-9161-9bec2ae74076`). Append the same to `initdb.sql`.

### 5. Frontend seam

- `types/user.ts`: add `permissions: string[]` to `UserResponse`.
- `stores/auth.ts`: add getter `hasPermission(perm: string): boolean` reading
  `user.permissions` (false when unauthenticated / permissions undefined). Optionally expose a
  `permissions` computed. No route/UI changes — that is the admin-UI feature.

### 6. Testing

- **`UserMapperTest`** (unit, `new UserMapperImpl()`): flatten across multiple roles → distinct,
  sorted permission names; null-safe when a role has no permissions.
- **`RoleMapperTest`** (unit): single role → sorted permission names; empty when none.
- **`PermissionControllerSecurityTest`** / **`RoleControllerSecurityTest`** (`@WebMvcTest` slice,
  same pattern as `ProblemControllerSecurityTest`): with the read permission → 200; without
  (e.g. only `ADMIN` authority, or wrong permission) → 403.
- **Frontend vitest**: `hasPermission` returns true/false correctly; false when unauthenticated.

### 7. Rollout / safety

Additive: new DTO field, new read endpoints, new permissions (idempotent V4). `@JsonIgnore` only
removes an accidental nested field introduced by the prior change — no consumer reads it. No
breaking change to existing responses.

## Success Criteria

- `GET /auth/me` returns a flat `permissions: string[]`; `roles[]` no longer carries nested
  permission arrays.
- `GET /api/v1/permissions` returns the catalog for a user with `permission:read`, 403 otherwise.
- `GET /api/v1/roles` returns roles with their permission names for a user with `role:read`,
  403 otherwise.
- ADMIN is seeded with `role:read` + `permission:read` (V4 idempotent).
- Frontend `authStore.hasPermission('problem:create')` works off `/me` data.
- New + existing tests pass (except the known offline `contextLoads`).
