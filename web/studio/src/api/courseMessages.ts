// French explanations for the course draft codes (docs/API.md "Course
// drafts"). Codes shared with lessons fall back to describeError
// (messages.ts). For refused input the server's own message (English, it
// names the field, module or step) is added as a detail, as messages.ts does
// for the lesson validator.
import { ApiError } from './client';
import { describeError } from './messages';
import { t } from '../i18n';

const MESSAGES: Record<string, string> = {
  not_found: t('courses.error.not_found'),
  version_conflict: t('courses.error.version_conflict'),
  not_draft: t('courses.error.not_draft'),
  active_candidate_exists: t('courses.error.active_candidate_exists'),
  archived: t('courses.error.archived'),
  has_history: t('courses.error.has_history'),
  invalid_input: t('courses.error.invalid_input'),
  placement_invalid: t('courses.error.placement_invalid'),
  invalid_step: t('courses.error.invalid_step'),
  unknown_media: t('courses.error.unknown_media'),
  unknown_subject: t('shell.error.unknown_subject'),
};

const DETAILED = new Set(['invalid_input', 'placement_invalid', 'invalid_step']);

/** A French sentence for a course failure. */
export function describeCourseError(error: unknown): string {
  if (!(error instanceof ApiError) || !(error.code in MESSAGES)) return describeError(error);
  const base = MESSAGES[error.code];
  return DETAILED.has(error.code) && error.message ? t('courses.error.serverDetail', { base, detail: error.message }) : base;
}
