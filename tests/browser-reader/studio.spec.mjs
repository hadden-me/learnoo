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

const editor = (page) => page.getByRole('textbox', { name: 'Contenu de la leçon' });
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

  // The reviewer requests changes through the review API (their UI is the next slice).
  const reviewerContext = await browser.newContext({ baseURL });
  const login = await reviewerContext.request.post('/api/auth/login', { data: reviewer });
  expect(login.status()).toBe(200);
  const { csrfToken } = await login.json();
  const decision = await reviewerContext.request.post(`/api/review/revisions/${submitted.id}/request-changes`, {
    data: { comment: 'TEST Précisez la définition du module.' },
    headers: { 'X-CSRF-Token': csrfToken },
  });
  expect(decision.status()).toBe(200);
  await reviewerContext.close();

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
  await saveButton(page).click();
  await expect(page).toHaveURL(/\/edit$/);
  const editURL = page.url();
  // The editor reopens at the lesson's address with the caret where the author was typing.
  await expect(editor(page)).toBeFocused();
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
