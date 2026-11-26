// Course drafts (docs/API.md "Course drafts (LMS-B03)") and the curriculum
// reads they place a course with ("Curriculum (LMS-B02)"). Built on the
// authoring client (client.ts): same session, account check, CSRF handling
// and typed errors. The server enforces every rule (ownership, placement,
// attachability, limits); nothing here grants or hides access on its own.
import type { Api } from './client';

/* ---------- curriculum ---------- */

export type EditionState = 'draft' | 'current' | 'superseded' | 'retired';

export interface Section {
  id: number;
  levelId: number;
  code: string;
  nameFr: string;
  nameAr: string;
  displayOrder: number;
  retiredAt: string | null;
  version: number;
  createdAt: string;
}

export interface Level {
  id: number;
  code: string;
  nameFr: string;
  nameAr: string;
  displayOrder: number;
  retiredAt: string | null;
  version: number;
  createdAt: string;
  sections: Section[];
}

export interface AssignmentChapter {
  chapterId: number;
  code: string;
  position: number;
  retired: boolean;
}

export interface Assignment {
  id: number;
  subjectId: number;
  subjectCode: string;
  /** Current edition, and neither its section, level nor subject retired: may be newly selected by a course. */
  selectable: boolean;
  chapters: AssignmentChapter[];
}

export interface Edition {
  id: number;
  sectionId: number;
  code: string;
  labelFr: string;
  labelAr: string;
  state: EditionState;
  currentSince: string | null;
  supersededAt: string | null;
  retiredAt: string | null;
  version: number;
  createdAt: string;
  /** Present on GET /api/catalogue/editions/{id} only. */
  assignments?: Assignment[];
}

/* ---------- courses ---------- */

/** B04 adds the transitions; B03 drafts are always `draft`. */
export type CourseRevisionStatus = 'draft' | 'in_review' | 'changes_requested' | 'approved';

export interface CourseRevisionSummary {
  id: number;
  number: number;
  status: CourseRevisionStatus;
  title: string;
  version: number;
  updatedAt: string;
  /** Withdrawn by an administrator (LMS-B04): it no longer counts as the course's candidate. */
  withdrawn?: boolean;
}

export interface Course {
  id: number;
  publicId: string;
  ownerId: number;
  subjectId: number;
  language: 'fr' | 'ar';
  publishedRevisionId: number | null;
  archived: boolean;
  /** Not archived, no published revision, and the last pointer change was an administrator's suspension. */
  suspended?: boolean;
  createdAt: string;
  /** Only in the list; absent when the course has no revision (its only draft was discarded). */
  latestRevision?: CourseRevisionSummary;
}

export interface CoursePlacement {
  assignmentId: number;
  editionId: number;
  editionState: EditionState;
  selectable: boolean;
  /** The edition, section or level is retired: refused at submission (B04). */
  retired: boolean;
}

export interface CourseStep {
  position: number;
  title: string;
  lessonId: number;
  revisionId: number;
  revisionNumber: number;
  ownLesson: boolean;
  /** The pinned revision may be submitted with the course (approved, published at least once, lesson published). */
  eligible: boolean;
}

export interface CourseModule {
  key: string;
  position: number;
  title: string;
  chapterId: number | null;
  steps: CourseStep[];
}

export interface DroppedStep {
  moduleKey: string;
  position: number;
  title: string;
  lessonId: number;
  revisionId: number;
}

export interface CourseRevision {
  id: number;
  courseId: number;
  number: number;
  status: CourseRevisionStatus;
  kind: 'ordinary' | 'amendment';
  authorId: number;
  baseRevisionId: number | null;
  compareRevisionId: number | null;
  subjectId: number;
  language: 'fr' | 'ar';
  title: string;
  summary: string;
  objectives: string;
  prerequisites: string;
  coverMediaId: number | null;
  coverAlt: string | null;
  version: number;
  createdAt: string;
  updatedAt: string;
  submittedAt: string | null;
  assignments: CoursePlacement[];
  modules: CourseModule[];
  /** Only on POST /api/courses/{id}/drafts: steps of the copied revision left out because they are no longer attachable. */
  droppedSteps?: DroppedStep[];
}

export interface CourseSaveStep {
  lessonId: number;
  revisionId: number;
  title: string;
}

export interface CourseSaveModule {
  /** A key returned by an earlier read, or null for a new module. */
  key: string | null;
  title: string;
  chapterId: number | null;
  steps: CourseSaveStep[];
}

/** PUT /api/course-revisions/{id} body without expectedVersion: the save replaces everything. */
export interface CourseSaveBody {
  title: string;
  summary: string;
  objectives: string;
  prerequisites: string;
  coverMediaId: number | null;
  coverAlt: string | null;
  assignmentIds: number[];
  modules: CourseSaveModule[];
}

export interface AttachableLesson {
  lessonId: number;
  publicId: string;
  /** The caller's own lesson (any revision may be pinned in a draft); otherwise another author's published lesson. */
  own: boolean;
  title: string;
  publishedRevisionId: number | null;
  latestRevision: CourseRevisionSummary | null;
}

export interface EligibleRevision {
  id: number;
  number: number;
  status: 'draft' | 'in_review' | 'changes_requested' | 'approved';
  title: string;
  eligible: boolean;
}

export interface Page<T> {
  items: T[];
  nextCursor: string | null;
}

const enc = encodeURIComponent;

/** Course and curriculum endpoints on top of the shared client. */
export function courseApi(api: Api) {
  return {
    /** One page (100) of the caller's courses, newest first. */
    myCourses: async (cursor: string | null = null): Promise<Page<Course>> => {
      const r = await api.request<{ courses: Course[]; nextCursor: string | null }>('GET', `/api/courses?mine=1${cursor ? `&cursor=${enc(cursor)}` : ''}`);
      return { items: r.courses, nextCursor: r.nextCursor };
    },
    createCourse: (subjectId: number, language: 'fr' | 'ar') => api.request<{ course: Course; revision: CourseRevision }>('POST', '/api/courses', { json: { subjectId, language } }),
    deleteCourse: (courseId: number) => api.request<void>('DELETE', `/api/courses/${courseId}`),
    startDraft: (courseId: number) => api.request<CourseRevision>('POST', `/api/courses/${courseId}/drafts`, { json: {} }),
    revision: (revisionId: number) => api.request<CourseRevision>('GET', `/api/course-revisions/${revisionId}`),
    save: (revisionId: number, expectedVersion: number, body: CourseSaveBody) =>
      api.request<CourseRevision>('PUT', `/api/course-revisions/${revisionId}`, { json: { expectedVersion, ...body } }),
    discard: (revisionId: number, expectedVersion: number) => api.request<void>('DELETE', `/api/course-revisions/${revisionId}?expectedVersion=${expectedVersion}`),
    /** One page (50) of lessons that may be attached to the course; q matches titles ignoring case and accents. */
    attachableLessons: async (courseId: number, q = '', cursor: string | null = null): Promise<Page<AttachableLesson>> => {
      const params = [q ? `q=${enc(q)}` : '', cursor ? `cursor=${enc(cursor)}` : ''].filter(Boolean).join('&');
      const r = await api.request<{ lessons: AttachableLesson[]; nextCursor: string | null }>('GET', `/api/courses/${courseId}/attachable-lessons${params ? `?${params}` : ''}`);
      return { items: r.lessons, nextCursor: r.nextCursor };
    },
    eligibleRevisions: async (lessonId: number) => (await api.request<{ revisions: EligibleRevision[] }>('GET', `/api/lessons/${lessonId}/eligible-revisions`)).revisions,

    levels: async () => (await api.request<{ levels: Level[] }>('GET', '/api/catalogue/levels')).levels,
    sectionEditions: (sectionId: number) => api.request<{ section: Section; editions: Edition[] }>('GET', `/api/catalogue/sections/${sectionId}/editions`),
    edition: (editionId: number) => api.request<Edition>('GET', `/api/catalogue/editions/${editionId}`),
  };
}

export type CourseApi = ReturnType<typeof courseApi>;

/** Follows the list's cursor until the course is found (the API has no single-course read). */
export async function findOwnCourse(courses: CourseApi, courseId: number, maxPages = 50): Promise<Course | null> {
  let cursor: string | null = null;
  for (let page = 0; page < maxPages; page++) {
    const r: Page<Course> = await courses.myCourses(cursor);
    const found = r.items.find((c) => c.id === courseId);
    if (found) return found;
    // Newest first by ID: once the page passes below the ID, it is not in the list.
    if (r.nextCursor === null || r.items.some((c) => c.id < courseId)) return null;
    cursor = r.nextCursor;
  }
  return null;
}
