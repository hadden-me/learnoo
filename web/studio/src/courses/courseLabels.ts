// Interface wording for courses. States are always written out: colour never
// stands alone (DESIGN.md). Course, module, lesson and official chapter are
// named consistently (LMS_PRODUCT_SPEC §10).
import type { Course, CourseRevisionStatus, EditionState } from '../api/courses';

export const COURSE_STATUS_LABELS: Record<CourseRevisionStatus, string> = {
  draft: 'Brouillon',
  in_review: 'En relecture',
  changes_requested: 'Modifications demandées',
  approved: 'Approuvé',
};

export type CourseStateFilter = 'all' | CourseRevisionStatus | 'none';

export const COURSE_FILTERS: Array<[CourseStateFilter, string]> = [
  ['all', 'Tous les états'],
  ['draft', 'Brouillon'],
  ['in_review', 'En relecture'],
  ['changes_requested', 'Modifications demandées'],
  ['approved', 'Approuvé'],
  ['none', 'Sans version en cours'],
];

/** The course's current state as one phrase, with the tone classes of the lesson list. */
export function courseState(course: Course): { label: string; tone: 'draft' | 'review' | 'changes' | 'approved' | 'published' | 'empty' } {
  const latest = course.latestRevision;
  if (!latest) return { label: 'Aucune version en cours', tone: 'empty' };
  if (latest.status === 'approved') return course.publishedRevisionId === latest.id ? { label: 'Publié', tone: 'published' } : { label: 'Approuvé, en attente de publication', tone: 'approved' };
  const tone = latest.status === 'draft' ? 'draft' : latest.status === 'in_review' ? 'review' : 'changes';
  return { label: COURSE_STATUS_LABELS[latest.status], tone };
}

export const EDITION_STATE_LABELS: Record<EditionState, string> = {
  draft: 'édition en préparation',
  current: 'édition en vigueur',
  superseded: 'édition remplacée',
  retired: 'édition retirée',
};

export const courseTitle = (title: string | undefined, id: number) => title?.trim() || `Cours sans titre (n° ${id})`;
