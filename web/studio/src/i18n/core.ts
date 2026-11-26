// Interface locale of the Studio (LMS-B09): French (default) or Arabic.
//
// The choice is the `__Host-learnoo_lang` cookie shared with the reader
// (internal/i18n). The server also writes the shell's <html lang dir> from
// it. The locale is fixed for a page load: the language switch is a link to
// the current route with ?lang=, which the server turns into the cookie and
// a redirect back (a full reload, guarded by the usual "leave without
// saving?" prompt and the local recovery copy). Message tables built at
// module load are therefore always in the right language.
//
// Content language stays independent: lesson and course text keeps its own
// lang/dir whatever the interface (see dirOf and catalogue.tsx).

export type Locale = 'fr' | 'ar';
export const LOCALES: readonly Locale[] = ['fr', 'ar'];
export const COOKIE_NAME = '__Host-learnoo_lang';
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

/** The cookie's locale, else the document's lang (set by the server), else French. */
export function detectLocale(doc: Pick<Document, 'cookie' | 'documentElement'> | undefined = typeof document === 'undefined' ? undefined : document): Locale {
  if (!doc) return 'fr';
  for (const part of doc.cookie.split(';')) {
    const [name, value] = part.trim().split('=');
    if (name === COOKIE_NAME) {
      const l = parseLocale(value);
      if (l) return l;
    }
  }
  return parseLocale(doc.documentElement.lang) ?? 'fr';
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

/** Replaces {name} placeholders; unknown ones are left as they are. */
export function format(text: string, params?: Record<string, string | number>): string {
  if (!params) return text;
  return text.replace(/\{(\w+)\}/g, (m, name: string) => (name in params ? String(params[name]) : m));
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
