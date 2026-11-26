// Unsaved-edit recovery copies of course drafts: crash protection only,
// never persistence, with the rules of the lesson copies (recovery.ts).
// The server is the only place a course is saved. While the editor holds
// changes the server has not accepted, a copy is kept in this browser's
// localStorage, keyed by user, course, revision, the server version the
// edits started from and the editor instance (tab). A copy is only ever
// listed for the user who wrote it, so an account switch never offers or
// sends another user's edits; sign-out deletes the user's copies; copies
// older than 14 days are deleted when the Studio starts. Copies are never
// sent anywhere and never loaded without the author asking.
import { isRecoverySuspended } from '../recovery';
import type { CourseDraft } from './outline';

export const COURSE_RECOVERY_PREFIX = 'learnoo-studio-course-recovery:v1:';

export interface CourseRecoveryTarget {
  userId: number;
  courseId: number;
  revisionId: number;
  baseVersion: number;
  /** Revision number, for display only. */
  revisionNumber: number;
  instance: string;
}

export interface CourseRecoveryEntry extends CourseRecoveryTarget {
  key: string;
  /** `conflict`: kept when the author reloaded the server version after a version conflict. */
  reason: 'unsaved' | 'conflict';
  savedAt: string;
  data: CourseDraft;
}

export const courseRecoveryKey = (t: CourseRecoveryTarget) => `${COURSE_RECOVERY_PREFIX}u${t.userId}:c${t.courseId}:r${t.revisionId}:v${t.baseVersion}:${t.instance}`;

function safe<T>(fn: () => T, fallback: T): T {
  try {
    return fn();
  } catch {
    return fallback;
  }
}

export function writeCourseRecovery(storage: Storage, target: CourseRecoveryTarget, data: CourseDraft, reason: CourseRecoveryEntry['reason'] = 'unsaved', now: () => Date = () => new Date()): boolean {
  if (isRecoverySuspended()) return false;
  const key = courseRecoveryKey(target);
  const entry: CourseRecoveryEntry = { ...target, key, reason, savedAt: now().toISOString(), data };
  return safe(() => {
    storage.setItem(key, JSON.stringify(entry));
    return true;
  }, false);
}

export function removeCourseRecovery(storage: Storage, key: string): void {
  if (!key.startsWith(COURSE_RECOVERY_PREFIX)) return;
  safe(() => storage.removeItem(key), undefined);
}

const isText = (v: unknown) => typeof v === 'string';

function parse(key: string, raw: string | null): CourseRecoveryEntry | null {
  if (raw === null) return null;
  const e = safe(() => JSON.parse(raw) as CourseRecoveryEntry, null);
  if (!e || typeof e !== 'object' || e.key !== key || typeof e.userId !== 'number' || typeof e.courseId !== 'number' || typeof e.instance !== 'string' || !e.data || typeof e.data !== 'object') return null;
  const d = e.data;
  if (![d.title, d.summary, d.objectives, d.prerequisites, d.coverAlt].every(isText) || !Array.isArray(d.assignmentIds) || !Array.isArray(d.modules)) return null;
  const valid = d.modules.every((m) => m && isText(m.localId) && isText(m.title) && Array.isArray(m.steps) && m.steps.every((s) => s && isText(s.localId) && isText(s.title) && typeof s.lessonId === 'number' && typeof s.revisionId === 'number'));
  return valid ? e : null;
}

function keys(storage: Storage): string[] {
  return safe(() => Array.from({ length: storage.length }, (_, i) => storage.key(i)), [] as (string | null)[]).filter((k): k is string => !!k?.startsWith(COURSE_RECOVERY_PREFIX));
}

function scan(storage: Storage, userId: number, keep: (e: CourseRecoveryEntry) => boolean): CourseRecoveryEntry[] {
  const prefix = `${COURSE_RECOVERY_PREFIX}u${userId}:`;
  const out: CourseRecoveryEntry[] = [];
  for (const key of keys(storage)) {
    if (!key.startsWith(prefix)) continue;
    const e = parse(key, safe(() => storage.getItem(key), null));
    if (e && e.userId === userId && keep(e)) out.push(e);
  }
  return out.sort((a, b) => b.savedAt.localeCompare(a.savedAt));
}

/** One user's copies of one course (any revision), newest first, without excludeKey (the editor's own live copy). */
export const findCourseRecovery = (storage: Storage, userId: number, courseId: number, excludeKey?: string) => scan(storage, userId, (e) => e.courseId === courseId && e.key !== excludeKey);

export const findUserCourseRecovery = (storage: Storage, userId: number) => scan(storage, userId, () => true);

/** Deletes every copy of `userId` (sign-out), and/or every copy saved before `olderThan` or unreadable. */
export function purgeCourseRecovery(storage: Storage, { userId, olderThan }: { userId?: number; olderThan?: Date }): void {
  for (const key of keys(storage)) {
    if (userId !== undefined && key.startsWith(`${COURSE_RECOVERY_PREFIX}u${userId}:`)) {
      removeCourseRecovery(storage, key);
      continue;
    }
    if (olderThan) {
      const e = parse(key, safe(() => storage.getItem(key), null));
      const saved = e ? Date.parse(e.savedAt) : NaN;
      if (!e || !(saved >= olderThan.getTime())) removeCourseRecovery(storage, key);
    }
  }
}
