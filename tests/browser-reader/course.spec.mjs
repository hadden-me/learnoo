import { test, expect } from '@playwright/test';

// LMS-B08 course journey at phone width: a signed-in TEST student opens a
// published course, follows next and previous, sees an unavailable step
// after an administrator withdraws its lesson, bookmarks the course and
// reports an error from course context. Accounts and the course are TEST
// fixtures of internal/server TestBrowserReaderServer.
// Desktop Chromium with a mobile viewport, not a physical phone.
const password = 'simulated-password-1';
const student = { email: 'test-course-student@example.test', password };
const admin = { email: 'test-admin@example.test', password };
const noOverflow = page => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth);

test('course reading with an unavailable step, a course bookmark and a course-context report', async ({ page, browser, baseURL }) => {
  await page.setViewportSize({ width: 360, height: 780 });
  const external = [];
  page.on('request', r => { if (new URL(r.url()).origin !== new URL(baseURL).origin) external.push(r.url()); });

  await page.goto('/login?next=/learn');
  await page.getByLabel('Adresse e-mail').fill(student.email);
  await page.getByLabel('Mot de passe').fill(student.password);
  await page.getByRole('button', { name: 'Se connecter' }).click();
  await expect(page).toHaveURL(/\/learn$/);

  // Home: the optional preference, the available course.
  await expect(page.getByText('Vous parcourez tout le programme')).toBeVisible();
  const available = page.locator('section[aria-labelledby="courses-heading"]');
  await available.getByRole('link', { name: 'TEST Cours de nombres complexes' }).click();
  await expect(page.getByRole('heading', { level: 1, name: 'TEST Cours de nombres complexes' })).toBeVisible();
  const outline = page.getByRole('navigation', { name: 'Plan du cours' });
  await expect(outline.getByRole('link')).toHaveText(['TEST Étape un', 'TEST Étape deux', 'TEST Étape trois']);
  expect(await noOverflow(page)).toBe(true);
  const stepTwoURL = await outline.getByRole('link', { name: 'TEST Étape deux' }).getAttribute('href');

  // Start, next, previous, next.
  await page.getByRole('link', { name: 'Commencer le cours' }).click();
  await expect(page.getByText('Corps de la leçon de cours un.')).toBeVisible();
  await expect(page.getByText('Leçon 1 sur 3')).toBeVisible();
  const steps = page.getByRole('navigation', { name: 'Leçons précédente et suivante' });
  await steps.getByRole('link', { name: /Suivante/ }).click();
  await expect(page.getByText('Corps de la leçon de cours deux.')).toBeVisible();
  await expect(page.getByRole('navigation', { name: 'Fil d’Ariane' })).toContainText('TEST Premier module');
  await expect(page.locator('#plan-du-cours [aria-current="page"]')).toHaveText('TEST Étape deux');
  await steps.getByRole('link', { name: /Précédente/ }).click();
  await expect(page.getByText('Corps de la leçon de cours un.')).toBeVisible();
  await steps.getByRole('link', { name: /Suivante/ }).click();
  await steps.getByRole('link', { name: /Suivante/ }).click();
  await expect(page.getByText('Corps de la leçon de cours trois.')).toBeVisible();
  await expect(page.getByRole('navigation', { name: 'Fil d’Ariane' })).toContainText('TEST Second module');
  expect(await noOverflow(page)).toBe(true);
  // The outline is reachable from the top of the page on a phone.
  await page.getByRole('link', { name: 'Plan du cours' }).first().click();
  await expect(page).toHaveURL(/#plan-du-cours$/);

  // An administrator withdraws the lesson of step two (API setup only).
  const lessonPublicID = stepTwoURL.split('/').pop();
  const adm = await browser.newContext({ baseURL });
  const login = await adm.request.post('/api/auth/login', { data: admin });
  expect(login.status()).toBe(200);
  const { csrfToken } = await login.json();
  const pub = await (await adm.request.get(`/api/lessons/${lessonPublicID}/publication`)).json();
  const withdraw = await adm.request.post(`/api/revisions/${pub.publishedRevisionId}/withdraw`, {
    data: { reason: 'TEST retrait pour le parcours de cours', expectedPublishedRevisionId: pub.publishedRevisionId },
    headers: { 'X-CSRF-Token': csrfToken },
  });
  expect(withdraw.status(), await withdraw.text()).toBe(200);
  await adm.close();

  // Navigation stops at the unavailable step with the notice.
  await steps.getByRole('link', { name: /Précédente/ }).click();
  await expect(page).toHaveURL(new RegExp(`${stepTwoURL}$`));
  await expect(page.getByRole('status')).toContainText('Cette leçon est temporairement indisponible');
  await expect(page.getByRole('heading', { level: 1, name: 'TEST Étape deux' })).toBeVisible();
  await expect(page.getByText('Corps de la leçon de cours deux.')).toHaveCount(0);
  await expect(page.getByText('Signaler une erreur')).toHaveCount(0);
  await steps.getByRole('link', { name: /Précédente/ }).click();
  await expect(page.getByText('Corps de la leçon de cours un.')).toBeVisible();
  await expect(steps.getByRole('link', { name: /Suivante/ })).toContainText('(temporairement indisponible)');
  expect(await noOverflow(page)).toBe(true);
  await page.screenshot({ path: 'test-results/course-step-mobile.png', fullPage: true });
  // From 64rem the outline is a sticky side column next to the lesson.
  await page.setViewportSize({ width: 1280, height: 800 });
  const box = await page.locator('#plan-du-cours').boundingBox();
  const main = await page.locator('.course-main').boundingBox();
  expect(box.x).toBeGreaterThan(main.x + main.width - 1);
  await page.screenshot({ path: 'test-results/course-step-desktop.png' });
  await page.setViewportSize({ width: 360, height: 780 });

  // Report an error on the displayed revision, from course context.
  await page.getByText('Signaler une erreur').click();
  await page.getByLabel('Décrivez l’erreur').fill('TEST Une étape du cours semble contenir une coquille.');
  await page.getByRole('button', { name: 'Envoyer le signalement' }).click();
  await expect(page.getByRole('heading', { name: 'Signalement envoyé' })).toBeVisible();
  await page.getByRole('link', { name: 'Revenir à la leçon du cours' }).click();
  await expect(page.getByText('Corps de la leçon de cours un.')).toBeVisible();

  // Bookmark the course; the outline and the home say so.
  await page.getByRole('navigation', { name: 'Fil d’Ariane' }).getByRole('link', { name: 'TEST Cours de nombres complexes' }).click();
  await expect(page.getByRole('status')).toContainText('Une leçon de ce cours est temporairement indisponible');
  await expect(outline.getByRole('link', { name: /TEST Étape deux/ })).toContainText('temporairement indisponible');
  await page.getByRole('button', { name: 'Enregistrer le cours' }).click();
  await expect(page.getByRole('button', { name: 'Retirer des signets' })).toBeVisible();
  await page.getByRole('navigation', { name: 'Navigation principale' }).getByRole('link', { name: 'Mes leçons' }).click();
  await expect(page.getByRole('heading', { name: 'Mes cours enregistrés (1)' })).toBeVisible();
  await expect(page.locator('section[aria-labelledby="course-bookmarks-heading"]')).toContainText('TEST Cours de nombres complexes');
  expect(await noOverflow(page)).toBe(true);
  await page.screenshot({ path: 'test-results/course-home-mobile.png', fullPage: true });
  expect(external).toEqual([]);
});
