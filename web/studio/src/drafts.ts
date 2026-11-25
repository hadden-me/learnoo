// Saving to the server: the only persistence path. Every save of an existing
// draft names the version it was based on (expectedVersion); a stale version
// is reported as a conflict and the caller keeps the local edits.
import type { JSONContent } from '@tiptap/core';
import { Api, ApiError, isApiError, type DraftFields, type Revision } from './api/client';
import { editorToLesson } from './schema/adapter';
import { LessonError, embeddedMediaIds, serializeLesson, type Lang } from './schema/lesson';
import type { NewLessonParams, RecoveryData } from './recovery';

export type SaveTarget =
  /** A lesson not yet created: POST /api/lessons. */
  | { kind: 'lesson'; params: NewLessonParams }
  /** An existing lesson without any revision (its only draft was discarded): POST /api/lessons/{id}/drafts. */
  | { kind: 'draft'; lessonId: number }
  /** An existing draft: PUT /api/revisions/{id} with expectedVersion. */
  | { kind: 'revision'; revisionId: number; version: number };

export type SaveOutcome =
  | { kind: 'saved'; revision: Revision }
  | { kind: 'conflict' }
  /** The draft was submitted (for example from another tab): it can no longer change. */
  | { kind: 'not_draft' }
  | { kind: 'expired' }
  | { kind: 'failed'; error: unknown };

export interface EditorState {
  doc: JSONContent;
  title: string;
  objective: string;
  language: Lang;
  prerequisites: string;
  chapterId: number | null;
  mediaIds: number[];
}

/** Converts the editor state into request fields, or the problems the author must fix first. */
export function buildFields(state: EditorState): { fields: DraftFields; errors: [] } | { fields: null; errors: string[] } {
  const result = editorToLesson(state.doc, { title: state.title, language: state.language, objective: state.objective });
  if (result.errors.length > 0) return { fields: null, errors: result.errors };
  let json: string;
  try {
    json = serializeLesson(result.document);
  } catch (error) {
    return { fields: null, errors: [error instanceof LessonError ? error.message : 'Document impossible à exporter.'] };
  }
  if ([...state.prerequisites].length > 2000) return { fields: null, errors: ['Les prérequis dépassent 2 000 caractères.'] };
  return {
    // Every image placed in the text is attached to the revision (the server refuses unattached ones);
    // attached images that are no longer in the text stay attached until the author removes them.
    fields: { content: JSON.parse(json) as unknown, prerequisites: state.prerequisites, chapterId: state.chapterId, mediaIds: [...new Set([...state.mediaIds, ...embeddedMediaIds(result.document.blocks)])] },
    errors: [],
  };
}

/**
 * Runs one write, repeating it once when the database asked for a retry
 * (409 retry: nothing was written). Classifies the result; never throws.
 */
async function classify(run: () => Promise<Revision>): Promise<SaveOutcome> {
  for (let attempt = 0; ; attempt++) {
    try {
      return { kind: 'saved', revision: await run() };
    } catch (error) {
      if (isApiError(error, 'retry') && attempt === 0) continue;
      if (isApiError(error, 'version_conflict')) return { kind: 'conflict' };
      if (isApiError(error, 'not_draft')) return { kind: 'not_draft' };
      if (isApiError(error, 'unauthenticated')) return { kind: 'expired' };
      return { kind: 'failed', error };
    }
  }
}

export function saveToServer(api: Api, target: SaveTarget, fields: DraftFields): Promise<SaveOutcome> {
  switch (target.kind) {
    case 'lesson': {
      const { subjectId, language, translationOf } = target.params;
      return classify(() => api.createLesson({ subjectId, language, translationOf, ...fields }));
    }
    case 'draft':
      return classify(() => api.startDraft(target.lessonId, fields));
    case 'revision':
      return classify(() => api.saveRevision(target.revisionId, target.version, fields));
  }
}

export const submitToServer = (api: Api, revisionId: number, version: number) => classify(() => api.submit(revisionId, version));

/** Discard: true when deleted; otherwise the error (version_conflict when another tab saved first). */
export async function discardOnServer(api: Api, revisionId: number, version: number): Promise<true | ApiError | unknown> {
  try {
    await api.discard(revisionId, version);
    return true;
  } catch (error) {
    return error;
  }
}

/** Text an author can paste elsewhere: the lesson JSON when valid, otherwise everything the editor held. */
export function recoveryText(data: RecoveryData, language: Lang): string {
  const built = buildFields({ ...data, doc: data.doc as JSONContent, language });
  if (built.fields) return JSON.stringify(built.fields.content, null, 2);
  return JSON.stringify({ title: data.title, objective: data.objective, prerequisites: data.prerequisites, editor: data.doc }, null, 2);
}
