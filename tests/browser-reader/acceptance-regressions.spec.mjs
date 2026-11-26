import { test, expect } from '@playwright/test';
import { apiCourse, apiSession, author, authorTwo, closeFixture, password, saveState, setup, signIn, watch } from './course-fixture.mjs';

// Regression tests for two acceptance gaps (docs/LMS_ACCEPTANCE.md): the
// course editor's account switch on a shared browser (LMS-A03) and the return
// to a course step after signing in from its deep link (LMS-A01). TEST
// accounts and content of internal/server TestBrowserReaderServer; every
// course created here is titled "TEST …". Desktop Chromium.
const student = { email: 'test-course-student@example.test', password };
const COURSE_RECOVERY = 'learnoo-studio-course-recovery:v1:';
const courseCopies = (page) => page.evaluate((prefix) => Object.keys(localStorage).filter((k) => k.startsWith(prefix)), COURSE_RECOVERY);
const saveButton = (page) => page.getByRole('button', { name: 'Enregistrer', exact: true });
/** Each of the user's courses with its latest revision and version, to show that nothing changed. */
const snapshot = async (session) =>
  (await session.call('GET', '/api/courses?mine=1')).courses.map((c) => ({ id: c.id, revision: c.latestRevision?.id ?? null, version: c.latestRevision?.version ?? null }));

test('a course draft open in one tab never writes or shows anything as another account signed in on the same browser; the author resumes and saves', async ({ page, context, browser, baseURL }) => {
  const problems = watch(page, baseURL);
  const f = await setup(browser, baseURL);
  const a = await apiSession(browser, baseURL, author);
  const b = await apiSession(browser, baseURL, authorTwo);
  const { course, revision } = await apiCourse(a, f.subject.id, { title: 'TEST Cours ouvert au changement de compte', modules: [{ key: null, title: 'TEST Module enregistré', chapterId: null, steps: [] }] });
  const { course: theirs } = await apiCourse(b, f.subject.id, { title: 'TEST Cours de l’autre auteur' });
  const theirsBefore = await snapshot(b);

  // Author A opens the draft and edits it without saving; the copy is kept under A's key.
  await signIn(page, author, `/studio/courses/${course.id}`);
  await expect(page.getByLabel('Titre du cours')).toHaveValue('TEST Cours ouvert au changement de compte');
  await page.getByLabel('Titre du cours').fill('TEST Titre non enregistré de A');
  await page.getByLabel('Titre du nouveau module').fill('TEST Module non enregistré de A');
  await page.getByRole('button', { name: 'Ajouter le module' }).click();
  const mine = `${COURSE_RECOVERY}u${a.me.id}:c${course.id}:`;
  await expect.poll(() => courseCopies(page)).toEqual([expect.stringMatching(new RegExp(`^${mine}`))]);

  // In another tab A signs out (asked first: unsaved copies exist), which removes A's copies; B signs in.
  const other = await context.newPage();
  await other.goto('/studio');
  await expect(other.locator('.who')).toBeVisible();
  await other.getByRole('button', { name: 'Se déconnecter' }).click();
  await other.getByRole('dialog', { name: 'Se déconnecter sans enregistrer ?' }).getByRole('button', { name: 'Se déconnecter quand même' }).click();
  await expect(other).toHaveURL(/\/login$/);
  expect(await courseCopies(other)).toEqual([]);
  await signIn(other, authorTwo, '/studio/courses');
  await expect(other.locator(`a[href="/studio/courses/${theirs.id}"]`)).toHaveText('TEST Cours de l’autre auteur');

  // A's tab tries to save: no write is accepted under B's session, the edits stay on screen,
  // and the copy is kept again under A's key only.
  const writes = [];
  page.on('response', async (r) => {
    if (r.request().method() === 'GET' || !new URL(r.url()).pathname.startsWith('/api/')) return;
    const w = { path: new URL(r.url()).pathname, status: r.status(), code: null };
    writes.push(w);
    w.code = (await r.json().catch(() => ({}))).error?.code ?? null;
  });
  await saveButton(page).click();
  const banner = page.locator('.banner.expired');
  await expect(banner).toContainText('Un autre compte est connecté dans ce navigateur.');
  await expect(page.getByRole('alert').filter({ hasText: 'Un autre compte est connecté' }).first()).toBeVisible();
  await expect(page.getByLabel('Titre du cours')).toHaveValue('TEST Titre non enregistré de A');
  await expect(page.getByLabel('Titre du module 2')).toHaveValue('TEST Module non enregistré de A');
  await saveButton(page).click();
  await expect(page.getByText(/Rien n’a été envoyé : un autre compte est connecté dans ce navigateur/)).toBeVisible();
  // The tab has not yet noticed the switch when the first save is clicked: that one request
  // carries B's session cookie with A's CSRF token and is refused (403 csrf_rejected) before
  // anything is written; the client then reads the session, sees B and sends nothing more.
  for (const w of writes) expect(w).toEqual({ path: `/api/course-revisions/${revision.id}`, status: 403, code: 'csrf_rejected' });
  expect(writes.length).toBeLessThanOrEqual(1);
  const copies = await courseCopies(page);
  expect(copies.length).toBeGreaterThan(0);
  for (const k of copies) expect(k.startsWith(mine)).toBe(true);
  const kept = await page.evaluate((k) => JSON.parse(localStorage.getItem(k)), copies[0]);
  expect(kept.userId).toBe(a.me.id);
  expect(kept.data.title).toBe('TEST Titre non enregistré de A');
  // Nothing changed for either account.
  expect(await snapshot(b)).toEqual(theirsBefore);
  const unchanged = await a.call('GET', `/api/course-revisions/${revision.id}`);
  expect({ title: unchanged.title, version: unchanged.version }).toEqual({ title: 'TEST Cours ouvert au changement de compte', version: revision.version });

  // A's tab shows nothing of B's: still A's header and draft, none of B's courses.
  await expect(page.locator('.who')).toHaveText(a.me.displayName);
  await expect(page.getByText('TEST Cours de l’autre auteur')).toHaveCount(0);

  // B sees neither A's draft nor A's unsaved edits, and is offered no restore of them.
  await expect(other.getByText(/TEST (Cours ouvert au changement de compte|Titre non enregistré de A)/)).toHaveCount(0);
  await other.goto(`/studio/courses/${course.id}`);
  await expect(other.getByRole('alert')).toContainText('Cours introuvable : il n’existe pas ou ne vous appartient pas.');
  await expect(other.getByLabel('Titre du cours')).toHaveCount(0);
  await other.goto(`/studio/courses/${theirs.id}`);
  await expect(other.getByLabel('Titre du cours')).toHaveValue('TEST Cours de l’autre auteur');
  await expect(other.getByRole('region', { name: 'Copies de secours locales (non enregistrées)' })).toHaveCount(0);
  await expect(other.getByText('TEST Titre non enregistré de A')).toHaveCount(0);
  expect((await b.raw('GET', `/api/course-revisions/${revision.id}`)).status()).toBe(404);
  for (const k of await courseCopies(other)) expect(k.startsWith(mine)).toBe(true);

  // Resuming while B is signed in is refused; once A is back, the tab resumes and saves as A.
  await page.getByRole('button', { name: 'Reprendre ici' }).click();
  await expect(banner).toContainText('Un autre compte est toujours connecté');
  await other.getByRole('button', { name: 'Se déconnecter' }).click();
  await expect(other).toHaveURL(/\/login$/);
  await signIn(other, author, '/studio/courses');
  await expect(other.locator(`a[href="/studio/courses/${course.id}"]`)).toHaveText('TEST Cours ouvert au changement de compte');
  await page.getByRole('button', { name: 'Reprendre ici' }).click();
  await expect(banner).toHaveCount(0);
  await saveButton(page).click();
  await expect(saveState(page)).toHaveText(/Enregistré sur le serveur/);
  const saved = await a.call('GET', `/api/course-revisions/${revision.id}`);
  expect(saved.title).toBe('TEST Titre non enregistré de A');
  expect(saved.modules.map((m) => m.title)).toEqual(['TEST Module enregistré', 'TEST Module non enregistré de A']);
  expect(saved.version).toBe(revision.version + 1);
  await expect.poll(() => courseCopies(page)).toEqual([]);
  expect(await snapshot(b)).toEqual(theirsBefore);
  expect(problems).toEqual([]);
  await other.close();
  await a.ctx.close();
  await b.ctx.close();
});

/** A published course's third step, found by a signed-in student in a separate browser context. */
async function courseStepThree(browser, baseURL) {
  const ctx = await browser.newContext({ baseURL });
  const p = await ctx.newPage();
  await p.goto('/login?next=/learn');
  await p.getByLabel('Adresse e-mail').fill(student.email);
  await p.getByLabel('Mot de passe').fill(student.password);
  await p.getByRole('button', { name: 'Se connecter' }).click();
  await expect(p).toHaveURL(/\/learn$/);
  await p.locator('section[aria-labelledby="courses-heading"]').getByRole('link', { name: 'TEST Cours de nombres complexes' }).click();
  const href = await p.getByRole('navigation', { name: 'Plan du cours' }).getByRole('link', { name: 'TEST Étape trois' }).getAttribute('href');
  await ctx.close();
  return href;
}

/** Opens path signed out: sent to the sign-in page without the lesson body, then back to path after signing in. */
async function signInFromDeepLink(page, path, body) {
  const direct = await page.request.get(path, { maxRedirects: 0 });
  expect(direct.status()).toBe(303);
  expect(direct.headers()['location']).toBe(`/login?next=${encodeURIComponent(path)}`);
  expect(await direct.text()).not.toContain(body);
  await page.goto(path);
  expect(new URL(page.url()).pathname).toBe('/login');
  expect(new URL(page.url()).searchParams.get('next')).toBe(path);
  await expect(page.getByText(body)).toHaveCount(0);
  await expect(page.locator('input[name="next"]')).toHaveValue(path);
  await page.getByLabel('Adresse e-mail').fill(student.email);
  await page.getByLabel('Mot de passe').fill(student.password);
  await Promise.all([page.waitForURL(`**${path}`), page.getByRole('button', { name: 'Se connecter' }).click()]);
  expect(new URL(page.url()).pathname).toBe(path);
  await expect(page.getByText(body)).toBeVisible();
}

test('signing in from a course-step or lesson deep link returns to that step or lesson', async ({ page, browser, baseURL }) => {
  const external = [];
  page.on('request', (r) => {
    if (new URL(r.url()).origin !== new URL(baseURL).origin) external.push(r.url());
  });
  const step = await courseStepThree(browser, baseURL);
  expect(step).toMatch(/^\/learn\/courses\/[0-9a-f-]{36}\/lessons\/[0-9a-f-]{36}$/);
  const body = 'Corps de la leçon de cours trois.';

  // The third step of the course, not the course outline, its first step or /learn.
  await signInFromDeepLink(page, step, body);
  await expect(page.getByText('Leçon 3 sur 3')).toBeVisible();
  await expect(page.locator('#plan-du-cours [aria-current="page"]')).toHaveText('TEST Étape trois');

  // The same lesson on its own page.
  await page.context().clearCookies();
  const lesson = `/learn/lessons/${step.split('/').pop()}`;
  await signInFromDeepLink(page, lesson, body);
  await expect(page.getByRole('heading', { level: 1, name: 'TEST Leçon de cours trois' })).toBeVisible();
  expect(external).toEqual([]);
});

test.afterAll(closeFixture);
