import { test, expect } from '@playwright/test';
import { apiSession, closeFixture, doc, password, setup, watch } from './course-fixture.mjs';

// Course archive request, archive, unarchive, restore and ownership transfer
// (LMS-A08) through the Studio's owner and administrator screens, against
// the Go application and PostgreSQL (internal/server TestBrowserReaderServer).
// Accounts are simulated TEST users used by no other spec: test-lifecycle-owner
// and test-lifecycle-heir (authors), with the shared test-reviewer,
// test-publisher and test-admin. The public course overview (B07) is not on
// this branch, so readability is checked through the owner's course API.

const account = (name) => ({ email: `${name}@example.test`, password });
const owner = account('test-lifecycle-owner');
const heir = account('test-lifecycle-heir');
const reviewer = account('test-reviewer');
const publisher = account('test-publisher');
const admin = account('test-admin');
const run = Date.now().toString(36);

async function signedIn(browser, baseURL, user, path) {
  const context = await browser.newContext({ baseURL, viewport: { width: 1280, height: 900 } });
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

test.afterAll(closeFixture);

test('the owner requests archival; an administrator archives, unarchives, restores and transfers the course', async ({ browser, baseURL }) => {
  test.setTimeout(180000);
  const f = await setup(browser, baseURL);
  const ow = await apiSession(browser, baseURL, owner);
  const rv = await apiSession(browser, baseURL, reviewer);
  const pu = await apiSession(browser, baseURL, publisher);
  const he = await apiSession(browser, baseURL, heir);

  // A published course of the owner, with their own published lesson, through the API.
  const lessonTitle = `TEST Leçon du cycle de vie ${run}`;
  const lesson = await ow.call('POST', '/api/lessons', { subjectId: f.subject.id, language: 'fr', content: doc(lessonTitle) });
  const lessonSub = await ow.call('POST', `/api/revisions/${lesson.id}/submit`, { expectedVersion: lesson.version });
  await rv.call('POST', `/api/review/revisions/${lessonSub.id}/approve`, {});
  await pu.call('POST', `/api/revisions/${lessonSub.id}/publish`, {});
  const title = `TEST Cours à archiver ${run}`;
  const { course, revision } = await ow.call('POST', '/api/courses', { subjectId: f.subject.id, language: 'fr' });
  const saved = await ow.call('PUT', `/api/course-revisions/${revision.id}`, {
    expectedVersion: revision.version, title, summary: 'TEST Résumé.', objectives: 'TEST Objectifs.', prerequisites: '',
    assignmentIds: [(await f.current()).assignmentId],
    modules: [{ key: null, title: 'TEST Module', chapterId: null, steps: [{ lessonId: lesson.lessonId, revisionId: lesson.id, title: lessonTitle }] }],
  });
  const submitted = await ow.call('POST', `/api/course-revisions/${saved.id}/submit`, { expectedVersion: saved.version });
  await rv.call('POST', `/api/review/course-revisions/${submitted.id}/approve`, {});
  await pu.call('POST', `/api/course-revisions/${submitted.id}/publish`, { expectedPublishedRevisionId: null });
  const mine = async (s) => (await s.call('GET', '/api/courses?mine=1')).courses.find((c) => c.id === course.id);
  expect((await mine(ow)).publishedRevisionId).toBe(submitted.id);

  // 1. The owner requests archival with a reason.
  const o = await signedIn(browser, baseURL, owner, `/studio/courses/${course.id}`);
  await expect(o.page.getByRole('heading', { name: 'Version 1 · Publiée' })).toBeVisible();
  await o.page.getByRole('button', { name: 'Demander l’archivage…' }).click();
  await expect(o.page.getByLabel('Motif de la demande')).toBeFocused();
  await o.page.getByRole('button', { name: 'Envoyer la demande…' }).click();
  await expect(o.page.getByText('Indiquez le motif de la demande : il est obligatoire.')).toBeVisible();
  await o.page.getByLabel('Motif de la demande').fill('TEST Programme abandonné.');
  await o.page.getByRole('button', { name: 'Envoyer la demande…' }).click();
  await o.page.getByRole('dialog', { name: 'Demander l’archivage du cours ?' }).getByRole('button', { name: 'Envoyer la demande' }).click();
  await expect(o.page.getByRole('status').filter({ hasText: 'Demande d’archivage envoyée' })).toBeFocused();

  // 2. The administrator sees the request and archives the course with a reason and a confirmation.
  const a = await signedIn(browser, baseURL, admin, '/studio/admin/courses');
  const request = a.page.getByRole('list', { name: 'Demandes d’archivage' }).getByRole('listitem').filter({ hasText: title });
  await expect(request).toContainText('À traiter');
  await expect(request).toContainText('TEST Programme abandonné.');
  await request.getByRole('link', { name: title }).click();
  await expect(a.page.getByRole('heading', { name: title, level: 1 })).toBeVisible();
  const act = async (button, dialogTitle, confirm, reason, done) => {
    await a.page.getByRole('button', { name: button }).click();
    if (reason) await a.page.getByLabel('Motif', { exact: true }).fill(reason);
    await a.page.getByRole('button', { name: 'Continuer…' }).click();
    await a.page.getByRole('dialog', { name: dialogTitle }).getByRole('button', { name: confirm }).click();
    await expect(a.page.getByRole('status').filter({ hasText: done })).toBeFocused();
  };
  await act('Archiver le cours…', 'Archiver le cours ?', 'Archiver le cours', 'TEST Archivage demandé par le propriétaire.', 'Cours archivé.');
  let state = await mine(ow);
  expect(state.archived).toBe(true);
  expect(state.publishedRevisionId).toBeNull();
  await o.page.reload();
  await expect(o.page.getByRole('heading', { name: 'Cours archivé' })).toBeVisible();
  await expect(o.page.getByRole('heading', { name: 'Version 1 · Publiée' })).toHaveCount(0);
  await expect(a.page.getByRole('button', { name: /Restaurer la version/ })).toHaveCount(0);

  // 3. Unarchiving leaves the course unreadable; a restore publishes it again.
  await act('Désarchiver le cours…', 'Désarchiver le cours ?', 'Désarchiver le cours', 'TEST Archivage par erreur.', 'Cours désarchivé.');
  state = await mine(ow);
  expect(state.archived).toBe(false);
  expect(state.publishedRevisionId).toBeNull();
  await act('Restaurer la version 1…', 'Restaurer une version (version 1) ?', 'Restaurer une version', null, 'Version 1 restaurée');
  state = await mine(ow);
  expect(state.publishedRevisionId).toBe(submitted.id);
  await expect(a.page.getByRole('list', { name: 'Historique du cours' })).toContainText('TEST Archivage par erreur.');

  // 6. With a draft open (started after this page loaded), the transfer is refused with a clear message.
  await ow.call('POST', `/api/courses/${course.id}/drafts`, {});
  await a.page.getByRole('button', { name: 'Transférer le cours…' }).click();
  await a.page.getByLabel('Rechercher un auteur').fill('Héritière');
  await a.page.getByLabel('Nouveau propriétaire').selectOption({ label: 'TEST Héritière Ines' });
  await a.page.getByLabel('Motif', { exact: true }).fill('TEST Départ du propriétaire.');
  await a.page.getByRole('button', { name: 'Continuer…' }).click();
  await a.page.getByRole('dialog', { name: 'Transférer le cours ?' }).getByRole('button', { name: 'Transférer le cours' }).click();
  const refusal = a.page.getByRole('alert').filter({ hasText: 'Transfert refusé : le cours a une version en cours' });
  await expect(refusal).toBeFocused();
  expect((await mine(ow)).ownerId).toBe(ow.me.id);
  await refusal.getByRole('button', { name: 'Recharger l’état actuel' }).click();
  await expect(a.page.getByRole('button', { name: 'Transférer le cours…' })).toBeDisabled();
  await expect(a.page.getByText('Transfert impossible tant qu’une version est en cours')).toBeVisible();
  await act('Supprimer le brouillon 2…', 'Supprimer le brouillon (version 2) ?', 'Supprimer le brouillon', 'TEST Brouillon abandonné avant le transfert.', 'Brouillon (version 2) supprimé.');

  // 4. The administrator transfers the course to another active author, with a reason.
  await a.page.getByRole('button', { name: 'Transférer le cours…' }).click();
  await a.page.getByLabel('Rechercher un auteur').fill('Héritière');
  await a.page.getByLabel('Nouveau propriétaire').selectOption({ label: 'TEST Héritière Ines' });
  await a.page.getByLabel('Motif', { exact: true }).fill('TEST Départ du propriétaire.');
  await a.page.getByRole('button', { name: 'Continuer…' }).click();
  await a.page.getByRole('dialog', { name: 'Transférer le cours ?' }).getByRole('button', { name: 'Transférer le cours' }).click();
  await expect(a.page.getByRole('status').filter({ hasText: 'Cours transféré à TEST Héritière Ines.' })).toBeFocused();
  await expect(a.page.locator('.facts')).toContainText('TEST Héritière Ines');

  // 5. The new owner finds it in "Mes cours" and starts a draft; the previous owner no longer can.
  const h = await signedIn(browser, baseURL, heir, '/studio/courses');
  await h.page.getByRole('link', { name: title }).click();
  await expect(h.page.getByRole('heading', { name: 'Version 1 · Publiée' })).toBeVisible();
  await h.page.getByRole('button', { name: 'Créer une nouvelle version' }).click();
  await expect(h.page.getByText(/Brouillon · version \d+ du cours/)).toBeVisible();
  expect((await mine(he)).ownerId).toBe(he.me.id);
  expect(await mine(ow)).toBeUndefined();
  const refused = await ow.raw('POST', `/api/courses/${course.id}/drafts`, {});
  expect(refused.status()).toBe(404);
  await o.page.reload();
  await expect(o.page.getByText('Cours introuvable : il n’existe pas ou ne vous appartient pas.')).toBeVisible();

  for (const s of [o, a, h]) {
    expect(s.problems).toEqual([]);
    await s.context.close();
  }
  for (const s of [ow, rv, pu, he]) await s.ctx.close();
});
