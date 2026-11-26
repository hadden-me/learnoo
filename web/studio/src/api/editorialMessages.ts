// French explanations for the review, publication, administration and
// error-report codes (docs/API.md). Codes shared with authoring fall back to
// describeError (messages.ts). Each says what happened and that nothing was
// changed when the server guarantees it.
import { ApiError } from './client';
import { t } from '../i18n';
import { describeError } from './messages';

const MESSAGES: Record<string, string> = {
  forbidden: t('review.error.forbidden'),
  not_found: t('review.error.not_found'),
  self_review: t('review.error.self_review'),
  not_in_review: t('review.error.not_in_review'),
  comment_required: t('review.error.comment_required'),
  invalid_comment: t('review.error.invalid_comment'),
  not_approved: t('review.error.not_approved'),
  already_published: t('review.error.already_published'),
  stale_base: t('review.error.stale_base'),
  withdrawn: t('review.error.withdrawn'),
  already_withdrawn: t('review.error.already_withdrawn'),
  stale_pointer: t('review.error.stale_pointer'),
  not_published: t('review.error.not_published'),
  never_published: t('review.error.never_published'),
  already_current: t('review.error.already_current'),
  ineligible: t('review.error.ineligible'),
  reason_required: t('review.error.reason_required'),
  invalid_reason: t('review.error.invalid_reason'),
  not_open: t('review.error.not_open'),
  note_required: t('review.error.note_required'),
  invalid_note: t('review.error.invalid_note'),
  immutable: t('review.error.immutable'),
  archived: t('review.error.archived'),
};

/** A French sentence for an editorial failure. */
export function describeEditorialError(error: unknown): string {
  if (error instanceof ApiError && error.code in MESSAGES) return MESSAGES[error.code];
  return describeError(error);
}
