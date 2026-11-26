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
    expect(format('{a} et {b} {c}', { a: 1, b: 'x' })).toBe('1 et x {c}');
    expect(pluralCategory(0, 'fr')).toBe('one');
    expect(pluralCategory(2, 'fr')).toBe('other');
    expect([0, 1, 2, 3, 11, 100].map((n) => pluralCategory(n, 'ar'))).toEqual(['zero', 'one', 'two', 'few', 'many', 'other']);
    expect(t('shell.myLessons')).toBe('Mes leçons');
    expect(t('shell.myLessons', undefined, 'ar')).toBe('دروسي');
    expect(t('shell.signOutFailed', { error: 'X' }, 'ar')).toBe('تعذّر تسجيل الخروج: X');
    const plural = Object.entries(TABLES.fr).find(([, v]) => typeof v !== 'string');
    if (plural) {
      const key = plural[0] as Parameters<typeof tn>[0];
      expect(tn(key, 2, undefined, 'fr')).not.toContain('{n}');
      expect(tn(key, 11, undefined, 'ar')).not.toContain('{n}');
    }
  });
});

describe('locale', () => {
  it('reads the cookie, then the document language, else French', () => {
    const doc = (cookie: string, lang: string) => ({ cookie, documentElement: { lang } as HTMLElement });
    expect(detectLocale(doc('a=1; __Host-learnoo_lang=ar', 'fr'))).toBe('ar');
    expect(detectLocale(doc('__Host-learnoo_lang=en', 'ar'))).toBe('ar');
    expect(detectLocale(doc('', ''))).toBe('fr');
    expect(detectLocale(undefined)).toBe('fr');
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
