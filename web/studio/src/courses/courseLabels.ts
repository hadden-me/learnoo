// Interface wording for courses. States are always written out: colour never
// stands alone (DESIGN.md). Course, module, lesson and official chapter are
// named consistently (LMS_PRODUCT_SPEC §10).
import type { Course, CourseRevisionStatus, EditionState } from '../api/courses';
import { t } from '../i18n';

export const COURSE_STATUS_LABELS: Record<CourseRevisionStatus, string> = {
  draft: t('shell.status.draft'),
  in_review: t('shell.status.in_review'),
  changes_requested: t('shell.status.changes_requested'),
  approved: t('courses.status.approved'),
};

export type CourseStateFilter = 'all' | CourseRevisionStatus | 'none';

export const COURSE_FILTERS: Array<[CourseStateFilter, string]> = [
  ['all', t('courses.filter.all')],
  ['draft', COURSE_STATUS_LABELS.draft],
  ['in_review', COURSE_STATUS_LABELS.in_review],
  ['changes_requested', COURSE_STATUS_LABELS.changes_requested],
  ['approved', COURSE_STATUS_LABELS.approved],
  ['none', t('courses.filter.none')],
];

/**
 * The course's current state as one phrase, with the tone classes of the
 * lesson list. Archive and suspension come first: they decide whether
 * students can read the course, whatever revision is in progress.
 */
export function courseState(course: Course): { label: string; tone: 'draft' | 'review' | 'changes' | 'approved' | 'published' | 'empty' | 'withdrawn' } {
  const latest = course.latestRevision;
  if (course.archived) return { label: t('courses.state.archived'), tone: 'withdrawn' };
  if (course.suspended) return { label: t('courses.state.suspended'), tone: 'withdrawn' };
  if (!latest) return { label: t('shell.state.none'), tone: 'empty' };
  if (latest.withdrawn) return { label: t('courses.state.withdrawn'), tone: 'withdrawn' };
  if (latest.status === 'approved') {
    if (course.publishedRevisionId === latest.id) return { label: t('courses.state.published'), tone: 'published' };
    if (latest.everPublished) return { label: t('courses.state.publishedBefore'), tone: 'approved' };
    return { label: t('courses.state.awaiting'), tone: 'approved' };
  }
  const tone = latest.status === 'draft' ? 'draft' : latest.status === 'in_review' ? 'review' : 'changes';
  return { label: COURSE_STATUS_LABELS[latest.status], tone };
}

export const EDITION_STATE_LABELS: Record<EditionState, string> = {
  draft: t('courses.edition.draft'),
  current: t('courses.edition.current'),
  superseded: t('courses.edition.superseded'),
  retired: t('courses.edition.retired'),
};

export const courseTitle = (title: string | undefined, id: number) => title?.trim() || t('courses.untitledCourse', { id });
