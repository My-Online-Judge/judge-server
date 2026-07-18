# Admin Role CRUD (Create + Delete) — Design

Date: 2026-07-18
Repos: `judge-api` (backend), `judge-portal` (frontend)
Status: approved, ready for planning

## Problem

The admin Roles page can list roles and replace a role's permission set
(`PUT /api/v1/roles/{id}/permissions`, shipped in the previous
`2026-07-18-admin-roles-permissions-design`). It **cannot create a new role or
delete one** — the earlier design put that out of scope ("the catalog is
fixed/seeded"). This design fills exactly that gap: **Create** and **Delete**.

Read (list) already exists; permission assignment (the "U" that matters) already
exists via the replace-all PUT. So "CRUD roles" here means adding Create +
Delete on top of what's shipped.

## Decisions (from brainstorming)

1. **Create UX — two-step.** The create dialog collects only `name` +
   `description`. A new role starts with **zero permissions**; the admin then
   uses the existing "Edit permissions" dialog to grant. Reuses the existing
   replace-all flow, keeps the POST body small, no duplicated permission picker.
2. **Delete guard — block (409).** Deleting a role that any user still holds is
   refused with a clear message; the admin must reassign those users first. No
   silent access changes.
3. **Scope — Create + Delete only.** No rename / description edit after
   creation, no editing of system roles' names. Assigning permissions stays on
   the existing `PUT …/permissions`.

## Guards / invariants

- **System roles `ADMIN` and `USER`** are seeded and referenced by code/seed
  data. They **cannot be deleted**. (`ADMIN` permissions were already immutable.)
- Role **name**: required, unique, must match `^[A-Z][A-Z0-9_]*$` (uppercase
  letters/digits/underscore, leading letter). The app has **no Jakarta Bean
  Validation provider** on the classpath (`@Valid`/`@NotNull` are no-ops
  app-wide), so the **service** validates and null-checks itself.
- A role may only be deleted when **no user holds it** — checked with a count
  query, not a cascade.

## Backend — judge-api

### Flyway `V8__seed_role_crud_permissions.sql`

Mirror `V7` (additive + idempotent; safe on fresh DBs, baseline-on-migrate DBs,
and re-runs). Insert two permissions and grant both to `ADMIN`:

- `role:create` — UUID `a1000000-0000-0000-0000-000000000008`, desc
  "Create a new role".
- `role:delete` — UUID `a1000000-0000-0000-0000-000000000009`, desc
  "Delete a role".
- Grant to `ADMIN` role id `626c2558-a7d5-4fa4-9161-9bec2ae74076` via
  `INSERT INTO t_roles_permissions … ON CONFLICT (role_id, permission_id) DO NOTHING`.
- Permission inserts use `ON CONFLICT (id) DO NOTHING`.

After V8, `ADMIN` holds **9 permissions**.

### Endpoints (`RoleController`)

| Method   | Path                   | `@PreAuthorize`         | Body                     | Response        |
|----------|------------------------|-------------------------|--------------------------|-----------------|
| `POST`   | `/api/v1/roles`        | `hasAuthority('role:create')` | `{ name, description? }` | created role, HTTP 201 |
| `DELETE` | `/api/v1/roles/{id}`   | `hasAuthority('role:delete')` | —                        | HTTP 200, no data |

Response wrapping follows the existing create-endpoint convention in the
codebase (`ApiResponse.created(...)` for POST; `ApiResponse.success()` no-data
for DELETE) — match whatever an existing `@PostMapping` create controller does.

### DTO

`role/dto/CreateRoleRequest`:

```java
@Data
public class CreateRoleRequest {
    private String name;
    private String description;
}
```

### `RoleService`

```java
private static final Set<String> SYSTEM_ROLES = Set.of("ADMIN", "USER");
private static final Pattern ROLE_NAME = Pattern.compile("^[A-Z][A-Z0-9_]*$");

@Transactional
public RoleResponse create(String name, String description) {
    String normalized = name == null ? "" : name.trim();
    if (!ROLE_NAME.matcher(normalized).matches()) {
        throw new AppException(ErrorCode.ROLE_NAME_INVALID);
    }
    if (roleRepository.findByName(normalized).isPresent()) {
        throw new AppException(ErrorCode.ROLE_EXISTED);
    }
    Role role = Role.builder()
            .name(normalized)
            .description(description == null ? null : description.trim())
            .permissions(new HashSet<>())
            .build();
    roleRepository.save(role);
    return roleMapper.toResponse(role);
}

@Transactional
public void delete(UUID roleId) {
    Role role = roleRepository.findById(roleId)
            .orElseThrow(() -> new AppException(ErrorCode.ROLE_NOT_EXISTED));
    if (SYSTEM_ROLES.contains(role.getName())) {
        throw new AppException(ErrorCode.ROLE_PROTECTED);
    }
    if (userRepository.countByRoles_Id(roleId) > 0) {
        throw new AppException(ErrorCode.ROLE_IN_USE);
    }
    roleRepository.delete(role); // clears t_roles_permissions (Role is the owning side)
}
```

`delete` relies on `Role` owning the `t_roles_permissions` join, so Hibernate
removes join rows before deleting the role. `t_users_roles` is owned by `User`
and is guaranteed empty by the in-use guard, so the FK
(`t_users_roles.role_id → t_roles(id)`, no cascade) never blocks.

### New supporting members

- `ErrorCode`: `ROLE_EXISTED` (409 CONFLICT, "Role name already exists"),
  `ROLE_PROTECTED` (409 CONFLICT, "This system role cannot be deleted"),
  `ROLE_IN_USE` (409 CONFLICT, "This role is still assigned to one or more
  users"), `ROLE_NAME_INVALID` (400 BAD_REQUEST, "Role name must be uppercase
  letters, digits, or underscore and start with a letter").
- `UserRepository`: `long countByRoles_Id(UUID roleId);`

### Tests (Mockito, offline)

`RoleServiceCreateTest`:
- valid name → saves role with empty permissions and returns mapped response;
- name failing the regex (e.g. blank, lowercase, leading digit) → `ROLE_NAME_INVALID`, never saves;
- duplicate name (`findByName` present) → `ROLE_EXISTED`, never saves;
- leading/trailing whitespace is trimmed before persist/uniqueness check.

`RoleServiceDeleteTest`:
- `ADMIN` and `USER` → `ROLE_PROTECTED`, never deletes, never counts users;
- unknown id → `ROLE_NOT_EXISTED`;
- custom role with `countByRoles_Id > 0` → `ROLE_IN_USE`, never deletes;
- custom role with `0` users → `roleRepository.delete(role)` called.

## Frontend — judge-portal

### `constants/apiPath.ts`

Add to `ROLES`: `DETAIL: (id: string) => \`/roles/${id}\``. (POST uses `ROOT`.)

### `services/roleService.ts`

- `create(payload: { name: string; description?: string })` →
  `axiosClient.post(API_ROUTES.ROLES.ROOT, payload)`.
- `remove(id: string)` → `axiosClient.delete(API_ROUTES.ROLES.DETAIL(id))`.

### `lib/roleName.ts` (+ `__tests__`)

`isValidRoleName(name: string): boolean` — the same `^[A-Z][A-Z0-9_]*$` regex,
used to gate the create dialog's Save button. Unit-tested (valid names; blank;
lowercase; leading digit; embedded space; underscore ok).

### `pages/admin/AdminRolesPage.vue`

- **Create button** in the card header (next to Refresh), shown when
  `hasPermission('role:create')`. Opens a create `Dialog`:
  - `name` input (`Input`) — hint "UPPERCASE, digits, underscore"; live-validate
    with `isValidRoleName`; Save disabled while invalid or empty.
  - `description` textarea (`Textarea`, optional).
  - Save → `roleService.create(...)` → success toast + `loadRoles()`; on error
    show `err.response.data.message` (covers 409 duplicate / 400 invalid).
- **Delete action** in the Actions cell — a Trash icon button, shown for
  non-system roles when `hasPermission('role:delete')`. Click opens a
  confirmation `Dialog` ("Delete role `X`? This can't be undone."); confirm →
  `roleService.remove(id)` → success toast + `loadRoles()`; on error show the
  server message (covers 409 in-use / protected).
- `isSystemRole(role)` = `role.name === 'ADMIN' || role.name === 'USER'`.
  System roles show no delete button (`ADMIN` keeps "Locked · full access",
  `USER` keeps its Edit-permissions button).
- Reuse the existing `useFetch` / `useToast` / `triggerToast` patterns already
  in the page. No new UI dependency — the delete confirm reuses `Dialog`.

## Out of scope (unchanged)

- Renaming or editing a role's description after creation; renaming system roles.
- Creating/deleting permissions in the catalog.
- The Users admin page + user↔role assignment (still a separate future feature;
  it is what would let an admin free up an in-use role before deleting it).

## Verification

- Backend: `./mvnw test` for the two new test classes (plus existing role tests).
- Deploy to the running `judge-deployment` stack; confirm Flyway advances to v8.
- Live checks: `GET /permissions` shows `role:create` + `role:delete`;
  `POST /roles` creates a custom role (201) and rejects a duplicate/invalid name;
  `DELETE /roles/{id}` removes a custom empty role, and returns 409 for `ADMIN`/
  `USER` and for a role still held by a user.
- Frontend: type-check + vitest (incl. new `roleName` test); manual pass of the
  create dialog and delete confirm in `/admin/roles`.
