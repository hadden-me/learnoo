# Server API

Contract home for Studio ↔ server and reader ↔ server endpoints ([CAHIER_DES_CHARGES.md §5.4](CAHIER_DES_CHARGES.md#54-shared-contracts-agree-first-then-build-in-parallel)). Changes used by another open task are agreed on its issue first.

Served by `cmd/server` (not `cmd/renderer`). Status: **LRN-005 foundation, the LRN-006 Studio API, LRN-007 review and publication, LRN-009 withdrawal, unpublication and restoration, the LRN-008 reader, LRN-015 error reports, the Studio integration (issue #65), the [curriculum](#curriculum-lms-b02) hierarchy and administration (LMS-B02) and [course drafts](#course-drafts-lms-b03) (LMS-B03).** Authentication endpoints, the server-side permission check every protected endpoint uses, the [Studio endpoints](#studio-lesson-drafts-and-media-lrn-006) for lesson drafts and media, the [review and publication endpoints](#review-and-publication-lrn-007), [withdrawal, unpublication and restoration](#withdrawal-unpublication-and-restoration-lrn-009), the server-rendered [student reader](#student-reader-lrn-008) (HTML pages, not JSON), [error reports](#error-reports-lrn-015) filed from the reader and handled through the review API, and the [Studio integration](#studio-integration-issue-65): catalogue, owned lessons, canonical preview and the served Studio bundle.

## Conventions

- JSON bodies, UTF-8. Request bodies must use `Content-Type: application/json` (the only exception is the raw image body of `POST /api/media`). Studio endpoints accept exactly one JSON object whose top-level keys are spelled exactly as documented (case-sensitive) and appear once; unknown, case-variant or duplicate keys, values of the wrong type, anything after the object and malformed JSON are rejected with `400 invalid_request`. `null` is accepted only where a field is documented as accepting it (such as `chapterId` and `translationOf`); it is `400` for every other field, including `content`, `prerequisites`, `mediaIds` (and its elements), `subjectId`, `language` and `expectedVersion`. Integer fields accept only integer literals that fit (`1.0`, `1e0` and `"1"` are `400`). Error messages name the JSON key, never an implementation type. The login endpoint applies the same key rules (see below).
- Every response carries `Cache-Control: no-store` (media responses, the [Studio integration](#studio-integration-issue-65) endpoints and `/studio`: `private, no-store`) and `Vary: Cookie`; authenticated responses must never be stored by shared caches. The only exceptions are fingerprinted files: [`/assets/<version>/…`](#fingerprinted-assets-assetsversionpath) (`public, max-age=31536000, immutable`, no `Vary`) and the Studio bundle's `/studio/assets/<name>-<hash>.<ext>` for signed-in staff (`private, max-age=31536000, immutable`).
- Session: cookie `__Host-learnoo_session` (`HttpOnly; Secure; SameSite=Lax; Path=/`, no `Domain`). The value is opaque; clients never read it. Browsers accept `Secure` cookies on `http://localhost` and `http://127.0.0.1` only.
- CSRF, two layers:
  1. Every unsafe method (`POST`, `PUT`, `PATCH`, `DELETE`) from a browser is rejected when `Sec-Fetch-Site` or `Origin` shows a cross-origin request (Go `http.CrossOriginProtection`). This also prevents login CSRF.
  2. Authenticated unsafe requests also need header `X-CSRF-Token` equal to the `csrfToken` returned by login or `GET /api/auth/session`.
- Errors share one shape:

```json
{"error": {"code": "invalid_credentials", "message": "Email or password is incorrect."}}
```

`message` is for display and may change or be translated; clients branch on `code`.

| Status | `code` | When |
| --- | --- | --- |
| 400 | `invalid_request` | Malformed JSON, unknown or wrong-case fields, duplicate fields, data after the JSON object, missing required fields (email, password, `content`, `expectedVersion`…), bad query parameter, empty upload |
| 401 | `invalid_credentials` | Login failed. Identical for unknown email, wrong password and disabled account |
| 401 | `unauthenticated` | No valid session (missing, expired, revoked or forged cookie) |
| 403 | `csrf_rejected` | Missing or wrong `X-CSRF-Token` on an authenticated mutation |
| 403 | `cross_origin_rejected` | Cross-origin unsafe request |
| 403 | `forbidden` | Signed in, but lacks the required permission (or it could not be verified) |
| 403 | `self_review` | A reviewer tried to approve or request changes on their own revision (administrators included) |
| 404 | `not_found` | Unknown route; a catalogue subject code, lesson, revision, media or error report ID that does not exist, is malformed, or (revisions) belongs to another author, or (error reports) belongs to a subject the caller does not review |
| 408 | `upload_timeout` | The upload body did not arrive within the server's read timeout. No `Retry-After`: retry on a better connection or with a smaller image |
| 409 | `version_conflict` | `expectedVersion` is stale: the draft (lesson or course) changed since it was loaded. Nothing was written |
| 409 | `not_draft` | The revision was submitted and can no longer change, be submitted again or be discarded |
| 409 | `active_candidate_exists` | The lesson (or course) already has an active candidate: a draft, a revision in review, or an approved revision not yet published (or withdrawn) |
| 409 | `archived` | The lesson is archived |
| 409 | `translation_exists` | The translation group already has a lesson in that language |
| 409 | `retry` | The database aborted the transaction to resolve a deadlock or serialization conflict with a concurrent request. Nothing was written; retry the same request (reload first if it carries `expectedVersion` and it fails again with `version_conflict`) |
| 409 | `immutable` | A database guard rejected a change to a frozen snapshot or asset (normally preceded by a more specific code) |
| 409 | `not_in_review` | The revision is not in review (a draft, or already decided, for example by a concurrent reviewer). Nothing was written |
| 409 | `not_approved` | Publication of a revision that is not approved |
| 409 | `already_published` | Publication of a revision that is or was already published; re-pointing to an earlier revision is [restoration](#post-apirevisionsrevisionidrestore) |
| 409 | `stale_base` | The lesson's published revision changed since the revision was drafted. Nothing was published |
| 409 | `withdrawn` | The revision was withdrawn and can never be published or restored |
| 409 | `already_withdrawn` | Withdrawal of a revision that is already withdrawn |
| 409 | `stale_pointer` | `expectedPublishedRevisionId` is not the lesson's current published revision. Nothing was changed |
| 409 | `not_published` | Unpublication of a lesson that has no published revision |
| 409 | `never_published` | Restoration of a revision that was never published (publish it instead) |
| 409 | `already_current` | Restoration of the revision that is already published |
| 409 | `not_open` | Resolution or dismissal of an error report that was already resolved or dismissed (for example by a concurrent reviewer). Nothing was written |
| 409 | `ineligible` | Restoration of a revision whose content no longer passes the current schema v1 validator, or whose media references do not resolve |
| 409 | `edition_not_draft` | A curriculum edition left draft: its assignments and chapter lists are immutable and it cannot be made current again. Nothing was written |
| 409 | `state_conflict` | A curriculum edition state change that is not allowed now: retiring a draft, or making an edition current when the section's current edition is not `expectedCurrentEditionId` (for example after a concurrent make-current). Nothing was written |
| 409 | `code_exists` | A level, section or edition code is already used in its scope (codes are never reused) |
| 409 | `already_retired` | Retirement of a curriculum entry, subject or chapter that is already retired |
| 409 | `parent_retired` | A section under a retired level, or an edition created or made current under a retired section or level |
| 409 | `assignment_exists` | The edition already assigns the subject |
| 409 | `has_history` | Deletion of a course one of whose revisions was submitted (or that was published): it is kept; discard its open draft instead |
| 409 | `referenced_by_course` | Discard of a lesson draft that a course draft pins as a step; remove the step from the course draft first |
| 413 | `too_large` | JSON body over 256 KiB, or image over 5 MiB |
| 415 | `unsupported_media_type` | Request body is not `application/json`; upload is not a PNG, JPEG or WebP image (SVG, GIF and everything else are refused) |
| 415 | `type_mismatch` | Uploaded bytes do not match the declared image `Content-Type` |
| 415 | `unsupported_heic` | The upload is a HEIC/HEIF photo (detected from its bytes or declared as `image/heic`/`image/heif`), not yet accepted; export it as JPEG or PNG |
| 415 | `unsupported_cmyk` | The upload is a CMYK or YCCK JPEG (print colour); convert it to RGB and export it as JPEG or PNG |
| 422 | `invalid_content` | The document fails schema v1 validation (drafts and `POST /api/preview` alike), or a field limit (prerequisites, number of media) is exceeded. `message` carries the validator's reason |
| 422 | `language_mismatch` | The document's `language` differs from the lesson's |
| 422 | `unknown_subject` / `unknown_chapter` / `unknown_media` | Referenced subject, chapter (of this subject) or media asset does not exist |
| 422 | `unattached_media` | The content embeds an uploaded image (`media:<id>`) that is not in the revision's `mediaIds` (issue #77) |
| 422 | `reason_required` / `invalid_reason` | Withdrawal or unpublication without a reason, or with one over 2000 characters or not valid text |
| 422 | `unknown_lesson` | `translationOf` names a lesson that does not exist or that the caller does not own |
| 422 | `translation_subject` | A translation names a subject other than its source lesson's |
| 422 | `invalid_image` | The image is corrupt or truncated |
| 422 | `invalid_dimensions` | Over 4096 px per side or 16 megapixels |
| 422 | `trailing_data` | Data after the end of the image that is not a validated phone-photo structure (MPF images, Ultra HDR gain map, XMP-declared motion-photo video, zero padding): polyglots, appended files, inconsistent declarations; re-export the image or upload PNG |
| 422 | `image_too_complex` | Decoding the image would need more than 192 MiB (estimated from its headers: large progressive JPEGs without chroma subsampling, large lossless or transparent WebP, large interlaced 16-bit PNG), or its re-encoded form would exceed 16 MiB; export photos as standard JPEG and screenshots or drawings as 8-bit PNG, or reduce the size |
| 422 | `comment_required` | A change request without a comment (missing, null or only whitespace) |
| 422 | `invalid_comment` | A review comment over 5000 characters or containing a NUL character |
| 422 | `note_required` / `invalid_note` | Error report resolution without a note (missing, null or only whitespace), or with one over 2000 characters or containing control (other than line feeds) or bidirectional formatting characters |
| 422 | `invalid_input` | A curriculum code, name, label or display order, or a course field, is malformed (`message` says which rule) |
| 422 | `placement_invalid` | A curriculum selection the rules refuse: a retired subject or chapter, a chapter that is not in the assignment's subject, a chapter listed twice, or making current an edition that assigns a retired subject or chapter (`message` names them); for a course draft, an assignment that cannot be newly selected or of another subject, or a module chapter in none of the draft's assignments or retired (`message` says which) |
| 422 | `retired_subject` / `retired_chapter` | A new lesson in a retired subject, or a draft created, saved with a chapter change, or submitted with a retired chapter other than the chapter of its published base revision |
| 422 | `invalid_step` | A course step that cannot be attached: unknown lesson or revision, another author's lesson that is not published or a revision of it that is not eligible, another subject or language, a revision of another lesson, or a lesson attached twice |
| 429 | `rate_limited` | Login throttled; see `Retry-After` (seconds) |
| 500 | `internal` | Unexpected server error (details only in server logs, never secrets) |
| 503 | `busy` | Too many uploads in progress: no upload slot freed within 5 seconds. Retry after `Retry-After` seconds |
| 503 | `unavailable` | Health check could not reach the database |

## Session object

Returned by login and `GET /api/auth/session`:

```json
{
  "user": {"id": 1, "email": "student@example.test", "displayName": "Simulated Student"},
  "csrfToken": "opaque-string",
  "expiresAt": "2026-11-25T09:00:00Z"
}
```

`expiresAt` is the current idle expiry; it moves forward with activity but never past the absolute limit.

## Endpoints

### `POST /api/auth/login`

Request: `{"email": "student@example.test", "password": "…"}`. Email is trimmed and compared case-insensitively. The request body is strict: field names must be exactly `email` and `password` (no other spelling, case or Unicode case folding such as `pasſword`), each at most once, and nothing but whitespace may follow the object; otherwise `400 invalid_request`. Rejected requests are not counted as sign-in attempts.

- `200` session object and a new session cookie. Any session cookie sent with the request is revoked first: the token always rotates at login.
- `401 invalid_credentials`, `400`, `415`, `403 cross_origin_rejected`, `429 rate_limited`.

Throttling (sliding 15-minute window, counted before the password is checked, enforced for existing and non-existent accounts alike): more than 5 attempts for one email address, or more than 30 from one client address, return `429` even with the correct password. A successful login clears that email's counter. The client address is the TCP peer, or, when the peer is a configured trusted proxy, the address derived from `X-Forwarded-For` or `CF-Connecting-IP` ([DEPLOYMENT.md](DEPLOYMENT.md#client-addresses-behind-the-proxy)); this applies to every per-address throttle.

### `POST /api/auth/logout`

Requires a session and `X-CSRF-Token`. Deletes the session server-side and clears the cookie.

- `204` no body.
- `401 unauthenticated`, `403 csrf_rejected`, `403 cross_origin_rejected`.

### `GET /api/auth/session`

- `200` session object for the current cookie (also refreshes the idle expiry).
- `401 unauthenticated`. An invalid cookie is also cleared.

### `GET /healthz`

Liveness: `200 {"status":"ok"}` whenever the process serves HTTP. It checks no dependency, so a database outage does not restart the container. Not authenticated.

### `GET /readyz`

Readiness: `200 {"status":"ok"}` when the database answers a ping and the media directory is writable by the server (an `access(2)` check that writes nothing; a full disk is not detected); otherwise `503 unavailable` with the message "Not ready.". The failing component and error are logged, never returned. Not authenticated. Both probes send `Cache-Control: no-store`. They are for the container health checks and internal monitoring only; the reverse proxy must not expose them publicly ([DEPLOYMENT.md](DEPLOYMENT.md#monitoring)).

## Session lifetime

| Limit | Value |
| --- | --- |
| Idle timeout | 24 hours without a request |
| Absolute timeout | 14 days after login |
| Idle extension | At most once per minute per session |

## Permissions

Role model: [DECISIONS.md](DECISIONS.md#role-model). Four independent permissions, stored in PostgreSQL and checked on the server for every request (no caching in the session, so revocation applies to the next request):

| Permission | Scope | Notes |
| --- | --- | --- |
| `author` | global | Default for Teacher and Administrator |
| `reviewer` | per subject | Never a default; granted per subject, including to administrators |
| `publisher` | global | Default for Administrator only; granted separately to teachers |
| `administrator` | global | Default for Administrator; the only permission allowed to change permissions |

No permission implies another; reviewing never implies publishing. Account types (Student, Teacher, Administrator) are presentation only and are derived from the permissions.

Protected endpoints wrap their handler in `permissions.Middleware.Require(permission, …)` or `RequireReviewer(subjectFromRequest, …)`, inside the session middleware. Deny by default:

- No session: `401 unauthenticated`.
- Session without the permission, an unknown permission, a disabled account, an unparseable subject or a database error: `403 forbidden`.

The middleware is a fast early check: it reads outside the handler's transaction, so a revocation or account disablement can commit between it and the change. Every state-changing editorial action must therefore re-check inside its own transaction with `permissions.RequireTx(ctx, tx, userID, permission)` or `permissions.RequireReviewerTx(ctx, tx, userID, subjectID)`. These share-lock the grant and account rows (`FOR SHARE`), so a concurrent revocation or disablement waits until the action commits and applies from the next transaction on. Such lock waits can end in a PostgreSQL deadlock or serialization error (SQLSTATE `40P01`/`40001`); callers report it, or retry the whole transaction, and never treat it as success.

Every grant and revoke is recorded in the append-only `permission_audit` table with actor (a user `actor_id`, or for local operator tools a self-asserted `operator_label` such as `user@host`, set through `permissions.OperatorAs`, the only operator constructor; labels with control, invisible, bidirectional or line-separator characters are rejected by both the code and the database), target, permission, subject and time (the database transaction time, equal to the grant's `granted_at`). Revoking from an unknown user or subject returns an error rather than a silent no-op. Revoking `administrator` from the last active administrator is refused (`permissions.ErrLastAdministrator`). Self-approval is rejected by the review workflow and by the database (LRN-007). No HTTP endpoint changes permissions or account status; they are administered by the local operator commands (`bootstrap-admin`, `grant`, `revoke`, `disable-user`, `enable-user`, `show-user`; [DEVELOPMENT.md](DEVELOPMENT.md#operator-commands-first-administrator-permissions-and-account-status)). Disabling an account deletes its sessions in the same transaction; a login racing the disable either fails with `invalid_credentials` or its new session is deleted by the disable.

## Studio: lesson drafts and media (LRN-006)

Decisions behind this section: [DECISIONS.md](DECISIONS.md#studio-api-lrn-006). Persistence rules (one active candidate, immutable submissions, versioned saves): [DECISIONS.md](DECISIONS.md#lesson-persistence-lrn-006). Upload rules: [DECISIONS.md](DECISIONS.md#media-upload-limits-lrn-006).

### Authorization

Every endpoint below, reads included, is checked in this order:

1. No valid session: `401 unauthenticated`.
2. Unsafe method (`POST`, `PUT`, `DELETE`): cross-origin request `403 cross_origin_rejected`; missing or wrong `X-CSRF-Token` `403 csrf_rejected`.
3. No `author` permission (students, reviewer-only, publisher-only and administrator-only accounts, disabled accounts): `403 forbidden`. This check happens before any ID is looked up, so it reveals nothing about which lessons, revisions or media exist.
4. Writes recheck `author` inside their database transaction, so a revocation racing the request either waits or denies. A racing revocation is reported as `404 not_found` on revision endpoints (indistinguishable from someone else's revision) and `403 forbidden` on `POST /api/lessons`, `POST /api/lessons/{lessonId}/drafts` and `POST /api/media`. If the permission check itself fails (database error), the response is `500 internal` or `409 retry`, never a denial.

| Endpoint | Who may call it |
| --- | --- |
| `POST /api/lessons` | Any `author`. The caller becomes the lesson's owner. With `translationOf`, only the owner of the source lesson; for anyone else, administrators included, `422 unknown_lesson`, identical to a missing source |
| `POST /api/lessons/{lessonId}/drafts` | The lesson's owner (its creator) only; everyone else with `author`, administrators included, gets `404 not_found`, identical to a missing lesson (see DECISIONS.md "Lesson ownership") |
| `GET /api/revisions/{revisionId}` | The revision's author only, whatever its status. Everyone else with `author` gets `404 not_found`, identical to a missing ID |
| `PUT /api/revisions/{revisionId}` | The draft's author only; others get `404 not_found` |
| `POST /api/revisions/{revisionId}/submit` | The draft's author only; others get `404 not_found` |
| `DELETE /api/revisions/{revisionId}` | The draft's author only; others get `404 not_found` |
| `POST /api/media` | Any `author` |
| `GET /api/media/{mediaId}` | An `author` who uploaded that asset (also when the upload was deduplicated) or who authored a revision referencing it. Everyone else with `author` gets `404 not_found`, identical to a missing ID. Reviewers use [`GET /api/review/media/{mediaId}`](#get-apireviewmediamediaid), publishers `GET /api/publish/media/{mediaId}`, and students the reader's `GET /learn/lessons/{publicId}/media/{mediaId}` |

Reviewer access to submitted revisions uses the [review endpoints](#review-and-publication-lrn-007), not these.

### Revision object

Returned by every lesson endpoint that succeeds with a body:

```json
{
  "id": 12,
  "lessonId": 4,
  "number": 1,
  "status": "draft",
  "authorId": 7,
  "baseRevisionId": null,
  "title": "Nombres complexes",
  "objectives": "…",
  "prerequisites": "…",
  "content": {"schemaVersion": 1, "title": "Nombres complexes", "language": "fr", "status": "unreviewed", "objective": "…", "blocks": []},
  "schemaVersion": 1,
  "language": "fr",
  "subjectId": 1,
  "chapterId": null,
  "version": 1,
  "createdAt": "2026-11-24T09:00:00Z",
  "updatedAt": "2026-11-24T09:00:00Z",
  "submittedAt": null,
  "mediaIds": [],
  "review": null,
  "approvedBy": null
}
```

- `status`: `draft`, `in_review`, `changes_requested` or `approved`. Publication is a lesson pointer, not a status.
- `authorId` is the revision's author. `review` is `null` until a reviewer decides, then `{"action": "approve" | "request_changes", "reviewerId": 9, "comment": "…" | null, "decidedAt": "…"}`; a change request always has a comment, which is the author's feedback. `approvedBy` is the approving reviewer's ID for approved revisions, otherwise `null`.
- `title`, `objectives`, `language` and `schemaVersion` are read from `content`; they are not sent separately.
- `content` is the stored schema v1 document ([fixtures/README.md](fixtures/README.md)), with embedded exercises and solutions. The server stores a canonical compact form, so key order and spacing may differ from what was sent.
- `version` changes on every save and on submission. Send it back as `expectedVersion`.
- `number` counts revisions of the lesson (1, 2, …). Numbers are never reused: after a discarded draft #2 the next draft is #3.
- IDs are numeric database IDs. `subjectId` and `chapterId` refer to the catalogue tables, read through the [catalogue endpoints](#catalogue).

### Draft fields

Lesson writes share these request fields:

| Field | Type | Notes |
| --- | --- | --- |
| `content` | object | Required. Schema v1 document, at most 128 KiB in compact form, `status` `unreviewed`. Validated with the reader's validator before anything is stored |
| `prerequisites` | string | Optional, at most 2000 characters |
| `chapterId` | integer or null | Optional; must be a chapter of the lesson's subject. Omitted or null clears it |
| `mediaIds` | integer array | Optional, at most 100 distinct uploaded asset IDs (duplicates are merged). Only assets the caller may read (see `GET /api/media/{mediaId}`) may be referenced; others get `422 unknown_media`, exactly like missing IDs. The list replaces the previous one. Every image the content embeds (`"src": "media:<id>"`, [schema](fixtures/README.md#experimental-content-contract)) must be in the list, else `422 unattached_media`; an attached image need not be embedded (removing it from the text does not detach it) |

Bodies are limited to 256 KiB (`413 too_large`).

### `POST /api/lessons`

Creates a lesson and its first draft.

```json
{"subjectId": 1, "language": "fr", "translationOf": null, "content": {…}, "prerequisites": "", "chapterId": null, "mediaIds": []}
```

- `subjectId` and `language` are required; `language` must equal `content.language` and is fixed for the lesson, as is the subject.
- `translationOf` (optional) names an existing lesson of the same subject, which the caller must own; the new lesson joins its translation group. A lesson the caller does not own is reported like a missing one (`422 unknown_lesson`).
- `201` revision object, `Location: /api/revisions/{id}`.
- `400`, `403`, `413`, `415`, `422 invalid_content | language_mismatch | unknown_subject | unknown_chapter | unknown_media | unattached_media | unknown_lesson | translation_subject`, `409 archived | translation_exists`.

### `POST /api/lessons/{lessonId}/drafts`

Starts a new draft of an existing lesson (for example the successor of a `changes_requested` revision). Only the lesson's owner may. Body: the draft fields. Its base is the lesson's published revision (`baseRevisionId`), read under a lock.

- `201` revision object, `Location` as above.
- `404 not_found` (unknown lesson, or a lesson the caller does not own; same body), `409 active_candidate_exists` (a draft, an in-review revision, or an approved revision awaiting publication exists; concurrent requests: exactly one succeeds), `409 archived`, `422` as above.

An approved revision keeps blocking new drafts until it is [published](#post-apirevisionsrevisionidpublish) or [withdrawn](#post-apirevisionsrevisionidwithdraw).

### `GET /api/revisions/{revisionId}`

- `200` revision object.
- `404 not_found` for a missing ID and for another author's revision (same body).

### `PUT /api/revisions/{revisionId}`

Saves the caller's own draft: content, prerequisites, chapter and media references are replaced together.

```json
{"expectedVersion": 3, "content": {…}, "prerequisites": "…", "chapterId": null, "mediaIds": [5]}
```

- `200` revision object with the new `version`.
- `409 version_conflict`: another tab or device saved first. Nothing is written; reload, merge and save again.
- `409 not_draft`: already submitted; start a new draft instead. `409 archived`.
- `404 not_found`, `400`, `413`, `415`, `422` as above.

### `POST /api/revisions/{revisionId}/submit`

Body `{"expectedVersion": 3}`. Moves the caller's own draft to `in_review` and records the submission (author, revision, time) in the append-only submission audit in the same transaction (`migrations/0011_submission_audit.sql`). From then on its content, metadata, exercises and media references are frozen by the database; corrections need a new draft.

- `200` revision object (`status` `in_review`, `submittedAt` set to the transaction time, which is also the audit row's time, `version` incremented). The submitter is always the revision's `authorId` (enforced by the database), so the revision object has no separate submitter field. A refused request writes no audit row.
- `409 version_conflict | not_draft | archived`, `404 not_found`, `422 invalid_content` (stored snapshot no longer validates).

### `DELETE /api/revisions/{revisionId}?expectedVersion=3`

Discards the caller's own draft and its media references (the assets remain). `expectedVersion` is the only query parameter, a plain decimal integer.

- `204` no body.
- `400 invalid_request` (missing or malformed `expectedVersion`), `409 version_conflict | not_draft | referenced_by_course`, `404 not_found`. A draft that a course draft pins as a step (LMS-B03) is `409 referenced_by_course`.

### `POST /api/media`

Uploads one image as the **raw request body** (no multipart) with `Content-Type: image/png`, `image/jpeg` or `image/webp`, plus `X-CSRF-Token`.

The type is detected from the bytes; the declared type must agree. The image is decoded and re-encoded (metadata removed; WebP stored as PNG). JPEG EXIF orientation is applied, so the stored image is upright and `width`/`height` are the upright size. Phone-photo structures after a JPEG's end (MPF previews, Ultra HDR gain maps, motion-photo video, zero padding) are validated and dropped; see [DECISIONS.md](DECISIONS.md#media-upload-limits-lrn-006). Limits: 5 MiB, 4096 px per side, 16 megapixels. A larger declared `Content-Length` is refused before the body is read; a longer body is cut off at the limit. Within these limits the server also bounds decoding memory ([measurements](measurements/upload-memory.md)): CMYK and YCCK JPEGs (print workflows; phones never produce them) are refused with `415 unsupported_cmyk`, and an image whose decoding would need more than 192 MiB, estimated from its headers (for example a progressive JPEG without chroma subsampling above about 13 megapixels, or 10 with EXIF rotation; a lossless WebP above about 7 megapixels or a WebP with lossless-compressed transparency above about 5, because Go's WebP decoder may need up to about 140 MiB for Huffman tables whatever the image size; an interlaced 16-bit PNG above about 12 megapixels), or whose stored form would exceed 16 MiB (for example a large detailed WebP photo, which is stored as PNG), is refused with `422 image_too_complex`. Ordinary phone photos, screenshots (PNG) and lossy WebP photos up to the limits are accepted.

```json
{"id": 5, "contentType": "image/png", "byteSize": 1234, "width": 800, "height": 600, "sha256": "…64 hex…", "url": "/api/media/5"}
```

- `201` (`Location` = `url`), whether the asset is new or identical canonical bytes were already uploaded (by anyone), in which case the existing asset ID is returned and the caller gains read access to it. Media IDs are sequential, so deduplication is observable only through the returned ID being older than expected. Assets never change; replacing an image means uploading a new one and referencing its ID.
- `400 invalid_request` (empty or interrupted upload), `413 too_large`, `415 unsupported_media_type | type_mismatch | unsupported_heic | unsupported_cmyk`, `422 invalid_image | invalid_dimensions | trailing_data | image_too_complex`, `408 upload_timeout`, `503 busy` (more than 4 uploads in progress for 5 seconds; `Retry-After: 5`), `409 retry`.
- The whole upload must arrive within the server's 15-second read timeout (about 3 Mbit/s for 5 MiB); otherwise `408 upload_timeout`.

### `GET /api/media/{mediaId}`

Returns the stored image bytes (also `HEAD`).

| Header | Value |
| --- | --- |
| `Content-Type` | `image/png` or `image/jpeg`, from the validated storage key |
| `Content-Disposition` | `inline` |
| `X-Content-Type-Options` | `nosniff` |
| `Content-Security-Policy` | `default-src 'none'; frame-ancestors 'none'; sandbox` |
| `Cache-Control` | `private, no-store` |
| `Vary` | `Cookie` |

- `404 not_found` for a missing or malformed ID (only positive decimal IDs; no keys, extensions or paths), for an asset the caller may not read, and for a row whose file or key is not valid. All return the same body.

## Review and publication (LRN-007)

Decisions behind this section: [DECISIONS.md](DECISIONS.md#review-and-publication-lrn-007). The workflow: an author submits a draft (`POST /api/revisions/{id}/submit`); a reviewer assigned to the lesson's subject approves it or requests changes with a comment; after a change request the author starts a successor draft (`POST /api/lessons/{lessonId}/drafts`) and submits it again; a `publisher` then publishes the approved revision. Every decision and publication is recorded in append-only audit tables in the same transaction.

### Authorization

Same conventions as above: no session `401 unauthenticated`; unsafe methods need the CSRF token and a same-origin request (`403 csrf_rejected` / `403 cross_origin_rejected`). Then:

| Endpoint | Who may call it |
| --- | --- |
| `GET /api/review/queue` | Any user assigned as `reviewer` for at least one subject; otherwise `403 forbidden` before anything is looked up (students, authors, publishers and administrators without an assignment). A disabled account has no session (`401`) |
| `GET /api/review/revisions/{revisionId}` | Same `403`. A reviewer of another subject, or a revision that is not in review, gets `404 not_found`, identical to a missing ID |
| `POST /api/review/revisions/{revisionId}/approve` | Same `403` and `404`. A reviewer who authored the revision gets `403 self_review`, administrators included |
| `POST /api/review/revisions/{revisionId}/request-changes` | As approve |
| `GET /api/review/media/{mediaId}` | Same `403`. Only assets referenced by a revision in review in one of the caller's subjects; anything else `404 not_found` |
| `POST /api/revisions/{revisionId}/publish` | `publisher` only, otherwise `403 forbidden` before any lookup. Reviewing never implies publishing, and `publisher` never allows approving |

Decisions and publication recheck the permission inside their database transaction, with the grant and account rows share-locked, so a revocation or disablement racing the request either waits until the write commits or denies it. A racing denial is `404 not_found` on review endpoints and `403 forbidden` on publication; nothing is written. If the check itself fails (database error) the response is `500 internal` or `409 retry`.

### `GET /api/review/queue`

Optional query parameter `subjectId` (a positive integer, at most once; no other parameters). Lists revisions in review in the caller's assigned subjects (or only `subjectId`, which must be one of them, else `404 not_found`), oldest submission first, at most 200, excluding the caller's own revisions.

```json
{"revisions": [{"revisionId": 12, "lessonId": 4, "number": 2, "title": "Nombres complexes", "language": "fr", "subjectId": 1, "chapterId": null, "authorId": 7, "authorName": "Amel", "submittedAt": "2026-11-24T09:00:00Z"}]}
```

- `200`; `400 invalid_request` for a malformed query.

### `GET /api/review/revisions/{revisionId}`

- `200` [revision object](#revision-object) of a revision in review, including `content` and `mediaIds`.
- `404 not_found` as described above. A reviewer may read their own revision but cannot decide on it.

### `POST /api/review/revisions/{revisionId}/approve`

Body `{"comment": "…"}`; `comment` is optional (omitted, null, empty or whitespace means no comment), trimmed, at most 5000 characters.

- `200` revision object with `status` `approved`, `review` and `approvedBy` set. The snapshot is otherwise unchanged; the revision stays the lesson's active candidate until published.
- `409 not_in_review` (a draft, or already decided: of two concurrent decisions exactly one is applied), `403 self_review`, `404 not_found`, `400`, `415`, `422 invalid_comment`.

### `POST /api/review/revisions/{revisionId}/request-changes`

Body `{"comment": "…"}`; `comment` is required (non-blank, at most 5000 characters).

- `200` revision object with `status` `changes_requested` and `review` set. The revision is kept as history; the lesson accepts a successor draft.
- `422 comment_required | invalid_comment`, then the same errors as approve.

### `GET /api/review/media/{mediaId}`

Same response and headers as [`GET /api/media/{mediaId}`](#get-apimediamediaid), for assets referenced by a revision in review in one of the caller's subjects. Access ends when the revision leaves review.

### `POST /api/revisions/{revisionId}/publish`

Body `{}` (an empty JSON object; any key is rejected). Publishes the approved revision: in one transaction the lesson row is locked, the revision must be an approved revision of that lesson that was never published, its `baseRevisionId` must equal the lesson's current published revision, the publication is audited and the lesson's published pointer is replaced.

```json
{"lessonId": 4, "publishedRevisionId": 12, "previousRevisionId": 9, "publishedBy": 3, "publishedAt": "2026-11-24T09:00:00Z"}
```

- `200` as above. The revision stops being the active candidate, so the lesson accepts a new draft based on it.
- `409 stale_base`: the published revision changed since this revision was drafted (for example after an unpublication or restoration, LRN-009). Nothing changes; a reconciled revision must be drafted and reviewed. While this revision stays approved it keeps blocking new drafts until an administrator [withdraws](#post-apirevisionsrevisionidwithdraw) it (open question in DECISIONS.md).
- `409 not_approved | withdrawn | already_published | archived`, `404 not_found` (missing revision), `403 forbidden`, `400`, `415`. Of concurrent publications of one revision exactly one succeeds and is audited once; the others get `409 already_published`.

## Withdrawal, unpublication and restoration (LRN-009)

Implemented in `internal/studioapi/withdraw.go` over `internal/review` and `migrations/0007_withdrawal_restoration.sql`. Decisions and open questions: [DECISIONS.md](DECISIONS.md#withdrawal-unpublication-and-restoration-lrn-009).

### Authorization

Session, CSRF token and strict JSON as for the Studio API. Withdrawal and unpublication need the global `administrator` permission; restoration needs `publisher` or `administrator`. The permission is checked before any lookup (`403 forbidden`, so existing and missing IDs look the same) and again inside the transaction with the grant and account rows share-locked; a revocation committed in between also gives `403`. These permissions are global, so their holders may learn which revisions and lessons exist (`404 not_found` for a missing or malformed ID).

### Expected published revision

Every request carries `expectedPublishedRevisionId`: the lesson's published revision the caller saw (a positive integer), or `null` if the lesson had none. It is required (a missing key is `400`). The server compares it with the current pointer while holding the lesson row lock; a mismatch is `409 stale_pointer` and nothing changes. Every change of the published revision, including clearing it, writes a row to the pointer audit in the same transaction; the database refuses a pointer change without one.

### Pointer change object

Returned by unpublication and restoration, and embedded in a withdrawal that unpublished:

```json
{"lessonId": 4, "action": "restore", "publishedRevisionId": 9, "previousRevisionId": 12, "actorId": 3, "reason": null, "occurredAt": "2026-11-24T09:00:00Z"}
```

`action` is `restore` or `unpublish`; `publishedRevisionId` is the new pointer (`null` after unpublication); `reason` is set for unpublication only.

### `POST /api/revisions/{revisionId}/withdraw`

Body `{"reason": "…", "expectedPublishedRevisionId": 12}`. `reason` is required, trimmed, at most 2000 characters. Withdraws an approved revision as incorrect or unlawful: it can never again be published or restored. In one transaction:

- the withdrawal (revision, administrator, reason, time) is recorded;
- if the revision is the published one, the lesson is unpublished (pointer to `null`, audited with the same reason);
- if the revision is an approved candidate awaiting publication, it stops being the lesson's active candidate, so the lesson accepts a new draft.

```json
{"revisionId": 12, "lessonId": 4, "withdrawnBy": 1, "reason": "Incorrect worked solution.", "withdrawnAt": "2026-11-24T09:00:00Z", "unpublication": {"lessonId": 4, "action": "unpublish", "publishedRevisionId": null, "previousRevisionId": 12, "actorId": 1, "reason": "Incorrect worked solution.", "occurredAt": "2026-11-24T09:00:00Z"}}
```

`unpublication` is `null` when the revision was not the published one.

- `409 not_approved` (only approved revisions can be withdrawn), `409 already_withdrawn`, `409 stale_pointer`, `422 reason_required | invalid_reason`, `404`, `403`, `400`, `415`. Withdrawal racing a publication of the same revision: either the withdrawal wins (the publication gets `409 withdrawn`) or the publication wins (the withdrawal gets `409 stale_pointer`).

### `POST /api/lessons/{lessonId}/unpublish`

Body `{"reason": "…", "expectedPublishedRevisionId": 12}`; here `expectedPublishedRevisionId` must be the published revision to remove (`null` is `400`). Emergency unpublication: the lesson's pointer goes to `null`, audited with the administrator and reason. The revision stays approved and can be restored.

- `200` pointer change object with `action` `unpublish`.
- `409 not_published | stale_pointer`, `422 reason_required | invalid_reason`, `404` (unknown lesson), `403`, `400`, `415`.

### `POST /api/revisions/{revisionId}/restore`

Body `{"expectedPublishedRevisionId": 12}` (or `null` for an unpublished lesson). Points the lesson back at a revision that was published before. The revision must be an approved revision of the lesson, never withdrawn, previously published, and still eligible: its content must pass the current schema v1 validator and its media references must resolve. The historical base check of publication is not applied. Restoration does not reopen the restored revision as a candidate, so it never itself blocks new drafts (their base is the restored revision); an approved candidate already pending for the lesson still blocks them until it is published or withdrawn (open question in DECISIONS.md).

- `200` pointer change object with `action` `restore`.
- `409 never_published` (an approved revision that was never published goes through [publication](#post-apirevisionsrevisionidpublish) and its base check; see the open question in DECISIONS.md), `409 withdrawn | not_approved | already_current | stale_pointer | archived | ineligible`, `404`, `403`, `400`, `415`. Restoration racing a publication or another restoration: exactly one applies and is audited once; the other gets `409 stale_pointer` or `409 stale_base`.

## Student reader (LRN-008)

Server-rendered HTML pages in French (interface language), in `internal/reader`. Decisions: [DECISIONS.md](DECISIONS.md#student-reader-lrn-008). They are not part of the JSON contract above: errors are HTML pages, forms are `application/x-www-form-urlencoded` (bodies over 8 KiB are refused; 32 KiB for the error report form), and redirects use `303 See Other`.

### Headers and access

- Every response from a reader route, including redirects, error pages, session-lookup failures and the unversioned `/static/` files, sends `Cache-Control: private, no-store` and `Vary: Cookie`. Responses produced before a reader route is reached (the cross-origin `403 cross_origin_rejected`, and the server's JSON `404 not_found` catch-all for unknown paths and for unsupported methods on reader paths, such as `PUT /learn`) carry the server-wide `Cache-Control: no-store` and `Vary: Cookie` instead; they contain no lesson data. Neither shared caches nor the browser may store any of them. Pages load their styles and scripts from the fingerprinted [`/assets/`](#fingerprinted-assets-assetsversionpath) URLs instead, which are cacheable (issue #88).
- Pages send `Content-Security-Policy: default-src 'none'; style-src 'self'; img-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'`. Lesson pages add `script-src 'self'`, `font-src 'self'`, `style-src-attr 'unsafe-inline'` (inline style attributes produced by KaTeX) and `frame-src https://www.youtube-nocookie.com` (the click-to-load video). No inline scripts.
- Anonymous `GET`/`HEAD` on `/learn…` pages: `303` to `/login?next=<path>`, whether or not the lesson exists. Anonymous `POST` on `/learn…`: `401` page. Neither contains lesson data.
- Unsafe methods pass Go's cross-origin protection (`403 cross_origin_rejected` JSON body, as for the API). Authenticated form posts also need the session's CSRF token in the form field `csrf_token` (the same value as `csrfToken`); missing or wrong: `403` page. A token in the query string is ignored.
- Any signed-in account can read; no permission is needed or granted. Invited students hold no permission and get `403`/`404` from the Studio and review APIs.
- Subjects and chapters are shown by their [catalogue display names](#catalogue), never by a name stored in the lesson: navigation (dashboard subjects, subject page heading and chapter headings) prefers the French name, a lesson's breadcrumb and its context in bookmarks and search results prefer the lesson's language; then the other language's name, then the code. Arabic names are wrapped in `<bdi lang="ar" dir="rtl">`. Subjects on the dashboard follow the catalogue display order, then code.
- URLs carry only identifiers: subject and chapter codes, the lesson's random `public_id` (UUID), and the search query on `/learn/search`. Invitation tokens appear only in the invitation link (`Referrer-Policy: no-referrer` on that page); the form posts the token in its body.

### Routes

| Route | Session | Result |
| --- | --- | --- |
| `GET /login?next=/learn/…` | — | Sign-in form (`next` may also be a `/studio` path). Signed-in users are redirected to `next` |
| `POST /login` | — | Fields `email`, `password`, `next`. Same service as `POST /api/auth/login`: same throttle (`429` page with `Retry-After`), same enumeration-safe failure (`401` page), new rotated session cookie, `303` to `next`. `next` must be `/learn`, `/studio` or a `/learn/…` or `/studio/…` path of plain segments; anything else (other paths, hosts, `//`, queries, `..`) becomes `/learn` |
| `POST /logout` | required | Field `csrf_token`. Deletes the session, clears the cookie, `303` to `/login` |
| `GET /invite/{token}` | — | Invitation form showing the invited email, or a `404` page with one message for unknown, malformed, expired and used tokens. Throttled |
| `POST /invite` | — | Fields `token`, `display_name`, `password`, `password_confirm`. Validation errors: `422` form (the display name is kept, passwords are not). Success: creates the Student account, marks the invitation used, signs in, `303` to `/learn`. Invalid token: the same `404` page. Throttled |
| `GET /learn` | required | Dashboard: the user's bookmarks (newest first, at most 500; lessons no longer published are listed as unavailable), subjects with published lessons, search box; extended into the student home by [LMS-B08](#course-reading-lms-b08) |
| `GET /learn/subjects/{code}` | required | The subject's published lessons grouped by the published revision's chapter (chapters by catalogue display order, then code; "Sans chapitre" last; lessons by title). `404` if the subject has no published lesson |
| `GET /learn/lessons/{publicId}` | required | Published lesson: breadcrumb, title, author and approving-reviewer credit (display names), bookmark button, objective and the canonical body from `rendering.RenderBlocks`. `404` page for unknown, malformed (only lower-case UUIDs), never-published, unpublished and archived lessons alike |
| `POST /learn/lessons/{publicId}/bookmark` | required + `csrf_token` | Bookmarks a published lesson (idempotent), `303` back to the lesson (or to `/learn` with `return=dashboard`). `404` if not published; `409` page at 500 bookmarks |
| `POST /learn/lessons/{publicId}/unbookmark` | required + `csrf_token` | Removes the bookmark, published or not (idempotent), `303` as above |
| `POST /learn/lessons/{publicId}/report` | required + `csrf_token` | Files an [error report](#error-reports-lrn-015) on the published revision shown. Fields `message` and `revision`. `303` to the confirmation page; `422` lesson page with the form reopened (text kept) for invalid text; `409` lesson page showing the current revision (text kept) if the published revision changed, or with a generic "try again" message (text kept) after a concurrent conflict or guard refusal; `413` lesson page (form reopened, text not kept) for a body over 32 KiB, which is never accepted, stored or throttled; `400` page for missing or malformed fields; `404` if the lesson is not published; `429` page when throttled |
| `GET /learn/lessons/{publicId}/media/{mediaId}` | required | An uploaded image embedded in the lesson's **currently published** revision (issue #77): the revision is the one `reader_published_lessons` exposes (approved, not withdrawn, lesson not archived), the asset is in its frozen `mediaIds` and its content embeds it as `media:{mediaId}`. Any signed-in account, as for the lesson page. Image bytes with `Content-Type` from the validated storage key, `Content-Disposition: inline`, `X-Content-Type-Options: nosniff`, `Content-Security-Policy: default-src 'none'; frame-ancestors 'none'; sandbox`, `Cache-Control: private, no-store`, `Vary: Cookie`; also `HEAD`. Everything else gets the same JSON `404 not_found` with no redirect: anonymous requests, malformed IDs or public IDs, another lesson's path, a draft, in-review or approved-unpublished revision's image, an image attached but not embedded, and lessons unpublished, withdrawn or archived. After a restoration, the restored revision's images are served again and the withdrawn revision's are not |
| `GET /learn/lessons/{publicId}/reported` | required | Confirmation page with a link back to the lesson; shows no lesson data |
| `GET /learn/search?q=…` | required | Title search over published lessons, see below |
| `GET /static/{path}` | — / required | Unversioned, `private, no-store`, kept for the renderer experiment and the Studio development server; pages use `/assets/` below. Public: `tokens.css`, `site.css`, `reader.css`, `math.js`, `vendor/katex/…` (generic code, no lesson content). The renderer's fixture image `complex-plane.svg` needs a session (uploaded images use the lesson media route above). Anything else, directories, and media without a session: `404` |
| `GET /assets/{version}/{path}` | — | The same public files, fingerprinted; see below |

### Fingerprinted assets: `/assets/{version}/{path}`

Issue #88, [DECISIONS.md](DECISIONS.md#fingerprinted-static-assets-issue-88). `{path}` is a public asset's `/static/` name (`site.css`, `vendor/katex/katex.min.css`, `vendor/katex/fonts/KaTeX_Main-Regular.woff2`…; `.css`, `.js`, `.woff2`, `.woff` and `.ttf` only) and `{version}` the 16 lowercase hex digits the server computed at start-up from SHA-256 of the file, or of the whole `vendor/katex/` directory for KaTeX files (so the fonts that `katex.min.css` loads by relative URL resolve under the same version). Pages get the URLs from the server; clients must not build them.

- No session is read (the session cookie is ignored and never cleared) and the response is the same for every requester: `200` with `Cache-Control: public, max-age=31536000, immutable`, an explicit `Content-Type` (`text/css; charset=utf-8`, `text/javascript; charset=utf-8`, `font/woff2`, `font/woff`, `font/ttf`), `ETag` (the file's SHA-256; `If-None-Match` gives `304`), `X-Content-Type-Options: nosniff`, no `Vary`, no `Set-Cookie`. `HEAD` is supported.
- Any other version of a name (an earlier build), an unknown or non-public name (lesson media, renderer files, KaTeX's `LICENSE`), a directory or a malformed version: the server's JSON `404 not_found` with `Cache-Control: no-store`. Never a redirect.

### Invitation throttling

Each invitation page view and form submission counts one attempt per client address (as for login) in the login throttle table under its own key scope. More than 20 attempts in the sliding 15-minute window return a `429` page with `Retry-After`. Tokens are 256-bit random values, so the limit protects the server rather than the tokens. Acceptance also signs in through the login service, so it counts one login attempt for the new account.

### Title search

- The query is trimmed, control characters are dropped and whitespace runs collapse to one space. It must then be 2 to 100 characters, and still at least 2 characters after folding (below), so a query of only harakat, tatweel or combining accents is refused rather than matching every title; otherwise the page asks for a valid query and runs no search.
- Matching is a substring match on the published revision's title. Both sides go through the SQL function `reader_fold` (migration 0008): Unicode NFD, combining marks removed (French accents, Arabic harakat, hamza and madda marks, superscript alef, tatweel), lower case, `œ`→`oe`, `æ`→`ae`. The folding does not depend on the database locale (it gives the same keys under `C` and `en_US.UTF-8`): ASCII is lowered with `lower(… COLLATE "C")`, and the other capitals it folds are listed explicitly (the Latin-1 and Latin Extended-A letters without a canonical decomposition, such as `Œ`, `Æ`, `Ø`, `Ł`, and the basic Greek capitals, with final `ς` folded to `σ`); capitals of other scripts, and capitals such as `ẞ`, are left unchanged under every locale (Cyrillic `Б` does not match `б`). So `equations` finds "Équations", `الدوال` finds "الدَّوَالّ" and `الاسية` finds "الأسية". Other Arabic letter variants (`ى`/`ي`, `ة`/`ه`) are not unified.
- The query is a bound parameter used with `strpos`, never SQL text or a `LIKE` pattern, so `%`, `_`, quotes and backslashes match literally.
- At most 50 results, ordered by folded title. There is no index for substring search: it scans the published lessons, which is fine for the pilot's size; a trigram index needs the `pg_trgm` extension and is left to later.

## Error reports (LRN-015)

Decisions: [DECISIONS.md](DECISIONS.md#error-reports-lrn-015). A signed-in reader reports an error on the published revision they were reading; reviewers of the lesson's subject and administrators list, read, resolve and dismiss reports through the review API. Reports never change lesson content: a correction is a new revision that goes through review and publication.

### Filing a report (reader)

- The lesson page has a closed "Signaler une erreur" section (`<details>`, no JavaScript) holding a form that posts to `POST /learn/lessons/{publicId}/report` with `csrf_token`, `message` and `revision` (the lesson-scoped number of the published revision displayed, not its database ID). Each field must appear exactly once in the body.
- Any signed-in, active account may report (CAHIER_DES_CHARGES §2.2: students, teachers, reviewers, administrators). Anonymous `401` page, CSRF `403` page and cross-origin `403 cross_origin_rejected`, as for bookmarks.
- `message`: line breaks are normalized to LF and surrounding whitespace trimmed; it must then hold 1 to 2000 characters of valid UTF-8 with no control character other than LF (tabs included) and no bidirectional formatting (U+061C, U+200E, U+200F, U+202A–U+202E, U+2066–U+2069) or line/paragraph separator (U+2028, U+2029) characters. The database enforces the same rule. Invalid text: `422` lesson page, form reopened with the text and an error. The textarea has no `maxlength` (browsers count UTF-16 code units, not characters); the hint states the limit and the server enforces it.
- The report names the revision shown: the server compares `revision` with the lesson's current published revision under a share lock on the lesson row, and a database guard repeats the check, so the stored `revision_id` is the displayed revision and is published at the report's commit. If it changed since the page was displayed: `409` lesson page showing the current revision, with the text kept; nothing is stored.
- Throttling: every authenticated, CSRF-valid submission (valid or not) counts one attempt for the account and one for the client address, in the login throttle table under their own key scopes. More than 10 per account or 60 per address in the sliding 15-minute window return a `429` page with `Retry-After`; nothing is stored. Sign-in and invitation budgets are separate.
- Success: `303` to `GET /learn/lessons/{publicId}/reported`. The reporter cannot list their reports or see their status.

### Authorization (review API)

Same conventions as above: no session `401 unauthenticated`; unsafe methods need the CSRF token and a same-origin request. Every route first requires a `reviewer` assignment for at least one subject or the `administrator` permission (`403 forbidden` before anything is looked up), so students, authors and publishers without either get `403`. Then, inside the transaction (grant and account rows share-locked): an active administrator sees and handles every report; a reviewer only reports on lessons of subjects they are actively assigned to; any other report is `404 not_found`, identical to a missing ID.

### Report object

```json
{"id": 3, "lessonId": 4, "lessonPublicId": "0b9c…", "subjectId": 1, "revisionId": 12, "revisionNumber": 2,
 "revisionTitle": "Nombres complexes", "currentlyPublished": true, "message": "Le signe de la ligne 2 est faux.",
 "status": "open", "createdAt": "2026-11-24T09:00:00Z", "resolution": null}
```

- `revisionId`/`revisionNumber`/`revisionTitle` are the revision the reader displayed; `currentlyPublished` tells whether it is still the lesson's published, readable revision.
- A report filed from a course step (LMS-B08, [below](#course-reading-lms-b08)) also carries `courseRevisionId`, `coursePublicId` and `courseRevisionNumber`, the published course revision the step was displayed in; its `revisionId` is the step's pinned revision, which may differ from the lesson's standalone published revision, and `currentlyPublished` then tells whether that step is still readable in that course revision. The three fields are absent for other reports.
- `message` is plain text with LF line breaks; clients must display it as text, never as HTML.
- `status` is `open`, `resolved` or `dismissed`. `resolution` is `null` while open, then `{"action": "resolve" | "dismiss", "actorId": 7, "note": "…", "occurredAt": "…"}`.
- The reporting account is stored but not returned.

### `GET /api/review/error-reports`

Optional query parameters, each at most once and nothing else: `status` (`open`, `resolved` or `dismissed`) and `subjectId` (a positive integer; for a reviewer it must be one of their subjects, else `404 not_found`). Lists visible reports, newest first, at most 200, no pagination: `{"reports": [ … ]}`. `400 invalid_request` for a malformed query.

### `GET /api/review/error-reports/{reportId}`

The report object. `404 not_found` for missing, malformed and other-subject IDs alike.

### `POST /api/review/error-reports/{reportId}/resolve` and `…/dismiss`

Body `{"note": "Corrigé dans la révision 3."}` (strict JSON, `note` required). The note follows the `message` rules above (1 to 2000 characters after trimming). In one transaction the report row is locked, the caller rechecked, an append-only resolution (report, actor, action, note, transaction time) inserted and the status set to `resolved` or `dismissed`. `200` with the report object; `422 note_required | invalid_note`; `404 not_found`; `409 not_open` if already resolved or dismissed (of concurrent resolutions exactly one applies); `409 retry` on a deadlock or serialization failure. A report is closed once and cannot be reopened; its text and the resolution never change.

## Studio integration (issue #65)

What the Studio needs from the authenticated application, so that neither `localStorage` nor the loopback renderer experiment is part of the authoring flow. Decisions: [DECISIONS.md](DECISIONS.md#studio-integration-issue-65). Every endpoint below needs a session (`401 unauthenticated`), runs the [conventions](#conventions) (strict JSON, CSRF on `POST`) and answers with `Cache-Control: private, no-store`.

| Endpoint | Who may call it |
| --- | --- |
| `GET /api/catalogue/subjects` | `author`, or a reviewer of any subject. Everyone else (students, publisher-only and administrator-only accounts): `403 forbidden` |
| `GET /api/catalogue/subjects/{code}/chapters` | As above. The permission check runs first, so a student gets `403` for known and unknown codes alike |
| `GET /api/lessons/mine` | `author` (checked by the middleware and again inside the listing's transaction). Lists only the caller's own lessons; administrators get no exception |
| `POST /api/preview` | Every account with Studio access: `author`, `publisher`, `administrator`, or a reviewer of any subject (issue #66 second slice: reviewers, publishers and administrators read submitted revisions through it). Students: `403 forbidden` |

### Catalogue

Subjects and chapters are identified by immutable codes (1 to 63 lower-case ASCII letters, digits and hyphens, not starting with a hyphen) and carry the IDs that `POST /api/lessons` (`subjectId`) and draft fields (`chapterId`) take. Each entry also has optional display names, `nameFr` and `nameAr` (`null` until an operator sets them; clients show the code instead), and a display order `sortOrder` (0 to 9999, default 0). Lists are ordered by `sortOrder`, then code, and bounded to 1000 entries. Names and order are display metadata, changed only by the audited operator commands `set-subject-name`, `set-chapter-name` and `set-order` ([DEVELOPMENT.md](DEVELOPMENT.md#provisioning-teachers-and-the-catalogue)); they are not part of any revision or snapshot. Nothing is seeded: the pilot catalogue waits for teacher-confirmed names (LRN-014). The name and order fields were added in issue #86; clients that ignore unknown fields are unaffected.

`GET /api/catalogue/subjects` (illustrative values):

```json
{"subjects": [{"id": 2, "code": "physique", "nameFr": null, "nameAr": null, "sortOrder": 0, "retiredAt": null},
              {"id": 1, "code": "maths", "nameFr": "Mathématiques", "nameAr": "رياضيات", "sortOrder": 1, "retiredAt": null}]}
```

Subjects and chapters also carry `retiredAt` (LMS-B02), `null` unless an administrator [retired](#curriculum-lms-b02) the entry. A retired entry stays listed and keeps resolving, but cannot be newly selected: no new lesson in a retired subject (`422 retired_subject`), no draft setting a retired chapter other than its published base revision's (`422 retired_chapter`), no new reviewer grant or chapter on a retired subject, no new curriculum assignment of either (`422 placement_invalid`).

`GET /api/catalogue/subjects/{code}/chapters`: `404 not_found` for an unknown or malformed code.

```json
{"subject": {"id": 1, "code": "maths", "nameFr": "Mathématiques", "nameAr": "رياضيات", "sortOrder": 1},
 "chapters": [{"id": 4, "code": "complexes", "nameFr": null, "nameAr": null, "sortOrder": 0},
              {"id": 3, "code": "suites", "nameFr": null, "nameAr": null, "sortOrder": 2}]}
```

`GET /api/lessons/mine` returns the names too: `subjectNameFr` and `subjectNameAr` on each lesson, `chapterNameFr` and `chapterNameAr` on `latestRevision` (`null` when unset or without a chapter). They are read at request time, so renaming a chapter changes how every lesson placed in it is labelled, published ones included.

Lists are always arrays, never `null`.

### `GET /api/lessons/mine`

The caller's lessons (`lessons.created_by`), most recently updated first (by the latest revision's `updatedAt`, or the lesson's creation when it has no revision), at most 500. Another author's lessons never appear, whatever the caller's other permissions.

```json
{"lessons": [{
  "id": 7, "publicId": "0b6f…", "subjectId": 1, "subjectCode": "maths",
  "subjectNameFr": "Mathématiques", "subjectNameAr": "رياضيات", "language": "fr", "translationGroupId": "5c1e…", "archived": false, "publishedRevisionId": 18,
  "createdAt": "2026-11-20T09:00:00Z", "hasFeedback": true, "feedbackRevisionId": 20,
  "latestRevision": {"id": 21, "number": 3, "status": "draft", "title": "Les nombres complexes",
    "chapterId": 4, "chapterCode": "complexes", "chapterNameFr": null, "chapterNameAr": null,
    "version": 5, "updatedAt": "2026-11-25T10:12:00Z"}
}]}
```

- `latestRevision` is the highest-numbered revision whatever its status (`draft`, `in_review`, `changes_requested`, `approved`), or `null` when the lesson's only draft was discarded. Its full content and review comment come from [`GET /api/revisions/{revisionId}`](#get-apirevisionsrevisionid).
- `hasFeedback` is `true` when the author has reviewer feedback to act on: the lesson's most recent review decision requested changes and no later revision has been submitted. It stays `true` while the successor draft (started with `POST /api/lessons/{lessonId}/drafts`) is edited, and turns `false` once a later revision is submitted. `feedbackRevisionId` names the revision that carries the feedback (read its `review.comment` with `GET /api/revisions/{revisionId}`), `null` when `hasFeedback` is `false`.
- `publishedRevisionId` is the lesson's current published revision, `null` if none (never published, unpublished or withdrawn).

### `POST /api/preview`

Body `{"content": {…schema v1 document…}, "mediaRoute": "author"}` (strict JSON, `content` required and not `null`, at most 256 KiB as for drafts). `mediaRoute` is optional: `"author"` (default), `"review"` or `"publish"`; any other value, including `null`, is `400 invalid_request`. It only chooses the URL embedded uploaded images (`media:<id>`) are linked to: `/api/media/{id}`, `/api/review/media/{id}` or `/api/publish/media/{id}`; that route checks access when the image is fetched, so the preview itself reveals nothing about an asset. The document is validated exactly as a draft save validates it (valid UTF-8, no NUL, the canonical schema v1 decoder and its 128 KiB document limit) and rendered by the production renderer (`internal/rendering`), never by the renderer experiment. Nothing is stored.

`200`:

```json
{"html": "<h1 lang=\"fr\" dir=\"ltr\">…</h1><p class=\"objective\" …>…</p><article class=\"lesson-body\" …>…</article>"}
```

`html` is the reader's markup for the same document, byte for byte except the `src` of embedded uploaded images (the reader links them to `/learn/lessons/{publicId}/media/{id}`): the lesson heading, the objective paragraph (absent without an objective) and the `article` holding the lesson body, as on `/learn/lessons/{publicId}`. Math is emitted as `.math` elements for the client to typeset with the locally hosted KaTeX, as the reader does; the YouTube block is the same click-to-load button. Errors: `400 invalid_request`, `413 too_large`, `415 unsupported_media_type`, `422 invalid_content` with the validator's message. The preview does not check `mediaIds` (it has none); saves do.

### Studio pages: `GET /studio` and `GET /studio/{path…}`

The built Studio bundle (`web/studio/dist`, built with Vite `base: '/studio/'`), served from `LEARNOO_STUDIO_DIR` ([DEVELOPMENT.md](DEVELOPMENT.md#studio-in-the-application), [DEPLOYMENT.md](DEPLOYMENT.md#build)). Not routed when that variable is unset.

- Access: a session and `author`, `publisher` or `administrator`, or a reviewer assignment for any subject. Without a session, page requests are redirected (`303`) to `/login?next=/studio…`; signed-in users without access (students) get the HTML `403` page. Asset requests (a last path segment containing a dot) answer `404 not_found` in both cases, so file names are not revealed.
- Files: an existing regular file of the bundle is served with its content type; a missing file, a hidden file (segment starting with `.`), a directory, or any path leaving the bundle (including through a symbolic link) is `404`. Every other path under `/studio/` is a client route and gets `index.html`. The distinction is the last path segment only: **Studio client routes must never contain a dot in their last segment** (`/studio/lessons/12` is a route; `/studio/lessons/v1.2` would be looked up as a file and answer `404`). The `Accept` header is not used.
- Headers: `Cache-Control: private, no-store`, except a bundle file named like Vite's output under `assets/` (`assets/<name>-<hash of at least 8 characters>.<ext>`) served to staff: `private, max-age=31536000, immutable`, browser cache only (issue #88, see DECISIONS.md); `Vary: Cookie`, `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, no `Last-Modified` or `ETag`, and `Content-Security-Policy: default-src 'none'; script-src 'self'; style-src 'self'; style-src-attr 'unsafe-inline'; img-src 'self'; font-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'`. The Vite build emits only an external module script and stylesheet, so no inline script is allowed; the editor does not inject a `<style>` element (TipTap `injectCSS: false`, base rules in the Studio stylesheet), so none is allowed either; inline style attributes are, for the editor and KaTeX, as on the reader's lesson page.
- `index.html` is sent with `<meta name="learnoo-katex-css">` and `<meta name="learnoo-katex-js">` added before `</head>`, giving the fingerprinted KaTeX URLs the preview loads.

## Studio publication reads (issue #66, second slice)

Read-only endpoints the Studio's publisher and administrator views need ([DECISIONS.md](DECISIONS.md#studio-review-publication-and-administration-issue-66-second-slice)), in `internal/review/publishing.go` and `internal/studioapi/publishing.go`. Session, `Cache-Control: private, no-store`. The permission is checked by the middleware before any lookup (`403 forbidden`, so existing and missing IDs look the same) and again inside the read's transaction with the grant and account rows share-locked, so a revocation committed in between also gives `403`. These are global permissions, so a missing ID is a plain `404 not_found`. None of them takes query parameters (`400 invalid_request`).

| Endpoint | Who may call it |
| --- | --- |
| `GET /api/publish/queue` | `publisher` |
| `GET /api/publish/revisions/{revisionId}` | `publisher` |
| `GET /api/publish/media/{mediaId}` | `publisher` |
| `GET /api/lessons/{lesson}/publication` | `publisher` or `administrator` (both may restore); publishers get a reduced view, below |
| `GET /api/admin/lessons` | `administrator` |

**Awaiting publication** means: status `approved`, still the lesson's candidate, never published, not withdrawn, lesson not archived.

`GET /api/publish/queue`: revisions awaiting publication, oldest approval first, at most 200. `staleBase` is `true` when the lesson's published revision is no longer the revision's base: publication will be refused with `409 stale_base`.

```json
{"revisions": [{"revisionId": 12, "lessonId": 4, "lessonPublicId": "0b9c…", "number": 2, "title": "Nombres complexes", "language": "fr",
  "subjectId": 1, "subjectCode": "maths", "authorId": 7, "authorName": "Amel", "approvedBy": 9, "approverName": "Karim",
  "approvedAt": "2026-11-24T09:00:00Z", "baseRevisionId": 9, "publishedRevisionId": 9, "staleBase": false}]}
```

`GET /api/publish/revisions/{revisionId}`: `{"item": {…queue item as above…}, "revision": {…revision object…}}` for a revision awaiting publication, so the publisher reads what they publish with its facts (current pointer, `staleBase`, names) however long the queue is. Any other revision (in review, published, withdrawn, archived lesson, missing, malformed ID): `404 not_found`.

`GET /api/publish/media/{mediaId}`: the image bytes of an uploaded asset referenced by a revision awaiting publication (issue #77), with the headers of [`GET /api/media/{mediaId}`](#get-apimediamediaid). Any other asset, a malformed ID, or a caller who lost `publisher` (rechecked in the query, with the account not disabled): `404 not_found`. It lets the publication page show the images of what is published; once published, images are read through the reader.

`GET /api/lessons/{lesson}/publication`: `{lesson}` is the numeric lesson ID or its public ID (lower-case UUID, as in reader URLs); anything else is `404`. The lesson's publication state, its submitted revisions (drafts are never listed; newest first, at most 200) and the pointer audit (newest first, at most 100). Whether the caller is an administrator is decided inside the transaction:

- `"view": "administrator"`: everything below.
- `"view": "publisher"` (callers without `administrator`): only what restoration needs, since the audit log is the administrator's (CAHIER_DES_CHARGES §2.2). Only `approved` revisions are listed, with `id`, `number`, `status`, `title`, `approvedAt`, `everPublished`, `candidate` and `withdrawn`; `authorId`, `authorName`, `baseRevisionId`, `submittedAt`, `approvedBy`, `approverName`, `withdrawal` and `submission` are `null`, and `history` is empty.

```json
{"view": "administrator", "lessonId": 4, "publicId": "0b9c…", "subjectId": 1, "subjectCode": "maths", "language": "fr", "archived": false, "publishedRevisionId": 9,
 "revisions": [{"id": 12, "number": 2, "status": "approved", "title": "…", "authorId": 7, "authorName": "Amel", "baseRevisionId": 9,
   "submittedAt": "…", "approvedBy": 9, "approverName": "Karim", "approvedAt": "…", "everPublished": true, "candidate": false, "withdrawn": true,
   "withdrawal": {"withdrawnBy": 1, "withdrawnByName": "Lina", "reason": "…", "withdrawnAt": "…"},
   "submission": {"submittedBy": 7, "submittedAt": "…", "backfilled": false}}],
 "history": [{"action": "unpublish", "publishedRevisionId": null, "previousRevisionId": 12, "actorId": 1, "actorName": "Lina", "reason": "…", "occurredAt": "…"}]}
```

`everPublished`: a publication names the revision (restoration is limited to such revisions). `candidate`: approved and still blocking new drafts. `withdrawal` is `null` unless withdrawn. `submission` (added with the submission audit, administrators only) is the revision's submission audit row: `submittedBy` is always the author, `submittedAt` equals the revision's `submittedAt`, and `backfilled` is `true` for revisions submitted before migration 0011, whose row was reconstructed from the revision's author and `submittedAt` rather than recorded at submission ([DECISIONS.md](DECISIONS.md#submission-audit)). Submissions are not part of `history`, which stays the pointer audit. `history[].reason` is set for unpublication only.

`GET /api/admin/lessons`: lessons with at least one approved revision (those withdrawal, unpublication and restoration act on), most recent approval or pointer change first, at most 200: `{"lessons": [{"lessonId", "publicId", "subjectCode", "language", "archived", "publishedRevisionId", "title", "pendingRevisionId", "lastChange"}]}`. `title` is the published revision's, else the latest approved one's; `pendingRevisionId` is the approved candidate awaiting publication, or `null`.

Display names of authors, reviewers and administrators are returned to staff only (reviewers through `authorName` in the review queue, publishers and administrators here); readers already see author and approving-reviewer credits.

## Curriculum (LMS-B02)

Levels, academic sections, curriculum editions and their subject assignments with ordered official chapters ([LMS_CONTRACT.md §3](LMS_CONTRACT.md#3-curriculum-b02-migration-0013), data model in [DECISIONS.md](DECISIONS.md#curriculum-lms-b02)). Every endpoint needs a session (`401 unauthenticated`), runs the permission check before any lookup and answers `Cache-Control: private, no-store`. Unsafe methods need the CSRF token and a same-origin request. Request bodies follow the [strict JSON rules](#conventions); every field below is required unless marked optional, and `null` is refused except for `expectedCurrentEditionId`.

| Endpoint | Who may call it |
| --- | --- |
| `GET /api/catalogue/levels`, `GET /api/catalogue/sections/{sectionId}/editions`, `GET /api/catalogue/editions/{editionId}` | Signed-in staff: `author`, `publisher`, `administrator`, or a reviewer of any subject. Students: `403 forbidden` |
| `POST`/`PUT`/`DELETE /api/admin/curriculum/…` | `administrator`, checked by the middleware (`403 forbidden` before any lookup) and again inside the write's transaction, which also writes its `curriculum_audit` row |

Codes: 1 to 63 lower-case ASCII letters, digits and hyphens, not starting with a hyphen; unique per scope (level codes globally, section codes per level, edition codes per section) and never reused or changed. Names and labels (`nameFr`, `nameAr`, `labelFr`, `labelAr`) follow the catalogue display-name rule: surrounding white space is trimmed, then 1 to 200 characters, no control, invisible-format or line/paragraph separator characters; Arabic text is allowed. `displayOrder`: 0 to 9999 (optional on creation, default 0). Lists sort by `displayOrder`, then code in byte order, whatever the database locale. Levels, sections and editions carry a `version`; every write names it as `expectedVersion` (`409 version_conflict` otherwise, nothing written) and increments it. Assignment and chapter-list edits increment the edition's version.

### Objects

Level: `{"id", "code", "nameFr", "nameAr", "displayOrder", "retiredAt", "version", "createdAt", "sections": [section…]}`. Section: `{"id", "levelId", "code", "nameFr", "nameAr", "displayOrder", "retiredAt", "version", "createdAt"}`. Edition:

```json
{"id": 7, "sectionId": 3, "code": "test-2026", "labelFr": "TEST 2026", "labelAr": "اختبار", "state": "current",
 "currentSince": "2026-11-26T10:00:00Z", "supersededAt": null, "retiredAt": null, "version": 4, "createdAt": "2026-11-26T09:00:00Z",
 "assignments": [{"id": 11, "subjectId": 1, "subjectCode": "test-math", "selectable": true,
                  "chapters": [{"chapterId": 4, "code": "ch-02", "position": 1, "retired": false}]}]}
```

`state` is `draft`, `current`, `superseded` or `retired`. `selectable` is `curriculum_assignment_selectable`: the edition is current and neither its section, its level nor the subject is retired (what course placement will be allowed to newly select). `position` orders the edition's chapters independently of the catalogue display order. Lists are always arrays.

### Reads

- `GET /api/catalogue/levels`: `{"levels": [level…]}`, retired entries included (`retiredAt` set). More than 1000 levels or sections is a `500 internal` error rather than a truncated list (the curriculum is far smaller).
- `GET /api/catalogue/sections/{sectionId}/editions`: `{"section": section, "editions": [edition without "assignments"…]}`, newest first. `404` for an unknown section.
- `GET /api/catalogue/editions/{editionId}`: the edition with its assignments and chapter lists. `404` for an unknown edition.

### Administration (`/api/admin/curriculum`)

| Request | Body | Success |
| --- | --- | --- |
| `POST /levels` | `code`, `nameFr`, `nameAr`, `displayOrder` (optional) | `201` level |
| `PUT /levels/{levelId}` | `expectedVersion`, `nameFr`, `nameAr`, `displayOrder` | `200` level (`sections` empty); an unchanged request writes nothing |
| `POST /levels/{levelId}/retire` | `expectedVersion` | `200` level |
| `POST /levels/{levelId}/sections` | as for a level | `201` section; `409 parent_retired` under a retired level |
| `PUT /sections/{sectionId}`, `POST /sections/{sectionId}/retire` | as for a level | `200` section |
| `POST /sections/{sectionId}/editions` | `code`, `labelFr`, `labelAr` | `201` draft edition; `409 parent_retired` |
| `PUT /editions/{editionId}` | `expectedVersion`, `labelFr`, `labelAr` | `200` edition (labels are display metadata, changeable in any state) |
| `POST /editions/{editionId}/assignments` | `expectedVersion`, `subjectId` | `201` edition; `409 edition_not_draft`, `409 assignment_exists`, `422 unknown_subject`, `422 placement_invalid` (retired subject) |
| `DELETE /editions/{editionId}/assignments/{assignmentId}?expectedVersion=N` | none (a body is `400 invalid_request`) | `200` edition (the chapter list goes with it); `409 edition_not_draft` |
| `PUT /editions/{editionId}/assignments/{assignmentId}/chapters` | `expectedVersion`, `chapterIds` (ordered, at most 500, may be empty) | `200` edition; replaces the list; `409 edition_not_draft`, `422 placement_invalid` |
| `POST /editions/{editionId}/make-current` | `expectedVersion`, `expectedCurrentEditionId` (the section's current edition ID, or `null` if it has none) | `200` `{"edition": edition, "supersededEditionId": id or null}`; the previous current edition becomes `superseded` in the same transaction. `409 edition_not_draft`, `409 state_conflict` (the current edition is not the expected one; of two racing requests one wins), `409 parent_retired`, `422 placement_invalid` (the edition assigns a retired subject or chapter; the message names them) |
| `POST /editions/{editionId}/retire` | `expectedVersion` | `200` edition; only `current` or `superseded` editions (`409 state_conflict` for a draft, `409 already_retired`). Retiring the current edition leaves the section without one |
| `POST /subjects/{subjectId}/retire`, `POST /chapters/{chapterId}/retire` | `{}` | `204`; `409 already_retired`. The entry stays readable and keeps every reference; it can no longer be newly assigned or listed |

Retirement is never undone. Nothing deletes a level, section, edition, subject or chapter. A `409 retry` may answer any write that collided with a concurrent one.

## Course drafts (LMS-B03)

Teacher-owned courses and their draft revisions ([LMS_CONTRACT.md §4, §5 and §8](LMS_CONTRACT.md#4-courses-b03-migration-0014), data model in [DECISIONS.md](DECISIONS.md#courses-lms-b03)), in `internal/courses` and `internal/studioapi/courses.go`. Submission, review, publication and the rest of the course lifecycle are [LMS-B04](#course-review-and-publication-lms-b04). Every endpoint needs a session (`401 unauthenticated`) and the `author` permission, checked by the middleware before any lookup (`403 forbidden`) and again inside the transaction; unsafe methods need the CSRF token and a same-origin request. Responses are `Cache-Control: private, no-store`. A course belongs to its owner (its creator; administrators get no exception): another user's course or revision, and another author's lesson that is not currently published, answer `404 not_found` exactly like an ID that does not exist. Request bodies follow the [strict JSON rules](#conventions) at every level, including each module and step object. Only the endpoints that list query parameters accept any (`400 invalid_request` otherwise).

| Endpoint | Body | Success and errors |
| --- | --- | --- |
| `GET /api/courses?mine=1&cursor=` | none (`mine=1` is required; `cursor` optional; nothing else) | `200 {"courses": [course…], "nextCursor": "…" or null}`, newest first (by course ID), 100 per page; pass `nextCursor` back as `cursor` |
| `POST /api/courses` | `subjectId`, `language` (`fr` or `ar`) | `201 {"course": course, "revision": revision}` with an empty first draft; `Location` names the revision. `422 unknown_subject`, `422 placement_invalid` (retired subject), `422 invalid_input` (language) |
| `DELETE /api/courses/{courseId}` | none | `204`: deletes the course with its draft revisions, placements, modules and steps, never lessons or media. `409 has_history` once any revision was submitted |
| `POST /api/courses/{courseId}/drafts` | `{}` | `201` revision: a new draft copying the course's latest revision (overview, cover if the caller uploaded it, placements, modules and the steps still attachable) or empty without one, with `droppedSteps`: `[{"moduleKey", "position", "title", "lessonId", "revisionId"}…]`, the copied revision's steps left out because another author's lesson is no longer attachable at its pinned revision (empty when none). Base is the published revision; the comparison revision is the published one, or the last published or restored one while the course is suspended or archived (LMS-B04). `409 active_candidate_exists`, `409 archived`. Placements are copied unchecked: the next save reports one that is no longer allowed |
| `GET /api/course-revisions/{revisionId}` | none | `200` revision |
| `PUT /api/course-revisions/{revisionId}` | the whole draft, below | `200` revision with `version` + 1. `409 version_conflict` (nothing written), `409 not_draft`, `422 invalid_input`, `422 invalid_step`, `422 placement_invalid`, `422 unknown_media` |
| `DELETE /api/course-revisions/{revisionId}?expectedVersion=N` | none | `204`: discards the draft and its children (its number is never reused). `409 version_conflict`, `409 not_draft` |
| `GET /api/courses/{courseId}/attachable-lessons?q=&cursor=` | none | `200 {"lessons": [{"lessonId", "publicId", "own", "title", "publishedRevisionId", "latestRevision"}…], "nextCursor": "…" or null}`: lessons of the course's subject and language that are the caller's own (title and `latestRevision` of the latest revision) or another author's currently published, unarchived lessons (title of the published revision, `latestRevision` null). `q` (optional, at most 100 characters) matches titles ignoring case and diacritics; pages of 50 by lesson ID; pass `nextCursor` back as `cursor`. Other parameters `400` |
| `GET /api/lessons/{lessonId}/eligible-revisions` | none | `200 {"revisions": [{"id", "number", "status", "title", "eligible"}…]}`, newest first: every revision of the caller's own lesson (`eligible` says which may be submitted with the course), or only the eligible revisions of another author's currently published lesson (`404` otherwise) |

Save body (every key required except `coverMediaId` and `coverAlt`, which may be omitted or `null`):

```json
{"expectedVersion": 3, "title": "TEST course", "summary": "", "objectives": "", "prerequisites": "",
 "coverMediaId": 12, "coverAlt": "TEST description of the cover",
 "assignmentIds": [11],
 "modules": [{"key": null, "title": "TEST module", "chapterId": 4,
              "steps": [{"lessonId": 7, "revisionId": 21, "title": "TEST step"}]}]}
```

- The save replaces everything; order in the arrays is the order of modules and steps. A module `key` is a lower-case UUID returned by an earlier read (it keeps the module's identity across revisions) or `null` for a new module.
- Text fields are plain text, trimmed, without control characters (line feeds only in `summary`, `objectives`, `prerequisites`), bidirectional formatting or line/paragraph separator characters: `title` ≤ 200, `summary` ≤ 1000, `objectives` ≤ 2000, `prerequisites` ≤ 2000, `coverAlt` ≤ 300, module and step titles 1 to 200. Drafts may be incomplete (empty overview fields, no modules). At most 50 modules and 200 steps.
- `coverMediaId` must be an image the caller uploaded (`422 unknown_media` otherwise) and needs `coverAlt`; `coverAlt` without a cover is `422 invalid_input`.
- `assignmentIds`: curriculum assignments of the course's subject. Each one not on the draft's comparison revision must be selectable now (current edition, section, level and subject not retired); a module `chapterId` must be in at least one of them and not retired, unless inherited unchanged from the comparison revision (`422 placement_invalid`).
- Steps: a lesson of the course's subject and language, at most once per revision. The caller's own lesson may pin any of its revisions, drafts included; another author's lesson only an eligible revision (approved, not withdrawn, published at least once, lesson currently published and not archived) (`422 invalid_step`).

Revision: `{"id", "courseId", "number", "status", "kind", "authorId", "baseRevisionId", "compareRevisionId", "subjectId", "language", "title", "summary", "objectives", "prerequisites", "coverMediaId", "coverAlt", "version", "createdAt", "updatedAt", "submittedAt", "withdrawnAt", "review", "blocked", "assignments": [{"assignmentId", "editionId", "editionState", "selectable", "retired"}…], "modules": [{"key", "position", "title", "chapterId", "steps": [{"position", "title", "lessonId", "revisionId", "revisionNumber", "ownLesson", "eligible"}…]}…]}`. `retired` flags a placement whose edition, section or level is retired (refused at submission). LMS-B04 adds `withdrawnAt`, `review` and `blocked` ([below](#course-review-and-publication-lms-b04)). A step shows only its course-authored title, never the lesson revision's. Course: `{"id", "publicId", "ownerId", "subjectId", "language", "publishedRevisionId", "archived", "createdAt", "latestRevision"}` (`latestRevision` only in the list: `{"id", "number", "status", "title", "version", "updatedAt"}`, omitted when the course has no revision). A `409 retry` may answer any write that collided with a concurrent one.

## Course review and publication (LMS-B04)

Submission, independent review, amendments, publication, suspension, archive, withdrawal and transfer of courses ([LMS_CONTRACT.md §5–§8](LMS_CONTRACT.md#5-eligibility-availability-and-comparison-one-definition-each), decisions in [DECISIONS.md](DECISIONS.md#course-review-and-publication-lms-b04)), in `internal/courses` (`workflow.go`, `review.go`, `publication.go`) and `internal/studioapi/course_review.go`. Every endpoint needs a session (`401 unauthenticated`) and a role checked by the middleware before any lookup (`403 forbidden`) and again inside the transaction; unsafe methods need the CSRF token and a same-origin request. Responses are `Cache-Control: private, no-store`. Bodies follow the [strict JSON rules](#conventions); these endpoints take no query parameters except where listed. `reason` and `comment` are trimmed; a reason is required (`422 reason_required`) and at most 2000 characters (`422 invalid_reason`). A `409 retry` may answer any write that collided with a concurrent one.

| Endpoint | Who | Body | Success and errors |
| --- | --- | --- | --- |
| `POST /api/course-revisions/{revisionId}/submit` | owner (`author`) | `{"expectedVersion"}` | `200` revision in review, `version` + 1, content unchanged; a `course_submission_audit` row is written in the same transaction. `422 incomplete` with `"fields"` (`title`, `summary`, `objectives`, `modules`, or `module:<key>` for a module without a step), `409 ineligible` with `"steps"`, `422 placement_invalid` (submit mode, contract §3), `409 version_conflict`, `409 not_draft`, `409 archived`, `404` |
| `GET /api/courses/{courseId}/newer-revisions` | owner, reviewers of the subject, publishers, administrators | none | `200 {"courseId", "comparisonRevisionId", "steps": [{"moduleKey", "position", "title", "lessonId", "pinnedRevisionId", "pinnedNumber", "newerRevisionId", "newerNumber"}…]}`: steps of the comparison revision whose lesson has a newer eligible revision (the newest one). Students get `403`; any other author, and a missing course, `404` |
| `POST /api/courses/{courseId}/amendments` | owner | `{"steps": [{"lessonId", "revisionId"}…]}` (`[]` takes every newer eligible revision of the notice) | `201` revision of kind `amendment`: a draft copying the comparison revision with only the chosen steps replaced. `409 draft_exists` (a draft is open), `409 active_candidate_exists` (a revision in review or approved), `409 not_published` (never published), `409 no_newer_revisions`, `409 archived`, `409 not_amendable`, `422 invalid_step` (not a step's lesson or not a newer eligible revision of it; or an unchanged step of another author's lesson that is no longer eligible: replace it too or start an ordinary revision) |
| `GET /api/review/queue?type=course&subjectId=` | reviewer of any subject | none | `200 {"revisions": [{"revisionId", "courseId", "number", "kind", "title", "language", "subjectId", "authorId", "authorName", "submittedAt", "eligible", "blocked"}…]}`: course revisions in review (not withdrawn) in the caller's subjects, oldest first, at most 200, including those the caller may not decide (`eligible: false`). `type=lesson` or no `type` is the lesson queue. `subjectId` of a subject the caller does not review: `404` |
| `GET /api/review/course-revisions/{revisionId}` | reviewer of the subject (revisions in review), publisher or administrator (any submitted revision) | none | `200 {"revision", "eligible", "problems", "metadataChanged", "steps", "removedSteps"}`. `steps`: every step in outline order with `change` `unchanged`, `moved` (same pinned revision, another module, position or title), `replaced` (`previousRevisionId`, `previousRevisionNumber`, `unavailable`, `previousUnavailable` and `diff`; a side that is not readable now, a withdrawn revision or a lesson unpublished or archived, is marked `unavailable` or `previousUnavailable` and `diff` is then `null`, so no content of either side is sent) or `added`, and for a lesson the comparison revision has, `previousModuleKey`, `previousPosition`, `previousTitle` and `structureChanged`; `removedSteps`: steps of the comparison revision whose lesson is gone; `metadataChanged`: overview, cover, placement, modules or step structure differ from the comparison revision (always true without one). `diff`: `{"fields": [{"name", "old", "new"}…], "blocks": [{"op": "equal"/"removed"/"added", "block"}…]}` over the two lesson documents. `404` for anyone else and for drafts |
| `POST /api/review/course-revisions/{revisionId}/approve`, `…/request-changes` | reviewer | `{"comment"}` (optional to approve, required to request changes) | `200` revision. `403 not_eligible_reviewer` with `"reasons": [{"reason", "lessonId", "lessonRevisionId"}…]` (`course_author`, `lesson_author`), `409 not_in_review`, `409 withdrawn`, `422 comment_required`, `422 invalid_comment`; a reviewer of another subject `404` |
| `GET /api/publish/queue?type=course` | publisher | none | `200 {"revisions": [{"revisionId", "courseId", "coursePublicId", "number", "kind", "title", "language", "subjectId", "authorId", "authorName", "approvedBy", "approvedAt", "baseRevisionId", "publishedRevisionId", "staleBase", "archived", "ineligibleSteps"}…]}`: approved, never published, not withdrawn, oldest approval first. No `type` (or `type=lesson`) is the lesson queue |
| `POST /api/course-revisions/{revisionId}/publish` | publisher | `{"expectedPublishedRevisionId"}` (ID or `null`) | `200` pointer change. `409 stale_pointer` (not the expected pointer), `409 stale_base` (drafted against another pointer, or the course's comparison revision changed since the draft was created), `409 ineligible` with `"steps"`, `422 placement_invalid` (publish mode), `409 not_approved`, `409 already_published`, `409 withdrawn`, `409 archived`; on any failure nothing changes |
| `POST /api/courses/{courseId}/restore` | publisher or administrator | `{"revisionId", "expectedPublishedRevisionId"}` | `200` pointer change: a previously published, non-withdrawn revision whose steps are all eligible now (lifting a suspension is a restore). `409 ineligible`, `409 never_published`, `409 already_current`, `409 withdrawn`, `409 archived`, `409 stale_pointer` |
| `POST /api/courses/{courseId}/suspend` | administrator | `{"reason", "expectedPublishedRevisionId"}` | `200` pointer change to `null`; the course leaves reading at once. `409 stale_pointer`, `409 not_published` |
| `POST /api/courses/{courseId}/archive` | administrator | `{"reason", "expectedPublishedRevisionId"}` | `200` pointer change (`action` `archive`; `publishedRevisionId` null): sets the archive and clears the pointer (a `course_publication_audit` row), or for a course with no pointer only a `course_lifecycle_audit` row. `409 archived`, `409 stale_pointer` |
| `POST /api/courses/{courseId}/unarchive` | administrator | `{"reason"}` | `200` lifecycle event. Never republishes: use restore or publish. `409 not_archived` |
| `POST /api/courses/{courseId}/transfer` | administrator | `{"toUserId", "reason"}` | `200` lifecycle event (`fromOwnerId`, `toOwnerId`). `409 active_candidate_exists` (discard or withdraw first), `422 unknown_user` (not an active author), `422 invalid_input` (already the owner) |
| `POST /api/course-revisions/{revisionId}/withdraw` | administrator | `{"reason"}` | `200 {"courseId", "revisionId", "withdrawnBy", "reason", "withdrawnAt"}`: a revision in review or approved; frees the course's candidate slot and blocks later decisions, publication and restoration of it. `409 published` (suspend instead), `409 withdrawn`, `409 not_withdrawable` |
| `DELETE /api/admin/course-revisions/{revisionId}` | administrator | `{"reason"}` | `200` lifecycle event (`discard_draft`): removes an open draft of any course. `409 not_draft` |
| `POST /api/courses/{courseId}/archive-request` | owner | `{"reason"}` | `201` lifecycle event. `409 archived`, `409 never_submitted` (delete the course instead) |
| `GET /api/admin/courses/archive-requests` | administrator | none | `200 {"requests": [{lifecycle event…, "archived", "publishedRevisionId", "title"}…]}`, newest first, at most 200 |
| `GET /api/admin/courses/blocked-reviews?cursor=` | administrator | none | `200 {"revisions": [queue item…], "nextCursor": "…" or null}`: course revisions in review that no active reviewer of their subject may decide, by revision ID, 200 per page; pass `nextCursor` back as `cursor` |

- Pointer change: `{"courseId", "action", "publishedRevisionId", "previousRevisionId", "actorId", "reason", "occurredAt"}` (`action`: `publish`, `restore`, `suspend`, `archive`). Lifecycle event: `{"id", "courseId", "actorId", "action", "reason", "revisionId", "fromOwnerId", "toOwnerId", "occurredAt"}` (`action`: `archive_request`, `archive`, `unarchive`, `discard_draft`, `transfer`).
- The course revision object (above) gains `withdrawnAt`, `review` (the decision: `{"action", "reviewerId", "comment", "decidedAt"}`, `null` until decided; the owner sees the change-request comment) and `blocked`: for a revision in review that no active reviewer of its subject may decide, `{"reason": "no_eligible_reviewer", "details": [{"reviewerId", "reviewerName", "reason", "lessonId", "lessonRevisionId"}…]}` (`reason` per reviewer `course_author` or `lesson_author`, or a single `no_subject_reviewer` when the subject has no active reviewer); `null` otherwise. `kind` is derived by the server on every save and checked at submission.
- `409 ineligible` bodies carry `"steps": [{"moduleKey", "position", "title", "lessonId", "revisionId"}…]` next to `code` and `message`; `422 incomplete` carries `"fields"`; `403 not_eligible_reviewer` carries `"reasons"`.
- Reader views (used by B08): `reader_published_courses` (readable courses: pointer set, not archived, published revision approved and not withdrawn) and `reader_course_steps` (every step of a readable course with `readable`; `lesson_title` and `content` are `NULL` for an unreadable step).

## Course reading (LMS-B08)

Server-rendered pages in `internal/reader` (`courses.go`, `course_http.go`, `home.go`) over `migrations/0017_course_reading.sql` and the views of 0015. Decisions: [DECISIONS.md](DECISIONS.md#course-reading-lms-b08). The [reader conventions](#headers-and-access) apply: a session is required (anonymous `GET` redirects to `/login?next=…`, which accepts these paths), every response is `private, no-store` with `Vary: Cookie`, forms need `csrf_token`.

| Route | Session | Result |
| --- | --- | --- |
| `GET /learn` | required | Student home: the optional level/section preference, subject shortcuts (with a preference, subjects assigned in a current edition of that section or level), the lesson bookmarks and course bookmarks (unreadable ones listed as unavailable), available courses (readable courses, newest publication first, at most 24; with a preference, those placed in a current edition of that section or level) and recently published lessons and courses (at most 10) |
| `GET /learn/preferences` · `POST /learn/preferences` | required + `csrf_token` | Optional preference. Field `choice`: `level:{levelId}` or `section:{levelId}:{sectionId}` (non-retired, section in the level); empty `choice` or `skip` clears it. `303` to `/learn`; `422` page for anything else. Nothing else is asked |
| `GET /learn/courses/{publicId}` | required | A readable course (`reader_published_courses`): title, summary, objectives, prerequisites, owner and approving course reviewer, placements (earlier editions labelled), bookmark button and the outline: modules and steps with their course-authored titles; an unreadable step stays in place marked "temporairement indisponible", and the page says the course is incomplete |
| `GET /learn/courses/{publicId}/lessons/{lessonPublicId}` | required | The step pinning that lesson. Readable: the pinned revision through the canonical renderer, lesson credit, a note with a link to `/learn/lessons/{lessonPublicId}` when the standalone published revision differs, and the report form. Unreadable: only the course-authored title and the unavailable notice (no lesson title, body, objective, media or report form). Both: breadcrumbs (course, module, step), "Leçon n sur N", previous/next to the neighbouring steps whether readable or not (marked "(indisponible)"), and the outline, after the lesson on phones and in a sticky side column from 64rem |
| `GET /learn/courses/{publicId}/lessons/{lessonPublicId}/media/{mediaId}` | required | An uploaded image embedded in the step's pinned revision (the embed check of the lesson media route: in its frozen `mediaIds` and in its content as `media:{mediaId}`), only while the course and the step are readable. Same headers as the lesson media route; everything else gets the JSON `404 not_found` |
| `POST /learn/courses/{publicId}/bookmark` · `/unbookmark` | required + `csrf_token` | Bookmarks a readable course (idempotent; `404` page otherwise; `409` page at 500) or removes a bookmark, readable or not. `303` to the course, or to `/learn` with `return=dashboard` |
| `POST /learn/courses/{publicId}/lessons/{lessonPublicId}/report` | required + `csrf_token` | Course-context [error report](#error-reports-lrn-015). Fields `message`, `revision` (the pinned lesson revision's number) and `course_revision` (the published course revision's number), each exactly once. Same text rules, throttles, `413` and `429` as the lesson form. `303` to `…/reported`; `409` step page (text kept) when either number is no longer the displayed one; `404` when the course or the step is no longer readable; `400` for missing or malformed fields |
| `GET /learn/courses/{publicId}/lessons/{lessonPublicId}/reported` | required | Confirmation with a link back to the step; no course or lesson data |
| `GET /learn/search?q=…` | required | Also lists readable courses whose title, summary, objectives, prerequisites or the title (course-authored or lesson) of a **readable** step matches, with the same folding and limits as the title search |

- A course that is unknown, never published, suspended, archived or whose published revision is withdrawn, a malformed ID and a lesson that is not a step of the published revision all get the same `404` page ("Cours indisponible") with a link back; bookmarks survive and show the course as unavailable.
- Each page reads the outline, the readable flags and the step's content in one statement, so it never mixes snapshots. No other revision is ever substituted for an unreadable step.
- Database: `course_bookmarks (user_id, course_id, created_at)`, `student_preferences (user_id, level_id, section_id, updated_at)` (deleted with the account) and `error_reports.course_revision_id`, with a composite foreign key to the step `(course_revision_id, lesson_id, revision_id)` and the replaced insert guard: a report with a course revision must name the pinned revision of a readable step of the course's readable published revision (lesson, then course, then account share-locked; READ COMMITTED only), and one without must name the lesson's current published revision, as before.

## Not yet defined

Public registration, password recovery, email verification, account deletion (LRN-022); web permission administration (LRN-019 console); administration pages for withdrawal, unpublication and restoration and a listing of the pointer audit and withdrawals (LRN-019); reader access to uploaded media (schema v1 cannot reference uploads yet; the Studio attaches them as `mediaIds`); the pilot catalogue seed (LRN-014, waiting for teacher-confirmed names); publisher and administrator access to uploaded media of approved revisions (the publisher view lists their number only); reviewer dashboards and notifications (M2); error report administration pages, notifications to reporters and reviewers, and a reporter's view of their own reports (LRN-019/LRN-020).
