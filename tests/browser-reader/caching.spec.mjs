import { test, expect } from '@playwright/test';

// Issue #88: fingerprinted assets are reused from the browser cache on the
// next page, while pages themselves (private, no-store) always come from the
// server. Chromium's DevTools protocol says where each response came from.
// TEST accounts from the fixture server (internal/server TestBrowserReaderServer).
const password = 'simulated-password-1';

/** Records, per URL path, whether the last load came from the network or a cache. */
async function recordLoads(page) {
  const cdp = await page.context().newCDPSession(page);
  await cdp.send('Network.enable');
  const byId = new Map();
  const loads = [];
  cdp.on('Network.requestWillBeSent', (e) => byId.set(e.requestId, { path: new URL(e.request.url).pathname, type: e.type, cached: false, status: 0, cacheControl: '' }));
  cdp.on('Network.requestServedFromCache', (e) => { const l = byId.get(e.requestId); if (l) l.cached = true; });
  cdp.on('Network.responseReceived', (e) => {
    const l = byId.get(e.requestId);
    if (!l) return;
    l.status = e.response.status;
    l.cacheControl = e.response.headers['Cache-Control'] ?? e.response.headers['cache-control'] ?? '';
    if (e.response.fromDiskCache || e.response.fromPrefetchCache) l.cached = true;
  });
  cdp.on('Network.loadingFinished', (e) => { const l = byId.get(e.requestId); if (l) loads.push(l); });
  return loads;
}

function watchCSP(page) {
  const problems = [];
  page.on('console', (m) => { if (/Content Security Policy|Refused to/i.test(m.text())) problems.push(m.text()); });
  return page.addInitScript(() => {
    window.cspViolations = [];
    document.addEventListener('securitypolicyviolation', (e) => window.cspViolations.push(`${e.violatedDirective} ${e.blockedURI}`));
  }).then(() => problems);
}

test('a second lesson reuses the cached fingerprinted assets; pages are fetched again', async ({ browser, baseURL }) => {
  const context = await browser.newContext({ baseURL });
  const page = await context.newPage();
  const problems = await watchCSP(page);
  await page.goto('/login');
  await page.getByLabel('Adresse e-mail').fill('test-student@example.test');
  await page.getByLabel('Mot de passe').fill(password);
  await page.getByRole('button', { name: 'Se connecter' }).click();
  await expect(page).toHaveURL(/\/learn$/);
  const loads = await recordLoads(page);

  // First lesson: assets from the network, publicly cacheable.
  await page.goto('/learn/subjects/test-math');
  await page.getByRole('link', { name: 'TEST Équations et nombres complexes' }).click();
  await expect(page.locator('.katex').first()).toBeVisible();
  expect(await page.evaluate(() => window.learnooMetrics.errors)).toBe(0);
  await page.evaluate(() => document.fonts.ready);
  expect(await page.evaluate(() => document.fonts.check('16px KaTeX_Main'))).toBe(true);
  const first = loads.filter((l) => l.path.startsWith('/assets/'));
  expect(first.map((l) => l.path).some((p) => p.endsWith('/vendor/katex/katex.min.js'))).toBe(true);
  expect(first.some((l) => l.path.includes('/vendor/katex/fonts/'))).toBe(true);
  // Only lesson media (the fixture image) stays under /static/, private.
  for (const l of loads.filter((l) => l.path.startsWith('/static/'))) {
    expect(l.path).toBe('/static/complex-plane.svg');
    expect(l.cacheControl).toBe('private, no-store');
  }
  for (const l of first.filter((l) => !l.cached)) {
    expect(l.status, l.path).toBe(200);
    expect(l.cacheControl, l.path).toBe('public, max-age=31536000, immutable');
  }

  // Second lesson: every fingerprinted asset comes from the cache; the page
  // itself (private, no-store) from the server.
  loads.length = 0;
  await page.goto('/learn/subjects/test-math');
  await page.getByRole('link', { name: 'TEST درس تجريبي' }).click();
  await expect(page.locator('.katex').first()).toBeVisible();
  expect(await page.evaluate(() => window.learnooMetrics.errors)).toBe(0);
  const second = loads.filter((l) => l.path.startsWith('/assets/'));
  expect(second.length).toBeGreaterThan(0);
  // The Arabic lesson may need a KaTeX font the first did not; anything
  // already loaded is not fetched again.
  const seen = new Set(first.map((l) => l.path));
  expect(second.filter((l) => seen.has(l.path) && !l.cached).map((l) => l.path)).toEqual([]);
  expect(second.filter((l) => seen.has(l.path)).length).toBeGreaterThanOrEqual(6);
  const documents = loads.filter((l) => l.type === 'Document');
  expect(documents.length).toBe(2);
  for (const d of documents) {
    expect(d.cached, d.path).toBe(false);
    expect(d.cacheControl, d.path).toBe('private, no-store');
  }

  expect(await page.evaluate(() => window.cspViolations)).toEqual([]);
  expect(problems).toEqual([]);
  await context.close();
});

test('a second Studio load reuses the cached bundle; the shell is fetched again', async ({ browser, baseURL }) => {
  const context = await browser.newContext({ baseURL });
  const page = await context.newPage();
  const problems = await watchCSP(page);
  await page.goto('/login?next=/studio');
  await page.getByLabel('Adresse e-mail').fill('test-author@example.test');
  await page.getByLabel('Mot de passe').fill(password);
  await page.getByRole('button', { name: 'Se connecter' }).click();
  await expect(page).toHaveURL(/\/studio$/);
  await expect(page.getByRole('heading', { name: 'Mes leçons' })).toBeVisible();
  const loads = await recordLoads(page);

  await page.goto('/studio/');
  await expect(page.getByRole('heading', { name: 'Mes leçons' })).toBeVisible();
  const bundle = loads.filter((l) => l.path.startsWith('/studio/assets/'));
  expect(bundle.length).toBeGreaterThan(0);
  expect(bundle.filter((l) => !l.cached).map((l) => l.path)).toEqual([]);
  const shell = loads.filter((l) => l.type === 'Document');
  expect(shell.length).toBe(1);
  expect(shell[0].cached).toBe(false);
  expect(shell[0].cacheControl).toBe('private, no-store');
  // The shell names the preview's KaTeX by its fingerprinted URL.
  expect(await page.locator('meta[name="learnoo-katex-js"]').getAttribute('content')).toMatch(/^\/assets\/[0-9a-f]{16}\/vendor\/katex\/katex\.min\.js$/);

  expect(await page.evaluate(() => window.cspViolations)).toEqual([]);
  expect(problems).toEqual([]);
  await context.close();
});
