# Learnoo — complete LMS product specification

Status: specification for the next product-completion milestone, 2026-11-26. Not a claim of implemented or deployed functionality.

The maintainer requested teacher-managed courses, subject and level assignment, and a real student entry experience. This document defines that scope; the implementation plan incorporates it by reference. Existing security and editorial safeguards remain mandatory. Two labels distinguish the status of choices. **Maintainer decision (2026-11-26)** marks rules the maintainer confirmed when reviewing this specification; they are binding. **Working default** marks recommendations supplied with this specification, not separately confirmed; implement them in reviewable slices and escalate only an actual conflict or irreversible consequence, not ordinary implementation details.

## 1. Product and completion boundary

Learnoo is a free curriculum-organized learning platform: teachers manage courses in Studio, independent reviewers validate educational content, and students discover and read published learning material. It is not yet a classroom-management system: cohorts, attendance, gradebooks, assignment submissions, payments, certificates and student messaging are excluded.

A usable internal LMS includes a homepage, discovery catalogue, course overview, student home, teacher course management, lesson editing, review/publication, and an administrator catalogue interface. A collection of working APIs or a direct lesson URL does not satisfy this milestone. One BAC section and subject constrain real pilot content, not the ability of the software to represent levels and sections.

Public launch additionally requires self-registration/recovery, the recorded verification/privacy decisions, real content and operational approval. These gates do not block local implementation using labelled test fixtures. No dates, legal permissions or educational approvals are inferred.

## 2. Vocabulary and data relationships

| Entity | Meaning and invariant |
| --- | --- |
| Education level | Stage/year, initially BAC; extensible through administrator-managed entries. Not the BAC track. |
| Academic section | Track within a level, such as Mathematics. Stable ID; localized display names. |
| Subject | Discipline with stable identity; available through curriculum assignments, not duplicated for each track. |
| Curriculum version (edition) | Named edition of the curriculum for one level and section (for example "BAC Mathematics, 2026 edition"). States: draft, current, superseded, retired. At most one current edition per level/section. Historical associations remain resolvable. |
| Curriculum assignment | The subject as taught in one edition: (edition, subject) plus the ordered list of official chapters that edition includes for that subject. Immutable once the edition leaves draft; a change of meaning is a new edition. |
| Curriculum chapter | Official catalogue topic maintained by administrators: the existing subject-scoped `chapters` identity (stable ID, never deleted or repurposed). One chapter can appear in several editions' assignments. Distinct from a teacher's course module. |
| Course | Teacher-owned learning collection with overview, curriculum placement and ordered modules/lessons. It is not a synonym for chapter or lesson. |
| Module | Teacher-created organizational group inside a course; title and order are part of the course's reviewed structure. |
| Lesson | Stable educational identity with independently reviewed immutable revisions. |
| Course revision | Immutable submitted snapshot of overview, curriculum assignments and ordered module/lesson references. |

**Working defaults:** a course has one owner, one primary subject, one content language and one or more compatible versioned curriculum assignments. The UI begins with one level/section selection and permits additional compatible assignments. Cross-subject courses are out of scope. A course module may map to an official chapter but cannot create/rename the official catalogue. Drafts may be incomplete; submission requires complete placement and metadata.

Published course snapshots reference exact approved lesson revisions, not silently changing live lesson pointers. A lesson can remain available at its standalone stable URL while course navigation uses the revision selected by the published course snapshot. Publication of a new standalone lesson revision does not silently change an existing course. Course endpoints must validate those references and enforce the same withdrawal/access rules (§5). Define this API contract before modifying the reader.

### Curriculum integrity

**Maintainer decision (2026-11-26).** The server enforces each rule; client checks are convenience only.

1. A course selects one or more curriculum assignments, all for the course's primary subject. Several editions may be selected only when each is explicitly chosen; selecting one edition never implies another.
2. A module may map to an official chapter only if that chapter belongs to at least one of the course's selected assignments. A mapping outside them is rejected server-side on save, on submission and again at publication (the catalogue may have changed in between). Only the current edition of a level/section can be newly selected; a successor may keep a superseded assignment inherited from its base. Retired editions cannot be submitted; drafts that still reference them must be corrected first. Each module maps to at most one chapter. A lesson's own chapter need not equal its module's chapter, but must be in the same subject; Studio shows the mismatch. Because assignments are immutable once their edition leaves draft, the publication re-check catches edition state changes (such as retirement).
3. A new or newly current edition never changes a course snapshot's assignments or a lesson revision's stored subject and chapter. Published course snapshots keep the assignments they were reviewed with. Standalone lessons appear under an edition only because that edition's assignment includes their stored chapter (rule 5); that is a listing, not a reclassification. Bringing a course into a new edition is a new course revision with independent review.
4. Students browse the current edition of their selected (or chosen) level/section by default. Superseded editions carry an explicit edition label everywhere they appear and are reached only through a deliberate "earlier editions" path, never mixed silently into current results.
5. Standalone lessons keep their existing placement: the subject and official chapter recorded on each revision. They remain readable at their stable URLs and discoverable under every edition whose assignment includes that chapter, labelled as a lesson and visually distinguished from courses. A lesson whose chapter is in no assignment stays readable by URL, bookmark and search, and is listed under its subject as "not yet placed in a curriculum edition"; it is never given an invented level or section. Revisions with no chapter (allowed today) are in this group. A published course whose only edition is retired stays readable by URL and bookmark and is listed only under that edition's labelled path. An assignment's chapter list is ordered independently of the catalogue display order (#86).
6. Migration keeps every existing ID, URL, bookmark, audit row and historical association. Placing existing chapters into editions is an explicit administrator action, audited, not an automatic backfill guess.

## 3. Roles and ownership

Preserve the confirmed independent permissions: `author`, subject-scoped `reviewer`, `publisher`, `administrator`. Student accounts carry none. Teacher defaults to author; Administrator defaults to administrator, publisher and author. Neither publisher nor administrator implies reviewer.

| Action | Who may do it |
| --- | --- |
| Browse public homepage/catalogue/overviews | Everyone; no full lesson or solution content exposed |
| Read full published lessons/courses; bookmark/report | Signed-in active accounts |
| Create a course or lesson | Active author |
| Edit course, modules, order or lesson draft | Owner with active author permission |
| Submit owned work | Owner; valid content and catalogue placement required |
| Review | Reviewer assigned to the subject who is independent under §5 "Independent review" |
| Publish | Publisher, only after required independent approval |
| Urgent unpublish/withdraw; ownership recovery | Administrator, with reason and audit; no approval bypass |
| Maintain catalogue and grant permissions | Administrator through protected UI; bootstrap CLI remains supported |

**Working default:** no collaborative editing in this milestone. Course ownership transfer (lesson ownership is unchanged and stays deferred to LRN-019) is an explicit administrator action with reason, audit, target-author validation and conflict handling. Original revision authorship is never rewritten. A new owner edits successors, not submitted history. Reviewer eligibility follows §5 "Independent review"; publication may be performed by the author if separately authorized after independent approval.

## 4. Teacher course lifecycle (CRUD)

### Create and read

`My courses` lists owned courses with title, placement, language, state, last update and actionable feedback. Teachers can filter/search their list, start a course, and open its outline or preview. All lists require pagination, accessible empty states and recoverable loading/error states.

New course fields: title, short public description, learning objectives, prerequisites, language, subject, level/section assignments, optional cover image with alt text. Descriptions cannot contain full protected lesson bodies. Validate lengths and allowed markup; document exact limits in the API contract.

Teachers add/rename/reorder modules, create lessons, and attach compatible lessons: their own, or another author's lesson of the same subject and language that is published. A step pins one **eligible lesson revision**: approved, not withdrawn, published at least once (a `publish` audit row), and its lesson currently published and not archived. An approved revision still awaiting standalone publication cannot be pinned, so a course never releases content the publisher has not released, and withdrawing a stale lesson candidate never breaks a course. Initial draft saving must not require a complete course. Prevent duplicates and invalid cross-subject/language attachments. Drag sorting has keyboard/button alternatives.

### Update and review

Explicit save with optimistic version checks; retain unsaved edits on conflicts, expired sessions and network errors. Account switches must never submit another user's edits. Preview uses the canonical renderer and clearly identifies draft material.

Lessons retain their existing independent workflow. A course revision can be submitted only with nonempty modules/outline, complete metadata and eligible approved lesson revisions. Course review validates the overview, sequence, completeness and curriculum fit; it does not replace lesson approval. Changes requested require a comment; corrections create a new course revision. Published course metadata/order remains unchanged while a successor is drafted/reviewed.

### Lesson corrections and course amendments

**Maintainer decision (2026-11-26).**

- Published course structure and lesson references stay versioned. Nothing changes a published reference except publishing a reviewed course revision.
- When a lesson referenced by a published course has a newer eligible revision than the one the course pins, Studio shows the course owner, the subject's reviewers, publishers and administrators that a newer approved revision is available, with the lessons concerned.
- A **course amendment** is a course revision whose only differences from the published base are replacements of pinned lesson revisions by newer approved revisions of the *same* lessons. The owner creates it in one action from that notice. The review page shows exactly what changed (old and new revision per step, with a content diff) and marks every other step as unchanged; reviewers do not re-review unchanged lessons. It needs independent, subject-authorized course approval (§5) and publishes atomically like any course revision. Any other change (order, modules, metadata, placement, added or removed lessons, or going back to an older revision) is an ordinary course revision. Only one candidate per course exists: if an ordinary draft is open, the amendment is refused with a named reason (`draft_exists`) and the owner may discard the draft or add the replacement to it.
- An amendment is not the emergency path. Content known to be unsafe or materially incorrect is withdrawn immediately (§5); the amendment that restores the step follows later.

### Delete, archive and restore

**Working default:** owners may permanently delete never-submitted course drafts after confirmation if no retained history/references require them; deleting a course draft never deletes shared lessons or media. Once submitted, keep identity/history: use archive or discard-current-draft instead of destructive deletion. Teachers may request archival of published courses; administrators execute it with a reason. Archiving removes discovery/read access but retains history and bookmark records with an unavailable state. Unarchiving never automatically republishes. Restoration rechecks every included revision and compares the expected current pointer transactionally. No cascade deletion of published learning content.

## 5. Publication and withdrawal safety

Keep one active candidate per lesson and per course: draft, in review or approved awaiting publication. Database constraints enforce it. Separate review state, archive state and published pointer.

Publishing a course atomically checks owner/subject metadata, independent approval, expected published base, and eligibility of every referenced lesson/media revision. Never expose a partially reviewed outline. A conflicting publish returns a recoverable conflict without changing visible content.

### Independent review

**Maintainer decision (2026-11-26).** Lesson approval and course-structure approval are separate decisions.

- Every lesson revision a course revision references must already be independently approved under the existing lesson rules. Course review never approves lesson content.
- The course reviewer must hold the reviewer permission for the course's subject and must not be the author of the course revision (its overview, metadata, placement, modules and order).
- Having authored an included lesson does not by itself disqualify a course reviewer. A reviewer may not approve a course revision that *adds, or replaces the pinned revision with,* a lesson revision they authored, because that is a change to their own lesson content in the course. Steps whose pinned revision is unchanged from the published base do not count. For a first course revision every step is an addition.
- If no eligible reviewer exists for a submitted course revision, it stays in review and cannot be published. The queue and the author's view say so and name what is missing (for example "no reviewer for Mathematics other than the course author and the author of lesson X"). Administrators see the same gap. Nothing approves on anyone's behalf.
- These rules are enforced server-side and in database guards, and each has a test: course author self-approval, approval by the author of an added or replaced lesson revision (refused), approval by the author of an unchanged included lesson (allowed), wrong-subject reviewer, and the no-eligible-reviewer state.

### Withdrawn lessons and course suspension

**Maintainer decision (2026-11-26).**

- A withdrawn lesson revision is unreadable immediately in every context: standalone, inside any course, previews outside Studio, search, overviews and media. Likewise, a lesson that an administrator has unpublished or archived is unreadable inside courses too, even though the course pins a revision that is not itself withdrawn; an emergency unpublish must not be bypassed through a course. A course step is readable only while its pinned revision is not withdrawn and its lesson is published and not archived. If an unpublished lesson is later restored, its steps return; content judged unsafe must therefore be withdrawn, not just unpublished.
- By default the course stays available: its overview and every unaffected step remain readable. The affected step stays in the outline with an explicit "this lesson is temporarily unavailable" notice, and previous/next navigation stops at it with the notice instead of skipping it. The course is never presented as complete while a step is unavailable. No other revision is substituted.
- An administrator can **suspend** the whole course (reason required, audited) when the missing step makes the rest misleading or unsafe. A suspended course disappears from discovery and reading; its overview shows a generic unavailable notice; bookmarks and history survive. Suspension is the course's emergency unpublish.
- Access returns only after eligibility checks: lifting a suspension re-checks every pinned revision transactionally and is refused while any step is ineligible; such a course needs a reviewed course revision (an amendment, or an ordinary revision when no newer eligible lesson revision exists) before it can be published again. Every effect of a withdrawal on a course is recorded.

### Restoration and audit

Restoration is limited to previously published eligible snapshots, matching current conservative behavior. Stale approved candidates may be released through audited administrator withdrawal followed by a fresh draft/review; expose that recovery path clearly. Every submission, decision, publication, withdrawal, suspension, archive, restoration and ownership transfer has an append-only audit event in the same transaction. Actor, target, action, time and required reason are recorded. These are requirements, not optional product questions.

## 6. Public arrival and discovery

The application serves a real homepage at `/`; do not expose or proxy the renderer experiment. It explains the free service, available curriculum coverage, review process and contribution path, with clear sign-in, browse and teacher-access actions. Do not advertise unavailable content, teacher validation or registration flows that are not operational.

Public catalogue navigation: level → section → subject → courses and standalone lessons, within the current edition by default (§2 "Curriculum integrity"); earlier editions only through their labelled path. Filters/search support actual published availability; use meaningful names, not database codes. Empty selections explain that no reviewed courses are available and offer navigation elsewhere. No fake testimonials, fabricated lesson counts or placeholder claims.

A public course overview shows approved title/summary, placement, prerequisites, outline titles, author/reviewer credit and language. Full lessons, solutions, private notes and draft content remain protected. A sign-in action preserves a validated local return destination. Before public registration is enabled, offer sign-in and an honest invitation-only explanation rather than a broken registration link.

## 7. Student experience

After sign-in, `/learn` is a useful home: selected level/section (editable), subject shortcuts, available courses, bookmarked courses/lessons, and recently published/updated material. Onboarding allows skipping preferences and browsing all available curriculum; do not require school/phone/address.

Course overview leads to ordered modules and lessons, with breadcrumbs, previous/next navigation and a persistent course outline appropriate to mobile. Course-context links read the pinned eligible revision; standalone lesson bookmarks still resolve to that lesson's current publication. When the course pins an older revision than the standalone page, the reader says which version it shows. Explain unavailability (an unavailable step, or a suspended course) and retain navigation back to discovery.

Reader: title, objectives, prerequisites, attribution, mathematical notation, images/alt/captions, click-to-load video, embedded exercises and hidden/revealable worked solutions, bookmark and report actions. Reports identify the exact displayed revision (including course context); today's error-report guard (0009) accepts only the lesson's current revision, the reader view and media route serve only the current revision, and bookmarks are keyed by lesson, so B01 defines the course-context extensions and B08 implements them. No automatic educational completion or grade is inferred from opening a page. Advanced progress tracking remains deferred.

Signed-out deep links return users to the requested eligible page after login. Active users without staff roles cannot enter Studio. No search result, response cache, media URL or overview may leak protected or withdrawn content.

## 8. Reviewer, publisher and administrator experiences

Reviewer queue groups courses and lessons by authorized subject with filters, pagination, submission age and clear content type. Review page shows the full immutable submitted snapshot, curriculum placement, preview, dependencies, the change from the published base (an amendment shows only its replaced steps as changed) and previous feedback. Items without an eligible reviewer are flagged as blocked, with the reason. Authors are visible (working default); no blind-review claim. Authors see reviewer identity and feedback. Decisions require the confirmed subject authorization and independent-review rules server-side.

Publisher queue lists approved eligible candidates; show expected current version, dependency eligibility and stale conflicts before confirmation. Administrator screens cover users/status, separate grants, reviewer subjects, curriculum hierarchy/names/order, reports, audit and recovery actions. Last-active-administrator protection must hold under concurrency. No page makes administrators implicit educational reviewers.

Maintain existing CLI as recovery/bootstrap tooling, but ordinary teacher/student use must require neither terminal commands nor knowledge of internal IDs.

## 9. Catalogue management and migration

Administrators create/edit localized display names and ordering, add hierarchy relationships, draft editions and their assignments, make an edition current (which supersedes the previous one for that level/section), retire obsolete entries and see affected courses. Stable IDs/codes must not be repurposed. Changing an assignment's meaning means a new edition; submitted snapshots keep their original association (§2 "Curriculum integrity"). Retired entries cannot be newly selected but historical material stays interpretable subject to publication eligibility policy.

Implement the schema/API/UI with synthetic TEST fixtures independently of real curriculum approval. Real seed data requires authoritative sources, rights where applicable, and teacher confirmation. Do not mark all catalogue engineering blocked by seed selection.

Existing standalone lessons and bookmarks keep their IDs and URLs. Do not invent a historical level/section for them. Add associations through explicit, reviewed migration/admin workflows; incomplete entries remain standalone until validly placed. No automatic course publication or retrospective educational approval. Document upgrade/rollback and preservation of audit history; all new changes use new migrations.

## 10. UX, language, accessibility and security

French and Arabic interface localization is required for product completion; content language is independent. Scientific content retains the confirmed French-first policy. Locale switch preserves route/context; mixed-direction math remains readable. Test keyboard navigation, focus/dialog behavior, clear errors, 360 px layouts, zoom and reduced motion. No decorative 3D or cinematic layer.

Every list/form needs loading, empty, validation, permission-denied, offline/network and retry states as applicable. Distinguish course, module, lesson and official chapter consistently in labels. Save indicators distinguish browser recovery copies from server persistence.

Keep server sessions, CSRF/origin checks, denial by default, subject/owner checks, throttling and private cache behavior. Bound uploads by bytes, dimensions, formats, concurrent work and measured process memory. Configure examples to fit demonstrated budgets; client checks never replace server checks. Store replacement media immutably; strip private metadata. Define schema-version changes explicitly before editor/backend contract changes.

Public static/curriculum pages may be cached only when no identity-dependent content is included. Protected HTML, API, previews and media must never enter shared caches. Test two users and signed-out access.

## 11. Accounts and external gates

Internal pilot: operator/admin-created teacher accounts and invited students, using real permissions. Product-completion software includes teacher access request/invitation administration and public-launch registration/recovery flows behind disabled-by-default release gates where policy is unresolved. Account deletion/export/retention behavior must implement an approved policy; do not invent legal text or irreversibly delete records to guess it.

External decisions: contributor terms/licensing; privacy/deletion/retention; email provider and verification gating; official curriculum seed; real qualified author/reviewer; HEIC library acceptance if needed; infrastructure budget, backup location/RPO/RTO and release approval. Isolate each gate from unrelated implementation. No production deployment is authorized by this document.

## 12. Acceptance and definition of done

| ID | Observable acceptance journey |
| --- | --- |
| LMS-A01 | Signed-out visitor enters `/`, selects level/section/subject, opens a published course overview, cannot fetch full lesson/media, signs in and returns to the intended lesson. |
| LMS-A02 | Teacher creates a course, sets placement, adds/reorders modules/lessons by keyboard, saves, signs out/in and reopens persisted structure. Wrong owner cannot read/change the draft. |
| LMS-A03 | Concurrent outline/lesson saves reject the stale write without data loss. Session expiry/account switch preserve edits without writing as another user. |
| LMS-A04 | Teacher submits lessons and course; independent assigned reviewer requests changes, sees successor and approves; separately authorized publisher publishes eligible snapshots. Self/wrong-subject approval fails. |
| LMS-A05 | Student chooses curriculum, finds course, follows previous/next, reads math/Arabic/media, bookmarks and reports the exact revision. Another user's private data never appears. |
| LMS-A06 | Teacher updates published course structure/content via successor; students retain old version until publication. Rejected publication changes nothing. A newer approved lesson revision is flagged to staff; a course amendment shows only the replaced step as changed, is approved by an eligible reviewer and publishes atomically; the author of the replacement lesson revision cannot approve it. |
| LMS-A07 | Withdraw an included lesson revision (and separately unpublish a lesson) while the course is read/published/restored concurrently: no withdrawn content leaks in any context; the overview and unaffected steps stay readable with an explicit unavailable-step notice and navigation never skips it; an administrator suspension hides the whole course; lifting it re-checks eligibility; history/bookmarks survive; recovery requires review. |
| LMS-A08 | Owner deletes an unsubmitted draft without deleting shared lessons; submitted/published history cannot be hard-deleted. Archive/transfer/restoration require authorization, audit and conflict checks. |
| LMS-A09 | Administrator maintains catalogue editions/names/order and independent grants; last-admin and concurrent-change guards hold; making a new edition current reclassifies nothing; students see the current edition by default and labelled earlier editions only deliberately; a module mapped outside the course's assignments is refused by the server. |
| LMS-A12 | A submitted course with no eligible reviewer stays blocked, names the missing reviewer, and cannot be published by any role. |
| LMS-A10 | All critical journeys work in French/Arabic UI on small screens and keyboard; automated tests distinguish viewport emulation from physical-phone sign-off. |
| LMS-A11 | Upgrade current database with all existing IDs/bookmarks/history preserved; restore database/media into isolation and verify course/lesson access; measured upload memory fits configured limit. |

For every requirement track: specified, implemented, independently reviewed, tested, merged, usable through UI, human-validated and deployed. Passing tests is not proof of legal compliance or educational validation. Acceptance needs an actual application route, authorization behavior, failure handling and evidence—not only unit coverage. A public launch is not complete until external gates and shared-server/mobile rehearsals are satisfied.

## 13. Handoff

See [LMS delivery backlog](LMS_DELIVERY_BACKLOG.md) for dependency order and links to existing work. Preserve and extend the working lesson platform; do not rebuild it. Report requirement IDs and evidence in each PR. Any discovered omission becomes an explicit gap with its impacted user journey; update the specification rather than silently narrowing product scope.
