// Unsaved-edit recovery buffer: crash protection only, never persistence.
//
// The server is the only place a lesson is saved. While the editor holds
// changes the server has not accepted, a copy is kept in this browser's
// localStorage so that a crash, a closed tab or an expired session does not
// lose them. Each copy is keyed by user, revision and the server version the
// edits were based on (or "new" for a lesson not yet created), is labelled
// in the interface as an unsaved local copy, and is removed when the edits
// it holds are saved or when the author deletes it. Each editor instance
// (tab) writes its own copy, so two tabs, or a new session after a crash,
// never overwrite a copy the author has not looked at yet. It is never sent
// anywhere and never loaded without the author asking.

export const RECOVERY_PREFIX = 'learnoo-studio-recovery:v1:';

/** What the editor needs to restore edits exactly (TipTap JSON, not schema v1, so invalid intermediate states survive). */
export interface RecoveryData {
  doc: unknown;
  title: string;
  objective: string;
  prerequisites: string;
  chapterId: number | null;
  mediaIds: number[];
}

/** Parameters of a lesson that does not exist on the server yet. */
export interface NewLessonParams {
  subjectId: number;
  subjectCode: string;
  language: 'fr' | 'ar';
  translationOf: number | null;
  /** Set when the lesson exists but has no revision (its only draft was discarded). */
  lessonId?: number;
}

export interface RecoveryTarget {
  userId: number;
  /** null for a lesson or draft not yet created on the server. */
  revisionId: number | null;
  /** The server version the edits started from; null when not yet created. */
  baseVersion: number | null;
  /** The lesson, once it exists on the server (keys not-yet-created drafts; finds copies across a lesson's revisions). */
  lessonId?: number | null;
  /** Revision number, for display only. */
  revisionNumber?: number | null;
  /** The editor instance (one per tab and page load) that wrote the copy. */
  instance: string;
}

export interface RecoveryEntry extends RecoveryTarget {
  key: string;
  /** `conflict`: kept when the author reloaded the server version after a version conflict. */
  reason: 'unsaved' | 'conflict';
  savedAt: string;
  data: RecoveryData;
  newLesson?: NewLessonParams;
}

/** Copies older than this are deleted when the Studio starts: crash protection, not storage. */
export const RECOVERY_MAX_AGE_MS = 14 * 24 * 60 * 60 * 1000;

export function recoveryKey(target: RecoveryTarget): string {
  const where = target.revisionId === null ? (target.lessonId ? `lesson-${target.lessonId}:new` : 'new') : `r${target.revisionId}:v${target.baseVersion}`;
  return `${RECOVERY_PREFIX}u${target.userId}:${where}:${target.instance}`;
}

/** Storage can be missing or full (private mode, quota): recovery is best effort and must never break editing. */
function safe<T>(fn: () => T, fallback: T): T {
  try {
    return fn();
  } catch {
    return fallback;
  }
}

/** After sign-out nothing more is written (an unload handler would otherwise recreate the copies just purged). */
let suspended = false;
export const suspendRecovery = () => {
  suspended = true;
};

export function writeRecovery(
  storage: Storage,
  target: RecoveryTarget,
  data: RecoveryData,
  reason: RecoveryEntry['reason'] = 'unsaved',
  newLesson?: NewLessonParams,
  now: () => Date = () => new Date(),
): boolean {
  if (suspended) return false;
  const key = recoveryKey(target);
  const entry: RecoveryEntry = { ...target, key, reason, savedAt: now().toISOString(), data, ...(newLesson ? { newLesson } : {}) };
  return safe(() => {
    storage.setItem(key, JSON.stringify(entry));
    return true;
  }, false);
}

export function removeRecovery(storage: Storage, key: string): void {
  if (!key.startsWith(RECOVERY_PREFIX)) return;
  safe(() => storage.removeItem(key), undefined);
}

function parseEntry(key: string, raw: string | null): RecoveryEntry | null {
  if (raw === null) return null;
  const entry = safe(() => JSON.parse(raw) as RecoveryEntry, null);
  if (!entry || typeof entry !== 'object' || entry.key !== key || typeof entry.instance !== 'string' || typeof entry.userId !== 'number' || !entry.data || typeof entry.data !== 'object') return null;
  const d = entry.data;
  if (typeof d.title !== 'string' || typeof d.objective !== 'string' || typeof d.prerequisites !== 'string' || !Array.isArray(d.mediaIds)) return null;
  return entry;
}

function storageKeys(storage: Storage): string[] {
  return safe(() => Array.from({ length: storage.length }, (_, i) => storage.key(i)), [] as (string | null)[]).filter((k): k is string => !!k?.startsWith(RECOVERY_PREFIX));
}

/** Readable copies of one user matching `keep`, newest first. Unreadable entries are ignored, never thrown. */
function scan(storage: Storage, userId: number, keep: (entry: RecoveryEntry) => boolean): RecoveryEntry[] {
  const prefix = `${RECOVERY_PREFIX}u${userId}:`;
  const out: RecoveryEntry[] = [];
  for (const key of storageKeys(storage)) {
    if (!key.startsWith(prefix)) continue;
    const entry = parseEntry(key, safe(() => storage.getItem(key), null));
    if (entry && entry.userId === userId && keep(entry)) out.push(entry);
  }
  return out.sort((a, b) => b.savedAt.localeCompare(a.savedAt));
}

/**
 * Copies for one user and one revision (any base version), or for the
 * user's not-yet-created lesson or draft when revisionId is null; newest
 * first, without excludeKey (the caller's own live copy).
 */
export function findRecovery(storage: Storage, userId: number, revisionId: number | null, lessonId: number | null = null, excludeKey?: string): RecoveryEntry[] {
  return scan(storage, userId, (e) => e.key !== excludeKey && (revisionId === null ? e.revisionId === null && (e.lessonId ?? null) === lessonId : e.revisionId === revisionId));
}

/** Every copy for a lesson, whatever revision it was written for (an earlier draft, since submitted or discarded, included). */
export function findLessonRecovery(storage: Storage, userId: number, lessonId: number, excludeKey?: string): RecoveryEntry[] {
  return scan(storage, userId, (e) => e.key !== excludeKey && e.lessonId === lessonId);
}

export const findUserRecovery = (storage: Storage, userId: number) => scan(storage, userId, () => true);

/**
 * Deletes copies: every copy of `userId` (sign-out on a shared device), and/or
 * every copy, of any user, saved before `olderThan` or unreadable.
 */
export function purgeRecovery(storage: Storage, { userId, olderThan }: { userId?: number; olderThan?: Date }): void {
  for (const key of storageKeys(storage)) {
    if (userId !== undefined && key.startsWith(`${RECOVERY_PREFIX}u${userId}:`)) {
      removeRecovery(storage, key);
      continue;
    }
    if (olderThan) {
      const entry = parseEntry(key, safe(() => storage.getItem(key), null));
      const saved = entry ? Date.parse(entry.savedAt) : NaN;
      if (!entry || !(saved >= olderThan.getTime())) removeRecovery(storage, key);
    }
  }
}

/** True when the recovered data differs from what the editor currently shows. */
export function differs(a: RecoveryData, b: RecoveryData): boolean {
  return snapshot(a) !== snapshot(b);
}

/** Stable comparison string for dirty tracking. */
export function snapshot(data: RecoveryData): string {
  return JSON.stringify([data.doc, data.title, data.objective, data.prerequisites, data.chapterId, data.mediaIds]);
}

/** A fresh editor instance identifier. */
export const newInstance = () => (globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${Math.random()}`).replace(/[^A-Za-z0-9-]/g, '');
