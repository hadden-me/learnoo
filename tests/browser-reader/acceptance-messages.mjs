// Interface wording for the LMS acceptance journeys (tests/browser-reader/
// acceptance-lms.spec.mjs), read from the application's own catalogs so the
// same journey runs in French and in Arabic: the Studio's message tables
// (web/studio/src/i18n/messages/*.ts, imported as TypeScript by Playwright)
// and the reader's Go catalogs (internal/i18n/messages_{fr,ar}.go, parsed
// here: one "key": "value" entry per line).
import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import * as admin from '../../web/studio/src/i18n/messages/admin.ts';
import * as courses from '../../web/studio/src/i18n/messages/courses.ts';
import * as lessons from '../../web/studio/src/i18n/messages/lessons.ts';
import * as review from '../../web/studio/src/i18n/messages/review.ts';
import * as shell from '../../web/studio/src/i18n/messages/shell.ts';
import { format } from '../../web/studio/src/i18n/core.ts';

const root = join(dirname(fileURLToPath(import.meta.url)), '..', '..');

function goCatalog(lang) {
  const out = {};
  const source = readFileSync(join(root, 'internal', 'i18n', `messages_${lang}.go`), 'utf8');
  for (const line of source.split('\n')) {
    const m = /^\s*"([^"]+)":\s*("(?:[^"\\]|\\.)*"),\s*(?:\/\/.*)?$/.exec(line);
    if (m) out[m[1]] = JSON.parse(m[2]);
  }
  if (Object.keys(out).length < 100) throw new Error(`messages_${lang}.go: parsed only ${Object.keys(out).length} entries`);
  return out;
}

const studio = (lang) => Object.assign({}, ...[admin, courses, lessons, review, shell].map((m) => m[lang]));
const tables = { fr: { studio: studio('fr'), reader: goCatalog('fr') }, ar: { studio: studio('ar'), reader: goCatalog('ar') } };

/**
 * Message functions for one interface language: s(key, vars) for the Studio
 * ({name} placeholders, CLDR plural objects chosen by vars.n) and r(key,
 * ...args) for the reader ({0}, {1}… placeholders; plural keys "key|one").
 */
export function messages(lang) {
  const { studio: st, reader: rd } = tables[lang];
  const plural = new Intl.PluralRules(lang);
  const s = (key, vars = {}) => {
    let m = st[key];
    if (m === undefined) throw new Error(`Studio message ${key} missing (${lang})`);
    if (typeof m === 'object') m = m[plural.select(vars.n)] ?? m.other;
    // The Studio's own format(), so values are isolated (FSI…PDI) exactly as rendered.
    return format(m, vars, lang);
  };
  const r = (key, ...args) => {
    let m = rd[key];
    if (m === undefined && typeof args[0] === 'number') m = rd[`${key}|${plural.select(args[0])}`] ?? rd[`${key}|other`];
    if (m === undefined) throw new Error(`reader message ${key} missing (${lang})`);
    return m.replace(/\{(\d+|n)\}/g, (all, i) => (i === 'n' ? String(args[0]) : i < args.length ? String(args[i]) : all));
  };
  return { s, r };
}

/** A RegExp matching a message whose {placeholders} may be anything. */
export const pattern = (text) => new RegExp(text.split(/\{\w+\}/).map((p) => p.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('.*'));
