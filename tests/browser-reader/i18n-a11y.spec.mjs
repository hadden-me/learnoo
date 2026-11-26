import { test, expect } from '@playwright/test';
import { admin, apiCourse, apiSession, author, closeFixture, doc, setup, watch } from './course-fixture.mjs';

// LMS-B09: the critical journeys in the French and Arabic interfaces, with
// targeted accessibility assertions rather than an automated rule engine
// (docs/DECISIONS.md "Interface localization"): <html lang dir>, one main
// landmark and one h1, a non-empty name for each visible control and link
// (an approximation of the accessible name: aria-labelledby, aria-label,
// labels, title, then textContent or alt), aria-describedby targets
// that exist, a visible keyboard focus, no horizontal overflow at 360 px
// and at 640 px (a 1280 px window at 200% zoom), and no animation under
// prefers-reduced-motion. TEST accounts and content only (internal/server
// TestBrowserReaderServer and course-fixture.mjs). Desktop Chromium.

const student = { email: 'test-course-student@example.test', password: 'simulated-password-1' };
const reviewer = { email: 'test-reviewer@example.test', password: 'simulated-password-1' };
const LOCALES = [
  { lang: 'fr', dir: 'ltr', switchTo: 'العربية' },
  { lang: 'ar', dir: 'rtl', switchTo: 'Français' },
];
const T = {
  fr: { catalogue: 'Catalogue', signIn: 'Se connecter', email: 'Adresse e-mail', password: 'Mot de passe', outline: 'Plan du cours', stepNav: 'Leçons précédente et suivante',
    next: /Suivante/, start: 'Commencer le cours', signInCourse: 'Se connecter pour lire le cours', moduleUp: (i, n) => `Monter le module ${i} : ${n}`, moduleDown: (i, n) => `Descendre le module ${i} : ${n}`,
    moduleTitle: (i) => `Titre du module ${i}`, disable: 'Désactiver le compte', reviewHeading: 'Relecture', studioNav: 'Studio', myCourses: 'Mes cours' },
  ar: { catalogue: 'الفهرس', signIn: 'تسجيل الدخول', email: 'البريد الإلكتروني', password: 'كلمة المرور', outline: 'مخطّط الدورة', stepNav: 'الدرس السابق والدرس التالي',
    next: /التالي/, start: 'بدء الدورة', signInCourse: 'تسجيل الدخول لقراءة الدورة', moduleUp: (i, n) => `نقل الوحدة ${i} إلى الأعلى: ${iso(n)}`, moduleDown: (i, n) => `نقل الوحدة ${i} إلى الأسفل: ${iso(n)}`,
    moduleTitle: (i) => `عنوان الوحدة ${i}`, disable: 'تعطيل الحساب', reviewHeading: 'المراجعة', studioNav: 'الاستوديو', myCourses: 'دوراتي' },
};

// Text interpolated into an Arabic sentence is isolated between U+2068 and U+2069 (web/studio/src/i18n/core.ts).
const iso = (s) => `\u2068${s}\u2069`;

test.use({ reducedMotion: 'reduce' });
test.afterAll(closeFixture);

/** Structural and accessibility checks of the page as it is now. */
async function audit(page, { lang, dir }, what) {
  await expect(page.locator('html'), what).toHaveAttribute('lang', lang);
  await expect(page.locator('html'), what).toHaveAttribute('dir', dir);
  const report = await page.evaluate(() => {
    const visible = (el) => el.getClientRects().length > 0 && getComputedStyle(el).visibility !== 'hidden';
    const name = (el) => {
      const labelledby = el.getAttribute('aria-labelledby');
      if (labelledby) return labelledby.split(/\s+/).map((id) => document.getElementById(id)?.textContent ?? '').join(' ').trim();
      if (el.getAttribute('aria-label')) return el.getAttribute('aria-label').trim();
      if (el.labels && el.labels.length) return [...el.labels].map((l) => l.textContent).join(' ').trim();
      if (el.getAttribute('title')) return el.getAttribute('title').trim();
      return (el.textContent || el.getAttribute('alt') || '').trim();
    };
    const problems = [];
    if (document.querySelectorAll('main').length !== 1) problems.push(`main landmarks: ${document.querySelectorAll('main').length}`);
    // The canonical lesson preview in the Studio carries the lesson's own h1 (the reader's markup, byte for byte).
    const h1 = [...document.querySelectorAll('h1')].filter((h) => !h.closest('.lesson-preview')).length;
    if (h1 !== 1) problems.push(`h1: ${h1}`);
    for (const el of document.querySelectorAll('input:not([type=hidden]), select, textarea, button, a[href], summary')) {
      if (!visible(el)) continue;
      if (!name(el)) problems.push(`unnamed ${el.tagName.toLowerCase()} ${el.outerHTML.slice(0, 80)}`);
    }
    for (const el of document.querySelectorAll('[aria-describedby]')) {
      for (const id of el.getAttribute('aria-describedby').split(/\s+/)) if (!document.getElementById(id)) problems.push(`aria-describedby to missing #${id}`);
    }
    for (const el of document.querySelectorAll('[lang]')) {
      if (!['fr', 'ar'].includes(el.getAttribute('lang'))) problems.push(`lang="${el.getAttribute('lang')}"`);
    }
    const running = document.getAnimations().filter((a) => a.playState === 'running').length;
    if (running) problems.push(`${running} running animations under reduced motion`);
    return problems;
  });
  expect(report, what).toEqual([]);
  // No horizontal scrolling at 360 px and at 200% zoom of a 1280 px window.
  const size = page.viewportSize();
  for (const width of [360, 640]) {
    await page.setViewportSize({ width, height: 800 });
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), `${what} at ${width} px`).toBe(true);
  }
  await page.setViewportSize(size);
}

/** Tab to the first focusable element after the skip link and check its focus ring is visible. */
async function visibleFocus(page, what) {
  await page.locator('body').focus();
  await page.keyboard.press('Tab');
  const skip = page.locator('a.skip');
  if (await skip.count()) await expect(skip, what).toBeFocused();
  await page.keyboard.press('Tab');
  const ring = await page.evaluate(() => {
    const el = document.activeElement;
    const s = getComputedStyle(el);
    return { tag: el.tagName, outline: s.outlineStyle !== 'none' && parseFloat(s.outlineWidth) >= 2, shadow: s.boxShadow !== 'none' };
  });
  expect(ring.outline || ring.shadow, `${what}: focus ring on ${ring.tag}`).toBe(true);
}

async function signInAt(page, user, next, lang) {
  await page.goto(`/login?next=${encodeURIComponent(next)}&lang=${lang}`);
  await expect(page).toHaveURL(/\/login\?next=/); // the server stored the choice and dropped ?lang
  await page.getByLabel(T[lang].email).fill(user.email);
  await page.getByLabel(T[lang].password).fill(user.password);
  await page.getByRole('button', { name: T[lang].signIn }).click();
}

for (const loc of LOCALES) {
  const t = T[loc.lang];
  const q = loc.lang === 'ar' ? '?lang=ar' : '';

  test(`public home, catalogue and course overview (${loc.lang})`, async ({ page, baseURL }) => {
    const problems = watch(page, baseURL);
    await page.goto(`/${q}`);
    await audit(page, loc, 'home');
    await visibleFocus(page, 'home');
    await page.getByRole('navigation').first().getByRole('link', { name: t.catalogue, exact: true }).click();
    await expect(page).toHaveURL(loc.lang === 'ar' ? /\/catalogue\?lang=ar$/ : /\/catalogue$/);
    await audit(page, loc, 'catalogue');
    await page.locator('main .cards a[href^="/catalogue/test-bac/test-maths"]').first().click();
    await audit(page, loc, 'section');
    await page.locator('main .cards a[href*="/test-math"]').first().click();
    await audit(page, loc, 'subject');
    await page.locator('main a[href^="/courses/"]').first().click();
    await expect(page).toHaveURL(loc.lang === 'ar' ? /\/courses\/[0-9a-f-]+\?lang=ar$/ : /\/courses\/[0-9a-f-]+$/);
    await audit(page, loc, 'course overview');
    // Course text keeps its language inside either interface.
    await expect(page.locator('#course-title')).toHaveAttribute('lang', 'fr');
    await expect(page.locator('#course-title')).toHaveAttribute('dir', 'ltr');
    // The switch keeps the route.
    const here = new URL(page.url()).pathname;
    await page.getByRole('link', { name: loc.switchTo }).click();
    await expect(page).toHaveURL(new RegExp(`${here}${loc.lang === 'ar' ? '$' : '\\?lang=ar$'}`));
    await expect(page.locator('html')).toHaveAttribute('lang', loc.lang === 'ar' ? 'fr' : 'ar');
    expect(problems).toEqual([]);
  });

  test(`student course reading with mixed-direction math (${loc.lang})`, async ({ page, baseURL }) => {
    const problems = watch(page, baseURL);
    // From the public overview, whose sign-in link carries the course and the locale.
    await page.goto(`/${q}`);
    await page.locator('main a[href^="/catalogue/test-bac/test-maths"]').first().click();
    await page.locator('main .cards a[href*="/test-math"]').first().click();
    await page.locator('main a[href^="/courses/"]').first().click();
    const signIn = page.getByRole('link', { name: t.signInCourse });
    await expect(signIn).toHaveAttribute('href', loc.lang === 'ar' ? /lang=ar/ : /^\/login\?next=/);
    await signIn.click();
    await page.getByLabel(t.email).fill(student.email);
    await page.getByLabel(t.password).fill(student.password);
    await page.getByRole('button', { name: t.signIn }).click();
    await expect(page).toHaveURL(/\/learn\/courses\/[0-9a-f-]+$/);
    await audit(page, loc, 'course');
    await page.getByRole('link', { name: t.start }).click();
    await audit(page, loc, 'step');
    await visibleFocus(page, 'step');
    // The French lesson stays left to right; formulas are typeset left to right inside the interface's direction.
    await expect(page.locator('article.lesson-body')).toHaveAttribute('dir', 'ltr');
    await expect(page.locator('.math[data-rendered="true"]').first()).toBeAttached();
    expect(await page.locator('.math .katex').first().evaluate((el) => getComputedStyle(el).direction)).toBe('ltr');
    // An Arabic explanation paragraph keeps its formula left to right, isolated from the paragraph.
    const rtlMath = page.locator('article.lesson-body p[lang="ar"][dir="rtl"] .math[dir="ltr"] .katex');
    await expect(rtlMath).toHaveCount(1);
    expect(await rtlMath.evaluate((el) => getComputedStyle(el).direction)).toBe('ltr');
    // The outline is reachable and named; the step navigation's forward link sits at the reading direction's end.
    await expect(page.getByRole('navigation', { name: t.outline })).toBeVisible();
    const nav = page.getByRole('navigation', { name: t.stepNav });
    const forward = nav.locator('a.next');
    await expect(forward).toHaveCount(1);
    const box = await forward.boundingBox();
    const navBox = await nav.boundingBox();
    if (loc.dir === 'rtl') expect(box.x).toBeLessThan(navBox.x + navBox.width / 2);
    else expect(box.x + box.width).toBeGreaterThan(navBox.x + navBox.width / 2);
    // The breadcrumb separators are mirrored glyphs (U+203A), so the trail reads in the interface's direction.
    expect(await page.locator('nav.breadcrumb').evaluate((el) => getComputedStyle(el).direction)).toBe(loc.dir);
    expect(problems).toEqual([]);
  });

  test(`teacher course editor: keyboard-only outline editing (${loc.lang})`, async ({ page, browser, baseURL }) => {
    const problems = watch(page, baseURL);
    const f = await setup(browser, baseURL);
    const au = await apiSession(browser, baseURL, author);
    const { course } = await apiCourse(au, f.subject.id, {
      title: `TEST Cours clavier ${loc.lang}`,
      modules: [
        { key: null, title: 'TEST Module A', chapterId: null, steps: [] },
        { key: null, title: 'TEST Module B', chapterId: null, steps: [] },
      ],
    });
    await au.ctx.close();
    await signInAt(page, author, `/studio/courses/${course.id}`, loc.lang);
    await expect(page.getByLabel(t.moduleTitle(1))).toHaveValue('TEST Module A');
    await audit(page, loc, 'course editor');
    await visibleFocus(page, 'course editor');
    // Course text inputs keep the course's language whatever the interface.
    await expect(page.getByLabel(t.moduleTitle(1))).toHaveAttribute('dir', /ltr|auto/);
    // Keyboard only: move module 2 up; focus follows the moved module.
    await page.getByRole('button', { name: t.moduleUp(2, 'TEST Module B') }).focus();
    await page.keyboard.press('Enter');
    await expect(page.getByLabel(t.moduleTitle(1))).toHaveValue('TEST Module B');
    await expect(page.getByRole('button', { name: t.moduleDown(1, 'TEST Module B') })).toBeFocused();
    // The Studio's switch keeps the route.
    await page.getByRole('link', { name: loc.switchTo }).click();
    await expect(page).toHaveURL(new RegExp(`/studio/courses/${course.id}$`));
    await expect(page.locator('html')).toHaveAttribute('lang', loc.lang === 'ar' ? 'fr' : 'ar');
    expect(problems).toEqual([]);
  });

  test(`reviewer review page (${loc.lang})`, async ({ page, browser, baseURL }) => {
    const problems = watch(page, baseURL);
    const f = await setup(browser, baseURL);
    // A TEST lesson submitted by test-author, for test-reviewer to open.
    const au = await apiSession(browser, baseURL, author);
    const title = `TEST Leçon à relire ${loc.lang}`;
    const created = await au.call('POST', '/api/lessons', { subjectId: f.subject.id, language: 'fr', content: doc(title) });
    await au.call('POST', `/api/revisions/${created.id}/submit`, { expectedVersion: created.version });
    await au.ctx.close();
    await signInAt(page, reviewer, '/studio/review', loc.lang);
    await expect(page.getByRole('navigation', { name: t.studioNav }).getByRole('link', { name: t.reviewHeading })).toHaveAttribute('aria-current', 'page');
    const item = page.locator('main a[href^="/studio/review/"]').filter({ hasText: title });
    await expect(item).toHaveCount(1);
    await audit(page, loc, 'review queue');
    await visibleFocus(page, 'review queue');
    await item.click();
    await expect(page.locator('main h1').first()).toContainText(title);
    await page.locator('.lesson-preview .katex, .lesson-preview p').first().waitFor();
    await audit(page, loc, 'review page');
    expect(problems).toEqual([]);
  });

  test(`administration: confirmation dialog traps and returns focus (${loc.lang})`, async ({ page, browser, baseURL }) => {
    const problems = watch(page, baseURL);
    const ad = await apiSession(browser, baseURL, admin);
    const { users } = await ad.call('GET', '/api/admin/users?q=onboard');
    await ad.ctx.close();
    const target = users.find((u) => u.email.startsWith('test-onboard'));
    await signInAt(page, admin, `/studio/admin/users/${target.id}`, loc.lang);
    await page.locator('main h1').filter({ hasText: 'TEST Enseignant à intégrer' }).waitFor();
    await audit(page, loc, 'user page');
    const opener = page.getByRole('button', { name: t.disable });
    await opener.focus();
    await page.keyboard.press('Enter');
    const dialog = page.getByRole('dialog');
    await expect(dialog).toBeVisible();
    // Focus starts inside, on the least destructive action.
    expect(await dialog.evaluate((d) => d.contains(document.activeElement))).toBe(true);
    // Shift+Tab from the cancel button reaches the dialog's other action, not the page behind; Tab
    // past the last control may go to the browser's own interface (document.body), never to the page.
    await page.keyboard.press('Shift+Tab');
    expect(await page.evaluate(() => !!document.activeElement?.closest('dialog'))).toBe(true);
    for (let i = 0; i < 3; i++) {
      await page.keyboard.press('Tab');
      expect(await page.evaluate(() => !!document.activeElement?.closest('dialog') || document.activeElement === document.body)).toBe(true);
    }
    await page.keyboard.press('Escape');
    await expect(dialog).toHaveCount(0);
    await expect(opener).toBeFocused();
    expect(problems).toEqual([]);
  });
}
