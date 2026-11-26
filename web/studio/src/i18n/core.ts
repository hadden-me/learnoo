// Interface locale of the Studio (LMS-B09): French (default) or Arabic.
//
// The choice is the `__Host-learnoo_lang` cookie shared with the reader
// (internal/i18n). It is HttpOnly: the server writes the shell's
// <html lang dir> from it, and the Studio reads that. The locale is fixed for a page load: the language switch is a link to
// the current route with ?lang=, which the server turns into the cookie and
// a redirect back (a full reload, guarded by the usual "leave without
// saving?" prompt and the local recovery copy). Message tables built at
// module load are therefore always in the right language.
//
// Content language stays independent: lesson and course text keeps its own
// lang/dir whatever the interface (see dirOf and catalogue.tsx).

export type Locale = 'fr' | 'ar';
export const LOCALES: readonly Locale[] = ['fr', 'ar'];
export const PARAM = 'lang';

/** A French plural message: CLDR categories used by French. */
export interface FrPlural {
  one: string;
  other: string;
}

/** An Arabic plural message: every CLDR category of Arabic. */
export interface ArPlural {
  zero: string;
  one: string;
  two: string;
  few: string;
  many: string;
  other: string;
}

export type FrTable = Record<string, string | FrPlural>;

/** The Arabic table matching a French one: same keys, Arabic plural forms. TypeScript checks parity. */
export type ArTable<T extends FrTable> = { [K in keyof T]: T[K] extends string ? string : ArPlural };

export const parseLocale = (s: string | null | undefined): Locale | null => (s === 'fr' || s === 'ar' ? s : null);

/**
 * The interface locale: the document's lang, which the server sets from the
 * locale cookie (an HttpOnly cookie, never read here), else French.
 */
export function detectLocale(doc: Pick<Document, 'documentElement'> | undefined = typeof document === 'undefined' ? undefined : document): Locale {
  return parseLocale(doc?.documentElement.lang) ?? 'fr';
}

let current: Locale | null = null;

/** The interface locale of this page load. */
export function locale(): Locale {
  if (current === null) current = detectLocale();
  return current;
}

/** Tests and the boot code only: fixes the locale (messages built at module load are not rebuilt). */
export function setLocale(l: Locale) {
  current = l;
}

export const dirOfLocale = (l: Locale): 'ltr' | 'rtl' => (l === 'ar' ? 'rtl' : 'ltr');
export const uiDir = () => dirOfLocale(locale());

/** BCP 47 tag for Intl formatting (Western digits in Arabic, as in Tunisian schools). */
export const intlLocale = (l: Locale = locale()) => (l === 'ar' ? 'ar-TN-u-nu-latn' : 'fr-FR');

/** Applies the locale's language and direction to the document (the server already does; Vite's dev server does not). */
export function applyToDocument(doc: Document = document, l: Locale = locale()) {
  doc.documentElement.lang = l;
  doc.documentElement.dir = dirOfLocale(l);
}

/** The current URL with ?lang= set to `target`: the server stores the choice and comes back to the route. */
export function switchHref(target: Locale, loc: Pick<Location, 'pathname' | 'search' | 'hash'> = location): string {
  const params = new URLSearchParams(loc.search);
  params.set(PARAM, target);
  return `${loc.pathname}?${params.toString()}${loc.hash}`;
}

const FSI = '\u2068';
const PDI = '\u2069';
const RTL_STRONG = /[\u0590-\u08FF\uFB1D-\uFDFF\uFE70-\uFEFF]/;

/**
 * A text value placed inside a sentence of locale l, isolated between FSI
 * (U+2068) and PDI (U+2069) when its direction could disturb the sentence:
 * always in the Arabic interface (a French title or a Latin name inside an
 * Arabic sentence), and in the French interface when it holds right-to-left
 * letters. Numbers are left as they are.
 */
export function isolate(value: string | number, l: Locale = locale()): string {
  if (typeof value === 'number') return String(value);
  if (value === '' || (l === 'fr' && !RTL_STRONG.test(value))) return value;
  return FSI + value + PDI;
}

/** Replaces {name} placeholders with isolated values (isolate); unknown ones are left as they are. */
export function format(text: string, params?: Record<string, string | number>, l: Locale = locale()): string {
  if (!params) return text;
  return text.replace(/\{(\w+)\}/g, (m, name: string) => (name in params ? isolate(params[name], l) : m));
}

const pluralRules = new Map<Locale, Intl.PluralRules>();

/** The CLDR plural category of n in the locale. */
export function pluralCategory(n: number, l: Locale = locale()): keyof ArPlural {
  let rules = pluralRules.get(l);
  if (!rules) {
    rules = new Intl.PluralRules(l);
    pluralRules.set(l, rules);
  }
  return rules.select(n) as keyof ArPlural;
}
