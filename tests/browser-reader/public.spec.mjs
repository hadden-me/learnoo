import { test, expect } from '@playwright/test';

// Public discovery (LMS-B07) at phone width, signed out: homepage → level →
// section → subject → course overview, then the sign-in link keeps the
// course reading route as its return path. TEST curriculum and course from
// the Go fixture (seedBrowserCatalogue). Desktop Chromium with a mobile
// viewport, not a physical phone.
const password = 'simulated-password-1';
const noOverflow = page => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth);

test('signed-out visitor finds a course from the homepage', async ({ page, baseURL }) => {
  await page.setViewportSize({ width: 360, height: 780 });
  const external = [];
  page.on('request', r => { if (new URL(r.url()).origin !== new URL(baseURL).origin) external.push(r.url()); });

  const home = await page.goto('/');
  expect(home.status()).toBe(200);
  expect(home.headers()['cache-control']).toBe('public, no-cache');
  expect(home.headers()['etag']).toBeTruthy();
  await expect(page.getByRole('heading', { level: 1 })).toBeVisible();
  await expect(page.getByRole('banner')).toBeVisible();
  await expect(page.getByRole('main')).toBeVisible();
  await expect(page.getByRole('contentinfo')).toContainText('sur invitation');
  await expect(page.locator('input[name="csrf_token"]')).toHaveCount(0);
  await expect(page.getByRole('link', { name: /inscri/i })).toHaveCount(0);
  await expect(page.getByRole('link', { name: 'Accès enseignants (Studio)' })).toHaveAttribute('href', '/login?next=%2Fstudio');
  expect(await noOverflow(page)).toBe(true);

  // Level → section, from the catalogue (names, never codes).
  await page.getByRole('link', { name: 'Parcourir le catalogue' }).click();
  await expect(page).toHaveURL(/\/catalogue$/);
  await expect(page.getByRole('heading', { level: 2, name: 'TEST Baccalauréat' })).toBeVisible();
  await page.getByRole('link', { name: 'TEST Section mathématiques' }).click();
  await expect(page).toHaveURL(/\/catalogue\/test-bac\/test-maths$/);
  await expect(page.getByText('Édition : TEST Édition 2026 (en vigueur)')).toBeVisible();
  expect(await noOverflow(page)).toBe(true);

  // Subject: the course and the standalone lessons, listed separately.
  await page.getByRole('link', { name: 'TEST Mathématiques' }).click();
  await expect(page).toHaveURL(/\/catalogue\/test-bac\/test-maths\/test-math$/);
  const courses = page.getByRole('region', { name: 'Cours' });
  const lessons = page.getByRole('region', { name: 'Leçons', exact: true });
  await expect(courses.getByRole('link', { name: 'TEST Cours de nombres complexes' })).toBeVisible();
  await expect(lessons.getByRole('link', { name: 'TEST Équations et nombres complexes' })).toBeVisible();
  await expect(lessons.getByRole('link', { name: 'TEST Cours de nombres complexes' })).toHaveCount(0);
  expect(await noOverflow(page)).toBe(true);

  // Course overview: metadata, cover with alt text, outline; no lesson
  // body or solution.
  await courses.getByRole('link', { name: 'TEST Cours de nombres complexes' }).click();
  await expect(page).toHaveURL(/\/courses\/[0-9a-f-]{36}$/);
  const id = page.url().split('/courses/')[1];
  await expect(page.getByRole('heading', { level: 1, name: 'TEST Cours de nombres complexes' })).toBeVisible();
  const cover = page.getByRole('img', { name: 'TEST description de la couverture' });
  await expect(cover).toBeVisible();
  expect(await cover.evaluate(img => img.complete && img.naturalWidth > 0)).toBe(true);
  await expect(page.getByRole('heading', { name: 'Plan du cours' })).toBeVisible();
  await expect(page.getByText('TEST Étape A TEST Cours de nombres complexes')).toBeVisible();
  await expect(page.getByText('Solution de test.')).toHaveCount(0);
  await expect(page.getByText('Texte de test avec')).toHaveCount(0);
  expect(await noOverflow(page)).toBe(true);
  await page.screenshot({ path: 'test-results/public-course-mobile.png', fullPage: true });

  // The sign-in link carries the course reading route; signing in returns
  // there (the route itself is LMS-B08's).
  const signIn = page.getByRole('link', { name: 'Se connecter pour lire le cours' });
  await expect(signIn).toHaveAttribute('href', `/login?next=%2Flearn%2Fcourses%2F${id}`);
  await signIn.click();
  await expect(page.locator('input[name="next"]')).toHaveValue(`/learn/courses/${id}`);
  await page.getByLabel('Adresse e-mail').fill('test-author@example.test');
  await page.getByLabel('Mot de passe').fill(password);
  await Promise.all([
    page.waitForURL(`**/learn/courses/${id}`),
    page.getByRole('button', { name: 'Se connecter' }).click(),
  ]);
  expect(new URL(page.url()).pathname).toBe(`/learn/courses/${id}`);
  expect(external).toEqual([]);
});
