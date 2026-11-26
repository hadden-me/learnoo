import { test, expect } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { tokenFile } from '../../playwright.reader.config.mjs';

// One flow at phone width: accept the TEST invitation, browse, read the mixed
// French/Arabic lesson and the Arabic lesson with KaTeX, report an error,
// bookmark, search.
// Desktop Chromium with a mobile viewport, not a physical phone.
const password = 'browser-test-password-1';
const noOverflow = page => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth);

test('student reader at mobile width with RTL and math', async ({ page, baseURL, browser }) => {
  await page.setViewportSize({ width: 360, height: 780 });
  const external = [];
  page.on('request', r => { if (new URL(r.url()).origin !== new URL(baseURL).origin) external.push(r.url()); });

  // Invitation acceptance signs the student in.
  await page.goto(`/invite/${readFileSync(tokenFile, 'utf8').trim()}`);
  await expect(page.getByText('browser-student@example.test')).toBeVisible();
  expect(await noOverflow(page)).toBe(true);
  await page.getByLabel('Prénom ou pseudonyme').fill('TEST Élève navigateur');
  await page.getByLabel('Mot de passe', { exact: true }).fill(password);
  await page.getByLabel('Confirmer le mot de passe').fill(password);
  await page.getByRole('button', { name: 'Activer mon compte' }).click();
  await expect(page).toHaveURL(/\/learn$/);
  await expect(page.getByRole('heading', { level: 1 })).toContainText('TEST Élève navigateur');
  expect(await noOverflow(page)).toBe(true);

  // Subject → chapter → mixed French lesson, named from the catalogue
  // (TEST names; the subject's code stays in the URL).
  await page.getByRole('link', { name: 'TEST Mathématiques' }).click();
  await expect(page).toHaveURL(/\/learn\/subjects\/test-math$/);
  await expect(page.getByRole('heading', { level: 1, name: 'TEST Mathématiques' })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'TEST Nombres complexes' })).toBeVisible();
  await page.getByRole('link', { name: 'TEST Équations et nombres complexes' }).click();
  await expect(page.locator('.katex').first()).toBeVisible();
  expect(await page.evaluate(() => window.learnooMetrics.errors)).toBe(0);
  expect(await page.evaluate(() => getComputedStyle(document.querySelector('p[lang="ar"]')).direction)).toBe('rtl');
  expect(await page.evaluate(() => getComputedStyle(document.querySelector('p[lang="ar"] .math')).direction)).toBe('ltr');
  await expect(page.locator('.credit')).toContainText('TEST Autrice Amel');
  await expect(page.locator('.credit')).toContainText('TEST Relecteur Karim');
  await page.getByText('Voir la solution expliquée').click();
  await expect(page.getByText('Solution de test.')).toBeVisible();
  expect(await noOverflow(page)).toBe(true);
  await page.screenshot({ path: 'test-results/reader-lesson-mobile.png', fullPage: true });

  // Nothing external loads until the student asks for the video; then the
  // player replaces both the button and its notice (issue #54). YouTube is
  // stubbed so the test stays offline.
  expect(external).toEqual([]);
  await page.route('https://www.youtube-nocookie.com/**', route => route.fulfill({ contentType: 'text/html', body: '<!doctype html><title>stub</title>' }));
  const video = page.locator('section.video');
  await video.getByRole('button', { name: 'Charger la vidéo YouTube' }).click();
  await expect(video.locator('iframe')).toHaveAttribute('src', 'https://www.youtube-nocookie.com/embed/TESTvideo01');
  await expect(video.locator('iframe')).toBeFocused();
  await expect(video.getByText('La vidéo ne se charge qu’après votre choix.')).toHaveCount(0);
  await expect(video.locator('.video-control, .video-load')).toHaveCount(0);
  expect(external.length).toBeGreaterThan(0);
  expect(external.every(u => u.startsWith('https://www.youtube-nocookie.com/embed/'))).toBe(true);
  external.length = 0;

  // Report an error (LRN-015): the form stays closed until asked for; an
  // empty report is refused with the form reopened, then a report is sent.
  await expect(page.getByLabel('Décrivez l’erreur')).toBeHidden();
  await page.getByText('Signaler une erreur').click();
  await page.getByRole('button', { name: 'Envoyer le signalement' }).click();
  await expect(page.getByRole('alert')).toContainText('Décrivez l’erreur avant');
  await expect(page.getByLabel('Décrivez l’erreur')).toBeVisible();
  expect(await noOverflow(page)).toBe(true);
  await page.locator('details.report').screenshot({ path: 'test-results/reader-report-mobile.png' });
  await page.getByLabel('Décrivez l’erreur').fill('TEST La solution de l’exercice semble inverser un signe.\nخطأ تجريبي');
  await page.getByRole('button', { name: 'Envoyer le signalement' }).click();
  await expect(page.getByRole('heading', { name: 'Signalement envoyé' })).toBeVisible();
  expect(await noOverflow(page)).toBe(true);
  await page.getByRole('link', { name: 'Revenir à la leçon' }).click();
  await expect(page.locator('.katex').first()).toBeVisible();

  // Bookmark, then find it on the dashboard.
  await page.getByRole('button', { name: 'Enregistrer' }).click();
  await expect(page.getByRole('button', { name: 'Retirer des signets' })).toBeVisible();
  await page.getByRole('navigation', { name: 'Navigation principale' }).getByRole('link', { name: 'Mes leçons' }).click();
  await expect(page.getByRole('heading', { name: 'Mes signets (1)' })).toBeVisible();
  await expect(page.locator('.cards').first()).toContainText('TEST Équations et nombres complexes');

  // Accent-insensitive title search, then the Arabic lesson.
  await page.getByRole('navigation', { name: 'Navigation principale' }).getByRole('link', { name: 'Rechercher' }).click();
  await page.getByLabel('Titre de la leçon').fill('equations');
  await page.getByRole('button', { name: 'Rechercher' }).click();
  await expect(page.getByRole('link', { name: 'TEST Équations et nombres complexes' })).toBeVisible();
  await page.getByLabel('Titre de la leçon').fill('درس');
  await page.getByRole('button', { name: 'Rechercher' }).click();
  await page.getByRole('link', { name: 'TEST درس تجريبي' }).click();
  await expect(page.locator('.katex').first()).toBeVisible();
  expect(await page.evaluate(() => window.learnooMetrics.errors)).toBe(0);
  expect(await page.evaluate(() => getComputedStyle(document.querySelector('h1')).direction)).toBe('rtl');
  expect(await page.evaluate(() => getComputedStyle(document.querySelector('article')).direction)).toBe('rtl');
  // The breadcrumb names the subject and chapter in the lesson's language,
  // right to left inside the French interface.
  const crumbs = page.locator('nav.breadcrumb bdi[lang="ar"]');
  await expect(crumbs).toHaveText(['TEST رياضيات', 'TEST الأعداد العقدية']);
  expect(await crumbs.evaluateAll((els) => els.map((e) => getComputedStyle(e).direction))).toEqual(['rtl', 'rtl']);
  expect(await page.evaluate(() => getComputedStyle(document.querySelector('.math')).direction)).toBe('ltr');
  // The unlabelled Arabic paragraph inherits lang="ar" from the article, so
  // p:lang(ar) applies (Arabic line height from site.css).
  const para = await page.evaluate(() => {
    const p = document.querySelector('article p');
    const cs = getComputedStyle(p);
    return { hasLang: p.hasAttribute('lang'), isArabic: p.matches(':lang(ar)'), lineHeight: parseFloat(cs.lineHeight), fontSize: parseFloat(cs.fontSize) };
  });
  expect(para.hasLang).toBe(false);
  expect(para.isArabic).toBe(true);
  expect(para.lineHeight / para.fontSize).toBeCloseTo(1.9, 1);
  expect(await noOverflow(page)).toBe(true);
  await page.screenshot({ path: 'test-results/reader-arabic-mobile.png', fullPage: true });
  expect(external).toEqual([]);

  // The lesson URL is useless without a session.
  const lessonURL = page.url();
  const anonymous = await browser.newContext();
  const other = await anonymous.newPage();
  await other.goto(lessonURL);
  await expect(other).toHaveURL(/\/login\?next=/);
  await expect(other.locator('body')).not.toContainText('TEST درس');
  await anonymous.close();

  // Sign out ends the session.
  await page.getByRole('button', { name: 'Se déconnecter' }).click();
  await expect(page).toHaveURL(/\/login$/);
  await page.goto(lessonURL);
  await expect(page).toHaveURL(/\/login\?next=/);
});
