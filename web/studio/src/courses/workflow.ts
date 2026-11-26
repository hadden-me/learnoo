// Pure logic of the course review screens (LMS-B06): the server's refusals
// mapped onto the editor's fields and steps, the "no eligible reviewer" gap
// and the reviewer's own disqualifications in words, the mixed lesson and
// course review queue (filters, pages, submission age), change labels, and
// which administrator recovery actions apply to a course. The server checks
// everything again; these only explain and offer.
import type { AdminCourseDetail, AdminCourseRevision, Blocked, CourseReviewItem, ReviewerProblem, StepChange, StepRef } from '../api/courseReview';
import type { ReviewQueueItem } from '../api/editorial';
import type { CourseDraft, Problem } from './outline';

/* ---------- submission refusals ---------- */

const FIELD_LABELS: Record<string, string> = {
  title: 'Titre du cours : obligatoire pour soumettre.',
  summary: 'Description courte : obligatoire pour soumettre.',
  objectives: 'Objectifs d’apprentissage : obligatoires pour soumettre.',
  modules: 'Plan du cours : ajoutez au moins un module avec une étape.',
};

/**
 * Problems for 422 incomplete (`fields`) and 409 ineligible (`steps`),
 * pointing at the editor's own fields, modules and steps (the targets of
 * outline.checkDraft). Module keys come from the server; steps are matched
 * by lesson, which a revision holds at most once.
 */
export function submissionProblems(draft: CourseDraft, fields: string[], steps: StepRef[]): Problem[] {
  const problems: Problem[] = [];
  for (const field of fields) {
    if (field.startsWith('module:')) {
      const key = field.slice('module:'.length);
      const index = draft.modules.findIndex((m) => m.key === key);
      const m = draft.modules[index];
      problems.push(
        m
          ? { target: `module:${m.localId}`, message: `Module ${index + 1} « ${m.title.trim() || 'sans titre'} » : ajoutez au moins une étape avant de soumettre.` }
          : { target: 'modules', message: 'Un module sans étape empêche la soumission. Enregistrez puis réessayez.' },
      );
    } else problems.push({ target: field, message: FIELD_LABELS[field] ?? `Champ « ${field} » : à compléter avant de soumettre.` });
  }
  for (const s of steps) {
    const index = draft.modules.findIndex((m) => m.steps.some((x) => x.lessonId === s.lessonId));
    const step = index >= 0 ? draft.modules[index].steps.find((x) => x.lessonId === s.lessonId) : undefined;
    const where = index >= 0 ? ` (module ${index + 1})` : '';
    problems.push({
      target: step ? `step:${step.localId}` : 'modules',
      message: `Étape « ${s.title} »${where} : la révision de leçon épinglée ne peut pas être soumise avec le cours. Elle doit être approuvée, publiée au moins une fois et non retirée, et la leçon doit être publiée. Épinglez une révision publiée ou retirez l’étape.`,
    });
  }
  return problems;
}

/* ---------- reviewer eligibility ---------- */

const revisionOf = (p: ReviewerProblem) =>
  p.lessonRevisionId !== null ? `la révision n° ${p.lessonRevisionId} de la leçon n° ${p.lessonId}` : `une révision de la leçon n° ${p.lessonId}`;

/** One reviewer's disqualification, as the queue and the author see it (with names). */
export function describeBlockedDetail(p: ReviewerProblem): string {
  const who = p.reviewerName ?? (p.reviewerId !== null ? `le compte n° ${p.reviewerId}` : 'ce relecteur');
  switch (p.reason) {
    case 'no_subject_reviewer':
      return 'La matière n’a aucun relecteur actif.';
    case 'course_author':
      return `${who} est l’auteur de cette version du cours.`;
    case 'lesson_author':
      return `${who} est l’auteur de ${revisionOf(p)}, ajoutée ou remplacée par cette version.`;
    default:
      return `${who} ne peut pas décider (${p.reason}).`;
  }
}

/** The "no eligible reviewer" gap in one sentence and its details, naming what is missing. */
export function describeBlocked(blocked: Blocked): { summary: string; details: string[] } {
  const none = blocked.details.length === 0 || blocked.details.some((d) => d.reason === 'no_subject_reviewer');
  return {
    summary: none
      ? 'Relecture bloquée : la matière n’a aucun relecteur actif. Un administrateur doit attribuer la permission de relecteur pour cette matière à une personne indépendante.'
      : 'Relecture bloquée : aucun relecteur de la matière n’est indépendant de cette version. Il faut un relecteur de la matière autre que l’auteur du cours et que les auteurs des leçons ajoutées ou remplacées.',
    details: none ? [] : blocked.details.map(describeBlockedDetail),
  };
}

/** Why the signed-in reviewer may not decide (403 not_eligible_reviewer or the review payload's problems). */
export function describeOwnProblem(p: ReviewerProblem): string {
  switch (p.reason) {
    case 'course_author':
      return 'Vous êtes l’auteur de cette version du cours : vous ne pouvez ni l’approuver ni demander des modifications.';
    case 'lesson_author':
      return `Vous êtes l’auteur de ${revisionOf(p)}, que cette version ajoute ou remplace : vous ne pouvez pas approuver un changement de votre propre leçon.`;
    case 'not_subject_reviewer':
      return 'Vous n’êtes pas relecteur de la matière de ce cours : vous pouvez le lire, pas en décider.';
    default:
      return `Vous ne pouvez pas décider de cette version (${p.reason}).`;
  }
}

/* ---------- mixed review queue ---------- */

export type QueueType = 'all' | 'lesson' | 'course';
export type QueueDecision = 'all' | 'mine' | 'blocked' | 'other';

export type QueueEntry =
  | { type: 'lesson'; id: string; subjectId: number; submittedAt: string; item: ReviewQueueItem }
  | { type: 'course'; id: string; subjectId: number; submittedAt: string; item: CourseReviewItem };

/** Lessons and courses in one list, oldest submission first. */
export function mergeQueue(lessons: ReviewQueueItem[], courses: CourseReviewItem[]): QueueEntry[] {
  const entries: QueueEntry[] = [
    ...lessons.map((item) => ({ type: 'lesson' as const, id: `lesson-${item.revisionId}`, subjectId: item.subjectId, submittedAt: item.submittedAt, item })),
    ...courses.map((item) => ({ type: 'course' as const, id: `course-${item.revisionId}`, subjectId: item.subjectId, submittedAt: item.submittedAt, item })),
  ];
  return entries.sort((a, b) => a.submittedAt.localeCompare(b.submittedAt) || a.id.localeCompare(b.id));
}

/**
 * `mine`: what the reviewer may decide (lessons in the queue always: the
 * server leaves the reviewer's own out); `blocked`: courses no one may
 * decide; `other`: courses another reviewer must decide.
 */
export function filterQueue(entries: QueueEntry[], f: { type: QueueType; subjectId: number | null; decision: QueueDecision }): QueueEntry[] {
  return entries.filter((e) => {
    if (f.type !== 'all' && e.type !== f.type) return false;
    if (f.subjectId !== null && e.subjectId !== f.subjectId) return false;
    if (f.decision === 'all') return true;
    if (e.type === 'lesson') return f.decision === 'mine';
    if (f.decision === 'mine') return e.item.eligible;
    if (f.decision === 'blocked') return e.item.blocked !== null;
    return !e.item.eligible && e.item.blocked === null;
  });
}

export const PAGE_SIZE = 20;

/** One page (0-based, clamped) and the page count, at least 1. */
export function paginate<T>(items: T[], page: number, size = PAGE_SIZE): { items: T[]; page: number; pages: number } {
  const pages = Math.max(1, Math.ceil(items.length / size));
  const p = Math.min(Math.max(0, page), pages - 1);
  return { items: items.slice(p * size, (p + 1) * size), page: p, pages };
}

/** How long ago something was submitted, in whole days. */
export function submissionAge(iso: string, now: Date = new Date()): string {
  const days = Math.floor((now.getTime() - new Date(iso).getTime()) / 86_400_000);
  if (days <= 0) return 'soumise aujourd’hui';
  if (days === 1) return 'soumise hier';
  return `soumise il y a ${days} jours`;
}

/* ---------- change list ---------- */

export const CHANGE_LABELS: Record<StepChange['change'], string> = {
  unchanged: 'Inchangée',
  moved: 'Déplacée ou renommée',
  replaced: 'Révision remplacée',
  added: 'Ajoutée',
};

/** Counts per change, for the summary above the list. */
export function changeSummary(steps: StepChange[], removed: number): string {
  const n = (c: StepChange['change']) => steps.filter((s) => s.change === c).length;
  const parts = [
    [n('replaced'), 'remplacée', 'remplacées'],
    [n('added'), 'ajoutée', 'ajoutées'],
    [n('moved'), 'déplacée ou renommée', 'déplacées ou renommées'],
    [removed, 'retirée', 'retirées'],
    [n('unchanged'), 'inchangée', 'inchangées'],
  ] as const;
  return parts
    .filter(([count]) => count > 0)
    .map(([count, one, many]) => `${count} étape${count > 1 ? 's' : ''} ${count > 1 ? many : one}`)
    .join(' · ');
}

/** A lesson document holding only some blocks, for the canonical preview of a diff side. */
export function partialDocument(content: unknown, blocks: unknown[], language: 'fr' | 'ar'): Record<string, unknown> {
  const base = content && typeof content === 'object' ? (content as Record<string, unknown>) : {};
  return {
    schemaVersion: base.schemaVersion ?? 1,
    title: typeof base.title === 'string' ? base.title : 'Extrait',
    language: base.language ?? language,
    status: base.status ?? 'unreviewed',
    objective: '',
    blocks,
  };
}

/* ---------- administrator recovery ---------- */

/** A revision that holds the course's candidate slot (draft, in review, or approved never published), not withdrawn. */
export const holdsCandidate = (r: AdminCourseRevision) =>
  r.withdrawnAt === null && (r.status === 'draft' || r.status === 'in_review' || (r.status === 'approved' && !r.everPublished));

export interface AdminCourseActions {
  suspend: boolean;
  archive: boolean;
  unarchive: boolean;
  /** Revisions that may be restored: published before, not withdrawn, not current. */
  restore: AdminCourseRevision[];
  /** Revisions that may be withdrawn: in review or approved, not withdrawn, not the published one. */
  withdraw: AdminCourseRevision[];
  /** Open drafts that may be discarded. */
  discard: AdminCourseRevision[];
  /** Transfer needs no open candidate (discard or withdraw it first). */
  transfer: boolean;
}

export function adminCourseActions(d: AdminCourseDetail): AdminCourseActions {
  const c = d.course;
  const current = c.publishedRevisionId;
  return {
    suspend: !c.archived && current !== null,
    archive: !c.archived,
    unarchive: c.archived,
    restore: c.archived ? [] : d.revisions.filter((r) => r.everPublished && r.withdrawnAt === null && r.id !== current && r.status === 'approved'),
    withdraw: d.revisions.filter((r) => r.withdrawnAt === null && r.id !== current && (r.status === 'in_review' || r.status === 'approved')),
    discard: d.revisions.filter((r) => r.status === 'draft'),
    transfer: !d.revisions.some(holdsCandidate),
  };
}

const LIFECYCLE_LABELS: Record<string, string> = {
  archive_request: 'Demande d’archivage par le propriétaire',
  archive: 'Archivage (sans version publiée)',
  unarchive: 'Désarchivage (aucune republication automatique)',
  discard_draft: 'Brouillon supprimé par un administrateur',
  transfer: 'Transfert de propriété',
};

const POINTER_LABELS: Record<string, string> = {
  publish: 'Publication',
  restore: 'Restauration',
  suspend: 'Suspension',
  archive: 'Archivage',
};

export const lifecycleLabel = (action: string) => LIFECYCLE_LABELS[action] ?? action;
export const pointerLabel = (action: string) => POINTER_LABELS[action] ?? action;
