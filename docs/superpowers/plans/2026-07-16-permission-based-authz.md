# Permission-Based Authorization Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move judge-api authorization from role-based to permission-based — a role has many permissions, and endpoint checks assert a specific permission (e.g. `problem:create`) instead of the `ADMIN` role.

**Architecture:** Add a `Permission` entity + `t_roles_permissions` join. The single authority choke point (`SecurityUserDetails.getAuthorities()`) emits the union of role names (back-compat) and each role's flattened permission names. The 4 existing `@PreAuthorize("hasAuthority('ADMIN')")` checks become permission checks. Schema + seed ship as an idempotent Flyway `V3` migration; tests run on H2 (Flyway off) with programmatic seed.

**Tech Stack:** Spring Boot 3.4, Spring Security 6.4 (`@EnableMethodSecurity`), JPA/Hibernate, Flyway, PostgreSQL (prod) / H2 (test), JUnit 5, Mockito, MockMvc, spring-security-test.

## Global Constraints

- Base package: `vn.thanhtuanle`. All entities extend `BaseEntity` (UUID PK + JPA auditing: `createdAt`, `updatedAt`, `createdBy`, `updatedBy`).
- Authorities are **un-prefixed** — use `hasAuthority('...')`, never `hasRole(...)`.
- Permission naming: `resource:action` (lowercase, colon-separated).
- Dev/prod run `ddl-auto: validate` → the entity model MUST match the Flyway-created schema exactly (mirror the existing `t_roles` / `t_users_roles` DDL for column names).
- Tests use the `test` profile: H2, `ddl-auto: update`, **Flyway disabled** → new tables auto-create from entities; seed is done in-test, not via migration.
- The 4 permissions and their ADMIN mapping: `problem:create`, `problem:update`, `problem:delete`, `judgeserver:read`.
- ADMIN role UUID (seeded): `626c2558-a7d5-4fa4-9161-9bec2ae74076`.
- Fixed permission UUIDs (for idempotent seed):
  - `problem:create` → `a1000000-0000-0000-0000-000000000001`
  - `problem:update` → `a1000000-0000-0000-0000-000000000002`
  - `problem:delete` → `a1000000-0000-0000-0000-000000000003`
  - `judgeserver:read` → `a1000000-0000-0000-0000-000000000004`
- Run tests with: `./mvnw -q -Dspring.profiles.active=test test` (Flyway off, H2). Use `-Dtest=ClassName` to scope.

---

### Task 1: Permission model + authority flattening

Adds the `Permission` entity, the `Role → Permission` association, a `Permissions` constants class, and rewrites `getAuthorities()` to emit role names + flattened permissions. Driven by a pure unit test (no Spring context, offline-safe).

**Files:**
- Create: `judge-api/src/main/java/vn/thanhtuanle/entity/Permission.java`
- Create: `judge-api/src/main/java/vn/thanhtuanle/common/constant/Permissions.java`
- Modify: `judge-api/src/main/java/vn/thanhtuanle/entity/Role.java`
- Modify: `judge-api/src/main/java/vn/thanhtuanle/auth/SecurityUserDetails.java:21-26`
- Test: `judge-api/src/test/java/vn/thanhtuanle/auth/SecurityUserDetailsTest.java`

**Interfaces:**
- Produces: `Permission` entity (`getName()/setName()`, `getDescription()/setDescription()`); `Role.getPermissions()/setPermissions(Set<Permission>)`; `Permissions.PROBLEM_CREATE/PROBLEM_UPDATE/PROBLEM_DELETE/JUDGESERVER_READ` (String constants); `SecurityUserDetails.getAuthorities()` returns role names ∪ permission names.

- [ ] **Step 1: Write the failing test**

Create `judge-api/src/test/java/vn/thanhtuanle/auth/SecurityUserDetailsTest.java`:

```java
package vn.thanhtuanle.auth;

import org.junit.jupiter.api.Test;
import org.springframework.security.core.GrantedAuthority;
import vn.thanhtuanle.entity.Permission;
import vn.thanhtuanle.entity.Role;
import vn.thanhtuanle.entity.User;

import java.util.List;
import java.util.Set;
import java.util.stream.Collectors;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

class SecurityUserDetailsTest {

    private Permission perm(String name) {
        Permission p = new Permission();
        p.setName(name);
        return p;
    }

    private Role role(String name, Permission... perms) {
        Role r = new Role();
        r.setName(name);
        r.setPermissions(Set.of(perms));
        return r;
    }

    private Set<String> authoritiesOf(User user) {
        return new SecurityUserDetails(user).getAuthorities().stream()
                .map(GrantedAuthority::getAuthority)
                .collect(Collectors.toSet());
    }

    @Test
    void includesRoleNameAndFlattenedPermissions() {
        User user = new User();
        user.setRoles(Set.of(role("ADMIN", perm("problem:create"), perm("problem:delete"))));

        Set<String> authorities = authoritiesOf(user);

        assertTrue(authorities.contains("ADMIN"));
        assertTrue(authorities.contains("problem:create"));
        assertTrue(authorities.contains("problem:delete"));
        assertEquals(3, authorities.size());
    }

    @Test
    void deduplicatesPermissionNameSharedAcrossRoles() {
        User user = new User();
        user.setRoles(Set.of(
                role("ADMIN", perm("problem:create")),
                role("EDITOR", perm("problem:create"))));

        long createCount = new SecurityUserDetails(user).getAuthorities().stream()
                .map(GrantedAuthority::getAuthority)
                .filter("problem:create"::equals)
                .count();

        assertEquals(1, createCount);
    }

    @Test
    void handlesRoleWithNullPermissions() {
        User user = new User();
        Role userRole = new Role();
        userRole.setName("USER");
        userRole.setPermissions(null);
        user.setRoles(Set.of(userRole));

        assertEquals(Set.of("USER"), authoritiesOf(user));
    }
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd judge-api && ./mvnw -q -Dspring.profiles.active=test -Dtest=SecurityUserDetailsTest test`
Expected: COMPILE FAILURE — `Permission` class and `Role.setPermissions(...)` do not exist yet.

- [ ] **Step 3: Create the `Permission` entity**

Create `judge-api/src/main/java/vn/thanhtuanle/entity/Permission.java`:

```java
package vn.thanhtuanle.entity;

import jakarta.persistence.Column;
import jakarta.persistence.Entity;
import jakarta.persistence.Table;
import lombok.AllArgsConstructor;
import lombok.EqualsAndHashCode;
import lombok.Getter;
import lombok.NoArgsConstructor;
import lombok.Setter;
import lombok.experimental.SuperBuilder;

@Entity
@Table(name = "t_permissions")
@Getter
@Setter
@NoArgsConstructor
@AllArgsConstructor
@SuperBuilder
@EqualsAndHashCode(callSuper = true)
public class Permission extends BaseEntity {

    @Column(unique = true)
    private String name;

    private String description;
}
```

- [ ] **Step 4: Add the `permissions` association to `Role`**

Modify `judge-api/src/main/java/vn/thanhtuanle/entity/Role.java` — replace the whole file with:

```java
package vn.thanhtuanle.entity;

import jakarta.persistence.Entity;
import jakarta.persistence.FetchType;
import jakarta.persistence.JoinColumn;
import jakarta.persistence.JoinTable;
import jakarta.persistence.ManyToMany;
import jakarta.persistence.Table;
import lombok.*;
import lombok.experimental.SuperBuilder;

import java.util.Set;

@Entity
@Table(name = "t_roles")
@Getter
@Setter
@NoArgsConstructor
@AllArgsConstructor
@SuperBuilder
@EqualsAndHashCode(callSuper = true, exclude = "permissions")
public class Role extends BaseEntity {
    private String name;
    private String description;

    @ManyToMany(fetch = FetchType.EAGER)
    @JoinTable(name = "t_roles_permissions",
            joinColumns = @JoinColumn(name = "role_id"),
            inverseJoinColumns = @JoinColumn(name = "permission_id"))
    private Set<Permission> permissions;
}
```

> `exclude = "permissions"` keeps the eager collection out of `equals`/`hashCode` (JPA anti-pattern otherwise).

- [ ] **Step 5: Create the `Permissions` constants class**

Create `judge-api/src/main/java/vn/thanhtuanle/common/constant/Permissions.java`:

```java
package vn.thanhtuanle.common.constant;

/**
 * Canonical permission strings (resource:action). These are the authority values emitted by
 * SecurityUserDetails and referenced by seed data / tests. @PreAuthorize annotations use the
 * literal strings (SpEL cannot cleanly reference these constants), so keep the two in sync.
 */
public final class Permissions {

    private Permissions() {}

    public static final String PROBLEM_CREATE = "problem:create";
    public static final String PROBLEM_UPDATE = "problem:update";
    public static final String PROBLEM_DELETE = "problem:delete";
    public static final String JUDGESERVER_READ = "judgeserver:read";
}
```

- [ ] **Step 6: Rewrite `getAuthorities()` to flatten permissions**

Modify `judge-api/src/main/java/vn/thanhtuanle/auth/SecurityUserDetails.java`. Replace the imports block (lines 1-10) and the `getAuthorities()` method (lines 21-26):

Replace imports:
```java
package vn.thanhtuanle.auth;

import lombok.Data;
import org.springframework.security.core.GrantedAuthority;
import org.springframework.security.core.authority.SimpleGrantedAuthority;
import org.springframework.security.core.userdetails.UserDetails;
import vn.thanhtuanle.entity.Permission;
import vn.thanhtuanle.entity.Role;
import vn.thanhtuanle.entity.User;

import java.util.Collection;
import java.util.HashSet;
import java.util.Set;
import java.util.stream.Collectors;
```

Replace the method:
```java
    @Override
    public Collection<? extends GrantedAuthority> getAuthorities() {
        Set<String> authorities = new HashSet<>();
        if (user.getRoles() != null) {
            for (Role role : user.getRoles()) {
                authorities.add(role.getName());
                if (role.getPermissions() != null) {
                    for (Permission permission : role.getPermissions()) {
                        authorities.add(permission.getName());
                    }
                }
            }
        }
        return authorities.stream()
                .map(SimpleGrantedAuthority::new)
                .collect(Collectors.toList());
    }
```

- [ ] **Step 7: Run the test to verify it passes**

Run: `cd judge-api && ./mvnw -q -Dspring.profiles.active=test -Dtest=SecurityUserDetailsTest test`
Expected: PASS (3 tests).

- [ ] **Step 8: Commit**

```bash
cd judge-api
git add src/main/java/vn/thanhtuanle/entity/Permission.java \
        src/main/java/vn/thanhtuanle/entity/Role.java \
        src/main/java/vn/thanhtuanle/common/constant/Permissions.java \
        src/main/java/vn/thanhtuanle/auth/SecurityUserDetails.java \
        src/test/java/vn/thanhtuanle/auth/SecurityUserDetailsTest.java
git commit -m "feat: permission model + flatten role permissions into authorities"
```

---

### Task 2: Convert endpoint checks to permission-based (test-first)

Adds `spring-security-test`, writes a MockMvc slice test proving DELETE requires `problem:delete` (and that the `ADMIN` authority alone no longer suffices), then converts the 4 annotations.

**Files:**
- Modify: `judge-api/pom.xml` (add `spring-security-test`)
- Test: `judge-api/src/test/java/vn/thanhtuanle/problem/ProblemControllerSecurityTest.java`
- Modify: `judge-api/src/main/java/vn/thanhtuanle/problem/ProblemController.java:43,77,87`
- Modify: `judge-api/src/main/java/vn/thanhtuanle/judgeserver/JudgeServerController.java:58`

**Interfaces:**
- Consumes: `SecurityConfig`, `JwtAuthenticationFilter`, `CustomAuthenticationEntryPoint`, `CustomAccessDeniedHandler`, `JwtUtil`, `ProblemService`, `UserDetailsService` bean named `userDetailsService`.
- Produces: `ProblemController` methods gated by `problem:create/update/delete`; `JudgeServerController#listJudgeServers` gated by `judgeserver:read`.

- [ ] **Step 1: Add `spring-security-test` to the pom (test scope)**

Modify `judge-api/pom.xml` — add immediately after the `spring-boot-starter-test` dependency (around line 72):

```xml
		<dependency>
			<groupId>org.springframework.security</groupId>
			<artifactId>spring-security-test</artifactId>
			<scope>test</scope>
		</dependency>
```

- [ ] **Step 2: Write the failing security test**

Create `judge-api/src/test/java/vn/thanhtuanle/problem/ProblemControllerSecurityTest.java`:

```java
package vn.thanhtuanle.problem;

import org.junit.jupiter.api.Test;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.test.autoconfigure.web.servlet.WebMvcTest;
import org.springframework.boot.test.mock.mockito.MockBean;
import org.springframework.context.annotation.Import;
import org.springframework.security.core.userdetails.UserDetailsService;
import org.springframework.security.test.context.support.WithMockUser;
import org.springframework.test.context.ActiveProfiles;
import org.springframework.test.web.servlet.MockMvc;

import vn.thanhtuanle.common.util.JwtUtil;
import vn.thanhtuanle.config.CustomAccessDeniedHandler;
import vn.thanhtuanle.config.CustomAuthenticationEntryPoint;
import vn.thanhtuanle.config.JwtAuthenticationFilter;
import vn.thanhtuanle.config.SecurityConfig;

import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.delete;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.status;

@WebMvcTest(controllers = ProblemController.class)
@Import({SecurityConfig.class, JwtAuthenticationFilter.class,
        CustomAuthenticationEntryPoint.class, CustomAccessDeniedHandler.class})
@ActiveProfiles("test")
class ProblemControllerSecurityTest {

    @Autowired
    private MockMvc mockMvc;

    @MockBean
    private ProblemService problemService;
    @MockBean
    private JwtUtil jwtUtil;
    @MockBean
    private UserDetailsService userDetailsService;

    @Test
    @WithMockUser(authorities = "problem:delete")
    void deleteAllowed_whenUserHasProblemDeletePermission() throws Exception {
        mockMvc.perform(delete("/api/v1/problems/some-slug"))
                .andExpect(status().isOk());
    }

    @Test
    @WithMockUser(authorities = "ADMIN")
    void deleteForbidden_whenUserHasOnlyAdminRoleAuthority() throws Exception {
        mockMvc.perform(delete("/api/v1/problems/some-slug"))
                .andExpect(status().isForbidden());
    }

    @Test
    @WithMockUser(authorities = "problem:create")
    void deleteForbidden_whenUserHasWrongPermission() throws Exception {
        mockMvc.perform(delete("/api/v1/problems/some-slug"))
                .andExpect(status().isForbidden());
    }
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `cd judge-api && ./mvnw -q -Dspring.profiles.active=test -Dtest=ProblemControllerSecurityTest test`
Expected: FAIL — `deleteForbidden_whenUserHasOnlyAdminRoleAuthority` gets 200 (still `hasAuthority('ADMIN')`), and `deleteAllowed_whenUserHasProblemDeletePermission` gets 403.

> If instead it fails to load the context (missing mock bean / handler dependency), add the missing collaborator as a `@MockBean` or to `@Import` and re-run — the goal is a green context with the 3 assertions as stated.

- [ ] **Step 4: Convert the ProblemController annotations**

In `judge-api/src/main/java/vn/thanhtuanle/problem/ProblemController.java`:
- Line 43: `@PreAuthorize("hasAuthority('ADMIN')")` → `@PreAuthorize("hasAuthority('problem:create')")`
- Line 77: `@PreAuthorize("hasAuthority('ADMIN')")` → `@PreAuthorize("hasAuthority('problem:update')")`
- Line 87: `@PreAuthorize("hasAuthority('ADMIN')")` → `@PreAuthorize("hasAuthority('problem:delete')")`

- [ ] **Step 5: Convert the JudgeServerController annotation**

In `judge-api/src/main/java/vn/thanhtuanle/judgeserver/JudgeServerController.java`:
- Line 58: `@PreAuthorize("hasAuthority('ADMIN')")` → `@PreAuthorize("hasAuthority('judgeserver:read')")`

- [ ] **Step 6: Run the security test to verify it passes**

Run: `cd judge-api && ./mvnw -q -Dspring.profiles.active=test -Dtest=ProblemControllerSecurityTest test`
Expected: PASS (3 tests).

- [ ] **Step 7: Commit**

```bash
cd judge-api
git add pom.xml \
        src/test/java/vn/thanhtuanle/problem/ProblemControllerSecurityTest.java \
        src/main/java/vn/thanhtuanle/problem/ProblemController.java \
        src/main/java/vn/thanhtuanle/judgeserver/JudgeServerController.java
git commit -m "feat: gate problem + judge-server endpoints by permission not role"
```

---

### Task 3: Flyway migration + initdb.sql seed

Ships the schema + seed to real (Postgres) databases so the app boots under `ddl-auto: validate` and ADMIN has the 4 permissions.

**Files:**
- Create: `judge-api/src/main/resources/db/migration/V3__add_permissions.sql`
- Modify: `judge-api/initdb.sql` (append the new tables + seed for fresh-volume consistency)

**Interfaces:**
- Consumes: `t_roles` seed row `626c2558-a7d5-4fa4-9161-9bec2ae74076` (ADMIN); `Permission` / `Role` entity column mapping from Task 1.
- Produces: tables `t_permissions`, `t_roles_permissions`; 4 seeded permissions mapped to ADMIN.

- [ ] **Step 1: Create the Flyway migration**

Create `judge-api/src/main/resources/db/migration/V3__add_permissions.sql`:

```sql
-- V3: permission-based authorization. Additive + idempotent so it is safe on a fresh DB,
-- on an existing DB adopting Flyway via baseline-on-migrate, and when re-run.
-- Column names mirror t_roles / t_users_roles so Hibernate ddl-auto:validate passes.

CREATE TABLE IF NOT EXISTS public.t_permissions (
    id uuid NOT NULL,
    created_at timestamp(6) without time zone NOT NULL,
    created_by character varying(255),
    updated_at timestamp(6) without time zone NOT NULL,
    updated_by character varying(255),
    description character varying(255),
    name character varying(255),
    CONSTRAINT t_permissions_pkey PRIMARY KEY (id),
    CONSTRAINT t_permissions_name_key UNIQUE (name)
);

CREATE TABLE IF NOT EXISTS public.t_roles_permissions (
    role_id uuid NOT NULL,
    permission_id uuid NOT NULL,
    CONSTRAINT t_roles_permissions_pkey PRIMARY KEY (role_id, permission_id),
    CONSTRAINT fk_roles_permissions_role FOREIGN KEY (role_id) REFERENCES public.t_roles(id),
    CONSTRAINT fk_roles_permissions_permission FOREIGN KEY (permission_id) REFERENCES public.t_permissions(id)
);

INSERT INTO public.t_permissions (id, created_at, created_by, updated_at, updated_by, description, name) VALUES
    ('a1000000-0000-0000-0000-000000000001', '2026-07-16 00:00:00', 'SYS', '2026-07-16 00:00:00', 'SYS', 'Create problems', 'problem:create'),
    ('a1000000-0000-0000-0000-000000000002', '2026-07-16 00:00:00', 'SYS', '2026-07-16 00:00:00', 'SYS', 'Update problems', 'problem:update'),
    ('a1000000-0000-0000-0000-000000000003', '2026-07-16 00:00:00', 'SYS', '2026-07-16 00:00:00', 'SYS', 'Delete problems', 'problem:delete'),
    ('a1000000-0000-0000-0000-000000000004', '2026-07-16 00:00:00', 'SYS', '2026-07-16 00:00:00', 'SYS', 'List judge servers', 'judgeserver:read')
ON CONFLICT (id) DO NOTHING;

INSERT INTO public.t_roles_permissions (role_id, permission_id) VALUES
    ('626c2558-a7d5-4fa4-9161-9bec2ae74076', 'a1000000-0000-0000-0000-000000000001'),
    ('626c2558-a7d5-4fa4-9161-9bec2ae74076', 'a1000000-0000-0000-0000-000000000002'),
    ('626c2558-a7d5-4fa4-9161-9bec2ae74076', 'a1000000-0000-0000-0000-000000000003'),
    ('626c2558-a7d5-4fa4-9161-9bec2ae74076', 'a1000000-0000-0000-0000-000000000004')
ON CONFLICT (role_id, permission_id) DO NOTHING;
```

- [ ] **Step 2: Append the same objects to `initdb.sql`**

Append at the END of `judge-api/initdb.sql` (fresh-volume dump consistency; runs only on an empty volume so plain statements are fine):

```sql

--
-- Name: t_permissions / t_roles_permissions (permission-based authz); added 2026-07-16
--

CREATE TABLE IF NOT EXISTS public.t_permissions (
    id uuid NOT NULL,
    created_at timestamp(6) without time zone NOT NULL,
    created_by character varying(255),
    updated_at timestamp(6) without time zone NOT NULL,
    updated_by character varying(255),
    description character varying(255),
    name character varying(255),
    CONSTRAINT t_permissions_pkey PRIMARY KEY (id),
    CONSTRAINT t_permissions_name_key UNIQUE (name)
);

CREATE TABLE IF NOT EXISTS public.t_roles_permissions (
    role_id uuid NOT NULL,
    permission_id uuid NOT NULL,
    CONSTRAINT t_roles_permissions_pkey PRIMARY KEY (role_id, permission_id),
    CONSTRAINT fk_roles_permissions_role FOREIGN KEY (role_id) REFERENCES public.t_roles(id),
    CONSTRAINT fk_roles_permissions_permission FOREIGN KEY (permission_id) REFERENCES public.t_permissions(id)
);

INSERT INTO public.t_permissions (id, created_at, created_by, updated_at, updated_by, description, name) VALUES
    ('a1000000-0000-0000-0000-000000000001', '2026-07-16 00:00:00', 'SYS', '2026-07-16 00:00:00', 'SYS', 'Create problems', 'problem:create'),
    ('a1000000-0000-0000-0000-000000000002', '2026-07-16 00:00:00', 'SYS', '2026-07-16 00:00:00', 'SYS', 'Update problems', 'problem:update'),
    ('a1000000-0000-0000-0000-000000000003', '2026-07-16 00:00:00', 'SYS', '2026-07-16 00:00:00', 'SYS', 'Delete problems', 'problem:delete'),
    ('a1000000-0000-0000-0000-000000000004', '2026-07-16 00:00:00', 'SYS', '2026-07-16 00:00:00', 'SYS', 'List judge servers', 'judgeserver:read')
ON CONFLICT (id) DO NOTHING;

INSERT INTO public.t_roles_permissions (role_id, permission_id) VALUES
    ('626c2558-a7d5-4fa4-9161-9bec2ae74076', 'a1000000-0000-0000-0000-000000000001'),
    ('626c2558-a7d5-4fa4-9161-9bec2ae74076', 'a1000000-0000-0000-0000-000000000002'),
    ('626c2558-a7d5-4fa4-9161-9bec2ae74076', 'a1000000-0000-0000-0000-000000000003'),
    ('626c2558-a7d5-4fa4-9161-9bec2ae74076', 'a1000000-0000-0000-0000-000000000004')
ON CONFLICT (role_id, permission_id) DO NOTHING;
```

- [ ] **Step 3: Verify the migration SQL applies cleanly + idempotently against Postgres**

Run against a disposable Postgres seeded from `initdb.sql` (per repo runtime notes; no published port, auto-removed):

```bash
cd judge-api
docker run -d --rm --name oj-v3-check -e POSTGRES_PASSWORD=pass \
  -v "$PWD/initdb.sql":/docker-entrypoint-initdb.d/init.sql postgres:16
sleep 6
# initdb.sql already created the tables (Step 2) — V3 must be a no-op, not error:
docker exec -i oj-v3-check psql -U postgres -d postgres < src/main/resources/db/migration/V3__add_permissions.sql
docker exec -i oj-v3-check psql -U postgres -d postgres < src/main/resources/db/migration/V3__add_permissions.sql   # run twice = idempotent
docker exec oj-v3-check psql -U postgres -d postgres -c \
  "SELECT r.name, p.name FROM t_roles_permissions rp JOIN t_roles r ON r.id=rp.role_id JOIN t_permissions p ON p.id=rp.permission_id ORDER BY 2;"
docker stop oj-v3-check
```
Expected: both `psql < V3` runs succeed with no error; the final SELECT shows ADMIN mapped to all 4 permissions.

> If `docker stop` fails with the AppArmor "permission denied" quirk noted in project memory, leave the container; it is `--rm` and harmless, and does not block the task.

- [ ] **Step 4: Commit**

```bash
cd judge-api
git add src/main/resources/db/migration/V3__add_permissions.sql initdb.sql
git commit -m "feat: seed permissions + role-permission mapping (Flyway V3 + initdb)"
```

---

### Task 4: Full regression + final verification

Confirms nothing else broke and captures the acceptance evidence.

**Files:** none (verification only).

- [ ] **Step 1: Run the full test suite (test profile)**

Run: `cd judge-api && ./mvnw -q -Dspring.profiles.active=test test`
Expected: BUILD SUCCESS. (The pre-existing `BackendServiceApplicationTests.contextLoads` may fail offline because it needs Postgres/Kafka — this is environmental per project memory, not a regression. Every other test, including the two new ones, passes.)

- [ ] **Step 2: Record acceptance**

Confirm and note:
- ADMIN user (via `getAuthorities()`) receives `{ADMIN, problem:create, problem:update, problem:delete, judgeserver:read}` — proven by `SecurityUserDetailsTest`.
- DELETE `/problems/{slug}` requires `problem:delete`; `ADMIN` authority alone is rejected — proven by `ProblemControllerSecurityTest`.
- `V3` migration applies idempotently and maps ADMIN → 4 permissions — proven in Task 3 Step 3.
- Deferred (not in this change): admin CRUD API, judge-portal UI, submission ownership checks, permissions-in-JWT.

> Full-boot `validate` verification (real Postgres + Kafka bring-up) is a manual/E2E step per project norms; the mirrored DDL guarantees column-level `validate` compatibility.

---

## Self-Review

**Spec coverage:**
- Data model (Permission + t_roles_permissions + Role.permissions) → Task 1 (entity/association) + Task 3 (tables).
- Authority choke point union → Task 1 Step 6 + `SecurityUserDetailsTest`.
- Seed catalog resource:action → Task 3 (Flyway + initdb).
- Convert 4 annotations → Task 2 Steps 4-5.
- Both role names + permissions in authorities → Task 1 (`authorities.add(role.getName())` kept).
- Testing (unit getAuthorities + MockMvc enforcement) → Task 1 + Task 2.
- Rollout additive/idempotent → Task 3 (CREATE IF NOT EXISTS / ON CONFLICT).
- Out of scope items → documented, no tasks. ✅ All spec sections covered.

**Placeholder scan:** No TBD/TODO. The one prose note (`userRole` typo guard) is an explicit instruction, not a placeholder. Docker-quirk note is a real contingency.

**Type consistency:** `Permissions.PROBLEM_CREATE` etc. are Strings matching the literal SpEL used in annotations and the seeded `name` values. `Role.getPermissions()/setPermissions(Set<Permission>)`, `Permission.getName()/setName()` used consistently across Tasks 1-3. Column names (`created_at`, `updated_at`, `created_by`, `updated_by`, `role_id`, `permission_id`) identical in entity mapping, V3, and initdb.
