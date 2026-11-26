// Interface wording for editorial states. Status is always written out:
// colour never stands alone (DESIGN.md).
import type { OwnedLesson, RevisionStatus } from './api/client';
import { catalogueLabel, type CatalogueLabel } from './catalogue';
import { intlLocale, t } from './i18n';

export const STATUS_LABELS: Record<RevisionStatus, string> = {
  draft: t('shell.status.draft'),
  in_review: t('shell.status.in_review'),
  changes_requested: t('shell.status.changes_requested'),
  approved: t('shell.status.approved'),
};

/** Content languages named in the interface language. */
export const LANGUAGE_LABELS = { fr: t('shell.language.fr'), ar: t('shell.language.ar') } as const;

/** The subject's and latest chapter's names of an owned lesson, in the lesson's language (else the other, else the code). */
export function ownedLessonPlace(lesson: OwnedLesson): { subject: CatalogueLabel; chapter: CatalogueLabel | null } {
  const latest = lesson.latestRevision;
  return {
    subject: catalogueLabel(lesson.subjectCode, lesson.subjectNameFr, lesson.subjectNameAr, lesson.language),
    chapter: latest?.chapterCode ? catalogueLabel(latest.chapterCode, latest.chapterNameFr, latest.chapterNameAr, lesson.language) : null,
  };
}

/** The lesson's current state as one phrase, including publication. */
export function lessonState(lesson: OwnedLesson): { label: string; tone: 'draft' | 'review' | 'changes' | 'approved' | 'published' | 'empty' } {
  const latest = lesson.latestRevision;
  if (!latest) return { label: t('shell.state.none'), tone: 'empty' };
  if (latest.status === 'approved') {
    return lesson.publishedRevisionId === latest.id ? { label: t('shell.state.published'), tone: 'published' } : { label: t('shell.state.awaitingPublication'), tone: 'approved' };
  }
  const tone = latest.status === 'draft' ? 'draft' : latest.status === 'in_review' ? 'review' : 'changes';
  return { label: STATUS_LABELS[latest.status], tone };
}

const dateTime = new Intl.DateTimeFormat(intlLocale(), { dateStyle: 'medium', timeStyle: 'short' });
const time = new Intl.DateTimeFormat(intlLocale(), { timeStyle: 'short' });
export const formatDateTime = (iso: string) => dateTime.format(new Date(iso));
export const formatTime = (date: Date) => time.format(date);
