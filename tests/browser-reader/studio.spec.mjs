import { test, expect } from '@playwright/test';

// The built Studio served by the Go application (/studio) under its real CSP,
// signed in as the simulated TEST author the fixture server creates
// (internal/server newReaderConfig; test password from server_test.go).
const author = { email: 'test-author@example.test', password: 'simulated-password-1' };

test('Studio loads from the application under its CSP without violations', async ({ page, baseURL }) => {
  const violations = [];
  const external = [];
  page.on('console', m => { if (/Content Security Policy|Refused to/i.test(m.text())) violations.push(m.text()); });
  page.on('request', r => { if (new URL(r.url()).origin !== new URL(baseURL).origin) external.push(r.url()); });
  await page.addInitScript(() => {
    window.cspViolations = [];
    document.addEventListener('securitypolicyviolation', e => window.cspViolations.push(`${e.violatedDirective} ${e.blockedURI}`));
  });

  // Anonymous: sent to sign-in, then back to the Studio.
  await page.goto('/studio');
  await expect(page).toHaveURL(/\/login\?next=%2Fstudio$/);
  await page.getByLabel('Adresse e-mail').fill(author.email);
  await page.getByLabel('Mot de passe').fill(author.password);
  await page.getByRole('button', { name: 'Se connecter' }).click();
  await expect(page).toHaveURL(/\/studio$/);
  const csp = (await page.request.get('/studio')).headers()['content-security-policy'];
  expect(csp).toContain("style-src 'self';");

  const editor = page.getByRole('textbox', { name: 'Contenu de la leçon' });
  await expect(editor).toBeVisible();
  await editor.click();
  await page.keyboard.type('Texte   avec espaces');
  // ProseMirror's base rules come from the Studio stylesheet, not an injected <style>.
  const ws = await page.evaluate(() => getComputedStyle(document.querySelector('.ProseMirror')).whiteSpace);
  expect(['pre-wrap', 'break-spaces']).toContain(ws);
  expect(await page.evaluate(() => document.querySelectorAll('style[data-tiptap-style]').length)).toBe(0);

  expect(await page.evaluate(() => window.cspViolations)).toEqual([]);
  expect(violations).toEqual([]);
  expect(external).toEqual([]);
});
