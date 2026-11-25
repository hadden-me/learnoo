// Typed client for the application server's JSON API (docs/API.md). Same
// origin only: the session cookie is HttpOnly and never read here; the CSRF
// token comes from GET /api/auth/session and goes in X-CSRF-Token on every
// unsafe request. Errors are ApiError values carrying the server's `code`,
// which callers branch on (the server's `message` is English and may change).

export interface User {
  id: number;
  email: string;
  displayName: string;
}

export interface Session {
  user: User;
  csrfToken: string;
  expiresAt: string;
}

export interface Subject {
  id: number;
  code: string;
}

export type Chapter = Subject;

export type RevisionStatus = 'draft' | 'in_review' | 'changes_requested' | 'approved';

export interface Review {
  action: 'approve' | 'request_changes';
  reviewerId: number;
  comment: string | null;
  decidedAt: string;
}

export interface Revision {
  id: number;
  lessonId: number;
  number: number;
  status: RevisionStatus;
  authorId: number;
  baseRevisionId: number | null;
  title: string;
  objectives: string;
  prerequisites: string;
  content: unknown;
  schemaVersion: number;
  language: 'fr' | 'ar';
  subjectId: number;
  chapterId: number | null;
  version: number;
  createdAt: string;
  updatedAt: string;
  submittedAt: string | null;
  mediaIds: number[];
  review: Review | null;
  approvedBy: number | null;
}

export interface LatestRevision {
  id: number;
  number: number;
  status: RevisionStatus;
  title: string;
  chapterId: number | null;
  chapterCode: string | null;
  version: number;
  updatedAt: string;
}

export interface OwnedLesson {
  id: number;
  publicId: string;
  subjectId: number;
  subjectCode: string;
  language: 'fr' | 'ar';
  translationGroupId: string;
  archived: boolean;
  publishedRevisionId: number | null;
  createdAt: string;
  hasFeedback: boolean;
  feedbackRevisionId: number | null;
  latestRevision: LatestRevision | null;
}

/** The Studio media route a preview links embedded images to: the author's, the reviewer's or the publisher's. */
export type MediaRoute = 'author' | 'review' | 'publish';

export interface Media {
  id: number;
  contentType: string;
  byteSize: number;
  width: number;
  height: number;
  sha256: string;
  url: string;
}

/** Fields shared by every draft write (API.md "Draft fields"). */
export interface DraftFields {
  content: unknown;
  prerequisites: string;
  chapterId: number | null;
  mediaIds: number[];
}

export interface NewLesson extends DraftFields {
  subjectId: number;
  language: 'fr' | 'ar';
  translationOf: number | null;
}

export class ApiError extends Error {
  constructor(
    /** HTTP status; 0 when the server could not be reached. */
    readonly status: number,
    /** The server's error code (API.md), or `network` / `http_<status>` when the body had none. */
    readonly code: string,
    message: string,
    /** Seconds from Retry-After, when sent. */
    readonly retryAfter: number | null = null,
  ) {
    super(message);
    this.name = 'ApiError';
  }
}

export const isApiError = (error: unknown, code?: string): error is ApiError =>
  error instanceof ApiError && (code === undefined || error.code === code);

interface RequestOptions {
  json?: unknown;
  body?: Blob;
  contentType?: string;
}

export interface ApiOptions {
  fetch?: typeof fetch;
  /** Called whenever the server answers 401 unauthenticated (expired, revoked or missing session). */
  onUnauthenticated?: () => void;
  /** Called when the browser's session now belongs to another account than the one this page started with. */
  onAccountChanged?: () => void;
  /** Clock, for tests. */
  now?: () => number;
}

/**
 * Reads are preceded by an account check (GET /api/auth/session) when the
 * last one is older than this, or was invalidated (navigation, the tab
 * becoming visible again), so a page never shows another account's data.
 */
export const ACCOUNT_CHECK_MS = 3000;

/**
 * Thrown (status 0, never sent by the server) when the session cookie now
 * belongs to another account, for example after someone signed in as another
 * user in another tab. Nothing is sent while in this state.
 */
export const ACCOUNT_CHANGED = 'account_changed';

const UNSAFE = new Set(['POST', 'PUT', 'PATCH', 'DELETE']);

export class Api {
  private csrf: string | null = null;
  /** The account this page started with; every later session must be the same account. */
  private userId: number | null = null;
  private accountChanged = false;
  /** When the account was last confirmed (0: never, or invalidated). */
  private checkedAt = 0;
  private checking: Promise<Session> | null = null;
  private readonly now: () => number;
  private readonly fetchImpl: typeof fetch;
  private readonly onUnauthenticated: () => void;
  private readonly onAccountChanged: () => void;

  constructor(options: ApiOptions = {}) {
    // Bound: calling an unbound window.fetch throws "Illegal invocation".
    this.fetchImpl = options.fetch ?? globalThis.fetch.bind(globalThis);
    this.onUnauthenticated = options.onUnauthenticated ?? (() => {});
    this.onAccountChanged = options.onAccountChanged ?? (() => {});
    this.now = options.now ?? Date.now;
  }

  /** Forces an account check before the next read (in-app navigation, tab shown again). */
  invalidateAccount() {
    this.checkedAt = 0;
  }

  /**
   * Reads the current session and keeps its CSRF token for later writes. The
   * first session fixes the page's account; a session of another account is
   * refused with account_changed, its token is never kept, and every request
   * is refused until a session of the original account is read again.
   */
  session(): Promise<Session> {
    // Concurrent checks share one request.
    this.checking ??= this.readSession().finally(() => {
      this.checking = null;
    });
    return this.checking;
  }

  private async readSession(): Promise<Session> {
    const session = await this.send<Session>('GET', '/api/auth/session', {}, false);
    if (this.userId === null) this.userId = session.user.id;
    if (session.user.id !== this.userId) {
      this.csrf = null;
      if (!this.accountChanged) {
        this.accountChanged = true;
        this.onAccountChanged();
      }
      throw new ApiError(0, ACCOUNT_CHANGED, 'Another account is signed in');
    }
    this.accountChanged = false;
    this.csrf = session.csrfToken;
    this.checkedAt = this.now();
    return session;
  }

  async logout(): Promise<void> {
    await this.request<void>('POST', '/api/auth/logout');
    this.csrf = null;
  }

  subjects = async () => (await this.request<{ subjects: Subject[] }>('GET', '/api/catalogue/subjects')).subjects;
  chapters = (code: string) => this.request<{ subject: Subject; chapters: Chapter[] }>('GET', `/api/catalogue/subjects/${encodeURIComponent(code)}/chapters`);
  myLessons = async () => (await this.request<{ lessons: OwnedLesson[] }>('GET', '/api/lessons/mine')).lessons;
  createLesson = (body: NewLesson) => this.request<Revision>('POST', '/api/lessons', { json: body });
  startDraft = (lessonId: number, body: DraftFields) => this.request<Revision>('POST', `/api/lessons/${lessonId}/drafts`, { json: body });
  revision = (id: number) => this.request<Revision>('GET', `/api/revisions/${id}`);
  saveRevision = (id: number, expectedVersion: number, body: DraftFields) =>
    this.request<Revision>('PUT', `/api/revisions/${id}`, { json: { expectedVersion, ...body } });
  submit = (id: number, expectedVersion: number) => this.request<Revision>('POST', `/api/revisions/${id}/submit`, { json: { expectedVersion } });
  discard = (id: number, expectedVersion: number) => this.request<void>('DELETE', `/api/revisions/${id}?expectedVersion=${expectedVersion}`);
  /** Raw image body, not multipart (API.md POST /api/media). */
  uploadMedia = (file: Blob) => this.request<Media>('POST', '/api/media', { body: file, contentType: file.type });
  /** mediaRoute: which access-checked route embedded uploaded images are linked to (API.md POST /api/preview). */
  preview = async (content: unknown, mediaRoute: MediaRoute = 'author') =>
    (await this.request<{ html: string }>('POST', '/api/preview', { json: mediaRoute === 'author' ? { content } : { content, mediaRoute } })).html;

  /**
   * Sends a request. A rejected CSRF token (for example after signing in
   * again in another tab, which rotates the session) is refreshed from the
   * session once and the request repeated; nothing was written the first time.
   * The refresh checks the account (session()), so a request is never
   * repeated as another user. Reads first confirm the account when the last
   * check is stale (writes are bound to the account by their CSRF token).
   */
  async request<T>(method: string, path: string, options: RequestOptions = {}): Promise<T> {
    if (this.accountChanged) throw new ApiError(0, ACCOUNT_CHANGED, 'Another account is signed in');
    if (UNSAFE.has(method) ? this.csrf === null : this.now() - this.checkedAt > ACCOUNT_CHECK_MS) await this.session();
    try {
      return await this.send<T>(method, path, options, true);
    } catch (error) {
      if (!isApiError(error, 'csrf_rejected')) throw error;
      await this.session();
      return this.send<T>(method, path, options, true);
    }
  }

  private async send<T>(method: string, path: string, options: RequestOptions, csrf: boolean): Promise<T> {
    const headers: Record<string, string> = { Accept: 'application/json' };
    let body: BodyInit | undefined;
    if (options.json !== undefined) {
      headers['Content-Type'] = 'application/json';
      body = JSON.stringify(options.json);
    } else if (options.body !== undefined) {
      headers['Content-Type'] = options.contentType ?? 'application/octet-stream';
      body = options.body;
    }
    if (csrf && UNSAFE.has(method) && this.csrf) headers['X-CSRF-Token'] = this.csrf;
    let response: Response;
    try {
      response = await this.fetchImpl(path, { method, headers, body, credentials: 'same-origin', cache: 'no-store' });
    } catch {
      throw new ApiError(0, 'network', 'Network error');
    }
    if (response.ok) {
      if (response.status === 204) return undefined as T;
      return (await response.json()) as T;
    }
    const error = await readError(response);
    if (error.status === 401 && error.code === 'unauthenticated') {
      this.csrf = null;
      this.onUnauthenticated();
    }
    throw error;
  }
}

async function readError(response: Response): Promise<ApiError> {
  const header = response.headers.get('Retry-After');
  const retryAfter = header !== null && /^\d+$/.test(header) ? Number(header) : null;
  let code = `http_${response.status}`;
  let message = response.statusText;
  try {
    const body = (await response.json()) as { error?: { code?: unknown; message?: unknown } };
    if (typeof body.error?.code === 'string') code = body.error.code;
    if (typeof body.error?.message === 'string') message = body.error.message;
  } catch {
    // Not JSON (a proxy page, for example): keep the status-based code.
  }
  return new ApiError(response.status, code, message, retryAfter);
}
