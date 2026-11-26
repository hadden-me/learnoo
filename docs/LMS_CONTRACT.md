# LMS shared contract (LMS-B01)

Status: agreed contract for the LMS product-completion slices, 2026-11-26. It turns [LMS_PRODUCT_SPEC.md](LMS_PRODUCT_SPEC.md) into names, invariants, endpoints and test scenarios that the backend (B02–B04), Studio (B05–B06) and reader (B07–B08) share. Nothing here is implemented yet. Where this file and the spec disagree, the spec wins and this file is corrected.

The conventions of the existing system apply unchanged: guards raise SQLSTATE `LRN01` and are named `zz_guard_*`; trigger functions pin `search_path` and reach tables through `TG_TABLE_SCHEMA`; owner-run (`SECURITY DEFINER`) triggers are hardened like those of 0004 and 0007; views are `security_invoker`; guards bind the least-privilege runtime role and are tested under it; state-changing actions recheck permissions inside their transaction (`permissions.RequireTx`, `RequireReviewerTx`); audit rows are append-only and written in the same transaction as the change; JSON errors use `{"error":{"code","message"}}` ([API.md](API.md#conventions)). Where course behaviour is not stated here, it mirrors the lesson behaviour of 0004–0009 (status transitions, change requests, discard of drafts, approval validity, restore permissions).

## 1. Sequencing and migrations

- Migrations 0011 (`feat/submission-audit`) and 0012 (`feat/catalogue-names`, #86) are in flight and land first. LMS migrations start at **0013**, one migration owner at a time: 0013 curriculum (B02), 0014 courses and course revisions (B03), 0015 course review, publication, suspension, archive and dependency effects (B04), then reader changes (B08). A slice that needs a schema change after its migration merged adds a new migration; merged migrations are never edited.
- Course submission audit follows the pattern 0011 establishes for lessons. B04 starts after 0011 is merged.
- Names below are binding for code and tests; column lists are the minimum and may gain columns.

## 2. Global lock order

Every transaction takes locks in this order and only in this order: permission and account rows (the existing `RequireTx` convention) → curriculum editions (ascending ID) → lessons (ascending ID) → course → course revision → child rows.

- Making an edition current or retiring it takes the edition `FOR UPDATE`. Course save, submit, publish and restore take the editions their revision references `FOR SHARE`.
- Course save and submit take the referenced lesson rows `FOR SHARE`, then the course `FOR SHARE`, before the course revision row (so the author-equals-owner guard never takes the course lock after the revision lock). Publish and restore take them `FOR SHARE`, then the course `FOR UPDATE`.
- Review decisions take the course `FOR SHARE`, then the revision `FOR UPDATE`.
- Dependency triggers run under the lesson lock their writer already holds, then lock affected courses in ascending ID.
- The course-context report guard locks the lesson, then the course.

Each pair of paths that can meet has a race test.

## 3. Curriculum (B02, migration 0013)

| Table | Key columns and invariants |
| --- | --- |
| `education_levels` | `id`, `code` (stable, never repurposed), `name_fr`, `name_ar`, `display_order`, `retired_at`. Never deleted. |
| `academic_sections` | `id`, `level_id`, `code` unique per level, names, order, `retired_at`. Never deleted. |
| `curriculum_editions` | `id`, `section_id`, `code` unique per section, `label_fr`, `label_ar`, `state` ∈ `draft`/`current`/`superseded`/`retired`, `current_since`. Partial unique index: one `current` per section. Transitions: draft → current (the section's previous current edition becomes superseded in the same transaction), current/superseded → retired. Never back to draft. |
| `curriculum_assignments` | `id`, `edition_id`, `subject_id`; unique `(edition_id, subject_id)` and `(id, subject_id)` (FK target). |
| `curriculum_assignment_chapters` | `assignment_id`, `subject_id`, `chapter_id`, `position`; FK `(assignment_id, subject_id)` → assignments, FK `(chapter_id, subject_id)` → `chapters`; unique `(assignment_id, chapter_id)` and `(assignment_id, position)`. |
| `subjects`, `chapters` | gain `retired_at`: a retired entry cannot be newly selected; existing references keep resolving. |
| `curriculum_audit` | Every create, rename, reorder, retirement, state change and assignment edit: actor, target, action, before/after, time. |

Invariants:
- Assignments and their chapter lists change only while the edition is `draft` (guard). A change of meaning is a new edition.
- Existing subjects and chapters keep their IDs, codes and 0012 names/order. The migration places no chapter into any edition; an administrator does it, audited. Assignment chapter `position` is independent of catalogue display order.
- **Placement validity** is one SQL function, `course_revision_placement_valid(course_revision_id)`, used by save, submit and publish:
  - every assignment is in an edition whose section and level are not retired, and the edition is `current`, or it is `superseded` and the same assignment is on the revision's comparison revision (§5); an ordinary revision with any `retired` assignment fails save-time warning, submit and publish; for an **amendment** the assignments equal the comparison revision's, whatever their edition state (spec rule 2 exception, so withdrawn steps stay repairable);
  - every module chapter is in at least one of the revision's assignments, and is not retired unless inherited unchanged.
  - Restore does not re-check placement (it reinstates a snapshot that was valid when published); it re-checks step eligibility only.
- Each module maps to at most one chapter. A step's lesson chapter may differ from its module's (Studio warns, the server accepts).
- Standalone lessons are listed under an edition when the chapter of their published revision is in that edition's assignment. Lessons whose chapter is in no assignment, or NULL, are listed under their subject as "not yet placed". Discovery defaults to the current edition of a level/section; superseded and retired editions are reachable only through an explicit edition parameter and are labelled wherever shown.

## 4. Courses (B03, migration 0014)

| Table | Key columns and invariants |
| --- | --- |
| `courses` | `id`, `public_id` (uuid), `owner_id`, `subject_id`, `language`, `published_revision_id`, `archived_at`, `last_revision_number`, `dependency_count`, `created_at`, `created_by`. Unique `(id, subject_id, language)`. Identity columns immutable; `owner_id` changes only through transfer (§7). |
| `course_revisions` | `id`, `course_id`, `subject_id`, `language` (FK `(course_id, subject_id, language)` → courses), `revision_number`, `status` ∈ `draft`/`in_review`/`changes_requested`/`approved`, `kind` ∈ `ordinary`/`amendment` (derived by the server), `author_id`, `base_revision_id` (the pointer when the draft was created, may be NULL), `compare_revision_id` (§5), `title`, `summary`, `objectives`, `prerequisites`, `cover_media_id`, `cover_alt`, `version`, timestamps, `submitted_at`, `candidate_closed_at`. Unique `(id, subject_id, language)`; modules and steps reference it, so their subject and language equal the revision's. Status and submission constraints as for `lesson_revisions`, plus `withdrawn` handling below. Immutable after submission. Guard: on insert and on every draft update `author_id` equals the course's `owner_id` (course row locked `FOR SHARE`). |
| `course_revision_assignments` | `course_revision_id`, `subject_id`, `assignment_id`; FK `(assignment_id, subject_id)` → assignments; FK `(course_revision_id, subject_id)` → revisions. |
| `course_modules` | `course_revision_id`, `subject_id`, `language`, `module_key` (uuid, stable across revisions), `position`, `title`, `chapter_id` (nullable); FK `(course_revision_id, subject_id, language)` → revisions; FK `(chapter_id, subject_id)`. Unique `(course_revision_id, position)`, `(course_revision_id, module_key)`. |
| `course_steps` | `course_revision_id`, `module_key`, `position`, `title` (course-authored, reviewed with the structure), `lesson_id`, `subject_id`, `language`, `lesson_revision_id`. FK `(lesson_id, language, subject_id)` → `lessons (id, language, subject_id)`; FK `(lesson_id, lesson_revision_id)` → `lesson_revisions (lesson_id, id)`; FK `(course_revision_id, subject_id, language)` → revisions; FK `(course_revision_id, module_key)` → modules. Unique `(course_revision_id, lesson_id)` and `(course_revision_id, module_key, position)`. |

Invariants:
- Child rows (assignments, modules, steps) are inserted, changed or deleted only while their revision is a draft; the guard locks the parent revision `FOR SHARE`, like the `revision_media` guard.
- One active candidate per course, with the 0004 predicate: `draft` or `in_review`, or `approved` with `candidate_closed_at IS NULL`; a withdrawn revision is never active. A `changes_requested` revision is history and does not block the correction draft. A second draft or an amendment while a candidate is open is `409 active_candidate_exists`.
- Attachable lessons: same subject and language, and either the owner's own lesson or another author's lesson that is currently published. A draft step may name any revision of the owner's own lesson; for another author's lesson only an eligible revision (§5). Submission requires every step eligible.
- A lesson draft revision referenced by a course draft cannot be discarded: `409 referenced_by_course` (not a raw FK error).
- Hard delete of a course only when none of its revisions was ever submitted, by the owner, after confirmation; it removes course rows only, never lessons or media. The owner can discard an open draft revision (as for lessons).

## 5. Eligibility, availability and comparison (one definition each)

SQL functions or views, used by every caller (API, reader, guards). No caller re-implements them.

- **Eligible lesson revision** (`course_step_revision_eligible(lesson_id, revision_id)`): approved, not withdrawn, published at least once (`publish` row in `publication_audit`), lesson currently published and not archived (spec §4).
- **Step readable** (`reader_course_steps`): course readable and the step's revision eligible. An unreadable step shows only its course-authored `course_steps.title`, the notice, no body, no media; `lesson_revisions.title` is never read for an unreadable step.
- **Course readable** (`reader_published_courses`): pointer set, course not archived, published revision approved.
- **Comparison revision** (`compare_revision_id`, set when the draft is created): the current pointer, or if it is NULL the revision named by the last `publish` or `restore` row of `course_publication_audit`, or NULL for a course never published. Amendment classification and reviewer rule 3 compare against it.
- **Publishable**: every step eligible; placement valid (§3); approved by a reviewer eligible at decision time (approval validity after a later grant revocation mirrors lessons); `base_revision_id IS NOT DISTINCT FROM published_revision_id` (the 0007 rule, so a course suspended after its last publication can be republished from a draft created while suspended); course not archived. Stale candidates are released by §7 withdrawal.
- **Newer revision notice**: a step's lesson has an eligible revision with a higher `revision_number` than the pinned one. Visible to the owner, reviewers of the subject, publishers and administrators; never to students.

## 6. Course review eligibility

`course_reviewer_eligible(user_id, course_revision_id)` is true only when:
1. the user is active and holds `reviewer` for the course's subject;
2. the user is not the revision's `author_id`;
3. the user is not the `author_id` of any lesson revision the revision adds or replaces relative to its comparison revision (every step when there is none).

The approve and request-changes endpoints and a guard on `course_review_decisions` enforce it. The queue lists submitted course revisions in the reviewer's subjects with `eligible` for the caller; a revision with no eligible active reviewer carries `blocked: "no_eligible_reviewer"` and the disqualification reasons, also shown to the author and administrators. No role bypasses it.

An **amendment** differs from its comparison revision only by steps that keep their `lesson_id`, module, position and title and name a newer eligible revision. Anything else makes it `ordinary`. The review payload lists the changed steps (old and new revision, a content diff) and marks the rest unchanged.

## 7. Publication, suspension, archive, withdrawal, transfer (B04, migration 0015)

`course_publication_audit` mirrors `publication_audit` with actions `publish`, `restore`, `suspend` and `archive` (the last two set the pointer to NULL with a required reason). A guard on `courses` requires an audit row of the same transaction for every pointer change.

- **Publish** (publisher): §5 publishable; on failure nothing changes, `409 stale_base` or `409 ineligible` with the failing steps.
- **Suspend** (administrator, reason): pointer to NULL; the course leaves discovery and reading immediately.
- **Restore** (publisher or administrator, as for lessons): a previously published revision, every step eligible now, otherwise `409 ineligible`. Lifting a suspension is a restore.
- **Archive** (owner requests, administrator executes with a reason): sets `archived_at` and clears the pointer (`archive` row). Archiving a course whose pointer is already NULL (suspended or never published) writes only a `course_lifecycle_audit` row. **Unarchive** (administrator) clears `archived_at` only; access returns only through restore or a new publication.
- **Withdraw a course revision** (administrator, reason): `course_revision_withdrawals`; applies to `in_review` and `approved` revisions. An owner-run trigger sets `course_revisions.withdrawn_at` (a column the guard lets only that trigger set); the active-candidate predicate excludes rows with `withdrawn_at`, so the slot is freed. Review decisions, publication and restore of a withdrawn revision are refused (guard and API). `candidate_closed_at` keeps its 0004 meaning (approved and superseded by publication).
- **Transfer** (administrator, reason): refused with `409 active_candidate_exists` while a candidate is open (discard or withdraw first); locks the course `FOR UPDATE`, changes `owner_id`; history keeps its authors. The previous owner's lessons become another author's lessons for the course: still attachable while published, never editable by the new owner (0004).
- `course_lifecycle_audit` records archive requests, archive without a pointer change, unarchive and transfer (actor, course, action, reason, from/to owner, time); suspend, archive, publish and restore are in `course_publication_audit`; withdrawals in `course_revision_withdrawals`; submissions and decisions as for lessons.
- **Dependency effects**: owner-run `AFTER` triggers on `revision_withdrawals` inserts, on `publication_audit` `unpublish` rows and on lessons whose `archived_at` is set write one `course_dependency_events` row per affected published course and step (cause, time) and increment `courses.dependency_count`, which a guard lets change only from those triggers and only upward. The triggers refuse unconditionally (whether or not a course is affected) unless the transaction is `READ COMMITTED`, so lesson withdrawal, unpublish and archive under `REPEATABLE READ` or `SERIALIZABLE` now fail by design, so a stale snapshot cannot miss a course published concurrently. Lesson archival still has no application code path; the trigger covers it regardless. Reading never relies on these events: §5 views decide availability live.

## 8. API surface

JSON, under the existing authorization model. New error codes are added to API.md as each slice lands.

| Endpoint | Who | Notes |
| --- | --- | --- |
| `GET /api/catalogue/levels` … `/editions/{id}` | signed-in staff | names, order, state |
| `POST/PUT /api/admin/curriculum/...` | administrator | levels, sections, editions, assignments, chapter lists, make current, retire; `422 placement_invalid`, `409 edition_not_draft` |
| `GET /api/courses?mine=1` · `POST /api/courses` | author | owned courses; create with subject and language |
| `POST /api/courses/{id}/drafts` · `GET/PUT/DELETE /api/course-revisions/{id}` | owner | `expectedVersion`; `409 version_conflict`, `409 active_candidate_exists`; `422 placement_invalid`, `invalid_step` |
| `POST /api/course-revisions/{id}/submit` | owner | `422 incomplete` (field list), `409 ineligible` (step list) |
| `DELETE /api/courses/{id}` | owner | only never-submitted; else `409 has_history` |
| `GET /api/courses/{id}/newer-revisions` · `POST /api/courses/{id}/amendments` | read: owner, subject reviewers, publishers, administrators; create: owner | amendment draft from the notice |
| `GET /api/review/queue?type=course` · `GET /api/review/course-revisions/{id}` | reviewer of the subject | `eligible`, `blocked`, change list |
| `POST /api/review/course-revisions/{id}/approve` · `/request-changes` | eligible reviewer | `403 not_eligible_reviewer` with reason |
| `GET /api/publish/queue?type=course` · `POST /api/course-revisions/{id}/publish` | publisher | `expectedPublishedRevisionId`; `409 stale_base`, `409 ineligible` |
| `POST /api/courses/{id}/restore` | publisher, administrator | `409 ineligible` |
| `POST /api/courses/{id}/suspend` · `/archive` · `/unarchive` · `/transfer` · `POST /api/course-revisions/{id}/withdraw` | administrator | reason where stated |
| `POST /api/courses/{id}/archive-request` | owner | recorded for administrators |

## 9. Reader and public routes (B07–B08)

| Route | Access | Content |
| --- | --- | --- |
| `GET /` | public | real homepage |
| `GET /catalogue`, `/catalogue/{level}/{section}[/{subject}]?edition={code}` | public | current edition by default; courses and standalone lessons listed separately and labelled; earlier editions behind an explicit link |
| `GET /courses/{publicId}` | public | approved overview, module titles and step titles only |
| `GET /learn/courses/{publicId}` | signed in | outline with unavailable-step notices |
| `GET /learn/courses/{publicId}/lessons/{lessonPublicId}` | signed in | the pinned revision; says when it differs from the standalone version; previous/next stop at unavailable steps |
| `GET /learn/courses/{publicId}/lessons/{lessonPublicId}/media/{mediaId}` | signed in | media embedded in the pinned revision's content (the existing embed check), under step readability |
| existing `/learn/lessons/{publicId}` | signed in | unchanged |

- Course search uses `reader_published_courses`, course metadata and titles of readable steps only.
- Public pages are `Cache-Control: public, no-cache` with validators (every use revalidates, so withdrawal and suspension take effect immediately); all signed-in pages and media stay `private, no-store`.
- `course_bookmarks (user_id, course_id)` is new (B08); `bookmarks` is unchanged.
- `error_reports` gains nullable `course_revision_id`; a new migration (B08) replaces the 0009 insert guard so a course-context report names the pinned revision of a readable step of that published course revision, and other reports still name the lesson's current revision.

## 10. Test scenarios required before parallel work

Against real PostgreSQL, under the runtime role where guards matter:

- **A04 / A12 review**: course author self-approval refused (API and direct SQL); author of an added lesson revision refused; author of an unchanged included lesson allowed on an amendment; wrong-subject reviewer refused; revocation racing approval refused; no-eligible-reviewer revision reports `blocked` with reasons and cannot be published by any role.
- **A06 amendments**: notice for staff, not students; amendment changes only chosen steps; any other change makes it `ordinary`; atomic publication; stale base refused with nothing changed; replacement revision withdrawn before publication refused; amendment while a draft is open refused; amendment on a retired edition accepted; republication after suspension from a draft created while suspended.
- **A07 withdrawal**: withdraw a pinned revision, unpublish a lesson and archive a lesson, each racing course read, save, publish and restore: no withdrawn body, title or media served anywhere; overview and other steps readable; navigation stops at the notice; dependency event present; withdrawal under REPEATABLE READ refused even when no course is affected; suspension hides the course; restore with an ineligible step refused; archive then unarchive leaves the course unreadable until restore.
- **A08 lifecycle**: never-submitted delete keeps lessons and media; submitted history cannot be deleted; transfer refused with an open candidate; course revision withdrawal (in review and approved) frees the slot and blocks later decisions; a correction draft can start after changes are requested.
- **A09 curriculum**: module chapter outside the assignments refused on save, submit and publish; assignment edit after draft refused; make-current racing submit; new current edition changes no course; discovery defaults and labels.
- **A11 upgrade**: migrate a populated database from 0010 (plus 0011/0012) through the latest LMS migration; every existing ID, URL, bookmark and audit row unchanged and every standalone lesson still reads; C-locale database included.
