# Contribution terms: preparation and acceptance

Status: requirements for legal review, not an agreement or a license grant.

On 2026-11-25 the owner confirmed that commercial reuse must remain restricted, including commercial educational use. The interim [rights notice](../LICENSE.md) remains unchanged. Public visibility is not permission to reuse the project. Do not describe Learnoo as OSI-defined open source.

## Terms that must be completed

The owner should obtain a reviewed software license and matching inbound contribution agreement covering the following requirements before accepting external work:

- Identify the legal recipient and rights holders. Contributors retain ownership; no copyright assignment is presumed.
- Specify which noncommercial activities are permitted, including running, copying, modifying, distributing and hosting the software, and the conditions on modified distributions. The current notice grants none of these generally.
- Define commercial use precisely, including paid courses, tutoring, institutions, advertising, sponsorship, cost recovery and hosted services. The existing prohibition must not silently acquire an educational-institution exception.
- Give the project sufficient explicit permission to maintain, modify, combine, distribute and operate accepted contributions under the reviewed terms, and specify duration, termination, sublicensing and any patent provisions. These are drafting requirements, not rights already obtained.
- Keep educational materials, third-party dependencies and branding distinct. Preserve third-party license notices and check compatibility; the project cannot impose new restrictions on someone else's existing license.
- Specify the contributor's authority to submit their own or employer-owned work, required provenance, and treatment of third-party material. Include applicable warranty/liability and governing-law provisions after review.
- State how future license changes are handled. Do not assume permission for commercial relicensing or sell contributed work without the necessary rights.

A standard open-source license permits commercial use and therefore does not implement this policy. A DCO sign-off or PR checkbox is not a replacement for a license or sufficient inbound permission.

## Existing and future contributions

External PRs may receive technical review and corrections while waiting, but remain unmerged. Neither opening a PR, passing CI, receiving a maintainer fix nor remaining silent establishes agreement to future terms.

Once reviewed terms are adopted:

1. Publish an immutable version of the software license and contribution agreement, identifying the recipient.
2. Ask each relevant rights holder to explicitly accept that version for their identified contribution; record the acceptance and its scope in a durable place linked from the PR. Do not ask for acceptance of this requirements document.
3. Inventory third-party files and dependencies with their licenses and provenance. Resolve missing or incompatible permissions before merge.
4. Re-review the current PR head against current develop, run appropriate tests and required CI, resolve discussions and obtain the required maintainer review.
5. Merge only the reviewed commit through normal PR controls. If rights cannot be established, leave the contribution blocked or close it with a clear explanation; do not copy it into an owner-authored PR to bypass the gate.

Review already merged external material as well before broader distribution; a previous merge is not evidence of adequate permission.

## Maintainer merge checklist

- Rights: adopted terms and explicit applicable contributor acceptance, with third-party provenance resolved.
- Scope: assigned issue or explicitly requested maintenance scope, selected Go/PostgreSQL/React/TypeScript/TipTap/KaTeX architecture, no unrelated dependencies.
- Security: server-side authorization, immutable editorial snapshots, independent educational review and safe rendering/uploads preserved.
- Validation: meaningful tests for the change and successful required checks on the current head; migrations also tested against current develop and existing data.
- Review: required code-owner approval, resolved discussions and no protection bypass for external contributions. Automation may act only within delegated authority; uncertainty leaves the PR blocked.
- Handoff: remaining acceptance criteria, operational checks and educational validation recorded honestly. A partial implementation must not close the whole roadmap issue.

## References

- [GitHub: licensing a repository](https://docs.github.com/en/repositories/managing-your-repositorys-settings-and-features/customizing-your-repository/licensing-a-repository)
- [Open Source Definition](https://opensource.org/osd)
- [Developer Certificate of Origin](https://developercertificate.org/)
