import type { Schema } from '@tiptap/pm/model';
import type { Revision } from '../api/client';
import { prepareLesson } from '../schema/importer';
import { parseLesson } from '../schema/lesson';
import type { RecoveryData } from '../recovery';

/**
 * A stored revision as editor data. The stored document is checked with the
 * same strict parser as an import; throws LessonError when the editor cannot
 * represent it (it is then shown read-only rather than repaired).
 */
export function revisionToData(schema: Schema, revision: Revision): RecoveryData {
  const doc = parseLesson(JSON.stringify(revision.content));
  const prepared = prepareLesson(schema, doc);
  return {
    doc: prepared.content,
    title: doc.title,
    objective: doc.objective,
    prerequisites: revision.prerequisites,
    chapterId: revision.chapterId,
    mediaIds: [...revision.mediaIds],
  };
}

export const emptyData = (chapterId: number | null = null): RecoveryData => ({
  doc: { type: 'doc', content: [{ type: 'paragraph' }] },
  title: '',
  objective: '',
  prerequisites: '',
  chapterId,
  mediaIds: [],
});
