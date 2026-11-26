// Review, publication, administration and error-report endpoints
// (docs/API.md "Review and publication", "Withdrawal, unpublication and
// restoration", "Error reports" and the Studio read endpoints of issue #66).
// Built on the authoring client (client.ts): same session, CSRF handling and
// typed errors. Every rule is enforced by the server; nothing here grants or
// hides access on its own.
import type { Api, Revision } from './client';

export interface ReviewQueueItem {
  revisionId: number;
  lessonId: number;
  number: number;
  title: string;
  language: 'fr' | 'ar';
  subjectId: number;
  chapterId: number | null;
  authorId: number;
  authorName: string;
  submittedAt: string;
}

export interface PublishQueueItem {
  revisionId: number;
  lessonId: number;
  lessonPublicId: string;
  number: number;
  title: string;
  language: 'fr' | 'ar';
  subjectId: number;
  subjectCode: string;
  authorId: number;
  authorName: string;
  approvedBy: number;
  approverName: string;
  approvedAt: string;
  baseRevisionId: number | null;
  publishedRevisionId: number | null;
  /** The published revision changed since this one was drafted: publication will be refused (stale_base). */
  staleBase: boolean;
}

export interface Publication {
  lessonId: number;
  publishedRevisionId: number;
  previousRevisionId: number | null;
  publishedBy: number;
  publishedAt: string;
}

export interface PointerChange {
  lessonId: number;
  action: 'restore' | 'unpublish';
  publishedRevisionId: number | null;
  previousRevisionId: number | null;
  actorId: number;
  reason: string | null;
  occurredAt: string;
}

export interface Withdrawal {
  revisionId: number;
  lessonId: number;
  withdrawnBy: number;
  reason: string;
  withdrawnAt: string;
  unpublication: PointerChange | null;
}

export interface LessonRevisionState {
  id: number;
  number: number;
  status: 'in_review' | 'changes_requested' | 'approved';
  title: string;
  /** null in the publisher view (names are for administrators). */
  authorId: number | null;
  authorName: string | null;
  baseRevisionId: number | null;
  submittedAt: string | null;
  approvedBy: number | null;
  approverName: string | null;
  approvedAt: string | null;
  everPublished: boolean;
  candidate: boolean;
  withdrawn: boolean;
  /** Reason and actor: administrator view only. */
  withdrawal: { withdrawnBy: number; withdrawnByName: string; reason: string; withdrawnAt: string } | null;
  /**
   * The submission audit row (migration 0011): administrator view only, null for publishers.
   * The submitter is always the author. `backfilled`: reconstructed by the migration for a
   * revision submitted before submissions were audited, not recorded at submission.
   */
  submission: { submittedBy: number; submittedAt: string; backfilled: boolean } | null;
}

export interface PointerEvent {
  action: 'publish' | 'restore' | 'unpublish';
  publishedRevisionId: number | null;
  previousRevisionId: number | null;
  actorId: number;
  actorName: string;
  reason: string | null;
  occurredAt: string;
}

export interface LessonPublication {
  /** `publisher`: only what restoration needs (approved revisions, no names, reasons or history). */
  view: 'administrator' | 'publisher';
  lessonId: number;
  publicId: string;
  subjectId: number;
  subjectCode: string;
  language: 'fr' | 'ar';
  archived: boolean;
  publishedRevisionId: number | null;
  revisions: LessonRevisionState[];
  history: PointerEvent[];
}

export interface AdminLesson {
  lessonId: number;
  publicId: string;
  subjectCode: string;
  language: 'fr' | 'ar';
  archived: boolean;
  publishedRevisionId: number | null;
  title: string;
  pendingRevisionId: number | null;
  lastChange: string;
}

export type ReportStatus = 'open' | 'resolved' | 'dismissed';

export interface ErrorReport {
  id: number;
  lessonId: number;
  lessonPublicId: string;
  subjectId: number;
  revisionId: number;
  revisionNumber: number;
  revisionTitle: string;
  currentlyPublished: boolean;
  message: string;
  status: ReportStatus;
  createdAt: string;
  resolution: { action: 'resolve' | 'dismiss'; actorId: number; note: string; occurredAt: string } | null;
}

const enc = encodeURIComponent;

/** Typed calls for the editorial spaces, sharing the authoring client's session. */
export function editorialApi(api: Api) {
  return {
    reviewQueue: async (subjectId?: number) =>
      (await api.request<{ revisions: ReviewQueueItem[] }>('GET', `/api/review/queue${subjectId ? `?subjectId=${subjectId}` : ''}`)).revisions,
    reviewRevision: (id: number) => api.request<Revision>('GET', `/api/review/revisions/${id}`),
    /** `comment` is optional for approval (blank means none). */
    approve: (id: number, comment: string) => api.request<Revision>('POST', `/api/review/revisions/${id}/approve`, { json: comment.trim() ? { comment } : {} }),
    requestChanges: (id: number, comment: string) => api.request<Revision>('POST', `/api/review/revisions/${id}/request-changes`, { json: { comment } }),

    publishQueue: async () => (await api.request<{ revisions: PublishQueueItem[] }>('GET', '/api/publish/queue')).revisions,
    /** The revision with its queue facts (current pointer, stale base, names). */
    publishRevision: (id: number) => api.request<{ item: PublishQueueItem; revision: Revision }>('GET', `/api/publish/revisions/${id}`),
    publish: (id: number) => api.request<Publication>('POST', `/api/revisions/${id}/publish`, { json: {} }),

    adminLessons: async () => (await api.request<{ lessons: AdminLesson[] }>('GET', '/api/admin/lessons')).lessons,
    /** `ref` is a numeric lesson ID or the lesson's public ID. */
    lessonPublication: (ref: string | number) => api.request<LessonPublication>('GET', `/api/lessons/${enc(String(ref))}/publication`),
    withdraw: (revisionId: number, reason: string, expectedPublishedRevisionId: number | null) =>
      api.request<Withdrawal>('POST', `/api/revisions/${revisionId}/withdraw`, { json: { reason, expectedPublishedRevisionId } }),
    unpublish: (lessonId: number, reason: string, expectedPublishedRevisionId: number) =>
      api.request<PointerChange>('POST', `/api/lessons/${lessonId}/unpublish`, { json: { reason, expectedPublishedRevisionId } }),
    restore: (revisionId: number, expectedPublishedRevisionId: number | null) =>
      api.request<PointerChange>('POST', `/api/revisions/${revisionId}/restore`, { json: { expectedPublishedRevisionId } }),

    reports: async (status?: ReportStatus) => (await api.request<{ reports: ErrorReport[] }>('GET', `/api/review/error-reports${status ? `?status=${status}` : ''}`)).reports,
    report: (id: number) => api.request<ErrorReport>('GET', `/api/review/error-reports/${id}`),
    resolveReport: (id: number, action: 'resolve' | 'dismiss', note: string) => api.request<ErrorReport>('POST', `/api/review/error-reports/${id}/${action}`, { json: { note } }),
  };
}

export type EditorialApi = ReturnType<typeof editorialApi>;
