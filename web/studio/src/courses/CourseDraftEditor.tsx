// The course draft editor: overview fields and cover, curriculum placement,
// and the outline of modules and steps. The server is the only persistence:
// "Enregistrer" sends the whole draft with expectedVersion; a stale save
// opens the conflict panel without touching the local edits, and unsaved
// edits are mirrored to the labelled per-user recovery copies
// (courseRecovery.ts) and guarded on navigation, sign-out and page unload,
// as the lesson editor does (editor/DraftEditor.tsx).
import { useCallback, useEffect, useId, useMemo, useRef, useState, type ChangeEvent, type ReactNode } from 'react';
import { ApiError, type Chapter, type Subject } from '../api/client';
import type { Course, CourseRevision, Edition, Level } from '../api/courses';
import { courseApi } from '../api/courses';
import { describeCourseError } from '../api/courseMessages';
import { describeError, isHeif } from '../api/messages';
import { CatalogueName, catalogueLabel, entryLabel } from '../catalogue';
import { isUnloadAllowed, useStudio } from '../context';
import { LANGUAGE_LABELS, formatDateTime, formatTime } from '../labels';
import { addNavigationGuard } from '../router';
import { newInstance } from '../recovery';
import { Dialog } from '../ui/Dialog';
import { courseRecoveryKey, findCourseRecovery, removeCourseRecovery, writeCourseRecovery, type CourseRecoveryEntry, type CourseRecoveryTarget } from './courseRecovery';
import { LessonPicker, type PickedStep } from './LessonPicker';
import { OutlineSection, type ChapterOption, type OutlineActions } from './OutlineSection';
import * as outline from './outline';
import { LIMITS, checkDraft, fromRevision, snapshot, toSaveBody, type CourseDraft, type DraftStep, type Problem } from './outline';
import { PlacementSection, type PlacementInfo } from './PlacementSection';
import { saveCourse } from './saveCourse';

const IMAGE_TYPES = ['image/png', 'image/jpeg', 'image/webp'];
const MAX_IMAGE_BYTES = 5 << 20;
const RECOVERY_DELAY = 700;

type Message = { text: string; error?: boolean } | null;

export interface CourseDraftEditorProps {
  course: Course;
  revision: CourseRevision;
  subject: Subject;
  /** The subject's catalogue chapters, for names. */
  chapters: Chapter[];
  levels: Level[];
  /** Titles of the author's own lessons, to name the lessons steps pin. */
  lessonTitles: Map<number, string>;
  /** Shown above the editor (for example steps dropped when the draft was created). */
  notice?: ReactNode;
  onDiscarded: () => void;
  onDeleted: () => void;
  /** The draft left draft status elsewhere (B04 submission): the page reloads it read-only. */
  onLeftDraft: () => void;
  /** After each successful save (the page heading shows the saved title). */
  onSaved?: (revision: CourseRevision) => void;
}

const withTitles = (draft: CourseDraft, titles: Map<number, string>): CourseDraft => ({
  ...draft,
  modules: draft.modules.map((m) => ({ ...m, steps: m.steps.map((s) => ({ ...s, lessonTitle: s.lessonTitle ?? titles.get(s.lessonId) ?? null })) })),
});

const placementsOf = (revision: CourseRevision): Record<number, PlacementInfo> => Object.fromEntries(revision.assignments.map((a) => [a.assignmentId, a]));

export function CourseDraftEditor(props: CourseDraftEditorProps) {
  const { api, session, expired, accountChanged } = useStudio();
  const courses = useMemo(() => courseApi(api), [api]);
  const { course, subject, levels } = props;
  const language = course.language;
  const dir = language === 'ar' ? 'rtl' : 'ltr';
  const userId = session.user.id;
  const instance = useMemo(newInstance, []);
  const ids = { title: useId(), summary: useId(), objectives: useId(), prerequisites: useId(), cover: useId(), coverAlt: useId(), save: useId(), overview: useId() };

  const [server, setServer] = useState({ id: props.revision.id, version: props.revision.version, number: props.revision.number });
  const [draft, setDraft] = useState<CourseDraft>(() => withTitles(fromRevision(props.revision), props.lessonTitles));
  const [baseline, setBaseline] = useState(() => snapshot(fromRevision(props.revision)));
  const [placements, setPlacements] = useState<Record<number, PlacementInfo>>(() => placementsOf(props.revision));
  const [editions, setEditions] = useState<Record<number, Edition>>({});
  const [saving, setSaving] = useState(false);
  const [lastSaved, setLastSaved] = useState<Date | null>(null);
  const [message, setMessage] = useState<Message>(null);
  const [problems, setProblems] = useState<Problem[]>([]);
  const [placementInvalid, setPlacementInvalid] = useState(false);
  const [conflict, setConflict] = useState(false);
  const [notDraft, setNotDraft] = useState(false);
  const [entries, setEntries] = useState<CourseRecoveryEntry[]>([]);
  const [restoredKey, setRestoredKey] = useState<string | null>(null);
  const [dialog, setDialog] = useState<'discard' | 'delete' | null>(null);
  const [picker, setPicker] = useState<{ moduleId: string; step?: DraftStep } | null>(null);
  const [announcement, setAnnouncement] = useState('');
  const [focusRequest, setFocusRequest] = useState<{ id: string; fallback?: string; n: number } | null>(null);
  const [uploading, setUploading] = useState(false);
  const [uploadError, setUploadError] = useState<string | null>(null);
  const conflictRef = useRef<HTMLHeadingElement>(null);
  const notDraftRef = useRef<HTMLHeadingElement>(null);
  const problemsRef = useRef<HTMLDivElement>(null);

  const current = useMemo(() => snapshot(draft), [draft]);
  const dirty = current !== baseline;
  const dirtyRef = useRef(dirty);
  dirtyRef.current = dirty;
  const draftRef = useRef(draft);
  draftRef.current = draft;

  /* ---------- curriculum names ---------- */

  // Edition details (labels, assignment chapters) for every placement shown.
  useEffect(() => {
    const missing = [...new Set(Object.values(placements).map((p) => p.editionId))].filter((id) => !editions[id]);
    if (missing.length === 0) return;
    let cancelled = false;
    void Promise.all(missing.map((id) => courses.edition(id).catch(() => null))).then((loaded) => {
      if (cancelled) return;
      setEditions((e) => ({ ...e, ...Object.fromEntries(loaded.filter((x): x is Edition => x !== null).map((x) => [x.id, x])) }));
    });
    return () => {
      cancelled = true;
    };
  }, [courses, placements, editions]);

  const chaptersOf = useCallback(
    (assignmentId: number) => {
      const info = placements[assignmentId];
      const assignment = info ? editions[info.editionId]?.assignments?.find((a) => a.id === assignmentId) : undefined;
      return assignment ? [...assignment.chapters].sort((a, b) => a.position - b.position) : [];
    },
    [placements, editions],
  );

  const chapterOptions = useMemo<ChapterOption[]>(() => {
    const byId = new Map(props.chapters.map((c) => [c.id, c]));
    const seen = new Map<number, ChapterOption>();
    for (const id of draft.assignmentIds)
      for (const c of chaptersOf(id)) {
        if (seen.has(c.chapterId)) continue;
        const entry = byId.get(c.chapterId);
        seen.set(c.chapterId, { id: c.chapterId, label: entry ? entryLabel(entry, language) : catalogueLabel(c.code, null, null, language), retired: c.retired || !!entry?.retiredAt });
      }
    return [...seen.values()];
  }, [draft.assignmentIds, chaptersOf, props.chapters, language]);

  /* ---------- recovery copies ---------- */

  const recoveryTarget = useCallback(
    (s = server): CourseRecoveryTarget => ({ userId, courseId: course.id, revisionId: s.id, baseVersion: s.version, revisionNumber: s.number, instance }),
    [server, userId, course.id, instance],
  );

  const refreshEntries = useCallback(
    (s = server) => setEntries(findCourseRecovery(localStorage, userId, course.id, courseRecoveryKey(recoveryTarget(s)))),
    [server, userId, course.id, recoveryTarget],
  );
  useEffect(() => refreshEntries(), []);

  // Copies identical to what the server holds are redundant: drop them silently.
  useEffect(() => {
    for (const e of entries) if (snapshot(e.data) === baseline && e.key !== restoredKey) removeCourseRecovery(localStorage, e.key);
  }, [entries, baseline]);
  const offered = entries.filter((e) => e.key !== restoredKey && snapshot(e.data) !== current && snapshot(e.data) !== baseline);

  const writeNow = useCallback(
    (reason: CourseRecoveryEntry['reason'] = 'unsaved') => {
      if (!dirtyRef.current && reason === 'unsaved') return;
      writeCourseRecovery(localStorage, recoveryTarget(), draftRef.current, reason);
    },
    [recoveryTarget],
  );
  useEffect(() => {
    if (!dirty) return;
    const timer = setTimeout(() => writeNow(), RECOVERY_DELAY);
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
  useEffect(() => {
    if (notDraft) notDraftRef.current?.focus();
  }, [notDraft]);

  /* ---------- saving ---------- */

  const save = useCallback(async () => {
    if (saving) return;
    const sent = draftRef.current;
    if (expired) {
      // Nothing is sent until the author resumes with their own account (banner at the top).
      writeCourseRecovery(localStorage, recoveryTarget(), sent);
      setMessage({ text: `Rien n’a été envoyé : ${accountChanged ? 'un autre compte est connecté dans ce navigateur' : 'la session a expiré'}. Vos modifications restent ici et dans la copie de secours ; reconnectez-vous puis choisissez « Reprendre ici ».`, error: true });
      return;
    }
    const found = checkDraft(sent);
    setProblems(found);
    if (found.length > 0) {
      setMessage({ text: 'Le brouillon contient des points à corriger ; rien n’a été enregistré.', error: true });
      requestAnimationFrame(() => problemsRef.current?.focus());
      return;
    }
    setSaving(true);
    setMessage(null);
    const before = recoveryTarget();
    const sentModules = sent.modules.map((m) => m.localId);
    // Edits made while the request is in flight stay unsaved: the baseline becomes what was sent, not what is shown.
    const outcome = await saveCourse(courses, server.id, server.version, toSaveBody(sent));
    setSaving(false);
    switch (outcome.kind) {
      case 'saved': {
        const revision = outcome.revision;
        removeCourseRecovery(localStorage, courseRecoveryKey(before));
        if (restoredKey) removeCourseRecovery(localStorage, restoredKey);
        setRestoredKey(null);
        const next = { id: revision.id, version: revision.version, number: revision.number };
        setServer(next);
        setPlacements((p) => ({ ...p, ...placementsOf(revision) }));
        const facts = new Map(revision.modules.flatMap((m) => m.steps).map((s) => [s.lessonId, s]));
        setDraft((d) =>
          outline.assignKeys(
            { ...d, modules: d.modules.map((m) => ({ ...m, steps: m.steps.map((s) => { const f = facts.get(s.lessonId); return f && f.revisionId === s.revisionId ? { ...s, revisionNumber: f.revisionNumber, ownLesson: f.ownLesson, eligible: f.eligible } : s; }) })) },
            sentModules,
            revision.modules.map((m) => m.key),
          ),
        );
        setBaseline(snapshot(sent));
        setLastSaved(new Date());
        setPlacementInvalid(false);
        setMessage({ text: 'Brouillon du cours enregistré sur le serveur.' });
        refreshEntries(next);
        props.onSaved?.(revision);
        return;
      }
      case 'conflict':
        writeCourseRecovery(localStorage, before, sent, 'conflict');
        setConflict(true);
        setMessage({ text: 'Enregistrement refusé : conflit de version. Vos modifications sont conservées.', error: true });
        return;
      case 'not_draft':
        writeCourseRecovery(localStorage, before, sent, 'conflict');
        setNotDraft(true);
        return;
      case 'expired':
        writeCourseRecovery(localStorage, before, sent);
        setMessage({ text: 'Session expirée : rien n’a été enregistré. Vos modifications restent ici et dans la copie de secours. Reconnectez-vous puis enregistrez à nouveau.', error: true });
        return;
      case 'failed':
        writeCourseRecovery(localStorage, before, sent);
        setPlacementInvalid(outcome.error instanceof ApiError && outcome.error.code === 'placement_invalid');
        setMessage({ text: `Enregistrement impossible. ${describeCourseError(outcome.error)} Vos modifications restent dans l’éditeur.`, error: true });
        return;
    }
  }, [saving, expired, accountChanged, recoveryTarget, courses, server, restoredKey, refreshEntries, props]);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.ctrlKey || e.metaKey) && !e.altKey && e.key.toLowerCase() === 's' && !picker && !dialog) {
        e.preventDefault();
        void save();
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [save, picker, dialog]);

  const reloadServer = async () => {
    writeCourseRecovery(localStorage, recoveryTarget(), draftRef.current, 'conflict');
    try {
      const revision = await courses.revision(server.id);
      if (revision.status !== 'draft') {
        setConflict(false);
        props.onLeftDraft();
        return;
      }
      const next = { id: revision.id, version: revision.version, number: revision.number };
      setServer(next);
      setPlacements(placementsOf(revision));
      const fresh = withTitles(fromRevision(revision), props.lessonTitles);
      setDraft(fresh);
      setBaseline(snapshot(fresh));
      setProblems([]);
      setConflict(false);
      refreshEntries(next);
      setMessage({ text: `Version ${revision.version} du serveur chargée. Votre copie locale est conservée ci-dessous : vous pouvez la restaurer.` });
    } catch (error) {
      setMessage({ text: `Rechargement impossible. ${describeCourseError(error)} Vos modifications sont toujours dans l’éditeur.`, error: true });
    }
  };

  /* ---------- discard and delete ---------- */

  const discard = async () => {
    setDialog(null);
    if (expired) return;
    try {
      await courses.discard(server.id, server.version);
      removeCourseRecovery(localStorage, courseRecoveryKey(recoveryTarget()));
      if (restoredKey) removeCourseRecovery(localStorage, restoredKey);
      dirtyRef.current = false;
      props.onDiscarded();
    } catch (error) {
      if (error instanceof ApiError && error.code === 'version_conflict') {
        setConflict(true);
        setMessage({ text: 'Suppression refusée : le brouillon a changé ailleurs. Rechargez la version du serveur avant de le supprimer.', error: true });
      } else setMessage({ text: `Suppression impossible. ${describeCourseError(error)}`, error: true });
    }
  };

  const deleteCourse = async () => {
    setDialog(null);
    if (expired) return;
    try {
      await courses.deleteCourse(course.id);
      // The course no longer exists: its copies on this device have nowhere to be restored.
      for (const e of findCourseRecovery(localStorage, userId, course.id)) removeCourseRecovery(localStorage, e.key);
      dirtyRef.current = false;
      props.onDeleted();
    } catch (error) {
      setMessage({ text: `Suppression du cours impossible. ${describeCourseError(error)}`, error: true });
    }
  };

  /* ---------- outline actions ---------- */

  const focusLater = (id: string, fallback?: string) => setFocusRequest((f) => ({ id, fallback, n: (f?.n ?? 0) + 1 }));

  const actions: OutlineActions = {
    addModule: (title) => {
      const next = outline.addModule(draft, title);
      setDraft(next);
      const added = next.modules[next.modules.length - 1];
      setAnnouncement(`Module ${next.modules.length} « ${title} » ajouté.`);
      focusLater(`module-title-${added.localId}`);
    },
    renameModule: (id, title) => setDraft((d) => outline.updateModule(d, id, { title })),
    setChapter: (id, chapterId) => setDraft((d) => outline.updateModule(d, id, { chapterId })),
    moveModule: (index, delta) => {
      const moved = draft.modules[index];
      const next = outline.moveModule(draft, index, delta);
      if (next === draft) return;
      setDraft(next);
      setAnnouncement(`Module « ${moved.title.trim() || 'sans titre'} » déplacé en position ${index + delta + 1} sur ${next.modules.length}.`);
      const [here, other] = delta < 0 ? ['up', 'down'] : ['down', 'up'];
      focusLater(`module-${here}-${moved.localId}`, `module-${other}-${moved.localId}`);
    },
    removeModule: (id) => {
      const index = draft.modules.findIndex((m) => m.localId === id);
      const removed = draft.modules[index];
      setDraft(outline.removeModule(draft, id));
      setAnnouncement(`Module « ${removed?.title.trim() || 'sans titre'} » supprimé du plan.`);
      const neighbour = draft.modules[index + 1] ?? draft.modules[index - 1];
      focusLater(neighbour ? `module-title-${neighbour.localId}` : 'course-outline-new');
    },
    renameStep: (id, title) => setDraft((d) => outline.updateStep(d, id, { title })),
    moveStep: (id, delta) => {
      const next = outline.moveStep(draft, id, delta);
      if (next === draft) return;
      setDraft(next);
      const at = outline.findStep(next, id)!;
      const step = next.modules[at.module].steps[at.step];
      setAnnouncement(`Étape « ${step.title.trim() || 'sans titre'} » déplacée : module ${at.module + 1}, position ${at.step + 1}.`);
      const [here, other] = delta < 0 ? ['up', 'down'] : ['down', 'up'];
      focusLater(`step-${here}-${id}`, `step-${other}-${id}`);
    },
    moveStepTo: (id, moduleId) => {
      const next = outline.moveStepToModule(draft, id, moduleId);
      if (next === draft) return;
      setDraft(next);
      const at = outline.findStep(next, id)!;
      setAnnouncement(`Étape déplacée à la fin du module ${at.module + 1}, position ${at.step + 1}.`);
      focusLater(`step-up-${id}`, `step-title-${id}`);
    },
    removeStep: (id) => {
      const at = outline.findStep(draft, id);
      if (!at) return;
      const module = draft.modules[at.module];
      const step = module.steps[at.step];
      setDraft(outline.removeStep(draft, id));
      setAnnouncement(`Étape « ${step.title.trim() || 'sans titre'} » retirée du plan. La leçon n’est pas supprimée.`);
      const neighbour = module.steps[at.step + 1] ?? module.steps[at.step - 1];
      focusLater(neighbour ? `step-title-${neighbour.localId}` : `module-title-${module.localId}`);
    },
    pickLesson: (moduleId, step) => setPicker({ moduleId, step }),
  };

  const onPicked = (picked: PickedStep) => {
    if (!picker) return;
    if (picker.step) {
      setDraft(outline.updateStep(draft, picker.step.localId, { revisionId: picked.revisionId, revisionNumber: picked.revisionNumber, eligible: picked.eligible }));
      setAnnouncement(`Révision ${picked.revisionNumber} épinglée.`);
    } else {
      const next = outline.addStep(draft, picker.moduleId, picked);
      setDraft(next);
      const module = next.modules.find((m) => m.localId === picker.moduleId)!;
      const added = module.steps[module.steps.length - 1];
      setAnnouncement(`Étape « ${picked.title} » ajoutée au module.`);
      focusLater(`step-title-${added.localId}`);
    }
    setPicker(null);
  };

  /* ---------- cover ---------- */

  const uploadCover = async (event: ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0];
    event.target.value = '';
    if (!file) return;
    setUploadError(null);
    if (expired) {
      setUploadError(describeError(new ApiError(0, accountChanged ? 'account_changed' : 'unauthenticated', '')));
      return;
    }
    if (isHeif(file)) return setUploadError(describeError(new ApiError(415, 'unsupported_heic', '')));
    if (!IMAGE_TYPES.includes(file.type)) return setUploadError('Format refusé : seules les images PNG, JPEG et WebP sont acceptées (pas de SVG ni de GIF).');
    if (file.size > MAX_IMAGE_BYTES) return setUploadError('Image trop lourde : 5 Mio au maximum. Réduisez-la ou ré-exportez-la.');
    setUploading(true);
    try {
      const media = await api.uploadMedia(file);
      setDraft((d) => ({ ...d, coverMediaId: media.id }));
      setMessage({ text: `Image « ${file.name} » téléversée (${media.width} × ${media.height}). Rédigez son texte alternatif, puis enregistrez le brouillon.` });
    } catch (error) {
      setUploadError(`Téléversement refusé. ${describeError(error)}`);
    } finally {
      setUploading(false);
    }
  };

  /* ---------- rendering ---------- */

  const invalidTargets = new Set(problems.map((p) => p.target));
  const text = (key: 'title' | 'summary' | 'objectives' | 'prerequisites', value: string) => setDraft((d) => ({ ...d, [key]: value }));
  const count = (value: string, max: number) => `${[...value.trim()].length.toLocaleString('fr-FR')} / ${max.toLocaleString('fr-FR')} caractères`;
  const saveState = saving
    ? 'Enregistrement en cours…'
    : expired && dirty
      ? 'Session expirée : modifications non enregistrées, conservées sur cet appareil.'
      : dirty
        ? 'Modifications non enregistrées sur le serveur.'
        : lastSaved
          ? `Enregistré sur le serveur à ${formatTime(lastSaved)} (version ${server.version}).`
          : `À jour avec le serveur (version ${server.version}).`;

  return (
    <div className="editor-page course-editor">
      <div className="editor-bar">
        <p className="revision-label">Brouillon · version {server.number} du cours</p>
        <p id={ids.save} className={dirty ? 'save-state dirty' : 'save-state'} role="status" aria-live="polite">
          {saveState}
        </p>
        <div className="actions">
          <button type="button" className="primary" onClick={() => void save()} disabled={saving} aria-describedby={ids.save}>
            Enregistrer
          </button>
          {/* Submission for review is LMS-B04; its action belongs here, after saving. */}
        </div>
      </div>

      {message && (
        <p className={message.error ? 'banner error' : 'banner success'} role={message.error ? 'alert' : 'status'}>
          {message.text}
        </p>
      )}
      {problems.length > 0 && (
        <div tabIndex={-1} ref={problemsRef} className="problems">
          <ul className="errors" aria-label="Points à corriger avant l’enregistrement">
            {problems.map((p, i) => (
              <li key={`${p.target}-${i}`}>{p.message}</li>
            ))}
          </ul>
        </div>
      )}

      {conflict && (
        <section className="conflict" aria-labelledby="course-conflict-heading">
          <h2 id="course-conflict-heading" tabIndex={-1} ref={conflictRef}>
            Conflit : ce brouillon de cours a été enregistré ailleurs
          </h2>
          <p>
            Une autre fenêtre ou un autre appareil a enregistré ce brouillon après son ouverture ici. Le serveur a refusé d’écraser cette version. <strong>Vos modifications ne sont pas perdues</strong> : elles sont toujours dans l’éditeur, et une copie de secours a été faite sur cet appareil.
          </p>
          <div className="actions">
            <button type="button" className="primary" onClick={() => void reloadServer()}>
              Charger la version du serveur
            </button>
            <button type="button" onClick={() => setConflict(false)}>
              Continuer à éditer
            </button>
          </div>
          <p className="hint">Charger la version du serveur remplace le contenu de l’éditeur ; votre version locale reste proposée ci-dessous comme copie de secours, à restaurer puis enregistrer.</p>
        </section>
      )}
      {notDraft && (
        <section className="conflict" aria-labelledby="course-notdraft-heading">
          <h2 id="course-notdraft-heading" tabIndex={-1} ref={notDraftRef}>
            Ce brouillon a quitté l’état de brouillon
          </h2>
          <p>
            Il a été soumis ou supprimé depuis un autre onglet ou appareil : il ne peut plus être modifié. <strong>Vos modifications ne sont pas perdues</strong> : une copie de secours a été faite sur cet appareil ; elle vous sera proposée sur le prochain brouillon de ce cours.
          </p>
          <button
            type="button"
            className="primary"
            onClick={() => {
              dirtyRef.current = false;
              props.onLeftDraft();
            }}
          >
            Recharger le cours
          </button>
        </section>
      )}

      {offered.length > 0 && (
        <section className="recovery" aria-labelledby="course-recovery-heading">
          <h2 id="course-recovery-heading">Copies de secours locales (non enregistrées)</h2>
          <p className="hint">Ces modifications n’ont jamais été enregistrées sur le serveur. Elles ne sont conservées que dans ce navigateur, pour votre compte (14 jours au plus, effacées à la déconnexion).</p>
          <ul>
            {offered.map((e) => (
              <li key={e.key}>
                <p>
                  Copie du {formatDateTime(e.savedAt)}
                  {e.revisionId !== server.id ? `, faite sur la version ${e.revisionNumber} du cours` : `, faite à partir de la version ${e.baseVersion} du serveur`}
                  {e.reason === 'conflict' ? ' (gardée lors d’un conflit)' : ''} : « {e.data.title || 'sans titre'} », {e.data.modules.length} module{e.data.modules.length > 1 ? 's' : ''}
                </p>
                <div className="actions">
                  <button
                    type="button"
                    onClick={() => {
                      setDraft(withTitles({ ...e.data, modules: e.data.modules.map((m) => ({ ...m, steps: m.steps.map((s) => ({ ...s })) })) }, props.lessonTitles));
                      setRestoredKey(e.key);
                      setProblems([]);
                      setMessage({ text: 'Copie de secours restaurée dans l’éditeur. Enregistrez pour la conserver sur le serveur.' });
                    }}
                  >
                    Restaurer dans l’éditeur
                  </button>
                  <button
                    type="button"
                    onClick={() => {
                      removeCourseRecovery(localStorage, e.key);
                      refreshEntries();
                    }}
                  >
                    Supprimer cette copie
                  </button>
                </div>
              </li>
            ))}
          </ul>
        </section>
      )}

      {props.notice}

      <section className="panel course-section" aria-labelledby={ids.overview}>
        <h2 id={ids.overview}>Présentation du cours</h2>
        <p className="hint">
          Matière :{' '}
          <strong>
            <CatalogueName label={entryLabel(subject, language)} />
          </strong>{' '}
          · Langue : <strong>{LANGUAGE_LABELS[language]}</strong> (fixées à la création). Les champs peuvent rester vides dans un brouillon ; ils seront exigés à la soumission.
        </p>
        <div className="fields">
          <label htmlFor={ids.title}>Titre du cours</label>
          <input id={ids.title} value={draft.title} maxLength={LIMITS.title} lang={language} dir={dir} aria-invalid={invalidTargets.has('title') || undefined} aria-describedby={`${ids.title}-count`} onChange={(e) => text('title', e.target.value)} />
          <p id={`${ids.title}-count`} className="hint">
            {count(draft.title, LIMITS.title)}
          </p>
          <label htmlFor={ids.summary}>Description courte (publique)</label>
          <textarea id={ids.summary} rows={3} value={draft.summary} lang={language} dir={dir} aria-invalid={invalidTargets.has('summary') || undefined} aria-describedby={`${ids.summary}-count`} onChange={(e) => text('summary', e.target.value)} />
          <p id={`${ids.summary}-count`} className="hint">
            {count(draft.summary, LIMITS.summary)} · Texte simple ; ne recopiez pas le contenu des leçons.
          </p>
          <label htmlFor={ids.objectives}>Objectifs d’apprentissage</label>
          <textarea id={ids.objectives} rows={3} value={draft.objectives} lang={language} dir={dir} aria-invalid={invalidTargets.has('objectives') || undefined} aria-describedby={`${ids.objectives}-count`} onChange={(e) => text('objectives', e.target.value)} />
          <p id={`${ids.objectives}-count`} className="hint">
            {count(draft.objectives, LIMITS.objectives)}
          </p>
          <label htmlFor={ids.prerequisites}>Prérequis</label>
          <textarea id={ids.prerequisites} rows={2} value={draft.prerequisites} lang={language} dir={dir} aria-invalid={invalidTargets.has('prerequisites') || undefined} aria-describedby={`${ids.prerequisites}-count`} onChange={(e) => text('prerequisites', e.target.value)} />
          <p id={`${ids.prerequisites}-count`} className="hint">
            {count(draft.prerequisites, LIMITS.prerequisites)}
          </p>
        </div>

        <fieldset className="media cover">
          <legend>Image de couverture (facultative)</legend>
          {draft.coverMediaId !== null ? (
            <>
              <img className="placing-preview" src={`/api/media/${draft.coverMediaId}`} alt="" />
              <label htmlFor={ids.coverAlt}>Texte alternatif de la couverture (obligatoire avec une image)</label>
              <input id={ids.coverAlt} value={draft.coverAlt} maxLength={LIMITS.coverAlt} lang={language} dir="auto" required aria-invalid={invalidTargets.has('coverAlt') || undefined} onChange={(e) => setDraft((d) => ({ ...d, coverAlt: e.target.value }))} />
              <p className="hint">Décrivez l’image pour les élèves qui ne la voient pas. {count(draft.coverAlt, LIMITS.coverAlt)}</p>
              <button type="button" onClick={() => setDraft((d) => ({ ...d, coverMediaId: null, coverAlt: '' }))}>
                Retirer la couverture
              </button>
            </>
          ) : (
            <p>Aucune image de couverture.</p>
          )}
          <label htmlFor={ids.cover}>{draft.coverMediaId !== null ? 'Remplacer l’image' : 'Téléverser une image'} (PNG, JPEG ou WebP, 5 Mio au maximum)</label>
          <input id={ids.cover} type="file" accept={IMAGE_TYPES.join(',')} onChange={(e) => void uploadCover(e)} disabled={uploading} />
          {uploading && <p role="status">Téléversement en cours…</p>}
          {uploadError && (
            <p className="banner error" role="alert">
              {uploadError}
            </p>
          )}
        </fieldset>
      </section>

      <PlacementSection
        courses={courses}
        language={language}
        subjectId={course.subjectId}
        levels={levels}
        assignmentIds={draft.assignmentIds}
        placements={placements}
        editions={editions}
        invalid={placementInvalid}
        onAdd={(info, edition) => {
          setPlacements((p) => ({ ...p, [info.assignmentId]: info }));
          setEditions((e) => ({ ...e, [edition.id]: edition }));
          setDraft((d) => (d.assignmentIds.includes(info.assignmentId) ? d : { ...d, assignmentIds: [...d.assignmentIds, info.assignmentId] }));
          setAnnouncement('Classement ajouté.');
        }}
        onRemove={(id) => {
          setDraft((d) => outline.removeAssignment(d, id, (a) => chaptersOf(a).map((c) => c.chapterId)));
          setAnnouncement('Classement retiré. Les modules dont le chapitre officiel n’appartient plus à aucun classement n’ont plus de chapitre.');
        }}
      />

      <OutlineSection draft={draft} language={language} chapters={chapterOptions} actions={actions} invalidTargets={invalidTargets} announcement={announcement} focusRequest={focusRequest} />

      <div className="danger-zone">
        <div className="actions">
          <button type="button" className="danger" onClick={() => setDialog('discard')} disabled={saving}>
            Supprimer ce brouillon
          </button>
          <button type="button" className="danger" onClick={() => setDialog('delete')} disabled={saving}>
            Supprimer le cours
          </button>
        </div>
        <p className="hint">Supprimer le cours n’est possible que s’il n’a jamais été soumis. Les leçons et les images ne sont jamais supprimées avec un cours.</p>
      </div>

      {picker && (
        <LessonPicker
          courses={courses}
          courseId={course.id}
          language={language}
          taken={outline.lessonsInOutline(draft)}
          change={picker.step}
          moduleTitle={draft.modules.find((m) => m.localId === picker.moduleId)?.title ?? ''}
          onPick={onPicked}
          onCancel={() => setPicker(null)}
        />
      )}
      {dialog === 'discard' && (
        <Dialog title="Supprimer ce brouillon ?" onCancel={() => setDialog(null)} cancelLabel="Garder le brouillon" actions={[{ label: 'Supprimer le brouillon', kind: 'danger', onClick: () => void discard() }]}>
          <p>Le brouillon (présentation, classement et plan) sera supprimé du serveur. Cette action est définitive. Le cours lui-même, ses versions déjà soumises, les leçons et les images ne sont pas touchés.</p>
        </Dialog>
      )}
      {dialog === 'delete' && (
        <Dialog title="Supprimer définitivement le cours ?" onCancel={() => setDialog(null)} cancelLabel="Garder le cours" actions={[{ label: 'Supprimer le cours', kind: 'danger', onClick: () => void deleteCourse() }]}>
          <p>
            Le cours et son brouillon seront supprimés du serveur, ainsi que les copies de secours de ce cours sur cet appareil. Cette action est définitive. Les leçons rattachées et les images ne sont pas supprimées. Un cours déjà soumis ne peut pas être supprimé.
          </p>
        </Dialog>
      )}
    </div>
  );
}
