// Shared by the course browser specs: TEST accounts, API sessions and the
// TEST curriculum, created through the administrator API once per server.
import { expect } from '@playwright/test';

export const password = 'simulated-password-1';
export const author = { email: 'test-author@example.test', password };
export const authorTwo = { email: 'test-author-two@example.test', password };
export const admin = { email: 'test-admin@example.test', password };

export function watch(page, baseURL) {
  const problems = [];
  page.on('console', (m) => {
    if (/Content Security Policy|Refused to/i.test(m.text())) problems.push(m.text());
  });
  page.on('request', (r) => {
    if (new URL(r.url()).origin !== new URL(baseURL).origin) problems.push(`external request ${r.url()}`);
  });
  return problems;
}

export async function signIn(page, user, path = '/studio') {
  await page.goto(path);
  await expect(page).toHaveURL(/\/login\?next=/);
  await page.getByLabel('Adresse e-mail').fill(user.email);
  await page.getByLabel('Mot de passe').fill(user.password);
  await page.getByRole('button', { name: 'Se connecter' }).click();
}

export async function apiSession(browser, baseURL, user) {
  const ctx = await browser.newContext({ baseURL });
  const login = await ctx.request.post('/api/auth/login', { data: user });
  expect(login.status()).toBe(200);
  const { csrfToken, user: me } = await login.json();
  const call = async (method, path, data) => {
    const r = await ctx.request.fetch(path, { method, data, headers: data === undefined ? {} : { 'X-CSRF-Token': csrfToken } });
    if (!r.ok()) throw new Error(`${method} ${path}: ${r.status()} ${await r.text()}`);
    return r.status() === 204 ? null : r.json();
  };
  const raw = (method, path, data) => ctx.request.fetch(path, { method, data, headers: { 'X-CSRF-Token': csrfToken } });
  return { ctx, me, call, raw };
}

export const noOverflow = (page) => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth);
export const saveState = (page) => page.locator('.save-state');
export const doc = (title) => ({ schemaVersion: 1, title, language: 'fr', status: 'unreviewed', objective: '', blocks: [{ type: 'paragraph', children: [{ type: 'text', text: 'TEST contenu.' }] }] });

/**
 * The TEST catalogue facts and a draft lesson of test-author, created once
 * per server. Playwright starts a new worker (and module state) after a
 * failed test, so existing entries are found again rather than recreated.
 */
let fixture = null;
export async function setup(browser, baseURL) {
  if (fixture) return fixture;
  const ad = await apiSession(browser, baseURL, admin);
  const au = await apiSession(browser, baseURL, author);
  const { subjects } = await au.call('GET', '/api/catalogue/subjects');
  const subject = subjects.find((s) => s.code === 'test-math');
  const { chapters } = await au.call('GET', '/api/catalogue/subjects/test-math/chapters');
  const chapter = chapters.find((c) => c.code === 'test-chapter');
  const retirable = chapters.find((c) => c.code === 'test-chapter-retirable');
  const base = '/api/admin/curriculum';
  // Chapters not retired at the time an edition is built (a spec retires test-chapter-retirable).
  const openChapterIds = async () =>
    (await au.call('GET', '/api/catalogue/subjects/test-math/chapters')).chapters.filter((c) => !c.retiredAt).map((c) => c.id);
  // test-bac may already exist (the Go fixture's public catalogue seeds it with
  // a test-maths section), so this section has a name of its own.
  const existing = (await ad.call('GET', '/api/catalogue/levels')).levels.find((l) => l.code === 'test-bac');
  const level = existing ?? (await ad.call('POST', `${base}/levels`, { code: 'test-bac', nameFr: 'TEST Baccalauréat', nameAr: 'TEST بكالوريا' }));
  const section =
    existing?.sections.find((x) => x.code === 'test-section-maths') ??
    (await ad.call('POST', `${base}/levels/${level.id}/sections`, { code: 'test-section-maths', nameFr: 'TEST Section sciences mathématiques', nameAr: 'TEST شعبة العلوم الرياضية' }));
  const editions = (await ad.call('GET', `/api/catalogue/sections/${section.id}/editions`)).editions;
  const edition = async (code, label, expectedCurrentEditionId) => {
    const found = editions.find((x) => x.code === code);
    if (found) return found;
    let e = await ad.call('POST', `${base}/sections/${section.id}/editions`, { code, labelFr: label, labelAr: `${label} ع` });
    e = await ad.call('POST', `${base}/editions/${e.id}/assignments`, { expectedVersion: e.version, subjectId: subject.id });
    const assignment = e.assignments.find((a) => a.subjectId === subject.id);
    e = await ad.call('PUT', `${base}/editions/${e.id}/assignments/${assignment.id}/chapters`, { expectedVersion: e.version, chapterIds: await openChapterIds() });
    const made = await ad.call('POST', `${base}/editions/${e.id}/make-current`, { expectedVersion: e.version, expectedCurrentEditionId });
    return { ...made.edition, assignmentId: assignment.id };
  };
  const first = await edition('test-2026', 'TEST édition 2026', null);
  // A draft lesson of the author's own: attachable to a draft, not yet publishable in a course.
  const own = (await au.call('GET', '/api/lessons/mine')).lessons.find((l) => l.latestRevision?.title === 'TEST Leçon de cours en brouillon');
  const lessonDraft = own
    ? { id: own.latestRevision.id, lessonId: own.id }
    : await au.call('POST', '/api/lessons', { subjectId: subject.id, language: 'fr', content: doc('TEST Leçon de cours en brouillon') });
  /** The section's current edition and its assignment of the TEST subject. */
  const current = async () => {
    const list = (await ad.call('GET', `/api/catalogue/sections/${section.id}/editions`)).editions;
    const e = await ad.call('GET', `/api/catalogue/editions/${list.find((x) => x.state === 'current').id}`);
    return { ...e, assignmentId: e.assignments.find((a) => a.subjectId === subject.id).id };
  };
  fixture = { ad, subject, chapter, retirable, first, lessonDraft, current, newEdition: (code, label, expected) => edition(code, label, expected), close: async () => { await ad.ctx.close(); await au.ctx.close(); } };
  return fixture;
}

/** A course created and saved through the API, for the tests that do not exercise creation. */
export async function apiCourse(au, subjectId, body) {
  const { course, revision } = await au.call('POST', '/api/courses', { subjectId, language: 'fr' });
  const saved = await au.call('PUT', `/api/course-revisions/${revision.id}`, {
    expectedVersion: revision.version, title: '', summary: '', objectives: '', prerequisites: '', assignmentIds: [], modules: [], ...body,
  });
  return { course, revision: saved };
}

export const closeFixture = async () => {
  await fixture?.close();
  fixture = null;
};
