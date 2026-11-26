// The lesson editor for a draft (or a lesson about to be created). The
// server is the only persistence: saves are version-checked, a stale save
// opens the conflict panel without touching the local edits, and unsaved
// edits are mirrored to the labelled recovery buffer (recovery.ts) and
// guarded on navigation and page unload.
import { useCallback, useEffect, useId, useMemo, useRef, useState, type ChangeEvent, type ReactNode } from 'react';
import { EditorContent, useEditor, type Editor } from '@tiptap/react';
import type { JSONContent } from '@tiptap/core';
import { NodeSelection } from '@tiptap/pm/state';
import { CatalogueName, entryLabel, labelAttrs, type CatalogueLabel } from '../catalogue';
import { ApiError, type Chapter, type Media, type Revision } from '../api/client';
import { describeError, isHeif } from '../api/messages';
import { isUnloadAllowed, useStudio } from '../context';
import { buildFields, discardOnServer, saveToServer, submitToServer, type SaveTarget } from '../drafts';
import { t } from '../i18n';
import { LANGUAGE_LABELS, formatDateTime, formatTime } from '../labels';
import { disableVideos, loadKatex, typeset } from '../preview';
import { findLessonRecovery, findRecovery, newInstance, recoveryKey, removeRecovery, snapshot, writeRecovery, type NewLessonParams, type RecoveryData, type RecoveryEntry, type RecoveryTarget } from '../recovery';
import { RecoveryList } from '../ui/RecoveryList';
import { addNavigationGuard } from '../router';
import { lessonExtensions } from '../schema/extensions';
import { prepareSource } from '../schema/importer';
import { BLANK, MEDIA_PREFIX, mediaId, type Lang } from '../schema/lesson';
import { describePaste, sanitizePastedHTML } from '../schema/paste';
import { Dialog } from '../ui/Dialog';
import { revisionToData } from './convert';
import { Inspector, Toolbar } from './Toolbar';

const IMAGE_TYPES = ['image/png', 'image/jpeg', 'image/webp'];
const MAX_IMAGE_BYTES = 5 << 20;
const AUTOSAVE_DELAY = 700;

export interface DraftEditorProps {
  target: SaveTarget;
  language: Lang;
  /** The lesson's subject, named in the lesson's language. */
  subject: CatalogueLabel;
  chapters: Chapter[];
  /** What the server holds (or an empty document for a lesson not yet created). */
  initial: RecoveryData;
  /** Revision number and status line shown in the header. */
  revisionLabel: string;
  /** The lesson, once it exists on the server; recovery copies of its other revisions are offered too. */
  lessonId: number | null;
  /** The open revision's number, for labelling recovery copies. */
  revisionNumber?: number | null;
  feedback?: ReactNode;
  newLesson?: NewLessonParams;
  /** A recovery copy to apply right away (the author chose to resume it). */
  restore?: RecoveryEntry;
  /** After the first save created the lesson or draft. The editor stays mounted, so typing during that save is kept. */
  onCreated?: (revision: Revision) => void;
  /** The draft left draft status (submitted here or elsewhere): the page shows it read-only. */
  onSubmitted?: (revision?: Revision) => void;
  onDiscarded?: () => void;
}

type Message = { text: string; error?: boolean } | null;

export function DraftEditor(props: DraftEditorProps) {
  const { api, session, expired, accountChanged } = useStudio();
  const { language, chapters } = props;
  const userId = session.user.id;
  const instance = useMemo(newInstance, []);
  const ids = { title: useId(), objective: useId(), chapter: useId(), prerequisites: useId(), upload: useId(), save: useId() };

  const [target, setTarget] = useState<SaveTarget>(props.target);
  const [lessonId, setLessonId] = useState<number | null>(props.lessonId);
  const [revisionNumber, setRevisionNumber] = useState<number | null>(props.revisionNumber ?? null);
  const [notDraft, setNotDraft] = useState(false);
  const [title, setTitle] = useState(props.initial.title);
  const [objective, setObjective] = useState(props.initial.objective);
  const [prerequisites, setPrerequisites] = useState(props.initial.prerequisites);
  const [chapterId, setChapterId] = useState<number | null>(props.initial.chapterId);
  const [mediaIds, setMediaIds] = useState<number[]>(props.initial.mediaIds);
  const [mediaInfo, setMediaInfo] = useState<Record<number, Media>>({});
  const [tick, setTick] = useState(0);
  const [baseline, setBaseline] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [lastSaved, setLastSaved] = useState<Date | null>(null);
  const [message, setMessage] = useState<Message>(null);
  const [errors, setErrors] = useState<string[]>([]);
  const [pasteNotice, setPasteNotice] = useState<string | null>(null);
  const [conflict, setConflict] = useState(false);
  const [entries, setEntries] = useState<RecoveryEntry[]>([]);
  const [restoredKey, setRestoredKey] = useState<string | null>(null);
  const [dialog, setDialog] = useState<'submit' | 'discard' | null>(null);
  const [uploadError, setUploadError] = useState<string | null>(null);
  const [uploading, setUploading] = useState(false);
  /** The attached image being placed in the text, with the alt text and caption typed so far. */
  const [placing, setPlacing] = useState<{ id: number; alt: string; caption: string } | null>(null);
  const altRef = useRef<HTMLInputElement>(null);
  const [preview, setPreview] = useState<{ html: string; snapshot: string; at: Date } | null>(null);
  const [previewError, setPreviewError] = useState<string | null>(null);
  const [previewing, setPreviewing] = useState(false);
  const pasteRef = useRef(setPasteNotice);
  pasteRef.current = setPasteNotice;
  const conflictRef = useRef<HTMLHeadingElement>(null);
  const previewRef = useRef<HTMLDivElement>(null);
  const importRef = useRef<HTMLInputElement>(null);

  const editor = useEditor({
    extensions: lessonExtensions,
    // The CSP refuses injected <style> elements; the base rules are in styles.css.
    injectCSS: false,
    content: props.initial.doc as JSONContent,
    editorProps: {
      attributes: { class: 'studio-editor', 'aria-label': t('lessons.editor.label'), 'aria-multiline': 'true', role: 'textbox' },
      transformPastedHTML: (html) => {
        const result = sanitizePastedHTML(html);
        pasteRef.current(describePaste(result.report));
        return result.html;
      },
    },
    onUpdate: () => setTick((t) => t + 1),
  });

  const data = useCallback(
    (ed: Editor | null = editor): RecoveryData => ({ doc: ed?.getJSON() ?? props.initial.doc, title, objective, prerequisites, chapterId, mediaIds }),
    [editor, title, objective, prerequisites, chapterId, mediaIds, props.initial.doc],
  );
  // tick makes the snapshot follow editor changes, which are not React state.
  const current = useMemo(() => snapshot(data()), [data, tick]);
  const dirty = baseline !== null && current !== baseline;
  const dirtyRef = useRef(dirty);
  dirtyRef.current = dirty;

  const recoveryTarget = useCallback(
    (t: SaveTarget = target): RecoveryTarget => ({
      userId,
      instance,
      revisionId: t.kind === 'revision' ? t.revisionId : null,
      baseVersion: t.kind === 'revision' ? t.version : null,
      lessonId: t.kind === 'draft' ? t.lessonId : lessonId,
      revisionNumber: t.kind === 'revision' ? revisionNumber : null,
    }),
    [target, userId, instance, lessonId, revisionNumber],
  );

  const refreshEntries = useCallback(
    (t: SaveTarget = target, lesson: number | null = lessonId) => {
      const own = recoveryKey(recoveryTarget(t));
      const found = findRecovery(localStorage, userId, t.kind === 'revision' ? t.revisionId : null, t.kind === 'draft' ? t.lessonId : null, own);
      // Copies written on the lesson's other revisions (a draft since submitted or discarded, another tab).
      const others = lesson === null ? [] : findLessonRecovery(localStorage, userId, lesson, own);
      const byKey = new Map([...found, ...others].map((e) => [e.key, e]));
      setEntries([...byKey.values()].sort((a, b) => b.savedAt.localeCompare(a.savedAt)));
    },
    [target, userId, recoveryTarget, lessonId],
  );

  /** Puts data in the editor and fields; `clean` makes it the new saved baseline. */
  const load = useCallback(
    (d: RecoveryData, clean: boolean) => {
      if (!editor) return;
      editor.commands.setContent(d.doc as JSONContent, { emitUpdate: false });
      setTitle(d.title);
      setObjective(d.objective);
      setPrerequisites(d.prerequisites);
      setChapterId(d.chapterId);
      setMediaIds(d.mediaIds);
      setPasteNotice(null);
      setErrors([]);
      const loaded = snapshot({ ...d, doc: editor.getJSON() });
      if (clean) setBaseline(loaded);
      setTick((t) => t + 1);
    },
    [editor],
  );

  // First load: the baseline is what the server holds; then an optional chosen recovery copy.
  useEffect(() => {
    if (!editor || baseline !== null) return;
    setBaseline(snapshot({ ...props.initial, doc: editor.getJSON() }));
    if (props.restore) {
      load(props.restore.data, false);
      setRestoredKey(props.restore.key);
      setMessage({ text: t('lessons.editor.restored') });
    }
    refreshEntries();
  }, [editor]);

  // Copies identical to the current server state are redundant: drop them silently.
  const offered = useMemo(() => {
    if (baseline === null) return [];
    return entries.filter((e) => e.key !== restoredKey && snapshot(e.data) !== current);
  }, [entries, baseline, current, restoredKey]);
  useEffect(() => {
    if (baseline === null) return;
    for (const e of entries) if (snapshot(e.data) === baseline && e.key !== restoredKey) removeRecovery(localStorage, e.key);
  }, [entries, baseline]);

  // Recovery buffer: unsaved edits are mirrored shortly after each change and when the page is hidden.
  const writeNow = useCallback(
    (reason: RecoveryEntry['reason'] = 'unsaved') => {
      if (!dirtyRef.current && reason === 'unsaved') return;
      writeRecovery(localStorage, recoveryTarget(), data(), reason, target.kind === 'lesson' ? target.params : undefined);
    },
    [recoveryTarget, data, target],
  );
  useEffect(() => {
    if (!dirty) return;
    const timer = setTimeout(() => writeNow(), AUTOSAVE_DELAY);
    return () => clearTimeout(timer);
  }, [dirty, current, writeNow]);

  useEffect(() => {
    const onBeforeUnload = (e: BeforeUnloadEvent) => {
      if (!dirtyRef.current) return;
      writeNow();
      if (isUnloadAllowed()) return;
      e.preventDefault();
      e.returnValue = '';
    };
    const onHide = () => writeNow();
    window.addEventListener('beforeunload', onBeforeUnload);
    window.addEventListener('pagehide', onHide);
    const removeGuard = addNavigationGuard(() => {
      if (dirtyRef.current) writeNow();
      return dirtyRef.current;
    });
    return () => {
      window.removeEventListener('beforeunload', onBeforeUnload);
      window.removeEventListener('pagehide', onHide);
      removeGuard();
    };
  }, [writeNow]);

  useEffect(() => {
    if (conflict) conflictRef.current?.focus();
  }, [conflict]);
  const notDraftRef = useRef<HTMLHeadingElement>(null);
  useEffect(() => {
    if (notDraft) notDraftRef.current?.focus();
  }, [notDraft]);

  /* ---------- saving ---------- */

  const save = useCallback(async (): Promise<Revision | null> => {
    if (!editor || saving) return null;
    if (expired) {
      // Nothing is sent until the author resumes with their own account (banner at the top).
      writeRecovery(localStorage, recoveryTarget(), data());
      setMessage({ text: accountChanged ? t('lessons.editor.blockedAccount') : t('lessons.editor.blockedExpired'), error: true });
      return null;
    }
    const sent = data();
    const built = buildFields({ ...sent, doc: sent.doc as JSONContent, language });
    setErrors(built.errors);
    if (!built.fields) {
      setMessage({ text: t('lessons.editor.invalid'), error: true });
      return null;
    }
    setSaving(true);
    setMessage(null);
    const before = recoveryTarget();
    // Edits typed while the request is in flight stay dirty: the baseline becomes what was sent, not what is shown.
    const outcome = await saveToServer(api, target, built.fields);
    setSaving(false);
    switch (outcome.kind) {
      case 'saved': {
        const revision = outcome.revision;
        removeRecovery(localStorage, recoveryKey(before));
        if (restoredKey) removeRecovery(localStorage, restoredKey);
        setRestoredKey(null);
        const next: SaveTarget = { kind: 'revision', revisionId: revision.id, version: revision.version };
        setTarget(next);
        setLessonId(revision.lessonId);
        setRevisionNumber(revision.number);
        setBaseline(snapshot(sent));
        setLastSaved(new Date());
        setErrors([]);
        refreshEntries(next, revision.lessonId);
        if (props.target.kind !== 'revision' && props.onCreated) props.onCreated(revision);
        return revision;
      }
      case 'conflict':
        writeRecovery(localStorage, before, sent, 'conflict');
        setConflict(true);
        setMessage({ text: t('lessons.editor.saveConflict'), error: true });
        return null;
      case 'not_draft':
        writeRecovery(localStorage, before, sent, 'conflict');
        setNotDraft(true);
        return null;
      case 'expired':
        writeRecovery(localStorage, before, sent);
        setMessage({ text: t('lessons.editor.saveExpired'), error: true });
        return null;
      case 'failed':
        writeRecovery(localStorage, before, sent);
        setMessage({ text: t('lessons.editor.saveFailed', { error: describeError(outcome.error) }), error: true });
        return null;
    }
  }, [editor, saving, expired, accountChanged, data, language, target, recoveryTarget, api, restoredKey, refreshEntries, props]);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.ctrlKey || e.metaKey) && !e.altKey && e.key.toLowerCase() === 's') {
        e.preventDefault();
        void save();
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [save]);

  /* ---------- conflict ---------- */

  const reloadServer = async () => {
    if (target.kind !== 'revision') return;
    // The local edits are in the recovery buffer (written when the conflict was detected); make sure.
    writeRecovery(localStorage, recoveryTarget(), data(), 'conflict');
    try {
      const revision = await api.revision(target.revisionId);
      if (revision.status !== 'draft') {
        setMessage({ text: t('lessons.editor.submittedElsewhere'), error: true });
        setConflict(false);
        props.onSubmitted?.(revision);
        return;
      }
      const serverData = revisionToData(editor!.schema, revision);
      const next: SaveTarget = { kind: 'revision', revisionId: revision.id, version: revision.version };
      setTarget(next);
      load(serverData, true);
      setConflict(false);
      refreshEntries(next);
      setMessage({ text: t('lessons.editor.serverLoaded', { version: revision.version }) });
    } catch (error) {
      setMessage({ text: t('lessons.editor.reloadFailed', { error: describeError(error) }), error: true });
    }
  };

  const [copyFallback, setCopyFallback] = useState<string | null>(null);
  const copyText = async (text: string, copied: string) => {
    try {
      await navigator.clipboard.writeText(text);
      setCopyFallback(null);
      setMessage({ text: copied });
    } catch {
      // Clipboard refused (permissions, insecure context): show the text to copy by hand.
      setCopyFallback(text);
    }
  };
  const localText = (d: RecoveryData) => {
    const built = buildFields({ ...d, doc: d.doc as JSONContent, language });
    if (built.fields) return JSON.stringify(built.fields.content, null, 2);
    return JSON.stringify({ title: d.title, objective: d.objective, prerequisites: d.prerequisites, editor: d.doc }, null, 2);
  };

  /* ---------- submit and discard ---------- */

  const submit = async () => {
    setDialog(null);
    if (expired) {
      void save();
      return;
    }
    let version = target.kind === 'revision' ? target.version : null;
    let revisionId = target.kind === 'revision' ? target.revisionId : null;
    if (dirty || revisionId === null) {
      const saved = await save();
      if (!saved) return;
      version = saved.version;
      revisionId = saved.id;
    }
    setSaving(true);
    const outcome = await submitToServer(api, revisionId!, version!);
    setSaving(false);
    if (outcome.kind === 'saved') {
      removeRecovery(localStorage, recoveryKey(recoveryTarget()));
      props.onSubmitted?.(outcome.revision);
    } else if (outcome.kind === 'conflict') {
      setConflict(true);
      setMessage({ text: t('lessons.editor.submitConflict'), error: true });
    } else if (outcome.kind === 'not_draft') {
      writeRecovery(localStorage, recoveryTarget(), data(), 'conflict');
      setNotDraft(true);
    } else if (outcome.kind === 'expired') {
      setMessage({ text: t('lessons.editor.submitExpired'), error: true });
    } else {
      setMessage({ text: t('lessons.editor.submitFailed', { error: describeError(outcome.error) }), error: true });
    }
  };

  const discard = async () => {
    setDialog(null);
    if (target.kind !== 'revision' || expired) return;
    const result = await discardOnServer(api, target.revisionId, target.version);
    if (result === true) {
      // Only this tab's copy: other tabs' unsaved copies stay offered on the lesson.
      removeRecovery(localStorage, recoveryKey(recoveryTarget()));
      if (restoredKey) removeRecovery(localStorage, restoredKey);
      dirtyRef.current = false;
      props.onDiscarded?.();
    } else if ((result as { code?: string }).code === 'version_conflict') {
      setConflict(true);
      setMessage({ text: t('lessons.editor.discardConflict'), error: true });
    } else {
      setMessage({ text: t('lessons.editor.discardFailed', { error: describeError(result) }), error: true });
    }
  };

  /* ---------- media ---------- */

  const upload = async (event: ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0];
    event.target.value = '';
    if (!file) return;
    setUploadError(null);
    if (expired) {
      setUploadError(describeError(new ApiError(0, accountChanged ? 'account_changed' : 'unauthenticated', '')));
      return;
    }
    if (isHeif(file)) {
      setUploadError(describeError(new ApiError(415, 'unsupported_heic', '')));
      return;
    }
    if (!IMAGE_TYPES.includes(file.type)) {
      setUploadError(t('shell.error.unsupported_media_type'));
      return;
    }
    if (file.size > MAX_IMAGE_BYTES) {
      setUploadError(t('lessons.media.tooHeavy'));
      return;
    }
    setUploading(true);
    try {
      const media = await api.uploadMedia(file);
      setMediaInfo((m) => ({ ...m, [media.id]: media }));
      setMediaIds((ids) => (ids.includes(media.id) ? ids : [...ids, media.id]));
      setMessage({ text: t('lessons.media.uploaded', { name: file.name, width: media.width, height: media.height }) });
    } catch (error) {
      setUploadError(t('lessons.media.uploadRefused', { error: describeError(error) }));
    } finally {
      setUploading(false);
    }
  };

  /** Inserts the attached image at the editor's cursor (or after the selected block). */
  const placeImage = () => {
    if (!editor || !placing || BLANK.test(placing.alt)) return;
    const attrs = { src: `${MEDIA_PREFIX}${placing.id}`, alt: placing.alt.trim(), caption: placing.caption.trim() };
    const { selection } = editor.state;
    // A selected node (formula, image, video) is kept: the image goes after it instead of replacing it.
    if (selection instanceof NodeSelection) editor.chain().focus().insertContentAt(selection.to, { type: 'image', attrs }).run();
    else editor.chain().focus().insertContent({ type: 'image', attrs }).run();
    setMediaIds((ids) => (ids.includes(placing.id) ? ids : [...ids, placing.id]));
    setMessage({ text: t('lessons.media.placedMessage', { id: placing.id }) });
    setPlacing(null);
  };

  // Attached images currently placed in the text (tick follows editor changes).
  const embedded = useMemo(() => {
    const found = new Set<number>();
    editor?.state.doc.descendants((node) => {
      const id = node.type.name === 'image' ? mediaId(String(node.attrs.src)) : null;
      if (id !== null) found.add(id);
    });
    return found;
  }, [editor, tick]);

  /* ---------- preview ---------- */

  const runPreview = async () => {
    const d = data();
    const built = buildFields({ ...d, doc: d.doc as JSONContent, language });
    setErrors(built.errors);
    if (!built.fields) {
      setPreviewError(t('lessons.preview.invalid'));
      return;
    }
    setPreviewing(true);
    try {
      const html = await api.preview(built.fields.content);
      setPreview({ html, snapshot: snapshot(d), at: new Date() });
      setPreviewError(null);
    } catch (error) {
      setPreviewError(t('lessons.preview.failed', { error: describeError(error) }));
    } finally {
      setPreviewing(false);
    }
  };
  useEffect(() => {
    const root = previewRef.current;
    if (!root || !preview) return;
    // Server-rendered, allowlisted lesson markup (POST /api/preview), as on the reader page.
    root.innerHTML = preview.html;
    disableVideos(root);
    let cancelled = false;
    void loadKatex().then((katex) => {
      if (!cancelled) root.dataset.mathErrors = String(typeset(root, katex));
    });
    return () => {
      cancelled = true;
    };
  }, [preview]);

  /* ---------- import / export ---------- */

  const onImport = async (event: ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0];
    event.target.value = '';
    if (!file || !editor) return;
    try {
      const prepared = prepareSource(editor.schema, await file.text());
      if (prepared.doc.language !== language) throw new Error(t('lessons.editor.importLanguage', { document: LANGUAGE_LABELS[prepared.doc.language], lesson: LANGUAGE_LABELS[language] }));
      load({ ...data(), doc: prepared.content, title: prepared.doc.title, objective: prepared.doc.objective }, false);
      setMessage({ text: t('lessons.editor.imported', { name: file.name }) });
    } catch (error) {
      setMessage({ text: t('lessons.editor.importRefused', { error: (error as Error).message }), error: true });
    }
  };
  const onExport = () => {
    const d = data();
    const built = buildFields({ ...d, doc: d.doc as JSONContent, language });
    setErrors(built.errors);
    if (!built.fields) return;
    const link = document.createElement('a');
    link.href = URL.createObjectURL(new Blob([JSON.stringify(built.fields.content, null, 2) + '\n'], { type: 'application/json' }));
    link.download = 'lecon.json';
    link.click();
    URL.revokeObjectURL(link.href);
  };

  /* ---------- rendering ---------- */

  const dir = language === 'ar' ? 'rtl' : 'ltr';
  const version = target.kind === 'revision' ? target.version : null;
  const saveState = saving
    ? t('lessons.save.saving')
    : expired && dirty
      ? t('lessons.save.expiredDirty')
      : dirty
        ? t('lessons.save.dirty')
        : lastSaved
          ? t('lessons.save.savedAt', { time: formatTime(lastSaved), version: String(version) })
          : version !== null
            ? t('lessons.save.upToDate', { version })
            : t('lessons.save.never');
  const previewStale = preview !== null && preview.snapshot !== current;

  return (
    <div className="editor-page">
      <div className="editor-bar">
        <p className="revision-label">{props.revisionLabel}</p>
        <p id={ids.save} className={dirty ? 'save-state dirty' : 'save-state'} role="status" aria-live="polite">
          {saveState}
        </p>
        <div className="actions">
          <button type="button" className="primary" onClick={() => void save()} disabled={saving} aria-describedby={ids.save}>
            {t('lessons.save.save')}
          </button>
          <button type="button" onClick={() => void runPreview()} disabled={previewing}>
            {t('lessons.save.preview')}
          </button>
          <button type="button" onClick={() => setDialog('submit')} disabled={saving || target.kind !== 'revision'}>
            {t('lessons.save.submit')}
          </button>
        </div>
      </div>

      {message && (
        <p className={message.error ? 'banner error' : 'banner success'} role={message.error ? 'alert' : 'status'}>
          {message.text}
        </p>
      )}
      {errors.length > 0 && (
        <ul className="errors" aria-label={t('lessons.editor.problems')}>
          {errors.map((e) => (
            <li key={e}>{e}</li>
          ))}
        </ul>
      )}

      {conflict && (
        <section className="conflict" aria-labelledby="conflict-heading">
          <h2 id="conflict-heading" tabIndex={-1} ref={conflictRef}>
            {t('lessons.conflict.title')}
          </h2>
          <p>
            {t('lessons.conflict.text')} <strong>{t('lessons.conflict.notLost')}</strong> {t('lessons.conflict.kept')}
          </p>
          <div className="actions">
            <button type="button" className="primary" onClick={() => void reloadServer()}>
              {t('lessons.conflict.loadServer')}
            </button>
            <button type="button" onClick={() => void copyText(localText(data()), t('lessons.editor.localCopied'))}>
              {t('lessons.conflict.copyLocal')}
            </button>
            <button type="button" onClick={() => setConflict(false)}>
              {t('lessons.conflict.continue')}
            </button>
          </div>
          <p className="hint">{t('lessons.conflict.hint')}</p>
        </section>
      )}
      {copyFallback !== null && (
        <section className="copy-fallback" aria-labelledby="copy-heading">
          <h2 id="copy-heading">{t('lessons.conflict.copyByHand')}</h2>
          <p className="hint">{t('lessons.clipboardRefused')}</p>
          <textarea readOnly dir="ltr" rows={8} value={copyFallback} aria-labelledby="copy-heading" onFocus={(e) => e.currentTarget.select()} />
          <button type="button" onClick={() => setCopyFallback(null)}>
            {t('lessons.close')}
          </button>
        </section>
      )}

      {notDraft && (
        <section className="conflict" aria-labelledby="notdraft-heading">
          <h2 id="notdraft-heading" tabIndex={-1} ref={notDraftRef}>
            {t('lessons.notDraft.title')}
          </h2>
          <p>
            {t('lessons.notDraft.text')} <strong>{t('lessons.conflict.notLost')}</strong> {t('lessons.notDraft.kept')}
          </p>
          <div className="actions">
            <button type="button" onClick={() => void copyText(localText(data()), t('lessons.editor.localCopied'))}>
              {t('lessons.conflict.copyLocal')}
            </button>
            <button
              type="button"
              className="primary"
              onClick={() => {
                writeRecovery(localStorage, recoveryTarget(), data(), 'conflict');
                dirtyRef.current = false;
                props.onSubmitted?.();
              }}
            >
              {t('lessons.notDraft.view')}
            </button>
          </div>
        </section>
      )}

      <RecoveryList
        entries={offered}
        currentRevisionId={target.kind === 'revision' ? target.revisionId : null}
        language={language}
        onRestore={(e) => {
          load(e.data, false);
          setRestoredKey(e.key);
          setMessage({ text: t('lessons.editor.restored') });
        }}
        onChanged={() => refreshEntries()}
      />

      {props.feedback}

      <div className="editor-layout">
        <section className="panel" aria-labelledby="edit-heading">
          <h2 id="edit-heading">{t('lessons.editor.content')}</h2>
          <div className="fields">
            <label htmlFor={ids.title}>{t('lessons.editor.title')}</label>
            <input id={ids.title} value={title} maxLength={240} lang={language} dir={dir} onChange={(e) => setTitle(e.target.value)} />
            <label htmlFor={ids.objective}>{t('lessons.editor.objective')}</label>
            <textarea id={ids.objective} value={objective} rows={2} lang={language} dir={dir} onChange={(e) => setObjective(e.target.value)} />
          </div>
          {editor && <Toolbar editor={editor} />}
          <div className="editor-frame" lang={language} dir={dir}>
            <EditorContent editor={editor} />
          </div>
          {pasteNotice && (
            <p className="paste-notice" role="status">
              {pasteNotice}
            </p>
          )}
          {editor && <Inspector editor={editor} />}

          <fieldset className="classification">
            <legend>{t('lessons.editor.classification')}</legend>
            <p className="hint">
              {t('lessons.editor.subject')}{' '}
              <strong>
                <CatalogueName label={props.subject} />
              </strong>
              {' · '}
              {t('lessons.editor.language')} <strong>{LANGUAGE_LABELS[language]}</strong> {t('lessons.editor.fixed')}
            </p>
            <label htmlFor={ids.chapter}>{t('lessons.editor.chapter')}</label>
            <select id={ids.chapter} value={chapterId ?? ''} onChange={(e) => setChapterId(e.target.value ? Number(e.target.value) : null)}>
              <option value="">{t('lessons.noChapter')}</option>
              {chapters.filter((c) => !c.retiredAt || c.id === chapterId).map((c) => {
                const label = entryLabel(c, language);
                return (
                  <option key={c.id} value={c.id} {...labelAttrs(label)}>
                    {label.text}
                  </option>
                );
              })}
            </select>
            <label htmlFor={ids.prerequisites}>{t('lessons.editor.prerequisites')}</label>
            <textarea id={ids.prerequisites} value={prerequisites} rows={2} lang={language} dir={dir} onChange={(e) => setPrerequisites(e.target.value)} />
          </fieldset>

          <section className="media" aria-labelledby="media-heading">
            <h3 id="media-heading">{t('lessons.media.attached')}</h3>
            <p className="hint">
              {t('lessons.media.hint')}
            </p>
            {mediaIds.length > 0 ? (
              <ul className="media-list">
                {mediaIds.map((id) => (
                  <li key={id}>
                    <img src={`/api/media/${id}`} alt={t('lessons.media.alt', { id })} width={mediaInfo[id]?.width} height={mediaInfo[id]?.height} />
                    <span>
                      {t('lessons.media.name', { id })}
                      {mediaInfo[id] ? ` · ${mediaInfo[id].width} × ${mediaInfo[id].height}` : ''}
                      {` · ${embedded.has(id) ? t('lessons.media.placed') : t('lessons.media.notPlaced')}`}
                    </span>
                    <button type="button" onClick={() => setPlacing({ id, alt: '', caption: '' })}>
                      {t('lessons.media.place')}
                    </button>
                    <button
                      type="button"
                      disabled={embedded.has(id)}
                      title={embedded.has(id) ? t('lessons.media.removeFirst') : undefined}
                      onClick={() => setMediaIds((ids) => ids.filter((x) => x !== id))}
                    >
                      {t('lessons.media.detach')}
                    </button>
                  </li>
                ))}
              </ul>
            ) : (
              <p>{t('lessons.media.none')}</p>
            )}
            <label htmlFor={ids.upload}>{t('lessons.media.upload')}</label>
            <input
              id={ids.upload}
              type="file"
              accept={IMAGE_TYPES.join(',')}
              onChange={(e) => void upload(e)}
              disabled={uploading}
              aria-invalid={uploadError ? true : undefined}
              aria-describedby={uploadError ? `${ids.upload}-error` : undefined}
            />
            {uploading && <p role="status">{t('lessons.media.uploading')}</p>}
            {uploadError && (
              <p id={`${ids.upload}-error`} className="banner error" role="alert">
                {uploadError}
              </p>
            )}
          </section>

          <details className="tools">
            <summary>{t('lessons.editor.jsonTools')}</summary>
            <p className="hint">{t('lessons.editor.jsonHint')}</p>
            <div className="actions">
              <button type="button" onClick={onExport}>
                {t('lessons.editor.exportJson')}
              </button>
              <button type="button" onClick={() => importRef.current?.click()}>
                {t('lessons.editor.importJson')}
              </button>
              <input ref={importRef} type="file" accept="application/json,.json" hidden onChange={(e) => void onImport(e)} />
            </div>
          </details>

          {target.kind === 'revision' && (
            <div className="danger-zone">
              <button type="button" className="danger" onClick={() => setDialog('discard')} disabled={saving}>
                {t('lessons.editor.deleteDraft')}
              </button>
            </div>
          )}
        </section>

        <section className="panel" aria-labelledby="preview-heading">
          <h2 id="preview-heading">{t('lessons.preview.title')}</h2>
          <p className="hint">{t('lessons.preview.hint')}</p>
          <button type="button" onClick={() => void runPreview()} disabled={previewing}>
            {preview ? t('lessons.preview.refresh') : t('lessons.save.preview')}
          </button>
          <p role="status" className={previewError ? 'error' : 'hint'}>
            {previewError ??
              (previewing ? t('lessons.preview.running') : preview ? (previewStale ? t('lessons.preview.stale', { time: formatTime(preview.at) }) : t('lessons.preview.fresh', { time: formatTime(preview.at) })) : '')}
          </p>
          <div ref={previewRef} className="lesson-preview" data-testid="lesson-preview" hidden={!preview} />
        </section>
      </div>

      {placing && (
        <Dialog
          title={t('lessons.media.placeTitle', { id: placing.id })}
          onCancel={() => setPlacing(null)}
          initialFocus={altRef}
          actions={[{ label: t('lessons.media.placeAction'), kind: 'primary', disabled: BLANK.test(placing.alt), onClick: placeImage }]}
        >
          <img className="placing-preview" src={`/api/media/${placing.id}`} alt="" />
          <label>
            {t('lessons.altRequired')}
            <input
              ref={altRef}
              dir="auto"
              lang={language}
              maxLength={1000}
              required
              aria-describedby={`${ids.upload}-alt-hint`}
              value={placing.alt}
              onChange={(e) => setPlacing({ ...placing, alt: e.target.value })}
            />
          </label>
          <p id={`${ids.upload}-alt-hint`} className="hint">
            {t('lessons.media.altHint')}
          </p>
          <label>
            {t('lessons.media.captionVisible')}
            <input dir="auto" lang={language} maxLength={1000} value={placing.caption} onChange={(e) => setPlacing({ ...placing, caption: e.target.value })} />
          </label>
          <p className="hint">{t('lessons.media.atCursor')}</p>
        </Dialog>
      )}
      {dialog === 'submit' && (
        <Dialog title={t('lessons.submit.title')} onCancel={() => setDialog(null)} actions={[{ label: t('lessons.submit.action'), kind: 'primary', onClick: () => void submit() }]}>
          <p>{t('lessons.submit.text')}</p>
          {dirty && <p>{t('lessons.submit.saveFirst')}</p>}
        </Dialog>
      )}
      {dialog === 'discard' && (
        <Dialog title={t('lessons.discard.title')} onCancel={() => setDialog(null)} cancelLabel={t('lessons.discard.keep')} actions={[{ label: t('lessons.discard.action'), kind: 'danger', onClick: () => void discard() }]}>
          <p>{t('lessons.discard.text')}</p>
        </Dialog>
      )}
    </div>
  );
}

/** Reviewer feedback to act on, from the revision that carries it. */
export function Feedback({ revision }: { revision: Revision }) {
  if (!revision.review?.comment) return null;
  return (
    <section className="feedback" aria-labelledby="feedback-heading">
      <h2 id="feedback-heading">{t('lessons.feedback.title')}</h2>
      <p className="hint">
        {t('lessons.feedback.meta', { number: revision.number, date: formatDateTime(revision.review.decidedAt) })}
      </p>
      <blockquote dir="auto">{revision.review.comment}</blockquote>
    </section>
  );
}
