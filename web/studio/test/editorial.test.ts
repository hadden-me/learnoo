import { describe, expect, it, vi } from 'vitest';
import { Api, ApiError } from '../src/api/client';
import { editorialApi, type LessonPublication, type LessonRevisionState } from '../src/api/editorial';
import { describeEditorialError } from '../src/api/editorialMessages';
import { groupBySubject } from '../src/editorial/ReviewPages';
import { latestOnly } from '../src/editorial/common';
import { excerpt } from '../src/editorial/ReportPages';
import { routeAccess } from '../src/editorial/spacesUi';
import { canUnpublish, describeEvent, describeSubmission, revisionActions, revisionStateLabel } from '../src/editorial/policy';
import { isEditorialRoute, lessonRefFromInput, parseEditorialRoute, paths } from '../src/editorialRoutes';
import { parseRoute } from '../src/router';
import { NO_SPACES, canHandleReports, discoverSpaces, type Spaces } from '../src/spaces';

type Call = { url: string; method: string; headers: Record<string, string>; body: unknown };

function fakeServer(respond: (url: string, method: string) => [number, unknown]) {
  const calls: Call[] = [];
  const fetch = vi.fn(async (url: string, init: RequestInit) => {
    calls.push({ url, method: init.method ?? 'GET', headers: init.headers as Record<string, string>, body: init.body ? JSON.parse(init.body as string) : undefined });
    const [status, body] = respond(url, init.method ?? 'GET');
    return new Response(status === 204 ? null : JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
  });
  return { fetch: fetch as unknown as typeof globalThis.fetch, calls };
}

const session = { user: { id: 7, email: 'test-reviewer@example.test', displayName: 'TEST' }, csrfToken: 'tok-1', expiresAt: '2026-11-25T09:00:00Z' };
const err = (code: string) => ({ error: { code, message: code } });

describe('editorial API', () => {
  it('sends the documented paths and bodies, with the CSRF token on writes', async () => {
    const server = fakeServer((url) => (url === '/api/auth/session' ? [200, session] : [200, { revisions: [], lessons: [], reports: [] }]));
    const ed = editorialApi(new Api({ fetch: server.fetch }));
    await ed.reviewQueue();
    await ed.reviewQueue(3);
    await ed.approve(12, '   ');
    await ed.approve(12, 'Très clair.');
    await ed.requestChanges(12, 'Précisez.');
    await ed.publishQueue();
    await ed.publishRevision(12);
    await ed.publish(12);
    await ed.lessonPublication('0b9c1d2e-0000-4000-8000-000000000000');
    await ed.withdraw(12, 'Erreur.', 9);
    await ed.unpublish(4, 'Urgence.', 9);
    await ed.restore(9, null);
    await ed.reports('open');
    await ed.resolveReport(5, 'dismiss', 'Doublon.');
    const sent = server.calls.filter((c) => c.url !== '/api/auth/session').map((c) => [c.method, c.url, c.body]);
    expect(sent).toEqual([
      ['GET', '/api/review/queue', undefined],
      ['GET', '/api/review/queue?subjectId=3', undefined],
      ['POST', '/api/review/revisions/12/approve', {}],
      ['POST', '/api/review/revisions/12/approve', { comment: 'Très clair.' }],
      ['POST', '/api/review/revisions/12/request-changes', { comment: 'Précisez.' }],
      ['GET', '/api/publish/queue', undefined],
      ['GET', '/api/publish/revisions/12', undefined],
      ['POST', '/api/revisions/12/publish', {}],
      ['GET', '/api/lessons/0b9c1d2e-0000-4000-8000-000000000000/publication', undefined],
      ['POST', '/api/revisions/12/withdraw', { reason: 'Erreur.', expectedPublishedRevisionId: 9 }],
      ['POST', '/api/lessons/4/unpublish', { reason: 'Urgence.', expectedPublishedRevisionId: 9 }],
      ['POST', '/api/revisions/9/restore', { expectedPublishedRevisionId: null }],
      ['GET', '/api/review/error-reports?status=open', undefined],
      ['POST', '/api/review/error-reports/5/dismiss', { note: 'Doublon.' }],
    ]);
    for (const c of server.calls.filter((c) => c.method === 'POST')) expect(c.headers['X-CSRF-Token']).toBe('tok-1');
    for (const c of server.calls.filter((c) => c.method === 'GET')) expect(c.headers['X-CSRF-Token']).toBeUndefined();
  });

  it('explains editorial refusals in French and falls back to the authoring messages', () => {
    expect(describeEditorialError(new ApiError(403, 'self_review', 'x'))).toContain('auteur de cette révision');
    expect(describeEditorialError(new ApiError(409, 'not_in_review', 'x'))).toContain('Rien n’a été écrit');
    expect(describeEditorialError(new ApiError(409, 'stale_base', 'x'))).toContain('Rien n’a été publié');
    expect(describeEditorialError(new ApiError(409, 'stale_pointer', 'x'))).toContain('Rien n’a été modifié');
    expect(describeEditorialError(new ApiError(409, 'already_published', 'x'))).toContain('restauration');
    expect(describeEditorialError(new ApiError(409, 'withdrawn', 'x'))).toContain('retirée');
    expect(describeEditorialError(new ApiError(409, 'not_open', 'x'))).toContain('déjà été traité');
    expect(describeEditorialError(new ApiError(409, 'retry', 'x'))).toContain('réessayez');
    expect(describeEditorialError(new ApiError(0, 'network', 'x'))).toContain('ne répond pas');
    expect(describeEditorialError('boom')).toBe('Erreur inattendue dans le Studio.');
  });
});

describe('space discovery', () => {
  it('reads each space from its endpoint: 403 hides the space, success shows it', async () => {
    const allowed = new Set(['/api/review/queue', '/api/admin/lessons']);
    const server = fakeServer((url) => (url === '/api/auth/session' ? [200, session] : allowed.has(url) ? [200, {}] : [403, err('forbidden')]));
    const spaces = await discoverSpaces(new Api({ fetch: server.fetch }));
    expect(spaces).toEqual({ author: false, reviewer: true, publisher: false, administrator: true });
    expect(server.calls.map((c) => c.url).filter((u) => u !== '/api/auth/session').sort()).toEqual(['/api/admin/lessons', '/api/lessons/mine', '/api/publish/queue', '/api/review/queue']);
    expect(canHandleReports(spaces)).toBe(true);
    expect(canHandleReports({ ...NO_SPACES, publisher: true })).toBe(false);
  });

  it('does not mistake a failure for a missing permission', async () => {
    const server = fakeServer((url) => (url === '/api/auth/session' ? [200, session] : url === '/api/publish/queue' ? [500, err('internal')] : [403, err('forbidden')]));
    await expect(discoverSpaces(new Api({ fetch: server.fetch }))).rejects.toMatchObject({ code: 'internal' });
  });

  it('offers a space route only to accounts the server lets use it', () => {
    const reviewer: Spaces = { ...NO_SPACES, reviewer: true };
    expect(routeAccess({ name: 'reviewQueue' }, reviewer)).toEqual({ space: 'Relecture', allowed: true });
    expect(routeAccess({ name: 'publishQueue' }, reviewer).allowed).toBe(false);
    expect(routeAccess({ name: 'reports' }, reviewer).allowed).toBe(true);
    expect(routeAccess({ name: 'adminLessons' }, reviewer).allowed).toBe(false);
    expect(routeAccess({ name: 'publication', ref: '4' }, { ...NO_SPACES, publisher: true }).allowed).toBe(true);
    expect(routeAccess({ name: 'reviewRevision', revisionId: 1 }, { ...NO_SPACES, author: true }).allowed).toBe(false);
  });
});

describe('editorial routes', () => {
  it('parses every route and builds paths without a dot in the last segment', () => {
    const uuid = '0b9c1d2e-0000-4000-8000-000000000000';
    const cases: Array<[string, unknown]> = [
      [paths.reviewQueue, { name: 'reviewQueue' }],
      [paths.reviewRevision(12), { name: 'reviewRevision', revisionId: 12 }],
      [paths.publishQueue, { name: 'publishQueue' }],
      [paths.publishRevision(12), { name: 'publishRevision', revisionId: 12 }],
      [paths.adminLessons, { name: 'adminLessons' }],
      [paths.publication(4), { name: 'publication', ref: '4' }],
      [paths.publication(uuid), { name: 'publication', ref: uuid }],
      [paths.reports, { name: 'reports' }],
      [paths.report(3), { name: 'report', reportId: 3 }],
    ];
    for (const [path, route] of cases) {
      expect(path.split('/').at(-1)).not.toContain('.');
      expect(parseEditorialRoute(path)).toEqual(route);
      expect(parseRoute(`${path}/`)).toEqual(route);
      expect(isEditorialRoute(parseRoute(path))).toBe(true);
    }
    for (const bad of ['/studio/review/revisions/0', '/studio/review/revisions/x', '/studio/publication/ABC', '/studio/publication/0b9c1d2e', '/studio/reports/1/x']) {
      expect(parseRoute(bad)).toEqual({ name: 'notFound' });
    }
    expect(isEditorialRoute(parseRoute('/studio/lessons/new'))).toBe(false);
  });

  it('finds a lesson from a public ID, a reader address or a number', () => {
    const uuid = '0b9c1d2e-0000-4000-8000-000000000000';
    expect(lessonRefFromInput(` ${uuid} `)).toBe(uuid);
    expect(lessonRefFromInput(`https://learnoo.example/learn/lessons/${uuid.toUpperCase()}`)).toBe(uuid);
    expect(lessonRefFromInput('42')).toBe('42');
    expect(lessonRefFromInput('042')).toBeNull();
    expect(lessonRefFromInput('leçon')).toBeNull();
  });
});

const rev = (over: Partial<LessonRevisionState>): LessonRevisionState => ({
  id: 1,
  number: 1,
  status: 'approved',
  title: 'TEST',
  authorId: 2,
  authorName: 'TEST Autrice',
  baseRevisionId: null,
  submittedAt: '2026-11-25T09:00:00Z',
  approvedBy: 3,
  approverName: 'TEST Relecteur',
  approvedAt: '2026-11-25T09:00:00Z',
  everPublished: false,
  candidate: false,
  withdrawn: over.withdrawal != null,
  withdrawal: null,
  submission: null,
  ...over,
});
const lesson = (publishedRevisionId: number | null, revisions: LessonRevisionState[]): LessonPublication => ({
  view: 'administrator',
  lessonId: 4,
  publicId: 'x',
  subjectId: 1,
  subjectCode: 'test-math',
  language: 'fr',
  archived: false,
  publishedRevisionId,
  revisions,
  history: [],
});

describe('publication policy', () => {
  const admin: Spaces = { ...NO_SPACES, administrator: true };
  const publisher: Spaces = { ...NO_SPACES, publisher: true };
  const reviewer: Spaces = { ...NO_SPACES, reviewer: true, author: true };
  const current = rev({ id: 9, number: 2, everPublished: true });
  const earlier = rev({ id: 5, number: 1, everPublished: true });
  const pending = rev({ id: 11, number: 3, candidate: true });
  const withdrawn = rev({ id: 6, number: 1, everPublished: true, withdrawal: { withdrawnBy: 1, withdrawnByName: 'A', reason: 'r', withdrawnAt: '2026-11-25T09:00:00Z' } });
  const inReview = rev({ id: 12, number: 4, status: 'in_review', approvedBy: null, approverName: null, approvedAt: null });
  const lp = lesson(9, [inReview, pending, current, withdrawn, earlier]);

  it('offers withdrawal to administrators, restoration to publishers and administrators', () => {
    expect(revisionActions(lp, current, admin)).toEqual({ current: true, withdraw: true, restore: false });
    expect(revisionActions(lp, earlier, admin)).toEqual({ current: false, withdraw: true, restore: true });
    expect(revisionActions(lp, earlier, publisher)).toEqual({ current: false, withdraw: false, restore: true });
    expect(revisionActions(lp, pending, admin)).toEqual({ current: false, withdraw: true, restore: false });
    expect(revisionActions(lp, pending, publisher)).toEqual({ current: false, withdraw: false, restore: false });
    expect(revisionActions(lp, withdrawn, admin)).toEqual({ current: false, withdraw: false, restore: false });
    expect(revisionActions(lp, inReview, admin)).toEqual({ current: false, withdraw: false, restore: false });
    expect(revisionActions(lp, earlier, reviewer)).toEqual({ current: false, withdraw: false, restore: false });
    expect(revisionActions({ ...lp, archived: true }, earlier, admin).restore).toBe(false);
    expect(revisionActions({ ...lp, archived: true }, earlier, publisher).restore).toBe(false);
    // The publisher view has the withdrawn flag but no withdrawal details.
    const flagged = rev({ id: 7, number: 1, everPublished: true, withdrawn: true });
    expect(revisionActions(lp, flagged, publisher).restore).toBe(false);
    expect(revisionStateLabel(lp, flagged).label).toBe('Retirée définitivement');
    expect(canUnpublish(lp, admin)).toBe(true);
    expect(canUnpublish(lp, publisher)).toBe(false);
    expect(canUnpublish(lesson(null, [earlier]), admin)).toBe(false);
  });

  it('describes the submission record, marking backfilled rows', () => {
    const at = (iso: string) => `[${iso}]`;
    const recorded = rev({ submission: { submittedBy: 2, submittedAt: '2026-11-26T08:00:00Z', backfilled: false } });
    expect(describeSubmission(recorded, at)).toBe('Soumise par TEST Autrice le [2026-11-26T08:00:00Z]');
    const backfilled = rev({ submission: { submittedBy: 2, submittedAt: '2026-11-20T08:00:00Z', backfilled: true } });
    expect(describeSubmission(backfilled, at)).toBe(
      'Soumise par TEST Autrice le [2026-11-20T08:00:00Z] (reconstitué : soumise avant l’enregistrement des soumissions)',
    );
    // Publisher view: no record and no names; the submission time alone, if sent.
    expect(describeSubmission(rev({ authorName: null }), at)).toBe('Soumise le [2026-11-25T09:00:00Z]');
    expect(describeSubmission(rev({ authorName: null, submittedAt: null }), at)).toBeNull();
  });

  it('names every revision state in words', () => {
    expect(revisionStateLabel(lp, current).label).toBe('Publiée (version lue par les élèves)');
    expect(revisionStateLabel(lp, earlier).label).toBe('Approuvée, publiée auparavant');
    expect(revisionStateLabel(lp, pending).label).toBe('Approuvée, en attente de publication');
    expect(revisionStateLabel(lp, withdrawn).label).toBe('Retirée définitivement');
    expect(revisionStateLabel(lp, inReview).label).toBe('En relecture');
    const numberOf = (id: number | null) => (id === 9 ? '2' : id === 5 ? '1' : '—');
    expect(describeEvent({ action: 'unpublish', publishedRevisionId: null, previousRevisionId: 9, actorId: 1, actorName: 'A', reason: 'r', occurredAt: '' }, numberOf)).toContain('révision 2');
    expect(describeEvent({ action: 'restore', publishedRevisionId: 5, previousRevisionId: null, actorId: 1, actorName: 'A', reason: null, occurredAt: '' }, numberOf)).toBe(
      'Restauration de la révision 1 (la leçon était dépubliée)',
    );
  });
});

describe('load sequence', () => {
  it('lets only the most recently started request apply its result', () => {
    const latest = latestOnly();
    const first = latest.next();
    const second = latest.next();
    expect(latest.isCurrent(first)).toBe(false);
    expect(latest.isCurrent(second)).toBe(true);
  });
});

describe('lists', () => {
  it('groups the review queue by subject code', () => {
    const codes = new Map([
      [1, 'test-math'],
      [2, 'test-chimie'],
    ]);
    const groups = groupBySubject([{ subjectId: 1, n: 'a' }, { subjectId: 2, n: 'b' }, { subjectId: 1, n: 'c' }, { subjectId: 9, n: 'd' }], codes);
    expect(groups.map((g) => [g.subjectId, g.items.map((i) => i.n).join('')])).toEqual([
      [9, 'd'],
      [2, 'b'],
      [1, 'ac'],
    ]);
  });

  it('shortens report messages without splitting characters', () => {
    expect(excerpt('Ligne 1\nLigne 2')).toBe('Ligne 1 Ligne 2');
    expect(excerpt('é'.repeat(200), 10)).toBe(`${'é'.repeat(10)}…`);
  });
});
