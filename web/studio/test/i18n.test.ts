// The Studio's message catalog and interface direction (LMS-B09). TypeScript
// already refuses a missing or extra Arabic key and an unknown key in t();
// these tests check what types cannot: no key is shared by two areas, no
// message is empty, placeholders match, plural forms are complete, and the
// locale drives the document's direction and catalogue name isolation.
import { afterEach, describe, expect, it } from 'vitest';
import { labelAttrs } from '../src/catalogue';
import { MODULES, TABLES, applyToDocument, detectLocale, dirOfLocale, format, pluralCategory, setLocale, switchHref, t, tn } from '../src/i18n';

const placeholders = (s: string) => [...new Set(s.match(/\{\w+\}/g) ?? [])].sort();
const FR_PLURAL = ['one', 'other'];
const AR_PLURAL = ['zero', 'one', 'two', 'few', 'many', 'other'];

afterEach(() => setLocale('fr'));

describe('catalog', () => {
  it('keeps each area under its own prefix, with the same keys in both languages', () => {
    const seen = new Map<string, string>();
    for (const [area, mod] of Object.entries(MODULES)) {
      const fr = Object.keys(mod.fr).sort();
      const ar = Object.keys(mod.ar).sort();
      expect(ar, area).toEqual(fr);
      for (const key of fr) {
        expect(key.startsWith(`${area}.`), `${key} in ${area}`).toBe(true);
        expect(seen.has(key), `${key} also in ${seen.get(key)}`).toBe(false);
        seen.set(key, area);
      }
    }
    expect(Object.keys(TABLES.ar).sort()).toEqual(Object.keys(TABLES.fr).sort());
  });

  it('has no empty message, matching placeholders and complete plural forms', () => {
    for (const [key, fr] of Object.entries(TABLES.fr)) {
      const ar = TABLES.ar[key];
      if (typeof fr === 'string') {
        expect(typeof ar, key).toBe('string');
        expect(fr.trim(), key).not.toBe('');
        expect((ar as string).trim(), key).not.toBe('');
        expect(placeholders(ar as string), key).toEqual(placeholders(fr));
        continue;
      }
      expect(Object.keys(fr).sort(), key).toEqual([...FR_PLURAL].sort());
      expect(Object.keys(ar).sort(), key).toEqual([...AR_PLURAL].sort());
      const frParams = placeholders(fr.other).filter((p) => p !== '{n}');
      for (const form of Object.values(ar as unknown as Record<string, string>)) {
        expect(form.trim(), key).not.toBe('');
        for (const p of placeholders(form)) if (p !== '{n}') expect(frParams, `${key}: ${p}`).toContain(p);
      }
    }
  });

  it('formats and selects plural forms per locale', () => {
    expect(format('{a} et {b} {c}', { a: 1, b: 'x' }, 'fr')).toBe('1 et x {c}');
    expect(pluralCategory(0, 'fr')).toBe('one');
    expect(pluralCategory(2, 'fr')).toBe('other');
    expect([0, 1, 2, 3, 11, 100].map((n) => pluralCategory(n, 'ar'))).toEqual(['zero', 'one', 'two', 'few', 'many', 'other']);
    expect(t('shell.myLessons')).toBe('Mes leçons');
    expect(t('shell.myLessons', undefined, 'ar')).toBe('دروسي');
    expect(t('shell.signOutFailed', { error: 'X' }, 'ar')).toBe('تعذّر تسجيل الخروج: \u2068X\u2069');
    const plural = Object.entries(TABLES.fr).find(([, v]) => typeof v !== 'string');
    if (plural) {
      const key = plural[0] as Parameters<typeof tn>[0];
      expect(tn(key, 2, undefined, 'fr')).not.toContain('{n}');
      expect(tn(key, 11, undefined, 'ar')).not.toContain('{n}');
    }
  });
});

describe('locale', () => {
  it('reads the document language set by the server, else French', () => {
    const doc = (lang: string) => ({ documentElement: { lang } as HTMLElement });
    expect(detectLocale(doc('ar'))).toBe('ar');
    expect(detectLocale(doc('fr'))).toBe('fr');
    expect(detectLocale(doc('en'))).toBe('fr');
    expect(detectLocale(doc(''))).toBe('fr');
    expect(detectLocale(undefined)).toBe('fr');
  });

  it('isolates interpolated text in Arabic sentences, and right-to-left text in French ones', () => {
    const fsi = '\u2068';
    const pdi = '\u2069';
    // A French course title inside an Arabic sentence keeps its own direction.
    const ar = t('review.course.approved', { title: 'TEST Cours des nombres complexes' }, 'ar');
    expect(ar).toContain(`${fsi}TEST Cours des nombres complexes${pdi}`);
    expect(ar).toMatch(/[\u0600-\u06FF]/);
    expect(t('review.course.approved', { title: 'TEST Cours' }, 'fr')).not.toContain(fsi);
    expect(t('review.course.approved', { title: 'TEST دورة' }, 'fr')).toContain(`${fsi}TEST دورة${pdi}`);
    expect(format('{a} {n}', { a: 3 }, 'ar')).toBe('3 {n}');
  });

  it('replaces the plural count before the arguments', () => {
    const plural = Object.entries(TABLES.fr).find(([, v]) => typeof v !== 'string' && /\{(?!n\})\w+\}/.test((v as { other: string }).other));
    if (!plural) return;
    const name = (plural[1] as { other: string }).other.match(/\{(?!n\})(\w+)\}/)![1];
    expect(tn(plural[0] as Parameters<typeof tn>[0], 2, { [name]: 'x {n} y' }, 'fr')).toContain('x {n} y');
  });

  it('applies the interface direction to the document (RTL for Arabic)', () => {
    applyToDocument(document, 'ar');
    expect(document.documentElement.lang).toBe('ar');
    expect(document.documentElement.dir).toBe('rtl');
    applyToDocument(document, 'fr');
    expect(document.documentElement.dir).toBe('ltr');
    expect(dirOfLocale('ar')).toBe('rtl');
  });

  it('switches language on the same route', () => {
    expect(switchHref('ar', { pathname: '/studio/courses/12', search: '?tab=outline', hash: '#m2' })).toBe('/studio/courses/12?tab=outline&lang=ar#m2');
    expect(switchHref('fr', { pathname: '/studio', search: '?lang=ar', hash: '' })).toBe('/studio?lang=fr');
  });

  it('isolates catalogue names that are not in the interface language', () => {
    const fr = { text: 'Mathématiques', lang: 'fr' as const };
    const ar = { text: 'رياضيات', lang: 'ar' as const };
    const code = { text: 'math', lang: null };
    setLocale('fr');
    expect(labelAttrs(fr)).toEqual({});
    expect(labelAttrs(ar)).toEqual({ lang: 'ar', dir: 'rtl' });
    expect(labelAttrs(code)).toEqual({});
    setLocale('ar');
    expect(labelAttrs(ar)).toEqual({});
    expect(labelAttrs(fr)).toEqual({ lang: 'fr', dir: 'ltr' });
    expect(labelAttrs(code)).toEqual({ dir: 'ltr' });
  });
});
