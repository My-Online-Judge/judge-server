# Admin Users CRUD + SYS_ROOT Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the `/admin/users` feature — full CRUD over user accounts (list/filter, create, edit, reset password, enable/disable, soft-delete, manage roles) with a protected `SYS_ROOT` super-user.

**Architecture:** New `UserController` + `UserService` in `judge-api` mirroring the existing `RoleController`/`RoleService` conventions; a Flyway `V9` migration seeds `user:*` permissions and the `SYS_ROOT` role; the Vue `AdminUsersPage` mirrors `AdminRolesPage`. Backend enforces guardrails (SYS_ROOT immutable, no privilege escalation, no self-lockout) in the service layer.

**Tech Stack:** Spring Boot 3, Flyway, MapStruct, JUnit 5 + Mockito; Vue 3 + shadcn-vue + Pinia + Vitest.

**Spec:** `docs/superpowers/specs/2026-07-19-admin-users-crud-design.md`

## Global Constraints

- No Jakarta Bean Validation provider on the classpath → `@Valid` is a no-op; **all validation is explicit in the service**.
- New seed rows go in a **new** `Vn` migration with `ON CONFLICT DO NOTHING` (never edit an applied migration). `ddl-auto: validate` — no entity schema changes here (`status` stays `Integer`).
- Fixed UUIDs: permissions `a1000000-0000-0000-0000-00000000000a..0d`; `SYS_ROOT` role `a2000000-0000-0000-0000-000000000001`; ADMIN role `626c2558-a7d5-4fa4-9161-9bec2ae74076`; root user `d0000000-0000-0000-0000-000000000001`.
- `UserStatus`: `ACTIVE(1)`, `DISABLED(0)`, `DELETED(2)`. `CommonStatus.ACTIVE.getValue() == 1`.
- Error responses via `throw new AppException(ErrorCode.X)`. Success via `ApiResponse.success(data)` / `ApiResponse.created(data)` / `ApiResponse.success()`.
- Reuse `UserResponse` (`user/dto/UserResponse.java`) + `UserMapper.toResponse` + `PageResponse.of(Page)`.
- Backend tests are pure Mockito unit tests (pattern: `RoleServiceCreateTest`). Repository JPQL + controller wiring are verified on the live stack (final task).
- Branches: `feature/admin-users-crud` off freshly-synced `main` in **both** `judge-api` and `judge-portal`. Frequent commits.

---

# Part A — Backend (`judge-api`)

### Task A1: Foundations (routes, error codes, status enum, SYS_ROOT protection)

**Files:**
- Modify: `src/main/java/vn/thanhtuanle/common/constant/Routes.java`
- Modify: `src/main/java/vn/thanhtuanle/common/exception/ErrorCode.java`
- Create: `src/main/java/vn/thanhtuanle/common/enums/UserStatus.java`
- Modify: `src/main/java/vn/thanhtuanle/role/RoleService.java:28`
- Test: `src/test/java/vn/thanhtuanle/role/RoleServiceDeleteTest.java` (add SYS_ROOT case)

**Interfaces produced:**
- `Routes.USERS` (`= BASE_API + "/users"`)
- `UserStatus{ ACTIVE(1), DISABLED(0), DELETED(2); int getValue(); static UserStatus fromValue(int) }`
- `ErrorCode`: `USER_NOT_FOUND`, `USERNAME_EXISTED`, `EMAIL_EXISTED`, `USER_PROTECTED`, `ROLE_NOT_ASSIGNABLE`, `USER_SELF_MODIFY`, `USERNAME_INVALID`, `PASSWORD_INVALID`, `EMAIL_INVALID`

- [ ] **Step 1:** Add `public static final String USERS = BASE_API + "/users";` to `Routes` (next to `ROLES`).
- [ ] **Step 2:** Add to `ErrorCode` (before `UNCATEGORIZED_EXCEPTION`):
```java
USER_NOT_FOUND("User not found", HttpStatus.NOT_FOUND),
USERNAME_EXISTED("Username already exists", HttpStatus.CONFLICT),
EMAIL_EXISTED("Email already exists", HttpStatus.CONFLICT),
USER_PROTECTED("This is a protected system account and cannot be modified", HttpStatus.CONFLICT),
ROLE_NOT_ASSIGNABLE("This role cannot be assigned to a user", HttpStatus.CONFLICT),
USER_SELF_MODIFY("You cannot change your own roles, status, or delete your own account", HttpStatus.CONFLICT),
USERNAME_INVALID("Username must be 3-32 chars: letters, digits, or underscore", HttpStatus.BAD_REQUEST),
PASSWORD_INVALID("Password must be at least 8 characters and include letters and digits", HttpStatus.BAD_REQUEST),
EMAIL_INVALID("Email is not valid", HttpStatus.BAD_REQUEST),
```
- [ ] **Step 3:** Create `UserStatus`:
```java
package vn.thanhtuanle.common.enums;

public enum UserStatus {
    ACTIVE(1), DISABLED(0), DELETED(2);
    private final int value;
    UserStatus(int value) { this.value = value; }
    public int getValue() { return value; }
    public static UserStatus fromValue(int value) {
        for (UserStatus s : values()) if (s.value == value) return s;
        throw new IllegalArgumentException("Invalid UserStatus value: " + value);
    }
}
```
- [ ] **Step 4:** In `RoleService`, change `SYSTEM_ROLES` to `Set.of("ADMIN", "USER", "SYS_ROOT")`.
- [ ] **Step 5:** Add a test in `RoleServiceDeleteTest`: deleting a role named `SYS_ROOT` throws `AppException` with `ErrorCode.ROLE_PROTECTED` (mock `roleRepository.findById` → a `Role` named "SYS_ROOT"). Run `./gradlew test --tests '*RoleServiceDeleteTest'` → PASS.
- [ ] **Step 6:** Commit `feat(users): routes, error codes, UserStatus, protect SYS_ROOT role`.

### Task A2: Flyway V9 migration

**Files:**
- Create: `src/main/resources/db/migration/V9__seed_user_permissions_and_sys_root.sql`

- [ ] **Step 1:** Write the migration (additive + idempotent, mirrors V8/V6):
```sql
-- V9: user:* permissions + SYS_ROOT super-user role. Additive + idempotent.

INSERT INTO public.t_permissions (id, created_at, created_by, updated_at, updated_by, description, name) VALUES
    ('a1000000-0000-0000-0000-00000000000a', '2026-07-19 00:00:00', 'SYS', '2026-07-19 00:00:00', 'SYS', 'List and view users', 'user:read'),
    ('a1000000-0000-0000-0000-00000000000b', '2026-07-19 00:00:00', 'SYS', '2026-07-19 00:00:00', 'SYS', 'Create a user', 'user:create'),
    ('a1000000-0000-0000-0000-00000000000c', '2026-07-19 00:00:00', 'SYS', '2026-07-19 00:00:00', 'SYS', 'Modify a user (profile, status, roles, password)', 'user:update'),
    ('a1000000-0000-0000-0000-00000000000d', '2026-07-19 00:00:00', 'SYS', '2026-07-19 00:00:00', 'SYS', 'Delete a user', 'user:delete')
ON CONFLICT (id) DO NOTHING;

-- Grant user:* to ADMIN.
INSERT INTO public.t_roles_permissions (role_id, permission_id) VALUES
    ('626c2558-a7d5-4fa4-9161-9bec2ae74076', 'a1000000-0000-0000-0000-00000000000a'),
    ('626c2558-a7d5-4fa4-9161-9bec2ae74076', 'a1000000-0000-0000-0000-00000000000b'),
    ('626c2558-a7d5-4fa4-9161-9bec2ae74076', 'a1000000-0000-0000-0000-00000000000c'),
    ('626c2558-a7d5-4fa4-9161-9bec2ae74076', 'a1000000-0000-0000-0000-00000000000d')
ON CONFLICT (role_id, permission_id) DO NOTHING;

-- SYS_ROOT role.
INSERT INTO public.t_roles (id, created_at, created_by, updated_at, updated_by, description, name) VALUES
    ('a2000000-0000-0000-0000-000000000001', '2026-07-19 00:00:00', 'SYS', '2026-07-19 00:00:00', 'SYS', 'System super-user; immutable via the admin API', 'SYS_ROOT')
ON CONFLICT (id) DO NOTHING;

-- SYS_ROOT gets EVERY permission in the catalog (present + all seeded before now).
INSERT INTO public.t_roles_permissions (role_id, permission_id)
SELECT 'a2000000-0000-0000-0000-000000000001', p.id
FROM public.t_permissions p
ON CONFLICT (role_id, permission_id) DO NOTHING;

-- Assign SYS_ROOT to the seeded root user (keeps its existing ADMIN grant).
INSERT INTO public.t_users_roles (user_id, role_id)
VALUES ('d0000000-0000-0000-0000-000000000001', 'a2000000-0000-0000-0000-000000000001')
ON CONFLICT (user_id, role_id) DO NOTHING;
```
- [ ] **Step 2:** Verify the `t_roles` / `t_permissions` column names match V6/V8 (they do: `id, created_at, created_by, updated_at, updated_by, description, name`). No run yet (verified at boot in A12).
- [ ] **Step 3:** Commit `feat(users): V9 seed user permissions + SYS_ROOT role`.

### Task A3: Request DTOs

**Files (all Create, package `vn.thanhtuanle.user.dto`):** `CreateUserRequest.java`, `UpdateUserRequest.java`, `UpdateUserStatusRequest.java`, `UpdateUserRolesRequest.java`, `ResetPasswordRequest.java`

**Interfaces produced:**
- `CreateUserRequest{ String username, name, email, password; Integer status; Set<UUID> roleIds }`
- `UpdateUserRequest{ String name, email; Integer status }`
- `UpdateUserStatusRequest{ Integer status }`
- `UpdateUserRolesRequest{ Set<UUID> roleIds }`
- `ResetPasswordRequest{ String newPassword }`

- [ ] **Step 1:** Create each as a `@Data` class (mirror `CreateRoleRequest`). Example:
```java
package vn.thanhtuanle.user.dto;
import lombok.Data;
import java.util.Set;
import java.util.UUID;

@Data
public class CreateUserRequest {
    private String username;
    private String name;
    private String email;
    private String password;
    private Integer status;      // optional; defaults to ACTIVE
    private Set<UUID> roleIds;    // optional
}
```
(Repeat the pattern for the other four with the fields listed above.)
- [ ] **Step 2:** Compile `./gradlew compileJava` → PASS. Commit `feat(users): request DTOs`.

### Task A4: UserRepository finders

**Files:**
- Modify: `src/main/java/vn/thanhtuanle/user/UserRepository.java`

**Interfaces produced:**
- `Page<User> search(String search, Integer status, UUID roleId, LocalDateTime createdFrom, LocalDateTime createdTo, Pageable pageable)`
- `boolean existsByEmail(String email)`
- `boolean existsByEmailAndIdNot(String email, UUID id)`

- [ ] **Step 1:** Add methods + the JPQL query. `search` param already lower-cased by caller; exclude `DELETED` (2) unless `status` is given:
```java
@Query("""
    SELECT u FROM User u WHERE
      (:search IS NULL OR LOWER(u.username) LIKE %:search% OR LOWER(u.email) LIKE %:search% OR LOWER(u.name) LIKE %:search%)
      AND (:status IS NULL OR u.status = :status)
      AND (:status IS NOT NULL OR u.status <> 2)
      AND (:roleId IS NULL OR EXISTS (SELECT 1 FROM u.roles r WHERE r.id = :roleId))
      AND (:createdFrom IS NULL OR u.createdAt >= :createdFrom)
      AND (:createdTo IS NULL OR u.createdAt < :createdTo)
    """)
Page<User> search(@Param("search") String search, @Param("status") Integer status,
                  @Param("roleId") UUID roleId, @Param("createdFrom") LocalDateTime createdFrom,
                  @Param("createdTo") LocalDateTime createdTo, Pageable pageable);

boolean existsByEmail(String email);
boolean existsByEmailAndIdNot(String email, UUID id);
```
(Add imports: `Query`, `Param`, `Page`, `Pageable`, `LocalDateTime`, `UUID`.)
- [ ] **Step 2:** `./gradlew compileJava` → PASS. Commit `feat(users): repository search + exists finders`.

### Task A5: UserService.create + validation

**Files:**
- Modify: `src/main/java/vn/thanhtuanle/user/UserService.java` (inject `RoleRepository roleRepository`, `PasswordEncoder passwordEncoder`, `UserMapper userMapper`)
- Test: `src/test/java/vn/thanhtuanle/user/UserServiceCreateTest.java`

**Interfaces produced:**
- `UserResponse create(CreateUserRequest req)`
- private helpers `validateUsername/validateEmail/validatePassword`, `resolveRoles(Set<UUID>)`, guard `assertNotSysRoot(Set<UUID>)`

- [ ] **Step 1 (test first):** `UserServiceCreateTest` with `@Mock UserRepository/RoleRepository/UserMapper`, `@Mock PasswordEncoder`, `@InjectMocks UserService`. Cases: valid create saves user with encoded password + ACTIVE default + resolved roles; blank/invalid username → `USERNAME_INVALID`; duplicate username (`existsByUsername`→true) → `USERNAME_EXISTED`; invalid email → `EMAIL_INVALID`; duplicate email → `EMAIL_EXISTED`; short password → `PASSWORD_INVALID`; `roleIds` containing SYS_ROOT id → `ROLE_NOT_ASSIGNABLE`; unknown roleId → `ROLE_NOT_EXISTED`.
- [ ] **Step 2:** Run → FAIL (method missing).
- [ ] **Step 3:** Implement. Patterns:
```java
private static final Pattern USERNAME = Pattern.compile("^[a-zA-Z0-9_]{3,32}$");
private static final Pattern EMAIL = Pattern.compile("^[^@\\s]+@[^@\\s]+\\.[^@\\s]+$");
private static final String SYS_ROOT = "SYS_ROOT";

@Transactional
public UserResponse create(CreateUserRequest req) {
    String username = req.getUsername() == null ? "" : req.getUsername().trim();
    if (!USERNAME.matcher(username).matches()) throw new AppException(ErrorCode.USERNAME_INVALID);
    if (Boolean.TRUE.equals(userRepository.existsByUsername(username))) throw new AppException(ErrorCode.USERNAME_EXISTED);
    String email = req.getEmail() == null ? "" : req.getEmail().trim();
    if (!EMAIL.matcher(email).matches()) throw new AppException(ErrorCode.EMAIL_INVALID);
    if (userRepository.existsByEmail(email)) throw new AppException(ErrorCode.EMAIL_EXISTED);
    String pwd = req.getPassword() == null ? "" : req.getPassword();
    if (pwd.length() < 8 || !pwd.matches(".*[a-zA-Z].*") || !pwd.matches(".*\\d.*")) throw new AppException(ErrorCode.PASSWORD_INVALID);

    Set<Role> roles = resolveRoles(req.getRoleIds());
    int status = req.getStatus() == null ? UserStatus.ACTIVE.getValue() : UserStatus.fromValue(req.getStatus()).getValue();
    User user = User.builder()
        .username(username).name(req.getName() == null ? null : req.getName().trim())
        .email(email).password(passwordEncoder.encode(pwd))
        .status(status).enabledMfa(false).roles(roles).build();
    userRepository.save(user);
    return userMapper.toResponse(user);
}

private Set<Role> resolveRoles(Set<UUID> roleIds) {
    if (roleIds == null || roleIds.isEmpty()) return new HashSet<>();
    Set<Role> roles = new HashSet<>();
    for (UUID id : roleIds) {
        Role r = roleRepository.findById(id).orElseThrow(() -> new AppException(ErrorCode.ROLE_NOT_EXISTED));
        if (SYS_ROOT.equals(r.getName())) throw new AppException(ErrorCode.ROLE_NOT_ASSIGNABLE);
        roles.add(r);
    }
    return roles;
}
```
- [ ] **Step 4:** Run → PASS. Commit `feat(users): create user with validation`.

### Task A6: UserService.list + get

**Files:** Modify `UserService.java`; Test `src/test/java/vn/thanhtuanle/user/UserServiceListTest.java`

**Interfaces produced:**
- `PageResponse<UserResponse> list(int page, int size, String search, Integer status, UUID roleId, LocalDate createdFrom, LocalDate createdTo)`
- `UserResponse getById(UUID id)`

- [ ] **Step 1 (test):** `list` passes lower-cased search + day-bounds to `repository.search` and maps via `userMapper` → `PageResponse` (mock repo returns `new PageImpl<>(List.of(user))`); `getById` unknown → `USER_NOT_FOUND`.
- [ ] **Step 2:** Run → FAIL.
- [ ] **Step 3:** Implement:
```java
public PageResponse<UserResponse> list(int page, int size, String search, Integer status, UUID roleId,
        LocalDate createdFrom, LocalDate createdTo) {
    Pageable pageable = PageRequest.of(page, size, Sort.by("createdAt").descending());
    String q = (search == null || search.isBlank()) ? null : search.trim().toLowerCase();
    LocalDateTime from = createdFrom == null ? null : createdFrom.atStartOfDay();
    LocalDateTime to = createdTo == null ? null : createdTo.plusDays(1).atStartOfDay();
    Page<UserResponse> dtoPage = userRepository.search(q, status, roleId, from, to, pageable)
        .map(userMapper::toResponse);
    return PageResponse.of(dtoPage);
}

public UserResponse getById(UUID id) {
    return userRepository.findById(id).map(userMapper::toResponse)
        .orElseThrow(() -> new AppException(ErrorCode.USER_NOT_FOUND));
}
```
- [ ] **Step 4:** Run → PASS. Commit `feat(users): list with filters + get by id`.

### Task A7: Guards + update + updateStatus + softDelete

**Files:** Modify `UserService.java`; Test `src/test/java/vn/thanhtuanle/user/UserServiceMutationTest.java`

**Interfaces produced:**
- `UserResponse update(UUID id, UpdateUserRequest req)`
- `UserResponse updateStatus(UUID id, Integer status)`
- `void softDelete(UUID id)`
- private `User loadModifiable(UUID id)` — loads user, throws `USER_NOT_FOUND`, throws `USER_PROTECTED` if target holds SYS_ROOT
- private `void assertNotSelf(User target)` — throws `USER_SELF_MODIFY` if target is current user

- [ ] **Step 1 (test):** update changes name/email (email uniqueness via `existsByEmailAndIdNot`) and rejects `DELETED` status; updateStatus on self → `USER_SELF_MODIFY`; any op on a SYS_ROOT user → `USER_PROTECTED`; softDelete sets status=2 and is blocked on self + SYS_ROOT. Mock `getCurrentUser()` by spying, or refactor self-check to compare `id` against `getCurrentUser().getId()`.
- [ ] **Step 2:** Run → FAIL.
- [ ] **Step 3:** Implement:
```java
private static boolean hasSysRoot(User u) {
    return u.getRoles() != null && u.getRoles().stream().anyMatch(r -> SYS_ROOT.equals(r.getName()));
}
private User loadModifiable(UUID id) {
    User u = userRepository.findById(id).orElseThrow(() -> new AppException(ErrorCode.USER_NOT_FOUND));
    if (hasSysRoot(u)) throw new AppException(ErrorCode.USER_PROTECTED);
    return u;
}
private void assertNotSelf(UUID id) {
    if (id.equals(getCurrentUser().getId())) throw new AppException(ErrorCode.USER_SELF_MODIFY);
}
private static int statusOrThrow(Integer status, boolean allowDeleted) {
    if (status == null) throw new AppException(ErrorCode.UNCATEGORIZED_EXCEPTION);
    UserStatus s = UserStatus.fromValue(status);           // throws IllegalArgument on bad value -> 500; acceptable
    if (!allowDeleted && s == UserStatus.DELETED) throw new AppException(ErrorCode.USER_SELF_MODIFY);
    return s.getValue();
}

@Transactional
public UserResponse update(UUID id, UpdateUserRequest req) {
    User u = loadModifiable(id);
    if (req.getName() != null) u.setName(req.getName().trim());
    if (req.getEmail() != null) {
        String email = req.getEmail().trim();
        if (!EMAIL.matcher(email).matches()) throw new AppException(ErrorCode.EMAIL_INVALID);
        if (userRepository.existsByEmailAndIdNot(email, id)) throw new AppException(ErrorCode.EMAIL_EXISTED);
        u.setEmail(email);
    }
    if (req.getStatus() != null) { assertNotSelf(id); u.setStatus(statusOrThrow(req.getStatus(), false)); }
    userRepository.save(u);
    return userMapper.toResponse(u);
}

@Transactional
public UserResponse updateStatus(UUID id, Integer status) {
    User u = loadModifiable(id);
    assertNotSelf(id);
    u.setStatus(statusOrThrow(status, false));   // only ACTIVE/DISABLED
    userRepository.save(u);
    return userMapper.toResponse(u);
}

@Transactional
public void softDelete(UUID id) {
    User u = loadModifiable(id);
    assertNotSelf(id);
    u.setStatus(UserStatus.DELETED.getValue());
    userRepository.save(u);
}
```
- [ ] **Step 4:** Run → PASS. Commit `feat(users): update, status toggle, soft-delete with guards`.

### Task A8: UserService.updateRoles

**Files:** Modify `UserService.java`; Test `UserServiceRolesTest.java`

**Interfaces produced:** `UserResponse updateRoles(UUID id, Set<UUID> roleIds)`

- [ ] **Step 1 (test):** replaces the whole set; self → `USER_SELF_MODIFY`; target SYS_ROOT → `USER_PROTECTED`; set containing SYS_ROOT id → `ROLE_NOT_ASSIGNABLE`; unknown role → `ROLE_NOT_EXISTED`.
- [ ] **Step 2:** Run → FAIL.
- [ ] **Step 3:** Implement:
```java
@Transactional
public UserResponse updateRoles(UUID id, Set<UUID> roleIds) {
    User u = loadModifiable(id);
    assertNotSelf(id);
    u.setRoles(resolveRoles(roleIds));   // resolveRoles already rejects SYS_ROOT + unknown ids
    userRepository.save(u);
    return userMapper.toResponse(u);
}
```
- [ ] **Step 4:** Run → PASS. Commit `feat(users): replace user roles with guards`.

### Task A9: UserService.resetPassword

**Files:** Modify `UserService.java`; Test `UserServiceResetPasswordTest.java`

**Interfaces produced:** `void resetPassword(UUID id, String newPassword)`

- [ ] **Step 1 (test):** valid password is encoded + saved; short password → `PASSWORD_INVALID`; target SYS_ROOT → `USER_PROTECTED`. (No self-guard — an admin resetting their own password is harmless; root is blocked by SYS_ROOT.)
- [ ] **Step 2:** Run → FAIL.
- [ ] **Step 3:** Implement (extract the password rule into `validatePassword(String)` reused by `create`):
```java
@Transactional
public void resetPassword(UUID id, String newPassword) {
    User u = loadModifiable(id);
    validatePassword(newPassword);
    u.setPassword(passwordEncoder.encode(newPassword));
    userRepository.save(u);
}
```
- [ ] **Step 4:** Run → PASS. Commit `feat(users): admin reset password`.

### Task A10: UserController

**Files:** Create `src/main/java/vn/thanhtuanle/user/UserController.java`

- [ ] **Step 1:** Wire endpoints (mirror `RoleController` + `ProblemController` list params):
```java
@RestController
@RequiredArgsConstructor
public class UserController {
    private final UserService userService;

    @GetMapping(Routes.USERS)
    @PreAuthorize("hasAuthority('user:read')")
    public ApiResponse<PageResponse<UserResponse>> list(
            @RequestParam(defaultValue = "0") int page,
            @RequestParam(defaultValue = "10") int size,
            @RequestParam(required = false) String search,
            @RequestParam(required = false) Integer status,
            @RequestParam(required = false) UUID roleId,
            @RequestParam(required = false) @DateTimeFormat(iso = DateTimeFormat.ISO.DATE) LocalDate createdFrom,
            @RequestParam(required = false) @DateTimeFormat(iso = DateTimeFormat.ISO.DATE) LocalDate createdTo) {
        return ApiResponse.success(userService.list(page, size, search, status, roleId, createdFrom, createdTo));
    }

    @GetMapping(Routes.USERS + "/{id}")
    @PreAuthorize("hasAuthority('user:read')")
    public ApiResponse<UserResponse> get(@PathVariable UUID id) { return ApiResponse.success(userService.getById(id)); }

    @PostMapping(Routes.USERS)
    @PreAuthorize("hasAuthority('user:create')")
    public ApiResponse<UserResponse> create(@RequestBody CreateUserRequest req) { return ApiResponse.created(userService.create(req)); }

    @PutMapping(Routes.USERS + "/{id}")
    @PreAuthorize("hasAuthority('user:update')")
    public ApiResponse<UserResponse> update(@PathVariable UUID id, @RequestBody UpdateUserRequest req) { return ApiResponse.success(userService.update(id, req)); }

    @PatchMapping(Routes.USERS + "/{id}/status")
    @PreAuthorize("hasAuthority('user:update')")
    public ApiResponse<UserResponse> status(@PathVariable UUID id, @RequestBody UpdateUserStatusRequest req) { return ApiResponse.success(userService.updateStatus(id, req.getStatus())); }

    @PutMapping(Routes.USERS + "/{id}/roles")
    @PreAuthorize("hasAuthority('user:update')")
    public ApiResponse<UserResponse> roles(@PathVariable UUID id, @RequestBody UpdateUserRolesRequest req) { return ApiResponse.success(userService.updateRoles(id, req.getRoleIds())); }

    @PostMapping(Routes.USERS + "/{id}/reset-password")
    @PreAuthorize("hasAuthority('user:update')")
    public ApiResponse<Void> resetPassword(@PathVariable UUID id, @RequestBody ResetPasswordRequest req) { userService.resetPassword(id, req.getNewPassword()); return ApiResponse.success(); }

    @DeleteMapping(Routes.USERS + "/{id}")
    @PreAuthorize("hasAuthority('user:delete')")
    public ApiResponse<Void> delete(@PathVariable UUID id) { userService.softDelete(id); return ApiResponse.success(); }
}
```
- [ ] **Step 2:** `./gradlew compileJava` → PASS. Commit `feat(users): UserController endpoints`.

### Task A11: Reject non-ACTIVE users on Google login

**Files:** Modify `src/main/java/vn/thanhtuanle/auth/AuthService.java` (Google path, after the user is loaded/created ~line 160-176)

- [ ] **Step 1:** After the existing-user branch resolves `user`, add the same guard the password path uses:
```java
if (user.getStatus() == null || user.getStatus() != CommonStatus.ACTIVE.getValue()) {
    throw new AppException(ErrorCode.USER_BLOCKED);
}
```
(Password login already enforces this — only the Google path is missing it.)
- [ ] **Step 2:** `./gradlew compileJava` → PASS. Commit `fix(auth): block non-active users on Google login`.

### Task A12: Backend build + live-stack verification

- [ ] **Step 1:** `./gradlew test` → all green (role + user suites).
- [ ] **Step 2:** Rebuild: from `judge-deployment`, `docker compose up -d --build judge-api`. Confirm Flyway applied `V9` in logs; query `t_permissions`/`t_roles` for the 4 perms + SYS_ROOT; confirm root user has SYS_ROOT.
- [ ] **Step 3:** With the admin cookie, exercise every endpoint (curl): list + each filter (search/status/roleId/createdFrom/createdTo); create (201) + duplicate username/email (409) + invalid (400); get; update; PATCH status toggle; PUT roles (replace) + SYS_ROOT-in-set (409) ; SYS_ROOT-target modify (409); self-modify (409); reset-password; DELETE (soft-delete → status 2, hidden from default list). Confirm a DISABLED user cannot log in.
- [ ] **Step 4:** Delete any test users created; leave DB clean (only seeded accounts).

---

# Part B — Frontend (`judge-portal`)

### Task B1: apiPath + types + userService

**Files:**
- Modify: `src/constants/apiPath.ts` (USERS block)
- Modify: `src/types/user.ts` (fix `Role.id` → `string`; add `UserStatus`, request payload types)
- Create: `src/services/userService.ts`
- Test: `src/services/__tests__/userService.spec.ts`

**Interfaces produced:** `userService.{ list(params), get(id), create(payload), update(id,payload), updateStatus(id,status), updateRoles(id,roleIds), resetPassword(id,newPassword), remove(id) }`

- [ ] **Step 1:** Extend `apiPath.USERS`:
```ts
USERS: {
    ROOT: '/users',
    ME: '/users/me',
    PROFILE: (id: string) => `/users/${id}`,
    STATUS: (id: string) => `/users/${id}/status`,
    ROLES: (id: string) => `/users/${id}/roles`,
    RESET_PASSWORD: (id: string) => `/users/${id}/reset-password`,
},
```
- [ ] **Step 2:** In `types/user.ts` change `Role.id` to `string`; add `export const USER_STATUS = { ACTIVE: 1, DISABLED: 0, DELETED: 2 } as const` and payload types (`CreateUserPayload`, `UpdateUserPayload`, `UserListParams`).
- [ ] **Step 3 (test):** `userService.spec.ts` asserts each method calls the right axios verb + path (mock the http client like `roleService.spec.ts`).
- [ ] **Step 4:** Implement `userService.ts` mirroring `roleService.ts`. Run `npx vitest run src/services/__tests__/userService.spec.ts` → PASS.
- [ ] **Step 5:** Commit `feat(users): apiPath, types, userService`.

### Task B2: userValidators

**Files:** Create `src/lib/userValidators.ts`; Test `src/lib/__tests__/userValidators.spec.ts`

- [ ] **Step 1 (test):** `isValidUsername('ab')===false`, `isValidUsername('good_1')===true`; `isValidPassword('short')===false`, `isValidPassword('abcd1234')===true`; email helper.
- [ ] **Step 2:** Implement (mirror `lib/roleName.ts`):
```ts
export function isValidUsername(v: string): boolean { return /^[a-zA-Z0-9_]{3,32}$/.test(v.trim()) }
export function isValidPassword(v: string): boolean { return v.length >= 8 && /[a-zA-Z]/.test(v) && /\d/.test(v) }
export function isValidEmail(v: string): boolean { return /^[^@\s]+@[^@\s]+\.[^@\s]+$/.test(v.trim()) }
```
- [ ] **Step 3:** Run → PASS. Commit `feat(users): field validators`.

### Task B3: AdminUsersPage — list + filters + pagination

**Files:** Rewrite `src/pages/admin/AdminUsersPage.vue`

- [ ] **Step 1:** Replace the stub with a `useFetch`-driven table (columns: username · name · email · role badges · status badge · created date). Toolbar: search `Input` (debounced), status `Select` (All / Active / Disabled / Deleted), role `Select` (from `roleService.list`), created-date range (two date `Input`s). Wire all to server query params; `PaginationFooter` at the bottom. Mirror `AdminRolesPage.vue` for fetch + `getErrorMessage` + `triggerToast`. Status badge maps `USER_STATUS`.
- [ ] **Step 2:** `npx vue-tsc --noEmit` (type-check) → clean. Commit `feat(users): admin users list + filters`.

### Task B4: Create + Edit dialogs

**Files:** Modify `AdminUsersPage.vue` (or extract `components/admin/user/*` dialogs if the page grows large)

- [ ] **Step 1:** **Create user** button (gate `user:create`) → dialog: username (validated), name, email (validated), password (validated), status Select, roles checkbox list (SYS_ROOT omitted). Save disabled until valid; calls `userService.create`; toast + refresh.
- [ ] **Step 2:** **Edit** row action (gate `user:update`) → dialog: name, email, status (username shown read-only). Calls `userService.update`.
- [ ] **Step 3:** Type-check clean. Commit `feat(users): create + edit dialogs`.

### Task B5: Manage-roles dialog

**Files:** Modify `AdminUsersPage.vue`

- [ ] **Step 1:** **Manage roles** row action (gate `user:update`, hidden for SYS_ROOT rows and the current user's own row) → dialog with a checkbox per assignable role (fetch roles once; **exclude SYS_ROOT**), pre-checked from the user's current roles. Save → `userService.updateRoles(id, checkedIds)`; toast + refresh.
- [ ] **Step 2:** Type-check clean. Commit `feat(users): manage-roles dialog`.

### Task B6: Reset-password + status toggle + delete + gating

**Files:** Modify `AdminUsersPage.vue`

- [ ] **Step 1:** **Reset password** action (gate `user:update`) → dialog with a password field (validated) → `userService.resetPassword`.
- [ ] **Step 2:** **Disable/Enable** toggle (gate `user:update`, hidden for self + SYS_ROOT) → `userService.updateStatus(id, next)`.
- [ ] **Step 3:** **Delete** action (gate `user:delete`, hidden for self + SYS_ROOT) → confirm dialog → `userService.remove(id)` (soft-delete); toast + refresh.
- [ ] **Step 4:** Per-row rules: rows whose user holds `SYS_ROOT` show no mutating actions; the current user's own row hides manage-roles/disable/delete (self id from `authStore`). Type-check clean. Commit `feat(users): reset-password, status toggle, delete + gating`.

### Task B7: Frontend build + live verification

- [ ] **Step 1:** `npm run lint` (0 errors), `npx vue-tsc --noEmit` (clean), `npx vitest run` (all pass).
- [ ] **Step 2:** `docker compose up -d --build judge-portal`; open `/admin/users` as admin; verify list/filters/pagination, create, edit, manage-roles, reset-password, disable/enable, delete; confirm SYS_ROOT (root) row is action-less and own row hides destructive actions. Screenshot.
- [ ] **Step 3:** Clean up any test users. Commit any final tweaks.

---

## Self-review notes (author)

- Spec coverage: every spec section (V9, UserStatus, 7 endpoints incl. PATCH status, guardrails, filters incl. created-date, FE page+dialogs, tests) maps to a task above.
- Login status: password path already guards; only the Google path needs A11.
- `USER_NOT_EXISTED` (401) is login-only; admin GET uses the new `USER_NOT_FOUND` (404).
- SYS_ROOT protection is enforced three ways: immutable target (`loadModifiable`), not-assignable (`resolveRoles`), not-deletable (RoleService `SYSTEM_ROLES`).
