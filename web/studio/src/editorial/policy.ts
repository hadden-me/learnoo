// Which publication actions the interface offers for a lesson's revision,
// mirroring the server's rules (docs/API.md "Withdrawal, unpublication and
// restoration") so that accounts never see a control the server would
// refuse them. The server still checks everything, including the expected
// published revision, under the lesson lock.
import type { LessonPublication, LessonRevisionState, PointerEvent } from '../api/editorial';
import type { Spaces } from '../spaces';

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
  if (revision.withdrawn) return { label: 'Retirée définitivement', tone: 'withdrawn' };
  if (lesson.publishedRevisionId === revision.id) return { label: 'Publiée (version lue par les élèves)', tone: 'published' };
  if (revision.status === 'in_review') return { label: 'En relecture', tone: 'review' };
  if (revision.status === 'changes_requested') return { label: 'Modifications demandées', tone: 'changes' };
  if (revision.candidate) return { label: 'Approuvée, en attente de publication', tone: 'approved' };
  if (revision.everPublished) return { label: 'Approuvée, publiée auparavant', tone: 'approved' };
  return { label: 'Approuvée', tone: 'approved' };
}

/** One pointer audit event in words, revisions named by their lesson-scoped number. */
export function describeEvent(event: PointerEvent, numberOf: (id: number | null) => string): string {
  switch (event.action) {
    case 'publish':
      return `Publication de la révision ${numberOf(event.publishedRevisionId)}${event.previousRevisionId !== null ? ` (remplace la révision ${numberOf(event.previousRevisionId)})` : ''}`;
    case 'restore':
      return `Restauration de la révision ${numberOf(event.publishedRevisionId)}${event.previousRevisionId !== null ? ` (remplace la révision ${numberOf(event.previousRevisionId)})` : ' (la leçon était dépubliée)'}`;
    case 'unpublish':
      return `Dépublication (la révision ${numberOf(event.previousRevisionId)} n’est plus lue par les élèves)`;
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
  if (!s) return revision.submittedAt ? `Soumise le ${formatTime(revision.submittedAt)}` : null;
  const by = revision.authorName ? ` par ${revision.authorName}` : '';
  const note = s.backfilled ? ' (reconstitué : soumise avant l’enregistrement des soumissions)' : '';
  return `Soumise${by} le ${formatTime(s.submittedAt)}${note}`;
}
