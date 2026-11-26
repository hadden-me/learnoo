// French explanations for the course review, publication and recovery codes
// (docs/API.md "Course review and publication (LMS-B04)"). Course draft
// codes fall back to describeCourseError (courseMessages.ts), the rest to
// describeEditorialError. Each says what happened and that nothing changed
// when the server guarantees it.
import { ApiError } from './client';
import { t } from '../i18n';
import { describeCourseError } from './courseMessages';
import { describeEditorialError } from './editorialMessages';

const MESSAGES: Record<string, string> = {
  incomplete: t('review.courseError.incomplete'),
  ineligible: t('review.courseError.ineligible'),
  draft_exists: t('review.courseError.draft_exists'),
  active_candidate_exists: t('review.courseError.active_candidate_exists'),
  not_published: t('review.courseError.not_published'),
  no_newer_revisions: t('review.courseError.no_newer_revisions'),
  not_amendable: t('review.courseError.not_amendable'),
  invalid_step: t('review.courseError.invalid_step'),
  not_eligible_reviewer: t('review.courseError.not_eligible_reviewer'),
  not_in_review: t('review.courseError.not_in_review'),
  withdrawn: t('review.courseError.withdrawn'),
  comment_required: t('review.error.comment_required'),
  invalid_comment: t('review.courseError.invalid_comment'),
  reason_required: t('review.error.reason_required'),
  invalid_reason: t('review.error.invalid_reason'),
  stale_pointer: t('review.courseError.stale_pointer'),
  stale_base: t('review.courseError.stale_base'),
  not_approved: t('review.courseError.not_approved'),
  already_published: t('review.courseError.already_published'),
  never_published: t('review.courseError.never_published'),
  already_current: t('review.courseError.already_current'),
  published: t('review.courseError.published'),
  not_withdrawable: t('review.courseError.not_withdrawable'),
  archived: t('review.courseError.archived'),
  not_archived: t('review.courseError.not_archived'),
  never_submitted: t('review.courseError.never_submitted'),
  unknown_user: t('review.courseError.unknown_user'),
  not_draft: t('review.courseError.not_draft'),
};

/** A French sentence for a course workflow failure. */
export function describeCourseWorkflowError(error: unknown): string {
  if (error instanceof ApiError) {
    if (error.code in MESSAGES) return MESSAGES[error.code];
    if (error.code === 'placement_invalid' || error.code === 'version_conflict' || error.code === 'not_found') return describeCourseError(error);
  }
  return describeEditorialError(error);
}
