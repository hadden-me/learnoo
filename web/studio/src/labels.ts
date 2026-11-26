// Interface wording for editorial states. Status is always written out:
// colour never stands alone (DESIGN.md).
import type { OwnedLesson, RevisionStatus } from './api/client';
import { catalogueLabel, type CatalogueLabel } from './catalogue';

export const STATUS_LABELS: Record<RevisionStatus, string> = {
  draft: 'Brouillon',
  in_review: 'En relecture',
  changes_requested: 'Modifications demandées',
  approved: 'Approuvée',
};

export const LANGUAGE_LABELS = { fr: 'Français', ar: 'Arabe' } as const;

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
  if (!latest) return { label: 'Aucune version en cours', tone: 'empty' };
  if (latest.status === 'approved') {
    return lesson.publishedRevisionId === latest.id ? { label: 'Publiée', tone: 'published' } : { label: 'Approuvée, en attente de publication', tone: 'approved' };
  }
  const tone = latest.status === 'draft' ? 'draft' : latest.status === 'in_review' ? 'review' : 'changes';
  return { label: STATUS_LABELS[latest.status], tone };
}

const dateTime = new Intl.DateTimeFormat('fr-FR', { dateStyle: 'medium', timeStyle: 'short' });
const time = new Intl.DateTimeFormat('fr-FR', { timeStyle: 'short' });
export const formatDateTime = (iso: string) => dateTime.format(new Date(iso));
export const formatTime = (date: Date) => time.format(date);
