// Catalogue display names (LRN-014, migrations/0012). Subjects and chapters
// are identified by immutable codes; their French and Arabic display names
// are optional metadata an operator sets. The Studio shows the name in the
// language of the lesson concerned, else the other language's name, else
// the code. A name in another language than the interface (an Arabic name
// in the French interface, a French one in the Arabic interface) is marked
// with its language and direction; a code is isolated left to right.
import type { CatalogueEntry } from './api/client';
import { locale } from './i18n';

export type Lang = 'fr' | 'ar';

export interface CatalogueLabel {
  text: string;
  /** The name's language; null when the code is shown. */
  lang: Lang | null;
}

export function catalogueLabel(code: string, nameFr: string | null | undefined, nameAr: string | null | undefined, lang: Lang): CatalogueLabel {
  const [first, second, firstLang, secondLang]: [string | null | undefined, string | null | undefined, Lang, Lang] =
    lang === 'ar' ? [nameAr, nameFr, 'ar', 'fr'] : [nameFr, nameAr, 'fr', 'ar'];
  if (first) return { text: first, lang: firstLang };
  if (second) return { text: second, lang: secondLang };
  return { text: code, lang: null };
}

export const entryLabel = (entry: CatalogueEntry, lang: Lang) => catalogueLabel(entry.code, entry.nameFr, entry.nameAr, lang);

/** lang and dir attributes for an element holding only this label: set when the name is not in the interface language. */
export function labelAttrs(label: CatalogueLabel): { lang?: Lang; dir?: 'rtl' | 'ltr' } {
  if (label.lang === null) return locale() === 'ar' ? { dir: 'ltr' } : {};
  return label.lang === locale() ? {} : { lang: label.lang, dir: label.lang === 'ar' ? 'rtl' : 'ltr' };
}

/** A label inside running interface text: a name in the other language (or a code in Arabic) is isolated (bdi). */
export function CatalogueName({ label }: { label: CatalogueLabel }) {
  const attrs = labelAttrs(label);
  return attrs.dir ? <bdi {...attrs}>{label.text}</bdi> : <>{label.text}</>;
}
