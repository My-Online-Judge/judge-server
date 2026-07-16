# Design Spec: Permission-Based Authorization (judge-api)

**Date:** 2026-07-16
**Status:** Approved
**Target repo:** `judge-api` (Spring Boot 3.4 / Spring Security 6.4, JWT)

## Problem & Goal

Authorization today is **role-based**: authorities are the raw role names (`USER`, `ADMIN`)
and the only enforced checks are 4 `@PreAuthorize("hasAuthority('ADMIN')")` annotations.
This couples "who you are" to "what you can do" — every new gated action reuses the coarse
`ADMIN` bucket, and changing who-can-do-what needs code edits.

**Goal:** move enforcement to **permissions**. A role has many permissions; checks assert a
specific permission (e.g. `problem:create`) rather than a role. Roles remain as groupings of
permissions. The permission model is **fully dynamic-capable** (permissions are DB rows with
arbitrary names; all role↔permission and user↔role mappings are data), so a future admin API
can manage them without schema/code changes.

## Scope

**In scope (this slice):** data model, Flyway migration, seed catalog + ADMIN mapping, rewire
the single authority choke point, convert the 4 checks, tests.

**Deferred (model supports, not built here):**
- Admin CRUD REST API for permissions / role↔permission / user↔role assignment.
- judge-portal admin UI.
- Submission ownership gaps (`/submissions/user/{userId}` has no owner check today).
- Moving permissions into the JWT (authorities stay DB-derived per request).

## Current State (verified)

- **Authority choke point:** `SecurityUserDetails.getAuthorities()` maps `role.name` →
  `SimpleGrantedAuthority`. Authorities are **un-prefixed** (`ADMIN`, not `ROLE_ADMIN`) → checks
  use `hasAuthority`, not `hasRole`. This is the ONE place to change.
- `UserDetails` is reloaded from DB every request by `CustomUserDetailsService`; the JWT `roles`
  claim is decorative (ignored for authz). → permissions can be added purely server-side.
- **Enforced checks (exactly 4):**
  - `ProblemController#createProblem` (POST `/api/v1/problems`) — `hasAuthority('ADMIN')`
  - `ProblemController#updateProblem` (PUT `/{slug}`) — `hasAuthority('ADMIN')`
  - `ProblemController#deleteProblem` (DELETE `/{slug}`) — `hasAuthority('ADMIN')`
  - `JudgeServerController#listJudgeServers` (GET `/api/v1/judge-servers`) — `hasAuthority('ADMIN')`
- **Entities** extend `BaseEntity` (UUID PK + audit). `Role` = name + description, no permissions yet.
  `User.roles` is `@ManyToMany(EAGER)` via `t_users_roles`.
- **DB seeding:** `initdb.sql` runs only on fresh docker volume init; **Flyway** (`V1`, `V2`) is the
  real source of truth for schema changes. Dev/prod use `ddl-auto: validate` → entity model MUST
  match a Flyway-created schema. **Tests** use H2 + `ddl-auto: update` (Flyway off) → new entities
  auto-create their tables; seed done in-test.

## Design

### 1. Data model (additive)

New `Permission` entity → table `t_permissions`:
- `id UUID` (via `BaseEntity`), audit columns.
- `name VARCHAR` — **unique, NOT NULL** — equals the Spring authority string (e.g. `problem:create`).
- `description VARCHAR`.

New join table `t_roles_permissions` (`role_id UUID`, `permission_id UUID`), PK `(role_id, permission_id)`,
FKs to `t_roles` / `t_permissions`.

`Role` entity gains:
```java
@ManyToMany(fetch = FetchType.EAGER)
@JoinTable(name = "t_roles_permissions",
    joinColumns = @JoinColumn(name = "role_id"),
    inverseJoinColumns = @JoinColumn(name = "permission_id"))
private Set<Permission> permissions;
```
EAGER is acceptable: roles are already eager on `User`, sets are small, and it avoids
`LazyInitializationException` when `getAuthorities()` flattens them outside a transaction.

### 2. Authority mapping — `SecurityUserDetails.getAuthorities()`

Emit the **union** of role names and flattened permission names, deduplicated:
```java
Set<String> authorities = new HashSet<>();
for (Role role : user.getRoles()) {
    authorities.add(role.getName());                       // back-compat: "ADMIN"
    if (role.getPermissions() != null) {
        role.getPermissions().forEach(p -> authorities.add(p.getName())); // "problem:create", ...
    }
}
return authorities.stream().map(SimpleGrantedAuthority::new).toList();
```
Result for an ADMIN user: `{ADMIN, problem:create, problem:update, problem:delete, judgeserver:read}`.
`hasAuthority('ADMIN')` still works everywhere; new checks use permission strings.

### 3. Permission catalog (seed) — `resource:action`

| name | description | mapped roles |
|---|---|---|
| `problem:create` | Create problems | ADMIN |
| `problem:update` | Update problems | ADMIN |
| `problem:delete` | Delete problems | ADMIN |
| `judgeserver:read` | List/view judge servers | ADMIN |

`USER` role → no permissions (unchanged behavior). New Google users still get `USER` by default.

### 4. Enforcement changes

Convert the 4 annotations to permission strings:

| File / method | New expression |
|---|---|
| `ProblemController#createProblem` | `hasAuthority('problem:create')` |
| `ProblemController#updateProblem` | `hasAuthority('problem:update')` |
| `ProblemController#deleteProblem` | `hasAuthority('problem:delete')` |
| `JudgeServerController#listJudgeServers` | `hasAuthority('judgeserver:read')` |

Add `common/constant/Permissions.java` holding the string constants (single source of truth for
seed + tests). Annotations keep literal strings (SpEL) to match the existing `'ADMIN'` style and
because `@PreAuthorize` cannot cleanly reference constants without verbose `T(...)` SpEL.

### 5. Schema + seed delivery

- **`V3__add_permissions.sql`** (Flyway) — source of truth for dev/prod/docker-existing:
  - `CREATE TABLE t_permissions (...)`, `CREATE TABLE t_roles_permissions (...)` with FKs.
  - Seed the 4 permissions (fixed UUIDs, idempotent `ON CONFLICT (id) DO NOTHING`).
  - Insert `t_roles_permissions` rows mapping ADMIN → all 4 (idempotent).
- **`initdb.sql`** — updated to include the new tables + seed so a fresh docker volume is
  consistent with Flyway. (Exact Flyway baseline interaction confirmed during implementation.)
- **Tests** — H2 `ddl-auto: update` builds the tables from entities; roles/permissions/users
  seeded programmatically in the test.

### 6. Testing strategy

1. **Unit — `SecurityUserDetails.getAuthorities()`:** returns role names + flattened permissions,
   deduplicated; handles null/empty permission set.
2. **MockMvc security test (new):**
   - ADMIN user (has `problem:create`) → POST `/api/v1/problems` is authorized (not 403).
   - USER (no permissions) → 403 on POST `/api/v1/problems`.
   - A user granted `problem:create/update` + `judgeserver:read` but **not** `problem:delete`
     → 403 on DELETE `/{slug}`, proving enforcement is per-permission, not per-role.

### 7. Rollout / safety

Purely additive schema; `ADMIN` authority is still emitted, so existing behavior is preserved
and permission checks are layered on. Zero-downtime, reversible (drop new tables + revert
annotations).

## Success Criteria

- New `t_permissions` + `t_roles_permissions` exist; ADMIN mapped to all 4 permissions.
- The 4 endpoints enforce their specific permission; an ADMIN passes, a USER gets 403.
- A user with a partial permission set is blocked on the permission they lack.
- Existing tests still pass; new security + authorities tests pass.
- App starts under `ddl-auto: validate` (entity model matches Flyway schema).
