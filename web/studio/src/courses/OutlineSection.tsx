// The course outline: modules (title, optional official chapter from the
// selected placements) and their steps (course-authored title, pinned lesson
// revision). Ordering uses buttons only (no drag): "Monter" and "Descendre"
// move a step across module boundaries too, and "Déplacer vers" sends it to
// another module. After each move, focus stays on the moved item's control
// and a polite live region says where it went.
import { useEffect, useId, useState, type FormEvent } from 'react';
import { labelAttrs, type CatalogueLabel, type Lang } from '../catalogue';
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
  s.eligible === null ? null : s.eligible ? 'Révision publiable dans un cours' : 'Révision pas encore publiable : elle devra être approuvée et publiée avant la soumission du cours';

export function OutlineSection({
  draft,
  language,
  chapters,
  actions,
  invalidTargets,
  announcement,
  focusRequest,
}: {
  draft: CourseDraft;
  language: Lang;
  /** Official chapters of the selected placements, in assignment order. */
  chapters: ChapterOption[];
  actions: OutlineActions;
  /** Targets of client-side problems (`module:<id>`, `step:<id>`). */
  invalidTargets: Set<string>;
  announcement: string;
  /** Element ID to focus after the next render (a moved item's button, a new module's title). */
  focusRequest: { id: string; fallback?: string; n: number } | null;
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
      <h2 id={heading}>Plan du cours</h2>
      <p className="hint">
        Un module regroupe des étapes ; chaque étape épingle une révision précise d’une leçon. Les titres des modules et des étapes appartiennent au cours et sont relus avec lui. {draft.modules.length} module{draft.modules.length > 1 ? 's' : ''} sur {LIMITS.modules}, {totalSteps} étape{totalSteps > 1 ? 's' : ''} sur {LIMITS.steps}.
      </p>
      <p className="visually-hidden" role="status" aria-live="polite">
        {announcement}
      </p>
      {draft.modules.length === 0 && <p>Le cours n’a pas encore de module. Ajoutez-en un ci-dessous.</p>}
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
            onRemove={() => (m.steps.length > 0 ? setConfirmRemove(m) : actions.removeModule(m.localId))}
          />
        ))}
      </ol>
      <form className="add-module" onSubmit={add}>
        <div className="fields">
          <label htmlFor={newModuleField}>Titre du nouveau module</label>
          <input id={newModuleField} value={newTitle} maxLength={LIMITS.moduleTitle} lang={language} dir={dir} onChange={(e) => setNewTitle(e.target.value)} />
        </div>
        <button type="submit" disabled={newTitle.trim() === '' || draft.modules.length >= LIMITS.modules}>
          Ajouter le module
        </button>
      </form>
      {confirmRemove && (
        <Dialog
          title={`Supprimer le module « ${confirmRemove.title || 'sans titre'} » ?`}
          onCancel={() => setConfirmRemove(null)}
          cancelLabel="Garder le module"
          actions={[
            {
              label: 'Supprimer le module et ses étapes',
              kind: 'danger',
              onClick: () => {
                actions.removeModule(confirmRemove.localId);
                setConfirmRemove(null);
              },
            },
          ]}
        >
          <p>
            Ses {confirmRemove.steps.length} étape{confirmRemove.steps.length > 1 ? 's' : ''} seront retirées du plan. Les leçons elles-mêmes ne sont pas supprimées. Rien n’est modifié sur le serveur avant l’enregistrement.
          </p>
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
  onRemove: () => void;
}) {
  const titleId = `module-title-${m.localId}`;
  const chapterId = useId();
  const n = index + 1;
  const name = m.title.trim() || 'sans titre';
  // Retired chapters are offered only when the module already has them.
  const options = chapters.filter((c) => !c.retired || c.id === m.chapterId);
  const unknownChapter = m.chapterId !== null && !chapters.some((c) => c.id === m.chapterId);
  return (
    <li className="module" aria-labelledby={`module-heading-${m.localId}`}>
      <h3 id={`module-heading-${m.localId}`}>
        Module {n} : <span lang={language} dir={dir}>{name}</span>
      </h3>
      <div className="fields">
        <label htmlFor={titleId}>Titre du module {n}</label>
        <input
          id={titleId}
          value={m.title}
          maxLength={LIMITS.moduleTitle}
          lang={language}
          dir={dir}
          aria-invalid={invalidTargets.has(`module:${m.localId}`) || undefined}
          onChange={(e) => actions.renameModule(m.localId, e.target.value)}
        />
        <label htmlFor={chapterId}>Chapitre officiel (facultatif)</label>
        <select id={chapterId} value={m.chapterId ?? ''} onChange={(e) => actions.setChapter(m.localId, e.target.value ? Number(e.target.value) : null)}>
          <option value="">Aucun chapitre officiel</option>
          {unknownChapter && <option value={m.chapterId!}>Chapitre n° {m.chapterId} (hors des classements choisis)</option>}
          {options.map((c) => (
            <option key={c.id} value={c.id} {...labelAttrs(c.label)}>
              {c.label.text}
              {c.retired ? ' (retiré)' : ''}
            </option>
          ))}
        </select>
        {chapters.length === 0 && <p className="hint">Les chapitres officiels proposés sont ceux des classements choisis : ajoutez d’abord un classement.</p>}
        {unknownChapter && <p className="field-error">Ce chapitre n’appartient à aucun classement choisi : le serveur refusera l’enregistrement. Choisissez-en un autre ou aucun.</p>}
      </div>
      <div className="actions" role="group" aria-label={`Ordre et suppression du module ${n}`}>
        <button id={`module-up-${m.localId}`} type="button" disabled={index === 0} onClick={() => actions.moveModule(index, -1)} aria-label={`Monter le module ${n} : ${name}`}>
          Monter
        </button>
        <button id={`module-down-${m.localId}`} type="button" disabled={index === count - 1} onClick={() => actions.moveModule(index, 1)} aria-label={`Descendre le module ${n} : ${name}`}>
          Descendre
        </button>
        <button type="button" className="danger" onClick={onRemove} aria-label={`Supprimer le module ${n} : ${name}`}>
          Supprimer le module
        </button>
      </div>
      {m.steps.length === 0 ? (
        <p className="hint">Aucune étape dans ce module.</p>
      ) : (
        <ol className="step-list" aria-label={`Étapes du module ${n}`}>
          {m.steps.map((s, j) => (
            <StepItem key={s.localId} step={s} index={j} moduleIndex={index} draft={draft} language={language} dir={dir} actions={actions} invalid={invalidTargets.has(`step:${s.localId}`)} />
          ))}
        </ol>
      )}
      <button type="button" onClick={() => actions.pickLesson(m.localId)} aria-label={`Ajouter une leçon au module ${n} : ${name}`}>
        Ajouter une leçon
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
}: {
  step: DraftStep;
  index: number;
  moduleIndex: number;
  draft: CourseDraft;
  language: Lang;
  dir: 'rtl' | 'ltr';
  actions: OutlineActions;
  invalid: boolean;
}) {
  const titleId = `step-title-${s.localId}`;
  const moveId = useId();
  const moves = stepMoves(draft, s.localId);
  const at = findStep(draft, s.localId);
  const label = `étape ${index + 1} du module ${moduleIndex + 1}`;
  const name = s.title.trim() || 'sans titre';
  const others = draft.modules.filter((_, i) => i !== at?.module);
  const state = stepState(s);
  return (
    <li className="step">
      <div className="fields">
        <label htmlFor={titleId}>Titre de l’{label}</label>
        <input id={titleId} value={s.title} maxLength={LIMITS.stepTitle} lang={language} dir={dir} aria-invalid={invalid || undefined} onChange={(e) => actions.renameStep(s.localId, e.target.value)} />
      </div>
      <p className="lesson-context">
        Leçon{' '}
        {s.lessonTitle ? (
          <>
            « <span lang={language} dir={dir}>{s.lessonTitle}</span> »
          </>
        ) : (
          `n° ${s.lessonId}`
        )}
        {s.revisionNumber !== null ? ` · révision ${s.revisionNumber}` : ''}
        {s.ownLesson === null ? '' : s.ownLesson ? ' · votre leçon' : ' · leçon d’un autre auteur'}
      </p>
      {state && <p className={s.eligible ? 'hint' : 'step-warning'}>{state}</p>}
      <div className="actions" role="group" aria-label={`Actions de l’${label} : ${name}`}>
        <button id={`step-up-${s.localId}`} type="button" disabled={!moves.up} onClick={() => actions.moveStep(s.localId, -1)} aria-label={`Monter l’${label} : ${name}`}>
          Monter
        </button>
        <button id={`step-down-${s.localId}`} type="button" disabled={!moves.down} onClick={() => actions.moveStep(s.localId, 1)} aria-label={`Descendre l’${label} : ${name}`}>
          Descendre
        </button>
        {others.length > 0 && (
          <span className="move-to">
            <label htmlFor={moveId} className="visually-hidden">
              Déplacer l’{label} vers le module
            </label>
            <select
              id={moveId}
              value=""
              onChange={(e) => {
                if (e.target.value) actions.moveStepTo(s.localId, e.target.value);
              }}
            >
              <option value="">Déplacer vers…</option>
              {draft.modules.map((m, i) =>
                i === at?.module ? null : (
                  <option key={m.localId} value={m.localId}>
                    Module {i + 1} : {m.title.trim() || 'sans titre'}
                  </option>
                ),
              )}
            </select>
          </span>
        )}
        <button type="button" onClick={() => actions.pickLesson(draft.modules[moduleIndex].localId, s)} aria-label={`Changer la révision de l’${label} : ${name}`}>
          Changer la révision
        </button>
        <button type="button" className="danger" onClick={() => actions.removeStep(s.localId)} aria-label={`Retirer l’${label} : ${name}`}>
          Retirer
        </button>
      </div>
    </li>
  );
}

