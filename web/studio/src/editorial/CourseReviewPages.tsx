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
import { t, tn } from '../i18n';
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
  if (review.revision.assignments.length === 0) return <p>{t('review.course.noPlacement')}</p>;
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
            {e ? <CatalogueName label={catalogueLabel(e.code, e.labelFr, e.labelAr, language)} /> : t('review.editionNo', { id: a.editionId })}
            {a.retired && ` ${t('review.course.placementRetired')}`}
          </li>
        );
      })}
    </ul>
  );
}

/* ---------- outline and changes ---------- */

const FIELD_NAMES: Record<string, string> = {
  title: t('review.course.field.title'),
  objective: t('review.course.field.objective'),
  language: t('review.course.field.language'),
  status: t('review.course.field.status'),
  schemaVersion: t('review.course.field.schemaVersion'),
};

function text(value: unknown): string {
  if (value === null || value === undefined) return t('review.empty');
  return typeof value === 'string' ? value : JSON.stringify(value);
}

export const UNAVAILABLE_CONTENT = t('review.course.unavailable');

/**
 * A replaced step's content diff: changed fields, then removed and added
 * blocks through the canonical preview. Only what the server sent is shown;
 * a side it marked unavailable says so instead (diffSides).
 */
function StepDiff({ step, language }: { step: StepChange; language: 'fr' | 'ar' }) {
  const sides = diffSides(step);
  const [shown, setShown] = useState(false);
  const cell = (value: unknown, visible: boolean) => (visible ? text(value) : UNAVAILABLE_CONTENT);
  const hasBlocks = (sides.removed?.length ?? 0) > 0 || (sides.added?.length ?? 0) > 0;
  return (
    <div className="step-diff">
      {sides.fields.length > 0 && (
        <table>
          <caption>{t('review.course.changedFields')}</caption>
          <thead>
            <tr>
              <th scope="col">{t('review.course.field')}</th>
              <th scope="col">{t('review.revisionN', { number: step.previousRevisionNumber ?? '' })}</th>
              <th scope="col">{t('review.revisionN', { number: step.revisionNumber })}</th>
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
      {sides.removed === null && <p className="step-warning">{t('review.course.sideUnavailable', { number: step.previousRevisionNumber ?? '', unavailable: UNAVAILABLE_CONTENT })}</p>}
      {sides.added === null && <p className="step-warning">{t('review.course.sideUnavailable', { number: step.revisionNumber, unavailable: UNAVAILABLE_CONTENT })}</p>}
      {sides.removed !== null && sides.added !== null && (
        <p>
          {t('review.course.blocks', {
            removed: tn('review.course.blocksRemoved', sides.removed.length),
            added: tn('review.course.blocksAdded', sides.added.length),
            equal: tn('review.course.blocksEqual', sides.equal),
          })}
        </p>
      )}
      {hasBlocks && (
        <button type="button" aria-expanded={shown} onClick={() => setShown((v) => !v)}>
          {shown ? t('review.course.hideBlocks') : t('review.course.showBlocks')}
        </button>
      )}
      {shown && (
        <div className="diff-sides">
          {sides.removed && sides.removed.length > 0 && (
            <section className="diff-removed" aria-label={t('review.course.removedBlocks', { number: step.previousRevisionNumber ?? '' })}>
              <h5>{t('review.course.removedHeading', { number: step.previousRevisionNumber ?? '' })}</h5>
              <CanonicalPreview content={partialDocument(step.content, sides.removed, language)} testId={`diff-removed-${step.lessonId}`} mediaRoute="review" />
            </section>
          )}
          {sides.added && sides.added.length > 0 && (
            <section className="diff-added" aria-label={t('review.course.addedBlocks', { number: step.revisionNumber })}>
              <h5>{t('review.course.addedHeading', { number: step.revisionNumber })}</h5>
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
        {!step.eligible && <span className="state state-withdrawn">{t('review.course.stepIneligible')}</span>}
      </p>
      <p className="lesson-context">
        {t('review.course.lessonNo', { id: step.lessonId })} ·{' '}
        {step.change === 'replaced'
          ? t('review.course.stepReplaced', { previous: step.previousRevisionNumber ?? '', number: step.revisionNumber })
          : t('review.course.stepPinned', { number: step.revisionNumber })}
        {step.structureChanged && step.previousTitle !== null && (
          <>
            {' '}
            · {t('review.course.stepBefore', { title: step.previousTitle, module: moduleTitle(step.previousModuleKey), position: step.previousPosition ?? '' })}
          </>
        )}
      </p>
      {step.change === 'replaced' && <StepDiff step={step} language={language} />}
      {step.content === null || step.content === undefined ? (
        <p className="hint">{t('review.course.stepUnreadable', { unavailable: UNAVAILABLE_CONTENT })}</p>
      ) : (
        <>
          <button type="button" aria-expanded={preview} onClick={() => setPreview((v) => !v)}>
            {preview ? t('review.course.hideLesson') : t('review.course.readLesson', { number: step.revisionNumber })}
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
    return m ? t('review.course.moduleNamed', { title: m.title }) : t('review.course.moduleRemoved');
  };
  const kindText = r.kind === 'amendment' ? t('review.course.amendment') : t('review.course.ordinary');
  return (
    <>
      <dl className="facts">
        <div>
          <dt>{t('review.fact.subject')}</dt>
          <dd>
            <SubjectName subjects={subjects} id={r.subjectId} />
          </dd>
        </div>
        <div>
          <dt>{t('review.fact.language')}</dt>
          <dd>{LANGUAGE_LABELS[r.language]}</dd>
        </div>
        <div>
          <dt>{t('review.fact.version')}</dt>
          <dd>
            {r.submittedAt
              ? t('review.course.versionKindSubmitted', { number: r.number, kind: kindText, date: formatDateTime(r.submittedAt) })
              : t('review.course.versionKind', { number: r.number, kind: kindText })}
          </dd>
        </div>
      </dl>
      <section className="panel" aria-labelledby="overview-heading">
        <h2 id="overview-heading">{review.metadataChanged ? t('review.course.overviewChanged') : t('review.course.overviewUnchanged')}</h2>
        <dl className="overview">
          <dt>{t('review.course.summary')}</dt>
          <dd lang={r.language} dir={dir} className="pre-line">
            {r.summary || t('review.empty')}
          </dd>
          <dt>{t('review.course.objectives')}</dt>
          <dd lang={r.language} dir={dir} className="pre-line">
            {r.objectives || t('review.empty')}
          </dd>
          <dt>{t('review.fact.prerequisites')}</dt>
          <dd lang={r.language} dir={dir} className="pre-line">
            {r.prerequisites || t('review.none')}
          </dd>
          {r.coverMediaId !== null && (
            <>
              <dt>{t('review.course.cover')}</dt>
              <dd>{t('review.course.coverText', { id: r.coverMediaId, alt: r.coverAlt ?? '' })}</dd>
            </>
          )}
        </dl>
        <h3>{t('review.course.placement')}</h3>
        <Placement review={review} />
      </section>
      <section className="panel" aria-labelledby="outline-heading">
        <h2 id="outline-heading">{t('review.course.outline')}</h2>
        <p role="note">
          {r.compareRevisionId === null
            ? t('review.course.firstVersion')
            : t('review.course.compared', { summary: changeSummary(review.steps, review.removedSteps.length) })}{' '}
          {t('review.course.lessonsReviewed')}
        </p>
        <ol className="review-modules">
          {r.modules.map((m, i) => (
            <li key={m.key}>
              <h3>
                {t('review.course.module', { n: i + 1 })}{' '}
                <span lang={r.language} dir={dir}>
                  {m.title}
                </span>
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
            <h3>{t('review.course.removedSteps')}</h3>
            <ul>
              {review.removedSteps.map((s) => (
                <li key={s.lessonId}>
                  {t('review.course.removedStep', { title: s.title, id: s.lessonId })}
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
      <h2 id="earlier-heading">{t('review.course.earlier')}</h2>
      <ul className="history">
        {review.earlierDecisions.map((d) => (
          <li key={d.revisionId}>
            <p>
              {t(d.action === 'approve' ? 'review.course.earlierApproved' : 'review.course.earlierChanges', {
                number: d.number,
                reviewer: d.reviewerName,
                date: formatDateTime(d.decidedAt),
              })}
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
  if (r.withdrawnAt) return { label: t('review.course.withdrawnByAdmin'), tone: 'state-withdrawn', inReview: false };
  switch (r.status) {
    case 'in_review':
      return { label: t('shell.status.in_review'), tone: 'state-review', inReview: true };
    case 'approved':
      return { label: t('shell.status.approved'), tone: 'state-approved', inReview: false };
    case 'changes_requested':
      return { label: t('shell.status.changes_requested'), tone: 'state-changes', inReview: false };
    default:
      return { label: t('shell.status.draft'), tone: '', inReview: false };
  }
}

/** The 404 text: reviewers see only revisions in review of their subjects; publishers and administrators any submitted revision. */
export function notFoundText(spaces: Spaces): string {
  if (spaces.publisher || spaces.administrator)
    return spaces.reviewer
      ? t('review.course.notFoundAnyReviewer')
      : t('review.course.notFoundAny');
  return t('review.course.notFoundReviewer');
}

function ReadOnlyStatus({ revision, label }: { revision: ReviewedRevision; label: string }) {
  const d = revision.review;
  return (
    <section className="status-card" aria-labelledby="readonly-heading">
      <h2 id="readonly-heading">{t('review.course.readonlyHeading', { status: label.toLowerCase() })}</h2>
      <p>
        {revision.withdrawnAt
          ? t('review.course.readonlyWithdrawn', { date: formatDateTime(revision.withdrawnAt) })
          : t('review.course.readonlyText')}
      </p>
      {d && (
        <p>
          {t(d.action === 'approve' ? 'review.course.readonlyApproved' : 'review.course.readonlyChanges', { reviewer: d.reviewerName, date: formatDateTime(d.decidedAt) })}
        </p>
      )}
    </section>
  );
}

export function CourseReviewPage({ revisionId }: { revisionId: number }) {
  const cr = useCourseReview();
  const { spaces } = useStudio();
  const [loaded] = useLoad(() => cr.review(revisionId), [cr, revisionId]);
  if (loaded.state === 'loading') return <LoadingPage label={t('review.course.loading')} />;
  if (loaded.state === 'error')
    return (
      <ErrorPage
        title={t('review.course.unavailableTitle')}
        message={loaded.notFound ? notFoundText(spaces) : loaded.message}
        back={
          spaces.reviewer
            ? { to: paths.reviewQueue, label: t('review.backToQueue') }
            : spaces.administrator
              ? { to: paths.adminCourses, label: t('review.backToCourseAdmin') }
              : { to: paths.publishQueue, label: t('review.backToContentToPublish') }
        }
      />
    );
  const review = loaded.data;
  const r = review.revision;
  const blocked = r.blocked ? describeBlocked(r.blocked) : null;
  const status = reviewStatus(r);
  return (
    <main className="page" id="main">
      <p className="breadcrumb">
        <Link to={paths.reviewQueue}>{t('shell.space.review')}</Link>
      </p>
      <h1 tabIndex={-1} lang={r.language} dir={dirOf(r.language)}>
        {r.title}
      </h1>
      <p className="lesson-meta">
        <span className="state kind-label">{r.kind === 'amendment' ? t('review.kind.amendment') : t('review.kind.course')}</span>
        <span className={`state ${status.tone}`}>{status.label}</span>
      </p>
      {blocked && (
        <section className="status-card warning" aria-labelledby="blocked-heading">
          <h2 id="blocked-heading">{t('review.course.blockedHeading')}</h2>
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
      <h2 id="not-eligible-heading">{t('review.course.notEligibleHeading')}</h2>
      {problems.length > 0 ? (
        <ul>
          {problems.map((p, i) => (
            <li key={`${p.reason}-${p.lessonId ?? i}`}>{describeOwnProblem(p)}</li>
          ))}
        </ul>
      ) : (
        <p>{t('review.course.notInReview')}</p>
      )}
      <p>{t('review.course.notEligibleText')}</p>
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
      setFieldError(t('review.course.commentRequired'));
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
          ? t('review.course.approved', { title: r.title })
          : t('review.course.changesRequested', { title: r.title }),
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
      <h2 id="decision-heading">{t('review.course.decisionHeading')}</h2>
      <p className="hint">{t('review.course.decisionHint')}</p>
      <NoteField
        id="course-review-comment"
        label={t('review.decision.commentLabel')}
        hint={t('review.decision.commentHint')}
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
          {t('review.course.approveButton')}
        </button>
        <button type="button" onClick={() => ask('request_changes')} disabled={busy}>
          {t('review.decision.requestButton')}
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
            <Link to={paths.reviewQueue}>{t('review.backToQueue')}</Link>
          </p>
        </div>
      )}
      {asking && (
        <Dialog
          title={asking === 'approve' ? t('review.course.approveTitle', { number: r.number }) : t('review.decision.requestTitle')}
          onCancel={() => setAsking(null)}
          cancelLabel={t('review.backToReview')}
          actions={[{ label: asking === 'approve' ? t('review.decision.approve') : t('review.decision.send'), kind: 'primary', disabled: busy, onClick: () => void decide(asking) }]}
        >
          <p>
            {asking === 'approve' ? t('review.course.approveText') : t('review.course.requestText')}
          </p>
          {comment.trim() ? (
            <>
              <p>{t('review.decision.comment')}</p>
              <Quote>{comment.trim()}</Quote>
            </>
          ) : (
            <p>{t('review.decision.noComment')}</p>
          )}
        </Dialog>
      )}
    </section>
  );
}

/* ---------- publication ---------- */

export const COURSE_STALE_EXPLANATION = t('review.coursePublish.stale');

/** The publisher's facts for a queue item: what publishing would replace and what blocks it. */
export function publishFacts(item: CoursePublishItem): { effect: string; blockers: string[] } {
  const blockers: string[] = [];
  if (item.archived) blockers.push(t('review.coursePublish.archived'));
  if (item.staleBase) blockers.push(COURSE_STALE_EXPLANATION);
  if (item.ineligibleSteps > 0) blockers.push(tn('review.coursePublish.ineligible', item.ineligibleSteps));
  return {
    effect: item.publishedRevisionId === null ? t('review.coursePublish.firstEffect') : t('review.coursePublish.replaceEffect'),
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

  if (loaded.state === 'loading') return <LoadingPage label={t('review.course.loading')} />;
  if (loaded.state === 'error' || loaded.data.item === null)
    return (
      <ErrorPage
        title={t('review.course.unavailableTitle')}
        message={
          loaded.state === 'ready' || loaded.notFound
            ? t('review.coursePublish.notFound')
            : loaded.message
        }
        back={{ to: paths.publishQueue, label: t('review.backToContentToPublish') }}
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
        <Link to={paths.publishQueue}>{t('shell.space.publish')}</Link>
      </p>
      <h1 tabIndex={-1} lang={r.language} dir={dirOf(r.language)}>
        {r.title}
      </h1>
      <p className="lesson-meta">
        <span className="state kind-label">{r.kind === 'amendment' ? t('review.kind.amendment') : t('review.kind.course')}</span>
        <span className={`state ${published ? 'state-published' : 'state-approved'}`}>{published ? t('review.coursePublish.published') : t('review.coursePublish.awaiting')}</span>
      </p>
      <dl className="facts">
        <div>
          <dt>{t('review.fact.author')}</dt>
          <dd>{item.authorName}</dd>
        </div>
        <div>
          <dt>{t('review.coursePublish.approved')}</dt>
          <dd>
            {t('review.fact.byOn', { name: r.review?.reviewerName ?? t('review.reviewerNo', { id: item.approvedBy }), date: formatDateTime(item.approvedAt) })}
          </dd>
        </div>
        <div>
          <dt>{t('review.fact.effect')}</dt>
          <dd>{facts.effect}</dd>
        </div>
      </dl>
      {r.review?.comment && (
        <section className="feedback" aria-label={t('review.reviewerComment')}>
          <p>{t('review.reviewerCommentLabel')}</p>
          <Quote>{r.review.comment}</Quote>
        </section>
      )}
      {published ? (
        <section className="status-card" aria-labelledby="published-heading" role="status" tabIndex={-1} ref={resultRef}>
          <h2 id="published-heading">{t('review.coursePublish.done')}</h2>
          <p>{t('review.coursePublish.doneText', { date: formatDateTime(published.occurredAt) })}</p>
          <p>
            <Link to={paths.publishQueue}>{t('review.backToContentToPublish')}</Link>
          </p>
        </section>
      ) : facts.blockers.length > 0 ? (
        <section className="status-card warning" aria-labelledby="blocked-publish-heading">
          <h2 id="blocked-publish-heading">{t('review.publishImpossible')}</h2>
          <ul>
            {facts.blockers.map((b) => (
              <li key={b}>{b}</li>
            ))}
          </ul>
        </section>
      ) : (
        <section className="status-card" aria-labelledby="publish-heading">
          <h2 id="publish-heading">{t('review.publish')}</h2>
          <p>{t('review.coursePublish.text')}</p>
          <button type="button" className="primary" onClick={() => setAsking(true)} disabled={busy}>
            {t('review.coursePublish.button')}
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
                  {t('review.coursePublish.step', { title: s.title, id: s.lessonId })}
                </li>
              ))}
            </ul>
          )}
          {error.stale && (
            <button type="button" onClick={() => { setError(null); void reload(); }}>
              {t('review.coursePublish.reload')}
            </button>
          )}
        </section>
      )}
      <CourseSnapshot review={review} />
      {asking && (
        <Dialog
          title={t('review.coursePublish.dialogTitle', { title: r.title })}
          onCancel={() => setAsking(false)}
          cancelLabel={t('review.dontPublish')}
          actions={[{ label: t('review.publish'), kind: 'primary', disabled: busy, onClick: () => void publish() }]}
        >
          <p>{item.publishedRevisionId === null ? t('review.coursePublish.firstText') : t('review.coursePublish.replaceText')}</p>
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
      <h2 id="course-publish-heading">{t('review.courses')}</h2>
      {queue.state === 'loading' && <p role="status">{t('review.coursePublish.loading')}</p>}
      {queue.state === 'error' && (
        <p className="banner error" role="alert">
          {queue.message}
        </p>
      )}
      {queue.state === 'ready' && queue.data.length === 0 && <p>{t('review.coursePublish.empty')}</p>}
      {queue.state === 'ready' && queue.data.length > 0 && (
        <ul className="lesson-list" aria-label={t('review.coursePublish.list')}>
          {queue.data.map((item) => {
            const facts = publishFacts(item);
            return (
              <li key={item.revisionId} className="lesson-card">
                <h3 lang={item.language} dir={dirOf(item.language)}>
                  <Link to={paths.publishCourse(item.revisionId)}>{item.title}</Link>
                </h3>
                <p className="lesson-meta">
                  <span className="state kind-label">{item.kind === 'amendment' ? t('review.kind.amendment') : t('review.kind.course')}</span>
                  <span className="state state-approved">{t('review.coursePublish.approved')}</span>
                  {facts.blockers.length > 0 && <span className="state state-changes">{t('review.publishImpossible')}</span>}
                  <span className="state">{item.publishedRevisionId === null ? t('review.coursePublish.noPublished') : t('review.replacesPublished')}</span>
                </p>
                <p className="lesson-context">
                  <SubjectName subjects={subjects} id={item.subjectId} /> ·{' '}
                  {t('review.coursePublish.context', {
                    language: LANGUAGE_LABELS[item.language],
                    number: item.number,
                    author: item.authorName,
                    date: formatDateTime(item.approvedAt),
                  })}
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
