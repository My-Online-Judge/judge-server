# Admin-UI Permission Groundwork Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Expose the current user's permissions on `GET /auth/me`, add read-only `GET /api/v1/permissions` and `GET /api/v1/roles` endpoints (gated by new `permission:read` / `role:read`), and add the frontend `permissions` type + `hasPermission()` store getter — the groundwork an admin UI will build on.

**Architecture:** Backend flattens role→permission names into a top-level `permissions` array on `UserResponse`; two new feature packages (`permission/`, `role/`) expose catalog reads gated by permission; a Flyway `V4` seeds the two new read permissions to ADMIN. Frontend gains the type field and a store getter. No admin pages/routes here.

**Tech Stack:** Spring Boot 3.4, Spring Security 6.4, MapStruct, Flyway, JUnit 5 + MockMvc + spring-security-test (H2 test profile); judge-portal Vue 3 + Pinia + Vitest.

## Global Constraints

- Backend base package `vn.thanhtuanle`; entities extend `BaseEntity` (UUID + audit). Authorities un-prefixed → `hasAuthority(...)`. Permission naming `resource:action`.
- Prior migration `V3` is released and immutable → new seed goes in `V4`.
- New permissions + UUIDs: `role:read` → `a1000000-0000-0000-0000-000000000005`; `permission:read` → `a1000000-0000-0000-0000-000000000006`. ADMIN role UUID `626c2558-a7d5-4fa4-9161-9bec2ae74076`.
- Backend tests: `cd judge-api && ./mvnw -q -Dspring.profiles.active=test -Dtest=ClassName test`.
- Frontend tests: `cd judge-portal && npm run test` (vitest) / `npm run type-check`.
- Read endpoints return the full small catalog (no pagination).

---

### Task 1: `/auth/me` exposes flat `permissions`

**Files:**
- Modify: `judge-api/src/main/java/vn/thanhtuanle/user/dto/UserResponse.java`
- Modify: `judge-api/src/main/java/vn/thanhtuanle/user/mapper/UserMapper.java`
- Modify: `judge-api/src/main/java/vn/thanhtuanle/entity/Role.java`
- Test: `judge-api/src/test/java/vn/thanhtuanle/user/mapper/UserMapperTest.java`

**Interfaces:**
- Produces: `UserResponse.getPermissions()/setPermissions(Set<String>)`; `UserMapper.toResponse(User)` populates `permissions` = distinct sorted permission names across the user's roles.

- [ ] **Step 1: Write the failing test** — `UserMapperTest.java`:

```java
package vn.thanhtuanle.user.mapper;

import org.junit.jupiter.api.Test;
import vn.thanhtuanle.entity.Permission;
import vn.thanhtuanle.entity.Role;
import vn.thanhtuanle.entity.User;
import vn.thanhtuanle.user.dto.UserResponse;

import java.util.List;
import java.util.Set;

import static org.junit.jupiter.api.Assertions.assertEquals;

class UserMapperTest {

    private final UserMapper mapper = new UserMapperImpl();

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

    @Test
    void flattensDistinctSortedPermissionNamesAcrossRoles() {
        User user = new User();
        user.setUsername("u@example.com");
        user.setRoles(Set.of(
                role("ADMIN", perm("problem:delete"), perm("problem:create")),
                role("EDITOR", perm("problem:create"))));

        UserResponse response = mapper.toResponse(user);

        assertEquals(List.of("problem:create", "problem:delete"),
                List.copyOf(response.getPermissions())); // sorted + distinct
    }

    @Test
    void emptyWhenRoleHasNoPermissions() {
        User user = new User();
        Role r = new Role();
        r.setName("USER");
        r.setPermissions(null);
        user.setRoles(Set.of(r));

        assertEquals(Set.of(), mapper.toResponse(user).getPermissions());
    }
}
```

- [ ] **Step 2: Run to verify it fails** — `./mvnw -q -Dspring.profiles.active=test -Dtest=UserMapperTest test`. Expected: COMPILE FAIL (`getPermissions` missing; `UserMapperImpl` lacks the mapping).

- [ ] **Step 3: Add `permissions` to `UserResponse`** — add the field after `roles` (line 28):

```java
    private Set<Role> roles;
    private Set<String> permissions;
```

- [ ] **Step 4: Flatten in `UserMapper`** — replace the file body with:

```java
package vn.thanhtuanle.user.mapper;

import org.mapstruct.Mapper;
import org.mapstruct.Mapping;
import org.mapstruct.Named;
import vn.thanhtuanle.entity.Permission;
import vn.thanhtuanle.entity.Role;
import vn.thanhtuanle.entity.User;
import vn.thanhtuanle.user.dto.UserResponse;

import java.util.Set;
import java.util.TreeSet;
import java.util.stream.Collectors;

@Mapper(componentModel = "spring")
public interface UserMapper {

    @Mapping(target = "permissions", source = "roles", qualifiedByName = "rolesToPermissionNames")
    UserResponse toResponse(User user);

    @Named("rolesToPermissionNames")
    default Set<String> rolesToPermissionNames(Set<Role> roles) {
        if (roles == null) {
            return Set.of();
        }
        return roles.stream()
                .filter(role -> role.getPermissions() != null)
                .flatMap(role -> role.getPermissions().stream())
                .map(Permission::getName)
                .collect(Collectors.toCollection(TreeSet::new));
    }
}
```

- [ ] **Step 5: `@JsonIgnore` the entity's nested permissions** — in `Role.java`, add the import and annotate the field so `/me` `roles[]` stays clean:

```java
import com.fasterxml.jackson.annotation.JsonIgnore;
```
```java
    @JsonIgnore
    @ManyToMany(fetch = FetchType.EAGER)
    @JoinTable(name = "t_roles_permissions",
            joinColumns = @JoinColumn(name = "role_id"),
            inverseJoinColumns = @JoinColumn(name = "permission_id"))
    private Set<Permission> permissions;
```

- [ ] **Step 6: Run to verify it passes** — `./mvnw -q -Dspring.profiles.active=test -Dtest=UserMapperTest test`. Expected: PASS (2).

- [ ] **Step 7: Commit**

```bash
cd judge-api
git add src/main/java/vn/thanhtuanle/user/dto/UserResponse.java \
        src/main/java/vn/thanhtuanle/user/mapper/UserMapper.java \
        src/main/java/vn/thanhtuanle/entity/Role.java \
        src/test/java/vn/thanhtuanle/user/mapper/UserMapperTest.java
git commit -m "feat: expose flat permissions on /auth/me"
```

---

### Task 2: `GET /api/v1/permissions`

**Files:**
- Create: `judge-api/src/main/java/vn/thanhtuanle/permission/PermissionRepository.java`
- Create: `judge-api/src/main/java/vn/thanhtuanle/permission/dto/PermissionResponse.java`
- Create: `judge-api/src/main/java/vn/thanhtuanle/permission/PermissionMapper.java`
- Create: `judge-api/src/main/java/vn/thanhtuanle/permission/PermissionService.java`
- Create: `judge-api/src/main/java/vn/thanhtuanle/permission/PermissionController.java`
- Modify: `judge-api/src/main/java/vn/thanhtuanle/common/constant/Routes.java`
- Test: `judge-api/src/test/java/vn/thanhtuanle/permission/PermissionControllerSecurityTest.java`

**Interfaces:**
- Produces: `GET /api/v1/permissions` → `ApiResponse<List<PermissionResponse>>`, gated by `permission:read`. `PermissionService.listPermissions()`.

- [ ] **Step 1: Add the route constant** — in `Routes.java` after `JUDGE_SERVERS`:

```java
    public static final String JUDGE_SERVERS = BASE_API + "/judge-servers";
    public static final String PERMISSIONS = BASE_API + "/permissions";
    public static final String ROLES = BASE_API + "/roles";
```

- [ ] **Step 2: Repository, DTO, mapper, service** — create:

`PermissionRepository.java`:
```java
package vn.thanhtuanle.permission;

import org.springframework.data.jpa.repository.JpaRepository;
import vn.thanhtuanle.entity.Permission;

import java.util.UUID;

public interface PermissionRepository extends JpaRepository<Permission, UUID> {
}
```

`dto/PermissionResponse.java`:
```java
package vn.thanhtuanle.permission.dto;

import lombok.Builder;
import lombok.Data;

import java.util.UUID;

@Data
@Builder
public class PermissionResponse {
    private UUID id;
    private String name;
    private String description;
}
```

`PermissionMapper.java`:
```java
package vn.thanhtuanle.permission;

import org.mapstruct.Mapper;
import vn.thanhtuanle.entity.Permission;
import vn.thanhtuanle.permission.dto.PermissionResponse;

import java.util.List;

@Mapper(componentModel = "spring")
public interface PermissionMapper {
    PermissionResponse toResponse(Permission permission);

    List<PermissionResponse> toResponses(List<Permission> permissions);
}
```

`PermissionService.java`:
```java
package vn.thanhtuanle.permission;

import lombok.RequiredArgsConstructor;
import org.springframework.data.domain.Sort;
import org.springframework.stereotype.Service;
import vn.thanhtuanle.permission.dto.PermissionResponse;

import java.util.List;

@Service
@RequiredArgsConstructor
public class PermissionService {

    private final PermissionRepository permissionRepository;
    private final PermissionMapper permissionMapper;

    public List<PermissionResponse> listPermissions() {
        return permissionMapper.toResponses(permissionRepository.findAll(Sort.by("name")));
    }
}
```

- [ ] **Step 3: Write the failing security test** — `PermissionControllerSecurityTest.java`:

```java
package vn.thanhtuanle.permission;

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

import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.get;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.status;

@WebMvcTest(controllers = PermissionController.class)
@Import({SecurityConfig.class, JwtAuthenticationFilter.class,
        CustomAuthenticationEntryPoint.class, CustomAccessDeniedHandler.class})
@ActiveProfiles("test")
class PermissionControllerSecurityTest {

    @Autowired
    private MockMvc mockMvc;

    @MockBean
    private PermissionService permissionService;
    @MockBean
    private JwtUtil jwtUtil;
    @MockBean
    private UserDetailsService userDetailsService;

    @Test
    @WithMockUser(authorities = "permission:read")
    void allowed_withPermissionRead() throws Exception {
        mockMvc.perform(get("/api/v1/permissions")).andExpect(status().isOk());
    }

    @Test
    @WithMockUser(authorities = "ADMIN")
    void forbidden_withOnlyAdminAuthority() throws Exception {
        mockMvc.perform(get("/api/v1/permissions")).andExpect(status().isForbidden());
    }

    @Test
    @WithMockUser(authorities = "role:read")
    void forbidden_withWrongPermission() throws Exception {
        mockMvc.perform(get("/api/v1/permissions")).andExpect(status().isForbidden());
    }
}
```

- [ ] **Step 4: Run to verify it fails** — `./mvnw -q -Dspring.profiles.active=test -Dtest=PermissionControllerSecurityTest test`. Expected: COMPILE FAIL (`PermissionController` does not exist).

- [ ] **Step 5: Create `PermissionController`**:

```java
package vn.thanhtuanle.permission;

import io.swagger.v3.oas.annotations.Operation;
import lombok.RequiredArgsConstructor;
import org.springframework.security.access.prepost.PreAuthorize;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.RestController;

import vn.thanhtuanle.common.constant.Routes;
import vn.thanhtuanle.common.payload.ApiResponse;
import vn.thanhtuanle.permission.dto.PermissionResponse;

import java.util.List;

@RestController
@RequiredArgsConstructor
public class PermissionController {

    private final PermissionService permissionService;

    @Operation(summary = "List all permissions (admin)")
    @GetMapping(Routes.PERMISSIONS)
    @PreAuthorize("hasAuthority('permission:read')")
    public ApiResponse<List<PermissionResponse>> listPermissions() {
        return ApiResponse.success(permissionService.listPermissions());
    }
}
```

- [ ] **Step 6: Run to verify it passes** — `-Dtest=PermissionControllerSecurityTest`. Expected: PASS (3).

- [ ] **Step 7: Commit**

```bash
cd judge-api
git add src/main/java/vn/thanhtuanle/permission/ \
        src/main/java/vn/thanhtuanle/common/constant/Routes.java \
        src/test/java/vn/thanhtuanle/permission/PermissionControllerSecurityTest.java
git commit -m "feat: GET /api/v1/permissions gated by permission:read"
```

---

### Task 3: `GET /api/v1/roles` (with each role's permission names)

**Files:**
- Create: `judge-api/src/main/java/vn/thanhtuanle/role/dto/RoleResponse.java`
- Create: `judge-api/src/main/java/vn/thanhtuanle/role/RoleMapper.java`
- Create: `judge-api/src/main/java/vn/thanhtuanle/role/RoleService.java`
- Create: `judge-api/src/main/java/vn/thanhtuanle/role/RoleController.java`
- Test: `judge-api/src/test/java/vn/thanhtuanle/role/RoleMapperTest.java`
- Test: `judge-api/src/test/java/vn/thanhtuanle/role/RoleControllerSecurityTest.java`

**Interfaces:**
- Consumes: `Routes.ROLES` (Task 2); `user.RoleRepository`.
- Produces: `GET /api/v1/roles` → `ApiResponse<List<RoleResponse>>`, gated by `role:read`. `RoleResponse` = `{id,name,description,permissions:Set<String>}`.

- [ ] **Step 1: Write the failing mapper test** — `RoleMapperTest.java`:

```java
package vn.thanhtuanle.role;

import org.junit.jupiter.api.Test;
import vn.thanhtuanle.entity.Permission;
import vn.thanhtuanle.entity.Role;
import vn.thanhtuanle.role.dto.RoleResponse;

import java.util.List;
import java.util.Set;

import static org.junit.jupiter.api.Assertions.assertEquals;

class RoleMapperTest {

    private final RoleMapper mapper = new RoleMapperImpl();

    private Permission perm(String name) {
        Permission p = new Permission();
        p.setName(name);
        return p;
    }

    @Test
    void mapsRoleWithSortedPermissionNames() {
        Role role = new Role();
        role.setName("ADMIN");
        role.setDescription("Admin role");
        role.setPermissions(Set.of(perm("problem:delete"), perm("problem:create")));

        RoleResponse response = mapper.toResponse(role);

        assertEquals("ADMIN", response.getName());
        assertEquals(List.of("problem:create", "problem:delete"),
                List.copyOf(response.getPermissions()));
    }

    @Test
    void emptyPermissionsWhenNull() {
        Role role = new Role();
        role.setName("USER");
        role.setPermissions(null);

        assertEquals(Set.of(), mapper.toResponse(role).getPermissions());
    }
}
```

- [ ] **Step 2: Run to verify it fails** — `-Dtest=RoleMapperTest`. Expected: COMPILE FAIL.

- [ ] **Step 3: DTO + mapper + service** — create:

`dto/RoleResponse.java`:
```java
package vn.thanhtuanle.role.dto;

import lombok.Builder;
import lombok.Data;

import java.util.Set;
import java.util.UUID;

@Data
@Builder
public class RoleResponse {
    private UUID id;
    private String name;
    private String description;
    private Set<String> permissions;
}
```

`RoleMapper.java`:
```java
package vn.thanhtuanle.role;

import org.mapstruct.Mapper;
import org.mapstruct.Mapping;
import org.mapstruct.Named;
import vn.thanhtuanle.entity.Permission;
import vn.thanhtuanle.entity.Role;
import vn.thanhtuanle.role.dto.RoleResponse;

import java.util.List;
import java.util.Set;
import java.util.TreeSet;
import java.util.stream.Collectors;

@Mapper(componentModel = "spring")
public interface RoleMapper {

    @Mapping(target = "permissions", source = "permissions", qualifiedByName = "permissionsToNames")
    RoleResponse toResponse(Role role);

    List<RoleResponse> toResponses(List<Role> roles);

    @Named("permissionsToNames")
    default Set<String> permissionsToNames(Set<Permission> permissions) {
        if (permissions == null) {
            return Set.of();
        }
        return permissions.stream()
                .map(Permission::getName)
                .collect(Collectors.toCollection(TreeSet::new));
    }
}
```

`RoleService.java`:
```java
package vn.thanhtuanle.role;

import lombok.RequiredArgsConstructor;
import org.springframework.data.domain.Sort;
import org.springframework.stereotype.Service;
import vn.thanhtuanle.role.dto.RoleResponse;
import vn.thanhtuanle.user.RoleRepository;

import java.util.List;

@Service
@RequiredArgsConstructor
public class RoleService {

    private final RoleRepository roleRepository;
    private final RoleMapper roleMapper;

    public List<RoleResponse> listRoles() {
        return roleMapper.toResponses(roleRepository.findAll(Sort.by("name")));
    }
}
```

- [ ] **Step 4: Run mapper test to verify it passes** — `-Dtest=RoleMapperTest`. Expected: PASS (2).

- [ ] **Step 5: Write the failing controller security test** — `RoleControllerSecurityTest.java`:

```java
package vn.thanhtuanle.role;

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

import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.get;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.status;

@WebMvcTest(controllers = RoleController.class)
@Import({SecurityConfig.class, JwtAuthenticationFilter.class,
        CustomAuthenticationEntryPoint.class, CustomAccessDeniedHandler.class})
@ActiveProfiles("test")
class RoleControllerSecurityTest {

    @Autowired
    private MockMvc mockMvc;

    @MockBean
    private RoleService roleService;
    @MockBean
    private JwtUtil jwtUtil;
    @MockBean
    private UserDetailsService userDetailsService;

    @Test
    @WithMockUser(authorities = "role:read")
    void allowed_withRoleRead() throws Exception {
        mockMvc.perform(get("/api/v1/roles")).andExpect(status().isOk());
    }

    @Test
    @WithMockUser(authorities = "ADMIN")
    void forbidden_withOnlyAdminAuthority() throws Exception {
        mockMvc.perform(get("/api/v1/roles")).andExpect(status().isForbidden());
    }

    @Test
    @WithMockUser(authorities = "permission:read")
    void forbidden_withWrongPermission() throws Exception {
        mockMvc.perform(get("/api/v1/roles")).andExpect(status().isForbidden());
    }
}
```

- [ ] **Step 6: Run to verify it fails** — `-Dtest=RoleControllerSecurityTest`. Expected: COMPILE FAIL (`RoleController` missing).

- [ ] **Step 7: Create `RoleController`**:

```java
package vn.thanhtuanle.role;

import io.swagger.v3.oas.annotations.Operation;
import lombok.RequiredArgsConstructor;
import org.springframework.security.access.prepost.PreAuthorize;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.RestController;

import vn.thanhtuanle.common.constant.Routes;
import vn.thanhtuanle.common.payload.ApiResponse;
import vn.thanhtuanle.role.dto.RoleResponse;

import java.util.List;

@RestController
@RequiredArgsConstructor
public class RoleController {

    private final RoleService roleService;

    @Operation(summary = "List all roles with their permissions (admin)")
    @GetMapping(Routes.ROLES)
    @PreAuthorize("hasAuthority('role:read')")
    public ApiResponse<List<RoleResponse>> listRoles() {
        return ApiResponse.success(roleService.listRoles());
    }
}
```

- [ ] **Step 8: Run both tests to verify they pass** — `-Dtest=RoleMapperTest,RoleControllerSecurityTest`. Expected: PASS (5).

- [ ] **Step 9: Commit**

```bash
cd judge-api
git add src/main/java/vn/thanhtuanle/role/ \
        src/test/java/vn/thanhtuanle/role/
git commit -m "feat: GET /api/v1/roles with permission names, gated by role:read"
```

---

### Task 4: Seed `role:read` + `permission:read` (Flyway V4 + initdb)

**Files:**
- Create: `judge-api/src/main/resources/db/migration/V4__seed_read_permissions.sql`
- Modify: `judge-api/initdb.sql`

- [ ] **Step 1: Create the migration** — `V4__seed_read_permissions.sql`:

```sql
-- V4: read-catalog permissions for the admin-UI groundwork. Additive + idempotent.

INSERT INTO public.t_permissions (id, created_at, created_by, updated_at, updated_by, description, name) VALUES
    ('a1000000-0000-0000-0000-000000000005', '2026-07-16 00:00:00', 'SYS', '2026-07-16 00:00:00', 'SYS', 'List roles', 'role:read'),
    ('a1000000-0000-0000-0000-000000000006', '2026-07-16 00:00:00', 'SYS', '2026-07-16 00:00:00', 'SYS', 'List permissions', 'permission:read')
ON CONFLICT (id) DO NOTHING;

INSERT INTO public.t_roles_permissions (role_id, permission_id) VALUES
    ('626c2558-a7d5-4fa4-9161-9bec2ae74076', 'a1000000-0000-0000-0000-000000000005'),
    ('626c2558-a7d5-4fa4-9161-9bec2ae74076', 'a1000000-0000-0000-0000-000000000006')
ON CONFLICT (role_id, permission_id) DO NOTHING;
```

- [ ] **Step 2: Append the same to `initdb.sql`** (at the end, after the V3 block added earlier):

```sql

--
-- role:read / permission:read (admin-UI groundwork); added 2026-07-16
--

INSERT INTO public.t_permissions (id, created_at, created_by, updated_at, updated_by, description, name) VALUES
    ('a1000000-0000-0000-0000-000000000005', '2026-07-16 00:00:00', 'SYS', '2026-07-16 00:00:00', 'SYS', 'List roles', 'role:read'),
    ('a1000000-0000-0000-0000-000000000006', '2026-07-16 00:00:00', 'SYS', '2026-07-16 00:00:00', 'SYS', 'List permissions', 'permission:read')
ON CONFLICT (id) DO NOTHING;

INSERT INTO public.t_roles_permissions (role_id, permission_id) VALUES
    ('626c2558-a7d5-4fa4-9161-9bec2ae74076', 'a1000000-0000-0000-0000-000000000005'),
    ('626c2558-a7d5-4fa4-9161-9bec2ae74076', 'a1000000-0000-0000-0000-000000000006')
ON CONFLICT (role_id, permission_id) DO NOTHING;
```

- [ ] **Step 3: Verify against a disposable Postgres** (existing DB already seeded via V3 + idempotent re-run):

```bash
cd judge-api
docker run -d --name oj-v4-check -e POSTGRES_PASSWORD=pass postgres:16
# wait for readiness (poll pg_isready), then:
docker exec -i oj-v4-check psql -q -U postgres -d postgres < initdb.sql            # full fresh schema incl. V3+V4 seed
docker exec -i oj-v4-check psql -v ON_ERROR_STOP=1 -U postgres -d postgres < src/main/resources/db/migration/V4__seed_read_permissions.sql  # no-op, must not error
docker exec oj-v4-check psql -U postgres -d postgres -c \
  "SELECT p.name FROM t_roles_permissions rp JOIN t_roles r ON r.id=rp.role_id JOIN t_permissions p ON p.id=rp.permission_id WHERE r.name='ADMIN' ORDER BY 1;"
docker rm -f oj-v4-check
```
Expected: V4 re-run exits 0; ADMIN now maps to all 6 permissions (`judgeserver:read`, `permission:read`, `problem:create`, `problem:delete`, `problem:update`, `role:read`).

- [ ] **Step 4: Commit**

```bash
cd judge-api
git add src/main/resources/db/migration/V4__seed_read_permissions.sql initdb.sql
git commit -m "feat: seed role:read + permission:read to ADMIN (Flyway V4 + initdb)"
```

---

### Task 5: Frontend seam — `permissions` type + `hasPermission()` store getter

**Files:**
- Modify: `judge-portal/src/types/user.ts`
- Modify: `judge-portal/src/stores/auth.ts`
- Test: `judge-portal/src/stores/__tests__/auth.spec.ts`

**Interfaces:**
- Produces: `UserResponse.permissions: string[]`; `authStore.hasPermission(perm: string): boolean`.

- [ ] **Step 1: Add `permissions` to the user type** — in `types/user.ts`, add to `UserResponse` after `roles`:

```java
    roles: Role[]
    permissions: string[]
```

- [ ] **Step 2: Write the failing store test** — `src/stores/__tests__/auth.spec.ts`:

```ts
import { describe, it, expect, beforeEach, vi } from 'vitest'
import { setActivePinia, createPinia } from 'pinia'

vi.mock('@/services/authService', () => ({
  default: { getMe: vi.fn(), logout: vi.fn() },
}))

import { useAuthStore } from '@/stores/auth'
import type { UserResponse } from '@/types/user'

const userWith = (permissions: string[]) =>
  ({ username: 'u@example.com', permissions } as unknown as UserResponse)

describe('auth store hasPermission', () => {
  beforeEach(() => setActivePinia(createPinia()))

  it('is false when unauthenticated', () => {
    const store = useAuthStore()
    expect(store.hasPermission('problem:create')).toBe(false)
  })

  it('reflects the logged-in user permissions', () => {
    const store = useAuthStore()
    store.login(userWith(['problem:create', 'problem:delete']))
    expect(store.hasPermission('problem:create')).toBe(true)
    expect(store.hasPermission('role:read')).toBe(false)
  })
})
```

- [ ] **Step 3: Run to verify it fails** — `cd judge-portal && npx vitest run src/stores/__tests__/auth.spec.ts`. Expected: FAIL (`hasPermission` is not a function).

- [ ] **Step 4: Add `hasPermission` to the store** — in `stores/auth.ts`, add the getter before the `return` and export it:

```ts
    const hasPermission = (permission: string): boolean => {
        return !!user.value?.permissions?.includes(permission)
    }

    return {
        user,
        isAuthenticated,
        login,
        logout,
        fetchUser,
        hasPermission
    }
```

- [ ] **Step 5: Run to verify it passes** — `npx vitest run src/stores/__tests__/auth.spec.ts`. Expected: PASS (2).

- [ ] **Step 6: Type-check** — `npm run type-check`. Expected: no errors. (If other code constructs a `UserResponse` literal without `permissions`, mark it optional as `permissions?: string[]` instead — but the login flow only ever receives it from the API.)

- [ ] **Step 7: Commit**

```bash
cd judge-portal
git add src/types/user.ts src/stores/auth.ts src/stores/__tests__/auth.spec.ts
git commit -m "feat: expose permissions + hasPermission() in auth store"
```

---

### Task 6: Full regression

- [ ] **Step 1: Backend suite** — `cd judge-api && ./mvnw -q -Dspring.profiles.active=test test`. Expected: only the known offline `BackendServiceApplicationTests.contextLoads` fails (no `DATABASE_URL`); all others pass, including the new mapper + controller tests.

- [ ] **Step 2: Frontend suite + type-check** — `cd judge-portal && npm run test && npm run type-check`. Expected: all pass.

- [ ] **Step 3: Record acceptance** — confirm: `/me` returns flat `permissions` and clean `roles[]`; `GET /permissions` and `GET /roles` return 200 with the read permission and 403 without; ADMIN seeded with all 6 permissions; `authStore.hasPermission` works. Note deferred: admin UI pages + write/CRUD APIs.

---

## Self-Review

**Spec coverage:** `/me` permissions → Task 1. `GET /permissions` → Task 2. `GET /roles` → Task 3. New gate permissions + V4 seed → Task 4. FE type + `hasPermission` → Task 5. Tests → Tasks 1-3, 5; regression → Task 6. All spec sections mapped.

**Placeholder scan:** none. The type-check contingency in Task 5 Step 6 is an explicit fallback, not a placeholder.

**Type consistency:** `permissions:Set<String>` (backend DTOs) ↔ `string[]` (FE). `Routes.PERMISSIONS`/`Routes.ROLES` defined in Task 2 Step 1, used in Tasks 2-3 controllers. `RoleMapperImpl`/`UserMapperImpl` are MapStruct-generated from the `@Mapper` interfaces. Permission UUIDs `...005`/`...006` identical in V4 and initdb. `hasPermission` name identical in store + test.
