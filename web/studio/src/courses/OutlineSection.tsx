// The course outline: modules (title, optional official chapter from the
// selected placements) and their steps (course-authored title, pinned lesson
// revision). Ordering uses buttons only (no drag): "Monter" and "Descendre"
// move a step across module boundaries too, and "Déplacer vers" sends it to
// another module. After each move, focus stays on the moved item's control
// and a polite live region says where it went.
import { useEffect, useId, useState, type FormEvent } from 'react';
import { labelAttrs, type CatalogueLabel, type Lang } from '../catalogue';
import { t, tn } from '../i18n';
import { Dialog } from '../ui/Dialog';
import { LIMITS, findStep, stepMoves, type CourseDraft, type DraftModule, type DraftStep } from './outline';

export interface ChapterOption {
  id: number;
  label: CatalogueLabel;
  retired: boolean;
}

export interface OutlineActions {
  addModule: (title: string) => void;
  renameModule: (moduleId: string, title: string) => void;
  setChapter: (moduleId: string, chapterId: number | null) => void;
  /** Removes the module's official chapter (the flagged one the next save would refuse). */
  clearChapter: (moduleId: string) => void;
  moveModule: (index: number, delta: -1 | 1) => void;
  removeModule: (moduleId: string) => void;
  renameStep: (stepId: string, title: string) => void;
  moveStep: (stepId: string, delta: -1 | 1) => void;
  moveStepTo: (stepId: string, moduleId: string) => void;
  removeStep: (stepId: string) => void;
  /** Opens the lesson picker to add a step to the module, or to change a step's revision. */
  pickLesson: (moduleId: string, step?: DraftStep) => void;
}

const stepState = (s: DraftStep) =>
  s.eligible === null ? null : s.eligible ? t('courses.step.eligible') : t('courses.step.notEligible');

export function OutlineSection({
  draft,
  language,
  chapters,
  actions,
  invalidTargets,
  announcement,
  focusRequest,
  refusedChapters,
  chaptersComplete,
  errorIds = () => undefined,
}: {
  draft: CourseDraft;
  language: Lang;
  /** Official chapters of the selected placements, in assignment order. */
  chapters: ChapterOption[];
  actions: OutlineActions;
  /** Targets of client-side problems (`module:<id>`, `step:<id>`). */
  invalidTargets: Set<string>;
  /** The text, and a counter that makes a repeated text a new node, so screen readers read it again. */
  announcement: { text: string; n: number };
  /** Modules (local IDs) whose chapter the next save will refuse: retired and not inherited unchanged. */
  refusedChapters: Set<string>;
  /** False while a selected placement's chapters are unknown (its edition could not be read). */
  chaptersComplete: boolean;
  /** Element ID to focus after the next render (a moved item's button, a new module's title). */
  focusRequest: { id: string; fallback?: string; n: number } | null;
  /** IDs of the problem messages about a target, for aria-describedby. */
  errorIds?: (target: string) => string | undefined;
}) {
  const heading = useId();
  const newModuleField = 'course-outline-new';
  const [newTitle, setNewTitle] = useState('');
  const [confirmRemove, setConfirmRemove] = useState<DraftModule | null>(null);
  const dir = language === 'ar' ? 'rtl' : 'ltr';
  const totalSteps = draft.modules.reduce((n, m) => n + m.steps.length, 0);

  useEffect(() => {
    if (!focusRequest) return;
    const el = document.getElementById(focusRequest.id) as HTMLButtonElement | null;
    if (el && !el.disabled) el.focus();
    else document.getElementById(focusRequest.fallback ?? '')?.focus();
  }, [focusRequest]);

  const add = (event: FormEvent) => {
    event.preventDefault();
    const title = newTitle.trim();
    if (!title) return;
    actions.addModule(title);
    setNewTitle('');
  };

  return (
    <section className="panel course-section" aria-labelledby={heading}>
      <h2 id={heading}>{t('courses.outline.heading')}</h2>
      <p className="hint">
        {t('courses.outline.intro')}{' '}
        {t('courses.outline.counts', {
          modules: tn('courses.outline.modulesCount', draft.modules.length, { max: LIMITS.modules }),
          steps: tn('courses.outline.stepsCount', totalSteps, { max: LIMITS.steps }),
        })}
      </p>
      <div className="visually-hidden" role="status" aria-live="polite">
        {announcement.text && <p key={announcement.n}>{announcement.text}</p>}
      </div>
      {draft.modules.length === 0 && <p>{t('courses.outline.empty')}</p>}
      <ol className="module-list">
        {draft.modules.map((m, i) => (
          <ModuleItem
            key={m.localId}
            module={m}
            index={i}
            count={draft.modules.length}
            draft={draft}
            language={language}
            dir={dir}
            chapters={chapters}
            actions={actions}
            invalidTargets={invalidTargets}
            chapterRefused={refusedChapters.has(m.localId)}
            chaptersComplete={chaptersComplete}
            errorIds={errorIds}
            onRemove={() => (m.steps.length > 0 ? setConfirmRemove(m) : actions.removeModule(m.localId))}
          />
        ))}
      </ol>
      <form className="add-module" onSubmit={add}>
        <div className="fields">
          <label htmlFor={newModuleField}>{t('courses.outline.newModule')}</label>
          <input id={newModuleField} value={newTitle} maxLength={LIMITS.moduleTitle} lang={language} dir={dir} onChange={(e) => setNewTitle(e.target.value)} />
        </div>
        <button type="submit" disabled={newTitle.trim() === '' || draft.modules.length >= LIMITS.modules}>
          {t('courses.outline.addModule')}
        </button>
      </form>
      {confirmRemove && (
        <Dialog
          title={t('courses.outline.removeTitle', { title: confirmRemove.title || t('courses.untitled') })}
          onCancel={() => setConfirmRemove(null)}
          cancelLabel={t('courses.outline.keepModule')}
          actions={[
            {
              label: t('courses.outline.removeAction'),
              kind: 'danger',
              onClick: () => {
                actions.removeModule(confirmRemove.localId);
                setConfirmRemove(null);
              },
            },
          ]}
        >
          <p>{tn('courses.outline.removeText', confirmRemove.steps.length)}</p>
        </Dialog>
      )}
    </section>
  );
}

function ModuleItem({
  module: m,
  index,
  count,
  draft,
  language,
  dir,
  chapters,
  actions,
  invalidTargets,
  chapterRefused,
  chaptersComplete,
  errorIds,
  onRemove,
}: {
  module: DraftModule;
  index: number;
  count: number;
  draft: CourseDraft;
  language: Lang;
  dir: 'rtl' | 'ltr';
  chapters: ChapterOption[];
  actions: OutlineActions;
  invalidTargets: Set<string>;
  chapterRefused: boolean;
  chaptersComplete: boolean;
  errorIds: (target: string) => string | undefined;
  onRemove: () => void;
}) {
  const titleId = `module-title-${m.localId}`;
  const chapterId = useId();
  const chapterError = useId();
  const n = index + 1;
  const name = m.title.trim() || t('courses.untitled');
  // Retired chapters are offered only when the module already has them.
  const options = chapters.filter((c) => !c.retired || c.id === m.chapterId);
  const missingChapter = m.chapterId !== null && !chapters.some((c) => c.id === m.chapterId);
  // Only claimed outside every placement when every placement's chapters are known.
  const unknownChapter = missingChapter && chaptersComplete;
  return (
    <li className="module" aria-labelledby={`module-heading-${m.localId}`}>
      <h3 id={`module-heading-${m.localId}`}>
        {t('courses.outline.moduleHeading', { index: n })}{' '}
        {m.title.trim() ? (
          <span lang={language} dir={dir}>
            {name}
          </span>
        ) : (
          name
        )}
      </h3>
      <div className="fields">
        <label htmlFor={titleId}>{t('courses.outline.moduleTitle', { index: n })}</label>
        <input
          id={titleId}
          value={m.title}
          maxLength={LIMITS.moduleTitle}
          lang={language}
          dir={dir}
          aria-invalid={invalidTargets.has(`module:${m.localId}`) || undefined}
          aria-describedby={errorIds(`module:${m.localId}`)}
          onChange={(e) => actions.renameModule(m.localId, e.target.value)}
        />
        <label htmlFor={chapterId}>{t('courses.outline.chapter')}</label>
        <select
          id={chapterId}
          aria-invalid={unknownChapter || chapterRefused || undefined}
          aria-describedby={unknownChapter || chapterRefused ? chapterError : undefined}
          value={m.chapterId ?? ''} onChange={(e) => actions.setChapter(m.localId, e.target.value ? Number(e.target.value) : null)}>
          <option value="">{t('courses.outline.noChapter')}</option>
          {missingChapter && (
            <option value={m.chapterId!}>
              {t('courses.outline.chapterNumber', { id: m.chapterId! })}
              {unknownChapter ? t('courses.outline.outsidePlacements') : ''}
            </option>
          )}
          {options.map((c) => (
            <option key={c.id} value={c.id} {...labelAttrs(c.label)}>
              {c.label.text}
              {c.retired ? t('courses.outline.retired') : ''}
            </option>
          ))}
        </select>
        {chapters.length === 0 && <p className="hint">{t('courses.outline.chaptersHint')}</p>}
        {unknownChapter && (
          <p id={chapterError} className="field-error">
            {t('courses.outline.unknownChapter')}
          </p>
        )}
        {chapterRefused && !unknownChapter && (
          <div className="field-error-block">
            <p id={chapterError} className="field-error">
              {t('courses.outline.chapterRefused')}
            </p>
            <button type="button" onClick={() => actions.clearChapter(m.localId)} aria-label={t('courses.outline.clearChapterLabel', { index: n, name })}>
              {t('courses.outline.clearChapter')}
            </button>
          </div>
        )}
      </div>
      <div className="actions" role="group" aria-label={t('courses.outline.moduleActions', { index: n })}>
        <button id={`module-up-${m.localId}`} type="button" disabled={index === 0} onClick={() => actions.moveModule(index, -1)} aria-label={t('courses.outline.moveModuleUp', { index: n, name })}>
          {t('courses.outline.up')}
        </button>
        <button id={`module-down-${m.localId}`} type="button" disabled={index === count - 1} onClick={() => actions.moveModule(index, 1)} aria-label={t('courses.outline.moveModuleDown', { index: n, name })}>
          {t('courses.outline.down')}
        </button>
        <button type="button" className="danger" onClick={onRemove} aria-label={t('courses.outline.removeModuleLabel', { index: n, name })}>
          {t('courses.outline.removeModule')}
        </button>
      </div>
      {m.steps.length === 0 ? (
        <p className="hint">{t('courses.outline.noSteps')}</p>
      ) : (
        <ol className="step-list" aria-label={t('courses.outline.steps', { index: n })}>
          {m.steps.map((s, j) => (
            <StepItem key={s.localId} step={s} index={j} moduleIndex={index} draft={draft} language={language} dir={dir} actions={actions} invalid={invalidTargets.has(`step:${s.localId}`)} describedBy={errorIds(`step:${s.localId}`)} />
          ))}
        </ol>
      )}
      <button type="button" onClick={() => actions.pickLesson(m.localId)} aria-label={t('courses.outline.addLessonLabel', { index: n, name })}>
        {t('courses.outline.addLesson')}
      </button>
    </li>
  );
}

function StepItem({
  step: s,
  index,
  moduleIndex,
  draft,
  language,
  dir,
  actions,
  invalid,
  describedBy,
}: {
  step: DraftStep;
  index: number;
  moduleIndex: number;
  draft: CourseDraft;
  language: Lang;
  dir: 'rtl' | 'ltr';
  actions: OutlineActions;
  invalid: boolean;
  describedBy?: string;
}) {
  const titleId = `step-title-${s.localId}`;
  const moveId = useId();
  const moves = stepMoves(draft, s.localId);
  const at = findStep(draft, s.localId);
  const where = { step: index + 1, module: moduleIndex + 1 };
  const name = s.title.trim() || t('courses.untitled');
  const others = draft.modules.filter((_, i) => i !== at?.module);
  const state = stepState(s);
  return (
    <li className="step">
      <div className="fields">
        <label htmlFor={titleId}>{t('courses.outline.stepTitle', where)}</label>
        <input id={titleId} value={s.title} maxLength={LIMITS.stepTitle} lang={language} dir={dir} aria-invalid={invalid || undefined} aria-describedby={describedBy} onChange={(e) => actions.renameStep(s.localId, e.target.value)} />
      </div>
      <p className="lesson-context">
        {s.lessonTitle ? (
          <>
            {t('courses.outline.lesson')} {t('courses.quoteOpen')}
            <span lang={language} dir={dir}>
              {s.lessonTitle}
            </span>
            {t('courses.quoteClose')}
          </>
        ) : (
          t('courses.lessonNumber', { id: s.lessonId })
        )}
        {s.revisionNumber !== null ? ` · ${t('courses.outline.revision', { number: s.revisionNumber })}` : ''}
        {s.ownLesson === null ? '' : ` · ${s.ownLesson ? t('courses.yourLesson') : t('courses.outline.otherAuthor')}`}
      </p>
      {state && <p className={s.eligible ? 'hint' : 'step-warning'}>{state}</p>}
      <div className="actions" role="group" aria-label={t('courses.outline.stepActions', { ...where, name })}>
        <button id={`step-up-${s.localId}`} type="button" disabled={!moves.up} onClick={() => actions.moveStep(s.localId, -1)} aria-label={t('courses.outline.moveStepUp', { ...where, name })}>
          {t('courses.outline.up')}
        </button>
        <button id={`step-down-${s.localId}`} type="button" disabled={!moves.down} onClick={() => actions.moveStep(s.localId, 1)} aria-label={t('courses.outline.moveStepDown', { ...where, name })}>
          {t('courses.outline.down')}
        </button>
        {others.length > 0 && (
          <span className="move-to">
            <label htmlFor={moveId} className="visually-hidden">
              {t('courses.outline.moveStepTo', where)}
            </label>
            <select
              id={moveId}
              value=""
              onChange={(e) => {
                if (e.target.value) actions.moveStepTo(s.localId, e.target.value);
              }}
            >
              <option value="">{t('courses.outline.moveTo')}</option>
              {draft.modules.map((m, i) =>
                i === at?.module ? null : (
                  <option key={m.localId} value={m.localId}>
                    {t('courses.outline.moduleOption', { index: i + 1, title: m.title.trim() || t('courses.untitled') })}
                  </option>
                ),
              )}
            </select>
          </span>
        )}
        <button type="button" onClick={() => actions.pickLesson(draft.modules[moduleIndex].localId, s)} aria-label={t('courses.outline.changeRevisionLabel', { ...where, name })}>
          {t('courses.outline.changeRevision')}
        </button>
        <button type="button" className="danger" onClick={() => actions.removeStep(s.localId)} aria-label={t('courses.outline.removeStepLabel', { ...where, name })}>
          {t('courses.outline.removeStep')}
        </button>
      </div>
    </li>
  );
}

