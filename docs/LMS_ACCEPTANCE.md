# LMS acceptance and operational readiness (LMS-B12)

Status: evidence for issue #102, recorded 2026-11-26 on the local branch `feat/lms-b12-acceptance`. It is a record of what was specified, built and tested, not a release approval. Nothing here is deployed, and nothing here was validated by a teacher, a student or a native speaker.

## What was run, and against what

The branch starts from B09 (`feat/lms-b09-i18n-a11y`), which sits on the local integration branch `integration/lms` combining every LMS slice (B03 to B10) on top of `develop` (B02 merged as fe827b2). None of B03 to B10 is merged into `develop`; the integration branch has never been pushed. Results below are from one machine (Apple silicon, macOS, Go 1.25.4, Node 22.20, Chromium from Playwright 1.65.1, PostgreSQL 16.13 initialised with `-E UTF8 --locale=C`, so every database-backed test and the browser suite ran in a C-locale database).

| Check | Command | Result (2026-11-26) |
| --- | --- | --- |
| Formatting, vet | `gofmt -l .`, `go vet ./...` | clean |
| Go tests | `go test -count=1 -p 4 ./...` | 24 packages passed |
| Race detector | `go test -race -count=1 -p 4 ./internal/courses ./internal/reader ./internal/reports ./internal/server` | passed (no race reported) |
| Studio | `npm run typecheck`, `npm test`, `npm run build` in `web/studio` | typecheck clean; 282 tests in 16 files passed; build passed |
| Browser suite, including this acceptance | `npm ci --ignore-scripts`, `npm run assets`, `npm run test:reader` | 49 tests passed (45 earlier tests, including `course-admin.spec.mjs`, the 2 acceptance journeys and the 2 regression tests of `acceptance-regressions.spec.mjs`), in 1.1 min, after rebuilding the Studio; run three times in a row, all passed |
| Upgrade from the develop schema | `TestUpgradeFrom0012KeepsLessonsBookmarksAndHistory` (default and C locale), and a binary-to-binary rehearsal (below) | passed |
| Backup, restore into isolation, verify-restore with courses | `TestBackupRestoreRehearsal` (real `deploy/backup.sh` and `deploy/restore.sh`, runtime role) | passed |
| Docker image and Compose stack with the reference proxy | `deploy/proxy/test-proxy.sh` (Docker Desktop 27.3.1) | image built; 72 checks passed; teardown left no container, network, volume or built image |

### Binary-to-binary upgrade rehearsal (manual, not committed)

`TestUpgradeFrom0012…` writes its 0012 data with the current stores (and SQL that follows the 0012 guards where current code writes later columns). To exercise the real previous application as well, the server at `7143c31` (main, last migration 0012) was built from `git archive` and used to populate a fresh C-locale database through its own operator commands (`bootstrap-admin`, `create-user`, `grant`, `add-subject`, `add-chapter`, `set-subject-name`, `invite-student`) and HTTP API (lessons written, submitted, approved and published; a second edition published, withdrawn and the first restored; a bookmark and an error report filed by a student through the reader forms). The current binary then ran `migrate` (0013 to 0017). Every row of every 0012 table, limited to its 0012 columns (IDs, public IDs, sessions, bookmarks, audit and history rows), was identical before and after; no edition, placement or course was created. The current `serve` answered the sessions opened before the upgrade: both lessons read (the withdrawn edition absent), the dashboard listed the bookmark, `/catalogue` listed the lessons as not yet placed, the open error report was listed for the reviewer, and `verify-restore -strict` passed. The script lives in the session's scratch directory only; this paragraph is its record.

## Status per acceptance journey

Columns follow the definition of done in [LMS_PRODUCT_SPEC.md §12](LMS_PRODUCT_SPEC.md#12-acceptance-and-definition-of-done). **Merged** means merged into `develop`: only B01 (specification #89, contract #90) and B02 (curriculum, #106) are. **Human-validated** and **deployed** are false for every journey. "Reviewed" refers to the independent reviews recorded on each slice's pull request (B03 to B08 and B10 reviewed at least twice; B09 and this B12 branch have had no independent review yet).

| ID | Specified | Implemented | Independently reviewed | Tested (automated) | Merged | Usable through the UI | Human-validated | Deployed |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| A01 | yes | yes (B07, B08) | B07, B08 yes; B09 no | yes, passed | no | yes | no | no |
| A02 | yes | yes (B03, B05) | yes | yes, passed | no | yes | no | no |
| A03 | yes | yes (B03, B05) | yes | yes, passed | no | yes | no | no |
| A04 | yes | yes (B04, B06) | yes | yes, passed | no | yes | no | no |
| A05 | yes | yes (B07, B08) | yes | yes, passed | no | yes | no | no |
| A06 | yes | yes (B04, B06, B08) | yes | yes, passed | no | yes | no | no |
| A07 | yes | yes (B04, B06, B08) | yes | yes, passed | no | yes; concurrency through automated tests only | no | no |
| A08 | yes | yes (B03, B04, B06, B10) | yes | yes, passed | no | yes | no | no |
| A09 | yes | yes (B02, B10) | yes | yes, passed | B02 only | yes | no | no |
| A10 | yes | yes (B09) | no | yes, passed (viewport emulation; Studio small screens and keyboard only as listed below) | no | yes | no (no native-speaker review, no physical phone) | no |
| A11 | yes | yes (LRN-010 scripts, #110 memory budget, B12 checks) | LRN-010 and #110 yes; B12 no | yes, passed | LRN-010 and #110 only | operator commands, not UI (by design) | no | no |
| A12 | yes | yes (B04, B06) | yes | yes, passed | no | yes | no | no |

## Evidence per journey

Browser specs are in `tests/browser-reader/` and run with `npm run test:reader`; Go tests run with `go test ./...` against PostgreSQL. "Central journey" is `acceptance-lms.spec.mjs` › `central LMS journey: curriculum, course authoring, independent review, publication, discovery, reading, withdrawal and amendment (fr)` and `(ar)`: one continuous flow per interface language in which an administrator sets up a level, section, current edition and assignment; a teacher builds, saves and submits a course from their own published lessons; the teacher's self-review is refused in the Studio and by the server; an independent reviewer approves; a publisher publishes; a signed-out visitor goes from `/` to the catalogue and overview, cannot fetch a lesson, signs in as a student and reads with previous/next at 360 px; an administrator withdraws an included lesson revision, the student meets the unavailable step, and an amendment restores it after review and publication. The lesson workflow inside that flow (lesson submission, review and publication) goes through the API; `editorial.spec.mjs` and `studio.spec.mjs` cover it in the interface.

### LMS-A01 — public arrival and return after sign-in

- Routes: `GET /`, `/catalogue`, `/catalogue/{level}/{section}`, `/catalogue/{level}/{section}/{subject}`, `/courses/{publicId}`, `/courses/{publicId}/cover`, `/login?next=…`, `/learn/courses/{publicId}`.
- Tests: central journey (fr, ar: `/` → catalogue → level/section → subject → overview; signed-out `GET /learn/lessons/{id}` and `/learn/courses/{id}/lessons/{id}` answer 303 to `/login?next=` without the body; sign-in from the overview returns to `/learn/courses/{id}`, the course page, not a lesson); `public.spec.mjs` › `signed-out visitor finds a course from the homepage` (cache headers, cover, sign-in return path); `i18n-a11y.spec.mjs` › `public home, catalogue and course overview (fr|ar)`; `caching.spec.mjs` (public pages, fingerprinted assets, private pages); Go: `TestPublicCatalogueAndCourseOverview`, `TestPublicCourseUnavailableAndCover`, `TestPublicSignInReturnPath`, `TestPublicRetiredCurriculumAndWithdrawnRevisions`, `TestCourseMediaServesOnlyReadablePinnedRevisions`, `TestCourseRoutesRefuseForgedPostsAndStayPrivate`, `TestSignInAndOutPages`.
- Return to the intended lesson: `TestPublicSignInReturnPath` covers course paths only (`/learn/courses/{id}` kept as `next`, unsafe values replaced by `/learn`); it has no lesson `next`. The lesson case is `TestSignInAndOutPages` (`/login?next=/learn/lessons/{id}` keeps the lesson path), and the redirect of a signed-out course step to `/login?next=` is `TestCourseOutlineNavigationAndUnavailableSteps`. In the browser, `acceptance-regressions.spec.mjs` › `signing in from a course-step or lesson deep link returns to that step or lesson` opens the third step of a published course (`/learn/courses/{id}/lessons/{id}`) signed out: the server answers 303 to `/login?next=` with that exact path and without the lesson body; after the student signs in, the browser is on that step's URL with its body, "Leçon 3 sur 3" and the step marked current in the outline (not `/learn`, the outline or the first step). The same test does the same for the standalone `/learn/lessons/{id}`. No defect found: `safeNext` (`internal/reader/http.go`) keeps both paths.
- Remaining: real curriculum content (#1, #86); a teacher's and a student's reading of the homepage wording; physical phone.

### LMS-A02 — course creation, placement, keyboard outline, persistence, ownership

- Routes: Studio `/studio/courses`, `/studio/courses/new`, `/studio/courses/{id}`; API `POST /api/courses`, `PUT /api/course-revisions/{id}`, `GET /api/courses?mine=1`, `GET /api/courses/{id}/attachable-lessons`, `GET /api/lessons/{id}/eligible-revisions`.
- Tests: `courses.spec.mjs` › `author creates a course, places it, builds the outline by keyboard, saves, signs out and in, and reopens it` and › `another author can neither open nor change a course draft`; central journey (creation, placement, module, steps from the picker, save); `i18n-a11y.spec.mjs` › `teacher course editor: keyboard-only outline editing (fr|ar)`; Go: `TestCourseDraftAPI`, `TestDraftLifecycle`, `TestStepRules`, `TestPlacementRules`, `TestOwnedPages`.
- Remaining: a teacher building a real course.

### LMS-A03 — concurrent saves, session expiry, account switch

- Tests: `courses.spec.mjs` › `a stale tab’s save is refused without losing its edits, which are restored after loading the server version` and › `discarding the draft and deleting a never-submitted course ask first, wait for a valid session, and keep the lessons`; `studio.spec.mjs` › `a stale tab never writes as another account signed in on the same browser; it resumes once the author is back`, › `a page never shows another account’s lessons after someone else signs in on the same browser`; Go: `TestStaleSaveOneWins`, `TestConcurrentDraftCreationExactlyOne`, `TestSaveLockOrder`, `TestDiscardRacesCourseSave`.
- Course editor account switch: `acceptance-regressions.spec.mjs` › `a course draft open in one tab never writes or shows anything as another account signed in on the same browser; the author resumes and saves`. Author A edits an open course draft without saving; in another tab A signs out through the Studio (asked first, and A's recovery copies are removed from the device) and author B signs in. In A's tab, saving is refused with the other-account banner and the edits stay on screen; the header and page show nothing of B's. The first save is the only request that leaves the tab: it carries B's session cookie with A's CSRF token and the server refuses it (`403 csrf_rejected`) before writing; the client then reads the session, sees B, and sends nothing more ("Rien n'a été envoyé"). Through the API, neither A's course revision nor any of B's courses changed. The refused save writes the recovery copy again, under A's key only (`learnoo-studio-course-recovery:v1:u{A}:…`, with A's edits). B's list, A's course URL (not found), B's own course editor (no recovery region, no trace of A's edits) and `GET /api/course-revisions/{A's}` (404) show nothing of A's. Resuming while B is signed in is refused; once A signs in again, the tab resumes, saves A's edits as A (version + 1) and its copies are removed.
- Observation (not a defect): sign-out removes the account's copies at that moment, but a still-open tab of that account writes its copy again when it is refused or when its edits change. The copy stays under that account's key and is never offered to another account.

### LMS-A04 — submission, independent review, publication

- Routes: `POST /api/course-revisions/{id}/submit`, `/api/review/course-revisions/{id}/approve|request-changes`, `/api/course-revisions/{id}/publish`; Studio `/studio/review`, `/studio/review/courses/{id}`, `/studio/publish`, `/studio/publish/courses/{id}`.
- Tests: central journey (submission, self-review refused in the Studio and with `403 not_eligible_reviewer` / `course_author`, independent approval, publication); `course-review.spec.mjs` › `course submission, change request, correction, approval, publication, amendment, suspension and restoration` and › `a course author cannot review their own course, in the Studio or through the API`; Go: `TestSubmit`, `TestIndependentReview`, `TestPublish`, `TestPublishRaces`, `TestReviewRacesRevocation`, `TestPublishRefusesStaleComparison`, `TestCourseReviewPublicationAPI`, `TestWorkflowGuardsBindTheRuntimeRole`.
- Go only: the wrong-subject reviewer refusal (`TestIndependentReview`, a physics-only reviewer gets `ErrNotFound`); no browser test.
- Remaining: a real independent reviewer of a real subject (#1).

### LMS-A05 — student reading

- Routes: `/learn`, `/learn/preferences`, `/learn/courses/{id}`, `/learn/courses/{id}/lessons/{lesson}`, `…/media/{media}`, `…/report`, `…/bookmark`, `/learn/search`.
- Tests: central journey (outline, start, previous/next, math typeset, 360 px, no overflow); `course.spec.mjs` › `course reading with an unavailable step, a course bookmark and a course-context report`; `i18n-a11y.spec.mjs` › `student course reading with mixed-direction math (fr|ar)`; `reader.spec.mjs`; Go: `TestCourseOutlineNavigationAndUnavailableSteps`, `TestCoursePinsItsRevisionAndSaysWhenItDiffers`, `TestCourseBookmarksAndPreferencesArePrivate`, `TestCourseContextReportsNameTheDisplayedRevision`, `TestCourseSearchMatchesOnlyReadableMaterial`, `TestCreateInCourseNamesThePinnedRevision`.
- Go only: course media (images of a pinned revision served only while the step is readable, `TestCourseMediaServesOnlyReadablePinnedRevisions`, `TestLessonWithdrawalRacingCourseReadAndMedia`); no browser test reads a course step with an image.
- Remaining: students reading real content; physical phone.

### LMS-A06 — successors, rejected publication, newer-revision notice, amendments

- Tests: central journey (newer revision published, notice to the owner, amendment created, review shows one replaced and one unchanged step, approval, atomic publication, step readable again); `course-review.spec.mjs` (amendment refused while a draft is open, content diff of the replaced step); Go: `TestAmendments`, `TestAmendmentRetiredEditionAndRepublication`, `TestAmendAfterWithdrawingLastPublished`, `TestPublishRefusesStaleComparison`, `TestIndependentReview` (author of a replacement revision refused, author of an unchanged lesson allowed), `TestStudentsKeepThePublishedCourseUntilTheSuccessorIsPublished` (students read the published revision while a successor is drafted, in review and approved; a stale publication is refused and changes nothing; only publication switches).
- Go only: "students keep the old version until publication" and the refused publication (the test above); the browser journeys show the replaced step only after publication but do not read the course while the amendment is in review.
- Remaining: none beyond human use.

### LMS-A07 — withdrawal, unpublication, suspension, restoration

- Routes: Studio `/studio/admin` (find a lesson, withdraw a revision), `/studio/admin/courses/{id}` (suspend, restore); API `POST /api/revisions/{id}/withdraw`, `/api/lessons/{id}/unpublish`, `/api/courses/{id}/suspend|restore`.
- Tests: central journey (withdrawal through the Studio, unavailable-step notice, navigation stops at the step, standalone page no longer serves the body, recovery only through a reviewed amendment); `course.spec.mjs`; `course-review.spec.mjs` (suspension and restoration); Go: `TestWithdrawalRacesCourseRead`, `TestLessonWithdrawalRacingCourseReadAndMedia`, `TestLessonEffectsRaceCoursePublicationAndRestore`, `TestSaveRacesLessonWithdrawalAndUnpublish`, `TestDependencyEffects`, `TestDependencyTriggersRequireReadCommitted`, `TestSuspendedArchivedAndUnpublishedCoursesAreHidden`, `TestCourseReportRacingWithdrawal`, `TestArchiveUnarchive`.
- Remaining: an operator rehearsal of an emergency withdrawal on a shared server (#10).

### LMS-A08 — delete, archive, transfer, restoration

- Tests: `courses.spec.mjs` › `discarding the draft and deleting a never-submitted course …`; `course-admin.spec.mjs` › `the owner requests archival; an administrator archives, unarchives, restores and transfers the course`; Go: `TestDeleteKeepsLessonsAndMedia`, `TestLifecycle`, `TestArchiveUnarchive`, `TestKindAndTransferGuards`, `TestAdminCourseReadsAPI`, `TestCourseReviewPublicationAPI`.
- Remaining: deletion of accounts and personal data waits for B11's policy.

### LMS-A09 — catalogue, grants, guards, editions

- Routes: Studio `/studio/admin/curriculum`, `…/sections/{id}`, `…/editions/{id}`, `/studio/admin/users`, `/studio/admin/catalogue`.
- Tests: central journey (level, section, edition, assignment and chapter list created and made current in the Studio, fr and ar); `admin.spec.mjs` (grants, disable/enable, edition, invitation, refusals); `courses.spec.mjs` (a new current edition leaves the draft's placement flagged and refused, nothing reclassified); `course-chapter-retirement.spec.mjs`; Go: `TestRacingMakeCurrentYieldsOneCurrent`, `TestAssignmentEditRacingMakeCurrent`, `TestDirectSQLRacesAtEveryIsolationLevel`, `TestPlacementModes`, `TestSaveRacesEditionStateChange`, `TestAdminConcurrentRemovalsKeepAnAdministrator`, `TestCurriculumAdministrationAPI`, `TestPublicCatalogueAndCourseOverview` (students see the current edition by default; earlier editions only through their labelled path), `TestPublicRetiredCurriculumAndWithdrawnRevisions`.
- Remaining: the official curriculum seed with authoritative sources and teacher confirmation (#1, #86).

### LMS-A10 — French and Arabic, small screens, keyboard

- Tests: central journey run in both interfaces (all wording read from the application's catalogs by `acceptance-messages.mjs`); `i18n-a11y.spec.mjs` (lang/dir, landmarks, names, focus, 360 and 640 px, reduced motion); Go: `TestPublicPagesPerLocale`, `TestReaderLocaleCookie`, `TestStudioShellLocale`, `TestMessageKeysUsed`, `TestLocaleMarkup`, Studio `npm test` catalog parity.
- Limits: the central journey runs the Studio at desktop width; the Studio's small-screen evidence is `i18n-a11y.spec.mjs` only (its audits at 360 and 640 px of the course editor, review queue and page, and an administration page), plus the 360 px checks in the slice specs. Keyboard-only operation is tested for the course outline editor (and the step picker in `courses.spec.mjs`) and the administration dialog's focus trap; other Studio screens are not tested keyboard-only.
- Remaining: native-speaker review of the Arabic wording (the catalogs say so); a physical phone; screen-reader use by a person. Viewport emulation is not a phone.

### LMS-A11 — upgrade, backup and restore, memory

- Tests: `TestUpgradeFrom0012KeepsLessonsBookmarksAndHistory` (every table that exists at 0012 is populated, including an uploaded image embedded in a published lesson revision (`media_assets`, `media_uploads`, `revision_media`), `login_attempts`, invitations and their audit; every row identical in its 0012 columns after 0013–0017; lessons, the image, bookmarks, reports and history served to pre-upgrade sessions; default and C locale); the binary-to-binary rehearsal above; `TestUpgradeFrom0012KeepsCatalogue`, `TestUpgradeFrom0013KeepsExistingRows`, `TestUpgradeTo0016KeepsOperatorRows`, `TestUpgradeFromB04KeepsExistingRows`; `TestBackupRestoreRehearsal` (now with a published, bookmarked course and a submitted successor: restored into a fresh database and media directory, `verify-restore -strict` passes with equal row counts, course, course step and bookmarks served again); `TestVerifyDetectsInconsistentCourses` and `TestVerifyReportsEachCourseInconsistency` (each reachable course message; the archived-course message is unreachable while the CHECK `courses_archived_unpublished` holds), `TestVerifyRestoreAcceptsEveryCourseState` (no problem reported for suspended, archived, withdrawn-in-review, amended and never-published courses); upload memory: `TestDecodeMemoryWithinEstimate` and [measurements/upload-memory.md](measurements/upload-memory.md) (#110; not re-measured here, since no LMS slice changes upload handling).
- `verify-restore` now checks courses as it checks lessons ([DEPLOYMENT.md](DEPLOYMENT.md#restore-rehearsal)): published pointers name an approved, non-withdrawn revision of a non-archived course and equal the latest course publication audit row, and every submitted course revision has its submission audit row.
- Remaining: a restore rehearsal on the shared server with its real backup location, RPO and RTO (#10); infrastructure budget.

### LMS-A12 — no eligible reviewer

- Tests: `course-review.spec.mjs` › `a course with no eligible reviewer is shown as blocked, with the gap named, to its author and administrators`; Go: `TestNoEligibleReviewerBlocks`, `TestBlockedReviewsPaging`, `TestReviewRacesRevocation`.
- Remaining: none beyond human use.

## Defects found by the acceptance run

None in the application. One wrong assumption in the new spec was corrected before commit: the Studio names levels, sections and editions in the course's language, not the interface's, which is the recorded design (`PlacementSection.tsx`).

## External gates (not satisfied; not claimed)

- **B11** (#101): public registration, recovery and verification, and account deletion, export and retention; blocked on the privacy, deletion and retention policy and the email provider decision.
- **Arabic**: review of every Arabic string by a native-speaking teacher.
- **Physical phone**: all small-screen evidence is Chromium viewport emulation.
- **Shared-server rehearsal** (#10): deployment, proxy, backup, restore and rollback on the real host.
- **Pilot** (#11): real teachers, reviewers and students.
- **Real curriculum content** (#1, #86): authoritative curriculum seed and real lessons, with teacher confirmation.
- **Contributor terms and licensing**: undecided ([LICENSING.md](LICENSING.md)).
- **Merging**: B03 to B10 and this branch still need their pull requests merged into `develop` in the recorded order, with CI.
