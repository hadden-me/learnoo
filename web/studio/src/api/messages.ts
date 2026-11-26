// Interface-language explanations for the server's error codes (docs/API.md). The
// server's own `message` is English and for diagnostics; the validator's
// reason for 422 invalid_content is shown after the explanation because it
// names the faulty part of the document.
import { ApiError } from './client';
import { t } from '../i18n';

/** Server codes with an explanation in the catalog (shell.error.<code>). */
const CODES = [
  'network', 'unauthenticated', 'account_changed', 'csrf_rejected', 'cross_origin_rejected', 'forbidden', 'not_found', 'version_conflict', 'not_draft',
  'active_candidate_exists', 'archived', 'translation_exists', 'retry', 'too_large', 'unsupported_media_type', 'unsupported_heic', 'unsupported_cmyk',
  'type_mismatch', 'invalid_content', 'language_mismatch', 'unknown_subject', 'unknown_chapter', 'unknown_media', 'unknown_lesson', 'translation_subject',
  'invalid_image', 'invalid_dimensions', 'image_too_complex', 'trailing_data', 'upload_timeout', 'busy', 'internal', 'invalid_request',
] as const;
type Code = (typeof CODES)[number];
const isCode = (code: string): code is Code => (CODES as readonly string[]).includes(code);

/** The catalog's explanation of a server code, or null. */
export const codeMessage = (code: string): string | null => (isCode(code) ? t(`shell.error.${code}`) : null);

const HEIF_TYPES = ['image/heic', 'image/heif', 'image/heic-sequence', 'image/heif-sequence'];

/**
 * A HEIC/HEIF photo, which the server refuses (`unsupported_heic`). The extension counts only when the browser
 * gives no type (empty or application/octet-stream); a file it identifies as another type is left to the other checks.
 */
export const isHeif = (file: { type: string; name: string }): boolean => {
  const type = file.type.toLowerCase();
  if (HEIF_TYPES.includes(type)) return true;
  return (type === '' || type === 'application/octet-stream') && /\.(heic|heif)$/i.test(file.name);
};

/** A sentence for the user in the interface language; includes the validator's reason for invalid content. */
export function describeError(error: unknown): string {
  if (!(error instanceof ApiError)) return t('shell.error.unexpected');
  const base = codeMessage(error.code) ?? t('shell.error.server', { status: error.status || t('shell.error.networkStatus') });
  if (error.code === 'invalid_content' && error.message) return t('shell.error.detail', { base, detail: error.message });
  if (error.retryAfter !== null && (error.code === 'busy' || error.code === 'rate_limited')) return t('shell.error.retryAfter', { base, seconds: error.retryAfter });
  return base;
}
