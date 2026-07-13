# Design — Problems list filters + tags column

**Date:** 2026-07-13
**Repos touched:** `judge-api` (backend: entity, DTO, query, seed), `judge-portal` (frontend: types, SearchBar, list page, table)
**Related:** builds on the existing `GET /problems` endpoint (already supports `search` + `status`).

## 1. Problem

The problems page (`ProblemListPage.vue`) ships with a filter bar (`SearchBar.vue`) that is **purely decorative** — a Difficulty dropdown, a Tags switch, a keyword input, and a Refresh button, none of which are wired to anything. `SearchBar` has no props, no emits, no state; it never talks to the list page. Meanwhile the backend `GET /problems` already accepts `search` and `status`, but the page only passes `search` (and even that is not driven by the visible input — the input is disconnected).

Two capabilities are missing:

1. **Filtering by difficulty** — the backend query has no `hardnessLevel` filter, and the dropdown is inert.
2. **A tags column with a show/hide toggle** — the Tags switch implies a tags column on the table, but **tags do not exist anywhere**: not in the `Problem` entity, not in `t_problems`, not in the DTO or the frontend `Problem` type. The sidebar `TagCloud` is hardcoded static strings.

## 2. Goals / Non-goals

**Goals**
- Difficulty dropdown (`All / Easy / Medium / Hard`) filters the list server-side via a new `hardnessLevel` query param.
- Keyword input drives the existing `search` param (debounced), replacing the disconnected input.
- Refresh button re-runs the current query.
- Add a lightweight `tags` field to problems (list of strings), returned by the API and **seeded in the DB for testing**.
- Add a **Tags column** to the problems table, shown/hidden by the Tags switch (default hidden, matching the mockup).
- Wire `SearchBar` ↔ `ProblemListPage` so all filters actually drive the fetch; changing any filter resets to page 0.

**Non-goals**
- **Filtering by tag** (clicking a tag to filter). Tags are display-only for now. The data model is chosen so tag-filtering can be added later without rework.
- Admin UI / form for assigning tags. There is no admin problem form today (`src/pages/admin` is empty); tags are populated by DB seed only.
- Persisting filter state in the URL query string (component-local state only).
- Making the sidebar `TagCloud` interactive; it stays as-is.
- Sorting changes; the existing default sort is kept.

## 3. Key facts (verified)

- **No tags anywhere.** `grep` over `judge-api/src` for "tag" hits only unrelated Swagger `@Tag` usage; `t_problems` in `initdb.sql` has no tags column; frontend `Problem` type has no `tags`.
- **`hardnessLevel` is an `int`** on `Problem` (`1 = easy, 2 = medium, 3 = hard`); the frontend already maps it via `getLevelInfo(level)` in `types/problem.ts`.
- **`GET /problems` list query is native SQL** (`findProblemsWithStats`) returning a `ProblemStatisticProjection` (problem columns + `totalSubmission` + `acceptedSubmission`) with `GROUP BY p.id`. It already filters `search` (title/description ILIKE) and `status`. A native projection cannot cleanly map a one-to-many collection, so tags must be loaded separately.
- **`ddl-auto: update`** (dev/prod profiles) → Hibernate auto-creates the `@ElementCollection` side table on startup. `initdb.sql` only runs on first DB init (`docker-entrypoint-initdb.d`), so seeding tags requires putting **both** the `CREATE TABLE` and the seed `INSERT`s in `initdb.sql` (the table must exist when the seed runs, before the app boots).
- **`SearchBar` is stateless** and structurally separate from the page; wiring is additive, not a rewrite.
- `ProblemSearchParams` today = `BaseSearchParams` (`page/size/search`) + `status?`.

## 4. Data model

### Backend — `Problem` entity (`judge-api`)
Add an element collection mapped to an explicit side table so the seed can target stable names:

```java
@ElementCollection(fetch = FetchType.LAZY)
@CollectionTable(name = "t_problem_tags", joinColumns = @JoinColumn(name = "problem_id"))
@Column(name = "tag")
@Builder.Default
private List<String> tags = new ArrayList<>();
```

- Table `t_problem_tags(problem_id uuid, tag varchar)`, FK → `t_problems(id)`.
- `ProblemResponseDto` gains `private List<String> tags;`.
- `ProblemMapper` maps `tags` when mapping from a `Problem` **entity** (create/update responses). The **list** path returns a native projection with no collection, so the service attaches tags after a batch load (§5). The **detail** path (`getProblemBySlug`, also projection-based) is unchanged — tags on the detail DTO are not required for this feature and may be null there.

### Seed (`judge-api/initdb.sql`)
- Add a `CREATE TABLE public.t_problem_tags (...)` block (matching the JPA-generated shape) with the FK constraint, placed consistently with the other table/constraint sections.
- Add seed `INSERT`s attaching a handful of tags (e.g. `DP`, `Math`, `Greedy`, `Graphs`, `Strings`) to the existing seeded problem IDs in the `t_problems` COPY block, so the column has data on a fresh DB.

## 5. Backend — filter API (`GET /problems`)

**Controller** (`ProblemController.getProblems`): add
```java
@RequestParam(required = false) Integer hardnessLevel
```
and pass through to the service. `search` and `status` unchanged.

**Repository** (`findProblemsWithStats`): add the difficulty predicate to **both** the main and the count query:
```sql
AND (:hardnessLevel IS NULL OR p.hardness_level = :hardnessLevel)
```
Method signature gains `Integer hardnessLevel`.

**Tags loading (avoids N+1 and array parsing):** in `ProblemService.getProblems`, after fetching the page of projections:
1. Collect the page's problem IDs.
2. If non-empty, run a second query — a new repository method
   `List<ProblemTagRow> findTagsByProblemIds(Collection<UUID> ids)` returning `(problemId, tag)` rows (`SELECT problem_id, tag FROM t_problem_tags WHERE problem_id IN (:ids)`).
3. Group into `Map<UUID, List<String>>` and set `tags` on each DTO before returning the `PageResponse`.

One extra query per page; empty page → skip the query.

**Service signature:** `getProblems(int page, int size, String search, ProblemStatus status, Integer hardnessLevel)`.

## 6. Frontend — integration (`judge-portal`)

### Types (`types/problem.ts`, `types/common.ts`)
- `Problem` gains `tags?: string[]`.
- `ProblemSearchParams` gains `hardnessLevel?: number`.

### `SearchBar.vue` — stateless → controlled
- Props: `search: string`, `difficulty: string` (`'all' | 'easy' | 'medium' | 'hard'`), `showTags: boolean`.
- Emits: `update:search`, `update:difficulty`, `update:showTags`, `refresh`.
- Keyword input is debounced (~350 ms) before emitting `update:search`.
- The Difficulty `Select` and Tags `Switch` become controlled by the props; Refresh emits `refresh`.

### `ProblemListPage.vue` — owns filter state
- Add refs: `difficulty = 'all'`, `showTags = false` (keep existing `page/size/search`).
- Map difficulty → `hardnessLevel`: `easy→1, medium→2, hard→3, all→undefined`.
- `fetchProblems()` includes `hardnessLevel`.
- Any filter change (`search`, `difficulty`) → `page = 0` then refetch; `refresh` → refetch current query.
- Bind `SearchBar` via the props/emits above; pass `:show-tags="showTags"` to `ProblemTable`.

### `ProblemTable.vue` — Tags column
- New prop `showTags: boolean`.
- Add a `Tags` `TableHead` + `TableCell` (placed after Title) that renders `problem.tags` as `Badge`s.
- Header cell and body cell render only when `showTags` is true (`v-if`), so toggling the switch shows/hides the whole column.
- Empty/absent `tags` → render nothing (or a subtle dash) in the cell.

## 7. Behavior details

- **Difficulty encoding:** dropdown values `all/easy/medium/hard` ↔ `hardnessLevel` `undefined/1/2/3`. Backend treats missing `hardnessLevel` as "no filter".
- **Debounce:** keyword ~350 ms; each committed change resets page to 0.
- **Tags switch:** client-only column visibility; default off; not persisted.
- **Refresh:** re-executes the current filter set (no state reset).
- **Loading/error:** reuse the page's existing `useFetch` `isLoading`/`error` handling and skeleton.

## 8. Testing

**Backend (`judge-api`, H2 `application-test.yml`):**
- Repository test: `findProblemsWithStats` filters correctly by `hardnessLevel` (matches / excludes / null = all), composed with `search`.
- Service test: `getProblems` attaches the right tags per problem (batch-load + grouping), and returns empty `tags` for problems with none.

**Frontend (`judge-portal`, existing Vitest setup):**
- `SearchBar` emits `update:*`/`refresh` on interaction; keyword emit is debounced.
- `ProblemTable` renders the Tags column only when `showTags` is true, and renders each problem's tag badges.
- (If a page-level test pattern exists) difficulty change maps to `hardnessLevel` and resets page to 0.

## 9. Out of scope (future)

- Filtering / querying by tag (`tag` param + JOIN + distinct-tags endpoint for a live `TagCloud`).
- Admin form to assign tags to a problem (`CreateProblemDto` / `UpdateProblemDto` tag fields).
- URL-synced filter state (shareable/bookmarkable filters, back-button support).
