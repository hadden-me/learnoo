# LMS product-completion delivery backlog

Status: planned work, 2026-11-26. This is a specification-to-implementation map, not live issue state. GitHub holds assignments, status and evidence. New LMS-B identifiers below are document IDs, **not GitHub issue numbers**. Create focused issues from them after checking current work. No item is declared implemented by this document.

## Baseline and coordination

Baseline inspected: develop `8be9f4b`. Existing work includes authenticated lessons/Studio, review/publication, invitations, reader, error reports, media and deployment artifacts. Reuse those modules. Current gaps include course entities and structure, full curriculum relationships, public homepage/discovery and richer student entry/navigation. Existing #86 covers names/seed; #87 and #88 cover proxy/static caching. Submission-audit and upload-memory work is active in other branches: find its current issues/PRs before duplicating it. #72 concerns contributor policy and is not authorized for legal approval by this specification.

## Delivery slices

| ID | Deliverable | Dependencies | Acceptance / tests |
| --- | --- | --- | --- |
| LMS-B01 | Reconcile product contracts: course/module vs official chapter, pinned lesson revision reading, versioned placement and API error shapes | Product spec | One agreed contract shared by backend, Studio and reader; migration strategy retains existing lessons; LMS-A04/A07/A11 test scenarios specified before parallel implementation |
| LMS-B02 | Catalogue levels, sections, versions/assignments, localized names/order and audited admin editing | B01; coordinate #86 | LMS-A09; stable identity, historical association, invalid cross-subject placement, retirement and concurrent writes; TEST fixtures only |
| LMS-B03 | Course identities/revisions/modules/ordered references, owner checks and draft CRUD | B01; B02 contract | LMS-A02/A03/A08; database constraints, conflict races, deletion/reference safety, upgrade tests |
| LMS-B04 | Course submission, independent review, publication and withdrawal dependency handling | B03; existing editorial services | LMS-A04/A06/A07; no silently updated pinned references, transactional eligibility, direct-SQL guards and auditable recovery |
| LMS-B05 | Teacher course dashboard/outline/placement, accessible sorting and editor integration | B03 API contract | LMS-A02/A03; course CRUD through UI, persistent saves, invalid placement, role/owner denials; lesson editor reused |
| LMS-B06 | Reviewer/publisher course screens and administrator course recovery | B04 API contract | LMS-A04/A08; independent reviewer, eligibility/conflict messages, exact snapshot preview and audit |
| LMS-B07 | Real public homepage and catalogue/course overviews | B02; B04 read contract | LMS-A01; no protected body leakage, accurate empty states, safe return URLs; reviewed metadata only; coordinate #88 |
| LMS-B08 | Student curriculum preferences/home, course outline, pinned reader, previous/next, course bookmarks and contextual reports | B04; B07 | LMS-A05/A07; exact revision reported; standalone bookmarks unchanged; missing/withdrawn paths and session transitions |
| LMS-B09 | French/Arabic interface, responsive/keyboard/accessibility completion across all routes | B05–B08 contracts | LMS-A10; locale independent of content, preserved route, no horizontal overflow, focus and reduced motion |
| LMS-B10 | User/grant/catalogue admin UI and teacher onboarding | B02; existing permissions; external contributor gate before real intake | LMS-A09; last-admin concurrency, no self-grants, scoped review, audit; operator CLI retained |
| LMS-B11 | Public account registration/recovery/verification and policy-backed lifecycle | Recorded policy/provider decisions for dependent portions | Enumeration/rate limits, token replay/expiry, deletion/history policy tests; release disabled until gates satisfied |
| LMS-B12 | Full application acceptance and operational readiness | B02–B11 as milestone requires | LMS-A01–A11 through real UI/PostgreSQL, C-locale, race tests, upgrade, backup restore, measured memory, Docker/proxy checks; separate human sign-off |

Public discovery, course CRUD and curriculum selection are part of the next product-completion milestone; do not defer them merely because the earlier technical pilot omitted them. Advanced progress, gradebooks, payments and broad LMS feature parity remain excluded. B11 can remain release-gated without blocking B02–B10.

## Per-issue readiness contract

Include purpose/user journey, included/excluded scope, technical vs external dependencies, shared contract versions, requirement IDs, observable acceptance criteria, adverse cases, validation commands and ownership. Use one migration owner at a time. Document interfaces before backend/UI work proceeds in parallel. Tests must exercise unauthorized actors, concurrency and historical data where applicable, not just happy-path mocks.

For each requirement record the application URL/action, implementation PR, reviewer findings/resolution, tests actually run and remaining manual evidence. Course availability must be tested after changes to every referenced revision or media eligibility rule.

## Genuine external gates

- Teacher-confirmed curriculum seed and independent educational validation (#1, #86): block real content, not schema/UI.
- Contributor terms and privacy/deletion/retention/provider decisions: block affected real-user intake and policy-dependent implementation only.
- Physical device, teacher/student feedback (#2–#4), shared-server rehearsal (#10), pilot (#11): remain open until evidence exists.
- Hosting capacity/backups/trusted proxies: no deployment claims based on local tests.

## Coordinator instructions

Read the product spec and inspect current GitHub state first. Preserve in-flight submission-audit, catalogue, memory, proxy and caching work. Convert this map into small assigned issues and PRs; do not duplicate existing ones. Use isolated worktrees, independent review, focused tests, CI and dependency-ordered merges under the maintainer's actual authorization. This file itself grants no extra tool/merge/deployment permissions.

When an external gate blocks a slice, proceed with independent work and TEST fixtures. When the spec has a genuine ambiguity, propose a concrete default and record it; do not invent legal/educational validation. Report course-management and student-discovery completeness separately from backend/editorial completeness.
