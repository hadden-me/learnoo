# Product requirements

## Product completion

[LMS_PRODUCT_SPEC.md](LMS_PRODUCT_SPEC.md), incorporated by the implementation plan, supplies the complete acceptance contract for teacher course CRUD, level/section/subject assignment, public homepage/catalogue, student home/course navigation and supporting administration. [LMS_DELIVERY_BACKLOG.md](LMS_DELIVERY_BACKLOG.md) maps it to delivery slices. Course and module management are distinct from editing individual lessons. Historical deferrals below do not exclude the minimum catalogue/administration UI required by this milestone; advanced analytics and classroom features remain deferred.

## Mission and initial scope

Provide free, trustworthy Tunisian BAC learning materials maintained by volunteers. Pilot the BAC Mathematics section, mathematics subject, and a coherent chapter (chapter selection pending); target 10–20 human-reviewed lessons for the educational pilot, not for the first editor experiment.

## Required experience

- Free accounts required for full lessons and exercises; public information and limited curriculum catalog.
- Email/password and recovery for public launch; email-verification gating remains undecided.
- Student bookmarks, subject/chapter navigation, basic title search, mobile lesson reader.
- French and Arabic interface/content support; interface language is independent of lesson language. Mixed-direction text and mathematics must work.
- Scientific lessons use French as the primary instructional language. Teachers may explain in Arabic or Tunisian dialect while retaining scientific keywords in French; Arabic explanation is optional, not a requirement for every lesson.
- Written explanations remain useful without video. Videos use YouTube, which remains independently accessible outside Learnoo.
- Invited authors, subject-authorized reviewers, publishers, and administrators have distinct permissions. A user may hold multiple permissions.
- Author cannot approve their own revision. A separate reviewer may also publish.
- Basic author/reviewer attribution and error reporting.

## Explicitly deferred

Public contributor onboarding, rich profiles, advanced reviewer assignment/diffs, review email notifications, broad curriculum management, translation dashboards, automated mathematics grading, progress analytics, AI tutoring, forums, messaging, live classes, certificates, native apps, and custom video streaming.

## Launch requirements

Use minimal personal data; no required student public profile. Define privacy, account deletion and retention procedures before public registration. Resolve licenses and inbound rights before external contributions. Recruit an author and a separate qualified reviewer. Verify mobile usability with actual students.

The publishing invariants live in [content and editorial rules](CONTENT_AND_EDITORIAL.md); delivery acceptance criteria live in [the roadmap](ROADMAP.md).
