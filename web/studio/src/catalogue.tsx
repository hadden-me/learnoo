// Catalogue display names (LRN-014, migrations/0012). Subjects and chapters
// are identified by immutable codes; their French and Arabic display names
// are optional metadata an operator sets. The Studio shows the name in the
// language of the lesson concerned, else the other language's name, else
// the code. An Arabic name is marked with its language and direction so it
// reads right to left inside the French interface.
import type { CatalogueEntry } from './api/client';

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

/** lang and dir attributes for an element holding only this label: set for Arabic names (the interface is French). */
export function labelAttrs(label: CatalogueLabel): { lang?: Lang; dir?: 'rtl' } {
  return label.lang === 'ar' ? { lang: 'ar', dir: 'rtl' } : {};
}

/** A label inside running French text: an Arabic name is isolated (bdi) with its language and direction. */
export function CatalogueName({ label }: { label: CatalogueLabel }) {
  return label.lang === 'ar' ? (
    <bdi lang="ar" dir="rtl">
      {label.text}
    </bdi>
  ) : (
    <>{label.text}</>
  );
}
