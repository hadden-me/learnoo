# LMS shared contract (LMS-B01)

Status: agreed contract for the LMS product-completion slices, 2026-11-26. It turns [LMS_PRODUCT_SPEC.md](LMS_PRODUCT_SPEC.md) into names, invariants, endpoints and test scenarios that the backend (B02–B04), Studio (B05–B06) and reader (B07–B08) share. Nothing here is implemented yet. Where this file and the spec disagree, the spec wins and this file is corrected.

The conventions of the existing system apply unchanged: guards raise SQLSTATE `LRN01` and are named `zz_guard_*`; trigger functions pin `search_path` and reach tables through `TG_TABLE_SCHEMA`; state-changing actions recheck permissions inside their transaction (`permissions.RequireTx`, `RequireReviewerTx`); audit rows are append-only and written in the same transaction as the change; lock order is parent row first, then child rows; JSON errors use `{"error":{"code","message"}}` ([API.md](API.md#conventions)).

## 1. Sequencing and migrations

- Migrations 0011 (`feat/submission-audit`) and 0012 (`feat/catalogue-names`, #86) are in flight and land first. LMS migrations start at **0013**, one migration owner at a time, in this order: 0013 curriculum editions (B02), 0014 courses and course revisions (B03), 0015 course review, publication, suspension and dependency effects (B04). A slice that needs a schema change after its migration merged adds a new migration; merged migrations are never edited.
- Course submission audit follows the pattern 0011 establishes for lessons (an append-only submission row in the submitting transaction). B04 starts after 0011 is merged.
- Names below are binding for code and tests; column lists are the minimum and may gain columns.

## 2. Curriculum (B02, migration 0013)

| Table | Key columns and invariants |
| --- | --- |
| `education_levels` | `id`, `code` (stable, never repurposed), `name_fr`, `name_ar`, `display_order`, `retired_at`. Never deleted. |
| `academic_sections` | `id`, `level_id`, `code` unique per level, names, order, `retired_at`. Never deleted. |
| `curriculum_editions` | `id`, `section_id`, `code` unique per section, `label_fr`, `label_ar`, `state` ∈ `draft`/`current`/`superseded`/`retired`, `current_since`. Partial unique index: at most one `current` per section. Transitions: draft → current (the previous current edition of the section becomes superseded in the same transaction), current/superseded → retired. No transition back to draft. |
| `curriculum_assignments` | `id`, `edition_id`, `subject_id`; unique `(edition_id, subject_id)`. |
| `curriculum_assignment_chapters` | `assignment_id`, `chapter_id`, `subject_id`, `position`; FK `(chapter_id, subject_id)` → `chapters`; the subject must equal the assignment's subject (composite FK on `curriculum_assignments (id, subject_id)`). |
| `curriculum_audit` | Every create, rename, reorder, state change and assignment edit: actor, target, action, before/after, time. |

Invariants:
- Assignments and their chapter lists are editable only while the edition is `draft`. Afterwards they are immutable (guard). Changing meaning means a new edition.
- Existing `subjects` and `chapters` keep their IDs, codes and the display names/order from 0012. No existing chapter is placed into an edition by the migration; an administrator does it explicitly (audited).
- Newly selectable: the edition is `current` and its section and level are not retired. A successor may keep a `superseded` assignment inherited from its base. `retired` editions cannot be submitted or published in an ordinary revision; an amendment (assignments identical to its base) is allowed so withdrawn steps can be repaired. Each module maps to at most one chapter; a step's lesson chapter may differ from its module's (Studio warns). Assignment chapter `position` is independent of the catalogue display order from 0012.
- Standalone lessons are listed under an edition when the chapter of their published revision is in that edition's assignment; lessons whose chapter is in no assignment, or NULL, are listed under their subject as "not yet placed".

Discovery default: for a level/section, the current edition. Superseded editions are reachable only through an explicit edition parameter and are labelled with their edition label wherever shown.

## 3. Courses (B03, migration 0014)

| Table | Key columns and invariants |
| --- | --- |
| `courses` | `id`, `public_id` (uuid, URL identity), `owner_id`, `subject_id`, `language` (`fr`/`ar`), `published_revision_id`, `archived_at`, `last_revision_number`, `dependency_count` (lock/serialization counter, like `lessons.withdrawal_count`), `created_at`, `created_by`. Identity columns immutable. Never deleted once any revision was submitted. |
| `course_revisions` | `id`, `course_id`, `revision_number`, `status` ∈ `draft`/`in_review`/`changes_requested`/`approved`, `kind` ∈ `ordinary`/`amendment`, `author_id`, `base_revision_id` (the published revision when the draft was created, or NULL), `title`, `summary`, `objectives`, `prerequisites`, `cover_media_id`, `cover_alt`, `version` (optimistic lock), `created_at`, `updated_at`, `submitted_at`, `candidate_closed_at`. Same status/submission constraints as `lesson_revisions`. Immutable after submission (guard). |
| `course_revision_assignments` | `course_revision_id`, `assignment_id`. The assignment's subject equals the course's subject (FK through `curriculum_assignments (id, subject_id)`). |
| `course_modules` | `course_revision_id`, `module_key` (uuid stable across revisions, for diffs), `position`, `title`, `chapter_id` (nullable), `subject_id`. Unique `(course_revision_id, position)` and `(course_revision_id, module_key)`. |
| `course_steps` | `course_revision_id`, `module_key`, `position`, `lesson_id`, `lesson_revision_id`. FK `(lesson_id, lesson_revision_id)` → `lesson_revisions (lesson_id, id)`; unique `(course_revision_id, lesson_id)` (no duplicate lesson in a course); the lesson's subject and language equal the course's. |

Invariants:
- One active candidate per course (draft, in review, or approved awaiting publication), enforced by a partial unique index like lessons.
- Draft saves accept incomplete structure but always enforce: owner and author permission, subject/language match of every step, no duplicates, and module chapter mapping inside the revision's assignments (§2 rule 2 of the spec). The mapping check runs on save, on submission and at publication, against the edition state at that moment.
- Attachable lessons have the course's subject and language and are either the owner's own (`lessons.created_by` = owner) or another author's lesson that is currently published. A draft step may name any revision of the owner's own lesson, including one still in draft or review, but only an eligible revision of another author's lesson; submission refuses until every step names an eligible revision (§4).
- Hard delete only for a course whose revisions were never submitted, by the owner, after confirmation. It removes course, revision, module and step rows only; lessons and media are untouched.
- Ownership transfer (administrator, reason, audited): changes `owner_id` only; submitted history keeps its `author_id`; an open draft of the previous owner is closed (kept for history) and the new owner starts a successor.

## 4. Eligibility and availability (one definition each)

These are SQL functions or views used by every caller (API, reader, publication guards). No caller re-implements them.

**Lesson revision eligible for a course step** (`course_step_revision_eligible(lesson_id, revision_id)`): the revision is `approved`, not withdrawn, has been published at least once (a `publish` row in `publication_audit`), and its lesson is currently published (`published_revision_id IS NOT NULL`) and not archived (spec §4). A course never makes a lesson revision readable that the publisher has not released standalone, and an approved candidate awaiting publication cannot be pinned.

**Step readable** (`reader_course_steps` view): the course is readable and `course_step_revision_eligible` holds for the step. A step that is not readable is shown as an unavailable step: title from the course snapshot, the notice, no lesson body, no media. Withdrawal of the pinned revision, an administrator unpublish of the lesson, and archiving the lesson all make the step unreadable immediately, in the same snapshot rules as `reader_published_lessons`.

**Course readable** (`reader_published_courses` view): `published_revision_id` set, not archived, the published revision `approved`. A suspended course has no published revision (§6), so it is not readable; its overview shows the generic unavailable notice.

**Course revision publishable**: course readable conditions on the candidate, plus every step eligible, every module mapping inside the revision's selectable assignments, approval by an eligible reviewer (§5), base equals the current published pointer (or NULL and never published), and the course not archived. Checked in one transaction under the lock order of §6.

**Newer revision notice**: for each step of a published course, the lesson has an eligible revision newer than the pinned one (higher `revision_number`). An amendment while an ordinary draft is open is refused with `409 draft_exists`. Shown in Studio to the owner, the subject's reviewers, publishers and administrators; never to students.

## 5. Course review eligibility

`course_reviewer_eligible(user_id, course_revision_id)` is true only when:
1. the user holds `reviewer` for the course's subject and is active;
2. the user is not the revision's `author_id`;
3. the user is not the `author_id` of any lesson revision that the revision *adds or replaces* relative to its base (steps whose `(lesson_id, lesson_revision_id)` is not in the base revision; for a first revision every step).

The approve endpoint and a database guard on `course_review_decisions` both enforce it. "Request changes" uses the same rule. The queue lists every submitted course revision in the reviewer's subjects with a flag `eligible` for the caller; a revision with no eligible active reviewer at all carries `blocked: "no_eligible_reviewer"` and the list of disqualification reasons (author of the course revision; author of lesson X), shown to the author and administrators too. No role bypasses the rule.

An **amendment** (`kind = amendment`) is valid only when, compared with its base: metadata, assignments, modules (keys, titles, order, chapters) and step order are identical, and each changed step keeps the same `lesson_id` with a newer approved `lesson_revision_id`. The server derives `kind`; a draft that changes anything else is `ordinary`. The review payload lists each changed step (old and new revision IDs, titles, a content diff) and marks all others unchanged.

## 6. Publication, suspension, archive, restore (B04, migration 0015)

`course_publication_audit` mirrors `publication_audit`: actions `publish`, `restore`, `suspend` (pointer to NULL, reason required), with `previous_revision_id`. A guard on `courses` requires an audit row of the same transaction for every pointer change.

- **Publish**: publisher permission, §4 publishable, in one transaction; on conflict nothing changes and the response is `409 stale_base` or `409 ineligible` with the failing steps.
- **Suspend** (administrator, reason): pointer to NULL. The course leaves discovery and reading immediately. Bookmarks and history stay.
- **Lift suspension** = restore a previously published revision of that course (`restore`): administrator, the revision was published before, every step eligible now. If a step is no longer eligible the restore is refused (`409 ineligible`) and the course needs a reviewed revision (amendment or ordinary) instead.
- **Archive**: owner requests, administrator executes with a reason; sets `archived_at`; unarchive never republishes.
- **Dependency effects**: withdrawing a lesson revision, unpublishing or archiving a lesson writes a `course_dependency_events` row per affected published course (course, step, cause, time) in the same transaction, and increments `courses.dependency_count` so a concurrent stale course publish or restore fails with a serialization error rather than acting on stale eligibility. Lock order everywhere: lesson rows first (ascending ID), then course rows (ascending ID). Dependency writers lock the lesson, then each affected course. Course publication and restore lock the referenced lesson rows `FOR SHARE` in ascending order, then the course row `FOR UPDATE`, then re-check eligibility. Both paths are covered by a race test.

## 7. API surface

JSON, under the existing authorization model. New error codes are added to the table in API.md as each slice lands.

| Endpoint | Who | Notes |
| --- | --- | --- |
| `GET /api/catalogue/levels` … `/editions/{id}` | any signed-in staff | names, order, state |
| `POST/PUT /api/admin/curriculum/...` | administrator | draft editions, assignments, chapter lists, make current, retire; `422 placement_invalid`, `409 edition_not_draft` |
| `GET /api/courses?mine=1` · `POST /api/courses` | author | owned courses; create with subject and language |
| `GET/PUT /api/course-revisions/{id}` · `POST /api/courses/{id}/drafts` | owner | `expectedVersion`; `409 version_conflict`; `422 placement_invalid`, `invalid_step` |
| `POST /api/course-revisions/{id}/submit` | owner | `422 incomplete` with field list; `409 ineligible` with step list |
| `DELETE /api/courses/{id}` | owner | only never-submitted; else `409 has_history` |
| `GET /api/courses/{id}/newer-revisions` · `POST /api/courses/{id}/amendments` | owner (read: staff of the subject) | creates an amendment draft from the notice |
| `GET /api/review/queue?type=course` · `GET /api/review/course-revisions/{id}` | reviewer of the subject | `eligible`, `blocked`, change list |
| `POST /api/review/course-revisions/{id}/approve` · `/request-changes` | eligible reviewer | `403 not_eligible_reviewer` with reason code |
| `POST /api/course-revisions/{id}/publish` | publisher | `expectedPublishedRevisionId`; `409 stale_base`, `409 ineligible` |
| `POST /api/courses/{id}/suspend` · `/restore` · `/archive` · `/unarchive` · `/transfer` | administrator | reason required where stated |

## 8. Reader and public routes (B07–B08)

| Route | Access | Content |
| --- | --- | --- |
| `GET /` | public | real homepage |
| `GET /catalogue`, `/catalogue/{level}/{section}[/{subject}]?edition={code}` | public | current edition by default; courses and standalone lessons listed separately and labelled; earlier editions behind an explicit link |
| `GET /courses/{publicId}` | public | approved overview and outline titles only; never lesson bodies, solutions or media |
| `GET /learn/courses/{publicId}` | signed in | outline with unavailable-step notices |
| `GET /learn/courses/{publicId}/lessons/{lessonPublicId}` | signed in | the pinned revision; says when it differs from the standalone current version; previous/next stop at unavailable steps with the notice |
| `GET /learn/courses/{publicId}/lessons/{lessonPublicId}/media/{mediaId}` | signed in | media of the pinned revision only, under step readability |
| existing `/learn/lessons/{publicId}` | signed in | unchanged: the current standalone publication |

Changes to existing pieces, each in the slice named:
- `course_bookmarks (user_id, course_id)` is a new table (B08); `bookmarks` is unchanged.
- `error_reports` gains nullable `course_revision_id`; the 0009 insert guard is replaced (new migration, B08) so a report may name the displayed revision when it is the pinned revision of a readable step of that published course revision, and otherwise still only the lesson's current revision.
- `reader_published_lessons` stays the standalone definition; course reading uses `reader_course_steps`. The media route for course context checks that the media belongs to the pinned revision of a readable step (B08). All reader responses keep `Cache-Control: private, no-store` except the public homepage/catalogue/overview, which may be cached only when they contain no identity-dependent content.

## 9. Test scenarios required before parallel work

Each slice's tests include, against real PostgreSQL (and the runtime role where guards matter):

- **A04 / A12 review**: course author self-approval refused (API and direct SQL); author of an added lesson revision refused; author of an unchanged included lesson allowed on an amendment; wrong-subject reviewer refused; reviewer whose permission is revoked mid-request refused; a revision with no eligible reviewer reports `blocked` with reasons and cannot be published by publisher or administrator.
- **A06 amendments**: the newer-revision notice appears for staff and not students; an amendment changes only the chosen steps; changing anything else makes it `ordinary`; publication swaps all changed steps atomically; a stale base is refused and changes nothing; an amendment whose new lesson revision is withdrawn before publication is refused.
- **A07 withdrawal**: withdraw a pinned revision, unpublish a lesson and archive a lesson, each racing a course read, publish and restore: no withdrawn body or media is served in any context; the overview and other steps stay readable; navigation stops at the notice; the dependency event exists; suspension hides the course; lifting it with an ineligible step is refused.
- **A09 curriculum**: module mapped outside the course's assignments refused on save, submit and publish; assignment edit after the edition leaves draft refused; making an edition current supersedes the previous one atomically and changes no course's assignments; discovery defaults to the current edition and labels earlier ones.
- **A11 upgrade**: migrate a database populated with lessons, revisions, bookmarks, reports and audit rows from 0010 (plus 0011/0012) through 0015; every existing ID, URL, bookmark and audit row is unchanged and every standalone lesson still reads; C-locale database included.
