// Client routes of the reviewer, publisher, administrator and error-report
// spaces, under /studio/ like the authoring routes (router.ts). No route has
// a dot in its last segment (the server would look it up as a file).
// Not imported from router.ts, which imports this module.
const BASE = '/studio';

export type EditorialRoute =
  | { name: 'reviewQueue' }
  | { name: 'reviewRevision'; revisionId: number }
  | { name: 'publishQueue' }
  | { name: 'publishRevision'; revisionId: number }
  | { name: 'adminLessons' }
  | { name: 'publication'; ref: string }
  | { name: 'reports' }
  | { name: 'report'; reportId: number };

const ID = '([1-9][0-9]{0,17})';
/** A numeric lesson ID or a lesson's public ID (lower-case UUID), as the server accepts. */
const LESSON_REF = '([1-9][0-9]{0,17}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})';

const PATTERNS: Array<[RegExp, (m: RegExpExecArray) => EditorialRoute]> = [
  [/^\/studio\/review$/, () => ({ name: 'reviewQueue' })],
  [new RegExp(`^/studio/review/revisions/${ID}$`), (m) => ({ name: 'reviewRevision', revisionId: Number(m[1]) })],
  [/^\/studio\/publish$/, () => ({ name: 'publishQueue' })],
  [new RegExp(`^/studio/publish/revisions/${ID}$`), (m) => ({ name: 'publishRevision', revisionId: Number(m[1]) })],
  [/^\/studio\/admin$/, () => ({ name: 'adminLessons' })],
  [new RegExp(`^/studio/publication/${LESSON_REF}$`), (m) => ({ name: 'publication', ref: m[1] })],
  [/^\/studio\/reports$/, () => ({ name: 'reports' })],
  [new RegExp(`^/studio/reports/${ID}$`), (m) => ({ name: 'report', reportId: Number(m[1]) })],
];

const NAMES = new Set<string>(['reviewQueue', 'reviewRevision', 'publishQueue', 'publishRevision', 'adminLessons', 'publication', 'reports', 'report']);

export const isEditorialRoute = (route: { name: string }): route is EditorialRoute => NAMES.has(route.name);

/** The editorial route for a path (trailing slashes ignored), or null. */
export function parseEditorialRoute(pathname: string): EditorialRoute | null {
  const path = pathname.replace(/\/+$/, '');
  for (const [pattern, build] of PATTERNS) {
    const m = pattern.exec(path);
    if (m) return build(m);
  }
  return null;
}

export const paths = {
  reviewQueue: `${BASE}/review`,
  reviewRevision: (id: number) => `${BASE}/review/revisions/${id}`,
  publishQueue: `${BASE}/publish`,
  publishRevision: (id: number) => `${BASE}/publish/revisions/${id}`,
  adminLessons: `${BASE}/admin`,
  publication: (ref: string | number) => `${BASE}/publication/${ref}`,
  reports: `${BASE}/reports`,
  report: (id: number) => `${BASE}/reports/${id}`,
  /** The student reader's page of a published lesson. */
  readerLesson: (publicId: string) => `/learn/lessons/${publicId}`,
} as const;

/** Normalizes what an administrator types to find a lesson: a numeric ID, a public ID or a reader URL. */
export function lessonRefFromInput(input: string): string | null {
  const text = input.trim().toLowerCase();
  const uuid = /[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/.exec(text);
  if (uuid) return uuid[0];
  if (/^[1-9][0-9]{0,17}$/.test(text)) return text;
  return null;
}
