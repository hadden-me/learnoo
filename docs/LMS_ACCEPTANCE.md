# LMS acceptance and operational readiness (LMS-B12)

Status: evidence for issue #102, recorded 2026-11-26 on `feat/lms-b12-acceptance` (draft PR #119). It is a record of what was specified, built and tested, not a release approval. Nothing here is deployed, and nothing here was validated by a teacher, a student or a native speaker.

## What was run, and against what

The branch starts from B09 (`feat/lms-b09-i18n-a11y`), which sits on the integration branch `integration/lms` (4a1c613) combining every LMS slice (B03 to B10) on top of `develop` (B02 merged as fe827b2). None of B03 to B10 is merged into `develop`. `integration/lms` was pushed on 2026-11-26 only as the base of draft PR #118 (B09). Results below are from one machine (Apple silicon, macOS, Go 1.25.4, Node 22.20, Chromium from Playwright 1.65.1, PostgreSQL 16.13 initialised with `-E UTF8 --locale=C`, so every database-backed test and the browser suite ran in a C-locale database).

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

## Review and test record

Each slice's pull request carries the full list of findings. "Reviewed head" is the last commit an independent review round covered. Later commits were audited by a separate reviewer on 2026-11-26, as noted.

| Slice | PR | Review rounds (commit) | Findings resolved | Later commits |
| --- | --- | --- | --- | --- |
| B03 | #109 | 52a2a24; then `52a2a24..27388df` | 8 (attachability guard, races, curriculum locks, step-count and revision-number guards, `noQuery`, paging, a DECISIONS note) | rebased as aab45c9 (same patches); CI green on aab45c9 |
| B04 | #113 | b147a26; 3e59d33; 491ee3e | 7, plus the review-diff leak of withdrawn lesson content | 95cfac5 (one doc line); 71a1719 and d49cd52, audited ok |
| B05 | #112 | first round; `0303046..7c1c060` | 6 | rebased as 5fc2a25 (same patches); 942e141 audited ok |
| B06 | #116 | 57d6daa; 69e361a | 6, plus nits | ddccb57 and 64bf2a7 (A08 browser journey), audited ok |
| B07 | #115 | 6cab30e; `6cab30e..dbaab55` | 4 of 5; the sign-in dependency is resolved by merging after #117 | 85d0637, 0b5874d and 2ef035e, audited ok |
| B08 | #117 | first round; 37aa758 | 4, plus nits | none |
| B10 | #114 | 4bc6140; `4bc6140..d2766a2` | 6 | a7494b9, 98bdfce, de1a364 and f53f610, audited ok; CI green on f53f610 |
| B09 | #118 | cc09d1f; `cc09d1f..31da8ef` | 8, plus Arabic wording | 701116e (merge), audited: it adds one fr/ar message, which joins the native-speaker list |
| B12 | #119 | 688bab8; `688bab8..c4d0e53` | 7, plus a nit | d30edf4 audited (should-fix: the revoke could mask failures), fixed in 99aa671; 086023c to 5c0bdc7 under review |

The browser runs above were made at c4d0e53 (47 tests) and again at 5c0bdc7 (49 tests, three runs). The Go, Studio, upgrade, backup and Docker results are from c4d0e53; later commits change only browser specs and this document.

## Status per acceptance journey

Columns follow the definition of done in [LMS_PRODUCT_SPEC.md §12](LMS_PRODUCT_SPEC.md#12-acceptance-and-definition-of-done). **Merged** means merged into `develop`: only B01 (specification #89, contract #90) and B02 (curriculum, #106) are. **Human-validated** and **deployed** are false for every journey. "Reviewed" refers to the independent reviews recorded on each slice's pull request (every slice from B03 to B12 had at least two independent review rounds; see [Review and test record](#review-and-test-record)).

| ID | Specified | Implemented | Independently reviewed | Tested (automated) | Merged | Usable through the UI | Human-validated | Deployed |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| A01 | yes | yes (B07, B08) | yes | yes, passed | no | yes | no | no |
| A02 | yes | yes (B03, B05) | yes | yes, passed | no | yes | no | no |
| A03 | yes | yes (B03, B05) | yes | yes, passed | no | yes | no | no |
| A04 | yes | yes (B04, B06) | yes | yes, passed | no | yes | no | no |
| A05 | yes | yes (B07, B08) | yes | yes, passed | no | yes | no | no |
| A06 | yes | yes (B04, B06, B08) | yes | yes, passed | no | yes | no | no |
| A07 | yes | yes (B04, B06, B08) | yes | yes, passed | no | yes; concurrency through automated tests only | no | no |
| A08 | yes | yes (B03, B04, B06, B10) | yes | yes, passed | no | yes | no | no |
| A09 | yes | yes (B02, B10) | yes | yes, passed | B02 only | yes | no | no |
| A10 | yes | yes (B09) | yes | yes, passed (viewport emulation; Studio small screens and keyboard only as listed below) | no | yes | no (no native-speaker review, no physical phone) | no |
| A11 | yes | yes (LRN-010 scripts, #110 memory budget, B12 checks) | yes | yes, passed | LRN-010 and #110 only | operator commands, not UI (by design) | no | no |
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
- Course editor account switch: `acceptance-regressions.spec.mjs` › `a course draft open in one tab never writes or shows anything as another account signed in on the same browser; the author resumes and saves`. Author A edits an open course draft without saving; in another tab A signs out through the Studio (asked first; A's recovery copies are removed from the device, asserted) and author B signs in. Asserted: the first save in A's tab sends the only write of the whole switch, a `PUT /api/course-revisions/{id}` whose Cookie header holds B's session cookie (not A's) and which the server refuses with `403 csrf_rejected` (A's CSRF token); A's tab then shows the other-account banner and "Un autre compte est connecté dans ce navigateur. Rien n'a été enregistré"; a second save sends nothing and shows "Rien n'a été envoyé : un autre compte est connecté…". Until A is back, A's tab requests nothing but `GET /api/auth/session` (so it reads no data under B's cookie), keeps A's header and edits, and its "Reprendre ici" is refused. Through the API, neither A's course revision nor any of B's courses changed. The refused save writes the recovery copy again, under A's key only (`learnoo-studio-course-recovery:v1:u{A}:…`, with A's edits). B's list, A's course URL (not found), B's own course editor (no recovery region, no trace of A's edits) and `GET /api/course-revisions/{A's}` as B (404) show nothing of A's. Once A signs in again, the tab resumes, saves A's edits as A (version + 1) and its copies are removed. Inferred from the code, not asserted: that the refusal happened before any course handler ran (`RequireCSRF` in `internal/auth/http.go` compares A's token with B's session token and answers first; the unchanged revisions are the asserted outcome), and that the client's session read after the refusal (`web/studio/src/api/client.ts`) is what raised the banner.
- **Open (maintainer decision):** after A signs out, A's still-open tab writes A's unsaved course edits back to `localStorage` while B uses the browser, undoing the sign-out purge on a shared device (never shown or offered to B in the Studio). Recorded with options in [DECISIONS.md](DECISIONS.md#open-decision-recovery-copies-after-sign-out-on-a-shared-device); behaviour unchanged.

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
