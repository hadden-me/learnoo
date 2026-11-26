// Formatting toolbar and selection inspector for the lesson editor. Buttons
// map one-to-one to the schema v1 node types (schema/extensions.ts).
import { useEditorState, type Editor } from '@tiptap/react';
import { NodeSelection } from '@tiptap/pm/state';
import { t } from '../i18n';
import { ALLOWED_IMAGES, YOUTUBE_ID, mediaId } from '../schema/lesson';

export function Toolbar({ editor }: { editor: Editor }) {
  const state = useEditorState({
    editor,
    selector: ({ editor: e }) => ({
      bold: e.isActive('bold'),
      heading: e.isActive('heading'),
      list: e.isActive('bulletList'),
      arText: e.isActive('lang', { lang: 'ar' }),
      frText: e.isActive('lang', { lang: 'fr' }),
      canSolution: e.can().wrapIn('solution'),
      canUndo: e.can().undo(),
      canRedo: e.can().redo(),
    }),
  });

  const insertAtom = (content: { type: string; attrs: Record<string, string> }) => {
    const from = editor.state.selection.from;
    editor.chain().focus().insertContent(content).run();
    // Select the new node so its properties can be edited right away.
    const pos = content.type === 'inlineMath' ? from : findAtomBefore(editor, editor.state.selection.from, content.type);
    if (pos !== null) editor.commands.setNodeSelection(pos);
  };

  const inlineMath = () => {
    const { from, to, empty } = editor.state.selection;
    const latex = empty ? '' : editor.state.doc.textBetween(from, to);
    editor.chain().focus().deleteSelection().run();
    insertAtom({ type: 'inlineMath', attrs: { latex } });
  };

  return (
    <div className="toolbar" role="toolbar" aria-label={t('lessons.toolbar.label')}>
      <div role="group" aria-label={t('lessons.toolbar.text')}>
        <B label={t('lessons.node.heading')} pressed={state.heading} onClick={() => editor.chain().focus().toggleNode('heading', 'paragraph').run()} />
        <B label={t('lessons.toolbar.bold')} pressed={state.bold} onClick={() => editor.chain().focus().toggleBold().run()} />
        <B label={t('lessons.node.list')} pressed={state.list} onClick={() => editor.chain().focus().toggleBulletList().run()} />
      </div>
      <div role="group" aria-label={t('lessons.toolbar.math')}>
        <B label={t('lessons.node.inlineMath')} onClick={inlineMath} />
        <B label={t('lessons.node.displayMath')} onClick={() => insertAtom({ type: 'displayMath', attrs: { latex: '' } })} />
      </div>
      <div role="group" aria-label={t('lessons.toolbar.blocks')}>
        <B label={t('lessons.node.callout')} onClick={() => editor.chain().focus().wrapIn('callout').run()} />
        <B label={t('lessons.node.exercise')} onClick={() => editor.chain().focus().wrapIn('exercise').run()} />
        <B label={t('lessons.node.solution')} disabled={!state.canSolution} onClick={() => editor.chain().focus().wrapIn('solution').run()} />
        <B label={t('lessons.toolbar.demoImage')} onClick={() => insertAtom({ type: 'image', attrs: { src: ALLOWED_IMAGES[0], alt: '' } })} />
        <B label={t('lessons.node.youtube')} onClick={() => insertAtom({ type: 'youtube', attrs: { title: '', videoId: '' } })} />
      </div>
      <div role="group" aria-label={t('lessons.toolbar.selectionLanguage')}>
        <B label={t('lessons.toolbar.arabicText')} pressed={state.arText} onClick={() => editor.chain().focus().setMark('lang', { lang: 'ar', dir: 'rtl' }).run()} />
        <B label={t('lessons.toolbar.frenchText')} pressed={state.frText} onClick={() => editor.chain().focus().setMark('lang', { lang: 'fr', dir: 'ltr' }).run()} />
        <B label={t('lessons.toolbar.documentLanguage')} onClick={() => editor.chain().focus().unsetMark('lang').run()} />
      </div>
      <div role="group" aria-label={t('lessons.toolbar.history')}>
        <B label={t('lessons.toolbar.undo')} disabled={!state.canUndo} onClick={() => editor.chain().focus().undo().run()} />
        <B label={t('lessons.toolbar.redo')} disabled={!state.canRedo} onClick={() => editor.chain().focus().redo().run()} />
      </div>
    </div>
  );
}

function B({ label, onClick, pressed, disabled }: { label: string; onClick: () => void; pressed?: boolean; disabled?: boolean }) {
  // mousedown is cancelled so the editor keeps its selection; keyboard users activate with Enter/Space.
  return (
    <button type="button" aria-pressed={pressed} disabled={disabled} onMouseDown={(e) => e.preventDefault()} onClick={onClick}>
      {label}
    </button>
  );
}

function findAtomBefore(editor: Editor, pos: number, type: string): number | null {
  let found: number | null = null;
  editor.state.doc.nodesBetween(Math.max(0, pos - 2), Math.min(editor.state.doc.content.size, pos + 2), (node, at) => {
    if (node.type.name === type) found = at;
  });
  return found;
}

/** Properties of the selected formula, media or container, and of the current block. */
export function Inspector({ editor }: { editor: Editor }) {
  const ctx = useEditorState({
    editor,
    selector: ({ editor: e }) => {
      const { selection } = e.state;
      const atom = selection instanceof NodeSelection ? { pos: selection.from, type: selection.node.type.name, attrs: selection.node.attrs } : null;
      let container: { pos: number; type: string; title: string } | null = null;
      let block: { pos: number; lang: string; dir: string } | null = null;
      const $from = selection.$from;
      for (let d = $from.depth; d > 0; d--) {
        const node = $from.node(d);
        if (!block && (node.type.name === 'paragraph' || node.type.name === 'heading')) block = { pos: $from.before(d), lang: node.attrs.lang ?? '', dir: node.attrs.dir ?? '' };
        if (!container && (node.type.name === 'callout' || node.type.name === 'exercise')) container = { pos: $from.before(d), type: node.type.name, title: node.attrs.title };
      }
      return { atom, container, block };
    },
  });
  if (!ctx) return null;
  const set = (pos: number, attr: string, value: string | null) => editor.view.dispatch(editor.state.tr.setNodeAttribute(pos, attr, value));
  const { atom, container, block } = ctx;
  const videoId = atom?.type === 'youtube' ? String(atom.attrs.videoId) : '';

  return (
    <fieldset className="inspector">
      <legend>{t('lessons.inspector.legend')}</legend>
      {atom && (atom.type === 'inlineMath' || atom.type === 'displayMath') && (
        <label>
          {atom.type === 'inlineMath' ? t('lessons.inspector.inlineTex') : t('lessons.inspector.displayTex')}
          <textarea className="tex" dir="ltr" rows={2} maxLength={2000} value={String(atom.attrs.latex)} onChange={(e) => set(atom.pos, 'latex', e.target.value)} />
        </label>
      )}
      {atom?.type === 'image' && (
        <>
          {mediaId(String(atom.attrs.src)) === null ? (
            <label>
              {t('lessons.inspector.localResource')}
              <select value={String(atom.attrs.src)} onChange={(e) => set(atom.pos, 'src', e.target.value)}>
                {ALLOWED_IMAGES.map((src) => (
                  <option key={src}>{src}</option>
                ))}
              </select>
            </label>
          ) : (
            <p className="hint">{t('lessons.inspector.uploaded', { id: String(mediaId(String(atom.attrs.src))) })}</p>
          )}
          <label>
            {t('lessons.altRequired')}
            <input dir="auto" maxLength={1000} value={String(atom.attrs.alt)} aria-invalid={String(atom.attrs.alt).trim() === ''} onChange={(e) => set(atom.pos, 'alt', e.target.value)} />
          </label>
          <label>
            {t('lessons.inspector.caption')}
            <input dir="auto" maxLength={1000} value={String(atom.attrs.caption ?? '')} onChange={(e) => set(atom.pos, 'caption', e.target.value)} />
          </label>
        </>
      )}
      {atom?.type === 'youtube' && (
        <>
          <label>
            {t('lessons.inspector.videoTitle')}
            <input dir="auto" maxLength={240} value={String(atom.attrs.title)} onChange={(e) => set(atom.pos, 'title', e.target.value)} />
          </label>
          <label>
            {t('lessons.inspector.videoId')}
            <input dir="ltr" maxLength={11} value={videoId} aria-invalid={videoId !== '' && !YOUTUBE_ID.test(videoId)} onChange={(e) => set(atom.pos, 'videoId', e.target.value.trim())} />
          </label>
        </>
      )}
      {container && (
        <label>
          {container.type === 'callout' ? t('lessons.inspector.calloutTitle') : t('lessons.inspector.exerciseTitle')}
          <input dir="auto" maxLength={240} value={container.title} onChange={(e) => set(container.pos, 'title', e.target.value)} />
        </label>
      )}
      {block && (
        <div className="row">
          <label>
            {t('lessons.inspector.paragraphLanguage')}
            <select
              value={block.lang}
              onChange={(e) => {
                const lang = e.target.value || null;
                let tr = editor.state.tr.setNodeAttribute(block.pos, 'lang', lang);
                // Choosing a language also sets its usual direction; it stays editable below.
                if (lang) tr = tr.setNodeAttribute(block.pos, 'dir', lang === 'ar' ? 'rtl' : 'ltr');
                editor.view.dispatch(tr);
              }}
            >
              <option value="">{t('lessons.inspector.documentLanguage')}</option>
              <option value="fr">{t('shell.language.fr')}</option>
              <option value="ar">{t('shell.language.ar')}</option>
            </select>
          </label>
          <label>
            {t('lessons.inspector.direction')}
            <select value={block.dir} onChange={(e) => set(block.pos, 'dir', e.target.value || null)}>
              <option value="">{t('lessons.inspector.inherited')}</option>
              <option value="ltr">{t('lessons.inspector.ltr')}</option>
              <option value="rtl">{t('lessons.inspector.rtl')}</option>
              <option value="auto">{t('lessons.inspector.auto')}</option>
            </select>
          </label>
        </div>
      )}
      {!atom && !container && !block && <p className="hint">{t('lessons.inspector.empty')}</p>}
    </fieldset>
  );
}
