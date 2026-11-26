// Course submission, review, amendments, publication and recovery
// (docs/API.md "Course review and publication (LMS-B04)" and the
// administrator reads of LMS-B06). Built on the authoring client
// (client.ts): same session, account check, CSRF handling and typed errors;
// error bodies keep their extra keys in ApiError.details. The server
// enforces every rule; nothing here grants or hides access on its own.
import { ApiError, type Api } from './client';
import type { Course, CourseRevision, CourseRevisionSummary } from './courses';

/* ---------- shared shapes ---------- */

export type CourseKind = 'ordinary' | 'amendment';

/** Why a reviewer may not decide: the course's author, or the author of a lesson revision it adds or replaces. */
export interface ReviewerProblem {
  reviewerId: number | null;
  /** Names are sent only in the `blocked` details. */
  reviewerName?: string | null;
  reason: 'course_author' | 'lesson_author' | 'no_subject_reviewer' | string;
  lessonId: number | null;
  lessonRevisionId: number | null;
}

/** A revision in review that no active reviewer of its subject may decide. */
export interface Blocked {
  reason: 'no_eligible_reviewer';
  details: ReviewerProblem[];
}

export interface CourseDecision {
  action: 'approve' | 'request_changes';
  reviewerId: number;
  reviewerName: string;
  comment: string | null;
  decidedAt: string;
}

/** The course revision object with the LMS-B04 keys. */
export interface ReviewedRevision extends CourseRevision {
  withdrawnAt: string | null;
  review: CourseDecision | null;
  blocked: Blocked | null;
}

export interface StepRef {
  moduleKey: string;
  position: number;
  title: string;
  lessonId: number;
  revisionId: number;
}

export interface NewerStep {
  moduleKey: string;
  position: number;
  title: string;
  lessonId: number;
  pinnedRevisionId: number;
  pinnedNumber: number;
  newerRevisionId: number;
  newerNumber: number;
}

export interface NewerRevisions {
  courseId: number;
  comparisonRevisionId: number | null;
  steps: NewerStep[];
}

export interface CourseReviewItem {
  revisionId: number;
  courseId: number;
  number: number;
  kind: CourseKind;
  title: string;
  language: 'fr' | 'ar';
  subjectId: number;
  authorId: number;
  authorName: string;
  submittedAt: string;
  eligible: boolean;
  blocked: Blocked | null;
}

export interface FieldChange {
  name: string;
  old: unknown;
  new: unknown;
}

export interface BlockChange {
  op: 'equal' | 'removed' | 'added';
  block: unknown;
}

/** The content diff of a replaced step over the two lesson documents. */
export interface ContentDiff {
  fields: FieldChange[];
  blocks: BlockChange[];
}

export interface StepChange {
  moduleKey: string;
  position: number;
  title: string;
  lessonId: number;
  revisionId: number;
  revisionNumber: number;
  change: 'unchanged' | 'moved' | 'replaced' | 'added';
  previousRevisionId: number | null;
  previousRevisionNumber: number | null;
  previousModuleKey: string | null;
  previousPosition: number | null;
  previousTitle: string | null;
  structureChanged: boolean;
  /**
   * For a replaced step, a side that is not readable now (withdrawn, or its
   * lesson unpublished or archived): `unavailable` for the new pin,
   * `previousUnavailable` for the previous revision. `diff` is then null.
   */
  unavailable?: boolean;
  previousUnavailable?: boolean;
  diff: ContentDiff | null;
  /** The pinned lesson revision is eligible now; only then is its document sent. */
  eligible: boolean;
  content: unknown | null;
}

export interface EarlierDecision extends CourseDecision {
  revisionId: number;
  number: number;
}

export interface CourseReview {
  revision: ReviewedRevision;
  eligible: boolean;
  problems: ReviewerProblem[];
  metadataChanged: boolean;
  steps: StepChange[];
  removedSteps: StepRef[];
  earlierDecisions: EarlierDecision[];
}

export interface CoursePublishItem {
  revisionId: number;
  courseId: number;
  coursePublicId: string;
  number: number;
  kind: CourseKind;
  title: string;
  language: 'fr' | 'ar';
  subjectId: number;
  authorId: number;
  authorName: string;
  approvedBy: number;
  approvedAt: string;
  baseRevisionId: number | null;
  publishedRevisionId: number | null;
  staleBase: boolean;
  archived: boolean;
  ineligibleSteps: number;
}

export interface CoursePointerChange {
  courseId: number;
  action: 'publish' | 'restore' | 'suspend' | 'archive';
  publishedRevisionId: number | null;
  previousRevisionId: number | null;
  actorId: number;
  reason: string | null;
  occurredAt: string;
}

export interface LifecycleEvent {
  id: number;
  courseId: number;
  actorId: number;
  action: 'archive_request' | 'archive' | 'unarchive' | 'discard_draft' | 'transfer';
  reason: string;
  revisionId: number | null;
  fromOwnerId: number | null;
  toOwnerId: number | null;
  occurredAt: string;
}

export interface ArchiveRequest extends LifecycleEvent {
  archived: boolean;
  publishedRevisionId: number | null;
  title: string;
}

export interface CourseWithdrawal {
  courseId: number;
  revisionId: number;
  withdrawnBy: number;
  reason: string;
  withdrawnAt: string;
}

export interface AdminCourse extends Course {
  ownerName: string;
  title: string;
  latestRevision?: CourseRevisionSummary & { withdrawn: boolean };
}

export interface AdminCourseRevision {
  id: number;
  number: number;
  status: CourseRevision['status'];
  kind: CourseKind;
  title: string;
  authorId: number;
  authorName: string;
  updatedAt: string;
  submittedAt: string | null;
  withdrawnAt: string | null;
  everPublished: boolean;
  review: CourseDecision | null;
}

export interface AdminCourseDetail {
  course: AdminCourse;
  revisions: AdminCourseRevision[];
  history: Array<CoursePointerChange & { actorName: string }>;
  lifecycle: Array<LifecycleEvent & { actorName: string }>;
  /** Active authors other than the owner: the possible targets of a transfer. */
  authors: Array<{ id: number; displayName: string }>;
}

/* ---------- error details ---------- */

/** `fields` of 422 incomplete: title, summary, objectives, modules or module:<key>. */
export const incompleteFields = (e: unknown): string[] =>
  e instanceof ApiError && Array.isArray(e.details.fields) ? (e.details.fields as unknown[]).filter((f): f is string => typeof f === 'string') : [];

/** `steps` of 409 ineligible. */
export const ineligibleSteps = (e: unknown): StepRef[] => (e instanceof ApiError && Array.isArray(e.details.steps) ? (e.details.steps as StepRef[]) : []);

/** `reasons` of 403 not_eligible_reviewer. */
export const reviewerReasons = (e: unknown): ReviewerProblem[] => (e instanceof ApiError && Array.isArray(e.details.reasons) ? (e.details.reasons as ReviewerProblem[]) : []);

/* ---------- calls ---------- */

const reason = (text: string) => ({ reason: text });

export function courseReviewApi(api: Api) {
  return {
    /* author */
    submit: (revisionId: number, expectedVersion: number) => api.request<ReviewedRevision>('POST', `/api/course-revisions/${revisionId}/submit`, { json: { expectedVersion } }),
    revision: (revisionId: number) => api.request<ReviewedRevision>('GET', `/api/course-revisions/${revisionId}`),
    newerRevisions: (courseId: number) => api.request<NewerRevisions>('GET', `/api/courses/${courseId}/newer-revisions`),
    /** An empty list takes every newer eligible revision of the notice. */
    createAmendment: (courseId: number, steps: Array<{ lessonId: number; revisionId: number }>) =>
      api.request<ReviewedRevision>('POST', `/api/courses/${courseId}/amendments`, { json: { steps } }),
    requestArchive: (courseId: number, text: string) => api.request<LifecycleEvent>('POST', `/api/courses/${courseId}/archive-request`, { json: reason(text) }),

    /* reviewer */
    reviewQueue: async (subjectId?: number) =>
      (await api.request<{ revisions: CourseReviewItem[] }>('GET', `/api/review/queue?type=course${subjectId ? `&subjectId=${subjectId}` : ''}`)).revisions,
    review: (revisionId: number) => api.request<CourseReview>('GET', `/api/review/course-revisions/${revisionId}`),
    /** `comment` is optional for approval (blank means none). */
    approve: (revisionId: number, comment: string) =>
      api.request<ReviewedRevision>('POST', `/api/review/course-revisions/${revisionId}/approve`, { json: comment.trim() ? { comment } : {} }),
    requestChanges: (revisionId: number, comment: string) => api.request<ReviewedRevision>('POST', `/api/review/course-revisions/${revisionId}/request-changes`, { json: { comment } }),

    /* publisher */
    publishQueue: async () => (await api.request<{ revisions: CoursePublishItem[] }>('GET', '/api/publish/queue?type=course')).revisions,
    publish: (revisionId: number, expectedPublishedRevisionId: number | null) =>
      api.request<CoursePointerChange>('POST', `/api/course-revisions/${revisionId}/publish`, { json: { expectedPublishedRevisionId } }),
    restore: (courseId: number, revisionId: number, expectedPublishedRevisionId: number | null) =>
      api.request<CoursePointerChange>('POST', `/api/courses/${courseId}/restore`, { json: { revisionId, expectedPublishedRevisionId } }),

    /* administrator */
    suspend: (courseId: number, text: string, expectedPublishedRevisionId: number | null) =>
      api.request<CoursePointerChange>('POST', `/api/courses/${courseId}/suspend`, { json: { reason: text, expectedPublishedRevisionId } }),
    archive: (courseId: number, text: string, expectedPublishedRevisionId: number | null) =>
      api.request<CoursePointerChange>('POST', `/api/courses/${courseId}/archive`, { json: { reason: text, expectedPublishedRevisionId } }),
    unarchive: (courseId: number, text: string) => api.request<LifecycleEvent>('POST', `/api/courses/${courseId}/unarchive`, { json: reason(text) }),
    transfer: (courseId: number, toUserId: number, text: string) => api.request<LifecycleEvent>('POST', `/api/courses/${courseId}/transfer`, { json: { toUserId, reason: text } }),
    withdraw: (revisionId: number, text: string) => api.request<CourseWithdrawal>('POST', `/api/course-revisions/${revisionId}/withdraw`, { json: reason(text) }),
    discardDraft: (revisionId: number, text: string) => api.request<LifecycleEvent>('DELETE', `/api/admin/course-revisions/${revisionId}`, { json: reason(text) }),
    archiveRequests: async () => (await api.request<{ requests: ArchiveRequest[] }>('GET', '/api/admin/courses/archive-requests')).requests,
    blockedReviews: async (cursor: string | null = null) =>
      api.request<{ revisions: CourseReviewItem[]; nextCursor: string | null }>('GET', `/api/admin/courses/blocked-reviews${cursor ? `?cursor=${encodeURIComponent(cursor)}` : ''}`),
    adminCourses: (cursor: string | null = null) =>
      api.request<{ courses: AdminCourse[]; nextCursor: string | null }>('GET', `/api/admin/courses${cursor ? `?cursor=${encodeURIComponent(cursor)}` : ''}`),
    adminCourse: (courseId: number) => api.request<AdminCourseDetail>('GET', `/api/admin/courses/${courseId}`),
  };
}

export type CourseReviewApi = ReturnType<typeof courseReviewApi>;
