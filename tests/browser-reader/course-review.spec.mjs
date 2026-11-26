import { test, expect } from '@playwright/test';
import { apiSession, closeFixture, doc, noOverflow, password, setup, watch } from './course-fixture.mjs';

// Studio course submission, review, publication and administrator recovery
// (LMS-B06) against the Go application and PostgreSQL (internal/server
// TestBrowserReaderServer), under the real CSP. Accounts are the fixture's
// simulated TEST users: test-author, test-reviewer and test-reviewer-b
// (reviewers of test-math), test-author-reviewer (author and reviewer of
// test-math), test-publisher, test-admin, and test-svt-a / test-svt-b (the
// only reviewers of test-svt, both authors). Lessons are created, reviewed
// and published through the API (the lesson workflow has its own spec);
// every course and lesson is titled "TEST …". Desktop Chromium; the 360px
// check emulates a phone viewport, not a phone.

const account = (name) => ({ email: `${name}@example.test`, password });
const author = account('test-author');
const reviewerA = account('test-reviewer');
const reviewerB = account('test-reviewer-b');
const authorReviewer = account('test-author-reviewer');
const publisher = account('test-publisher');
const admin = account('test-admin');
const svtA = account('test-svt-a');
const svtB = account('test-svt-b');

const run = Date.now().toString(36);
const nav = (page) => page.getByRole('navigation', { name: 'Studio' });

async function signedIn(browser, baseURL, user, path = '/studio') {
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

/** A lesson written by `by`, approved by `reviewer` and published, through the API. */
async function publishedLesson(by, reviewer, pub, subjectId, title) {
  const rev = await by.call('POST', '/api/lessons', { subjectId, language: 'fr', content: doc(title) });
  const sub = await by.call('POST', `/api/revisions/${rev.id}/submit`, { expectedVersion: rev.version });
  await reviewer.call('POST', `/api/review/revisions/${sub.id}/approve`, {});
  await pub.call('POST', `/api/revisions/${sub.id}/publish`, {});
  return { lessonId: rev.lessonId, revisionId: rev.id, title };
}

/** A newer published revision of a lesson, with one more paragraph. */
async function newerRevision(by, reviewer, pub, lesson, extra) {
  const content = doc(lesson.title);
  content.blocks.push({ type: 'paragraph', children: [{ type: 'text', text: extra }] });
  const rev = await by.call('POST', `/api/lessons/${lesson.lessonId}/drafts`, { content, prerequisites: '', chapterId: null, mediaIds: [] });
  const sub = await by.call('POST', `/api/revisions/${rev.id}/submit`, { expectedVersion: rev.version });
  await reviewer.call('POST', `/api/review/revisions/${sub.id}/approve`, {});
  await pub.call('POST', `/api/revisions/${sub.id}/publish`, {});
  return rev;
}

/** A complete course of `by` pinning the given lessons in one module, submitted, through the API. */
async function submittedCourse(by, subjectId, title, lessons, assignmentIds) {
  const { revision } = await by.call('POST', '/api/courses', { subjectId, language: 'fr' });
  const saved = await by.call('PUT', `/api/course-revisions/${revision.id}`, {
    expectedVersion: revision.version, title, summary: 'TEST Résumé.', objectives: 'TEST Objectifs.', prerequisites: '', assignmentIds,
    modules: [{ key: null, title: 'TEST Module', chapterId: null, steps: lessons.map((l) => ({ lessonId: l.lessonId, revisionId: l.revisionId, title: l.title })) }],
  });
  return by.call('POST', `/api/course-revisions/${saved.id}/submit`, { expectedVersion: saved.version });
}

test.afterAll(closeFixture);

test('course submission, change request, correction, approval, publication, amendment, suspension and restoration', async ({ browser, baseURL }) => {
  test.setTimeout(240000);
  const f = await setup(browser, baseURL);
  const au = await apiSession(browser, baseURL, author);
  const ra = await apiSession(browser, baseURL, reviewerA);
  const pu = await apiSession(browser, baseURL, publisher);
  const lessonOne = await publishedLesson(au, ra, pu, f.subject.id, `TEST Leçon un ${run}`);
  const lessonTwo = await publishedLesson(au, ra, pu, f.subject.id, `TEST Leçon deux ${run}`);
  const courseTitle = `TEST Cours relu ${run}`;
  const current = await f.current();

  // 1. The author creates the course in the Studio with two of their own published lessons and submits it.
  const a = await signedIn(browser, baseURL, author, '/studio/courses/new');
  await a.page.getByLabel('Matière').selectOption({ label: 'TEST Mathématiques' });
  await a.page.getByRole('radio', { name: /Français/ }).check();
  await a.page.getByRole('button', { name: 'Créer le cours' }).click();
  await expect(a.page).toHaveURL(/\/studio\/courses\/\d+$/);
  const courseId = Number(new URL(a.page.url()).pathname.split('/').pop());
  await a.page.getByLabel('Titre du cours').fill(courseTitle);
  await a.page.getByLabel('Description courte (publique)').fill('TEST Un cours relu de bout en bout.');
  await a.page.getByLabel('Niveau').selectOption({ label: 'TEST Baccalauréat' });
  await a.page.getByLabel('Section', { exact: true }).selectOption({ label: 'TEST Section sciences mathématiques' });
  await a.page.getByRole('button', { name: `Ajouter l’édition en vigueur « ${current.labelFr} »` }).click();
  await a.page.getByLabel('Titre du nouveau module').fill('TEST Module un');
  await a.page.keyboard.press('Enter');
  for (const lesson of [lessonOne, lessonTwo]) {
    await a.page.getByRole('button', { name: 'Ajouter une leçon au module 1 : TEST Module un' }).click();
    const dialog = a.page.getByRole('dialog', { name: 'Ajouter une leçon au module « TEST Module un »' });
    await dialog.getByRole('searchbox').fill(lesson.title);
    await dialog.getByRole('button', { name: 'Rechercher' }).click();
    await dialog.getByRole('button', { name: new RegExp(`${lesson.title} · votre leçon`) }).click();
    await expect(dialog.getByLabel('Révision à épingler')).toContainText('publiable dans un cours');
    await dialog.getByRole('button', { name: 'Ajouter l’étape' }).click();
    await expect(dialog).toHaveCount(0);
  }
  await a.page.getByRole('button', { name: 'Enregistrer', exact: true }).click();
  await expect(a.page.locator('.save-state')).toHaveText(/Enregistré sur le serveur/);
  // The objectives are missing: the server's refusal is shown on the field, nothing is submitted.
  await a.page.getByRole('button', { name: 'Soumettre pour relecture…' }).click();
  await a.page.getByRole('dialog', { name: 'Soumettre la version 1 du cours pour relecture ?' }).getByRole('button', { name: 'Soumettre', exact: true }).click();
  const problemsList = a.page.getByRole('list', { name: 'Points à corriger' });
  await expect(problemsList).toContainText('Objectifs d’apprentissage : obligatoires pour soumettre.');
  await expect(a.page.getByLabel('Objectifs d’apprentissage')).toHaveAttribute('aria-invalid', 'true');
  await expect(a.page.getByRole('alert').filter({ hasText: 'Soumission impossible' })).toBeVisible();
  await a.page.getByLabel('Objectifs d’apprentissage').fill('TEST Savoir calculer.');
  await a.page.getByRole('button', { name: 'Enregistrer', exact: true }).click();
  await expect(a.page.locator('.save-state')).toHaveText(/Enregistré sur le serveur/);
  await a.page.getByRole('button', { name: 'Soumettre pour relecture…' }).click();
  await a.page.getByRole('dialog', { name: 'Soumettre la version 1 du cours pour relecture ?' }).getByRole('button', { name: 'Soumettre', exact: true }).click();
  await expect(a.page.getByRole('heading', { name: 'Version 1 · En relecture' })).toBeVisible();
  await expect(a.page.getByText('Version 1 soumise pour relecture.')).toBeVisible();

  // 2. Reviewer A finds it in the mixed queue, filtered to courses, reads the snapshot and requests changes.
  const r = await signedIn(browser, baseURL, reviewerA);
  await nav(r.page).getByRole('link', { name: 'Relecture' }).click();
  await r.page.getByLabel('Type de contenu').selectOption({ label: 'Cours' });
  const card = r.page.getByRole('listitem').filter({ hasText: courseTitle });
  await expect(card).toContainText('Cours');
  await expect(card).toContainText('Vous pouvez décider');
  await expect(card).toContainText('soumise aujourd’hui');
  await card.getByRole('link', { name: courseTitle }).click();
  await expect(r.page.getByRole('heading', { name: courseTitle, level: 1 })).toBeVisible();
  await expect(r.page.getByText('Première version du cours : toutes les étapes sont nouvelles.')).toBeVisible();
  await expect(r.page.locator('.review-step[data-change="added"]')).toHaveCount(2);
  await expect(r.page.getByRole('region', { name: /Présentation/ })).toContainText(current.labelFr);
  await r.page.getByRole('button', { name: 'Lire la révision 1 de la leçon' }).first().click();
  await expect(r.page.getByTestId(`step-preview-${lessonOne.lessonId}`).getByRole('heading', { level: 1 })).toHaveText(lessonOne.title);
  await r.page.getByRole('button', { name: 'Demander des modifications…' }).click();
  await expect(r.page.getByLabel('Commentaire pour l’auteur')).toBeFocused();
  await expect(r.page.getByText('le commentaire est obligatoire pour renvoyer le cours')).toBeVisible();
  await r.page.getByLabel('Commentaire pour l’auteur').fill('TEST Précisez le résumé.');
  await r.page.getByRole('button', { name: 'Demander des modifications…' }).click();
  await r.page.getByRole('dialog', { name: 'Demander des modifications ?' }).getByRole('button', { name: 'Envoyer la demande' }).click();
  await expect(r.page.getByText(`Modifications demandées pour le cours « ${courseTitle} »`)).toBeVisible();

  // The author sees the feedback and starts the correction, then resubmits it.
  await a.page.reload();
  await expect(a.page.getByRole('heading', { name: 'Version 1 · Modifications demandées' })).toBeVisible();
  await expect(a.page.getByText('TEST Précisez le résumé.')).toBeVisible();
  await a.page.getByRole('button', { name: 'Créer la version corrigée' }).click();
  await expect(a.page.getByText('Brouillon · version 2 du cours')).toBeVisible();
  await a.page.getByLabel('Description courte (publique)').fill('TEST Un cours relu de bout en bout, résumé précisé.');
  await a.page.getByRole('button', { name: 'Enregistrer', exact: true }).click();
  await expect(a.page.locator('.save-state')).toHaveText(/Enregistré sur le serveur/);
  await a.page.getByRole('button', { name: 'Soumettre pour relecture…' }).click();
  await a.page.getByRole('dialog', { name: 'Soumettre la version 2 du cours pour relecture ?' }).getByRole('button', { name: 'Soumettre', exact: true }).click();
  await expect(a.page.getByRole('heading', { name: 'Version 2 · En relecture' })).toBeVisible();

  // Reviewer B (eligible) sees the earlier feedback and approves, at phone width without overflow.
  const b = await signedIn(browser, baseURL, reviewerB);
  await nav(b.page).getByRole('link', { name: 'Relecture' }).click();
  await b.page.getByRole('link', { name: courseTitle }).click();
  await expect(b.page.getByRole('heading', { name: 'Décisions précédentes sur ce cours' })).toBeVisible();
  await expect(b.page.getByText('TEST Précisez le résumé.')).toBeVisible();
  await b.page.setViewportSize({ width: 360, height: 800 });
  expect(await noOverflow(b.page)).toBe(true);
  await b.page.setViewportSize({ width: 1280, height: 900 });
  await b.page.getByRole('button', { name: 'Approuver le cours…' }).click();
  await b.page.getByRole('dialog', { name: 'Approuver la version 2 du cours ?' }).getByRole('button', { name: 'Approuver', exact: true }).click();
  await expect(b.page.getByText(`Cours approuvé : « ${courseTitle} »`)).toBeVisible();

  // The publisher publishes from the course queue.
  const p = await signedIn(browser, baseURL, publisher);
  await nav(p.page).getByRole('link', { name: 'Publication' }).click();
  const queued = p.page.getByRole('list', { name: 'Cours approuvés à publier' }).getByRole('listitem').filter({ hasText: courseTitle });
  await expect(queued).toContainText('Aucune version publiée');
  await queued.getByRole('link', { name: courseTitle }).click();
  await p.page.getByRole('button', { name: 'Publier ce cours…' }).click();
  await p.page.getByRole('dialog', { name: `Publier le cours « ${courseTitle} » ?` }).getByRole('button', { name: 'Publier', exact: true }).click();
  await expect(p.page.getByRole('heading', { name: 'Cours publié' })).toBeVisible();
  await expect(p.page.getByRole('status').filter({ hasText: 'Cours publié' })).toBeFocused();
  await a.page.reload();
  await expect(a.page.getByRole('heading', { name: 'Version 2 · Publiée' })).toBeVisible();

  // 5. Amendment: a newer revision of lesson two is published; the owner is told and amends.
  await newerRevision(au, ra, pu, lessonTwo, 'TEST Paragraphe ajouté.');
  await a.page.reload();
  const notice = a.page.getByRole('region', { name: 'Des leçons du cours publié ont une révision plus récente' });
  await expect(notice).toContainText(`« ${lessonTwo.title} » : révision 1 épinglée, révision 2 disponible`);
  // With a draft open, the amendment is refused and the reason named.
  await a.page.getByRole('button', { name: 'Créer une nouvelle version' }).click();
  await expect(a.page.getByText('Brouillon · version 3 du cours')).toBeVisible();
  await notice.getByRole('button', { name: 'Créer un amendement' }).click();
  await expect(notice.getByRole('alert')).toContainText('Un brouillon de ce cours est déjà ouvert');
  await a.page.getByRole('button', { name: 'Supprimer ce brouillon' }).click();
  await a.page.getByRole('dialog', { name: 'Supprimer ce brouillon ?' }).getByRole('button', { name: 'Supprimer le brouillon' }).click();
  await expect(a.page).toHaveURL(/\/studio\/courses$/);
  await a.page.getByRole('link', { name: courseTitle }).click();
  await expect(notice.getByRole('checkbox')).toBeChecked();
  await notice.getByRole('button', { name: 'Créer un amendement' }).click();
  await expect(a.page.getByText(/Amendement créé/)).toBeVisible();
  await expect(a.page.getByText(/amendement \(remplacement de révisions de leçons\)/)).toBeVisible();
  await a.page.getByRole('button', { name: 'Soumettre pour relecture…' }).click();
  await a.page.getByRole('dialog', { name: /Soumettre la version \d+ du cours pour relecture \?/ }).getByRole('button', { name: 'Soumettre', exact: true }).click();
  await expect(a.page.getByRole('heading', { name: /Version \d+ · En relecture/ })).toBeVisible();

  // The review shows only the replaced step as changed, with its content diff; reviewer B approves; published.
  await b.page.goto('/studio/review');
  const amendmentCard = b.page.getByRole('listitem').filter({ hasText: courseTitle });
  await expect(amendmentCard).toContainText('Cours · amendement');
  await amendmentCard.getByRole('link', { name: courseTitle }).click();
  await expect(b.page.getByText('1 étape remplacée · 1 étape inchangée')).toBeVisible();
  const replaced = b.page.locator('.review-step[data-change="replaced"]');
  await expect(replaced).toHaveCount(1);
  await expect(replaced).toContainText(lessonTwo.title);
  await expect(replaced).toContainText('révision 1 → révision 2');
  await expect(b.page.locator('.review-step[data-change="unchanged"]')).toContainText(lessonOne.title);
  await replaced.getByRole('button', { name: 'Afficher les blocs modifiés' }).click();
  await expect(b.page.getByTestId(`diff-added-${lessonTwo.lessonId}`)).toContainText('TEST Paragraphe ajouté.');
  await b.page.getByRole('button', { name: 'Approuver le cours…' }).click();
  await b.page.getByRole('dialog', { name: /Approuver la version \d+ du cours \?/ }).getByRole('button', { name: 'Approuver', exact: true }).click();
  await expect(b.page.getByText(`Cours approuvé : « ${courseTitle} »`)).toBeVisible();
  await nav(p.page).getByRole('link', { name: 'Publication' }).click();
  const amended = p.page.getByRole('list', { name: 'Cours approuvés à publier' }).getByRole('listitem').filter({ hasText: courseTitle });
  await expect(amended).toContainText('Remplacera la version publiée');
  await amended.getByRole('link', { name: courseTitle }).click();
  await p.page.getByRole('button', { name: 'Publier ce cours…' }).click();
  await p.page.getByRole('dialog', { name: `Publier le cours « ${courseTitle} » ?` }).getByRole('button', { name: 'Publier', exact: true }).click();
  await expect(p.page.getByRole('heading', { name: 'Cours publié' })).toBeVisible();
  await a.page.reload();
  await expect(notice).toHaveCount(0);
  const published = (await au.call('GET', '/api/courses?mine=1')).courses.find((c) => c.id === courseId);
  expect(published.publishedRevisionId).toBe(published.latestRevision.id);

  // 6. The administrator suspends the course, then restores it.
  const ad = await signedIn(browser, baseURL, admin);
  await nav(ad.page).getByRole('link', { name: 'Administration' }).click();
  await ad.page.getByRole('navigation', { name: 'Administration' }).getByRole('link', { name: 'Cours' }).click();
  await ad.page.getByRole('list', { name: 'Tous les cours' }).getByRole('link', { name: courseTitle }).click();
  await expect(ad.page.getByRole('heading', { name: courseTitle, level: 1 })).toBeVisible();
  await ad.page.getByRole('button', { name: 'Suspendre le cours…' }).click();
  await expect(ad.page.getByLabel('Motif')).toBeFocused();
  await ad.page.getByRole('button', { name: 'Continuer…' }).click();
  await expect(ad.page.getByText('Indiquez un motif : il est obligatoire')).toBeVisible();
  await ad.page.getByLabel('Motif').fill('TEST Étape trompeuse.');
  await ad.page.getByRole('button', { name: 'Continuer…' }).click();
  await ad.page.getByRole('dialog', { name: 'Suspendre le cours ?' }).getByRole('button', { name: 'Suspendre le cours' }).click();
  await expect(ad.page.getByRole('status').filter({ hasText: 'Cours suspendu : il n’est plus lisible par les élèves.' })).toBeFocused();
  await expect(ad.page.getByRole('list', { name: 'Historique du cours' })).toContainText('TEST Étape trompeuse.');
  await a.page.reload();
  await expect(a.page.getByRole('heading', { name: 'Cours suspendu' })).toBeVisible();
  const restoreButton = ad.page.getByRole('button', { name: /^Restaurer la version \d+…$/ }).first();
  await restoreButton.click();
  await ad.page.getByRole('button', { name: 'Continuer…' }).click();
  await ad.page.getByRole('dialog', { name: /Restaurer une version \(version \d+\) \?/ }).getByRole('button', { name: 'Restaurer une version' }).click();
  await expect(ad.page.getByText(/Version \d+ restaurée/)).toBeVisible();
  const restored = (await au.call('GET', '/api/courses?mine=1')).courses.find((c) => c.id === courseId);
  expect(restored.suspended).toBe(false);
  expect(restored.publishedRevisionId).toBe(restored.latestRevision.id);
  await a.page.reload();
  await expect(a.page.getByRole('heading', { name: 'Cours suspendu' })).toHaveCount(0);

  for (const s of [a, r, b, p, ad]) {
    expect(s.problems).toEqual([]);
    await s.context.close();
  }
  for (const s of [au, ra, pu]) await s.ctx.close();
});

test('a course author cannot review their own course, in the Studio or through the API', async ({ browser, baseURL }) => {
  const f = await setup(browser, baseURL);
  const au = await apiSession(browser, baseURL, author);
  const ra = await apiSession(browser, baseURL, reviewerA);
  const pu = await apiSession(browser, baseURL, publisher);
  const ar = await apiSession(browser, baseURL, authorReviewer);
  const lesson = await publishedLesson(au, ra, pu, f.subject.id, `TEST Leçon pour autrice relectrice ${run}`);
  const title = `TEST Cours de l’autrice relectrice ${run}`;
  const submitted = await submittedCourse(ar, f.subject.id, title, [lesson], [(await f.current()).assignmentId]);

  const s = await signedIn(browser, baseURL, authorReviewer);
  await nav(s.page).getByRole('link', { name: 'Relecture' }).click();
  const card = s.page.getByRole('listitem').filter({ hasText: title });
  await expect(card).toContainText('Un autre relecteur doit décider');
  await s.page.getByLabel('Décision').selectOption({ label: 'Je peux décider' });
  await expect(s.page.getByRole('listitem').filter({ hasText: title })).toHaveCount(0);
  await s.page.goto(`/studio/review/courses/${submitted.id}`);
  await expect(s.page.getByRole('heading', { name: 'Vous ne pouvez pas décider de cette version' })).toBeVisible();
  await expect(s.page.getByText('Vous êtes l’auteur de cette version du cours')).toBeVisible();
  await expect(s.page.getByRole('button', { name: /Approuver|Demander des modifications/ })).toHaveCount(0);
  const refused = await ar.raw('POST', `/api/review/course-revisions/${submitted.id}/approve`, {});
  expect(refused.status()).toBe(403);
  const body = await refused.json();
  expect(body.error.code).toBe('not_eligible_reviewer');
  expect(body.error.reasons.map((x) => x.reason)).toContain('course_author');
  expect(s.problems).toEqual([]);
  await s.context.close();
  for (const x of [au, ra, pu, ar]) await x.ctx.close();
});

/** A current edition of a new TEST section assigning test-svt (no chapters), for the blocked course's placement. */
async function svtAssignment(f, svtId) {
  const base = '/api/admin/curriculum';
  const level = (await f.ad.call('GET', '/api/catalogue/levels')).levels.find((l) => l.code === 'test-bac');
  const section = await f.ad.call('POST', `${base}/levels/${level.id}/sections`, { code: `test-section-svt-${run}`, nameFr: 'TEST Section SVT', nameAr: 'TEST شعبة' });
  let e = await f.ad.call('POST', `${base}/sections/${section.id}/editions`, { code: `test-svt-${run}`, labelFr: 'TEST édition SVT', labelAr: 'TEST ع' });
  e = await f.ad.call('POST', `${base}/editions/${e.id}/assignments`, { expectedVersion: e.version, subjectId: svtId });
  const assignment = e.assignments.find((a) => a.subjectId === svtId);
  await f.ad.call('POST', `${base}/editions/${e.id}/make-current`, { expectedVersion: e.version, expectedCurrentEditionId: null });
  return assignment.id;
}

test('a course with no eligible reviewer is shown as blocked, with the gap named, to its author and administrators', async ({ browser, baseURL }) => {
  const f = await setup(browser, baseURL);
  const a = await apiSession(browser, baseURL, svtA);
  const b = await apiSession(browser, baseURL, svtB);
  const pu = await apiSession(browser, baseURL, publisher);
  const svt = (await a.call('GET', '/api/catalogue/subjects')).subjects.find((x) => x.code === 'test-svt');
  // Bochra's lesson, reviewed by Anis under the lesson rules; Anis's course includes it.
  const lesson = await publishedLesson(b, a, pu, svt.id, `TEST Leçon de SVT ${run}`);
  const title = `TEST Cours de SVT bloqué ${run}`;
  const submitted = await submittedCourse(a, svt.id, title, [lesson], [await svtAssignment(f, svt.id)]);
  expect(submitted.blocked.reason).toBe('no_eligible_reviewer');

  const owner = await signedIn(browser, baseURL, svtA, `/studio/courses/${submitted.courseId}`);
  const blocked = owner.page.getByRole('note').filter({ hasText: 'Relecture bloquée : aucun relecteur éligible' });
  await expect(blocked).toContainText('TEST Auteur SVT Anis est l’auteur de cette version du cours.');
  await expect(blocked).toContainText(`TEST Autrice SVT Bochra est l’auteur de la révision n° ${lesson.revisionId} de la leçon n° ${lesson.lessonId}`);

  const ad = await signedIn(browser, baseURL, admin, '/studio/admin/courses');
  const item = ad.page.getByRole('list', { name: 'Versions de cours sans relecteur éligible' }).getByRole('listitem').filter({ hasText: title });
  await expect(item).toContainText('Bloqué : aucun relecteur éligible');
  await expect(item).toContainText('TEST Autrice SVT Bochra est l’auteur');

  // Neither reviewer may approve, and nothing can be published.
  for (const who of [a, b]) {
    const r = await who.raw('POST', `/api/review/course-revisions/${submitted.id}/approve`, {});
    expect(r.status()).toBe(403);
  }
  const queue = await pu.call('GET', '/api/publish/queue?type=course');
  expect(queue.revisions.some((x) => x.revisionId === submitted.id)).toBe(false);
  for (const s of [owner, ad]) {
    expect(s.problems).toEqual([]);
    await s.context.close();
  }
  for (const x of [a, b, pu]) await x.ctx.close();
});
