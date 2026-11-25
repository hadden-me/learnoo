# Contribution review and maintainer decisions — 2026-11-25

## Your contribution model

Anyone may fork Learnoo and propose a focused pull request. Only the maintainer decides what is accepted. Opening a PR gives no right to merge, direct-push, bypass review or add unrelated code. The owner clarified that external merges require their explicit approval. Automated review can prepare fixes and recommendations, but must not manufacture that approval.

The owner intends to allow noncommercial use and forbid commercial use, including commercial educational use. This is a separate question from who may contribute or merge. Public collaboration does not require accepting every contribution.

## Written policy versus the public explanation

The owner reports explaining this policy in Facebook videos; those videos were not supplied or independently reviewed. The existing LICENSE.md and docs/LICENSING.md expressly say no general reuse grant is currently provided. That does not yet express the intended affirmative noncommercial permission.

Use “public-source project welcoming contributions, with a noncommercial-use policy” while the terms are completed. Under the [Open Source Definition](https://opensource.org/osd), open-source licenses allow commercial use. This terminology does not change your control over merges.

Suggested public clarification, not a replacement license:

> Everyone is welcome to fork Learnoo and propose improvements through a pull request. Every contribution requires my explicit approval before it is merged. My intended policy is to permit noncommercial use and forbid commercial use, including commercial educational use. The repository currently has an interim rights notice; complete reuse and contribution terms are being prepared. Please use the written repository terms as the authoritative reference.

## Review snapshot

Base reviewed: develop `14b2ae1` (includes lesson persistence #70 and Studio API #71). Both maintainer PRs were already merged by the time of this check; this report does not claim an independent full review of them. All five external PR heads below retain successful GitHub verify checks, but those runs predate the latest base.

| PR and reviewed head | Technical result | Recommendation |
| --- | --- | --- |
| [#52](https://github.com/hadden-me/learnoo/pull/52), `1506712` | Exact loopback host allowlist accepts portless requests while rejecting external hosts. Targeted tests pass on latest base. | First merge candidate after rights, current CI and explicit owner approval. Local renderer remains loopback-only. |
| [#58](https://github.com/hadden-me/learnoo/pull/58), `8f3f989` | Adds RTL/math wrapper assertions and 375px browser overflow checks. Rendering tests pass on latest base. Previous browser suite passed; not rerun in this pass. | Candidate as partial #4 work after the same gates and current browser validation. Does not complete real-phone, slow-network or teacher-selected-video checks. |
| [#56](https://github.com/hadden-me/learnoo/pull/56), `41f47a6` | EXIF orientation parsing is bounded; transformed pixel view avoids a second full-size image allocation. Strict trailer rejection remains. Media race tests pass on latest base. | Candidate only for EXIF orientation after rights, current CI and owner approval. Do not describe it as complete phone-photo support; appended phone metadata/HEIC and device/memory evidence remain outstanding. |
| [#47](https://github.com/hadden-me/learnoo/pull/47), `fdf07a5` | Optional video scripts use shared slide data and stale-input checks; four offline tests pass. No production dependency. | Consider only as an explicitly unvalidated experiment after rights and owner approval. Regenerated audio/video, external-service/output rights and teacher review remain unverified. Do not publish as approved teaching material. |
| [#40](https://github.com/hadden-me/learnoo/pull/40), `8148630` | Blocking schema incompatibility: its 0003 migration creates chapters, while current develop's 0004 also creates chapters with lesson-reference and identity guards. GitHub also reported conflicts. | Do not merge. Rebuild as a later additive migration extending the existing table; preserve immutable identity and composite references, then test fresh installation and upgrade with existing lesson data. Rights and owner approval still required. |

PR #49 was previously closed as superseded. PR #72 contains the contribution-process documentation and this report; it does not adopt a license or accept contributors' work.

## Checks actually executed in this pass

In a disposable checkout of develop `14b2ae1`, applied only the code/test changes from #52, #56, #58 and the isolated experiment files from #47 together. Documentation conflicts were excluded, so this is targeted integration evidence, not an exact proposed merge or replacement for PR CI.

- `go test -race ./cmd/renderer ./internal/rendering ./internal/media`: passed.
- `go vet ./cmd/renderer ./internal/rendering ./internal/media`: passed.
- `python3 -m unittest discover -s docs/fixtures/video-eval/scripts -p 'test_*.py'`: four passed.
- `git diff --check`: passed.
- No browser, full PostgreSQL suite, video synthesis or real-device validation was run in this pass. No external PR was merged or approved.

## What to do with existing contributions

1. Keep the useful PRs open. Do not discard contributors' work merely because the terms need clarification.
2. Complete a written noncommercial software license and matching inbound permission, using CONTRIBUTION_TERMS.md as the review brief. Define use, modification, redistribution, hosting, commercial activity and third-party exceptions precisely. Separate software, teaching materials and branding. This report does not create those grants.
3. After adopting a definite version, ask the relevant contributors to explicitly confirm their authority and acceptance for their identified PRs/commits. An old PR, a Facebook announcement, silence or a maintainer's fixes cannot substitute for that record. No ownership transfer is presumed.
4. Check provenance and third-party permissions. A custom agreement is not necessarily the only mechanism, but any chosen mechanism must establish sufficient rights; a checkbox cannot repair missing underlying permission. GitHub's [contribution terms](https://docs.github.com/en/site-policy/github-terms/github-terms-of-service#6-contributions-under-repository-license) refer to contributions under a repository license; do not treat this incomplete notice as a finished license.
5. Rebase or merge the latest develop, run the relevant full checks, resolve findings and obtain the owner's explicit approval on the reviewed head. Suggested order: #52, #58, #56; decide whether to retain #47 as an experiment; rebuild #40 before reconsidering it.
6. If a contributor declines or cannot establish rights, leave the PR unmerged or close it with an explanation. Do not move their code into an owner-authored PR to avoid the requirement.

A merged-PR inventory of up to 100 recent PRs found no PR authors other than the owner. That is not a copyright audit: inspect commit authors, copied material and third-party provenance before claiming all existing material is owner-created.
