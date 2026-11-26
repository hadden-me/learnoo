import { beforeEach, describe, expect, it, vi } from 'vitest';
import { Api, ApiError } from '../src/api/client';
import { courseApi, findOwnCourse, type CourseRevision } from '../src/api/courses';
import { describeCourseError } from '../src/api/courseMessages';
import { COURSE_RECOVERY_PREFIX, courseRecoveryKey, findCourseRecovery, findUserCourseRecovery, purgeCourseRecovery, removeCourseRecovery, writeCourseRecovery } from '../src/courses/courseRecovery';
import { courseState } from '../src/courses/courseLabels';
import { defaultRevision } from '../src/courses/LessonPicker';
import * as o from '../src/courses/outline';
import { saveCourse } from '../src/courses/saveCourse';
import { parseRoute, routePath } from '../src/router';

type Call = { url: string; method: string; headers: Record<string, string>; body: unknown };

function fakeServer(responses: Array<[number, unknown]>) {
  const calls: Call[] = [];
  const fetch = vi.fn(async (url: string, init: RequestInit) => {
    calls.push({ url, method: init.method ?? 'GET', headers: init.headers as Record<string, string>, body: init.body });
    const next = responses.shift();
    if (!next) throw new TypeError('Failed to fetch');
    const [status, body] = next;
    return new Response(status === 204 ? null : JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
  });
  return { fetch: fetch as unknown as typeof globalThis.fetch, calls };
}

const session = { user: { id: 7, email: 'test-author@example.test', displayName: 'TEST' }, csrfToken: 'tok-1', expiresAt: '2026-11-26T09:00:00Z' };
const err = (code: string, message = code) => ({ error: { code, message } });

const revision = (over: Partial<CourseRevision> = {}): CourseRevision => ({
  id: 30,
  courseId: 5,
  number: 1,
  status: 'draft',
  kind: 'ordinary',
  authorId: 7,
  baseRevisionId: null,
  compareRevisionId: null,
  subjectId: 1,
  language: 'fr',
  title: 'TEST cours',
  summary: '',
  objectives: '',
  prerequisites: '',
  coverMediaId: null,
  coverAlt: null,
  version: 3,
  createdAt: '2026-11-26T09:00:00Z',
  updatedAt: '2026-11-26T09:00:00Z',
  submittedAt: null,
  assignments: [{ assignmentId: 11, editionId: 4, editionState: 'current', selectable: true, retired: false }],
  modules: [
    {
      key: 'aaaaaaaa-0000-4000-8000-000000000001',
      position: 1,
      title: 'TEST module A',
      chapterId: 9,
      steps: [
        { position: 1, title: 'A1', lessonId: 101, revisionId: 1001, revisionNumber: 1, ownLesson: true, eligible: false },
        { position: 2, title: 'A2', lessonId: 102, revisionId: 1002, revisionNumber: 2, ownLesson: false, eligible: true },
      ],
    },
    { key: 'aaaaaaaa-0000-4000-8000-000000000002', position: 2, title: 'TEST module B', chapterId: null, steps: [{ position: 1, title: 'B1', lessonId: 103, revisionId: 1003, revisionNumber: 1, ownLesson: true, eligible: true }] },
  ],
  ...over,
});

const titles = (d: o.CourseDraft) => d.modules.map((m) => m.steps.map((s) => s.title));
const stepId = (d: o.CourseDraft, title: string) => d.modules.flatMap((m) => m.steps).find((s) => s.title === title)!.localId;

describe('course API client', () => {
  it('pages the owned list with the cursor and sends CSRF only on writes', async () => {
    const server = fakeServer([
      [200, session],
      [200, { courses: [{ id: 9 }], nextCursor: 'c/1+2' }],
      [200, { courses: [], nextCursor: null }],
      [201, { course: { id: 9 }, revision: revision() }],
    ]);
    const courses = courseApi(new Api({ fetch: server.fetch }));
    expect(await courses.myCourses()).toEqual({ items: [{ id: 9 }], nextCursor: 'c/1+2' });
    await courses.myCourses('c/1+2');
    await courses.createCourse(1, 'ar');
    expect(server.calls.map((c) => `${c.method} ${c.url}`)).toEqual(['GET /api/auth/session', 'GET /api/courses?mine=1', 'GET /api/courses?mine=1&cursor=c%2F1%2B2', 'POST /api/courses']);
    expect(JSON.parse(server.calls[3].body as string)).toEqual({ subjectId: 1, language: 'ar' });
    expect(server.calls[3].headers['X-CSRF-Token']).toBe('tok-1');
    expect(server.calls[1].headers['X-CSRF-Token']).toBeUndefined();
  });

  it('saves the whole draft with expectedVersion and discards with the version as the only query parameter', async () => {
    const server = fakeServer([[200, session], [200, revision({ version: 4 })], [204, null], [201, revision()], [204, null]]);
    const courses = courseApi(new Api({ fetch: server.fetch }));
    const body = o.toSaveBody(o.fromRevision(revision()));
    await courses.save(30, 3, body);
    await courses.discard(30, 4);
    await courses.startDraft(5);
    await courses.deleteCourse(5);
    expect(server.calls.slice(1).map((c) => `${c.method} ${c.url}`)).toEqual(['PUT /api/course-revisions/30', 'DELETE /api/course-revisions/30?expectedVersion=4', 'POST /api/courses/5/drafts', 'DELETE /api/courses/5']);
    expect(JSON.parse(server.calls[1].body as string)).toEqual({ expectedVersion: 3, ...body });
    expect(JSON.parse(server.calls[3].body as string)).toEqual({});
  });

  it('searches attachable lessons and reads eligible revisions and the curriculum', async () => {
    const server = fakeServer([
      [200, session],
      [200, { lessons: [], nextCursor: null }],
      [200, { lessons: [], nextCursor: null }],
      [200, { revisions: [{ id: 1, eligible: true }] }],
      [200, { levels: [] }],
      [200, { section: {}, editions: [] }],
      [200, { id: 4 }],
    ]);
    const courses = courseApi(new Api({ fetch: server.fetch }));
    await courses.attachableLessons(5);
    await courses.attachableLessons(5, 'équation & co', 'n2');
    expect(await courses.eligibleRevisions(101)).toEqual([{ id: 1, eligible: true }]);
    await courses.levels();
    await courses.sectionEditions(3);
    await courses.edition(4);
    expect(server.calls.slice(1).map((c) => c.url)).toEqual([
      '/api/courses/5/attachable-lessons',
      '/api/courses/5/attachable-lessons?q=%C3%A9quation%20%26%20co&cursor=n2',
      '/api/lessons/101/eligible-revisions',
      '/api/catalogue/levels',
      '/api/catalogue/sections/3/editions',
      '/api/catalogue/editions/4',
    ]);
  });

  it('turns a refused save into a typed error carrying the server code and message', async () => {
    const server = fakeServer([[200, session], [422, err('placement_invalid', 'chapter 9 is retired')], [409, err('version_conflict')]]);
    const courses = courseApi(new Api({ fetch: server.fetch }));
    const body = o.toSaveBody(o.fromRevision(revision()));
    expect(await saveCourse(courses, 30, 3, body)).toMatchObject({ kind: 'failed', error: { status: 422, code: 'placement_invalid', message: 'chapter 9 is retired' } });
    expect(await saveCourse(courses, 30, 3, body)).toEqual({ kind: 'conflict' });
  });

  it('finds an owned course across pages and stops once the IDs pass it', async () => {
    const pages = [
      { items: [{ id: 50 }, { id: 40 }], nextCursor: 'p2' },
      { items: [{ id: 30 }, { id: 20 }], nextCursor: 'p3' },
    ];
    const myCourses = vi.fn(async (cursor: string | null) => pages[cursor === null ? 0 : 1]);
    const courses = { myCourses } as unknown as ReturnType<typeof courseApi>;
    expect(await findOwnCourse(courses, 30)).toEqual({ id: 30 });
    expect(await findOwnCourse(courses, 35)).toBeNull();
    expect(myCourses).toHaveBeenCalledTimes(4);
  });
});

describe('course save outcomes', () => {
  const courses = (fail: unknown[]) => ({ save: vi.fn(async () => { const next = fail.shift(); if (next) throw next; return revision({ version: 4 }); }) }) as unknown as ReturnType<typeof courseApi>;
  const body = o.toSaveBody(o.fromRevision(revision()));

  it('classifies conflicts, submitted drafts, expiry and refusals without throwing', async () => {
    expect(await saveCourse(courses([new ApiError(409, 'version_conflict', '')]), 30, 3, body)).toEqual({ kind: 'conflict' });
    expect(await saveCourse(courses([new ApiError(409, 'not_draft', '')]), 30, 3, body)).toEqual({ kind: 'not_draft' });
    expect(await saveCourse(courses([new ApiError(401, 'unauthenticated', '')]), 30, 3, body)).toEqual({ kind: 'expired' });
    const refused = new ApiError(422, 'placement_invalid', 'chapter 9 is in none of the revision\'s curriculum assignments');
    expect(await saveCourse(courses([refused]), 30, 3, body)).toEqual({ kind: 'failed', error: refused });
    const network = new ApiError(0, 'network', 'Network error');
    expect(await saveCourse(courses([network]), 30, 3, body)).toEqual({ kind: 'failed', error: network });
  });

  it('repeats a write once after 409 retry, never twice', async () => {
    expect((await saveCourse(courses([new ApiError(409, 'retry', '')]), 30, 3, body)).kind).toBe('saved');
    const twice = await saveCourse(courses([new ApiError(409, 'retry', ''), new ApiError(409, 'retry', '')]), 30, 3, body);
    expect(twice).toMatchObject({ kind: 'failed' });
  });
});

describe('course error messages', () => {
  it('explains server refusals in French with the server detail for input errors', () => {
    expect(describeCourseError(new ApiError(422, 'placement_invalid', 'chapter 9 is retired'))).toMatch(/^Classement refusé.*Détail du serveur : chapter 9 is retired$/);
    expect(describeCourseError(new ApiError(422, 'invalid_step', 'module 1, step 2: lesson 4 is attached twice'))).toMatch(/^Une étape ne peut pas être rattachée.*attached twice$/);
    expect(describeCourseError(new ApiError(422, 'invalid_input', 'title must be plain text'))).toMatch(/^Un champ du cours est refusé/);
    expect(describeCourseError(new ApiError(409, 'active_candidate_exists', 'x'))).toMatch(/^Le cours a déjà une version en cours/);
    expect(describeCourseError(new ApiError(409, 'has_history', 'x'))).toMatch(/ne peut pas être supprimé/);
    expect(describeCourseError(new ApiError(404, 'not_found', 'x'))).toBe('Introuvable : ce cours ou ce brouillon n’existe pas ou ne vous appartient pas.');
    expect(describeCourseError(new ApiError(409, 'version_conflict', 'x'))).not.toMatch(/Détail/);
    // Shared codes fall back to the authoring messages.
    expect(describeCourseError(new ApiError(0, 'network', ''))).toMatch(/^Le serveur ne répond pas/);
    expect(describeCourseError(new Error('boom'))).toBe('Erreur inattendue dans le Studio.');
  });
});

describe('course outline', () => {
  it('converts a revision and back without losing order, keys or pinned revisions', () => {
    const draft = o.fromRevision(revision({ coverMediaId: 12, coverAlt: ' TEST alt ' }));
    expect(o.toSaveBody(draft)).toEqual({
      title: 'TEST cours',
      summary: '',
      objectives: '',
      prerequisites: '',
      coverMediaId: 12,
      coverAlt: 'TEST alt',
      assignmentIds: [11],
      modules: [
        { key: 'aaaaaaaa-0000-4000-8000-000000000001', title: 'TEST module A', chapterId: 9, steps: [{ lessonId: 101, revisionId: 1001, title: 'A1' }, { lessonId: 102, revisionId: 1002, title: 'A2' }] },
        { key: 'aaaaaaaa-0000-4000-8000-000000000002', title: 'TEST module B', chapterId: null, steps: [{ lessonId: 103, revisionId: 1003, title: 'B1' }] },
      ],
    });
    // Alt text is never sent without a cover.
    expect(o.toSaveBody({ ...draft, coverMediaId: null }).coverAlt).toBeNull();
  });

  it('moves steps within a module and across module boundaries in both directions', () => {
    let d = o.fromRevision(revision());
    d = o.moveStep(d, stepId(d, 'A2'), -1);
    expect(titles(d)).toEqual([['A2', 'A1'], ['B1']]);
    d = o.moveStep(d, stepId(d, 'A1'), 1); // bottom of A → top of B
    expect(titles(d)).toEqual([['A2'], ['A1', 'B1']]);
    d = o.moveStep(d, stepId(d, 'A1'), -1); // top of B → bottom of A
    expect(titles(d)).toEqual([['A2', 'A1'], ['B1']]);
    const first = stepId(d, 'A2');
    expect(o.moveStep(d, first, -1)).toBe(d); // nowhere to go
    expect(o.stepMoves(d, first)).toEqual({ up: false, down: true });
    expect(o.stepMoves(d, stepId(d, 'B1'))).toEqual({ up: true, down: false });
    d = o.moveStepToModule(d, first, d.modules[1].localId);
    expect(titles(d)).toEqual([['A1'], ['B1', 'A2']]);
  });

  it('moves a step into an empty module and reorders modules', () => {
    let d = o.addModule(o.fromRevision(revision()), 'TEST module C');
    d = o.moveStep(d, stepId(d, 'B1'), 1);
    expect(titles(d)).toEqual([['A1', 'A2'], [], ['B1']]);
    d = o.moveModule(d, 2, -1);
    expect(d.modules.map((m) => m.title)).toEqual(['TEST module A', 'TEST module C', 'TEST module B']);
    expect(o.moveModule(d, 0, -1)).toEqual(d);
  });

  it('adds, updates and removes modules and steps; a lesson appears once', () => {
    let d = o.fromRevision(revision());
    const b = d.modules[1].localId;
    d = o.addStep(d, b, { lessonId: 104, revisionId: 1004, title: 'B2', revisionNumber: 1, ownLesson: true, eligible: false, lessonTitle: 'TEST leçon' });
    expect(titles(d)).toEqual([['A1', 'A2'], ['B1', 'B2']]);
    expect([...o.lessonsInOutline(d)].sort()).toEqual([101, 102, 103, 104]);
    d = o.updateStep(d, stepId(d, 'B2'), { revisionId: 1005 });
    expect(d.modules[1].steps[1].revisionId).toBe(1005);
    d = o.removeStep(d, stepId(d, 'A1'));
    d = o.updateModule(d, b, { title: 'renamed', chapterId: 9 });
    d = o.removeModule(d, d.modules[0].localId);
    expect(o.toSaveBody(d).modules).toEqual([{ key: 'aaaaaaaa-0000-4000-8000-000000000002', title: 'renamed', chapterId: 9, steps: [{ lessonId: 103, revisionId: 1003, title: 'B1' }, { lessonId: 104, revisionId: 1005, title: 'B2' }] }]);
  });

  it('removing a placement clears module chapters no remaining placement provides', () => {
    const d = o.fromRevision(revision());
    const withTwo = { ...d, assignmentIds: [11, 12], modules: [{ ...d.modules[0], chapterId: 9 }, { ...d.modules[1], chapterId: 8 }] };
    const chapters: Record<number, number[]> = { 11: [9], 12: [8, 9] };
    const after = o.removeAssignment(withTwo, 12, (id) => chapters[id] ?? []);
    expect(after.assignmentIds).toEqual([11]);
    expect(after.modules.map((m) => m.chapterId)).toEqual([9, null]);
  });

  it('gives server keys to the modules that were sent, not to modules added during the save', () => {
    let d = o.addModule(o.fromRevision(revision()), 'new');
    const sent = d.modules.map((m) => m.localId);
    d = o.addModule(d, 'added while saving');
    d = o.assignKeys(d, sent, ['k1', 'k2', 'k3']);
    expect(d.modules.map((m) => m.key)).toEqual(['aaaaaaaa-0000-4000-8000-000000000001', 'aaaaaaaa-0000-4000-8000-000000000002', 'k3', null]);
  });

  it('tracks unsaved changes by content, ignoring local IDs, server keys and surrounding spaces', () => {
    const a = o.fromRevision(revision());
    const b = o.fromRevision(revision());
    expect(o.snapshot(a)).toBe(o.snapshot(b));
    expect(o.snapshot({ ...a, title: ' TEST cours ' })).toBe(o.snapshot(a));
    expect(o.snapshot({ ...a, modules: a.modules.map((m) => ({ ...m, key: null })) })).toBe(o.snapshot(a));
    expect(o.snapshot(o.moveStep(a, stepId(a, 'A2'), -1))).not.toBe(o.snapshot(a));
  });

  it('checks the contract limits before sending, and accepts an incomplete draft', () => {
    const empty: o.CourseDraft = { title: '', summary: '', objectives: '', prerequisites: '', coverMediaId: null, coverAlt: '', assignmentIds: [], modules: [] };
    expect(o.checkDraft(empty)).toEqual([]);
    expect(o.checkDraft({ ...empty, title: 'x'.repeat(200), summary: 'é'.repeat(1000) })).toEqual([]);
    const problems = o.checkDraft({
      ...empty,
      title: 'x'.repeat(201),
      summary: 'line\nline',
      objectives: 'tab\there',
      coverMediaId: 4,
      coverAlt: '  ',
      modules: [{ localId: 'm1', key: null, title: ' ', chapterId: null, steps: [{ localId: 's1', lessonId: 1, revisionId: 1, title: 'a‮b', revisionNumber: 1, ownLesson: true, eligible: true, lessonTitle: null }] }],
    });
    expect(problems.map((p) => p.target)).toEqual(['title', 'objectives', 'coverAlt', 'module:m1', 'step:s1']);
    expect(problems[0].message).toMatch(/200 caractères au maximum/);
    const many = { ...empty, modules: Array.from({ length: 51 }, (_, i) => ({ localId: `m${i}`, key: null, title: `M${i}`, chapterId: null, steps: [] })) };
    expect(o.checkDraft(many).map((p) => p.target)).toEqual(['modules']);
  });

  it('folds accents and case for title search', () => {
    expect(o.fold('Équations DIFFÉRENTIELLES')).toBe('equations differentielles');
  });
});

describe('lesson picker defaults', () => {
  it('pins the newest eligible revision, else the newest one', () => {
    const r = (id: number, eligible: boolean) => ({ id, number: id, status: 'approved' as const, title: '', eligible });
    expect(defaultRevision([r(3, false), r(2, true), r(1, true)])?.id).toBe(2);
    expect(defaultRevision([r(3, false), r(2, false)])?.id).toBe(3);
    expect(defaultRevision([])).toBeNull();
  });
});

describe('course labels and routes', () => {
  it('writes the state out, publication included', () => {
    const latest = { id: 30, number: 1, status: 'approved' as const, title: '', version: 1, updatedAt: '' };
    const course = { id: 5, publicId: '', ownerId: 7, subjectId: 1, language: 'fr' as const, publishedRevisionId: null, archived: false, createdAt: '' };
    expect(courseState(course).label).toBe('Aucune version en cours');
    expect(courseState({ ...course, latestRevision: { ...latest, status: 'draft' } }).label).toBe('Brouillon');
    expect(courseState({ ...course, latestRevision: latest }).tone).toBe('approved');
    expect(courseState({ ...course, publishedRevisionId: 30, latestRevision: latest }).label).toBe('Publié');
  });

  it('parses course routes, none with a dot in the last segment', () => {
    expect(parseRoute('/studio/courses')).toEqual({ name: 'courses' });
    expect(parseRoute('/studio/courses/')).toEqual({ name: 'courses' });
    expect(parseRoute('/studio/courses/new')).toEqual({ name: 'newCourse' });
    expect(parseRoute('/studio/courses/42')).toEqual({ name: 'course', courseId: 42 });
    expect(parseRoute('/studio/courses/042')).toEqual({ name: 'notFound' });
    expect(parseRoute('/studio/courses/4.2')).toEqual({ name: 'notFound' });
    expect(routePath({ name: 'course', courseId: 42 })).toBe('/studio/courses/42');
  });
});

describe('course recovery copies', () => {
  beforeEach(() => localStorage.clear());
  const draft = (title: string) => ({ ...o.fromRevision(revision()), title });
  const target = (userId: number, over: Partial<{ revisionId: number; baseVersion: number; instance: string; courseId: number }> = {}) => ({ userId, courseId: 5, revisionId: 30, baseVersion: 3, revisionNumber: 1, instance: 'a', ...over });

  it('keys copies by user, course, revision, base version and tab', () => {
    expect(courseRecoveryKey(target(7))).toBe(`${COURSE_RECOVERY_PREFIX}u7:c5:r30:v3:a`);
  });

  it('lists only the writing user’s copies of the course, newest first, and survives a reload of the draft', () => {
    writeCourseRecovery(localStorage, target(7), draft('v3 tab a'), 'conflict', () => new Date('2026-11-26T10:00:00Z'));
    writeCourseRecovery(localStorage, target(7, { baseVersion: 4, instance: 'b' }), draft('v4 tab b'), 'unsaved', () => new Date('2026-11-26T11:00:00Z'));
    writeCourseRecovery(localStorage, target(7, { courseId: 6 }), draft('other course'));
    writeCourseRecovery(localStorage, target(8), draft('other user'));
    const found = findCourseRecovery(localStorage, 7, 5);
    expect(found.map((e) => e.data.title)).toEqual(['v4 tab b', 'v3 tab a']);
    expect(found[1].reason).toBe('conflict');
    // The restored data keeps the outline exactly.
    expect(o.toSaveBody(found[0].data)).toEqual(o.toSaveBody(draft('v4 tab b')));
    expect(findCourseRecovery(localStorage, 7, 5, found[0].key).map((e) => e.data.title)).toEqual(['v3 tab a']);
    // An account switch: the other user sees only their own copy.
    expect(findUserCourseRecovery(localStorage, 8).map((e) => e.data.title)).toEqual(['other user']);
  });

  it('ignores tampered entries, removes only its own keys, and purges on sign-out and by age', () => {
    localStorage.setItem(`${COURSE_RECOVERY_PREFIX}u7:c5:r30:v1:x`, '{bad');
    localStorage.setItem(`${COURSE_RECOVERY_PREFIX}u7:c5:r30:v2:y`, JSON.stringify({ key: 'elsewhere', userId: 7, courseId: 5, instance: 'y', data: draft('x') }));
    localStorage.setItem(`${COURSE_RECOVERY_PREFIX}u7:c5:r30:v3:z`, JSON.stringify({ key: `${COURSE_RECOVERY_PREFIX}u7:c5:r30:v3:z`, userId: 7, courseId: 5, instance: 'z', data: { ...draft('x'), modules: [{ steps: 'no' }] } }));
    localStorage.setItem('learnoo-studio-recovery:v1:u7:new:a', 'lesson copy');
    expect(findCourseRecovery(localStorage, 7, 5)).toEqual([]);
    removeCourseRecovery(localStorage, 'learnoo-studio-recovery:v1:u7:new:a');
    expect(localStorage.getItem('learnoo-studio-recovery:v1:u7:new:a')).toBe('lesson copy');

    writeCourseRecovery(localStorage, target(7), draft('old'), 'unsaved', () => new Date('2026-11-01T00:00:00Z'));
    writeCourseRecovery(localStorage, target(7, { instance: 'n' }), draft('new'), 'unsaved', () => new Date('2026-11-26T00:00:00Z'));
    writeCourseRecovery(localStorage, target(8), draft('user 8'));
    purgeCourseRecovery(localStorage, { olderThan: new Date('2026-11-20T00:00:00Z') });
    expect(findUserCourseRecovery(localStorage, 7).map((e) => e.data.title)).toEqual(['new']);
    expect(localStorage.getItem(`${COURSE_RECOVERY_PREFIX}u7:c5:r30:v1:x`)).toBeNull();
    purgeCourseRecovery(localStorage, { userId: 7 });
    expect(findUserCourseRecovery(localStorage, 7)).toEqual([]);
    expect(findUserCourseRecovery(localStorage, 8)).toHaveLength(1);
    expect(localStorage.getItem('learnoo-studio-recovery:v1:u7:new:a')).toBe('lesson copy');
  });
});
