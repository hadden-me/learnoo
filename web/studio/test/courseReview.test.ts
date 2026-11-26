import { describe, expect, it, vi } from 'vitest';
import { Api, ApiError } from '../src/api/client';
import type { Course } from '../src/api/courses';
import {
  courseReviewApi,
  incompleteFields,
  ineligibleSteps,
  reviewerReasons,
  type AdminCourseDetail,
  type AdminCourseRevision,
  type CoursePublishItem,
  type CourseReviewItem,
  type ReviewedRevision,
  type StepChange,
} from '../src/api/courseReview';
import { describeCourseWorkflowError } from '../src/api/courseReviewMessages';
import type { ReviewQueueItem } from '../src/api/editorial';
import { courseState } from '../src/courses/courseLabels';
import { canStartDraft } from '../src/courses/CourseStatus';
import { fromRevision } from '../src/courses/outline';
import {
  adminCourseActions,
  changeSummary,
  describeBlocked,
  describeOwnProblem,
  filterQueue,
  holdsCandidate,
  mergeQueue,
  paginate,
  partialDocument,
  submissionAge,
  submissionProblems,
} from '../src/courses/workflow';
import { publishFacts } from '../src/editorial/CourseReviewPages';
import { routeAccess } from '../src/editorial/spacesUi';
import { parseEditorialRoute, paths } from '../src/editorialRoutes';
import { NO_SPACES } from '../src/spaces';

type Call = { url: string; method: string; body: unknown };

function fakeServer(respond: (url: string, method: string) => [number, unknown]) {
  const calls: Call[] = [];
  const fetch = vi.fn(async (url: string, init: RequestInit) => {
    calls.push({ url, method: init.method ?? 'GET', body: init.body ? JSON.parse(init.body as string) : undefined });
    const [status, body] = respond(url, init.method ?? 'GET');
    return new Response(status === 204 ? null : JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
  });
  return { fetch: fetch as unknown as typeof globalThis.fetch, calls };
}

const session = { user: { id: 7, email: 'test-reviewer@example.test', displayName: 'TEST' }, csrfToken: 'tok-1', expiresAt: '2026-11-25T09:00:00Z' };

describe('course review API', () => {
  it('sends the documented paths and bodies', async () => {
    const server = fakeServer((url) => (url === '/api/auth/session' ? [200, session] : [200, { revisions: [], requests: [], courses: [], nextCursor: null }]));
    const cr = courseReviewApi(new Api({ fetch: server.fetch }));
    await cr.submit(5, 3);
    await cr.newerRevisions(2);
    await cr.createAmendment(2, [{ lessonId: 4, revisionId: 9 }]);
    await cr.requestArchive(2, 'Obsolète.');
    await cr.reviewQueue();
    await cr.reviewQueue(3);
    await cr.review(5);
    await cr.approve(5, '  ');
    await cr.approve(5, 'Bien.');
    await cr.requestChanges(5, 'Précisez.');
    await cr.publishQueue();
    await cr.publish(5, null);
    await cr.restore(2, 5, null);
    await cr.suspend(2, 'Urgence.', 5);
    await cr.archive(2, 'Demande.', null);
    await cr.unarchive(2, 'Erreur.');
    await cr.transfer(2, 11, 'Départ.');
    await cr.withdraw(5, 'Périmée.');
    await cr.discardDraft(6, 'Bloqué.');
    await cr.archiveRequests();
    await cr.blockedReviews('40');
    await cr.adminCourses();
    await cr.adminCourse(2);
    const sent = server.calls.filter((c) => c.url !== '/api/auth/session').map((c) => [c.method, c.url, c.body]);
    expect(sent).toEqual([
      ['POST', '/api/course-revisions/5/submit', { expectedVersion: 3 }],
      ['GET', '/api/courses/2/newer-revisions', undefined],
      ['POST', '/api/courses/2/amendments', { steps: [{ lessonId: 4, revisionId: 9 }] }],
      ['POST', '/api/courses/2/archive-request', { reason: 'Obsolète.' }],
      ['GET', '/api/review/queue?type=course', undefined],
      ['GET', '/api/review/queue?type=course&subjectId=3', undefined],
      ['GET', '/api/review/course-revisions/5', undefined],
      ['POST', '/api/review/course-revisions/5/approve', {}],
      ['POST', '/api/review/course-revisions/5/approve', { comment: 'Bien.' }],
      ['POST', '/api/review/course-revisions/5/request-changes', { comment: 'Précisez.' }],
      ['GET', '/api/publish/queue?type=course', undefined],
      ['POST', '/api/course-revisions/5/publish', { expectedPublishedRevisionId: null }],
      ['POST', '/api/courses/2/restore', { revisionId: 5, expectedPublishedRevisionId: null }],
      ['POST', '/api/courses/2/suspend', { reason: 'Urgence.', expectedPublishedRevisionId: 5 }],
      ['POST', '/api/courses/2/archive', { reason: 'Demande.', expectedPublishedRevisionId: null }],
      ['POST', '/api/courses/2/unarchive', { reason: 'Erreur.' }],
      ['POST', '/api/courses/2/transfer', { toUserId: 11, reason: 'Départ.' }],
      ['POST', '/api/course-revisions/5/withdraw', { reason: 'Périmée.' }],
      ['DELETE', '/api/admin/course-revisions/6', { reason: 'Bloqué.' }],
      ['GET', '/api/admin/courses/archive-requests', undefined],
      ['GET', '/api/admin/courses/blocked-reviews?cursor=40', undefined],
      ['GET', '/api/admin/courses', undefined],
      ['GET', '/api/admin/courses/2', undefined],
    ]);
  });

  it('keeps the error body’s extra keys: fields, steps and reasons', async () => {
    const step = { moduleKey: 'k1', position: 1, title: 'S', lessonId: 4, revisionId: 9 };
    const bodies: Record<string, unknown> = {
      '/api/course-revisions/1/submit': { error: { code: 'incomplete', message: 'x', fields: ['title', 'module:k1'] } },
      '/api/course-revisions/2/submit': { error: { code: 'ineligible', message: 'x', steps: [step] } },
      '/api/review/course-revisions/3/approve': { error: { code: 'not_eligible_reviewer', message: 'x', reasons: [{ reason: 'course_author', lessonId: null, lessonRevisionId: null, reviewerId: 7 }] } },
    };
    const server = fakeServer((url) => (url === '/api/auth/session' ? [200, session] : [url.includes('approve') ? 403 : url.includes('/1/') ? 422 : 409, bodies[url]]));
    const cr = courseReviewApi(new Api({ fetch: server.fetch }));
    const e1 = await cr.submit(1, 1).catch((e) => e);
    expect(e1).toBeInstanceOf(ApiError);
    expect(incompleteFields(e1)).toEqual(['title', 'module:k1']);
    expect(e1.details.code).toBeUndefined();
    const e2 = await cr.submit(2, 1).catch((e) => e);
    expect(ineligibleSteps(e2)).toEqual([step]);
    const e3 = await cr.approve(3, '').catch((e) => e);
    expect(reviewerReasons(e3)[0].reason).toBe('course_author');
    expect(incompleteFields(new Error('x'))).toEqual([]);
  });

  it('explains the workflow codes in French and falls back for the others', () => {
    expect(describeCourseWorkflowError(new ApiError(409, 'draft_exists', ''))).toContain('brouillon');
    expect(describeCourseWorkflowError(new ApiError(409, 'stale_pointer', ''))).toContain('Rien n’a été modifié');
    expect(describeCourseWorkflowError(new ApiError(409, 'stale_base', ''))).toContain('Rien n’a été publié');
    expect(describeCourseWorkflowError(new ApiError(422, 'placement_invalid', 'detail'))).toContain('Classement refusé');
    expect(describeCourseWorkflowError(new ApiError(0, 'network', ''))).toContain('Le serveur ne répond pas');
  });
});

const revision = (over: Partial<ReviewedRevision> = {}): ReviewedRevision => ({
  id: 10,
  courseId: 2,
  number: 1,
  status: 'draft',
  kind: 'ordinary',
  authorId: 7,
  baseRevisionId: null,
  compareRevisionId: null,
  subjectId: 1,
  language: 'fr',
  title: 'TEST',
  summary: '',
  objectives: '',
  prerequisites: '',
  coverMediaId: null,
  coverAlt: null,
  version: 1,
  createdAt: '2026-11-20T09:00:00Z',
  updatedAt: '2026-11-20T09:00:00Z',
  submittedAt: null,
  withdrawnAt: null,
  review: null,
  blocked: null,
  assignments: [],
  modules: [
    { key: 'k1', position: 1, title: 'Un', chapterId: null, steps: [{ position: 1, title: 'Étape A', lessonId: 4, revisionId: 9, revisionNumber: 2, ownLesson: true, eligible: false }] },
    { key: 'k2', position: 2, title: 'Deux', chapterId: null, steps: [] },
  ],
  ...over,
});

describe('submission refusals', () => {
  it('points incomplete fields and ineligible steps at the editor’s targets', () => {
    const draft = fromRevision(revision());
    const problems = submissionProblems(draft, ['title', 'objectives', 'module:k2', 'module:gone'], [{ moduleKey: 'k1', position: 1, title: 'Étape A', lessonId: 4, revisionId: 9 }]);
    expect(problems.map((p) => p.target)).toEqual(['title', 'objectives', `module:${draft.modules[1].localId}`, 'modules', `step:${draft.modules[0].steps[0].localId}`]);
    expect(problems[2].message).toContain('Module 2 « Deux »');
    expect(problems[4].message).toContain('module 1');
  });
});

describe('reviewer eligibility in words', () => {
  it('names the gap of a blocked revision', () => {
    const none = describeBlocked({ reason: 'no_eligible_reviewer', details: [{ reviewerId: null, reason: 'no_subject_reviewer', lessonId: null, lessonRevisionId: null }] });
    expect(none.summary).toContain('aucun relecteur actif');
    expect(none.details).toEqual([]);
    const gap = describeBlocked({
      reason: 'no_eligible_reviewer',
      details: [
        { reviewerId: 3, reviewerName: 'Amel', reason: 'course_author', lessonId: null, lessonRevisionId: null },
        { reviewerId: 5, reviewerName: 'Karim', reason: 'lesson_author', lessonId: 4, lessonRevisionId: 9 },
      ],
    });
    expect(gap.details).toEqual(['Amel est l’auteur de cette version du cours.', 'Karim est l’auteur de la révision n° 9 de la leçon n° 4, ajoutée ou remplacée par cette version.']);
  });

  it('explains the caller’s own disqualification', () => {
    expect(describeOwnProblem({ reviewerId: 7, reason: 'course_author', lessonId: null, lessonRevisionId: null })).toContain('auteur de cette version du cours');
    expect(describeOwnProblem({ reviewerId: 7, reason: 'lesson_author', lessonId: 4, lessonRevisionId: 9 })).toContain('votre propre leçon');
    expect(describeOwnProblem({ reviewerId: 7, reason: 'not_subject_reviewer', lessonId: null, lessonRevisionId: null })).toContain('pas relecteur');
  });
});

describe('mixed review queue', () => {
  const lesson = (id: number, subjectId: number, submittedAt: string): ReviewQueueItem => ({
    revisionId: id, lessonId: id, number: 1, title: `L${id}`, language: 'fr', subjectId, chapterId: null, authorId: 1, authorName: 'A', submittedAt,
  });
  const course = (id: number, subjectId: number, submittedAt: string, eligible: boolean, blocked = false): CourseReviewItem => ({
    revisionId: id, courseId: id, number: 1, kind: 'ordinary', title: `C${id}`, language: 'fr', subjectId, authorId: 1, authorName: 'A', submittedAt, eligible,
    blocked: blocked ? { reason: 'no_eligible_reviewer', details: [] } : null,
  });
  const entries = mergeQueue(
    [lesson(1, 1, '2026-11-20T10:00:00Z'), lesson(2, 2, '2026-11-18T10:00:00Z')],
    [course(1, 1, '2026-11-19T10:00:00Z', true), course(2, 1, '2026-11-21T10:00:00Z', false, true), course(3, 2, '2026-11-22T10:00:00Z', false)],
  );

  it('merges lessons and courses, oldest first, with distinct IDs for equal revision IDs', () => {
    expect(entries.map((e) => e.id)).toEqual(['lesson-2', 'course-1', 'lesson-1', 'course-2', 'course-3']);
  });

  it('filters by content type, subject and decision', () => {
    const ids = (f: Parameters<typeof filterQueue>[1]) => filterQueue(entries, f).map((e) => e.id);
    expect(ids({ type: 'course', subjectId: null, decision: 'all' })).toEqual(['course-1', 'course-2', 'course-3']);
    expect(ids({ type: 'all', subjectId: 1, decision: 'all' })).toEqual(['course-1', 'lesson-1', 'course-2']);
    expect(ids({ type: 'all', subjectId: null, decision: 'mine' })).toEqual(['lesson-2', 'course-1', 'lesson-1']);
    expect(ids({ type: 'all', subjectId: null, decision: 'blocked' })).toEqual(['course-2']);
    expect(ids({ type: 'all', subjectId: null, decision: 'other' })).toEqual(['course-3']);
  });

  it('pages and clamps', () => {
    expect(paginate([1, 2, 3, 4, 5], 1, 2)).toEqual({ items: [3, 4], page: 1, pages: 3 });
    expect(paginate([1, 2, 3], 9, 2)).toEqual({ items: [3], page: 1, pages: 2 });
    expect(paginate([], 0, 2)).toEqual({ items: [], page: 0, pages: 1 });
  });

  it('says how long ago a submission was made', () => {
    const now = new Date('2026-11-26T12:00:00Z');
    expect(submissionAge('2026-11-26T08:00:00Z', now)).toBe('soumise aujourd’hui');
    expect(submissionAge('2026-11-25T08:00:00Z', now)).toBe('soumise hier');
    expect(submissionAge('2026-11-20T08:00:00Z', now)).toBe('soumise il y a 6 jours');
  });
});

describe('change list', () => {
  const step = (change: StepChange['change']): StepChange => ({
    moduleKey: 'k', position: 1, title: 't', lessonId: 1, revisionId: 1, revisionNumber: 1, change, previousRevisionId: null, previousRevisionNumber: null,
    previousModuleKey: null, previousPosition: null, previousTitle: null, structureChanged: false, diff: null, eligible: true, content: null,
  });
  it('summarizes the changes', () => {
    expect(changeSummary([step('replaced'), step('unchanged'), step('unchanged')], 0)).toBe('1 étape remplacée · 2 étapes inchangées');
    expect(changeSummary([step('added')], 2)).toBe('1 étape ajoutée · 2 étapes retirées');
  });
  it('builds a partial document for a diff side', () => {
    const doc = partialDocument({ schemaVersion: 1, title: 'T', language: 'ar', status: 'unreviewed', objective: 'O', blocks: [1, 2] }, [{ type: 'paragraph' }], 'fr');
    expect(doc).toEqual({ schemaVersion: 1, title: 'T', language: 'ar', status: 'unreviewed', objective: '', blocks: [{ type: 'paragraph' }] });
    expect(partialDocument(null, [], 'fr').language).toBe('fr');
  });
});

describe('course states', () => {
  const course = (over: Partial<Course> = {}): Course => ({
    id: 2, publicId: 'p', ownerId: 7, subjectId: 1, language: 'fr', publishedRevisionId: null, archived: false, suspended: false, createdAt: '',
    latestRevision: { id: 10, number: 1, status: 'approved', title: 'T', version: 2, updatedAt: '' }, ...over,
  });
  it('puts archive and suspension before the revision state', () => {
    expect(courseState(course({ publishedRevisionId: 10 })).label).toBe('Publié');
    expect(courseState(course()).label).toBe('Approuvé, en attente de publication');
    expect(courseState(course({ suspended: true })).label).toBe('Suspendu par un administrateur');
    expect(courseState(course({ archived: true, suspended: false })).label).toBe('Archivé');
    expect(courseState(course({ latestRevision: { id: 10, number: 1, status: 'in_review', title: 'T', version: 2, updatedAt: '', withdrawn: true } })).label).toBe(
      'Version retirée par un administrateur',
    );
  });
  it('offers a new draft only when the course has no open candidate', () => {
    expect(canStartDraft(course(), null)).toBe(true);
    expect(canStartDraft(course(), revision({ status: 'in_review' }))).toBe(false);
    expect(canStartDraft(course(), revision({ status: 'changes_requested' }))).toBe(true);
    expect(canStartDraft(course(), revision({ status: 'approved' }))).toBe(false);
    expect(canStartDraft(course({ publishedRevisionId: 10 }), revision({ status: 'approved' }))).toBe(true);
    expect(canStartDraft(course({ suspended: true }), revision({ status: 'approved' }))).toBe(true);
    expect(canStartDraft(course(), revision({ status: 'in_review', withdrawnAt: '2026-11-21T00:00:00Z' }))).toBe(true);
    expect(canStartDraft(course({ archived: true }), null)).toBe(false);
  });
});

describe('publication facts', () => {
  const item = (over: Partial<CoursePublishItem> = {}): CoursePublishItem => ({
    revisionId: 5, courseId: 2, coursePublicId: 'p', number: 2, kind: 'amendment', title: 'T', language: 'fr', subjectId: 1, authorId: 7, authorName: 'A',
    approvedBy: 3, approvedAt: '', baseRevisionId: 4, publishedRevisionId: 4, staleBase: false, archived: false, ineligibleSteps: 0, ...over,
  });
  it('lists what blocks publication', () => {
    expect(publishFacts(item()).blockers).toEqual([]);
    expect(publishFacts(item({ publishedRevisionId: null })).effect).toContain('Première publication');
    const f = publishFacts(item({ staleBase: true, archived: true, ineligibleSteps: 2 }));
    expect(f.blockers).toHaveLength(3);
    expect(f.blockers[2]).toContain('2 étapes épinglent');
  });
});

describe('administrator recovery actions', () => {
  const rev = (over: Partial<AdminCourseRevision>): AdminCourseRevision => ({
    id: 1, number: 1, status: 'approved', kind: 'ordinary', title: 'T', authorId: 7, authorName: 'A', updatedAt: '', submittedAt: '', withdrawnAt: null, everPublished: false, review: null, ...over,
  });
  const detail = (course: Partial<AdminCourseDetail['course']>, revisions: AdminCourseRevision[]): AdminCourseDetail => ({
    course: { id: 2, publicId: 'p', ownerId: 7, subjectId: 1, language: 'fr', publishedRevisionId: null, archived: false, suspended: false, createdAt: '', ownerName: 'A', title: 'T', ...course },
    revisions, history: [], lifecycle: [], authors: [],
  });

  it('offers what the server would accept', () => {
    const published = rev({ id: 1, number: 1, everPublished: true });
    const pending = rev({ id: 2, number: 2, status: 'in_review' });
    const a = adminCourseActions(detail({ publishedRevisionId: 1 }, [pending, published]));
    expect(a.suspend).toBe(true);
    expect(a.archive).toBe(true);
    expect(a.unarchive).toBe(false);
    expect(a.restore).toEqual([]);
    expect(a.withdraw).toEqual([pending]);
    expect(a.transfer).toBe(false);

    const suspended = adminCourseActions(detail({ publishedRevisionId: null, suspended: true }, [published]));
    expect(suspended.suspend).toBe(false);
    expect(suspended.restore).toEqual([published]);
    expect(suspended.withdraw).toEqual([published]);
    expect(suspended.transfer).toBe(true);

    const draft = rev({ id: 3, number: 3, status: 'draft' });
    const archived = adminCourseActions(detail({ archived: true }, [draft, published]));
    expect(archived.unarchive).toBe(true);
    expect(archived.archive).toBe(false);
    expect(archived.restore).toEqual([]);
    expect(archived.discard).toEqual([draft]);
    expect(holdsCandidate(draft)).toBe(true);
    expect(holdsCandidate(rev({ status: 'in_review', withdrawnAt: 'x' }))).toBe(false);
  });
});

describe('routes', () => {
  it('parses the course review, publication and administration routes', () => {
    expect(parseEditorialRoute(paths.reviewCourse(5))).toEqual({ name: 'reviewCourse', revisionId: 5 });
    expect(parseEditorialRoute(paths.publishCourse(5))).toEqual({ name: 'publishCourse', revisionId: 5 });
    expect(parseEditorialRoute(paths.adminCourses)).toEqual({ name: 'adminCourses' });
    expect(parseEditorialRoute(`${paths.adminCourse(2)}/`)).toEqual({ name: 'adminCourse', courseId: 2 });
    expect(parseEditorialRoute('/studio/review/courses/0')).toBeNull();
  });
  it('lets publishers and administrators read a course revision, never decide', () => {
    const route = { name: 'reviewCourse', revisionId: 5 } as const;
    expect(routeAccess(route, NO_SPACES).allowed).toBe(false);
    expect(routeAccess(route, { ...NO_SPACES, administrator: true }).allowed).toBe(true);
    expect(routeAccess({ name: 'adminCourse', courseId: 2 }, { ...NO_SPACES, publisher: true }).allowed).toBe(false);
    expect(routeAccess({ name: 'publishCourse', revisionId: 5 }, { ...NO_SPACES, publisher: true }).allowed).toBe(true);
  });
});
