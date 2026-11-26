// The Studio's message catalog (LMS-B09). Messages live in one module per
// area under messages/, each exporting `fr` (the French table, `satisfies
// FrTable`) and `ar` (typed `ArTable<typeof fr>`, so TypeScript refuses a
// missing or extra Arabic key). Keys are prefixed by area so tables never
// collide (test/i18n.test.ts checks it, and that placeholders match).
//
// Use t('key', { name }) for text and tn('key', count, { name }) for plural
// messages; {n} is the count. Unknown keys do not compile.
import { format, locale, pluralCategory, type ArPlural, type FrPlural, type Locale } from './core';
import * as shell from './messages/shell';
import * as courses from './messages/courses';
import * as review from './messages/review';
import * as admin from './messages/admin';
import * as lessons from './messages/lessons';

export * from './core';

/** Each area's tables, for the parity tests. */
export const MODULES = { shell, courses, review, admin, lessons };

const FR = { ...shell.fr, ...courses.fr, ...review.fr, ...admin.fr, ...lessons.fr };
const AR = { ...shell.ar, ...courses.ar, ...review.ar, ...admin.ar, ...lessons.ar };

type Fr = typeof FR;
export type MessageKey = { [K in keyof Fr]: Fr[K] extends string ? K : never }[keyof Fr];
export type PluralKey = { [K in keyof Fr]: Fr[K] extends string ? never : K }[keyof Fr];

export const TABLES: Record<Locale, Record<string, string | FrPlural | ArPlural>> = { fr: FR, ar: AR };

/** The interface text of key in the current locale, with {name} placeholders filled. */
export function t(key: MessageKey, params?: Record<string, string | number>, l: Locale = locale()): string {
  const text = TABLES[l][key] ?? TABLES.fr[key];
  return format(typeof text === 'string' ? text : key, params);
}

/** The plural form of key for n ({n} is replaced by n). */
export function tn(key: PluralKey, n: number, params?: Record<string, string | number>, l: Locale = locale()): string {
  const forms = (TABLES[l][key] ?? TABLES.fr[key]) as Partial<ArPlural> | undefined;
  const text = forms?.[pluralCategory(n, l)] ?? forms?.other ?? key;
  return format(text, { n, ...params });
}
