// Which publication actions the interface offers for a lesson's revision,
// mirroring the server's rules (docs/API.md "Withdrawal, unpublication and
// restoration") so that accounts never see a control the server would
// refuse them. The server still checks everything, including the expected
// published revision, under the lesson lock.
import type { LessonPublication, LessonRevisionState, PointerEvent } from '../api/editorial';
import type { Spaces } from '../spaces';
import { t } from '../i18n';

export interface RevisionActions {
  /** The revision is the lesson's published revision. */
  current: boolean;
  /** Administrators withdraw approved revisions not already withdrawn (published or pending). */
  withdraw: boolean;
  /** Publishers and administrators restore a revision published before, not withdrawn, not current. */
  restore: boolean;
}

export function revisionActions(lesson: LessonPublication, revision: LessonRevisionState, spaces: Spaces): RevisionActions {
  const current = lesson.publishedRevisionId === revision.id;
  const approved = revision.status === 'approved' && !revision.withdrawn;
  return {
    current,
    withdraw: spaces.administrator && approved,
    restore: (spaces.publisher || spaces.administrator) && approved && revision.everPublished && !current && !lesson.archived,
  };
}

/** Emergency unpublication: administrators, on a lesson that has a published revision. */
export const canUnpublish = (lesson: LessonPublication, spaces: Spaces) => spaces.administrator && lesson.publishedRevisionId !== null;

/** The revision's state in words; colour never stands alone (DESIGN.md). */
export function revisionStateLabel(lesson: LessonPublication, revision: LessonRevisionState): { label: string; tone: 'published' | 'withdrawn' | 'approved' | 'review' | 'changes' } {
  if (revision.withdrawn) return { label: t('review.state.withdrawn'), tone: 'withdrawn' };
  if (lesson.publishedRevisionId === revision.id) return { label: t('review.state.current'), tone: 'published' };
  if (revision.status === 'in_review') return { label: t('shell.status.in_review'), tone: 'review' };
  if (revision.status === 'changes_requested') return { label: t('shell.status.changes_requested'), tone: 'changes' };
  if (revision.candidate) return { label: t('shell.state.awaitingPublication'), tone: 'approved' };
  if (revision.everPublished) return { label: t('review.state.publishedBefore'), tone: 'approved' };
  return { label: t('shell.status.approved'), tone: 'approved' };
}

/** One pointer audit event in words, revisions named by their lesson-scoped number. */
export function describeEvent(event: PointerEvent, numberOf: (id: number | null) => string): string {
  switch (event.action) {
    case 'publish':
      return event.previousRevisionId !== null
        ? t('review.event.publishReplacing', { number: numberOf(event.publishedRevisionId), previous: numberOf(event.previousRevisionId) })
        : t('review.event.publish', { number: numberOf(event.publishedRevisionId) });
    case 'restore':
      return event.previousRevisionId !== null
        ? t('review.event.restoreReplacing', { number: numberOf(event.publishedRevisionId), previous: numberOf(event.previousRevisionId) })
        : t('review.event.restoreUnpublished', { number: numberOf(event.publishedRevisionId) });
    case 'unpublish':
      return t('review.event.unpublish', { previous: numberOf(event.previousRevisionId) });
  }
}

/**
 * Who submitted a revision and when, from its submission audit row (administrator view). The
 * submitter is always the revision's author. A backfilled row was reconstructed when submissions
 * started being audited, so it is marked as such. Without a row (publisher view) it falls back
 * to the revision's submission time, if any.
 */
export function describeSubmission(revision: LessonRevisionState, formatTime: (iso: string) => string): string | null {
  const s = revision.submission;
  if (!s) return revision.submittedAt ? t('review.submission.at', { date: formatTime(revision.submittedAt) }) : null;
  const date = formatTime(s.submittedAt);
  if (revision.authorName) return t(s.backfilled ? 'review.submission.byAtBackfilled' : 'review.submission.byAt', { author: revision.authorName, date });
  return t(s.backfilled ? 'review.submission.atBackfilled' : 'review.submission.at', { date });
}
