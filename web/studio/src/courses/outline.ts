// The course draft as the editor holds it, and the pure operations on it:
// conversion from and to the API, module and step moves (button and keyboard
// alternatives to dragging; there is no drag), additions, removals and the
// client-side checks that mirror the server's limits (docs/API.md "Course
// drafts"). The server checks everything again; these only explain problems
// before a save is sent.
import type { CourseRevision, CourseSaveBody } from '../api/courses';
import { intlLocale, t } from '../i18n';

export const LIMITS = { title: 200, summary: 1000, objectives: 2000, prerequisites: 2000, coverAlt: 300, moduleTitle: 200, stepTitle: 200, modules: 50, steps: 200 } as const;

export interface DraftStep {
  /** Local identity for React keys and focus; never sent. */
  localId: string;
  lessonId: number;
  revisionId: number;
  title: string;
  /** Display facts from the server or the lesson picker; not sent. */
  revisionNumber: number | null;
  ownLesson: boolean | null;
  eligible: boolean | null;
  lessonTitle: string | null;
}

export interface DraftModule {
  localId: string;
  /** Server key (stable across revisions), null until the first save of a new module. */
  key: string | null;
  title: string;
  chapterId: number | null;
  steps: DraftStep[];
}

export interface CourseDraft {
  title: string;
  summary: string;
  objectives: string;
  prerequisites: string;
  coverMediaId: number | null;
  coverAlt: string;
  assignmentIds: number[];
  modules: DraftModule[];
}

let counter = 0;
export const localId = (prefix: string) => `${prefix}-${++counter}-${Math.random().toString(36).slice(2, 8)}`;

export function fromRevision(revision: CourseRevision): CourseDraft {
  return {
    title: revision.title,
    summary: revision.summary,
    objectives: revision.objectives,
    prerequisites: revision.prerequisites,
    coverMediaId: revision.coverMediaId,
    coverAlt: revision.coverAlt ?? '',
    assignmentIds: revision.assignments.map((a) => a.assignmentId),
    modules: revision.modules.map((m) => ({
      localId: localId('m'),
      key: m.key,
      title: m.title,
      chapterId: m.chapterId,
      steps: m.steps.map((s) => ({
        localId: localId('s'),
        lessonId: s.lessonId,
        revisionId: s.revisionId,
        title: s.title,
        revisionNumber: s.revisionNumber,
        ownLesson: s.ownLesson,
        eligible: s.eligible,
        lessonTitle: null,
      })),
    })),
  };
}

/** The save body: trimmed text, cover alt only with a cover, order of the arrays is the order of the outline. */
export function toSaveBody(draft: CourseDraft): CourseSaveBody {
  const cover = draft.coverMediaId;
  return {
    title: draft.title.trim(),
    summary: draft.summary.trim(),
    objectives: draft.objectives.trim(),
    prerequisites: draft.prerequisites.trim(),
    coverMediaId: cover,
    coverAlt: cover === null ? null : draft.coverAlt.trim(),
    assignmentIds: [...draft.assignmentIds],
    modules: draft.modules.map((m) => ({
      key: m.key,
      title: m.title.trim(),
      chapterId: m.chapterId,
      steps: m.steps.map((s) => ({ lessonId: s.lessonId, revisionId: s.revisionId, title: s.title.trim() })),
    })),
  };
}

/** Comparison string for unsaved-change tracking: what a save would send, without module keys (assigned by the server). */
export function snapshot(draft: CourseDraft): string {
  const body = toSaveBody(draft);
  return JSON.stringify({ ...body, modules: body.modules.map((m) => ({ ...m, key: undefined })) });
}

/* ---------- moves ---------- */

function move<T>(items: T[], from: number, to: number): T[] {
  if (from === to || from < 0 || to < 0 || from >= items.length || to >= items.length) return items;
  const next = [...items];
  const [item] = next.splice(from, 1);
  next.splice(to, 0, item);
  return next;
}

export function moveModule(draft: CourseDraft, index: number, delta: -1 | 1): CourseDraft {
  return { ...draft, modules: move(draft.modules, index, index + delta) };
}

export interface StepPlace {
  module: number;
  step: number;
}

export function findStep(draft: CourseDraft, stepLocalId: string): StepPlace | null {
  for (let m = 0; m < draft.modules.length; m++) {
    const s = draft.modules[m].steps.findIndex((x) => x.localId === stepLocalId);
    if (s >= 0) return { module: m, step: s };
  }
  return null;
}

/**
 * Moves a step one place up or down. At the top (bottom) of its module it
 * goes to the end (start) of the previous (next) module, so every position
 * of the outline is reachable with these two actions alone.
 */
export function moveStep(draft: CourseDraft, stepLocalId: string, delta: -1 | 1): CourseDraft {
  const at = findStep(draft, stepLocalId);
  if (!at) return draft;
  const modules = draft.modules.map((m) => ({ ...m, steps: [...m.steps] }));
  const source = modules[at.module];
  const target = at.step + delta;
  if (target >= 0 && target < source.steps.length) {
    source.steps = move(source.steps, at.step, target);
    return { ...draft, modules };
  }
  const neighbour = modules[at.module + delta];
  if (!neighbour) return draft;
  const [step] = source.steps.splice(at.step, 1);
  if (delta < 0) neighbour.steps.push(step);
  else neighbour.steps.unshift(step);
  return { ...draft, modules };
}

/** Moves a step to the end of another module. */
export function moveStepToModule(draft: CourseDraft, stepLocalId: string, moduleLocalId: string): CourseDraft {
  const at = findStep(draft, stepLocalId);
  const target = draft.modules.findIndex((m) => m.localId === moduleLocalId);
  if (!at || target < 0 || target === at.module) return draft;
  const modules = draft.modules.map((m) => ({ ...m, steps: [...m.steps] }));
  const [step] = modules[at.module].steps.splice(at.step, 1);
  modules[target].steps.push(step);
  return { ...draft, modules };
}

/** Whether a step can move up or down (across modules included). */
export function stepMoves(draft: CourseDraft, stepLocalId: string): { up: boolean; down: boolean } {
  const at = findStep(draft, stepLocalId);
  if (!at) return { up: false, down: false };
  const last = draft.modules.length - 1;
  return {
    up: at.step > 0 || at.module > 0,
    down: at.step < draft.modules[at.module].steps.length - 1 || at.module < last,
  };
}

/* ---------- edits ---------- */

export const addModule = (draft: CourseDraft, title: string): CourseDraft => ({
  ...draft,
  modules: [...draft.modules, { localId: localId('m'), key: null, title, chapterId: null, steps: [] }],
});

export const updateModule = (draft: CourseDraft, moduleLocalId: string, change: Partial<Pick<DraftModule, 'title' | 'chapterId'>>): CourseDraft => ({
  ...draft,
  modules: draft.modules.map((m) => (m.localId === moduleLocalId ? { ...m, ...change } : m)),
});

export const removeModule = (draft: CourseDraft, moduleLocalId: string): CourseDraft => ({ ...draft, modules: draft.modules.filter((m) => m.localId !== moduleLocalId) });

export function addStep(draft: CourseDraft, moduleLocalId: string, step: Omit<DraftStep, 'localId'>): CourseDraft {
  return { ...draft, modules: draft.modules.map((m) => (m.localId === moduleLocalId ? { ...m, steps: [...m.steps, { ...step, localId: localId('s') }] } : m)) };
}

export function updateStep(draft: CourseDraft, stepLocalId: string, change: Partial<Omit<DraftStep, 'localId'>>): CourseDraft {
  return { ...draft, modules: draft.modules.map((m) => ({ ...m, steps: m.steps.map((s) => (s.localId === stepLocalId ? { ...s, ...change } : s)) })) };
}

export const removeStep = (draft: CourseDraft, stepLocalId: string): CourseDraft => ({
  ...draft,
  modules: draft.modules.map((m) => ({ ...m, steps: m.steps.filter((s) => s.localId !== stepLocalId) })),
});

/** Lessons already in the outline (a lesson may appear once per course). */
export const lessonsInOutline = (draft: CourseDraft) => new Set(draft.modules.flatMap((m) => m.steps.map((s) => s.lessonId)));

/**
 * Removes a placement and clears module chapters that no remaining placement
 * provides. `chaptersOf` returns null when a placement's chapters are not
 * known (its edition could not be read): then no chapter is cleared, since
 * nothing proves it is no longer provided.
 */
export function removeAssignment(draft: CourseDraft, assignmentId: number, chaptersOf: (assignmentId: number) => number[] | null): CourseDraft {
  const assignmentIds = draft.assignmentIds.filter((id) => id !== assignmentId);
  const lists = assignmentIds.map(chaptersOf);
  if (lists.some((l) => l === null)) return { ...draft, assignmentIds };
  const allowed = new Set((lists as number[][]).flat());
  return { ...draft, assignmentIds, modules: draft.modules.map((m) => (m.chapterId !== null && !allowed.has(m.chapterId) ? { ...m, chapterId: null } : m)) };
}

/**
 * Editions whose details are still to be read: shown by a placement, not
 * known yet and not already failed (a failure is retried only on request,
 * never in a loop).
 */
export function editionsToFetch(editionIds: number[], known: Record<number, unknown>, failed: number[]): number[] {
  return [...new Set(editionIds)].filter((id) => !(id in known) && !failed.includes(id));
}

/* ---------- what the next save will refuse ---------- */

/**
 * The draft's comparison revision (compareRevisionId: the published revision
 * the draft started from, none for a course never published). A placement or
 * a module chapter is "carried" when it is unchanged from it; the server's save
 * check accepts carried entries that can no longer be newly selected.
 */
export interface Comparison {
  assignmentIds: Set<number>;
  /** Module key → chapter on the comparison revision. */
  moduleChapters: Map<string, number | null>;
}

export const NO_COMPARISON: Comparison = { assignmentIds: new Set(), moduleChapters: new Map() };

export function comparisonOf(revision: CourseRevision | null): Comparison {
  if (!revision) return NO_COMPARISON;
  return { assignmentIds: new Set(revision.assignments.map((a) => a.assignmentId)), moduleChapters: new Map(revision.modules.map((m) => [m.key, m.chapterId])) };
}

export const placementCarried = (assignmentId: number, cmp: Comparison) => cmp.assignmentIds.has(assignmentId);

/** A placement the next save refuses: not carried, and no longer selectable (edition not current, or something retired). */
export const placementRefused = (assignmentId: number, info: { selectable: boolean } | undefined, cmp: Comparison) => !!info && !info.selectable && !placementCarried(assignmentId, cmp);

/** A module chapter the next save refuses: retired, and not inherited unchanged (same module key and chapter on the comparison). */
export function chapterRefused(module: Pick<DraftModule, 'key' | 'chapterId'>, retired: Set<number>, cmp: Comparison): boolean {
  if (module.chapterId === null || !retired.has(module.chapterId)) return false;
  return !(module.key !== null && cmp.moduleChapters.get(module.key) === module.chapterId);
}

/**
 * After a save the server returns keys for new modules. The modules that
 * were sent (by local ID, in order) receive the keys at the same positions;
 * modules added while the save was in flight stay new.
 */
export function assignKeys(draft: CourseDraft, sentModuleIds: string[], savedKeys: string[]): CourseDraft {
  const keys = new Map(sentModuleIds.map((id, i) => [id, savedKeys[i]]));
  return { ...draft, modules: draft.modules.map((m) => (m.key === null && keys.get(m.localId) ? { ...m, key: keys.get(m.localId)! } : m)) };
}

/* ---------- checks ---------- */

export interface Problem {
  /** The field or element concerned, for aria-invalid and focus: `title`, `module:<localId>`, `step:<localId>`, `coverAlt`… */
  target: string;
  message: string;
}

const length = (s: string) => [...s.trim()].length;
// The server refuses control characters (line feeds allowed in the long fields), bidirectional formatting and line/paragraph separators.
const BIDI_AND_SEPARATORS = '\\u200e\\u200f\\u202a-\\u202e\\u2066-\\u2069\\u2028\\u2029';
const FORBIDDEN_LINE = new RegExp(`[\\u0000-\\u001f\\u007f-\\u009f${BIDI_AND_SEPARATORS}]`, 'u');
const FORBIDDEN_TEXT = new RegExp(`[\\u0000-\\u0009\\u000b-\\u001f\\u007f-\\u009f${BIDI_AND_SEPARATORS}]`, 'u');

/** Problems the server would refuse, in French, in outline order. An incomplete draft (empty fields, no modules) is allowed. */
export function checkDraft(draft: CourseDraft): Problem[] {
  const problems: Problem[] = [];
  const text = (target: string, label: string, value: string, max: number, multiline: boolean) => {
    if (length(value) > max) problems.push({ target, message: t('courses.check.tooLong', { label, max: max.toLocaleString(intlLocale()), count: length(value).toLocaleString(intlLocale()) }) });
    else if ((multiline ? FORBIDDEN_TEXT : FORBIDDEN_LINE).test(value.trim())) problems.push({ target, message: t('courses.check.forbidden', { label }) });
  };
  text('title', t('courses.field.title'), draft.title, LIMITS.title, false);
  text('summary', t('courses.field.summary'), draft.summary, LIMITS.summary, true);
  text('objectives', t('courses.field.objectives'), draft.objectives, LIMITS.objectives, true);
  text('prerequisites', t('courses.field.prerequisites'), draft.prerequisites, LIMITS.prerequisites, true);
  if (draft.coverMediaId !== null) {
    if (length(draft.coverAlt) === 0) problems.push({ target: 'coverAlt', message: t('courses.check.coverAltRequired') });
    else text('coverAlt', t('courses.field.coverAlt'), draft.coverAlt, LIMITS.coverAlt, false);
  }
  if (draft.modules.length > LIMITS.modules) problems.push({ target: 'modules', message: t('courses.check.modulesMax', { max: LIMITS.modules }) });
  const steps = draft.modules.reduce((n, m) => n + m.steps.length, 0);
  if (steps > LIMITS.steps) problems.push({ target: 'modules', message: t('courses.check.stepsMax', { max: LIMITS.steps }) });
  draft.modules.forEach((m, i) => {
    const label = t('courses.check.module', { index: i + 1 });
    if (length(m.title) === 0) problems.push({ target: `module:${m.localId}`, message: t('courses.check.titleRequired', { label }) });
    else text(`module:${m.localId}`, t('courses.check.titleOf', { label }), m.title, LIMITS.moduleTitle, false);
    m.steps.forEach((s, j) => {
      const stepLabel = t('courses.check.step', { module: label, index: j + 1 });
      if (length(s.title) === 0) problems.push({ target: `step:${s.localId}`, message: t('courses.check.titleRequired', { label: stepLabel }) });
      else text(`step:${s.localId}`, t('courses.check.titleOf', { label: stepLabel }), s.title, LIMITS.stepTitle, false);
    });
  });
  return problems;
}

/** Plain-text accent- and case-insensitive form, for searching titles. */
export const fold = (s: string) => s.normalize('NFD').replace(/\p{M}/gu, '').toLocaleLowerCase('fr');
