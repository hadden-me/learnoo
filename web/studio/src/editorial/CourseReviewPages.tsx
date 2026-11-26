// Course review and publication (LMS-B06). The review page shows the full
// submitted snapshot of a course revision (GET /api/review/course-revisions):
// overview, curriculum placement, the outline with each step's change from
// the published base (replaced, moved, added, unchanged, and removed steps),
// the content diff of replaced lessons and every pinned lesson through the
// canonical preview (sent only while the lesson revision is publishable),
// and earlier feedback. The decision form follows the server's eligibility:
// a reviewer who authored the course or an added or replaced lesson sees
// why they cannot decide. The publication page shows the same snapshot with
// the queue's facts (expected published version, staleness, ineligible
// steps) and publishes with the expected pointer.
import { useEffect, useMemo, useRef, useState } from 'react';
import { isApiError } from '../api/client';
import { courseApi, type Edition, type Level } from '../api/courses';
import {
  courseReviewApi,
  ineligibleSteps,
  reviewerReasons,
  type CoursePointerChange,
  type CoursePublishItem,
  type CourseReview,
  type ReviewedRevision,
  type ReviewerProblem,
  type StepChange,
  type StepRef,
} from '../api/courseReview';
import type { Spaces } from '../spaces';
import { describeCourseWorkflowError } from '../api/courseReviewMessages';
import { catalogueLabel, CatalogueName } from '../catalogue';
import { setFlash, useStudio } from '../context';
import { CHANGE_LABELS, changeSummary, describeBlocked, describeOwnProblem, diffSides, partialDocument } from '../courses/workflow';
import { paths } from '../editorialRoutes';
import { LANGUAGE_LABELS, formatDateTime } from '../labels';
import { navigate } from '../router';
import { Dialog } from '../ui/Dialog';
import { Link } from '../ui/Link';
import { CanonicalPreview, ErrorPage, LoadingPage, NoteField, Quote, SubjectName, dirOf, useLoad, useSubjects } from './common';

export function useCourseReview() {
  const { api } = useStudio();
  return useMemo(() => courseReviewApi(api), [api]);
}

/* ---------- placement ---------- */

/** "Level › Section › Edition" for each placement, read from the catalogue (codes or IDs when it cannot be read). */
function Placement({ review }: { review: CourseReview }) {
  const { api } = useStudio();
  const courses = useMemo(() => courseApi(api), [api]);
  const language = review.revision.language;
  const [levels, setLevels] = useState<Level[]>([]);
  const [editions, setEditions] = useState<Record<number, Edition>>({});
  useEffect(() => {
    let cancelled = false;
    courses.levels().then((l) => !cancelled && setLevels(l), () => undefined);
    for (const a of review.revision.assignments)
      courses.edition(a.editionId).then((e) => !cancelled && setEditions((x) => ({ ...x, [e.id]: e })), () => undefined);
    return () => {
      cancelled = true;
    };
  }, [courses, review.revision.assignments]);
  if (review.revision.assignments.length === 0) return <p>Aucun classement.</p>;
  return (
    <ul>
      {review.revision.assignments.map((a) => {
        const e = editions[a.editionId];
        const section = e ? levels.flatMap((l) => l.sections.map((s) => ({ l, s }))).find((x) => x.s.id === e.sectionId) : undefined;
        return (
          <li key={a.assignmentId}>
            {section && (
              <>
                <CatalogueName label={catalogueLabel(section.l.code, section.l.nameFr, section.l.nameAr, language)} /> ›{' '}
                <CatalogueName label={catalogueLabel(section.s.code, section.s.nameFr, section.s.nameAr, language)} /> ›{' '}
              </>
            )}
            {e ? <CatalogueName label={catalogueLabel(e.code, e.labelFr, e.labelAr, language)} /> : `édition n° ${a.editionId}`}
            {a.retired && ' (retiré : refusé à la publication)'}
          </li>
        );
      })}
    </ul>
  );
}

/* ---------- outline and changes ---------- */

const FIELD_NAMES: Record<string, string> = { title: 'Titre', objective: 'Objectif', language: 'Langue', status: 'Statut', schemaVersion: 'Version du format' };

function text(value: unknown): string {
  if (value === null || value === undefined) return '(vide)';
  return typeof value === 'string' ? value : JSON.stringify(value);
}

export const UNAVAILABLE_CONTENT = 'Contenu indisponible (leçon retirée ou dépubliée)';

/**
 * A replaced step's content diff: changed fields, then removed and added
 * blocks through the canonical preview. Only what the server sent is shown;
 * a side it marked unavailable says so instead (diffSides).
 */
function StepDiff({ step, language }: { step: StepChange; language: 'fr' | 'ar' }) {
  const sides = diffSides(step.diff, step.eligible);
  const [shown, setShown] = useState(false);
  const cell = (value: unknown, visible: boolean) => (visible ? text(value) : UNAVAILABLE_CONTENT);
  const hasBlocks = (sides.removed?.length ?? 0) > 0 || (sides.added?.length ?? 0) > 0;
  return (
    <div className="step-diff">
      {sides.fields.length > 0 && (
        <table>
          <caption>Champs modifiés</caption>
          <thead>
            <tr>
              <th scope="col">Champ</th>
              <th scope="col">Révision {step.previousRevisionNumber}</th>
              <th scope="col">Révision {step.revisionNumber}</th>
            </tr>
          </thead>
          <tbody>
            {sides.fields.map((f) => (
              <tr key={f.name}>
                <th scope="row">{FIELD_NAMES[f.name] ?? f.name}</th>
                <td dir="auto">{cell(f.old, f.oldShown)}</td>
                <td dir="auto">{cell(f.new, f.newShown)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {sides.removed === null && <p className="step-warning">Révision {step.previousRevisionNumber} : {UNAVAILABLE_CONTENT}.</p>}
      {sides.added === null && <p className="step-warning">Révision {step.revisionNumber} : {UNAVAILABLE_CONTENT}.</p>}
      {sides.removed !== null && sides.added !== null && (
        <p>
          Blocs du contenu : {sides.removed.length} retiré{sides.removed.length > 1 ? 's' : ''}, {sides.added.length} ajouté{sides.added.length > 1 ? 's' : ''}, {sides.equal} inchangé
          {sides.equal > 1 ? 's' : ''}.
        </p>
      )}
      {hasBlocks && (
        <button type="button" aria-expanded={shown} onClick={() => setShown((v) => !v)}>
          {shown ? 'Masquer les blocs modifiés' : 'Afficher les blocs modifiés'}
        </button>
      )}
      {shown && (
        <div className="diff-sides">
          {sides.removed && sides.removed.length > 0 && (
            <section className="diff-removed" aria-label={`Blocs retirés (révision ${step.previousRevisionNumber})`}>
              <h5>Retiré (révision {step.previousRevisionNumber})</h5>
              <CanonicalPreview content={partialDocument(step.content, sides.removed, language)} testId={`diff-removed-${step.lessonId}`} mediaRoute="review" />
            </section>
          )}
          {sides.added && sides.added.length > 0 && (
            <section className="diff-added" aria-label={`Blocs ajoutés (révision ${step.revisionNumber})`}>
              <h5>Ajouté (révision {step.revisionNumber})</h5>
              <CanonicalPreview content={partialDocument(step.content, sides.added, language)} testId={`diff-added-${step.lessonId}`} mediaRoute="review" />
            </section>
          )}
        </div>
      )}
    </div>
  );
}

function StepRow({ step, language, moduleTitle }: { step: StepChange; language: 'fr' | 'ar'; moduleTitle: (key: string | null) => string }) {
  const [preview, setPreview] = useState(false);
  const changed = step.change !== 'unchanged';
  return (
    <li className={`review-step change-${step.change}`} data-change={step.change}>
      <h4 lang={language} dir={dirOf(language)}>
        {step.title}
      </h4>
      <p className="lesson-meta">
        <span className={`state ${changed ? 'state-changes' : ''}`}>{CHANGE_LABELS[step.change]}</span>
        {!step.eligible && <span className="state state-withdrawn">Leçon indisponible : révision non publiable</span>}
      </p>
      <p className="lesson-context">
        Leçon n° {step.lessonId} ·{' '}
        {step.change === 'replaced' ? (
          <>
            révision {step.previousRevisionNumber} → révision {step.revisionNumber}
          </>
        ) : (
          <>révision {step.revisionNumber} épinglée</>
        )}
        {step.structureChanged && step.previousTitle !== null && (
          <>
            {' '}
            · auparavant « {step.previousTitle} », {moduleTitle(step.previousModuleKey)}, position {step.previousPosition}
          </>
        )}
      </p>
      {step.change === 'replaced' && step.diff && <StepDiff step={step} language={language} />}
      {step.content === null || step.content === undefined ? (
        <p className="hint">{UNAVAILABLE_CONTENT} : la leçon ne peut pas être lue ici.</p>
      ) : (
        <>
          <button type="button" aria-expanded={preview} onClick={() => setPreview((v) => !v)}>
            {preview ? 'Masquer la leçon' : `Lire la révision ${step.revisionNumber} de la leçon`}
          </button>
          {preview && <CanonicalPreview content={step.content} testId={`step-preview-${step.lessonId}`} mediaRoute="review" />}
        </>
      )}
    </li>
  );
}

/** The submitted snapshot: overview, placement, outline with the change list, removed steps. */
export function CourseSnapshot({ review }: { review: CourseReview }) {
  const r = review.revision;
  const dir = dirOf(r.language);
  const subjects = useSubjects();
  const byModule = new Map<string, StepChange[]>();
  for (const s of review.steps) byModule.set(s.moduleKey, [...(byModule.get(s.moduleKey) ?? []), s]);
  const moduleTitle = (key: string | null) => {
    const m = r.modules.find((x) => x.key === key);
    return m ? `module « ${m.title} »` : 'un module retiré';
  };
  return (
    <>
      <dl className="facts">
        <div>
          <dt>Matière</dt>
          <dd>
            <SubjectName subjects={subjects} id={r.subjectId} />
          </dd>
        </div>
        <div>
          <dt>Langue</dt>
          <dd>{LANGUAGE_LABELS[r.language]}</dd>
        </div>
        <div>
          <dt>Version</dt>
          <dd>
            n° {r.number} · {r.kind === 'amendment' ? 'amendement (remplacement de révisions de leçons)' : 'version ordinaire'}
            {r.submittedAt && `, soumise le ${formatDateTime(r.submittedAt)}`}
          </dd>
        </div>
      </dl>
      <section className="panel" aria-labelledby="overview-heading">
        <h2 id="overview-heading">Présentation {review.metadataChanged ? '(modifiée depuis la version publiée)' : '(inchangée)'}</h2>
        <dl className="overview">
          <dt>Description courte</dt>
          <dd lang={r.language} dir={dir} className="pre-line">
            {r.summary || '(vide)'}
          </dd>
          <dt>Objectifs d’apprentissage</dt>
          <dd lang={r.language} dir={dir} className="pre-line">
            {r.objectives || '(vide)'}
          </dd>
          <dt>Prérequis</dt>
          <dd lang={r.language} dir={dir} className="pre-line">
            {r.prerequisites || '(aucun)'}
          </dd>
          {r.coverMediaId !== null && (
            <>
              <dt>Couverture</dt>
              <dd>Image n° {r.coverMediaId}, texte alternatif : « {r.coverAlt} »</dd>
            </>
          )}
        </dl>
        <h3>Classement dans le programme</h3>
        <Placement review={review} />
      </section>
      <section className="panel" aria-labelledby="outline-heading">
        <h2 id="outline-heading">Plan et changements</h2>
        <p role="note">
          {r.compareRevisionId === null
            ? 'Première version du cours : toutes les étapes sont nouvelles.'
            : `Comparé à la version publiée de référence : ${changeSummary(review.steps, review.removedSteps.length)}.`}{' '}
          La relecture du cours ne remplace pas celle des leçons : chaque révision épinglée a déjà été approuvée.
        </p>
        <ol className="review-modules">
          {r.modules.map((m, i) => (
            <li key={m.key}>
              <h3 lang={r.language} dir={dir}>
                Module {i + 1} : {m.title}
              </h3>
              <ol className="review-steps">
                {(byModule.get(m.key) ?? []).map((s) => (
                  <StepRow key={s.lessonId} step={s} language={r.language} moduleTitle={moduleTitle} />
                ))}
              </ol>
            </li>
          ))}
        </ol>
        {review.removedSteps.length > 0 && (
          <>
            <h3>Étapes retirées du cours</h3>
            <ul>
              {review.removedSteps.map((s) => (
                <li key={s.lessonId}>
                  « {s.title} » (leçon n° {s.lessonId})
                </li>
              ))}
            </ul>
          </>
        )}
      </section>
    </>
  );
}

function EarlierFeedback({ review }: { review: CourseReview }) {
  if (review.earlierDecisions.length === 0) return null;
  return (
    <section className="panel" aria-labelledby="earlier-heading">
      <h2 id="earlier-heading">Décisions précédentes sur ce cours</h2>
      <ul className="history">
        {review.earlierDecisions.map((d) => (
          <li key={d.revisionId}>
            <p>
              Version {d.number} : {d.action === 'approve' ? 'approuvée' : 'modifications demandées'} par {d.reviewerName}, le {formatDateTime(d.decidedAt)}
            </p>
            {d.comment && <Quote>{d.comment}</Quote>}
          </li>
        ))}
      </ul>
    </section>
  );
}

/* ---------- review page ---------- */

/** The revision's state on the read-only review page, from its status and withdrawal (publishers and administrators read any submitted revision). */
export function reviewStatus(r: Pick<ReviewedRevision, 'status' | 'withdrawnAt'>): { label: string; tone: string; inReview: boolean } {
  if (r.withdrawnAt) return { label: 'Retirée par un administrateur', tone: 'state-withdrawn', inReview: false };
  switch (r.status) {
    case 'in_review':
      return { label: 'En relecture', tone: 'state-review', inReview: true };
    case 'approved':
      return { label: 'Approuvée', tone: 'state-approved', inReview: false };
    case 'changes_requested':
      return { label: 'Modifications demandées', tone: 'state-changes', inReview: false };
    default:
      return { label: 'Brouillon', tone: '', inReview: false };
  }
}

/** The 404 text: reviewers see only revisions in review of their subjects; publishers and administrators any submitted revision. */
export function notFoundText(spaces: Spaces): string {
  if (spaces.publisher || spaces.administrator)
    return spaces.reviewer
      ? 'Cette version de cours est introuvable : elle n’existe pas ou n’a jamais été soumise (un brouillon ne se relit pas).'
      : 'Cette version de cours est introuvable : elle n’existe pas ou n’a jamais été soumise. Un brouillon n’est visible que de son auteur.';
  return 'Cette version de cours n’est pas en relecture dans l’une de vos matières : elle n’existe pas, a déjà été décidée, ou relève d’une matière que vous ne relisez pas.';
}

function ReadOnlyStatus({ revision, label }: { revision: ReviewedRevision; label: string }) {
  const d = revision.review;
  return (
    <section className="status-card" aria-labelledby="readonly-heading">
      <h2 id="readonly-heading">Version {label.toLowerCase()}</h2>
      <p>
        {revision.withdrawnAt
          ? `Un administrateur a retiré cette version le ${formatDateTime(revision.withdrawnAt)} : elle ne sera ni relue, ni publiée, ni restaurée.`
          : 'Cette version n’est plus en relecture : vous la lisez sans pouvoir en décider.'}
      </p>
      {d && (
        <p>
          {d.action === 'approve' ? 'Approuvée' : 'Modifications demandées'} par {d.reviewerName}, le {formatDateTime(d.decidedAt)}.
        </p>
      )}
    </section>
  );
}

export function CourseReviewPage({ revisionId }: { revisionId: number }) {
  const cr = useCourseReview();
  const { spaces } = useStudio();
  const [loaded] = useLoad(() => cr.review(revisionId), [cr, revisionId]);
  if (loaded.state === 'loading') return <LoadingPage label="Chargement de la version du cours…" />;
  if (loaded.state === 'error')
    return (
      <ErrorPage
        title="Version de cours indisponible"
        message={loaded.notFound ? notFoundText(spaces) : loaded.message}
        back={spaces.reviewer ? { to: paths.reviewQueue, label: 'Revenir à la file de relecture' } : spaces.administrator ? { to: paths.adminCourses, label: 'Revenir à l’administration des cours' } : { to: paths.publishQueue, label: 'Revenir aux contenus à publier' }}
      />
    );
  const review = loaded.data;
  const r = review.revision;
  const blocked = r.blocked ? describeBlocked(r.blocked) : null;
  const status = reviewStatus(r);
  return (
    <main className="page" id="main">
      <p className="breadcrumb">
        <Link to={paths.reviewQueue}>Relecture</Link>
      </p>
      <h1 tabIndex={-1} lang={r.language} dir={dirOf(r.language)}>
        {r.title}
      </h1>
      <p className="lesson-meta">
        <span className="state kind-label">{r.kind === 'amendment' ? 'Cours · amendement' : 'Cours'}</span>
        <span className={`state ${status.tone}`}>{status.label}</span>
      </p>
      {blocked && (
        <section className="status-card warning" aria-labelledby="blocked-heading">
          <h2 id="blocked-heading">Relecture bloquée : aucun relecteur éligible</h2>
          <p>{blocked.summary}</p>
          {blocked.details.length > 0 && (
            <ul>
              {blocked.details.map((d) => (
                <li key={d}>{d}</li>
              ))}
            </ul>
          )}
        </section>
      )}
      {review.eligible ? <CourseDecisionForm review={review} /> : status.inReview ? <NotEligible problems={review.problems} /> : <ReadOnlyStatus revision={r} label={status.label} />}
      <EarlierFeedback review={review} />
      <CourseSnapshot review={review} />
    </main>
  );
}

function NotEligible({ problems }: { problems: ReviewerProblem[] }) {
  return (
    <section className="status-card" aria-labelledby="not-eligible-heading">
      <h2 id="not-eligible-heading">Vous ne pouvez pas décider de cette version</h2>
      {problems.length > 0 ? (
        <ul>
          {problems.map((p, i) => (
            <li key={`${p.reason}-${p.lessonId ?? i}`}>{describeOwnProblem(p)}</li>
          ))}
        </ul>
      ) : (
        <p>Cette version n’est plus en relecture.</p>
      )}
      <p>Vous pouvez la lire. Un autre relecteur de la matière, indépendant de ces contenus, doit la relire : le serveur refuse toute décision de votre part.</p>
    </section>
  );
}

type Decision = 'approve' | 'request_changes';

function CourseDecisionForm({ review }: { review: CourseReview }) {
  const cr = useCourseReview();
  const r = review.revision;
  const [comment, setComment] = useState('');
  const [fieldError, setFieldError] = useState<string | null>(null);
  const [asking, setAsking] = useState<Decision | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<{ text: string; reasons: ReviewerProblem[] } | null>(null);
  const field = useRef<HTMLTextAreaElement>(null);
  const errorRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (error) errorRef.current?.focus();
  }, [error]);

  const ask = (decision: Decision) => {
    setError(null);
    if (decision === 'request_changes' && !comment.trim()) {
      setFieldError('Expliquez les modifications demandées : le commentaire est obligatoire pour renvoyer le cours.');
      field.current?.focus();
      return;
    }
    setFieldError(null);
    setAsking(decision);
  };

  const decide = async (decision: Decision) => {
    setBusy(true);
    try {
      if (decision === 'approve') await cr.approve(r.id, comment);
      else await cr.requestChanges(r.id, comment);
      setFlash(
        decision === 'approve'
          ? `Cours approuvé : « ${r.title} ». Il attend maintenant sa publication.`
          : `Modifications demandées pour le cours « ${r.title} ». L’auteur verra votre commentaire.`,
      );
      navigate(paths.reviewQueue, { force: true });
    } catch (e) {
      setAsking(null);
      setError({ text: describeCourseWorkflowError(e), reasons: reviewerReasons(e) });
      if (isApiError(e, 'comment_required') || isApiError(e, 'invalid_comment')) field.current?.focus();
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="status-card decision" aria-labelledby="decision-heading">
      <h2 id="decision-heading">Votre décision sur le cours</h2>
      <p className="hint">Vous relisez la présentation, le plan, l’enchaînement et le classement du cours. Les leçons ont chacune déjà été approuvées.</p>
      <NoteField
        id="course-review-comment"
        label="Commentaire pour l’auteur"
        hint="Facultatif pour approuver, obligatoire pour demander des modifications (5 000 caractères au maximum)."
        value={comment}
        onChange={(v) => {
          setComment(v);
          if (fieldError && v.trim()) setFieldError(null);
        }}
        error={fieldError}
        fieldRef={field}
      />
      <div className="actions">
        <button type="button" className="primary" onClick={() => ask('approve')} disabled={busy}>
          Approuver le cours…
        </button>
        <button type="button" onClick={() => ask('request_changes')} disabled={busy}>
          Demander des modifications…
        </button>
      </div>
      {error && (
        <div className="banner error" role="alert" tabIndex={-1} ref={errorRef}>
          <p>{error.text}</p>
          {error.reasons.length > 0 && (
            <ul>
              {error.reasons.map((p, i) => (
                <li key={`${p.reason}-${p.lessonId ?? i}`}>{describeOwnProblem(p)}</li>
              ))}
            </ul>
          )}
          <p>
            <Link to={paths.reviewQueue}>Revenir à la file de relecture</Link>
          </p>
        </div>
      )}
      {asking && (
        <Dialog
          title={asking === 'approve' ? `Approuver la version ${r.number} du cours ?` : 'Demander des modifications ?'}
          onCancel={() => setAsking(null)}
          cancelLabel="Revenir à la relecture"
          actions={[{ label: asking === 'approve' ? 'Approuver' : 'Envoyer la demande', kind: 'primary', disabled: busy, onClick: () => void decide(asking) }]}
        >
          <p>
            {asking === 'approve'
              ? 'Le cours sera proposé aux éditeurs pour publication. La décision est définitive.'
              : 'Le cours sera renvoyé à son auteur avec votre commentaire ; il rédigera une version corrigée. La décision est définitive.'}
          </p>
          {comment.trim() ? (
            <>
              <p>Commentaire :</p>
              <Quote>{comment.trim()}</Quote>
            </>
          ) : (
            <p>Sans commentaire.</p>
          )}
        </Dialog>
      )}
    </section>
  );
}

/* ---------- publication ---------- */

export const COURSE_STALE_EXPLANATION =
  'La version publiée du cours a changé depuis la rédaction de cette version (autre publication, suspension ou restauration) : le serveur refusera de la publier. Une version à jour doit être rédigée et relue ; un administrateur peut retirer celle-ci pour libérer le cours.';

/** The publisher's facts for a queue item: what publishing would replace and what blocks it. */
export function publishFacts(item: CoursePublishItem): { effect: string; blockers: string[] } {
  const blockers: string[] = [];
  if (item.archived) blockers.push('Le cours est archivé : il ne peut pas être publié.');
  if (item.staleBase) blockers.push(COURSE_STALE_EXPLANATION);
  if (item.ineligibleSteps > 0)
    blockers.push(
      `${item.ineligibleSteps} étape${item.ineligibleSteps > 1 ? 's épinglent' : ' épingle'} une révision de leçon qui n’est plus publiable (retirée, ou leçon dépubliée) : la publication sera refusée.`,
    );
  return {
    effect: item.publishedRevisionId === null ? 'Première publication du cours (ou republication après une suspension).' : 'Remplacera la version du cours que lisent actuellement les élèves.',
    blockers,
  };
}

export function CoursePublishPage({ revisionId }: { revisionId: number }) {
  const cr = useCourseReview();
  const [loaded, reload] = useLoad(async () => {
    const [queue, review] = await Promise.all([cr.publishQueue(), cr.review(revisionId)]);
    return { item: queue.find((q) => q.revisionId === revisionId) ?? null, review };
  }, [cr, revisionId]);
  const [asking, setAsking] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<{ text: string; steps: StepRef[]; stale: boolean } | null>(null);
  const [published, setPublished] = useState<CoursePointerChange | null>(null);
  const resultRef = useRef<HTMLElement>(null);
  useEffect(() => {
    if (published || error) resultRef.current?.focus();
  }, [published, error]);

  if (loaded.state === 'loading') return <LoadingPage label="Chargement de la version du cours…" />;
  if (loaded.state === 'error' || loaded.data.item === null)
    return (
      <ErrorPage
        title="Version de cours indisponible"
        message={
          loaded.state === 'ready' || loaded.notFound
            ? 'Cette version de cours n’attend pas de publication : elle n’existe pas, n’est pas approuvée, a déjà été publiée ou a été retirée.'
            : loaded.message
        }
        back={{ to: paths.publishQueue, label: 'Revenir aux contenus à publier' }}
      />
    );
  const { item, review } = loaded.data;
  const r = review.revision;
  const facts = publishFacts(item);

  const publish = async () => {
    setBusy(true);
    try {
      setPublished(await cr.publish(r.id, item.publishedRevisionId));
      setAsking(false);
      setError(null);
    } catch (e) {
      setAsking(false);
      setError({ text: describeCourseWorkflowError(e), steps: ineligibleSteps(e), stale: isApiError(e, 'stale_pointer') });
    } finally {
      setBusy(false);
    }
  };

  return (
    <main className="page" id="main">
      <p className="breadcrumb">
        <Link to={paths.publishQueue}>Publication</Link>
      </p>
      <h1 tabIndex={-1} lang={r.language} dir={dirOf(r.language)}>
        {r.title}
      </h1>
      <p className="lesson-meta">
        <span className="state kind-label">{r.kind === 'amendment' ? 'Cours · amendement' : 'Cours'}</span>
        <span className={`state ${published ? 'state-published' : 'state-approved'}`}>{published ? 'Publié' : 'Approuvé, en attente de publication'}</span>
      </p>
      <dl className="facts">
        <div>
          <dt>Auteur</dt>
          <dd>{item.authorName}</dd>
        </div>
        <div>
          <dt>Approuvé</dt>
          <dd>
            {r.review?.reviewerName ?? `relecteur n° ${item.approvedBy}`}, le {formatDateTime(item.approvedAt)}
          </dd>
        </div>
        <div>
          <dt>Effet</dt>
          <dd>{facts.effect}</dd>
        </div>
      </dl>
      {r.review?.comment && (
        <section className="feedback" aria-label="Commentaire du relecteur">
          <p>Commentaire du relecteur :</p>
          <Quote>{r.review.comment}</Quote>
        </section>
      )}
      {published ? (
        <section className="status-card" aria-labelledby="published-heading" role="status" tabIndex={-1} ref={resultRef}>
          <h2 id="published-heading">Cours publié</h2>
          <p>Les élèves lisent maintenant cette version du cours (publiée le {formatDateTime(published.occurredAt)}).</p>
          <p>
            <Link to={paths.publishQueue}>Revenir aux contenus à publier</Link>
          </p>
        </section>
      ) : facts.blockers.length > 0 ? (
        <section className="status-card warning" aria-labelledby="blocked-publish-heading">
          <h2 id="blocked-publish-heading">Publication impossible</h2>
          <ul>
            {facts.blockers.map((b) => (
              <li key={b}>{b}</li>
            ))}
          </ul>
        </section>
      ) : (
        <section className="status-card" aria-labelledby="publish-heading">
          <h2 id="publish-heading">Publier</h2>
          <p>Relisez le cours ci-dessous. La publication est immédiate et atomique : tout le cours change en une fois, ou rien ne change.</p>
          <button type="button" className="primary" onClick={() => setAsking(true)} disabled={busy}>
            Publier ce cours…
          </button>
        </section>
      )}
      {error && (
        <section className="banner error" role="alert" tabIndex={-1} ref={resultRef}>
          <p>{error.text}</p>
          {error.steps.length > 0 && (
            <ul>
              {error.steps.map((s) => (
                <li key={s.lessonId}>
                  Étape « {s.title} » (leçon n° {s.lessonId})
                </li>
              ))}
            </ul>
          )}
          {error.stale && (
            <button type="button" onClick={() => { setError(null); void reload(); }}>
              Recharger l’état actuel
            </button>
          )}
        </section>
      )}
      <CourseSnapshot review={review} />
      {asking && (
        <Dialog
          title={`Publier le cours « ${r.title} » ?`}
          onCancel={() => setAsking(false)}
          cancelLabel="Ne pas publier"
          actions={[{ label: 'Publier', kind: 'primary', disabled: busy, onClick: () => void publish() }]}
        >
          <p>{item.publishedRevisionId === null ? 'Le cours deviendra lisible par les élèves immédiatement.' : 'Cette version remplacera immédiatement celle que lisent les élèves.'}</p>
        </Dialog>
      )}
    </main>
  );
}

/** The courses awaiting publication, in the publisher's queue page. */
export function CoursePublishQueue() {
  const cr = useCourseReview();
  const subjects = useSubjects();
  const [queue] = useLoad(() => cr.publishQueue(), [cr]);
  return (
    <section aria-labelledby="course-publish-heading">
      <h2 id="course-publish-heading">Cours</h2>
      {queue.state === 'loading' && <p role="status">Chargement des cours à publier…</p>}
      {queue.state === 'error' && (
        <p className="banner error" role="alert">
          {queue.message}
        </p>
      )}
      {queue.state === 'ready' && queue.data.length === 0 && <p>Aucun cours approuvé n’attend de publication.</p>}
      {queue.state === 'ready' && queue.data.length > 0 && (
        <ul className="lesson-list" aria-label="Cours approuvés à publier">
          {queue.data.map((item) => {
            const facts = publishFacts(item);
            return (
              <li key={item.revisionId} className="lesson-card">
                <h3 lang={item.language} dir={dirOf(item.language)}>
                  <Link to={paths.publishCourse(item.revisionId)}>{item.title}</Link>
                </h3>
                <p className="lesson-meta">
                  <span className="state kind-label">{item.kind === 'amendment' ? 'Cours · amendement' : 'Cours'}</span>
                  <span className="state state-approved">Approuvé</span>
                  {facts.blockers.length > 0 && <span className="state state-changes">Publication impossible</span>}
                  <span className="state">{item.publishedRevisionId === null ? 'Aucune version publiée' : 'Remplacera la version publiée'}</span>
                </p>
                <p className="lesson-context">
                  <SubjectName subjects={subjects} id={item.subjectId} /> · {LANGUAGE_LABELS[item.language]} · version {item.number} · par {item.authorName} · approuvé le {formatDateTime(item.approvedAt)}
                </p>
                {facts.blockers.length > 0 && (
                  <ul className="lesson-context">
                    {facts.blockers.map((b) => (
                      <li key={b}>{b}</li>
                    ))}
                  </ul>
                )}
              </li>
            );
          })}
        </ul>
      )}
    </section>
  );
}
