// The lesson editor for a draft (or a lesson about to be created). The
// server is the only persistence: saves are version-checked, a stale save
// opens the conflict panel without touching the local edits, and unsaved
// edits are mirrored to the labelled recovery buffer (recovery.ts) and
// guarded on navigation and page unload.
import { useCallback, useEffect, useId, useMemo, useRef, useState, type ChangeEvent, type ReactNode } from 'react';
import { EditorContent, useEditor, type Editor } from '@tiptap/react';
import type { JSONContent } from '@tiptap/core';
import { NodeSelection } from '@tiptap/pm/state';
import { ApiError, type Chapter, type Media, type Revision } from '../api/client';
import { describeError } from '../api/messages';
import { isUnloadAllowed, useStudio } from '../context';
import { buildFields, discardOnServer, saveToServer, submitToServer, type SaveTarget } from '../drafts';
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
  subjectCode: string;
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
      attributes: { class: 'studio-editor', 'aria-label': 'Éditeur du contenu de la leçon', 'aria-multiline': 'true', role: 'textbox' },
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
      setMessage({ text: 'Copie de secours restaurée dans l’éditeur. Enregistrez pour la conserver sur le serveur.' });
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
      setMessage({ text: `Rien n’a été envoyé : ${accountChanged ? 'un autre compte est connecté dans ce navigateur' : 'la session a expiré'}. Vos modifications restent ici et dans la copie de secours ; reconnectez-vous puis choisissez « Reprendre ici ».`, error: true });
      return null;
    }
    const sent = data();
    const built = buildFields({ ...sent, doc: sent.doc as JSONContent, language });
    setErrors(built.errors);
    if (!built.fields) {
      setMessage({ text: 'Le document ne respecte pas le format de leçon ; rien n’a été enregistré. Corrigez les points ci-dessous.', error: true });
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
        setMessage({ text: 'Enregistrement refusé : conflit de version. Vos modifications sont conservées.', error: true });
        return null;
      case 'not_draft':
        writeRecovery(localStorage, before, sent, 'conflict');
        setNotDraft(true);
        return null;
      case 'expired':
        writeRecovery(localStorage, before, sent);
        setMessage({ text: 'Session expirée : rien n’a été enregistré. Vos modifications restent ici et dans la copie de secours. Reconnectez-vous puis enregistrez à nouveau.', error: true });
        return null;
      case 'failed':
        writeRecovery(localStorage, before, sent);
        setMessage({ text: `Enregistrement impossible. ${describeError(outcome.error)}`, error: true });
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
        setMessage({ text: 'Cette révision a été soumise depuis un autre onglet ; elle ne peut plus être modifiée ici. Votre copie locale est conservée sur cet appareil.', error: true });
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
      setMessage({ text: `Version ${revision.version} du serveur chargée. Votre copie locale est conservée ci-dessous : vous pouvez la copier ou la restaurer.` });
    } catch (error) {
      setMessage({ text: `Rechargement impossible. ${describeError(error)} Vos modifications sont toujours dans l’éditeur.`, error: true });
    }
  };

  const [copyFallback, setCopyFallback] = useState<string | null>(null);
  const copyText = async (text: string, what: string) => {
    try {
      await navigator.clipboard.writeText(text);
      setCopyFallback(null);
      setMessage({ text: `${what} copié dans le presse-papiers.` });
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
      setMessage({ text: 'Soumission refusée : le brouillon a changé ailleurs depuis son ouverture.', error: true });
    } else if (outcome.kind === 'not_draft') {
      writeRecovery(localStorage, recoveryTarget(), data(), 'conflict');
      setNotDraft(true);
    } else if (outcome.kind === 'expired') {
      setMessage({ text: 'Session expirée : la leçon n’a pas été soumise. Reconnectez-vous puis réessayez.', error: true });
    } else {
      setMessage({ text: `Soumission impossible. ${describeError(outcome.error)}`, error: true });
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
      setMessage({ text: 'Suppression refusée : le brouillon a changé ailleurs. Rechargez la version du serveur avant de le supprimer.', error: true });
    } else {
      setMessage({ text: `Suppression impossible. ${describeError(result)}`, error: true });
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
    if (!IMAGE_TYPES.includes(file.type)) {
      setUploadError('Format refusé : seules les images PNG, JPEG et WebP sont acceptées (pas de SVG ni de GIF).');
      return;
    }
    if (file.size > MAX_IMAGE_BYTES) {
      setUploadError('Image trop lourde : 5 Mio au maximum. Réduisez-la ou ré-exportez-la.');
      return;
    }
    setUploading(true);
    try {
      const media = await api.uploadMedia(file);
      setMediaInfo((m) => ({ ...m, [media.id]: media }));
      setMediaIds((ids) => (ids.includes(media.id) ? ids : [...ids, media.id]));
      setMessage({ text: `Image « ${file.name} » téléversée (${media.width} × ${media.height}). Placez le curseur dans le texte puis choisissez « Placer dans le texte » ; enregistrez ensuite le brouillon.` });
    } catch (error) {
      setUploadError(`Téléversement refusé. ${describeError(error)}`);
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
    setMessage({ text: `Image n° ${placing.id} placée dans le texte.` });
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
      setPreviewError('Aperçu impossible : le document ne respecte pas le format de leçon (voir les points à corriger).');
      return;
    }
    setPreviewing(true);
    try {
      const html = await api.preview(built.fields.content);
      setPreview({ html, snapshot: snapshot(d), at: new Date() });
      setPreviewError(null);
    } catch (error) {
      setPreviewError(`Aperçu impossible. ${describeError(error)} Le dernier aperçu est conservé.`);
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
      if (prepared.doc.language !== language) throw new Error(`le document est en ${LANGUAGE_LABELS[prepared.doc.language]}, la leçon en ${LANGUAGE_LABELS[language]}.`);
      load({ ...data(), doc: prepared.content, title: prepared.doc.title, objective: prepared.doc.objective }, false);
      setMessage({ text: `Fichier « ${file.name} » importé dans l’éditeur. Enregistrez pour le conserver.` });
    } catch (error) {
      setMessage({ text: `Import refusé, document actuel conservé : ${(error as Error).message}`, error: true });
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
    ? 'Enregistrement en cours…'
    : expired && dirty
      ? 'Session expirée : modifications non enregistrées, conservées sur cet appareil.'
      : dirty
        ? 'Modifications non enregistrées sur le serveur.'
        : lastSaved
          ? `Enregistré sur le serveur à ${formatTime(lastSaved)} (version ${version}).`
          : version !== null
            ? `À jour avec le serveur (version ${version}).`
            : 'Pas encore enregistré sur le serveur.';
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
            Enregistrer
          </button>
          <button type="button" onClick={() => void runPreview()} disabled={previewing}>
            Prévisualiser
          </button>
          <button type="button" onClick={() => setDialog('submit')} disabled={saving || target.kind !== 'revision'}>
            Soumettre pour relecture
          </button>
        </div>
      </div>

      {message && (
        <p className={message.error ? 'banner error' : 'banner success'} role={message.error ? 'alert' : 'status'}>
          {message.text}
        </p>
      )}
      {errors.length > 0 && (
        <ul className="errors" aria-label="Problèmes à corriger">
          {errors.map((e) => (
            <li key={e}>{e}</li>
          ))}
        </ul>
      )}

      {conflict && (
        <section className="conflict" aria-labelledby="conflict-heading">
          <h2 id="conflict-heading" tabIndex={-1} ref={conflictRef}>
            Conflit : ce brouillon a été enregistré ailleurs
          </h2>
          <p>
            Une autre fenêtre ou un autre appareil a enregistré ce brouillon après son ouverture ici. Le serveur a refusé d’écraser cette version. <strong>Vos modifications ne sont pas perdues</strong> : elles sont toujours dans l’éditeur, et une copie de secours a été faite sur cet appareil.
          </p>
          <div className="actions">
            <button type="button" className="primary" onClick={() => void reloadServer()}>
              Charger la version du serveur
            </button>
            <button type="button" onClick={() => void copyText(localText(data()), 'Votre contenu local')}>
              Copier mon contenu local
            </button>
            <button type="button" onClick={() => setConflict(false)}>
              Continuer à éditer
            </button>
          </div>
          <p className="hint">Charger la version du serveur remplace le contenu de l’éditeur ; votre version locale reste proposée ci-dessous comme copie de secours, à copier ou à restaurer.</p>
        </section>
      )}
      {copyFallback !== null && (
        <section className="copy-fallback" aria-labelledby="copy-heading">
          <h2 id="copy-heading">Copier à la main</h2>
          <p className="hint">Le navigateur refuse l’accès au presse-papiers. Sélectionnez le texte ci-dessous puis copiez-le.</p>
          <textarea readOnly dir="ltr" rows={8} value={copyFallback} aria-labelledby="copy-heading" onFocus={(e) => e.currentTarget.select()} />
          <button type="button" onClick={() => setCopyFallback(null)}>
            Fermer
          </button>
        </section>
      )}

      {notDraft && (
        <section className="conflict" aria-labelledby="notdraft-heading">
          <h2 id="notdraft-heading" tabIndex={-1} ref={notDraftRef}>
            Ce brouillon a été soumis ailleurs
          </h2>
          <p>
            Une autre fenêtre ou un autre appareil a soumis ce brouillon pour relecture : il ne peut plus être modifié. <strong>Vos modifications ne sont pas perdues</strong> : elles sont toujours dans l’éditeur, et une copie de secours de cette leçon a été faite sur cet appareil. Elle vous sera proposée sur la révision soumise et sur la prochaine version de la leçon.
          </p>
          <div className="actions">
            <button type="button" onClick={() => void copyText(localText(data()), 'Votre contenu local')}>
              Copier mon contenu local
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
              Voir la révision soumise
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
          setMessage({ text: 'Copie de secours restaurée dans l’éditeur. Enregistrez pour la conserver sur le serveur.' });
        }}
        onChanged={() => refreshEntries()}
      />

      {props.feedback}

      <div className="editor-layout">
        <section className="panel" aria-labelledby="edit-heading">
          <h2 id="edit-heading">Contenu</h2>
          <div className="fields">
            <label htmlFor={ids.title}>Titre</label>
            <input id={ids.title} value={title} maxLength={240} lang={language} dir={dir} onChange={(e) => setTitle(e.target.value)} />
            <label htmlFor={ids.objective}>Objectif</label>
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
            <legend>Classement</legend>
            <p className="hint">
              Matière : <strong>{props.subjectCode}</strong> · Langue : <strong>{LANGUAGE_LABELS[language]}</strong> (fixées à la création)
            </p>
            <label htmlFor={ids.chapter}>Chapitre</label>
            <select id={ids.chapter} value={chapterId ?? ''} onChange={(e) => setChapterId(e.target.value ? Number(e.target.value) : null)}>
              <option value="">Sans chapitre</option>
              {chapters.map((c) => (
                <option key={c.id} value={c.id}>
                  {c.code}
                </option>
              ))}
            </select>
            <label htmlFor={ids.prerequisites}>Prérequis</label>
            <textarea id={ids.prerequisites} value={prerequisites} rows={2} lang={language} dir={dir} onChange={(e) => setPrerequisites(e.target.value)} />
          </fieldset>

          <section className="media" aria-labelledby="media-heading">
            <h3 id="media-heading">Images jointes</h3>
            <p className="hint">
              Téléversez une image puis placez-la dans le texte, à l’emplacement du curseur, avec un texte alternatif obligatoire. Les images sont jointes à la révision comme références immuables : enregistrées avec le brouillon, figées à la soumission, visibles des relecteurs, puis des élèves une fois la leçon publiée. Une image retirée du texte reste jointe jusqu’à ce que vous la retiriez de la révision.
            </p>
            {mediaIds.length > 0 ? (
              <ul className="media-list">
                {mediaIds.map((id) => (
                  <li key={id}>
                    <img src={`/api/media/${id}`} alt={`Image jointe n° ${id}`} width={mediaInfo[id]?.width} height={mediaInfo[id]?.height} />
                    <span>
                      Image n° {id}
                      {mediaInfo[id] ? ` · ${mediaInfo[id].width} × ${mediaInfo[id].height}` : ''}
                      {embedded.has(id) ? ' · placée dans le texte' : ' · non placée'}
                    </span>
                    <button type="button" onClick={() => setPlacing({ id, alt: '', caption: '' })}>
                      Placer dans le texte
                    </button>
                    <button
                      type="button"
                      disabled={embedded.has(id)}
                      title={embedded.has(id) ? 'Supprimez d’abord l’image du texte.' : undefined}
                      onClick={() => setMediaIds((ids) => ids.filter((x) => x !== id))}
                    >
                      Retirer de la révision
                    </button>
                  </li>
                ))}
              </ul>
            ) : (
              <p>Aucune image jointe.</p>
            )}
            <label htmlFor={ids.upload}>Téléverser une image (PNG, JPEG ou WebP, 5 Mio au maximum)</label>
            <input id={ids.upload} type="file" accept={IMAGE_TYPES.join(',')} onChange={(e) => void upload(e)} disabled={uploading} />
            {uploading && <p role="status">Téléversement en cours…</p>}
            {uploadError && (
              <p className="banner error" role="alert">
                {uploadError}
              </p>
            )}
          </section>

          <details className="tools">
            <summary>Importer ou exporter le JSON</summary>
            <p className="hint">Format restreint (schéma 1). L’import remplace le contenu de l’éditeur, sans rien enregistrer.</p>
            <div className="actions">
              <button type="button" onClick={onExport}>
                Exporter le JSON
              </button>
              <button type="button" onClick={() => importRef.current?.click()}>
                Importer un JSON
              </button>
              <input ref={importRef} type="file" accept="application/json,.json" hidden onChange={(e) => void onImport(e)} />
            </div>
          </details>

          {target.kind === 'revision' && (
            <div className="danger-zone">
              <button type="button" className="danger" onClick={() => setDialog('discard')} disabled={saving}>
                Supprimer ce brouillon
              </button>
            </div>
          )}
        </section>

        <section className="panel" aria-labelledby="preview-heading">
          <h2 id="preview-heading">Aperçu élève</h2>
          <p className="hint">Rendu par le serveur, identique à la page de lecture de l’élève. Non publié.</p>
          <button type="button" onClick={() => void runPreview()} disabled={previewing}>
            {preview ? 'Actualiser l’aperçu' : 'Prévisualiser'}
          </button>
          <p role="status" className={previewError ? 'error' : 'hint'}>
            {previewError ??
              (previewing ? 'Aperçu en cours…' : preview ? (previewStale ? `Aperçu du ${formatTime(preview.at)} : il ne reflète pas les dernières modifications.` : `Aperçu à jour (${formatTime(preview.at)}).`) : '')}
          </p>
          <div ref={previewRef} className="lesson-preview" data-testid="lesson-preview" hidden={!preview} />
        </section>
      </div>

      {placing && (
        <Dialog
          title={`Placer l’image n° ${placing.id} dans le texte`}
          onCancel={() => setPlacing(null)}
          initialFocus={altRef}
          actions={[{ label: 'Placer l’image', kind: 'primary', disabled: BLANK.test(placing.alt), onClick: placeImage }]}
        >
          <img className="placing-preview" src={`/api/media/${placing.id}`} alt="" />
          <label>
            Texte alternatif (obligatoire)
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
            Décrivez ce que l’image apporte à la leçon, pour les élèves qui ne la voient pas.
          </p>
          <label>
            Légende (facultative, visible sous l’image)
            <input dir="auto" lang={language} maxLength={1000} value={placing.caption} onChange={(e) => setPlacing({ ...placing, caption: e.target.value })} />
          </label>
          <p className="hint">L’image sera insérée à l’emplacement du curseur dans l’éditeur.</p>
        </Dialog>
      )}
      {dialog === 'submit' && (
        <Dialog title="Soumettre pour relecture ?" onCancel={() => setDialog(null)} actions={[{ label: 'Soumettre', kind: 'primary', onClick: () => void submit() }]}>
          <p>Après soumission, cette révision ne pourra plus être modifiée. Un relecteur de la matière l’examinera ; pour corriger ensuite, vous créerez une nouvelle version.</p>
          {dirty && <p>Les modifications en cours seront d’abord enregistrées.</p>}
        </Dialog>
      )}
      {dialog === 'discard' && (
        <Dialog title="Supprimer ce brouillon ?" onCancel={() => setDialog(null)} cancelLabel="Garder le brouillon" actions={[{ label: 'Supprimer définitivement', kind: 'danger', onClick: () => void discard() }]}>
          <p>Le brouillon et ses références d’images seront supprimés du serveur. Cette action est définitive. Les versions déjà soumises ou publiées ne sont pas touchées. Les copies de secours d’autres onglets restent proposées sur cette leçon.</p>
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
      <h2 id="feedback-heading">Le relecteur a demandé des modifications</h2>
      <p className="hint">
        Révision {revision.number} · {formatDateTime(revision.review.decidedAt)}
      </p>
      <blockquote dir="auto">{revision.review.comment}</blockquote>
    </section>
  );
}
