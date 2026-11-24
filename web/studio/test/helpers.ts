import { Editor } from '@tiptap/core';
import { lessonExtensions } from '../src/schema/extensions';
import { editorToLesson, lessonToEditorChecked } from '../src/schema/adapter';
import { parseLesson, type LessonDocument } from '../src/schema/lesson';

export function createEditor(): Editor {
  return new Editor({ element: document.createElement('div'), extensions: lessonExtensions });
}

export function loadInto(editor: Editor, doc: LessonDocument) {
  editor.commands.setContent(lessonToEditorChecked(editor.schema, doc.blocks), { emitUpdate: false });
}

export function exportFrom(editor: Editor, meta: Pick<LessonDocument, 'title' | 'language' | 'objective'>) {
  return editorToLesson(editor.getJSON(), meta);
}

/** Types text the way the browser does, so input rules ($…$, **…**) run. */
export function type(editor: Editor, text: string) {
  for (const char of text) {
    const { from, to } = editor.state.selection;
    const handled = editor.view.someProp('handleTextInput', (f) => f(editor.view, from, to, char, () => editor.state.tr.insertText(char, from, to)));
    if (!handled) editor.view.dispatch(editor.state.tr.insertText(char, from, to));
  }
}

export const roundTrip = (source: string) => {
  const doc = parseLesson(source);
  const editor = createEditor();
  loadInto(editor, doc);
  const result = exportFrom(editor, doc);
  editor.destroy();
  return { doc, result };
};
