# Problems List Filters + Tags Column — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Wire the problems-page filter bar to real API filters (difficulty + keyword), add a lightweight `tags` field to problems (seeded in the DB), and render a Tags column that the Tags switch shows/hides.

**Architecture:** `judge-api` gains a `hardnessLevel` filter on `GET /problems` and a `tags` element-collection on `Problem` (loaded for the list via one batch query per page, attached to DTOs in the service). `judge-portal` turns the stateless `SearchBar` into a controlled component, lifts filter state into `ProblemListPage`, and adds a toggleable Tags column to `ProblemTable`.

**Tech Stack:** Spring Boot 3 / JPA (native queries) / MapStruct / Lombok / JUnit 5 + Mockito (backend); Vue 3 `<script setup>` / TypeScript / reka-ui / Vitest (frontend). PostgreSQL 16, seeded via `initdb.sql`.

## Global Constraints

- Difficulty encoding is fixed: dropdown `all/easy/medium/hard` ↔ `hardnessLevel` `undefined/1/2/3`. Missing `hardnessLevel` = no filter.
- Tags are **display-only** in this feature — no tag filtering, no tag-assignment UI. Data comes from DB seed only.
- Tags column is **hidden by default** (switch off), matching the current mockup.
- Filter state is component-local — **not** synced to the URL.
- Backend list tags are loaded with **one** extra query per page (batch by problem id); an empty page must skip that query.
- Follow existing test patterns: backend = Mockito service tests (no `@DataJpaTest` infra exists); frontend = pure-logic Vitest tests (`environment: 'node'`, no `@vue/test-utils`/DOM). Component/page UI is verified by `vue-tsc` type-check + a manual run.
- Backend repos: `/home/tuan/Project/my-oj/judge-api`. Frontend: `/home/tuan/Project/my-oj/judge-portal`. Commit within each repo separately.

---

## Task 1: Backend — persistence layer (entity tags, DTO, tag projection, repository queries)

**Files:**
- Modify: `judge-api/src/main/java/vn/thanhtuanle/entity/Problem.java`
- Modify: `judge-api/src/main/java/vn/thanhtuanle/problem/dto/ProblemResponseDto.java`
- Create: `judge-api/src/main/java/vn/thanhtuanle/problem/dto/ProblemTagRow.java`
- Modify: `judge-api/src/main/java/vn/thanhtuanle/problem/ProblemRepository.java`

**Interfaces:**
- Produces:
  - `Problem.getTags()/setTags()` → `List<String>`, mapped to table `t_problem_tags(problem_id, tag)`.
  - `ProblemResponseDto.getTags()/setTags()` → `List<String>`.
  - `interface ProblemTagRow { UUID getProblemId(); String getTag(); }`
  - `Page<ProblemStatisticProjection> ProblemRepository.findProblemsWithStats(String search, Integer status, Integer hardnessLevel, Pageable pageable)`
  - `List<ProblemTagRow> ProblemRepository.findTagsByProblemIds(Collection<UUID> ids)`

- [ ] **Step 1: Add the `tags` element collection to `Problem`**

In `Problem.java`, add these imports next to the existing `jakarta.persistence.*` imports:

```java
import jakarta.persistence.CollectionTable;
import jakarta.persistence.ElementCollection;
import jakarta.persistence.FetchType;
import jakarta.persistence.JoinColumn;
```

Then add this field inside the class, directly after the `status` field (before `testCases`):

```java
    @ElementCollection(fetch = FetchType.LAZY)
    @CollectionTable(name = "t_problem_tags", joinColumns = @JoinColumn(name = "problem_id"))
    @Column(name = "tag")
    @Builder.Default
    private List<String> tags = new ArrayList<>();
```

(`Column`, `List`, `ArrayList`, and `@Builder.Default` are already imported/used in this file.)

- [ ] **Step 2: Add `tags` to `ProblemResponseDto`**

In `ProblemResponseDto.java`, add the import and field:

```java
import java.util.List;
```

Add this field after `private Map<String, Integer> statisticInfo;`:

```java
    private List<String> tags;
```

- [ ] **Step 3: Create the `ProblemTagRow` projection**

Create `judge-api/src/main/java/vn/thanhtuanle/problem/dto/ProblemTagRow.java`:

```java
package vn.thanhtuanle.problem.dto;

import java.util.UUID;

/** Native-query row projection: one (problem id, tag) pair from t_problem_tags. */
public interface ProblemTagRow {
    UUID getProblemId();

    String getTag();
}
```

- [ ] **Step 4: Add the `hardnessLevel` predicate and the tags batch query to `ProblemRepository`**

In `ProblemRepository.java`, add imports:

```java
import java.util.Collection;
import vn.thanhtuanle.problem.dto.ProblemTagRow;
```

Replace the existing `findProblemsWithStats` method (the `@Query(...)` block and its signature) with this version — it adds the `hardnessLevel` predicate to **both** the main and the count query and adds the parameter:

```java
    @Query(value = """
            SELECT
                p.*,
                COUNT(s.id) AS totalSubmission,
                COUNT(CASE WHEN s.status = 0 THEN 1 END) AS acceptedSubmission
            FROM t_problems p
            LEFT JOIN t_submissions s ON p.id = s.problem_id
            WHERE (:search IS NULL OR p.title ILIKE CONCAT('%', :search, '%')
                   OR p.description ILIKE CONCAT('%', :search, '%'))
              AND (:status IS NULL OR p.status = :status)
              AND (:hardnessLevel IS NULL OR p.hardness_level = :hardnessLevel)
            GROUP BY p.id
            """, countQuery = """
            SELECT count(*) FROM t_problems p
            WHERE (:search IS NULL OR p.title ILIKE CONCAT('%', :search, '%'))
              AND (:status IS NULL OR p.status = :status)
              AND (:hardnessLevel IS NULL OR p.hardness_level = :hardnessLevel)
            """, nativeQuery = true)
    Page<ProblemStatisticProjection> findProblemsWithStats(String search, Integer status, Integer hardnessLevel,
            Pageable pageable);
```

Then add this new method right after it:

```java
    @Query(value = "SELECT problem_id AS problemId, tag AS tag FROM t_problem_tags WHERE problem_id IN (:ids)", nativeQuery = true)
    List<ProblemTagRow> findTagsByProblemIds(@Param("ids") Collection<UUID> ids);
```

- [ ] **Step 5: Compile**

Run: `cd /home/tuan/Project/my-oj/judge-api && ./mvnw -q compile`
Expected: BUILD SUCCESS (no errors). This will fail to link until Task 2 updates the service call site — see note.

> NOTE: `ProblemService.getProblems` still calls the old 3-arg `findProblemsWithStats`. Compilation of the repository/entity/DTO themselves succeeds, but `mvn compile` compiles the whole module, so the service call site is now a compile error. That is expected and fixed in Task 2. If you want a green compile at this boundary, do Steps 1–4 here and the service edit in Task 2 Step 3 before running `mvn compile`. **Commit Task 1 + Task 2 together** (they are one compilable unit); the checkbox commit is at the end of Task 2.

---

## Task 2: Backend — service tag-attach + controller param (TDD)

**Files:**
- Modify: `judge-api/src/main/java/vn/thanhtuanle/problem/ProblemService.java`
- Modify: `judge-api/src/main/java/vn/thanhtuanle/problem/ProblemController.java`
- Create: `judge-api/src/test/java/vn/thanhtuanle/problem/ProblemServiceTest.java`

**Interfaces:**
- Consumes: `ProblemRepository.findProblemsWithStats(String, Integer, Integer, Pageable)`, `ProblemRepository.findTagsByProblemIds(Collection<UUID>)`, `ProblemTagRow`, `ProblemResponseDto.setTags(List<String>)` (Task 1).
- Produces: `ProblemService.getProblems(int page, int size, String search, ProblemStatus status, Integer hardnessLevel)` → `PageResponse<ProblemResponseDto>` with `tags` populated. `GET /problems` accepts `hardnessLevel` query param.

- [ ] **Step 1: Write the failing service test**

Create `judge-api/src/test/java/vn/thanhtuanle/problem/ProblemServiceTest.java`:

```java
package vn.thanhtuanle.problem;

import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.extension.ExtendWith;
import org.mockito.InjectMocks;
import org.mockito.Mock;
import org.mockito.junit.jupiter.MockitoExtension;
import org.springframework.data.domain.Page;
import org.springframework.data.domain.PageImpl;
import org.springframework.data.domain.PageRequest;
import org.springframework.data.domain.Pageable;

import vn.thanhtuanle.common.enums.ProblemStatus;
import vn.thanhtuanle.common.payload.PageResponse;
import vn.thanhtuanle.common.util.GenerateTestCaseInfoUtil;
import vn.thanhtuanle.problem.dto.ProblemResponseDto;
import vn.thanhtuanle.problem.dto.ProblemStatisticProjection;
import vn.thanhtuanle.problem.dto.ProblemTagRow;
import vn.thanhtuanle.problem.mapper.ProblemMapper;

import java.util.List;
import java.util.UUID;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.anyList;
import static org.mockito.ArgumentMatchers.eq;
import static org.mockito.ArgumentMatchers.isNull;
import static org.mockito.Mockito.never;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;
import static org.mockito.Mockito.mock;

@ExtendWith(MockitoExtension.class)
class ProblemServiceTest {

    @Mock
    private ProblemRepository problemRepository;
    @Mock
    private ProblemMapper problemMapper;
    @Mock
    private GenerateTestCaseInfoUtil infoGenerator;

    @InjectMocks
    private ProblemService problemService;

    private static ProblemTagRow tagRow(UUID id, String tag) {
        return new ProblemTagRow() {
            public UUID getProblemId() { return id; }
            public String getTag() { return tag; }
        };
    }

    @Test
    void getProblems_forwardsHardnessLevel_andAttachesTagsGroupedByProblem() {
        UUID id1 = UUID.fromString("11111111-1111-1111-1111-111111111111");
        UUID id2 = UUID.fromString("22222222-2222-2222-2222-222222222222");

        ProblemStatisticProjection proj1 = mock(ProblemStatisticProjection.class);
        ProblemStatisticProjection proj2 = mock(ProblemStatisticProjection.class);
        ProblemResponseDto dto1 = ProblemResponseDto.builder().id(id1).build();
        ProblemResponseDto dto2 = ProblemResponseDto.builder().id(id2).build();

        Page<ProblemStatisticProjection> page =
                new PageImpl<>(List.of(proj1, proj2), PageRequest.of(0, 10), 2);
        when(problemRepository.findProblemsWithStats(eq("gcd"), eq(1), eq(2), any(Pageable.class)))
                .thenReturn(page);
        when(problemMapper.toDto(proj1)).thenReturn(dto1);
        when(problemMapper.toDto(proj2)).thenReturn(dto2);
        when(problemRepository.findTagsByProblemIds(List.of(id1, id2)))
                .thenReturn(List.of(tagRow(id1, "Math"), tagRow(id1, "Greedy")));

        PageResponse<ProblemResponseDto> result =
                problemService.getProblems(0, 10, "gcd", ProblemStatus.ACTIVE, 2);

        verify(problemRepository).findProblemsWithStats(eq("gcd"), eq(1), eq(2), any(Pageable.class));
        assertEquals(List.of("Math", "Greedy"), result.getData().get(0).getTags());
        assertTrue(result.getData().get(1).getTags().isEmpty());
    }

    @Test
    void getProblems_emptyPage_doesNotQueryTags() {
        Page<ProblemStatisticProjection> empty =
                new PageImpl<>(List.of(), PageRequest.of(0, 10), 0);
        when(problemRepository.findProblemsWithStats(isNull(), isNull(), isNull(), any(Pageable.class)))
                .thenReturn(empty);

        PageResponse<ProblemResponseDto> result =
                problemService.getProblems(0, 10, null, null, null);

        assertTrue(result.getData().isEmpty());
        verify(problemRepository, never()).findTagsByProblemIds(anyList());
    }
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd /home/tuan/Project/my-oj/judge-api && ./mvnw -q -Dtest=ProblemServiceTest test`
Expected: compile failure / test failure — `getProblems` does not yet accept a 5th `Integer` argument and does not attach tags.

- [ ] **Step 3: Update `ProblemService.getProblems`**

In `ProblemService.java`, add imports:

```java
import java.util.UUID;
import vn.thanhtuanle.problem.dto.ProblemTagRow;
```

Replace the existing `getProblems(...)` method with:

```java
    public PageResponse<ProblemResponseDto> getProblems(int page, int size, String search, ProblemStatus status,
            Integer hardnessLevel) {
        Pageable pageable = PageRequest.of(page, size, Sort.by(AppProperties.DEFAULT_SORT_BY).descending());

        Integer statusValue = status != null ? status.getValue() : null;
        Page<ProblemResponseDto> dtoPage = problemRepository
                .findProblemsWithStats(search, statusValue, hardnessLevel, pageable)
                .map(problemMapper::toDto);

        attachTags(dtoPage.getContent());
        return PageResponse.of(dtoPage);
    }

    private void attachTags(List<ProblemResponseDto> problems) {
        if (problems.isEmpty()) {
            return;
        }
        List<UUID> ids = problems.stream().map(ProblemResponseDto::getId).toList();
        Map<UUID, List<String>> tagsByProblem = problemRepository.findTagsByProblemIds(ids).stream()
                .collect(Collectors.groupingBy(
                        ProblemTagRow::getProblemId,
                        Collectors.mapping(ProblemTagRow::getTag, Collectors.toList())));
        problems.forEach(p -> p.setTags(tagsByProblem.getOrDefault(p.getId(), List.of())));
    }
```

(`Map`, `List`, `Collectors`, `Page`, `Pageable`, `PageRequest`, `Sort` are already imported.)

- [ ] **Step 4: Update `ProblemController.getProblems` to accept `hardnessLevel`**

In `ProblemController.java`, replace the `getProblems` handler with:

```java
    @GetMapping
    @Operation(summary = "Get list of problems")
    public ApiResponse<List<ProblemResponseDto>> getProblems(
            @RequestParam(defaultValue = "0") int page,
            @RequestParam(defaultValue = "10") int size,
            @RequestParam(required = false) String search,
            @RequestParam(required = false) ProblemStatus status,
            @RequestParam(required = false) Integer hardnessLevel) {

        log.info("Start get problems: page={}, size={}, search={}, status={}, hardnessLevel={}",
                page, size, search, status, hardnessLevel);
        return ApiResponse.success(problemService.getProblems(page, size, search, status, hardnessLevel));
    }
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `cd /home/tuan/Project/my-oj/judge-api && ./mvnw -q -Dtest=ProblemServiceTest test`
Expected: PASS (2 tests).

- [ ] **Step 6: Full compile + existing tests still green**

Run: `cd /home/tuan/Project/my-oj/judge-api && ./mvnw -q test`
Expected: BUILD SUCCESS.

- [ ] **Step 7: Commit (Task 1 + Task 2 together)**

```bash
cd /home/tuan/Project/my-oj/judge-api
git add src/main/java/vn/thanhtuanle/entity/Problem.java \
        src/main/java/vn/thanhtuanle/problem/dto/ProblemResponseDto.java \
        src/main/java/vn/thanhtuanle/problem/dto/ProblemTagRow.java \
        src/main/java/vn/thanhtuanle/problem/ProblemRepository.java \
        src/main/java/vn/thanhtuanle/problem/ProblemService.java \
        src/main/java/vn/thanhtuanle/problem/ProblemController.java \
        src/test/java/vn/thanhtuanle/problem/ProblemServiceTest.java
git commit -m "feat(problems): add hardnessLevel filter + tags to problem list API"
```

---

## Task 3: Backend — seed `t_problem_tags` in `initdb.sql`

**Files:**
- Modify: `judge-api/initdb.sql`

**Interfaces:**
- Consumes: table shape from Task 1 (`t_problem_tags(problem_id uuid, tag varchar)`), existing seeded problem ids in `t_problems`.
- Produces: a fresh DB has tags for the three seeded problems, so the Tags column shows data.

- [ ] **Step 1: Append the tags table + seed block to `initdb.sql`**

Append the following at the **end** of `judge-api/initdb.sql` (after this point all referenced problems and the `t_problems` primary key already exist, so the `CREATE`, `INSERT`, and `FK` all succeed in order):

```sql
--
-- Name: t_problem_tags; Type: TABLE; Schema: public; Owner: postgres
--

CREATE TABLE public.t_problem_tags (
    problem_id uuid NOT NULL,
    tag character varying(255)
);

ALTER TABLE public.t_problem_tags OWNER TO postgres;

--
-- Data for Name: t_problem_tags; Type: TABLE DATA; Schema: public; Owner: postgres
--

INSERT INTO public.t_problem_tags (problem_id, tag) VALUES
('6347d5d8-e782-468e-814f-52af5a2c8f57', 'Math'),
('6347d5d8-e782-468e-814f-52af5a2c8f57', 'Strings'),
('bb355243-e789-45ee-ad67-016a8ba1ee18', 'Math'),
('bb355243-e789-45ee-ad67-016a8ba1ee18', 'Implementation'),
('23408310-414d-4438-9844-dc8f15f04d47', 'Math'),
('23408310-414d-4438-9844-dc8f15f04d47', 'Number Theory'),
('23408310-414d-4438-9844-dc8f15f04d47', 'Greedy');

--
-- Name: t_problem_tags fk_problem_tags_problem; Type: FK CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.t_problem_tags
    ADD CONSTRAINT fk_problem_tags_problem FOREIGN KEY (problem_id) REFERENCES public.t_problems(id);
```

> The three UUIDs are the existing seeded problems (`bac-thang-danh-vong`, `simple-a-plus-b`, `ucln-a-b`). All three are `hardness_level = 1` (Easy), so a Difficulty = Medium/Hard filter correctly returns an empty list until more problems exist — that is expected, not a bug.

- [ ] **Step 2: Verify on a fresh DB**

`initdb.sql` only runs when the Postgres data volume is empty, so recreate it:

```bash
cd /home/tuan/Project/my-oj/judge-api
docker compose down -v
docker compose up -d db
sleep 5
docker compose exec -T db psql -U "$DATABASE_USERNAME" -d my_oj -c "SELECT problem_id, tag FROM t_problem_tags ORDER BY tag;"
```

Expected: 7 rows printed (Greedy, Implementation, Math×3, Number Theory, Strings). If `$DATABASE_USERNAME` is unset, substitute the value from the judge-api `.env`.

- [ ] **Step 3: Commit**

```bash
cd /home/tuan/Project/my-oj/judge-api
git add initdb.sql
git commit -m "chore(seed): add t_problem_tags table and seed tags for demo problems"
```

---

## Task 4: Frontend — types + difficulty mapping (TDD)

**Files:**
- Modify: `judge-portal/src/types/problem.ts`
- Modify: `judge-portal/src/types/common.ts`
- Create: `judge-portal/src/types/__tests__/problem.spec.ts`

**Interfaces:**
- Produces:
  - `Problem.tags?: string[]`
  - `ProblemSearchParams.hardnessLevel?: number` (extends `BaseSearchParams`)
  - `difficultyToHardness(difficulty: string): number | undefined`

- [ ] **Step 1: Write the failing test**

Create `judge-portal/src/types/__tests__/problem.spec.ts`:

```ts
import { describe, it, expect } from 'vitest'
import { difficultyToHardness } from '@/types/problem'

describe('difficultyToHardness', () => {
  it('maps difficulty labels to hardness levels', () => {
    expect(difficultyToHardness('easy')).toBe(1)
    expect(difficultyToHardness('medium')).toBe(2)
    expect(difficultyToHardness('hard')).toBe(3)
  })

  it('returns undefined for "all" or unknown values', () => {
    expect(difficultyToHardness('all')).toBeUndefined()
    expect(difficultyToHardness('')).toBeUndefined()
    expect(difficultyToHardness('whatever')).toBeUndefined()
  })
})
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd /home/tuan/Project/my-oj/judge-portal && npx vitest run src/types/__tests__/problem.spec.ts`
Expected: FAIL — `difficultyToHardness` is not exported.

- [ ] **Step 3: Add the `tags` field and the mapping function**

In `judge-portal/src/types/problem.ts`, add `tags` to the `Problem` interface (after `acceptedSubmission`):

```ts
    tags?: string[]
```

Then add this exported function at the end of the file:

```ts
export const difficultyToHardness = (difficulty: string): number | undefined => {
    switch (difficulty) {
        case 'easy': return 1
        case 'medium': return 2
        case 'hard': return 3
        default: return undefined // 'all' or unknown → no filter
    }
}
```

- [ ] **Step 4: Add `hardnessLevel` to `ProblemSearchParams`**

In `judge-portal/src/types/problem.ts`, update the interface:

```ts
export interface ProblemSearchParams extends BaseSearchParams {
    status?: 'ACTIVE' | 'INACTIVE'
    hardnessLevel?: number
}
```

(No change needed in `common.ts` — `BaseSearchParams` already has `page/size/search`. If a later reviewer prefers `hardnessLevel` on the base type, leave it here; it is problem-specific.)

- [ ] **Step 5: Run the test to verify it passes**

Run: `cd /home/tuan/Project/my-oj/judge-portal && npx vitest run src/types/__tests__/problem.spec.ts`
Expected: PASS (2 tests).

- [ ] **Step 6: Commit**

```bash
cd /home/tuan/Project/my-oj/judge-portal
git add src/types/problem.ts src/types/__tests__/problem.spec.ts
git commit -m "feat(problems): add tags type + difficulty->hardness mapping"
```

---

## Task 5: Frontend — Tags column in `ProblemTable`

**Files:**
- Modify: `judge-portal/src/components/problem/ProblemTable.vue`

**Interfaces:**
- Consumes: `Problem.tags` (Task 4).
- Produces: `ProblemTable` prop `showTags: boolean` (default `false`); renders a Tags column only when `showTags` is true.

- [ ] **Step 1: Add the `showTags` prop**

In `ProblemTable.vue`, replace the existing `defineProps` block (`defineProps<{ problems: Problem[] }>()`) with a version that adds `showTags` (kept bare, matching the file's existing `problems` usage — props are reactive in the template without a `const` binding):

```ts
withDefaults(defineProps<{
    problems: Problem[]
    showTags?: boolean
}>(), {
    showTags: false,
})
```

- [ ] **Step 2: Add the Tags header cell**

In the `<TableHeader>` row, insert this `TableHead` immediately after the Title head (`<TableHead class="min-w-[200px]">Title</TableHead>`):

```html
                    <TableHead v-if="showTags" class="w-[220px]">Tags</TableHead>
```

- [ ] **Step 3: Add the Tags body cell**

In the `<TableBody>` row, insert this `TableCell` immediately after the Title cell (the `<TableCell>` containing the `RouterLink`):

```html
                    <TableCell v-if="showTags">
                        <div class="flex flex-wrap gap-1.5">
                            <Badge v-for="tag in problem.tags || []" :key="tag" variant="secondary"
                                class="rounded-md border-0 bg-muted px-2 py-0.5 text-[11px] font-normal text-muted-foreground">
                                {{ tag }}
                            </Badge>
                            <span v-if="!(problem.tags && problem.tags.length)" class="text-xs text-muted-foreground">—</span>
                        </div>
                    </TableCell>
```

(`Badge` is already imported in this file.)

- [ ] **Step 4: Type-check**

Run: `cd /home/tuan/Project/my-oj/judge-portal && npm run type-check`
Expected: no errors.

- [ ] **Step 5: Commit**

```bash
cd /home/tuan/Project/my-oj/judge-portal
git add src/components/problem/ProblemTable.vue
git commit -m "feat(problems): add toggleable Tags column to ProblemTable"
```

---

## Task 6: Frontend — controlled `SearchBar` (props/emits + debounced search)

**Files:**
- Modify: `judge-portal/src/components/problem/SearchBar.vue`

**Interfaces:**
- Produces: `SearchBar` with
  - props: `search: string`, `difficulty: string`, `showTags: boolean`
  - emits: `update:search` (debounced ~350ms), `update:difficulty`, `update:showTags`, `refresh`

- [ ] **Step 1: Replace `SearchBar.vue` with the controlled version**

Replace the entire contents of `judge-portal/src/components/problem/SearchBar.vue` with:

```vue
<template>
    <div class="flex flex-col gap-6 border-b border-border p-6 lg:flex-row lg:items-center lg:justify-between">
        <!-- Title -->
        <h1 class="font-display text-3xl text-foreground">Problems</h1>

        <!-- Filters -->
        <div class="flex flex-col gap-3 lg:flex-row lg:items-center">
            <!-- Difficulty Dropdown -->
            <Select :model-value="difficulty" @update:model-value="onDifficulty">
                <SelectTrigger class="w-[120px]">
                    <SelectValue placeholder="Difficulty" />
                </SelectTrigger>
                <SelectContent>
                    <SelectItem value="all">All</SelectItem>
                    <SelectItem value="easy">Easy</SelectItem>
                    <SelectItem value="medium">Medium</SelectItem>
                    <SelectItem value="hard">Hard</SelectItem>
                </SelectContent>
            </Select>

            <!-- Tags Switch -->
            <div class="flex items-center gap-2 rounded-md border border-border px-3 py-2">
                <Switch id="tags-mode" class="scale-90 data-[state=checked]:bg-primary"
                    :model-value="showTags" @update:model-value="onToggleTags" />
                <Label for="tags-mode" class="cursor-pointer text-sm font-medium text-muted-foreground">Tags</Label>
            </div>

            <!-- Keyword Input -->
            <div class="relative w-full lg:w-[280px]">
                <Search class="absolute left-3 top-2.5 h-4 w-4 text-muted-foreground" />
                <Input v-model="localSearch" placeholder="Search by title or ID…" class="pl-9" />
            </div>

            <!-- Refresh Button -->
            <Button variant="outline" size="icon" class="cursor-pointer" @click="$emit('refresh')">
                <RotateCw class="h-4 w-4" />
            </Button>
        </div>
    </div>
</template>

<script setup lang="ts">
import { ref, watch } from 'vue'
import { watchDebounced } from '@vueuse/core'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'
import { Label } from '@/components/ui/label'
import {
    Select,
    SelectContent,
    SelectItem,
    SelectTrigger,
    SelectValue,
} from '@/components/ui/select'
import { Search, RotateCw } from 'lucide-vue-next'

const props = withDefaults(defineProps<{
    search?: string
    difficulty?: string
    showTags?: boolean
}>(), {
    search: '',
    difficulty: 'all',
    showTags: false,
})

const emit = defineEmits<{
    (e: 'update:search', value: string): void
    (e: 'update:difficulty', value: string): void
    (e: 'update:showTags', value: boolean): void
    (e: 'refresh'): void
}>()

// Local mirror of the keyword so typing is smooth; emit is debounced.
const localSearch = ref(props.search)

// Keep local in sync if the parent resets search externally.
watch(() => props.search, (v) => {
    if (v !== localSearch.value) localSearch.value = v
})

watchDebounced(localSearch, (v) => emit('update:search', v), { debounce: 350 })

const onDifficulty = (value: unknown) => emit('update:difficulty', (value as string) ?? 'all')
const onToggleTags = (value: boolean) => emit('update:showTags', value)
</script>
```

- [ ] **Step 2: Type-check**

Run: `cd /home/tuan/Project/my-oj/judge-portal && npm run type-check`
Expected: no errors.

- [ ] **Step 3: Commit**

```bash
cd /home/tuan/Project/my-oj/judge-portal
git add src/components/problem/SearchBar.vue
git commit -m "feat(problems): make SearchBar a controlled filter component"
```

---

## Task 7: Frontend — wire filters in `ProblemListPage`

**Files:**
- Modify: `judge-portal/src/pages/user/problem/ProblemListPage.vue`

**Interfaces:**
- Consumes: `SearchBar` props/emits (Task 6), `ProblemTable` `showTags` prop (Task 5), `difficultyToHardness` + `ProblemSearchParams.hardnessLevel` (Task 4).

- [ ] **Step 1: Bind `SearchBar` + `ProblemTable` and lift filter state**

Replace the entire contents of `judge-portal/src/pages/user/problem/ProblemListPage.vue` with:

```vue
<template>
    <div class="animate-enter grid grid-cols-1 gap-6 lg:grid-cols-[9.5fr_2.5fr]">
        <!-- Main Content (Left) -->
        <div>
            <div class="rounded-xl border border-border bg-card">
                <SearchBar
                    :search="search"
                    :difficulty="difficulty"
                    :show-tags="showTags"
                    @update:search="onSearch"
                    @update:difficulty="onDifficulty"
                    @update:show-tags="showTags = $event"
                    @refresh="fetchProblems" />
                <div class="px-6 pb-6">
                    <div v-if="isLoading" class="space-y-3 py-5">
                        <div v-for="n in 6" :key="n" class="flex items-center gap-4">
                            <div class="h-4 w-6 shrink-0 animate-pulse rounded bg-muted"></div>
                            <div class="h-4 w-full max-w-[220px] animate-pulse rounded bg-muted"></div>
                            <div class="ml-auto h-5 w-14 shrink-0 animate-pulse rounded bg-muted"></div>
                            <div class="h-4 w-20 shrink-0 animate-pulse rounded bg-muted"></div>
                        </div>
                    </div>
                    <div v-else-if="error" class="p-8 text-center text-destructive">Couldn't load problems. Please try again.</div>
                    <ProblemTable v-else :problems="problems" :show-tags="showTags" />
                    <PaginationFooter :pagination="pagination" @change-page="onPageChange" @update:size="onSizeChange" />
                </div>
            </div>
        </div>

        <!-- Sidebar (Right) -->
        <aside class="space-y-6">
            <TagCloud />
            <Announcements />
        </aside>
    </div>
</template>

<script setup lang="ts">
import { ref, computed } from 'vue'
import SearchBar from '@/components/problem/SearchBar.vue'
import ProblemTable from '@/components/problem/ProblemTable.vue'
import TagCloud from '@/components/problem/TagCloud.vue'
import PaginationFooter from '@/components/common/PaginationFooter.vue'
import Announcements from '@/components/common/Announcements.vue'
import { useFetch } from '@/composables/useFetch'
import problemService from '@/services/problemService'
import { difficultyToHardness } from '@/types/problem'

const page = ref(0)
const size = ref(10)
const search = ref('')
const difficulty = ref('all')
const showTags = ref(false)

const { data: response, isLoading, error, execute } = useFetch(problemService.getProblems, {
    immediate: false
})

const problems = computed(() => response.value?.data || [])
const pagination = computed(() => response.value?.pagination)

const fetchProblems = () => {
    execute({
        page: page.value,
        size: size.value,
        search: search.value || undefined,
        hardnessLevel: difficultyToHardness(difficulty.value)
    })
}

// Any filter change resets to the first page.
const onSearch = (value: string) => {
    search.value = value
    page.value = 0
    fetchProblems()
}

const onDifficulty = (value: string) => {
    difficulty.value = value
    page.value = 0
    fetchProblems()
}

const onPageChange = (newPage: number) => {
    page.value = newPage
    fetchProblems()
}

const onSizeChange = (newSize: number) => {
    size.value = newSize
    page.value = 0
    fetchProblems()
}

// Initial fetch
fetchProblems()
</script>
```

- [ ] **Step 2: Type-check + build**

Run: `cd /home/tuan/Project/my-oj/judge-portal && npm run type-check`
Expected: no errors.

- [ ] **Step 3: Commit**

```bash
cd /home/tuan/Project/my-oj/judge-portal
git add src/pages/user/problem/ProblemListPage.vue
git commit -m "feat(problems): wire difficulty/search/tags filters into problem list"
```

---

## Task 8: End-to-end manual verification

**Files:** none (verification only).

- [ ] **Step 1: Start backend with a fresh, seeded DB**

```bash
cd /home/tuan/Project/my-oj/judge-api
docker compose down -v && docker compose up -d --build
```

Wait for the API to be healthy (check `docker compose logs -f judge-api` until it reports the port is listening).

- [ ] **Step 2: Verify the API returns tags and honors `hardnessLevel`**

```bash
# tags present on the list
curl -s "http://localhost:8080/api/v1/problems?page=0&size=10" | python3 -m json.tool | grep -A6 '"tags"' | head
# difficulty filter: Easy (1) returns the 3 seeded problems...
curl -s "http://localhost:8080/api/v1/problems?hardnessLevel=1" | python3 -c "import sys,json; print(len(json.load(sys.stdin)['data']),'problems')"
# ...Hard (3) returns none
curl -s "http://localhost:8080/api/v1/problems?hardnessLevel=3" | python3 -c "import sys,json; print(len(json.load(sys.stdin)['data']),'problems')"
```

Expected: tags array populated (e.g. `["Math","Strings"]`); `hardnessLevel=1` → `3 problems`; `hardnessLevel=3` → `0 problems`. (Adjust the base path if `Routes.BASE_API` differs from `/api/v1`.)

- [ ] **Step 3: Verify the UI**

Run the frontend (`cd /home/tuan/Project/my-oj/judge-portal && npm run dev`) and open the Problems page. Confirm:
- Typing in the keyword box filters the list after a short pause (debounced).
- The Difficulty dropdown: **Easy** shows problems; **Hard** shows an empty table; **All** restores them.
- The **Tags** switch toggles a Tags column with badges (off by default).
- The **Refresh** button re-runs the current query.
- Changing difficulty or search resets pagination to page 1.

- [ ] **Step 4: Run the full test suites once more**

```bash
cd /home/tuan/Project/my-oj/judge-api && ./mvnw -q test
cd /home/tuan/Project/my-oj/judge-portal && npm run test && npm run type-check
```

Expected: all green.

---

## Notes for the executor

- **Task 1 + Task 2 are one compile unit** — do not run a full `mvn compile`/`mvn test` between them; the service call site changes in Task 2. The single commit is at Task 2 Step 7.
- The `hardnessLevel` **SQL predicate** itself (native query correctness) is verified end-to-end in **Task 8 Step 2** (curl `hardnessLevel=1` → 3 problems, `=3` → 0), not by a unit test — there is no `@DataJpaTest`/H2 native-query infra in this repo, and the service test only proves the parameter is forwarded. This is the intended coverage for spec §8's difficulty-filter requirement.
- Do **not** add tag-filtering, a tag-assignment form, or URL sync — explicitly out of scope (see the design spec §9).
- If `./mvnw` is not present in judge-api, use `mvn`. If the frontend dev server port differs, use whatever `npm run dev` prints.
- Base API path: the plan assumes `Routes.BASE_API = /api/v1`. Verify against `judge-api/.../common/constant/Routes.java` if a curl 404s.
