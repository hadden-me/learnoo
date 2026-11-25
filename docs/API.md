# Server API

Contract home for Studio ↔ server and reader ↔ server endpoints ([CAHIER_DES_CHARGES.md §5.4](CAHIER_DES_CHARGES.md#54-shared-contracts-agree-first-then-build-in-parallel)). Changes used by another open task are agreed on its issue first.

Served by `cmd/server` (not `cmd/renderer`). Status: **LRN-005 foundation, the LRN-006 Studio API and LRN-007 review and publication.** Authentication endpoints, the server-side permission check every protected endpoint uses, the [Studio endpoints](#studio-lesson-drafts-and-media-lrn-006) for lesson drafts and media, and the [review and publication endpoints](#review-and-publication-lrn-007). Reader endpoints do not exist yet.

## Conventions

- JSON bodies, UTF-8. Request bodies must use `Content-Type: application/json` (the only exception is the raw image body of `POST /api/media`). Studio endpoints accept exactly one JSON object whose top-level keys are spelled exactly as documented (case-sensitive) and appear once; unknown, case-variant or duplicate keys, values of the wrong type, anything after the object and malformed JSON are rejected with `400 invalid_request`. `null` is accepted only where a field is documented as accepting it (such as `chapterId` and `translationOf`); it is `400` for every other field, including `content`, `prerequisites`, `mediaIds` (and its elements), `subjectId`, `language` and `expectedVersion`. Integer fields accept only integer literals that fit (`1.0`, `1e0` and `"1"` are `400`). Error messages name the JSON key, never an implementation type. The login endpoint applies the same key rules (see below).
- Every response carries `Cache-Control: no-store` (media responses: `private, no-store`) and `Vary: Cookie`; authenticated responses must never be stored by shared caches.
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
| 404 | `not_found` | Unknown route; a lesson, revision or media ID that does not exist, is malformed, or (revisions) belongs to another author |
| 408 | `upload_timeout` | The upload body did not arrive within the server's read timeout. No `Retry-After`: retry on a better connection or with a smaller image |
| 409 | `version_conflict` | `expectedVersion` is stale: the draft changed since it was loaded. Nothing was written |
| 409 | `not_draft` | The revision was submitted and can no longer change, be submitted again or be discarded |
| 409 | `active_candidate_exists` | The lesson already has an active candidate: a draft, a revision in review, or an approved revision not yet published (or withdrawn) |
| 409 | `archived` | The lesson is archived |
| 409 | `translation_exists` | The translation group already has a lesson in that language |
| 409 | `retry` | The database aborted the transaction to resolve a deadlock or serialization conflict with a concurrent request. Nothing was written; retry the same request (reload first if it carries `expectedVersion` and it fails again with `version_conflict`) |
| 409 | `immutable` | A database guard rejected a change to a frozen snapshot or asset (normally preceded by a more specific code) |
| 409 | `not_in_review` | The revision is not in review (a draft, or already decided, for example by a concurrent reviewer). Nothing was written |
| 409 | `not_approved` | Publication of a revision that is not approved |
| 409 | `already_published` | Publication of a revision that is or was already published; re-pointing to an earlier revision is restoration (LRN-009) |
| 409 | `stale_base` | The lesson's published revision changed since the revision was drafted. Nothing was published |
| 413 | `too_large` | JSON body over 256 KiB, or image over 5 MiB |
| 415 | `unsupported_media_type` | Request body is not `application/json`; upload is not a PNG, JPEG or WebP image (SVG, GIF and everything else are refused) |
| 415 | `type_mismatch` | Uploaded bytes do not match the declared image `Content-Type` |
| 422 | `invalid_content` | The document fails schema v1 validation, or a field limit (prerequisites, number of media) is exceeded. `message` carries the validator's reason |
| 422 | `language_mismatch` | The document's `language` differs from the lesson's |
| 422 | `unknown_subject` / `unknown_chapter` / `unknown_media` | Referenced subject, chapter (of this subject) or media asset does not exist |
| 422 | `unknown_lesson` | `translationOf` names a lesson that does not exist or that the caller does not own |
| 422 | `translation_subject` | A translation names a subject other than its source lesson's |
| 422 | `invalid_image` | The image is corrupt or truncated |
| 422 | `invalid_dimensions` | Over 4096 px per side or 16 megapixels |
| 422 | `trailing_data` | Data after the end of the image (polyglots, many phone photos); re-export the image or upload PNG |
| 422 | `comment_required` | A change request without a comment (missing, null or only whitespace) |
| 422 | `invalid_comment` | A review comment over 5000 characters or containing a NUL character |
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

Throttling (sliding 15-minute window, counted before the password is checked, enforced for existing and non-existent accounts alike): more than 5 attempts for one email address, or more than 30 from one client address, return `429` even with the correct password. A successful login clears that email's counter. The client address is the TCP peer; proxy headers are not trusted until deployment defines the proxy chain.

### `POST /api/auth/logout`

Requires a session and `X-CSRF-Token`. Deletes the session server-side and clears the cookie.

- `204` no body.
- `401 unauthenticated`, `403 csrf_rejected`, `403 cross_origin_rejected`.

### `GET /api/auth/session`

- `200` session object for the current cookie (also refreshes the idle expiry).
- `401 unauthenticated`. An invalid cookie is also cleared.

### `GET /healthz`

`200 {"status":"ok"}` when the database answers; `503 unavailable` otherwise. Not authenticated; reveals nothing else.

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
| `GET /api/media/{mediaId}` | An `author` who uploaded that asset (also when the upload was deduplicated) or who authored a revision referencing it. Everyone else with `author` gets `404 not_found`, identical to a missing ID. Reviewers use [`GET /api/review/media/{mediaId}`](#get-apireviewmediamediaid); student and reader access comes with LRN-008 |

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
- IDs are numeric database IDs. `subjectId` and `chapterId` refer to the catalogue tables; a catalogue endpoint arrives with LRN-014.

### Draft fields

Lesson writes share these request fields:

| Field | Type | Notes |
| --- | --- | --- |
| `content` | object | Required. Schema v1 document, at most 128 KiB in compact form, `status` `unreviewed`. Validated with the reader's validator before anything is stored |
| `prerequisites` | string | Optional, at most 2000 characters |
| `chapterId` | integer or null | Optional; must be a chapter of the lesson's subject. Omitted or null clears it |
| `mediaIds` | integer array | Optional, at most 100 distinct uploaded asset IDs (duplicates are merged). Only assets the caller may read (see `GET /api/media/{mediaId}`) may be referenced; others get `422 unknown_media`, exactly like missing IDs. The list replaces the previous one |

Bodies are limited to 256 KiB (`413 too_large`).

### `POST /api/lessons`

Creates a lesson and its first draft.

```json
{"subjectId": 1, "language": "fr", "translationOf": null, "content": {…}, "prerequisites": "", "chapterId": null, "mediaIds": []}
```

- `subjectId` and `language` are required; `language` must equal `content.language` and is fixed for the lesson, as is the subject.
- `translationOf` (optional) names an existing lesson of the same subject, which the caller must own; the new lesson joins its translation group. A lesson the caller does not own is reported like a missing one (`422 unknown_lesson`).
- `201` revision object, `Location: /api/revisions/{id}`.
- `400`, `403`, `413`, `415`, `422 invalid_content | language_mismatch | unknown_subject | unknown_chapter | unknown_media | unknown_lesson | translation_subject`, `409 archived | translation_exists`.

### `POST /api/lessons/{lessonId}/drafts`

Starts a new draft of an existing lesson (for example the successor of a `changes_requested` revision). Only the lesson's owner may. Body: the draft fields. Its base is the lesson's published revision (`baseRevisionId`), read under a lock.

- `201` revision object, `Location` as above.
- `404 not_found` (unknown lesson, or a lesson the caller does not own; same body), `409 active_candidate_exists` (a draft, an in-review revision, or an approved revision awaiting publication exists; concurrent requests: exactly one succeeds), `409 archived`, `422` as above.

An approved revision keeps blocking new drafts until it is [published](#post-apirevisionsrevisionidpublish) or withdrawn (LRN-009, not yet available).

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

Body `{"expectedVersion": 3}`. Moves the caller's own draft to `in_review`. From then on its content, metadata, exercises and media references are frozen by the database; corrections need a new draft.

- `200` revision object (`status` `in_review`, `submittedAt` set, `version` incremented).
- `409 version_conflict | not_draft | archived`, `404 not_found`, `422 invalid_content` (stored snapshot no longer validates).

### `DELETE /api/revisions/{revisionId}?expectedVersion=3`

Discards the caller's own draft and its media references (the assets remain). `expectedVersion` is the only query parameter, a plain decimal integer.

- `204` no body.
- `400 invalid_request` (missing or malformed `expectedVersion`), `409 version_conflict | not_draft`, `404 not_found`.

### `POST /api/media`

Uploads one image as the **raw request body** (no multipart) with `Content-Type: image/png`, `image/jpeg` or `image/webp`, plus `X-CSRF-Token`.

The type is detected from the bytes; the declared type must agree. The image is decoded and re-encoded (metadata removed; WebP stored as PNG). Limits: 5 MiB, 4096 px per side, 16 megapixels. A larger declared `Content-Length` is refused before the body is read; a longer body is cut off at the limit.

```json
{"id": 5, "contentType": "image/png", "byteSize": 1234, "width": 800, "height": 600, "sha256": "…64 hex…", "url": "/api/media/5"}
```

- `201` (`Location` = `url`), whether the asset is new or identical canonical bytes were already uploaded (by anyone), in which case the existing asset ID is returned and the caller gains read access to it. Media IDs are sequential, so deduplication is observable only through the returned ID being older than expected. Assets never change; replacing an image means uploading a new one and referencing its ID.
- `400 invalid_request` (empty or interrupted upload), `413 too_large`, `415 unsupported_media_type | type_mismatch`, `422 invalid_image | invalid_dimensions | trailing_data`, `408 upload_timeout`, `503 busy` (more than 4 uploads in progress for 5 seconds; `Retry-After: 5`), `409 retry`.
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
{"revisions": [{"revisionId": 12, "lessonId": 4, "number": 2, "title": "Nombres complexes", "language": "fr", "subjectId": 1, "chapterId": null, "authorId": 7, "submittedAt": "2026-11-24T09:00:00Z"}]}
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
- `409 stale_base`: the published revision changed since this revision was drafted (for example after an unpublication or restoration, LRN-009). Nothing changes; a reconciled revision must be drafted and reviewed. While this revision stays approved it keeps blocking new drafts (open question in DECISIONS.md).
- `409 not_approved | already_published | archived`, `404 not_found` (missing revision), `403 forbidden`, `400`, `415`. Of concurrent publications of one revision exactly one succeeds and is audited once; the others get `409 already_published`.

## Not yet defined

Registration, password recovery, email verification, account deletion (LRN-022); web permission administration (LRN-019 console); unpublication, withdrawal and restoration (LRN-009); reader pages and reader media access (LRN-008); catalogue endpoints (LRN-014); lesson listing for the Studio; a publisher's list of approved revisions awaiting publication; reviewer dashboards and notifications (M2).
