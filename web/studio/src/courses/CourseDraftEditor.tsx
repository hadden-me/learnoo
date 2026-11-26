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
import { courseReviewApi, incompleteFields, ineligibleSteps, type ReviewedRevision } from '../api/courseReview';
import { describeCourseWorkflowError } from '../api/courseReviewMessages';
import { describeError, isHeif } from '../api/messages';
import { CatalogueName, catalogueLabel, entryLabel } from '../catalogue';
import { isUnloadAllowed, useStudio } from '../context';
import { intlLocale, t, tn } from '../i18n';
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
import { submissionProblems } from './workflow';

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
  /** The draft was submitted for review (LMS-B04): the page shows it read-only. */
  onSubmitted?: (revision: ReviewedRevision) => void;
}

const withTitles = (draft: CourseDraft, titles: Map<number, string>): CourseDraft => ({
  ...draft,
  modules: draft.modules.map((m) => ({ ...m, steps: m.steps.map((s) => ({ ...s, lessonTitle: s.lessonTitle ?? titles.get(s.lessonId) ?? null })) })),
});

const placementsOf = (revision: CourseRevision): Record<number, PlacementInfo> => Object.fromEntries(revision.assignments.map((a) => [a.assignmentId, a]));

export function CourseDraftEditor(props: CourseDraftEditorProps) {
  const { api, session, expired, accountChanged } = useStudio();
  const courses = useMemo(() => courseApi(api), [api]);
  const review = useMemo(() => courseReviewApi(api), [api]);
  const { course, subject, levels } = props;
  const language = course.language;
  const dir = language === 'ar' ? 'rtl' : 'ltr';
  const userId = session.user.id;
  const instance = useMemo(newInstance, []);
  const ids = { title: useId(), summary: useId(), objectives: useId(), prerequisites: useId(), cover: useId(), coverAlt: useId(), save: useId(), overview: useId(), problems: useId() };

  const [server, setServer] = useState({ id: props.revision.id, version: props.revision.version, number: props.revision.number });
  /** The kind the server derived at the last save (an amendment edited beyond replacements becomes ordinary, and back). */
  const [kind, setKind] = useState(props.revision.kind);
  const [draft, setDraft] = useState<CourseDraft>(() => withTitles(fromRevision(props.revision), props.lessonTitles));
  const [baseline, setBaseline] = useState(() => snapshot(fromRevision(props.revision)));
  const [placements, setPlacements] = useState<Record<number, PlacementInfo>>(() => placementsOf(props.revision));
  const [editions, setEditions] = useState<Record<number, Edition>>({});
  /** Editions whose details could not be read; retried only on request. */
  const [failedEditions, setFailedEditions] = useState<number[]>([]);
  /** The comparison revision's placements and module chapters; null while unknown (no flags are shown then). */
  const [comparison, setComparison] = useState<outline.Comparison | null>(() => (props.revision.compareRevisionId === null ? outline.NO_COMPARISON : null));
  const [comparisonError, setComparisonError] = useState(false);
  const [saving, setSaving] = useState(false);
  const [lastSaved, setLastSaved] = useState<Date | null>(null);
  const [message, setMessage] = useState<Message>(null);
  const [problems, setProblems] = useState<Problem[]>([]);
  const [placementInvalid, setPlacementInvalid] = useState(false);
  const [conflict, setConflict] = useState(false);
  const [notDraft, setNotDraft] = useState(false);
  const [entries, setEntries] = useState<CourseRecoveryEntry[]>([]);
  const [restoredKey, setRestoredKey] = useState<string | null>(null);
  const [dialog, setDialog] = useState<'discard' | 'delete' | 'submit' | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [picker, setPicker] = useState<{ moduleId: string; step?: DraftStep } | null>(null);
  const [announcement, setAnnouncement] = useState({ text: '', n: 0 });
  const announce = (text: string) => setAnnouncement((a) => ({ text, n: a.n + 1 }));
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
    const missing = outline.editionsToFetch(Object.values(placements).map((p) => p.editionId), editions, failedEditions);
    if (missing.length === 0) return;
    let cancelled = false;
    void Promise.all(missing.map((id) => courses.edition(id).catch(() => null))).then((loaded) => {
      if (cancelled) return;
      const ok = loaded.filter((x): x is Edition => x !== null);
      const failed = missing.filter((_, i) => loaded[i] === null);
      // State changes only when something happened, so a failure is never refetched in a loop.
      if (ok.length > 0) setEditions((e) => ({ ...e, ...Object.fromEntries(ok.map((x) => [x.id, x])) }));
      if (failed.length > 0) setFailedEditions((f) => [...f, ...failed]);
    });
    return () => {
      cancelled = true;
    };
  }, [courses, placements, editions, failedEditions]);

  const compareId = props.revision.compareRevisionId;
  const loadComparison = useCallback(() => {
    if (compareId === null) return;
    setComparisonError(false);
    courses.revision(compareId).then(
      (r) => setComparison(outline.comparisonOf(r)),
      () => setComparisonError(true),
    );
  }, [courses, compareId]);
  useEffect(loadComparison, [loadComparison]);

  const chaptersOf = useCallback(
    (assignmentId: number) => {
      const info = placements[assignmentId];
      const assignment = info ? editions[info.editionId]?.assignments?.find((a) => a.id === assignmentId) : undefined;
      // null: unknown (edition not read), never treated as "no chapters".
      return assignment ? [...assignment.chapters].sort((a, b) => a.position - b.position) : null;
    },
    [placements, editions],
  );

  const chapterOptions = useMemo<ChapterOption[]>(() => {
    const byId = new Map(props.chapters.map((c) => [c.id, c]));
    const seen = new Map<number, ChapterOption>();
    for (const id of draft.assignmentIds)
      for (const c of chaptersOf(id) ?? []) {
        if (seen.has(c.chapterId)) continue;
        const entry = byId.get(c.chapterId);
        seen.set(c.chapterId, { id: c.chapterId, label: entry ? entryLabel(entry, language) : catalogueLabel(c.code, null, null, language), retired: c.retired || !!entry?.retiredAt });
      }
    return [...seen.values()];
  }, [draft.assignmentIds, chaptersOf, props.chapters, language]);

  const retiredChapters = useMemo(() => {
    const ids = new Set(props.chapters.filter((c) => c.retiredAt).map((c) => c.id));
    for (const e of Object.values(editions)) for (const a of e.assignments ?? []) for (const c of a.chapters) if (c.retired) ids.add(c.chapterId);
    return ids;
  }, [props.chapters, editions]);
  const refusedPlacements = useMemo(
    () => new Set(comparison ? draft.assignmentIds.filter((id) => outline.placementRefused(id, placements[id], comparison)) : []),
    [draft.assignmentIds, placements, comparison],
  );
  const carriedPlacements = useMemo(() => new Set(comparison ? draft.assignmentIds.filter((id) => outline.placementCarried(id, comparison)) : []), [draft.assignmentIds, comparison]);
  const refusedChapters = useMemo(
    () => new Set(comparison ? draft.modules.filter((m) => outline.chapterRefused(m, retiredChapters, comparison)).map((m) => m.localId) : []),
    [draft.modules, retiredChapters, comparison],
  );

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
      setMessage({ text: t('courses.editor.notSentKept', { reason: accountChanged ? t('courses.editor.reason.otherAccount') : t('courses.editor.reason.expired') }), error: true });
      return;
    }
    const found = checkDraft(sent);
    setProblems(found);
    if (found.length > 0) {
      setMessage({ text: t('courses.editor.problemsNotSaved'), error: true });
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
        setKind(revision.kind);
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
        // Edits made during the save are still unsaved: protect them under the new version's key right away.
        if (snapshot(draftRef.current) !== snapshot(sent)) writeCourseRecovery(localStorage, recoveryTarget(next), draftRef.current);
        setLastSaved(new Date());
        setPlacementInvalid(false);
        setMessage({ text: t('courses.editor.saved') });
        refreshEntries(next);
        props.onSaved?.(revision);
        return;
      }
      case 'conflict':
        writeCourseRecovery(localStorage, before, sent, 'conflict');
        setConflict(true);
        setMessage({ text: t('courses.editor.conflictRefused'), error: true });
        return;
      case 'not_draft':
        writeCourseRecovery(localStorage, before, sent, 'conflict');
        setNotDraft(true);
        return;
      case 'expired':
        writeCourseRecovery(localStorage, before, sent);
        setMessage({ text: t('courses.editor.expiredNotSaved'), error: true });
        return;
      case 'failed':
        writeCourseRecovery(localStorage, before, sent);
        setPlacementInvalid(outcome.error instanceof ApiError && outcome.error.code === 'placement_invalid');
        setMessage({ text: t('courses.editor.saveFailed', { error: describeCourseError(outcome.error) }), error: true });
        return;
    }
  }, [saving, expired, accountChanged, recoveryTarget, courses, server, restoredKey, refreshEntries, props]);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.ctrlKey || e.metaKey) && !e.altKey && e.key.toLowerCase() === 's' && !picker && !dialog && !submitting) {
        e.preventDefault();
        void save();
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [save, picker, dialog, submitting]);

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
      setKind(revision.kind);
      setPlacements(placementsOf(revision));
      const fresh = withTitles(fromRevision(revision), props.lessonTitles);
      setDraft(fresh);
      setBaseline(snapshot(fresh));
      setProblems([]);
      setConflict(false);
      refreshEntries(next);
      setMessage({ text: t('courses.editor.serverLoaded', { version: revision.version }) });
    } catch (error) {
      setMessage({ text: t('courses.editor.reloadFailed', { error: describeCourseError(error) }), error: true });
    }
  };

  /* ---------- discard and delete ---------- */

  /** Nothing is sent while the session is expired or another account is signed in. */
  const refuseExpired = () =>
    setMessage({ text: t('courses.editor.notSent', { reason: accountChanged ? t('courses.editor.reason.otherAccount') : t('courses.editor.reason.expired') }), error: true });

  const discard = async () => {
    setDialog(null);
    if (expired) return refuseExpired();
    try {
      await courses.discard(server.id, server.version);
      removeCourseRecovery(localStorage, courseRecoveryKey(recoveryTarget()));
      if (restoredKey) removeCourseRecovery(localStorage, restoredKey);
      dirtyRef.current = false;
      props.onDiscarded();
    } catch (error) {
      if (error instanceof ApiError && error.code === 'version_conflict') {
        setConflict(true);
        setMessage({ text: t('courses.editor.discardConflict'), error: true });
      } else setMessage({ text: t('courses.editor.discardFailed', { error: describeCourseError(error) }), error: true });
    }
  };

  const deleteCourse = async () => {
    setDialog(null);
    if (expired) return refuseExpired();
    try {
      await courses.deleteCourse(course.id);
      // The course no longer exists: its copies on this device have nowhere to be restored.
      for (const e of findCourseRecovery(localStorage, userId, course.id)) removeCourseRecovery(localStorage, e.key);
      dirtyRef.current = false;
      props.onDeleted();
    } catch (error) {
      setMessage({ text: t('courses.editor.deleteFailed', { error: describeCourseError(error) }), error: true });
    }
  };

  /* ---------- submission (LMS-B04) ---------- */

  /** Asks for confirmation once the saved draft passes the client checks; the server checks everything again. */
  const askSubmit = () => {
    if (expired) return refuseExpired();
    if (dirtyRef.current) {
      setMessage({ text: t('courses.editor.saveFirst'), error: true });
      return;
    }
    const found = checkDraft(draftRef.current);
    setProblems(found);
    if (found.length > 0) {
      setMessage({ text: t('courses.editor.problemsNotSubmitted'), error: true });
      requestAnimationFrame(() => problemsRef.current?.focus());
      return;
    }
    setDialog('submit');
  };

  const submit = async () => {
    setDialog(null);
    if (expired) return refuseExpired();
    setSubmitting(true);
    setMessage(null);
    try {
      const submitted = await review.submit(server.id, server.version);
      removeCourseRecovery(localStorage, courseRecoveryKey(recoveryTarget()));
      if (restoredKey) removeCourseRecovery(localStorage, restoredKey);
      dirtyRef.current = false;
      props.onSubmitted?.(submitted);
    } catch (error) {
      if (error instanceof ApiError && (error.code === 'incomplete' || error.code === 'ineligible')) {
        setProblems(submissionProblems(draftRef.current, incompleteFields(error), ineligibleSteps(error)));
        requestAnimationFrame(() => problemsRef.current?.focus());
      }
      if (error instanceof ApiError && error.code === 'placement_invalid') setPlacementInvalid(true);
      if (error instanceof ApiError && error.code === 'version_conflict') setConflict(true);
      if (error instanceof ApiError && error.code === 'not_draft') {
        setNotDraft(true);
        return;
      }
      setMessage({ text: t('courses.editor.submitFailed', { error: describeCourseWorkflowError(error) }), error: true });
    } finally {
      setSubmitting(false);
    }
  };

  /* ---------- outline actions ---------- */

  const focusLater = (id: string, fallback?: string) => setFocusRequest((f) => ({ id, fallback, n: (f?.n ?? 0) + 1 }));

  const actions: OutlineActions = {
    addModule: (title) => {
      const next = outline.addModule(draft, title);
      setDraft(next);
      const added = next.modules[next.modules.length - 1];
      announce(t('courses.editor.moduleAdded', { index: next.modules.length, title }));
      focusLater(`module-title-${added.localId}`);
    },
    clearChapter: (id) => {
      setDraft((d) => outline.updateModule(d, id, { chapterId: null }));
      announce(t('courses.editor.chapterCleared'));
      focusLater(`module-title-${id}`);
    },
    renameModule: (id, title) => setDraft((d) => outline.updateModule(d, id, { title })),
    setChapter: (id, chapterId) => setDraft((d) => outline.updateModule(d, id, { chapterId })),
    moveModule: (index, delta) => {
      const moved = draft.modules[index];
      const next = outline.moveModule(draft, index, delta);
      if (next === draft) return;
      setDraft(next);
      announce(t('courses.editor.moduleMoved', { title: moved.title.trim() || t('courses.untitled'), position: index + delta + 1, total: next.modules.length }));
      const [here, other] = delta < 0 ? ['up', 'down'] : ['down', 'up'];
      focusLater(`module-${here}-${moved.localId}`, `module-${other}-${moved.localId}`);
    },
    removeModule: (id) => {
      const index = draft.modules.findIndex((m) => m.localId === id);
      const removed = draft.modules[index];
      setDraft(outline.removeModule(draft, id));
      announce(t('courses.editor.moduleRemoved', { title: removed?.title.trim() || t('courses.untitled') }));
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
      announce(t('courses.editor.stepMoved', { title: step.title.trim() || t('courses.untitled'), module: at.module + 1, position: at.step + 1 }));
      const [here, other] = delta < 0 ? ['up', 'down'] : ['down', 'up'];
      focusLater(`step-${here}-${id}`, `step-${other}-${id}`);
    },
    moveStepTo: (id, moduleId) => {
      const next = outline.moveStepToModule(draft, id, moduleId);
      if (next === draft) return;
      setDraft(next);
      const at = outline.findStep(next, id)!;
      announce(t('courses.editor.stepMovedTo', { module: at.module + 1, position: at.step + 1 }));
      focusLater(`step-up-${id}`, `step-title-${id}`);
    },
    removeStep: (id) => {
      const at = outline.findStep(draft, id);
      if (!at) return;
      const module = draft.modules[at.module];
      const step = module.steps[at.step];
      setDraft(outline.removeStep(draft, id));
      announce(t('courses.editor.stepRemoved', { title: step.title.trim() || t('courses.untitled') }));
      const neighbour = module.steps[at.step + 1] ?? module.steps[at.step - 1];
      focusLater(neighbour ? `step-title-${neighbour.localId}` : `module-title-${module.localId}`);
    },
    pickLesson: (moduleId, step) => setPicker({ moduleId, step }),
  };

  const onPicked = (picked: PickedStep) => {
    if (!picker) return;
    if (picker.step) {
      setDraft(outline.updateStep(draft, picker.step.localId, { revisionId: picked.revisionId, revisionNumber: picked.revisionNumber, eligible: picked.eligible }));
      announce(t('courses.editor.revisionPinned', { number: String(picked.revisionNumber) }));
    } else {
      const next = outline.addStep(draft, picker.moduleId, picked);
      setDraft(next);
      const module = next.modules.find((m) => m.localId === picker.moduleId)!;
      const added = module.steps[module.steps.length - 1];
      announce(t('courses.editor.stepAdded', { title: picked.title }));
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
    if (!IMAGE_TYPES.includes(file.type)) return setUploadError(t('shell.error.unsupported_media_type'));
    if (file.size > MAX_IMAGE_BYTES) return setUploadError(t('courses.editor.imageTooLarge'));
    setUploading(true);
    try {
      const media = await api.uploadMedia(file);
      setDraft((d) => ({ ...d, coverMediaId: media.id }));
      setMessage({ text: t('courses.editor.imageUploaded', { name: file.name, width: media.width, height: media.height }) });
    } catch (error) {
      setUploadError(t('courses.editor.uploadRefused', { error: describeError(error) }));
    } finally {
      setUploading(false);
    }
  };

  /* ---------- rendering ---------- */

  const invalidTargets = new Set(problems.map((p) => p.target));
  const text = (key: 'title' | 'summary' | 'objectives' | 'prerequisites', value: string) => setDraft((d) => ({ ...d, [key]: value }));
  const count = (value: string, max: number) => t('courses.chars', { count: [...value.trim()].length.toLocaleString(intlLocale()), max: max.toLocaleString(intlLocale()) });
  /** IDs of the problem messages about a target (aria-describedby). */
  const errorIds = (target: string) => {
    const found = problems.flatMap((p, i) => (p.target === target ? [`${ids.problems}-${i}`] : []));
    return found.length > 0 ? found.join(' ') : undefined;
  };
  const describedBy = (...list: Array<string | undefined>) => list.filter(Boolean).join(' ') || undefined;
  const saveState = saving
    ? t('courses.editor.saving')
    : expired && dirty
      ? t('courses.editor.expiredDirty')
      : dirty
        ? t('courses.editor.dirty')
        : lastSaved
          ? t('courses.editor.savedAt', { time: formatTime(lastSaved), version: server.version })
          : t('courses.editor.upToDate', { version: server.version });

  return (
    <div className="editor-page course-editor">
      <div className="editor-bar">
        <p className="revision-label">
          {t('courses.editor.revisionLabel', { number: server.number })}
          {kind === 'amendment' ? ` · ${t('courses.editor.amendment')}` : ''}
        </p>
        <p id={ids.save} className={dirty ? 'save-state dirty' : 'save-state'} role="status" aria-live="polite">
          {saveState}
        </p>
        <div className="actions">
          <button type="button" className="primary" onClick={() => void save()} disabled={saving || submitting} aria-describedby={ids.save}>
            {t('courses.editor.save')}
          </button>
          <button type="button" onClick={askSubmit} disabled={saving || submitting} aria-describedby={ids.save}>
            {submitting ? t('courses.editor.submitting') : t('courses.editor.submit')}
          </button>
        </div>
      </div>

      {message && (
        <p className={message.error ? 'banner error' : 'banner success'} role={message.error ? 'alert' : 'status'}>
          {message.text}
        </p>
      )}
      {problems.length > 0 && (
        <div tabIndex={-1} ref={problemsRef} className="problems">
          <ul className="errors" aria-label={t('courses.editor.problems')}>
            {problems.map((p, i) => (
              <li key={`${p.target}-${i}`} id={`${ids.problems}-${i}`}>
                {p.message}
              </li>
            ))}
          </ul>
        </div>
      )}

      {conflict && (
        <section className="conflict" aria-labelledby="course-conflict-heading">
          <h2 id="course-conflict-heading" tabIndex={-1} ref={conflictRef}>
            {t('courses.editor.conflictHeading')}
          </h2>
          <p>
            {t('courses.editor.conflictText')} <strong>{t('courses.editor.notLost')}</strong>
            {t('courses.editor.conflictKept')}
          </p>
          <div className="actions">
            <button type="button" className="primary" onClick={() => void reloadServer()}>
              {t('courses.editor.loadServer')}
            </button>
            <button type="button" onClick={() => setConflict(false)}>
              {t('courses.editor.keepEditing')}
            </button>
          </div>
          <p className="hint">{t('courses.editor.conflictHint')}</p>
        </section>
      )}
      {notDraft && (
        <section className="conflict" aria-labelledby="course-notdraft-heading">
          <h2 id="course-notdraft-heading" tabIndex={-1} ref={notDraftRef}>
            {t('courses.editor.notDraftHeading')}
          </h2>
          <p>
            {t('courses.editor.notDraftText')} <strong>{t('courses.editor.notLost')}</strong>
            {t('courses.editor.notDraftKept')}
          </p>
          <button
            type="button"
            className="primary"
            onClick={() => {
              dirtyRef.current = false;
              props.onLeftDraft();
            }}
          >
            {t('courses.editor.reloadCourse')}
          </button>
        </section>
      )}

      {offered.length > 0 && (
        <section className="recovery" aria-labelledby="course-recovery-heading">
          <h2 id="course-recovery-heading">{t('courses.editor.recoveryHeading')}</h2>
          <p className="hint">{t('courses.editor.recoveryHint')}</p>
          <ul>
            {offered.map((e) => (
              <li key={e.key}>
                <p>
                  {t('courses.editor.recoveryCopy', {
                    date: formatDateTime(e.savedAt),
                    origin: e.revisionId !== server.id ? t('courses.editor.recoveryOtherRevision', { number: e.revisionNumber }) : t('courses.editor.recoverySameRevision', { version: e.baseVersion }),
                    conflict: e.reason === 'conflict' ? t('courses.editor.recoveryConflict') : '',
                    title: e.data.title || t('courses.untitled'),
                    modules: tn('courses.editor.modules', e.data.modules.length),
                  })}
                </p>
                <div className="actions">
                  <button
                    type="button"
                    onClick={() => {
                      setDraft(withTitles({ ...e.data, modules: e.data.modules.map((m) => ({ ...m, steps: m.steps.map((s) => ({ ...s })) })) }, props.lessonTitles));
                      setRestoredKey(e.key);
                      setProblems([]);
                      setMessage({ text: t('courses.editor.restored') });
                    }}
                  >
                    {t('courses.editor.restore')}
                  </button>
                  <button
                    type="button"
                    onClick={() => {
                      removeCourseRecovery(localStorage, e.key);
                      refreshEntries();
                    }}
                  >
                    {t('courses.editor.deleteCopy')}
                  </button>
                </div>
              </li>
            ))}
          </ul>
        </section>
      )}

      {props.notice}

      <section className="panel course-section" aria-labelledby={ids.overview}>
        <h2 id={ids.overview}>{t('courses.editor.overview')}</h2>
        <p className="hint">
          {t('courses.editor.subjectLabel')}{' '}
          <strong>
            <CatalogueName label={entryLabel(subject, language)} />
          </strong>{' '}
          · {t('courses.editor.languageLabel')} <strong>{LANGUAGE_LABELS[language]}</strong>
          {t('courses.editor.fixedNote')}
        </p>
        <div className="fields">
          <label htmlFor={ids.title}>{t('courses.field.title')}</label>
          <input id={ids.title} value={draft.title} maxLength={LIMITS.title} lang={language} dir={dir} aria-invalid={invalidTargets.has('title') || undefined} aria-describedby={describedBy(`${ids.title}-count`, errorIds('title'))} onChange={(e) => text('title', e.target.value)} />
          <p id={`${ids.title}-count`} className="hint">
            {count(draft.title, LIMITS.title)}
          </p>
          <label htmlFor={ids.summary}>{t('courses.editor.summaryLabel')}</label>
          <textarea id={ids.summary} rows={3} value={draft.summary} lang={language} dir={dir} aria-invalid={invalidTargets.has('summary') || undefined} aria-describedby={describedBy(`${ids.summary}-count`, errorIds('summary'))} onChange={(e) => text('summary', e.target.value)} />
          <p id={`${ids.summary}-count`} className="hint">
            {count(draft.summary, LIMITS.summary)} · {t('courses.editor.summaryHint')}
          </p>
          <label htmlFor={ids.objectives}>{t('courses.editor.objectivesLabel')}</label>
          <textarea id={ids.objectives} rows={3} value={draft.objectives} lang={language} dir={dir} aria-invalid={invalidTargets.has('objectives') || undefined} aria-describedby={describedBy(`${ids.objectives}-count`, errorIds('objectives'))} onChange={(e) => text('objectives', e.target.value)} />
          <p id={`${ids.objectives}-count`} className="hint">
            {count(draft.objectives, LIMITS.objectives)}
          </p>
          <label htmlFor={ids.prerequisites}>{t('courses.field.prerequisites')}</label>
          <textarea id={ids.prerequisites} rows={2} value={draft.prerequisites} lang={language} dir={dir} aria-invalid={invalidTargets.has('prerequisites') || undefined} aria-describedby={describedBy(`${ids.prerequisites}-count`, errorIds('prerequisites'))} onChange={(e) => text('prerequisites', e.target.value)} />
          <p id={`${ids.prerequisites}-count`} className="hint">
            {count(draft.prerequisites, LIMITS.prerequisites)}
          </p>
        </div>

        <fieldset className="media cover">
          <legend>{t('courses.editor.cover')}</legend>
          {draft.coverMediaId !== null ? (
            <>
              <img className="placing-preview" src={`/api/media/${draft.coverMediaId}`} alt="" />
              <label htmlFor={ids.coverAlt}>{t('courses.editor.coverAltLabel')}</label>
              <input id={ids.coverAlt} value={draft.coverAlt} maxLength={LIMITS.coverAlt} lang={language} dir="auto" required aria-invalid={invalidTargets.has('coverAlt') || undefined} aria-describedby={describedBy(`${ids.coverAlt}-hint`, errorIds('coverAlt'))} onChange={(e) => setDraft((d) => ({ ...d, coverAlt: e.target.value }))} />
              <p id={`${ids.coverAlt}-hint`} className="hint">
                {t('courses.editor.coverAltHint')} {count(draft.coverAlt, LIMITS.coverAlt)}
              </p>
              <button type="button" onClick={() => setDraft((d) => ({ ...d, coverMediaId: null, coverAlt: '' }))}>
                {t('courses.editor.removeCover')}
              </button>
            </>
          ) : (
            <p>{t('courses.editor.noCover')}</p>
          )}
          <label htmlFor={ids.cover}>{draft.coverMediaId !== null ? t('courses.editor.replaceImage') : t('courses.editor.uploadImage')}</label>
          <input
            id={ids.cover}
            aria-invalid={uploadError ? true : undefined}
            aria-describedby={uploadError ? `${ids.cover}-error` : undefined}
            type="file" accept={IMAGE_TYPES.join(',')} onChange={(e) => void uploadCover(e)} disabled={uploading} />
          {uploading && <p role="status">{t('courses.editor.uploading')}</p>}
          {uploadError && (
            <p id={`${ids.cover}-error`} className="banner error" role="alert">
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
        refused={refusedPlacements}
        carried={carriedPlacements}
        editionsFailed={failedEditions.length > 0}
        onRetryEditions={() => setFailedEditions([])}
        onAdd={(info, edition) => {
          setPlacements((p) => ({ ...p, [info.assignmentId]: info }));
          setEditions((e) => ({ ...e, [edition.id]: edition }));
          setDraft((d) => (d.assignmentIds.includes(info.assignmentId) ? d : { ...d, assignmentIds: [...d.assignmentIds, info.assignmentId] }));
          announce(t('courses.editor.placementAdded'));
        }}
        onRemove={(id) => {
          setDraft((d) => outline.removeAssignment(d, id, (a) => chaptersOf(a)?.map((c) => c.chapterId) ?? null));
          announce(t('courses.editor.placementRemoved'));
        }}
      />

      {comparisonError && (
        <div className="banner error" role="alert">
          <p>{t('courses.editor.comparisonFailed')}</p>
          <button type="button" onClick={loadComparison}>
            {t('shell.retry')}
          </button>
        </div>
      )}
      <OutlineSection
        refusedChapters={refusedChapters}
        chaptersComplete={draft.assignmentIds.every((id) => chaptersOf(id) !== null)}
        draft={draft} language={language} chapters={chapterOptions} actions={actions} invalidTargets={invalidTargets} announcement={announcement} focusRequest={focusRequest} errorIds={errorIds} />

      <div className="danger-zone">
        <div className="actions">
          <button type="button" className="danger" onClick={() => setDialog('discard')} disabled={saving || expired} aria-describedby={expired ? 'course-danger-expired' : undefined}>
            {t('courses.editor.discardDraft')}
          </button>
          <button type="button" className="danger" onClick={() => setDialog('delete')} disabled={saving || expired} aria-describedby={expired ? 'course-danger-expired' : undefined}>
            {t('courses.editor.deleteCourse')}
          </button>
        </div>
        {expired && (
          <p id="course-danger-expired" className="field-error">
            {t('courses.editor.dangerExpired')}
          </p>
        )}
        <p className="hint">{t('courses.editor.dangerHint')}</p>
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
        <Dialog title={t('courses.editor.discardTitle')} onCancel={() => setDialog(null)} cancelLabel={t('courses.editor.keepDraft')} actions={[{ label: t('courses.editor.discardAction'), kind: 'danger', onClick: () => void discard() }]}>
          <p>{t('courses.editor.discardText')}</p>
        </Dialog>
      )}
      {dialog === 'submit' && (
        <Dialog
          title={t('courses.editor.submitTitle', { number: server.number })}
          onCancel={() => setDialog(null)}
          cancelLabel={t('courses.editor.keepWriting')}
          actions={[{ label: t('courses.editor.submitAction'), kind: 'primary', disabled: submitting, onClick: () => void submit() }]}
        >
          <p>{t('courses.editor.submitText')}</p>
          <p>{t('courses.editor.submitLessons')}</p>
        </Dialog>
      )}
      {dialog === 'delete' && (
        <Dialog title={t('courses.editor.deleteTitle')} onCancel={() => setDialog(null)} cancelLabel={t('courses.editor.keepCourse')} actions={[{ label: t('courses.editor.deleteCourse'), kind: 'danger', onClick: () => void deleteCourse() }]}>
          <p>{t('courses.editor.deleteText')}</p>
        </Dialog>
      )}
    </div>
  );
}
