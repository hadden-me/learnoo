import { test, expect } from '@playwright/test';
import { deflateSync, crc32 } from 'node:zlib';

// The built Studio served by the Go application (/studio) under its real CSP,
// against the fixture server's PostgreSQL schema (internal/server
// TestBrowserReaderServer). Accounts are the simulated TEST users the
// fixture creates (newReaderConfig; test password from server_test.go):
// test-author (author), test-reviewer (reviewer of test-math). Every lesson
// these tests create is titled "TEST …". Desktop Chromium; the 360px runs
// emulate a phone viewport, not a physical phone.
const password = 'simulated-password-1';
const author = { email: 'test-author@example.test', password };
const reviewer = { email: 'test-reviewer@example.test', password };
const authorTwo = { email: 'test-author-two@example.test', password };

/** A small valid PNG (solid colour), built here so no binary fixture is needed. */
function png(width = 40, height = 24) {
  const chunk = (type, data) => {
    const len = Buffer.alloc(4);
    len.writeUInt32BE(data.length);
    const body = Buffer.concat([Buffer.from(type, 'ascii'), data]);
    const crc = Buffer.alloc(4);
    crc.writeUInt32BE(crc32(body) >>> 0);
    return Buffer.concat([len, body, crc]);
  };
  const header = Buffer.alloc(13);
  header.writeUInt32BE(width, 0);
  header.writeUInt32BE(height, 4);
  header.set([8, 2, 0, 0, 0], 8); // 8-bit RGB
  const rows = Buffer.concat(Array.from({ length: height }, () => Buffer.concat([Buffer.from([0]), Buffer.alloc(width * 3, 0x3a)])));
  return Buffer.concat([Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]), chunk('IHDR', header), chunk('IDAT', deflateSync(rows)), chunk('IEND', Buffer.alloc(0))]);
}

/** Fails the test on any CSP violation or request leaving the application's origin. */
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

const editor = (page) => page.getByRole('textbox', { name: 'Éditeur du contenu de la leçon' });
const recoveryKeys = (page) => page.evaluate(() => Object.keys(localStorage).filter((k) => k.startsWith('learnoo-studio-recovery')));
const saveButton = (page) => page.getByRole('button', { name: 'Enregistrer', exact: true });
const saveState = (page) => page.locator('.save-state');
const noOverflow = (page) => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth);

async function newLesson(page, { title, language = 'fr' }) {
  await page.getByRole('link', { name: 'Nouvelle leçon' }).click();
  await expect(page.getByRole('heading', { name: 'Nouvelle leçon' })).toBeVisible();
  await page.getByLabel('Matière').selectOption({ label: 'test-math' });
  await page.getByLabel('Chapitre (facultatif)').selectOption({ label: 'test-chapter' });
  if (language === 'ar') await page.getByRole('radio', { name: /Arabe/ }).check();
  await page.getByRole('button', { name: 'Commencer la rédaction' }).click();
  await page.getByLabel('Titre', { exact: true }).fill(title);
}

/** The TEST reviewer requests changes through the review API (their UI is the next slice). */
async function requestChanges(browser, baseURL, revisionId, comment) {
  const reviewerContext = await browser.newContext({ baseURL });
  const login = await reviewerContext.request.post('/api/auth/login', { data: reviewer });
  expect(login.status()).toBe(200);
  const { csrfToken } = await login.json();
  const decision = await reviewerContext.request.post(`/api/review/revisions/${revisionId}/request-changes`, { data: { comment }, headers: { 'X-CSRF-Token': csrfToken } });
  expect(decision.status()).toBe(200);
  await reviewerContext.close();
}

/** Signs this browser context in as `user` through the API (as another tab would), replacing its current session. */
async function switchAccount(context, user) {
  const current = await context.request.get('/api/auth/session');
  if (current.ok()) await context.request.post('/api/auth/logout', { headers: { 'X-CSRF-Token': (await current.json()).csrfToken } });
  expect((await context.request.post('/api/auth/login', { data: user })).status()).toBe(200);
}

async function save(page) {
  await saveButton(page).click();
  await expect(saveState(page)).toContainText('Enregistré sur le serveur');
}

/** The revision currently open, read through the API with the page's own session. */
async function openRevision(page) {
  const lessonId = Number(/\/studio\/lessons\/(\d+)\/edit/.exec(page.url())[1]);
  const mine = await (await page.request.get('/api/lessons/mine')).json();
  const lesson = mine.lessons.find((l) => l.id === lessonId);
  return (await page.request.get(`/api/revisions/${lesson.latestRevision.id}`)).json();
}

test('Studio loads from the application under its CSP without violations', async ({ page, baseURL }) => {
  const problems = watch(page, baseURL);
  await page.addInitScript(() => {
    window.cspViolations = [];
    document.addEventListener('securitypolicyviolation', (e) => window.cspViolations.push(`${e.violatedDirective} ${e.blockedURI}`));
  });

  // Anonymous: sent to sign-in, then back to the Studio.
  await signIn(page, author);
  await expect(page).toHaveURL(/\/studio$/);
  const csp = (await page.request.get('/studio')).headers()['content-security-policy'];
  expect(csp).toContain("style-src 'self';");
  await expect(page.getByRole('heading', { name: 'Mes leçons' })).toBeVisible();

  await newLesson(page, { title: 'TEST Espaces' });
  await editor(page).click();
  await page.keyboard.type('Texte   avec espaces');
  // ProseMirror's base rules come from the Studio stylesheet, not an injected <style>.
  const ws = await page.evaluate(() => getComputedStyle(document.querySelector('.ProseMirror')).whiteSpace);
  expect(['pre-wrap', 'break-spaces']).toContain(ws);
  expect(await page.evaluate(() => document.querySelectorAll('style').length)).toBe(0);

  expect(await page.evaluate(() => window.cspViolations)).toEqual([]);
  expect(problems).toEqual([]);
});

test('author writes, uploads, previews, saves, signs out and in, and reopens the draft; a stale tab cannot overwrite it; submit', async ({ page, context, baseURL }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  const problems = watch(page, baseURL);
  await signIn(page, author);
  await newLesson(page, { title: 'TEST Parcours auteur' });
  await page.getByLabel('Objectif').fill('Objectif de test.');

  // Keyboard entry with bold, inline math and an Arabic paragraph.
  await editor(page).click();
  await page.keyboard.type('Texte ');
  await page.keyboard.press('ControlOrMeta+b');
  await page.keyboard.type('gras');
  await page.keyboard.press('ControlOrMeta+b');
  await page.keyboard.type(' avec $z=3+2i$ fin.');
  await page.keyboard.press('Enter');
  await page.keyboard.type('فقرة');
  await page.getByLabel('Langue du paragraphe').selectOption('ar');
  await expect(saveState(page)).toContainText('non enregistrées');

  // Image upload: a valid PNG is attached; a file with trailing data is explained.
  const upload = page.getByLabel('Téléverser une image (PNG, JPEG ou WebP, 5 Mio au maximum)');
  await upload.setInputFiles({ name: 'polyglotte.png', mimeType: 'image/png', buffer: Buffer.concat([png(), Buffer.from('trailing')]) });
  await expect(page.getByRole('alert')).toContainText('données après sa fin');
  await upload.setInputFiles({ name: 'figure.png', mimeType: 'image/png', buffer: png() });
  await expect(page.getByRole('img', { name: /Image jointe n° \d+/ })).toBeVisible();
  expect(await page.getByRole('img', { name: /Image jointe/ }).evaluate((img) => img.naturalWidth)).toBe(40);

  // Canonical preview from the server, typeset with the application's KaTeX.
  await page.getByRole('button', { name: 'Prévisualiser' }).first().click();
  const preview = page.getByTestId('lesson-preview');
  await expect(preview.locator('.katex').first()).toBeVisible();
  await expect(preview.getByRole('heading', { level: 1 })).toHaveText('TEST Parcours auteur');
  expect(await preview.locator('p[lang="ar"]').evaluate((p) => getComputedStyle(p).direction)).toBe('rtl');

  // First save creates the lesson and moves to its address.
  await saveButton(page).click();
  await expect(page).toHaveURL(/\/studio\/lessons\/\d+\/edit$/);
  await expect(page.getByText('Leçon créée et enregistrée sur le serveur.')).toBeVisible();
  const editURL = page.url();
  let revision = await openRevision(page);
  expect(revision.status).toBe('draft');
  expect(revision.mediaIds).toHaveLength(1);
  expect(revision.content.blocks).toEqual([
    { type: 'paragraph', children: [{ type: 'text', text: 'Texte ' }, { type: 'strong', children: [{ type: 'text', text: 'gras' }] }, { type: 'text', text: ' avec ' }, { type: 'math', text: 'z=3+2i' }, { type: 'text', text: ' fin.' }] },
    { type: 'paragraph', lang: 'ar', dir: 'rtl', children: [{ type: 'text', text: 'فقرة' }] },
  ]);
  // Nothing is persisted in localStorage once the server has the edits.
  expect(await page.evaluate(() => Object.keys(localStorage).filter((k) => k.startsWith('learnoo-studio-recovery')))).toEqual([]);

  // Sign out, sign in, reopen from the list.
  await page.getByRole('button', { name: 'Se déconnecter' }).click();
  await expect(page).toHaveURL(/\/login$/);
  await signIn(page, author);
  await page.getByRole('link', { name: 'TEST Parcours auteur' }).click();
  await expect(page).toHaveURL(editURL);
  await expect(editor(page)).toContainText('Texte gras avec');
  await expect(editor(page)).toContainText('فقرة');
  await expect(page.getByLabel('Objectif')).toHaveValue('Objectif de test.');
  await expect(page.getByRole('img', { name: /Image jointe/ })).toBeVisible();
  await expect(saveState(page)).toContainText(`version ${revision.version}`);

  // A second tab saves first; the stale tab's save is refused and keeps its edits.
  const other = await context.newPage();
  await other.goto(editURL);
  await editor(other).click();
  await other.keyboard.press('ControlOrMeta+End');
  await other.keyboard.press('Enter');
  await other.keyboard.type('Ajout depuis le second onglet.');
  await save(other);
  await other.close();

  await editor(page).click();
  await page.keyboard.press('ControlOrMeta+End');
  await page.keyboard.press('Enter');
  await page.keyboard.type('Ajout local à ne pas perdre.');
  await saveButton(page).click();
  await expect(page.getByRole('heading', { name: 'Conflit : ce brouillon a été enregistré ailleurs' })).toBeFocused();
  await expect(editor(page)).toContainText('Ajout local à ne pas perdre.');
  await expect(saveState(page)).toContainText('non enregistrées');
  // Leaving now would ask first.
  await page.getByRole('link', { name: 'Mes leçons' }).first().click();
  await expect(page.getByRole('dialog', { name: 'Quitter sans enregistrer ?' })).toBeVisible();
  await page.getByRole('button', { name: 'Rester sur la page' }).click();
  await expect(page).toHaveURL(editURL);

  // Reload the server version: the local copy stays available and can be restored.
  await page.getByRole('button', { name: 'Charger la version du serveur' }).click();
  await expect(editor(page)).toContainText('Ajout depuis le second onglet.');
  await expect(editor(page)).not.toContainText('Ajout local à ne pas perdre.');
  const recovery = page.getByRole('region', { name: 'Copies de secours locales (non enregistrées)' });
  await expect(recovery).toContainText('gardée lors d’un conflit');
  await recovery.getByRole('button', { name: 'Restaurer dans l’éditeur' }).click();
  await expect(editor(page)).toContainText('Ajout local à ne pas perdre.');
  await save(page);
  revision = await openRevision(page);
  expect(JSON.stringify(revision.content)).toContain('Ajout local à ne pas perdre.');
  expect(await page.evaluate(() => Object.keys(localStorage).filter((k) => k.startsWith('learnoo-studio-recovery')))).toEqual([]);

  // Submit: confirmation, then the revision is read-only and in review.
  await page.getByRole('button', { name: 'Soumettre pour relecture' }).click();
  const dialog = page.getByRole('dialog', { name: 'Soumettre pour relecture ?' });
  await expect(dialog).toContainText('ne pourra plus être modifiée');
  await dialog.getByRole('button', { name: 'Soumettre', exact: true }).click();
  await expect(page.getByRole('heading', { name: /Révision 1 · En relecture/ })).toBeVisible();
  await expect(page.getByTestId('revision-preview').locator('.katex').first()).toBeVisible();
  await expect(editor(page)).toHaveCount(0);
  expect((await openRevision(page)).status).toBe('in_review');
  await page.screenshot({ path: 'test-results/studio-submitted-desktop.png', fullPage: true });

  expect(problems).toEqual([]);
});

test('reviewer feedback reaches the author, who drafts a correction and resubmits', async ({ page, browser, baseURL }) => {
  const problems = watch(page, baseURL);
  await signIn(page, author);
  await newLesson(page, { title: 'TEST Retour relecteur' });
  await editor(page).click();
  await page.keyboard.type('Première version.');
  await saveButton(page).click();
  await expect(page).toHaveURL(/\/edit$/);
  await page.getByRole('button', { name: 'Soumettre pour relecture' }).click();
  await page.getByRole('dialog').getByRole('button', { name: 'Soumettre', exact: true }).click();
  await expect(page.getByRole('heading', { name: /En relecture/ })).toBeVisible();
  const submitted = await openRevision(page);

  await requestChanges(browser, baseURL, submitted.id, 'TEST Précisez la définition du module.');

  await page.getByRole('link', { name: 'Mes leçons' }).first().click();
  const card = page.getByRole('listitem').filter({ hasText: 'TEST Retour relecteur' });
  await expect(card).toContainText('Modifications demandées');
  await expect(card).toContainText('Retour du relecteur à traiter');
  await card.getByRole('link').click();
  const feedback = page.getByRole('region', { name: 'Le relecteur a demandé des modifications' });
  await expect(feedback).toContainText('TEST Précisez la définition du module.');
  await page.getByRole('button', { name: 'Créer la version corrigée' }).click();
  await expect(page.getByText('Brouillon · révision 2')).toBeVisible();
  await expect(feedback).toContainText('TEST Précisez la définition du module.');
  await expect(editor(page)).toContainText('Première version.');
  await editor(page).click();
  await page.keyboard.press('ControlOrMeta+End');
  await page.keyboard.type(' Définition précisée.');
  // Submitting saves the pending edits first.
  await page.getByRole('button', { name: 'Soumettre pour relecture' }).click();
  await page.getByRole('dialog').getByRole('button', { name: 'Soumettre', exact: true }).click();
  await expect(page.getByRole('heading', { name: /Révision 2 · En relecture/ })).toBeVisible();
  const resubmitted = await openRevision(page);
  expect(resubmitted.number).toBe(2);
  expect(JSON.stringify(resubmitted.content)).toContain('Définition précisée.');

  // A revision in review blocks a new draft; the server's refusal is explained.
  const again = await page.request.post(`/api/lessons/${resubmitted.lessonId}/drafts`, {
    data: { content: resubmitted.content, prerequisites: '', chapterId: null, mediaIds: [] },
    headers: { 'X-CSRF-Token': (await (await page.request.get('/api/auth/session')).json()).csrfToken },
  });
  expect((await again.json()).error.code).toBe('active_candidate_exists');
  expect(problems).toEqual([]);
});

test('an Arabic lesson is edited right to left at 360px, and an expired session keeps the edits', async ({ page, context, baseURL }) => {
  await page.setViewportSize({ width: 360, height: 780 });
  const problems = watch(page, baseURL);
  await signIn(page, author);
  expect(await noOverflow(page)).toBe(true);
  await newLesson(page, { title: 'TEST درس الاستوديو', language: 'ar' });
  await expect(page.locator('.editor-frame')).toHaveAttribute('dir', 'rtl');
  expect(await page.getByLabel('Titre', { exact: true }).evaluate((el) => getComputedStyle(el).direction)).toBe('rtl');
  await editor(page).click();
  await page.keyboard.type('نص تجريبي مع صيغة $x^2$ هنا.');
  expect(await editor(page).evaluate((el) => getComputedStyle(el).direction)).toBe('rtl');
  await saveButton(page).click();
  await expect(page).toHaveURL(/\/edit$/);
  expect(await noOverflow(page)).toBe(true);
  await page.getByRole('button', { name: 'Prévisualiser' }).first().click();
  const preview = page.getByTestId('lesson-preview');
  await expect(preview.locator('.katex').first()).toBeVisible();
  expect(await preview.locator('article').evaluate((el) => getComputedStyle(el).direction)).toBe('rtl');
  expect(await preview.locator('.math').first().evaluate((el) => getComputedStyle(el).direction)).toBe('ltr');
  expect(await noOverflow(page)).toBe(true);
  await page.screenshot({ path: 'test-results/studio-arabic-mobile.png', fullPage: true });

  // The session ends while editing: nothing is lost, and saving resumes after signing in again.
  await editor(page).click();
  await page.keyboard.press('ControlOrMeta+End');
  await page.keyboard.type(' إضافة بعد انتهاء الجلسة.');
  await context.clearCookies();
  await saveButton(page).click();
  await expect(page.getByRole('alert').filter({ hasText: 'Session expirée.' })).toBeVisible();
  await expect(editor(page)).toContainText('إضافة بعد انتهاء الجلسة.');
  await expect.poll(() => page.evaluate(() => Object.keys(localStorage).filter((k) => k.startsWith('learnoo-studio-recovery')).length)).toBe(1);
  expect(await noOverflow(page)).toBe(true);
  await page.screenshot({ path: 'test-results/studio-expired-mobile.png', fullPage: true });

  const relogin = await context.newPage();
  await signIn(relogin, author);
  await expect(relogin.getByRole('heading', { name: 'Mes leçons' })).toBeVisible();
  await relogin.close();
  await page.getByRole('button', { name: 'Reprendre ici' }).click();
  await expect(page.getByText('Session expirée.')).toHaveCount(0);
  await save(page);
  expect(JSON.stringify((await openRevision(page)).content)).toContain('إضافة بعد انتهاء الجلسة.');
  expect(problems).toEqual([]);
});

test('a crashed tab leaves a labelled recovery copy that can be restored; discard asks first', async ({ page, context, baseURL }) => {
  const problems = watch(page, baseURL);
  await signIn(page, author);
  await newLesson(page, { title: 'TEST Copie de secours' });
  await editor(page).click();
  await page.keyboard.type('Version enregistrée.');
  // Text typed while the first save is still creating the lesson is kept, and stays unsaved.
  let release;
  const held = new Promise((resolve) => (release = resolve));
  await page.route('**/api/lessons', async (route) => {
    if (route.request().method() === 'POST') await held;
    await route.continue();
  });
  await saveButton(page).click();
  await expect(saveState(page)).toContainText('Enregistrement en cours');
  await editor(page).click();
  await page.keyboard.press('ControlOrMeta+End');
  await page.keyboard.type(' Tapé pendant la création.');
  release();
  await expect(page).toHaveURL(/\/edit$/);
  await expect(page.getByText('Leçon créée et enregistrée sur le serveur.')).toBeVisible();
  await expect(saveState(page)).toContainText('non enregistrées');
  await expect(editor(page)).toContainText('Version enregistrée. Tapé pendant la création.');
  await page.unroute('**/api/lessons');
  await save(page);
  expect(JSON.stringify((await openRevision(page)).content)).toContain('Tapé pendant la création.');
  const editURL = page.url();
  await editor(page).click();
  await page.keyboard.press('ControlOrMeta+End');
  await page.keyboard.type(' Ajout jamais enregistré.');
  await expect.poll(() => page.evaluate(() => Object.keys(localStorage).filter((k) => k.startsWith('learnoo-studio-recovery')).length)).toBe(1);
  // Simulate a crash: the tab goes away without unload handlers.
  const fresh = await context.newPage();
  await page.close({ runBeforeUnload: false });
  await fresh.goto(editURL);
  await expect(editor(fresh)).not.toContainText('Ajout jamais enregistré.');
  const recovery = fresh.getByRole('region', { name: 'Copies de secours locales (non enregistrées)' });
  await expect(recovery).toContainText('« TEST Copie de secours »');
  await recovery.getByRole('button', { name: 'Restaurer dans l’éditeur' }).click();
  await expect(editor(fresh)).toContainText('Ajout jamais enregistré.');
  await save(fresh);

  // Another tab's unsaved copy survives this tab's discard and is offered on the lesson's next draft.
  const tab = await context.newPage();
  await tab.goto(editURL);
  await editor(tab).click();
  await tab.keyboard.press('ControlOrMeta+End');
  await tab.keyboard.type(' Copie d’un autre onglet.');
  await expect.poll(() => recoveryKeys(tab).then((k) => k.length)).toBe(1);
  await tab.close({ runBeforeUnload: false });

  await fresh.getByRole('button', { name: 'Supprimer ce brouillon' }).click();
  const dialog = fresh.getByRole('dialog', { name: 'Supprimer ce brouillon ?' });
  await expect(dialog.getByRole('button', { name: 'Garder le brouillon' })).toBeFocused();
  await fresh.keyboard.press('Escape');
  await expect(dialog).toHaveCount(0);
  await fresh.getByRole('button', { name: 'Supprimer ce brouillon' }).click();
  await fresh.getByRole('dialog').getByRole('button', { name: 'Supprimer définitivement' }).click();
  await expect(fresh.getByRole('heading', { name: 'Mes leçons' })).toBeVisible();
  await expect(fresh.getByText('Brouillon supprimé.')).toBeVisible();
  await expect(fresh.getByRole('listitem').filter({ hasText: /Leçon n° \d+/ }).first()).toContainText('Aucune version en cours');
  expect(await recoveryKeys(fresh)).toHaveLength(1);
  await fresh.goto(editURL);
  const offered = fresh.getByRole('region', { name: 'Copies de secours locales (non enregistrées)' });
  await offered.getByRole('button', { name: 'Restaurer dans l’éditeur' }).click();
  await expect(editor(fresh)).toContainText('Copie d’un autre onglet.');
  await save(fresh);
  expect(await recoveryKeys(fresh)).toEqual([]);
  expect(problems).toEqual([]);
});

test('pasting HTML strips unsupported markup; an invalid import is refused without touching the document', async ({ page, baseURL }) => {
  const problems = watch(page, baseURL);
  await signIn(page, author);
  await newLesson(page, { title: 'TEST Collage' });
  await editor(page).click();
  await page.evaluate(() => {
    window.pwned = false;
    const data = new DataTransfer();
    data.setData('text/html', '<p>Collé <em>ici</em><img src="x" onerror="window.pwned=true"><script>window.pwned=true</script></p>');
    data.setData('text/plain', 'Collé ici');
    document.querySelector('.studio-editor').dispatchEvent(new ClipboardEvent('paste', { clipboardData: data, bubbles: true, cancelable: true }));
  });
  await expect(page.locator('.paste-notice')).toContainText('image externe');
  await expect(page.locator('.paste-notice')).toContainText('script');
  await expect(editor(page)).toContainText('Collé ici');
  expect(await page.evaluate(() => window.pwned)).toBe(false);

  await page.getByText('Importer ou exporter le JSON').click();
  const chooser = page.waitForEvent('filechooser');
  await page.getByRole('button', { name: 'Importer un JSON' }).click();
  await (await chooser).setFiles({ name: 'bad.json', mimeType: 'application/json', buffer: Buffer.from('{"schemaVersion":1,"title":"x","language":"fr","status":"unreviewed","objective":"","blocks":[{"type":"html","text":"<script>"}]}') });
  await expect(page.getByRole('alert')).toContainText('Import refusé');
  await expect(editor(page)).toContainText('Collé ici');
  expect(problems).toEqual([]);
});

test('a stale tab never writes as another account signed in on the same browser; it resumes once the author is back', async ({ page, context, baseURL }) => {
  const problems = watch(page, baseURL);
  await signIn(page, author);
  await newLesson(page, { title: 'TEST Compte changé' });
  await editor(page).click();
  await page.keyboard.type('Version enregistrée.');
  await saveButton(page).click();
  await expect(page).toHaveURL(/\/edit$/);
  const before = await openRevision(page);
  await editor(page).click();
  await page.keyboard.press('ControlOrMeta+End');
  await page.keyboard.type(' Modification locale.');
  // A second tab holds a lesson not created yet.
  const fresh = await context.newPage();
  await fresh.goto('/studio');
  await newLesson(fresh, { title: 'TEST Jamais créée' });
  await editor(fresh).click();
  await fresh.keyboard.type('Texte de la leçon à créer.');

  // Someone signs in with another author account in this browser.
  await switchAccount(context, authorTwo);

  // Save, upload and create are refused in the stale tabs; the edits stay.
  await saveButton(page).click();
  const banner = page.locator('.banner.expired');
  await expect(banner).toContainText('Un autre compte est connecté dans ce navigateur.');
  await expect(editor(page)).toContainText('Version enregistrée. Modification locale.');
  await page.getByLabel('Téléverser une image (PNG, JPEG ou WebP, 5 Mio au maximum)').setInputFiles({ name: 'figure.png', mimeType: 'image/png', buffer: png() });
  await expect(page.locator('.media [role="alert"]')).toContainText('Un autre compte est connecté');
  await saveButton(fresh).click();
  await expect(fresh.locator('.banner.expired')).toContainText('Un autre compte est connecté dans ce navigateur.');
  await expect(fresh).toHaveURL(/\/studio\/lessons\/new$/);
  await expect(editor(fresh)).toContainText('Texte de la leçon à créer.');
  // Nothing reached the other account.
  const theirs = await (await context.request.get('/api/lessons/mine')).json();
  expect(theirs.lessons).toEqual([]);
  // Resuming while the other account is still signed in is refused.
  await page.getByRole('button', { name: 'Reprendre ici' }).click();
  await expect(banner).toContainText('Un autre compte est toujours connecté');

  // The author signs in again: both tabs resume and save as the author.
  await switchAccount(context, author);
  await page.getByRole('button', { name: 'Reprendre ici' }).click();
  await expect(banner).toHaveCount(0);
  await save(page);
  const after = await openRevision(page);
  expect(after.version).toBe(before.version + 1);
  expect(JSON.stringify(after.content)).toContain('Modification locale.');
  await fresh.getByRole('button', { name: 'Reprendre ici' }).click();
  await saveButton(fresh).click();
  await expect(fresh).toHaveURL(/\/studio\/lessons\/\d+\/edit$/);
  expect((await openRevision(fresh)).authorId).toBe(after.authorId);
  expect(problems).toEqual([]);
});

test('edits in a tab whose draft was submitted elsewhere are kept and restored on the corrected version; sign-out clears copies', async ({ page, context, browser, baseURL }) => {
  const problems = watch(page, baseURL);
  await signIn(page, author);
  await newLesson(page, { title: 'TEST Soumis ailleurs' });
  await editor(page).click();
  await page.keyboard.type('Première version.');
  await saveButton(page).click();
  await expect(page).toHaveURL(/\/edit$/);
  const editURL = page.url();

  const other = await context.newPage();
  await other.goto(editURL);
  await other.getByRole('button', { name: 'Soumettre pour relecture' }).click();
  await other.getByRole('dialog').getByRole('button', { name: 'Soumettre', exact: true }).click();
  await expect(other.getByRole('heading', { name: /Révision 1 · En relecture/ })).toBeVisible();
  await other.close();

  await editor(page).click();
  await page.keyboard.press('ControlOrMeta+End');
  await page.keyboard.type(' Ajout après soumission.');
  await saveButton(page).click();
  await expect(page.getByRole('heading', { name: 'Ce brouillon a été soumis ailleurs' })).toBeFocused();
  await expect(editor(page)).toContainText('Première version. Ajout après soumission.');
  await page.getByRole('button', { name: 'Voir la révision soumise' }).click();
  await expect(page.getByRole('heading', { name: /Révision 1 · En relecture/ })).toBeVisible();
  const recovery = page.getByRole('region', { name: 'Copies de secours locales (non enregistrées)' });
  await expect(recovery).toContainText('gardée lors d’un conflit');
  await expect(recovery.getByRole('button', { name: 'Restaurer dans l’éditeur' })).toHaveCount(0);

  await requestChanges(browser, baseURL, (await openRevision(page)).id, 'TEST Ajoutez un exemple.');
  await page.reload();
  await page.getByRole('button', { name: 'Créer la version corrigée' }).click();
  await expect(page.getByText('Brouillon · révision 2')).toBeVisible();
  await expect(recovery).toContainText('faite sur la révision 1');
  await recovery.getByRole('button', { name: 'Restaurer dans l’éditeur' }).click();
  await expect(editor(page)).toContainText('Ajout après soumission.');
  await save(page);
  const corrected = await openRevision(page);
  expect(corrected.number).toBe(2);
  expect(JSON.stringify(corrected.content)).toContain('Ajout après soumission.');
  expect(await recoveryKeys(page)).toEqual([]);

  // Unsaved edits at sign-out: asked first, then this account's copies are removed from the device.
  await editor(page).click();
  await page.keyboard.type(' Jamais enregistré.');
  await expect.poll(() => recoveryKeys(page).then((k) => k.length)).toBe(1);
  await page.getByRole('button', { name: 'Se déconnecter' }).click();
  const dialog = page.getByRole('dialog', { name: 'Se déconnecter sans enregistrer ?' });
  await expect(dialog).toContainText('effacées de cet appareil');
  await dialog.getByRole('button', { name: 'Se déconnecter quand même' }).click();
  await expect(page).toHaveURL(/\/login$/);
  expect(await recoveryKeys(page)).toEqual([]);
  expect(problems).toEqual([]);
});

test('the preview typesets display math and keeps a malformed formula readable as source', async ({ page, baseURL }) => {
  const problems = watch(page, baseURL);
  await signIn(page, author);
  await newLesson(page, { title: 'TEST Formules' });
  await editor(page).click();
  await page.keyboard.type('Avant $\\frac{1}{$ après la formule fautive.');
  await page.keyboard.press('Enter');
  await page.keyboard.type('$$ ');
  await page.getByLabel('Formule centrée (TeX)').fill('\\int_0^1 x\\,dx = \\frac{1}{2}');
  await page.getByRole('button', { name: 'Prévisualiser' }).first().click();
  const preview = page.getByTestId('lesson-preview');
  await expect(preview.locator('.formula .katex-display')).toBeVisible();
  const broken = preview.locator('.math-error');
  await expect(broken).toHaveCount(1);
  await expect(broken).toHaveText('\\frac{1}{');
  await expect(broken).toHaveAttribute('aria-label', 'Formule non rendue : \\frac{1}{');
  await expect(preview).toContainText('après la formule fautive.');
  expect(await preview.getAttribute('data-math-errors')).toBe('1');
  expect(problems).toEqual([]);
});

test('a page never shows another account’s lessons after someone else signs in on the same browser', async ({ page, context, baseURL }) => {
  const problems = watch(page, baseURL);
  await signIn(page, authorTwo);
  await expect(page.getByRole('heading', { name: 'Mes leçons' })).toBeVisible();
  await expect(page.locator('.who')).toHaveText('TEST Auteur Deux');
  // test-author signs in from another tab (here: the context's API, same cookies).
  await switchAccount(context, author);
  await page.getByRole('link', { name: 'Nouvelle leçon' }).click();
  await page.getByRole('navigation', { name: 'Studio' }).getByRole('link', { name: 'Mes leçons' }).click();
  await expect(page.locator('.banner.expired')).toContainText('Un autre compte est connecté dans ce navigateur.');
  // Nothing of test-author's was fetched or shown under test-author-two's header.
  await expect(page.locator('.lesson-list')).toHaveCount(0);
  await expect(page.getByText(/TEST (Parcours auteur|Compte changé|Soumis ailleurs)/)).toHaveCount(0);
  await expect(page.getByLabel('Matière')).toHaveCount(0);
  await switchAccount(context, authorTwo);
  expect(problems).toEqual([]);
});

/** A signed-in API session (another browser context) for the TEST editorial accounts. */
async function apiSession(browser, baseURL, user) {
  const ctx = await browser.newContext({ baseURL });
  const login = await ctx.request.post('/api/auth/login', { data: user });
  expect(login.status()).toBe(200);
  const { csrfToken } = await login.json();
  return { ctx, post: (path, data = {}) => ctx.request.post(path, { data, headers: { 'X-CSRF-Token': csrfToken } }), get: (path) => ctx.request.get(path) };
}

test('an uploaded image placed in the text is previewed, reviewed, published to students and withdrawn with its revision', async ({ page, browser, baseURL }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  const problems = watch(page, baseURL);
  await page.addInitScript(() => {
    window.cspViolations = [];
    document.addEventListener('securitypolicyviolation', (e) => window.cspViolations.push(`${e.violatedDirective} ${e.blockedURI}`));
  });
  await signIn(page, author);
  await newLesson(page, { title: 'TEST Image intégrée' });
  await editor(page).click();
  await page.keyboard.type('Avant la figure.');

  // Upload, then place it at the cursor: the alt text is required.
  await page.getByLabel('Téléverser une image (PNG, JPEG ou WebP, 5 Mio au maximum)').setInputFiles({ name: 'figure.png', mimeType: 'image/png', buffer: png(48, 30) });
  const attached = page.getByRole('img', { name: /Image jointe n° \d+/ });
  await expect(attached).toBeVisible();
  const mediaId = Number(/n° (\d+)/.exec(await attached.getAttribute('alt'))[1]);
  await page.getByRole('button', { name: 'Placer dans le texte' }).click();
  const dialog = page.getByRole('dialog', { name: `Placer l’image n° ${mediaId} dans le texte` });
  await expect(dialog.getByLabel('Texte alternatif (obligatoire)')).toBeFocused();
  await expect(dialog.getByRole('button', { name: 'Placer l’image' })).toBeDisabled();
  await dialog.getByLabel('Texte alternatif (obligatoire)').fill('Rectangle gris de test');
  await dialog.getByLabel('Légende (facultative, visible sous l’image)').fill('Figure de test');
  await dialog.getByRole('button', { name: 'Placer l’image' }).click();
  await expect(dialog).toHaveCount(0);
  const inEditor = editor(page).getByRole('img', { name: 'Rectangle gris de test' });
  await expect(inEditor).toHaveAttribute('src', `/api/media/${mediaId}`);
  await expect.poll(() => inEditor.evaluate((img) => img.naturalWidth)).toBe(48);
  await expect(page.locator('.media-list')).toContainText('placée dans le texte');
  await expect(page.getByRole('button', { name: 'Retirer de la révision' })).toBeDisabled();

  // Canonical preview through the author's media route.
  await page.getByRole('button', { name: 'Prévisualiser' }).first().click();
  const previewImg = page.getByTestId('lesson-preview').getByRole('img', { name: 'Rectangle gris de test' });
  await expect(previewImg).toHaveAttribute('src', `/api/media/${mediaId}`);
  await expect.poll(() => previewImg.evaluate((img) => img.naturalWidth)).toBe(48);
  await expect(page.getByTestId('lesson-preview').locator('figcaption')).toHaveText('Figure de test');

  // Save and submit.
  await saveButton(page).click();
  await expect(page).toHaveURL(/\/studio\/lessons\/\d+\/edit$/);
  const revision = await openRevision(page);
  expect(revision.mediaIds).toEqual([mediaId]);
  expect(revision.content.blocks).toContainEqual({ type: 'image', src: `media:${mediaId}`, alt: 'Rectangle gris de test', caption: 'Figure de test' });
  await page.getByRole('button', { name: 'Soumettre pour relecture' }).click();
  await page.getByRole('dialog').getByRole('button', { name: 'Soumettre', exact: true }).click();
  await expect(page.getByRole('heading', { name: /En relecture/ })).toBeVisible();
  await expect.poll(() => page.getByTestId('revision-preview').getByRole('img', { name: 'Rectangle gris de test' }).evaluate((img) => img.naturalWidth)).toBe(48);
  expect(await page.evaluate(() => window.cspViolations)).toEqual([]);
  expect(problems).toEqual([]);

  // The reviewer sees it through the review API and the review media route (the review views are the next slice).
  const rev = await apiSession(browser, baseURL, reviewer);
  const reviewed = await (await rev.get(`/api/review/revisions/${revision.id}`)).json();
  expect(JSON.stringify(reviewed.content)).toContain(`media:${mediaId}`);
  const reviewMedia = await rev.get(`/api/review/media/${mediaId}`);
  expect(reviewMedia.status()).toBe(200);
  expect(reviewMedia.headers()['content-type']).toBe('image/png');
  const reviewPreview = await (await rev.post('/api/preview', { content: reviewed.content, mediaRoute: 'review' })).json();
  expect(reviewPreview.html).toContain(`src="/api/review/media/${mediaId}"`);
  expect((await rev.post(`/api/review/revisions/${revision.id}/approve`)).status()).toBe(200);
  await rev.ctx.close();
  const pub = await apiSession(browser, baseURL, { email: 'test-publisher@example.test', password });
  expect((await pub.post(`/api/revisions/${revision.id}/publish`)).status()).toBe(200);
  await pub.ctx.close();

  // A student reads it; the image comes from the lesson-scoped reader route under the reader's CSP.
  const lessonId = Number(/\/studio\/lessons\/(\d+)\/edit/.exec(page.url())[1]);
  const { publicId } = (await (await page.request.get('/api/lessons/mine')).json()).lessons.find((l) => l.id === lessonId);
  const mediaURL = `/learn/lessons/${publicId}/media/${mediaId}`;
  const studentContext = await browser.newContext({ baseURL });
  const student = await studentContext.newPage();
  const studentProblems = watch(student, baseURL);
  await student.addInitScript(() => {
    window.cspViolations = [];
    document.addEventListener('securitypolicyviolation', (e) => window.cspViolations.push(`${e.violatedDirective} ${e.blockedURI}`));
  });
  await signIn(student, { email: 'test-student@example.test', password }, `/learn/lessons/${publicId}`);
  await expect(student).toHaveURL(new RegExp(`/learn/lessons/${publicId}$`));
  const readerImg = student.getByRole('img', { name: 'Rectangle gris de test' });
  await expect(readerImg).toHaveAttribute('src', mediaURL);
  await expect.poll(() => readerImg.evaluate((img) => img.naturalWidth)).toBe(48);
  await expect(student.locator('figure.lesson-media figcaption')).toHaveText('Figure de test');
  const served = await student.request.get(mediaURL);
  expect(served.status()).toBe(200);
  expect(served.headers()['cache-control']).toBe('private, no-store');
  expect(served.headers()['x-content-type-options']).toBe('nosniff');
  // The student has no access to the Studio's routes for the same image.
  expect((await student.request.get(`/api/media/${mediaId}`)).status()).toBe(403);
  expect(await student.evaluate(() => window.cspViolations)).toEqual([]);
  expect(studentProblems).toEqual([]);

  // Nobody else gets it: anonymous readers, and another author through the author route.
  const anonymous = await browser.newContext({ baseURL });
  const anon = await anonymous.request.get(mediaURL, { maxRedirects: 0 });
  expect(anon.status()).toBe(404);
  await anonymous.close();
  const other = await apiSession(browser, baseURL, authorTwo);
  expect((await other.get(`/api/media/${mediaId}`)).status()).toBe(404);
  await other.ctx.close();

  // Withdrawn: the image is no longer served, even to the student who read it.
  const admin = await apiSession(browser, baseURL, { email: 'test-admin@example.test', password });
  expect((await admin.post(`/api/revisions/${revision.id}/withdraw`, { reason: 'TEST retrait.', expectedPublishedRevisionId: revision.id })).status()).toBe(200);
  await admin.ctx.close();
  expect((await student.request.get(mediaURL)).status()).toBe(404);
  await studentContext.close();
});
