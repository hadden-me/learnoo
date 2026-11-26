import { test, expect } from '@playwright/test';

// The Studio's reviewer, publisher, administrator and error-report spaces
// (issue #66, second slice), driven through the built Studio served by the
// Go application against the fixture server's PostgreSQL schema
// (internal/server TestBrowserReaderServer). Every account is a simulated
// TEST user created by the fixture, with the test password from
// server_test.go:
//   test-author (author), test-reviewer and test-reviewer-b (reviewers of
//   test-math), test-reviewer-physique (reviewer of test-physique only),
//   test-author-reviewer (author and reviewer of test-math), test-publisher
//   (publisher only), test-admin (administrator only), and the invited
//   student test-student@example.test.
// Every lesson these tests create is titled "TEST …". Desktop Chromium; the
// 360px runs emulate a phone viewport, not a physical phone.
const password = 'simulated-password-1';
const account = (name) => ({ email: `${name}@example.test`, password });
const author = account('test-author');
const reviewerA = account('test-reviewer');
const reviewerB = account('test-reviewer-b');
const reviewerOther = account('test-reviewer-physique');
const authorReviewer = account('test-author-reviewer');
const publisher = account('test-publisher');
const admin = account('test-admin');
const student = account('test-student');

const desktop = { width: 1280, height: 900 };
const phone = { width: 360, height: 780 };

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

/** A fresh browser context signed in as `user`, opened at `path`. */
async function signedIn(browser, baseURL, user, { path = '/studio', viewport = desktop } = {}) {
  const context = await browser.newContext({ baseURL, viewport });
  const page = await context.newPage();
  const problems = watch(page, baseURL);
  await page.goto(path);
  await expect(page).toHaveURL(/\/login\?next=/);
  await page.getByLabel('Adresse e-mail').fill(user.email);
  await page.getByLabel('Mot de passe').fill(user.password);
  await page.getByRole('button', { name: 'Se connecter' }).click();
  await expect(page).not.toHaveURL(/\/login/);
  return { context, page, problems };
}

const nav = (page) => page.getByRole('navigation', { name: 'Studio' });
const editor = (page) => page.getByRole('textbox', { name: 'Éditeur du contenu de la leçon' });
const noOverflow = (page) => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth);

async function writeAndSubmit(page, { title, text, language = 'fr' }) {
  await page.getByRole('link', { name: 'Nouvelle leçon' }).click();
  await page.getByLabel('Matière').selectOption({ label: 'TEST Mathématiques' });
  await page.getByLabel('Chapitre (facultatif)').selectOption({ label: 'TEST Nombres complexes' });
  if (language === 'ar') await page.getByRole('radio', { name: /Arabe/ }).check();
  await page.getByRole('button', { name: 'Commencer la rédaction' }).click();
  await page.getByLabel('Titre', { exact: true }).fill(title);
  await editor(page).click();
  await page.keyboard.type(text);
  await page.getByRole('button', { name: 'Enregistrer', exact: true }).click();
  await expect(page).toHaveURL(/\/studio\/lessons\/\d+\/edit$/);
  await submit(page);
  return Number(/\/studio\/lessons\/(\d+)\/edit/.exec(page.url())[1]);
}

async function submit(page) {
  await page.getByRole('button', { name: 'Soumettre pour relecture' }).click();
  await page.getByRole('dialog', { name: 'Soumettre pour relecture ?' }).getByRole('button', { name: 'Soumettre', exact: true }).click();
  await expect(page.getByRole('heading', { name: /· En relecture/ })).toBeVisible();
}

/** The latest revision of one of the signed-in author's lessons, through the API with the page's session. */
async function latestRevision(page, lessonId) {
  const mine = await (await page.request.get('/api/lessons/mine')).json();
  return mine.lessons.find((l) => l.id === lessonId).latestRevision;
}

async function csrf(page) {
  return (await (await page.request.get('/api/auth/session')).json()).csrfToken;
}

async function openFromQueue(page, title) {
  await nav(page).getByRole('link', { name: 'Relecture' }).click();
  await expect(page.getByRole('heading', { name: 'Relecture', level: 1 })).toBeVisible();
  await page.getByRole('link', { name: title }).click();
  await expect(page.locator('main > h1')).toHaveText(title);
  await expect(page.getByTestId('review-preview').getByRole('heading', { level: 1 })).toHaveText(title);
}

async function approveInUI(page, title, comment = '') {
  await openFromQueue(page, title);
  if (comment) await page.getByLabel('Commentaire pour l’auteur').fill(comment);
  await page.getByRole('button', { name: 'Approuver…' }).click();
  await page.getByRole('dialog').getByRole('button', { name: 'Approuver', exact: true }).click();
  await expect(page.getByText(`Révision approuvée : « ${title} »`)).toBeVisible();
}

async function publishInUI(page, title, { first = null } = {}) {
  await nav(page).getByRole('link', { name: 'Publication' }).click();
  await page.getByRole('link', { name: title }).click();
  await expect(page.getByTestId('publish-preview').getByRole('heading', { level: 1 })).toHaveText(title);
  await page.getByRole('button', { name: 'Publier cette révision…' }).click();
  const dialog = page.getByRole('dialog', { name: `Publier « ${title} » ?` });
  if (first === true) await expect(dialog).toContainText('première publication');
  if (first === false) await expect(dialog).toContainText('remplacera immédiatement');
  await dialog.getByRole('button', { name: 'Publier', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Révision publiée' })).toBeVisible();
}

test('full editorial journey: review, change request, second reviewer, publication, reader report, correction and emergency actions', async ({ browser, baseURL }) => {
  test.setTimeout(180000);
  const title = 'TEST Parcours éditorial';

  // The author writes and submits.
  const a = await signedIn(browser, baseURL, author);
  const lessonId = await writeAndSubmit(a.page, { title, text: 'Le module de $z=3+4i$ vaut 5. Version initiale.' });

  // Reviewer A sees only the spaces they can use, reads the canonical preview and requests changes.
  const r = await signedIn(browser, baseURL, reviewerA);
  await expect(r.page.getByRole('heading', { name: 'Studio', level: 1 })).toBeVisible();
  await expect(nav(r.page).getByRole('link')).toHaveText(['Relecture', 'Signalements']);
  await openFromQueue(r.page, title);
  await expect(r.page.getByTestId('review-preview').locator('.katex').first()).toBeVisible();
  await expect(r.page.locator('.facts')).toContainText('TEST Autrice Amel');
  await expect(r.page.locator('.facts')).toContainText('TEST Mathématiques · TEST Nombres complexes');
  // A change request needs a comment: refused in place, focus on the field.
  await r.page.getByRole('button', { name: 'Demander des modifications…' }).click();
  await expect(r.page.getByText('le commentaire est obligatoire')).toBeVisible();
  await expect(r.page.getByLabel('Commentaire pour l’auteur')).toBeFocused();
  await r.page.getByLabel('Commentaire pour l’auteur').fill('TEST Ajoutez un exemple numérique.');
  await r.page.getByRole('button', { name: 'Demander des modifications…' }).click();
  const confirm = r.page.getByRole('dialog', { name: 'Demander des modifications ?' });
  await expect(confirm).toContainText('TEST Ajoutez un exemple numérique.');
  await confirm.getByRole('button', { name: 'Envoyer la demande' }).click();
  await expect(r.page.getByText(`Modifications demandées pour « ${title} »`)).toBeVisible();
  await expect(r.page.getByRole('link', { name: title })).toHaveCount(0);
  await r.page.screenshot({ path: 'test-results/editorial-review-queue-desktop.png', fullPage: true });

  // The author reads the feedback, drafts the successor and resubmits.
  await a.page.getByRole('link', { name: 'Mes leçons' }).first().click();
  const card = a.page.getByRole('listitem').filter({ hasText: title });
  await expect(card).toContainText('Retour du relecteur à traiter');
  await card.getByRole('link').click();
  await expect(a.page.getByRole('region', { name: 'Le relecteur a demandé des modifications' })).toContainText('TEST Ajoutez un exemple numérique.');
  await a.page.getByRole('button', { name: 'Créer la version corrigée' }).click();
  await editor(a.page).click();
  await a.page.keyboard.press('ControlOrMeta+End');
  await a.page.keyboard.type(' Exemple : z=1+i.');
  await submit(a.page);
  expect((await latestRevision(a.page, lessonId)).number).toBe(2);

  // A different reviewer approves with a comment.
  const b = await signedIn(browser, baseURL, reviewerB);
  await approveInUI(b.page, title, 'TEST Bon pour publication.');

  // The publisher publishes; their navigation has only the publication space.
  const p = await signedIn(browser, baseURL, publisher);
  await expect(nav(p.page).getByRole('link')).toHaveText(['Publication']);
  await nav(p.page).getByRole('link', { name: 'Publication' }).click();
  const queued = p.page.getByRole('listitem').filter({ hasText: title });
  await expect(queued).toContainText('Première publication');
  await expect(queued).toContainText('approuvée par TEST Relectrice Nadia');
  await publishInUI(p.page, title, { first: true });
  const readerLink = p.page.getByRole('link', { name: 'Voir la leçon publiée' });
  const lessonPath = await readerLink.getAttribute('href');
  expect(lessonPath).toMatch(/^\/learn\/lessons\/[0-9a-f-]{36}$/);
  await p.page.screenshot({ path: 'test-results/editorial-published-desktop.png', fullPage: true });

  // The invited student reads, bookmarks and reports an error.
  const s = await signedIn(browser, baseURL, student, { path: lessonPath, viewport: phone });
  await expect(s.page.getByRole('heading', { name: title, level: 1 })).toBeVisible();
  await expect(s.page.locator('.katex').first()).toBeVisible();
  await expect(s.page.locator('.credit')).toContainText('TEST Relectrice Nadia');
  await s.page.getByRole('button', { name: 'Enregistrer' }).click();
  await expect(s.page.getByRole('button', { name: 'Retirer des signets' })).toBeVisible();
  await s.page.getByText('Signaler une erreur').click();
  await s.page.getByLabel('Décrivez l’erreur').fill('TEST Le module de 1+i vaut racine de 2, pas 2.');
  await s.page.getByRole('button', { name: 'Envoyer le signalement' }).click();
  await expect(s.page.getByRole('heading', { name: 'Signalement envoyé' })).toBeVisible();

  // Reviewer A handles the report: a note is required, then it is resolved.
  await nav(r.page).getByRole('link', { name: 'Signalements' }).click();
  const report = r.page.getByRole('listitem').filter({ hasText: 'TEST Le module de 1+i' });
  await expect(report).toContainText('À traiter');
  await report.getByRole('link').click();
  await expect(r.page.locator('.quote').first()).toHaveText('TEST Le module de 1+i vaut racine de 2, pas 2.');
  await r.page.getByRole('button', { name: 'Marquer comme corrigé…' }).click();
  await expect(r.page.getByLabel('Note de traitement (obligatoire)')).toBeFocused();
  await r.page.getByLabel('Note de traitement (obligatoire)').fill('TEST Corrigé dans la révision 3.');
  await r.page.getByRole('button', { name: 'Marquer comme corrigé…' }).click();
  await r.page.getByRole('dialog').getByRole('button', { name: 'Marquer comme corrigé', exact: true }).click();
  await expect(r.page.getByText('Signalement marqué comme corrigé.')).toBeVisible();
  await expect(r.page.getByRole('heading', { name: 'Corrigé', level: 2 })).toBeVisible();
  await expect(r.page.getByRole('button', { name: 'Marquer comme corrigé…' })).toHaveCount(0);

  // The author's correction goes through review and publication again.
  await a.page.getByRole('link', { name: 'Mes leçons' }).first().click();
  await a.page.getByRole('listitem').filter({ hasText: title }).getByRole('link').click();
  await expect(a.page.getByRole('heading', { name: 'Révision 2 · Publiée' })).toBeVisible();
  await a.page.getByRole('button', { name: 'Créer une nouvelle version' }).click();
  await editor(a.page).click();
  await a.page.keyboard.press('ControlOrMeta+End');
  await a.page.keyboard.type(' Correction : le module de 1+i vaut racine de 2.');
  await submit(a.page);
  await approveInUI(r.page, title);
  await nav(p.page).getByRole('link', { name: 'Publication' }).click();
  await expect(p.page.getByRole('listitem').filter({ hasText: title })).toContainText('Remplacera la version publiée');
  await publishInUI(p.page, title, { first: false });
  await s.page.goto(lessonPath);
  await expect(s.page.locator('article')).toContainText('Correction : le module');

  // The administrator finds the lesson from its reader address.
  const ad = await signedIn(browser, baseURL, admin);
  await expect(nav(ad.page).getByRole('link')).toHaveText(['Signalements', 'Administration']);
  await nav(ad.page).getByRole('link', { name: 'Administration' }).click();
  await ad.page.getByLabel('Retrouver une leçon').fill(`${baseURL}${lessonPath}`);
  await ad.page.getByRole('button', { name: 'Ouvrir' }).click();
  await expect(ad.page.getByRole('heading', { name: title, level: 1 })).toBeVisible();
  await expect(ad.page.getByRole('heading', { name: 'Version publiée : révision 3' })).toBeVisible();
  const publicationURL = ad.page.url();
  const revision = (n) => ad.page.getByRole('listitem', { name: `Révision ${n}`, exact: true });
  await expect(revision(1)).toContainText('Modifications demandées');
  await expect(revision(1).getByRole('button')).toHaveCount(0);
  await expect(revision(2)).toContainText('Approuvée, publiée auparavant');

  // Withdrawal of the published revision: reason required, the expected pointer and its effect are shown.
  await revision(3).getByRole('button', { name: 'Retirer cette révision…' }).click();
  await expect(ad.page.getByLabel('Motif (obligatoire)')).toBeFocused();
  await revision(3).getByRole('button', { name: 'Retirer…' }).click();
  await expect(ad.page.getByText('Indiquez un motif')).toBeVisible();
  await ad.page.getByLabel('Motif (obligatoire)').fill('TEST Erreur de calcul dans la correction.');
  await revision(3).getByRole('button', { name: 'Retirer…' }).click();
  let dialog = ad.page.getByRole('dialog', { name: 'Retirer la révision 3 ?' });
  await expect(dialog).toContainText('Version publiée attendue : la révision 3.');
  await expect(dialog).toContainText('la leçon sera aussi dépubliée');
  await expect(dialog.getByRole('button', { name: 'Ne rien changer' })).toBeFocused();
  await dialog.getByRole('button', { name: 'Retirer définitivement' }).click();
  await expect(ad.page.getByText('Révision 3 retirée définitivement.')).toBeFocused();
  await expect(ad.page.getByRole('heading', { name: 'Aucune version publiée' })).toBeVisible();
  await expect(revision(3)).toContainText('Retirée définitivement');
  await expect(revision(3)).toContainText('TEST Erreur de calcul dans la correction.');
  await expect(revision(3).getByRole('button')).toHaveCount(0);

  // Restoration of the earlier publication.
  await revision(2).getByRole('button', { name: 'Restaurer cette révision…' }).click();
  dialog = ad.page.getByRole('dialog', { name: 'Restaurer la révision 2 ?' });
  await expect(dialog).toContainText('Version publiée attendue : aucune version publiée.');
  await dialog.getByRole('button', { name: 'Restaurer' }).click();
  await expect(ad.page.getByRole('heading', { name: 'Version publiée : révision 2' })).toBeVisible();

  // A second tab keeps the old state; the first unpublishes in an emergency.
  const stale = await ad.context.newPage();
  await stale.goto(publicationURL);
  await expect(stale.getByRole('heading', { name: 'Version publiée : révision 2' })).toBeVisible();
  await ad.page.getByRole('button', { name: 'Dépublier en urgence…' }).click();
  await ad.page.getByLabel('Motif (obligatoire)').fill('TEST Vérification en urgence.');
  await ad.page.getByRole('button', { name: 'Dépublier…' }).click();
  await ad.page.getByRole('dialog', { name: 'Dépublier la leçon ?' }).getByRole('button', { name: 'Dépublier', exact: true }).click();
  await expect(ad.page.getByRole('heading', { name: 'Aucune version publiée' })).toBeVisible();
  await expect(ad.page.getByText('Leçon dépubliée')).toBeFocused();
  await s.page.goto(lessonPath);
  await expect(s.page.getByRole('heading', { name: title })).toHaveCount(0);

  // The stale tab's action is refused by the server's expected-pointer check; nothing changes.
  // (Withdrawing revision 2 there names revision 2 as the expected published revision.)
  const staleRevision2 = stale.getByRole('listitem', { name: 'Révision 2', exact: true });
  await staleRevision2.getByRole('button', { name: 'Retirer cette révision…' }).click();
  await stale.getByLabel('Motif (obligatoire)').fill('TEST Second onglet.');
  await staleRevision2.getByRole('button', { name: 'Retirer…' }).click();
  await expect(stale.getByRole('dialog')).toContainText('Version publiée attendue : la révision 2.');
  await stale.getByRole('dialog').getByRole('button', { name: 'Retirer définitivement' }).click();
  await expect(stale.getByRole('alert')).toContainText('a changé depuis l’ouverture de la page');
  await expect(stale.getByRole('alert')).toContainText('Rien n’a été modifié');
  await stale.screenshot({ path: 'test-results/editorial-stale-pointer-desktop.png', fullPage: true });
  await stale.getByRole('alert').getByRole('button', { name: 'Recharger l’état actuel' }).click();
  await expect(stale.getByRole('heading', { name: 'Aucune version publiée' })).toBeVisible();
  await stale.close();

  // Restore again: students read revision 2.
  await revision(2).getByRole('button', { name: 'Restaurer cette révision…' }).click();
  await ad.page.getByRole('dialog').getByRole('button', { name: 'Restaurer' }).click();
  await expect(ad.page.getByRole('heading', { name: 'Version publiée : révision 2' })).toBeVisible();
  const history = ad.page.getByRole('list', { name: /Historique de publication/ });
  await expect(history.getByRole('listitem')).toHaveCount(6);
  await expect(history.getByRole('listitem').nth(1)).toContainText('Dépublication');
  await expect(history.getByRole('listitem').nth(1)).toContainText('TEST Vérification en urgence.');
  await ad.page.screenshot({ path: 'test-results/editorial-admin-desktop.png', fullPage: true });
  await s.page.goto(lessonPath);
  await expect(s.page.locator('article')).toContainText('Exemple : z=1+i.');
  await expect(s.page.locator('article')).not.toContainText('Correction : le module');

  // The publisher may restore too, but sees no withdrawal or unpublication control.
  await p.page.goto(publicationURL);
  await expect(p.page.getByRole('heading', { name: 'Version publiée : révision 2' })).toBeVisible();
  await expect(p.page.getByRole('button', { name: /Retirer|Dépublier/ })).toHaveCount(0);
  // The publisher view has what restoration needs only: no audit log, reasons, names or unapproved revisions.
  await expect(p.page.getByText('Vue éditeur')).toBeVisible();
  await expect(p.page.getByRole('list', { name: /Historique de publication/ })).toHaveCount(0);
  for (const hidden of ['TEST Vérification en urgence.', 'TEST Erreur de calcul', 'TEST Autrice Amel', 'TEST Admin Lina', 'Modifications demandées']) {
    await expect(p.page.getByText(hidden)).toHaveCount(0);
  }
  await expect(p.page.getByRole('listitem', { name: 'Révision 3', exact: true })).toContainText('Retirée définitivement');
  await expect(p.page.getByRole('listitem', { name: 'Révision 1', exact: true })).toHaveCount(0);

  for (const who of [a, r, b, p, s, ad]) {
    expect(who.problems).toEqual([]);
    await who.context.close();
  }
});

test('unauthorized accounts see no editorial controls, and the server refuses them anyway', async ({ browser, baseURL }) => {
  // A student has no Studio at all: the server answers its 403 page.
  const s = await signedIn(browser, baseURL, student);
  await expect(s.page.getByRole('heading', { name: 'Accès réservé' })).toBeVisible();
  expect((await s.page.request.get('/studio')).status()).toBe(403);
  await expect(s.page.getByRole('navigation', { name: 'Studio' })).toHaveCount(0);
  expect((await s.page.request.get('/api/review/queue')).status()).toBe(403);

  // An author (no reviewer assignment) has no review space.
  const a = await signedIn(browser, baseURL, author);
  await expect(nav(a.page).getByRole('link')).toHaveText(['Mes leçons', 'Mes cours']);
  const lessonId = await writeAndSubmit(a.page, { title: 'TEST Hors de portée', text: 'Texte relu par personne ici.' });
  const inReview = await latestRevision(a.page, lessonId);
  await a.page.goto('/studio/review');
  await expect(a.page.getByRole('heading', { name: 'Accès refusé' })).toBeVisible();
  await expect(a.page.getByText('« Relecture »')).toBeVisible();
  await a.page.goto('/studio/admin');
  await expect(a.page.getByRole('heading', { name: 'Accès refusé' })).toBeVisible();
  const token = await csrf(a.page);
  const approve = await a.page.request.post(`/api/review/revisions/${inReview.id}/approve`, { data: {}, headers: { 'X-CSRF-Token': token } });
  expect(approve.status()).toBe(403);

  // A reviewer of another subject sees nothing of test-math.
  const o = await signedIn(browser, baseURL, reviewerOther);
  await nav(o.page).getByRole('link', { name: 'Relecture' }).click();
  await expect(o.page.getByText('Aucune révision n’attend de relecture dans vos matières.')).toBeVisible();
  await o.page.goto(`/studio/review/revisions/${inReview.id}`);
  await expect(o.page.getByRole('heading', { name: 'Révision indisponible' })).toBeVisible();
  await expect(o.page.getByRole('button', { name: /Approuver/ })).toHaveCount(0);
  await nav(o.page).getByRole('link', { name: 'Signalements' }).click();
  await o.page.getByLabel('Afficher').selectOption({ label: 'Tous' });
  await expect(o.page.getByText('Aucun signalement dans cette catégorie.')).toBeVisible();

  // An author who reviews the subject cannot review their own revision.
  const ar = await signedIn(browser, baseURL, authorReviewer);
  await expect(nav(ar.page).getByRole('link')).toHaveText(['Mes leçons', 'Mes cours', 'Relecture', 'Signalements']);
  const ownLesson = await writeAndSubmit(ar.page, { title: 'TEST Ma propre leçon', text: 'Texte de l’autrice relectrice.' });
  const own = await latestRevision(ar.page, ownLesson);
  await nav(ar.page).getByRole('link', { name: 'Relecture' }).click();
  await expect(ar.page.getByRole('heading', { name: 'Relecture', level: 1 })).toBeVisible();
  await expect(ar.page.getByRole('link', { name: 'TEST Ma propre leçon' })).toHaveCount(0);
  await ar.page.goto(`/studio/review/revisions/${own.id}`);
  await expect(ar.page.getByRole('heading', { name: 'Vous êtes l’auteur de cette révision' })).toBeVisible();
  await expect(ar.page.getByRole('button', { name: /Approuver|Demander des modifications/ })).toHaveCount(0);
  const self = await ar.page.request.post(`/api/review/revisions/${own.id}/approve`, { data: {}, headers: { 'X-CSRF-Token': await csrf(ar.page) } });
  expect(self.status()).toBe(403);
  expect((await self.json()).error.code).toBe('self_review');

  for (const who of [s, a, o, ar]) {
    expect(who.problems).toEqual([]);
    await who.context.close();
  }
});

test('an Arabic lesson is reviewed right to left at 360px with the keyboard, and published at 360px', async ({ browser, baseURL }) => {
  const title = 'TEST درس للمراجعة';
  const a = await signedIn(browser, baseURL, author, { viewport: phone });
  await writeAndSubmit(a.page, { title, text: 'نص تجريبي مع صيغة $x^2+1$ هنا.', language: 'ar' });

  const r = await signedIn(browser, baseURL, reviewerB, { viewport: phone });
  expect(await noOverflow(r.page)).toBe(true);
  await openFromQueue(r.page, title);
  const heading = r.page.locator('main > h1');
  expect(await heading.evaluate((el) => getComputedStyle(el).direction)).toBe('rtl');
  const preview = r.page.getByTestId('review-preview');
  await expect(preview.locator('.katex').first()).toBeVisible();
  expect(await preview.locator('article').evaluate((el) => getComputedStyle(el).direction)).toBe('rtl');
  expect(await preview.locator('.math').first().evaluate((el) => getComputedStyle(el).direction)).toBe('ltr');
  expect(await noOverflow(r.page)).toBe(true);
  await r.page.screenshot({ path: 'test-results/editorial-review-arabic-mobile.png', fullPage: true });

  // Keyboard only: type a comment, open the confirmation, back out with Escape, then confirm.
  await r.page.getByLabel('Commentaire pour l’auteur').focus();
  await r.page.keyboard.type('TEST ممتاز');
  await r.page.keyboard.press('Tab');
  await expect(r.page.getByRole('button', { name: 'Approuver…' })).toBeFocused();
  await r.page.keyboard.press('Enter');
  const dialog = r.page.getByRole('dialog', { name: 'Approuver la révision 1 ?' });
  await expect(dialog.getByRole('button', { name: 'Revenir à la relecture' })).toBeFocused();
  await r.page.keyboard.press('Escape');
  await expect(dialog).toHaveCount(0);
  await expect(r.page.getByRole('button', { name: 'Approuver…' })).toBeFocused();
  await r.page.keyboard.press('Enter');
  await r.page.keyboard.press('Shift+Tab');
  await expect(dialog.getByRole('button', { name: 'Approuver', exact: true })).toBeFocused();
  await r.page.keyboard.press('Enter');
  await expect(r.page.getByText(`Révision approuvée : « ${title} »`)).toBeVisible();

  const p = await signedIn(browser, baseURL, publisher, { viewport: phone });
  await nav(p.page).getByRole('link', { name: 'Publication' }).click();
  expect(await noOverflow(p.page)).toBe(true);
  await publishInUI(p.page, title);
  expect(await noOverflow(p.page)).toBe(true);
  await p.page.getByRole('link', { name: 'Historique de publication' }).click();
  await expect(p.page.getByRole('heading', { name: 'Version publiée : révision 1' })).toBeVisible();
  expect(await p.page.getByRole('heading', { name: title, level: 1 }).evaluate((el) => getComputedStyle(el).direction)).toBe('rtl');
  expect(await noOverflow(p.page)).toBe(true);
  await p.page.screenshot({ path: 'test-results/editorial-publication-arabic-mobile.png', fullPage: true });

  const ad = await signedIn(browser, baseURL, admin, { viewport: phone, path: '/studio/admin' });
  await expect(ad.page.getByRole('link', { name: title })).toBeVisible();
  expect(await noOverflow(ad.page)).toBe(true);
  await ad.page.getByRole('link', { name: title }).click();
  await expect(ad.page.getByRole('button', { name: 'Dépublier en urgence…' })).toBeVisible();
  expect(await noOverflow(ad.page)).toBe(true);

  for (const who of [a, r, p, ad]) {
    expect(who.problems).toEqual([]);
    await who.context.close();
  }
});

/** An API session for test setup only (the UI is what is under test). */
async function apiSession(browser, baseURL, user) {
  const context = await browser.newContext({ baseURL });
  const login = await context.request.post('/api/auth/login', { data: user });
  expect(login.status()).toBe(200);
  const { csrfToken } = await login.json();
  const call = async (method, path, data) => {
    const res = await context.request.fetch(path, { method, data, headers: { 'X-CSRF-Token': csrfToken } });
    expect(res.status(), `${method} ${path}: ${await res.text()}`).toBeLessThan(300);
    return res.json();
  };
  return { context, call };
}

const doc = (title, text) => ({ schemaVersion: 1, title, language: 'fr', status: 'unreviewed', objective: '', blocks: [{ type: 'paragraph', children: [{ type: 'text', text }] }] });

test('stale base, a stale restore by a publisher, withdrawal of a pending candidate, and an account switch', async ({ browser, baseURL }) => {
  const title = 'TEST Base périmée';
  // Setup through the API: revisions 1 and 2 published, revision 3 approved and pending.
  const au = await apiSession(browser, baseURL, author);
  const rv = await apiSession(browser, baseURL, reviewerA);
  const pu = await apiSession(browser, baseURL, publisher);
  const adm = await apiSession(browser, baseURL, admin);
  const { subjects } = await au.call('GET', '/api/catalogue/subjects');
  const subjectId = subjects.find((x) => x.code === 'test-math').id;
  let rev = await au.call('POST', '/api/lessons', { subjectId, language: 'fr', content: doc(title, 'Édition 1.') });
  const lessonId = rev.lessonId;
  const cycle = async (r, publish) => {
    const submitted = await au.call('POST', `/api/revisions/${r.id}/submit`, { expectedVersion: r.version });
    await rv.call('POST', `/api/review/revisions/${submitted.id}/approve`, {});
    if (publish) await pu.call('POST', `/api/revisions/${submitted.id}/publish`, {});
    return submitted;
  };
  const r1 = await cycle(rev, true);
  const r2 = await cycle(await au.call('POST', `/api/lessons/${lessonId}/drafts`, { content: doc(title, 'Édition 2.') }), true);
  await cycle(await au.call('POST', `/api/lessons/${lessonId}/drafts`, { content: doc(title, 'Édition 3.') }), false);

  // The publisher opens the lesson (revision 2 published) and offers to restore revision 1.
  const p = await signedIn(browser, baseURL, publisher, { path: `/studio/publication/${lessonId}` });
  await expect(p.page.getByRole('heading', { name: 'Version publiée : révision 2' })).toBeVisible();
  // Meanwhile an administrator unpublishes; the publisher's restore names revision 2 and is refused.
  await adm.call('POST', `/api/lessons/${lessonId}/unpublish`, { reason: 'TEST Urgence ailleurs.', expectedPublishedRevisionId: r2.id });
  await p.page.getByRole('listitem', { name: 'Révision 1', exact: true }).getByRole('button', { name: 'Restaurer cette révision…' }).click();
  await expect(p.page.getByRole('dialog')).toContainText('Version publiée attendue : la révision 2.');
  await p.page.getByRole('dialog').getByRole('button', { name: 'Restaurer' }).click();
  await expect(p.page.getByRole('alert')).toContainText('a changé depuis l’ouverture de la page');
  await p.page.getByRole('alert').getByRole('button', { name: 'Recharger l’état actuel' }).click();
  await expect(p.page.getByRole('heading', { name: 'Aucune version publiée' })).toBeVisible();

  // Revision 3's base (revision 2) is no longer published: flagged, and no publish button.
  await nav(p.page).getByRole('link', { name: 'Publication' }).click();
  const card = p.page.getByRole('listitem').filter({ hasText: title });
  await expect(card).toContainText('Base périmée : publication impossible');
  await card.getByRole('link', { name: title }).click();
  await expect(p.page.getByRole('heading', { name: 'Publication impossible' })).toBeVisible();
  await expect(p.page.getByRole('button', { name: 'Publier cette révision…' })).toHaveCount(0);

  // The administrator withdraws the pending, never-published candidate: the lesson stays unpublished.
  const ad = await signedIn(browser, baseURL, admin, { path: `/studio/publication/${lessonId}` });
  const r3 = ad.page.getByRole('listitem', { name: 'Révision 3', exact: true });
  await expect(r3).toContainText('Approuvée, en attente de publication');
  await r3.getByRole('button', { name: 'Retirer cette révision…' }).click();
  await ad.page.getByLabel('Motif (obligatoire)').fill('TEST Base périmée, à réécrire.');
  await r3.getByRole('button', { name: 'Retirer…' }).click();
  const dialog = ad.page.getByRole('dialog', { name: 'Retirer la révision 3 ?' });
  await expect(dialog).toContainText('Version publiée attendue : aucune version publiée.');
  await expect(dialog).not.toContainText('dépubliée');
  await dialog.getByRole('button', { name: 'Retirer définitivement' }).click();
  await expect(ad.page.getByText('Révision 3 retirée définitivement.')).toBeFocused();
  await expect(r3).toContainText('Retirée définitivement');
  await nav(p.page).getByRole('link', { name: 'Publication' }).click();
  await expect(p.page.getByRole('heading', { name: 'Publication', level: 1 })).toBeVisible();
  await expect(p.page.getByRole('link', { name: title })).toHaveCount(0);

  // Another account signs in on the administrator's browser: the open page sends nothing.
  await ad.page.getByRole('listitem', { name: 'Révision 1', exact: true }).getByRole('button', { name: 'Restaurer cette révision…' }).click();
  const other = await ad.context.request.post('/api/auth/login', { data: reviewerB });
  expect(other.status()).toBe(200);
  await ad.page.getByRole('dialog').getByRole('button', { name: 'Restaurer' }).click();
  await expect(ad.page.getByRole('alert').filter({ hasText: 'Un autre compte est connecté' }).first()).toBeVisible();
  const after = await adm.call('GET', `/api/lessons/${lessonId}/publication`);
  expect(after.publishedRevisionId).toBeNull();
  expect(r1.id).toBeGreaterThan(0);

  for (const who of [p, ad]) {
    expect(who.problems).toEqual([]);
    await who.context.close();
  }
  for (const c of [au, rv, pu, adm]) await c.context.close();
});
