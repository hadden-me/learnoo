import { test, expect } from '@playwright/test';

// Studio course drafts (LMS-B05, author side) against the Go application and
// PostgreSQL (internal/server TestBrowserReaderServer), under the real CSP.
// Accounts are the fixture's simulated TEST users; the TEST curriculum
// (level, section, editions) is created here through the administrator API,
// and every course and lesson these tests create is titled "TEST …".
// Desktop Chromium; the 360px check emulates a phone viewport, not a phone.
const password = 'simulated-password-1';
const author = { email: 'test-author@example.test', password };
const authorTwo = { email: 'test-author-two@example.test', password };
const admin = { email: 'test-admin@example.test', password };

function watch(page, baseURL) {
  const problems = [];
  page.on('console', (m) => {
    if (/Content Security Policy|Refused to/i.test(m.text())) problems.push(m.text());
  });
  page.on('request', (r) => {
    if (new URL(r.url()).origin !== new URL(baseURL).origin) problems.push(`external request ${r.url()}`);
  });
  return problems;
}

async function signIn(page, user, path = '/studio') {
  await page.goto(path);
  await expect(page).toHaveURL(/\/login\?next=/);
  await page.getByLabel('Adresse e-mail').fill(user.email);
  await page.getByLabel('Mot de passe').fill(user.password);
  await page.getByRole('button', { name: 'Se connecter' }).click();
}

async function apiSession(browser, baseURL, user) {
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

const noOverflow = (page) => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth);
const saveState = (page) => page.locator('.save-state');
const doc = (title) => ({ schemaVersion: 1, title, language: 'fr', status: 'unreviewed', objective: '', blocks: [{ type: 'paragraph', children: [{ type: 'text', text: 'TEST contenu.' }] }] });

/**
 * The TEST catalogue facts and a draft lesson of test-author, created once
 * per server. Playwright starts a new worker (and module state) after a
 * failed test, so existing entries are found again rather than recreated.
 */
let fixture = null;
async function setup(browser, baseURL) {
  if (fixture) return fixture;
  const ad = await apiSession(browser, baseURL, admin);
  const au = await apiSession(browser, baseURL, author);
  const { subjects } = await au.call('GET', '/api/catalogue/subjects');
  const subject = subjects.find((s) => s.code === 'test-math');
  const { chapters } = await au.call('GET', '/api/catalogue/subjects/test-math/chapters');
  const chapter = chapters.find((c) => c.code === 'test-chapter');
  const base = '/api/admin/curriculum';
  const existing = (await ad.call('GET', '/api/catalogue/levels')).levels.find((l) => l.code === 'test-bac');
  const level = existing ?? (await ad.call('POST', `${base}/levels`, { code: 'test-bac', nameFr: 'TEST Baccalauréat', nameAr: 'TEST بكالوريا' }));
  const section =
    existing?.sections.find((x) => x.code === 'test-section-maths') ??
    (await ad.call('POST', `${base}/levels/${level.id}/sections`, { code: 'test-section-maths', nameFr: 'TEST Section mathématiques', nameAr: 'TEST شعبة الرياضيات' }));
  const editions = (await ad.call('GET', `/api/catalogue/sections/${section.id}/editions`)).editions;
  const edition = async (code, label, expectedCurrentEditionId) => {
    const found = editions.find((x) => x.code === code);
    if (found) return found;
    let e = await ad.call('POST', `${base}/sections/${section.id}/editions`, { code, labelFr: label, labelAr: `${label} ع` });
    e = await ad.call('POST', `${base}/editions/${e.id}/assignments`, { expectedVersion: e.version, subjectId: subject.id });
    const assignment = e.assignments.find((a) => a.subjectId === subject.id);
    e = await ad.call('PUT', `${base}/editions/${e.id}/assignments/${assignment.id}/chapters`, { expectedVersion: e.version, chapterIds: [chapter.id] });
    const made = await ad.call('POST', `${base}/editions/${e.id}/make-current`, { expectedVersion: e.version, expectedCurrentEditionId });
    return { ...made.edition, assignmentId: assignment.id };
  };
  const first = await edition('test-2026', 'TEST édition 2026', null);
  // A draft lesson of the author's own: attachable to a draft, not yet publishable in a course.
  const own = (await au.call('GET', '/api/lessons/mine')).lessons.find((l) => l.latestRevision?.title === 'TEST Leçon de cours en brouillon');
  const lessonDraft = own
    ? { id: own.latestRevision.id, lessonId: own.id }
    : await au.call('POST', '/api/lessons', { subjectId: subject.id, language: 'fr', content: doc('TEST Leçon de cours en brouillon') });
  fixture = { subject, chapter, first, lessonDraft, newEdition: (code, label, expected) => edition(code, label, expected), close: async () => { await ad.ctx.close(); await au.ctx.close(); } };
  return fixture;
}

/** A course created and saved through the API, for the tests that do not exercise creation. */
async function apiCourse(au, subjectId, body) {
  const { course, revision } = await au.call('POST', '/api/courses', { subjectId, language: 'fr' });
  const saved = await au.call('PUT', `/api/course-revisions/${revision.id}`, {
    expectedVersion: revision.version, title: '', summary: '', objectives: '', prerequisites: '', assignmentIds: [], modules: [], ...body,
  });
  return { course, revision: saved };
}

test('author creates a course, places it, builds the outline by keyboard, saves, signs out and in, and reopens it', async ({ page, browser, baseURL }) => {
  const f = await setup(browser, baseURL);
  await page.setViewportSize({ width: 1280, height: 900 });
  const problems = watch(page, baseURL);
  await signIn(page, author);
  await page.getByRole('link', { name: 'Mes cours', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Mes cours' })).toBeFocused();

  // Create: subject and language, then the empty server draft opens.
  await page.getByRole('link', { name: 'Nouveau cours' }).click();
  await page.getByLabel('Matière').selectOption({ label: 'TEST Mathématiques' });
  await page.getByRole('radio', { name: /Français/ }).check();
  await page.getByRole('button', { name: 'Créer le cours' }).click();
  await expect(page).toHaveURL(/\/studio\/courses\/\d+$/);
  const courseId = Number(new URL(page.url()).pathname.split('/').pop());
  await expect(page.getByText('Cours créé avec un brouillon vide')).toBeVisible();
  await expect(saveState(page)).toHaveText('À jour avec le serveur (version 1).');

  await page.getByLabel('Titre du cours').fill('TEST Cours des nombres complexes');
  await page.getByLabel('Description courte (publique)').fill('TEST Un parcours court.');
  await expect(saveState(page)).toHaveText('Modifications non enregistrées sur le serveur.');

  // Placement: level → section → the current edition, chosen with the keyboard.
  await page.getByLabel('Niveau').selectOption({ label: 'TEST Baccalauréat' });
  await page.getByLabel('Section', { exact: true }).selectOption({ label: 'TEST Section mathématiques' });
  const addEdition = page.getByRole('button', { name: 'Ajouter l’édition en vigueur « TEST édition 2026 »' });
  await addEdition.focus();
  await page.keyboard.press('Enter');
  const placements = page.getByRole('list', { name: 'Classements choisis' });
  await expect(placements).toContainText('TEST Baccalauréat › TEST Section mathématiques › TEST édition 2026');
  await expect(placements).toContainText('édition en vigueur');

  // Modules: typed and added with Enter.
  const newModule = page.getByLabel('Titre du nouveau module');
  await newModule.focus();
  await page.keyboard.type('TEST Module un');
  await page.keyboard.press('Enter');
  await expect(page.getByLabel('Titre du module 1')).toBeFocused();
  await newModule.focus();
  await page.keyboard.type('TEST Module deux');
  await page.keyboard.press('Enter');
  await page.getByLabel('Chapitre officiel (facultatif)').first().selectOption({ label: 'TEST Nombres complexes' });

  // Steps: the lesson search and revision choice, all from the keyboard.
  const addStep = async (search, result) => {
    await page.getByRole('button', { name: 'Ajouter une leçon au module 1 : TEST Module un' }).focus();
    await page.keyboard.press('Enter');
    const dialog = page.getByRole('dialog', { name: 'Ajouter une leçon au module « TEST Module un »' });
    await expect(dialog.getByRole('searchbox')).toBeFocused();
    await page.keyboard.type(search);
    await page.keyboard.press('Enter');
    const choice = dialog.getByRole('button', { name: result });
    await choice.focus();
    await page.keyboard.press('Enter');
    await expect(dialog.getByLabel('Révision à épingler')).toBeFocused();
    return dialog;
  };
  let dialog = await addStep('equations', /TEST Équations et nombres complexes · votre leçon/);
  await expect(dialog.getByLabel('Révision à épingler')).toContainText('publiable dans un cours');
  await expect(dialog.getByLabel('Titre de l’étape dans le cours')).toHaveValue('TEST Équations et nombres complexes');
  await dialog.getByRole('button', { name: 'Ajouter l’étape' }).focus();
  await page.keyboard.press('Enter');
  await expect(dialog).toHaveCount(0);
  await expect(page.getByLabel('Titre de l’étape 1 du module 1')).toBeFocused();

  dialog = await addStep('brouillon', /TEST Leçon de cours en brouillon · votre leçon/);
  // A lesson already in the outline cannot be added twice.
  await dialog.getByRole('searchbox').fill('');
  await dialog.getByRole('button', { name: 'Rechercher' }).click();
  await expect(dialog.getByRole('button', { name: /TEST Équations et nombres complexes.*déjà dans le cours/ })).toBeDisabled();
  await expect(dialog.getByText('devra être approuvée et publiée avant la soumission du cours')).toBeVisible();
  await dialog.getByLabel('Titre de l’étape dans le cours').fill('TEST Étape brouillon');
  await dialog.getByRole('button', { name: 'Ajouter l’étape' }).focus();
  await page.keyboard.press('Enter');
  await expect(page.getByLabel('Titre de l’étape 2 du module 1')).toHaveValue('TEST Étape brouillon');
  await expect(page.getByText('Révision pas encore publiable')).toBeVisible();

  // Reorder by keyboard: the second step goes down into module 2; focus follows it; then module 2 goes up.
  await page.getByRole('button', { name: 'Descendre l’étape 2 du module 1 : TEST Étape brouillon' }).focus();
  await page.keyboard.press('Enter');
  const moved = page.getByRole('button', { name: 'Descendre l’étape 1 du module 2 : TEST Étape brouillon' });
  await expect(moved).toBeDisabled();
  await expect(page.getByRole('button', { name: 'Monter l’étape 1 du module 2 : TEST Étape brouillon' })).toBeFocused();
  await expect(page.getByRole('status').filter({ hasText: 'déplacée : module 2, position 1' })).toBeAttached();
  await page.getByRole('button', { name: 'Monter le module 2 : TEST Module deux' }).focus();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('button', { name: 'Monter le module 1 : TEST Module deux' })).toBeDisabled();
  await expect(page.getByRole('button', { name: 'Descendre le module 1 : TEST Module deux' })).toBeFocused();

  // Explicit save with the keyboard shortcut.
  await page.keyboard.press('Control+s');
  await expect(saveState(page)).toHaveText(/Enregistré sur le serveur à .* \(version 2\)\./);
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('TEST Cours des nombres complexes');

  await page.setViewportSize({ width: 360, height: 800 });
  expect(await noOverflow(page)).toBe(true);
  await page.screenshot({ path: 'test-results/studio-course-editor-360.png', fullPage: true });
  await page.setViewportSize({ width: 1280, height: 900 });

  // Sign out and in: the persisted structure comes back from the server.
  await page.getByRole('button', { name: 'Se déconnecter' }).click();
  await expect(page).toHaveURL(/\/login$/);
  await signIn(page, author, '/studio/courses');
  await expect(page).toHaveURL(/\/studio\/courses$/);
  await page.getByLabel('Rechercher un titre').fill('nombres COMPLEXES');
  await page.getByRole('link', { name: 'TEST Cours des nombres complexes' }).click();
  await expect(page).toHaveURL(new RegExp(`/studio/courses/${courseId}$`));
  await expect(page.getByLabel('Titre du cours')).toHaveValue('TEST Cours des nombres complexes');
  await expect(page.getByLabel('Description courte (publique)')).toHaveValue('TEST Un parcours court.');
  await expect(page.getByLabel('Titre du module 1')).toHaveValue('TEST Module deux');
  await expect(page.getByLabel('Titre du module 2')).toHaveValue('TEST Module un');
  await expect(page.getByLabel('Titre de l’étape 1 du module 1')).toHaveValue('TEST Étape brouillon');
  await expect(page.getByLabel('Titre de l’étape 1 du module 2')).toHaveValue('TEST Équations et nombres complexes');
  await expect(page.getByLabel('Chapitre officiel (facultatif)').nth(1)).toHaveValue(String(f.chapter.id));
  await expect(page.getByRole('list', { name: 'Classements choisis' })).toContainText('TEST édition 2026');

  // A new current edition supersedes the course's: it stays, flagged, and the new one is offered.
  await f.newEdition('test-2027', 'TEST édition 2027', f.first.id);
  await page.reload();
  const list = page.getByRole('list', { name: 'Classements choisis' });
  await expect(list).toContainText('édition remplacée');
  await expect(list).toContainText('Si vous la retirez, elle ne pourra plus être choisie de nouveau');
  await page.getByLabel('Niveau').selectOption({ label: 'TEST Baccalauréat' });
  await page.getByLabel('Section', { exact: true }).selectOption({ label: 'TEST Section mathématiques' });
  await expect(page.getByRole('button', { name: 'Ajouter l’édition en vigueur « TEST édition 2027 »' })).toBeVisible();

  expect(problems).toEqual([]);
});

test('a stale tab’s save is refused without losing its edits, which are restored after loading the server version', async ({ page, context, browser, baseURL }) => {
  const f = await setup(browser, baseURL);
  const au = await apiSession(browser, baseURL, author);
  const { course } = await apiCourse(au, f.subject.id, { title: 'TEST Cours à deux onglets', modules: [{ key: null, title: 'TEST Module commun', chapterId: null, steps: [] }] });
  await signIn(page, author, `/studio/courses/${course.id}`);
  await expect(page.getByLabel('Titre du cours')).toHaveValue('TEST Cours à deux onglets');

  const other = await context.newPage();
  await other.goto(`/studio/courses/${course.id}`);
  await other.getByLabel('Titre du cours').fill('TEST Titre de l’autre onglet');
  await other.getByRole('button', { name: 'Enregistrer', exact: true }).click();
  await expect(saveState(other)).toHaveText(/Enregistré sur le serveur/);

  await page.getByLabel('Titre du cours').fill('TEST Titre du premier onglet');
  await page.getByLabel('Titre du nouveau module').fill('TEST Module du premier onglet');
  await page.getByRole('button', { name: 'Ajouter le module' }).click();
  await page.getByRole('button', { name: 'Enregistrer', exact: true }).click();
  const conflict = page.getByRole('heading', { name: 'Conflit : ce brouillon de cours a été enregistré ailleurs' });
  await expect(conflict).toBeFocused();
  await expect(page.getByLabel('Titre du cours')).toHaveValue('TEST Titre du premier onglet');
  await expect(page.getByLabel('Titre du module 2')).toHaveValue('TEST Module du premier onglet');
  // The copy is scoped to this user.
  const keys = await page.evaluate(() => Object.keys(localStorage).filter((k) => k.startsWith('learnoo-studio-course-recovery')));
  expect(keys.length).toBeGreaterThan(0);
  for (const k of keys) expect(k.startsWith(`learnoo-studio-course-recovery:v1:u${au.me.id}:c${course.id}:`)).toBe(true);

  await page.getByRole('button', { name: 'Charger la version du serveur' }).click();
  await expect(page.getByLabel('Titre du cours')).toHaveValue('TEST Titre de l’autre onglet');
  await expect(page.getByLabel('Titre du module 2')).toHaveCount(0);
  const recovery = page.getByRole('region', { name: 'Copies de secours locales (non enregistrées)' });
  await expect(recovery).toContainText('gardée lors d’un conflit');
  await recovery.getByRole('button', { name: 'Restaurer dans l’éditeur' }).first().click();
  await expect(page.getByLabel('Titre du cours')).toHaveValue('TEST Titre du premier onglet');
  await expect(page.getByLabel('Titre du module 2')).toHaveValue('TEST Module du premier onglet');
  await page.getByRole('button', { name: 'Enregistrer', exact: true }).click();
  await expect(saveState(page)).toHaveText(/Enregistré sur le serveur/);

  const { courses } = await au.call('GET', '/api/courses?mine=1');
  const saved = await au.call('GET', `/api/course-revisions/${courses.find((c) => c.id === course.id).latestRevision.id}`);
  expect(saved.title).toBe('TEST Titre du premier onglet');
  expect(saved.modules.map((m) => m.title)).toEqual(['TEST Module commun', 'TEST Module du premier onglet']);
  await au.ctx.close();
});

test('another author can neither open nor change a course draft', async ({ page, browser, baseURL }) => {
  const f = await setup(browser, baseURL);
  const au = await apiSession(browser, baseURL, author);
  const { course, revision } = await apiCourse(au, f.subject.id, { title: 'TEST Cours privé' });
  await signIn(page, authorTwo, `/studio/courses/${course.id}`);
  await expect(page.getByRole('alert')).toContainText('Cours introuvable : il n’existe pas ou ne vous appartient pas.');
  await expect(page.getByLabel('Titre du cours')).toHaveCount(0);
  await page.getByRole('navigation', { name: 'Studio' }).getByRole('link', { name: 'Mes cours' }).click();
  await expect(page.getByText('Vous n’avez pas encore de cours.')).toBeVisible();
  await expect(page.getByText('TEST Cours privé')).toHaveCount(0);

  const two = await apiSession(browser, baseURL, authorTwo);
  expect((await two.raw('GET', `/api/course-revisions/${revision.id}`)).status()).toBe(404);
  const put = await two.raw('PUT', `/api/course-revisions/${revision.id}`, { expectedVersion: revision.version, title: 'pris', summary: '', objectives: '', prerequisites: '', assignmentIds: [], modules: [] });
  expect(put.status()).toBe(404);
  expect((await two.raw('DELETE', `/api/courses/${course.id}`)).status()).toBe(404);
  expect((await au.call('GET', `/api/course-revisions/${revision.id}`)).title).toBe('TEST Cours privé');
  await two.ctx.close();
  await au.ctx.close();
});

test('discarding the draft and deleting a never-submitted course ask first and keep the lessons', async ({ page, browser, baseURL }) => {
  const f = await setup(browser, baseURL);
  const au = await apiSession(browser, baseURL, author);
  const { course } = await apiCourse(au, f.subject.id, {
    title: 'TEST Cours à supprimer',
    modules: [{ key: null, title: 'TEST Module', chapterId: null, steps: [{ lessonId: f.lessonDraft.lessonId, revisionId: f.lessonDraft.id, title: 'TEST Étape' }] }],
  });
  await signIn(page, author, `/studio/courses/${course.id}`);
  await expect(page.getByLabel('Titre de l’étape 1 du module 1')).toHaveValue('TEST Étape');

  // Discard the draft: confirmation first, then the course remains without a draft.
  await page.getByRole('button', { name: 'Supprimer ce brouillon' }).click();
  let dialog = page.getByRole('dialog', { name: 'Supprimer ce brouillon ?' });
  await expect(dialog.getByRole('button', { name: 'Garder le brouillon' })).toBeFocused();
  await dialog.getByRole('button', { name: 'Supprimer le brouillon' }).click();
  await expect(page).toHaveURL(/\/studio\/courses$/);
  await expect(page.getByText('Brouillon du cours supprimé.')).toBeVisible();
  // The title belongs to the discarded draft: the course is listed by number, without a current version.
  const untitled = page.getByRole('listitem').filter({ has: page.getByRole('link', { name: `Cours sans titre (n° ${course.id})` }) });
  await expect(untitled).toContainText('Aucune version en cours');
  await untitled.getByRole('link').click();
  await expect(page.getByRole('heading', { name: 'Aucun brouillon en cours' })).toBeVisible();

  // Delete the never-submitted course.
  await page.getByRole('button', { name: 'Supprimer le cours' }).click();
  dialog = page.getByRole('dialog', { name: 'Supprimer définitivement le cours ?' });
  await expect(dialog).toContainText('Les leçons et les images ne sont pas supprimées.');
  await dialog.getByRole('button', { name: 'Supprimer le cours' }).click();
  await expect(page).toHaveURL(/\/studio\/courses$/);
  await expect(page.getByText('Cours supprimé. Les leçons et les images n’ont pas été touchées.')).toBeVisible();
  await expect(page.getByRole('link', { name: `Cours sans titre (n° ${course.id})` })).toHaveCount(0);

  const { courses } = await au.call('GET', '/api/courses?mine=1');
  expect(courses.some((c) => c.id === course.id)).toBe(false);
  const lessons = (await au.call('GET', '/api/lessons/mine')).lessons;
  const kept = lessons.find((l) => l.id === f.lessonDraft.lessonId);
  expect(kept.latestRevision.id).toBe(f.lessonDraft.id);
  expect((await au.call('GET', `/api/revisions/${f.lessonDraft.id}`)).status).toBe('draft');
  await au.ctx.close();
});

test.afterAll(async () => {
  await fixture?.close();
});
