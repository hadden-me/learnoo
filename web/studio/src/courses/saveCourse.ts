// Saving a course draft: the only persistence path. Every save names the
// version it was based on (expectedVersion); a stale version is reported as
// a conflict and the caller keeps the local edits (drafts.ts does the same
// for lessons).
import { isApiError } from '../api/client';
import type { CourseApi, CourseRevision, CourseSaveBody } from '../api/courses';

export type CourseSaveOutcome =
  | { kind: 'saved'; revision: CourseRevision }
  | { kind: 'conflict' }
  /** The draft left draft status (submitted from elsewhere, B04). */
  | { kind: 'not_draft' }
  | { kind: 'expired' }
  /** Refused input (invalid_input, invalid_step, placement_invalid, unknown_media…), network or server error. */
  | { kind: 'failed'; error: unknown };

/** One write, repeated once when the database asked for a retry (409 retry: nothing was written). Never throws. */
export async function saveCourse(courses: CourseApi, revisionId: number, version: number, body: CourseSaveBody): Promise<CourseSaveOutcome> {
  for (let attempt = 0; ; attempt++) {
    try {
      return { kind: 'saved', revision: await courses.save(revisionId, version, body) };
    } catch (error) {
      if (isApiError(error, 'retry') && attempt === 0) continue;
      if (isApiError(error, 'version_conflict')) return { kind: 'conflict' };
      if (isApiError(error, 'not_draft')) return { kind: 'not_draft' };
      if (isApiError(error, 'unauthenticated')) return { kind: 'expired' };
      return { kind: 'failed', error };
    }
  }
}
