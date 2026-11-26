// The author's views of a course outside its draft (LMS-B06): the state of
// its latest revision (in review, possibly blocked for want of an eligible
// reviewer; changes requested with the reviewer's feedback; approved;
// published; withdrawn), the course's own state (suspended, archived), the
// newer-lesson-revision notice with amendment creation, and the archive
// request. The server decides everything; these explain its answers.
import { useEffect, useId, useMemo, useRef, useState } from 'react';
import { isApiError } from '../api/client';
import { courseApi, type Course, type CourseRevision } from '../api/courses';
import { courseReviewApi, type NewerRevisions, type ReviewedRevision } from '../api/courseReview';
import { describeCourseWorkflowError } from '../api/courseReviewMessages';
import { useStudio } from '../context';
import { NoteField, Quote } from '../editorial/common';
import { t } from '../i18n';
import { STATUS_LABELS, formatDateTime } from '../labels';
import { Dialog } from '../ui/Dialog';
import { COURSE_STATUS_LABELS } from './courseLabels';
import { describeBlocked } from './workflow';

/** Whether the course has no open candidate, so the owner may start a new draft (the server decides). */
export function canStartDraft(course: Course, revision: ReviewedRevision | null): boolean {
  if (course.archived) return false;
  if (!revision || revision.withdrawnAt) return true;
  if (revision.status === 'changes_requested') return true;
  return revision.status === 'approved' && (course.publishedRevisionId === revision.id || everPublished(course, revision));
}

/** The revision was published at some point (the owner's list says so for the latest revision). */
export const everPublished = (course: Course, revision: ReviewedRevision) =>
  course.publishedRevisionId === revision.id || (course.latestRevision?.id === revision.id && !!course.latestRevision.everPublished);

/** Archive and suspension, which decide whether students can read the course whatever revision is in progress. */
export function CourseStateNotice({ course }: { course: Course }) {
  if (course.archived)
    return (
      <section className="status-card warning" aria-labelledby="course-state-heading">
        <h2 id="course-state-heading">{t('courses.status.archivedHeading')}</h2>
        <p>{t('courses.status.archivedText')}</p>
      </section>
    );
  if (course.suspended)
    return (
      <section className="status-card warning" aria-labelledby="course-state-heading">
        <h2 id="course-state-heading">{t('courses.status.suspendedHeading')}</h2>
        <p>{t('courses.status.suspendedText')}</p>
      </section>
    );
  return null;
}

function Feedback({ revision }: { revision: ReviewedRevision }) {
  const r = revision.review;
  if (!r) return null;
  return (
    <section className="feedback" aria-labelledby="feedback-heading">
      <h3 id="feedback-heading">{r.action === 'request_changes' ? t('courses.status.changesRequested') : t('courses.status.reviewerComment')}</h3>
      <p>{t('courses.status.decidedBy', { name: r.reviewerName, date: formatDateTime(r.decidedAt) })}</p>
      {r.comment ? <Quote>{r.comment}</Quote> : <p>{t('courses.status.noComment')}</p>}
    </section>
  );
}

export function CourseStatusView({
  course,
  revision,
  onDraft,
  onDeleted,
}: {
  course: Course;
  revision: ReviewedRevision | null;
  onDraft: (created: CourseRevision) => void;
  onDeleted: () => void;
}) {
  const { api, expired } = useStudio();
  const courses = useMemo(() => courseApi(api), [api]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [confirm, setConfirm] = useState(false);
  const errorRef = useRef<HTMLParagraphElement>(null);
  useEffect(() => {
    if (error) errorRef.current?.focus();
  }, [error]);

  const start = async () => {
    setBusy(true);
    setError(null);
    try {
      onDraft(await courses.startDraft(course.id));
    } catch (e) {
      setError(describeCourseWorkflowError(e));
    } finally {
      setBusy(false);
    }
  };
  const remove = async () => {
    setConfirm(false);
    setError(null);
    try {
      await courses.deleteCourse(course.id);
      onDeleted();
    } catch (e) {
      setError(describeCourseWorkflowError(e));
    }
  };

  const published = revision !== null && course.publishedRevisionId === revision.id;
  let heading: string;
  let body: React.ReactNode;
  if (!revision) {
    heading = t('courses.status.noDraft');
    body = <p>{t('courses.status.noDraftText')}</p>;
  } else if (revision.withdrawnAt) {
    heading = t('courses.status.heading', { number: revision.number, status: t('courses.status.withdrawnState') });
    body = <p>{t('courses.status.withdrawnText', { date: formatDateTime(revision.withdrawnAt) })}</p>;
  } else if (revision.status === 'in_review') {
    const blocked = revision.blocked ? describeBlocked(revision.blocked) : null;
    heading = t('courses.status.heading', { number: revision.number, status: STATUS_LABELS.in_review });
    body = (
      <>
        <p>
          {revision.kind === 'amendment' ? t('courses.status.inReviewAmendment') : t('courses.status.inReviewVersion')}
          {revision.submittedAt && t('courses.status.submittedOn', { date: formatDateTime(revision.submittedAt) })}
        </p>
        {blocked && (
          <div className="status-card warning" role="note" aria-labelledby="blocked-heading">
            <h3 id="blocked-heading">{t('courses.status.blockedHeading')}</h3>
            <p>{blocked.summary}</p>
            {blocked.details.length > 0 && (
              <ul>
                {blocked.details.map((d) => (
                  <li key={d}>{d}</li>
                ))}
              </ul>
            )}
            <p>{t('courses.status.blockedText')}</p>
          </div>
        )}
      </>
    );
  } else if (revision.status === 'changes_requested') {
    heading = t('courses.status.heading', { number: revision.number, status: STATUS_LABELS.changes_requested });
    body = <p>{t('courses.status.changesText')}</p>;
  } else if (revision.status === 'approved' && published) {
    heading = t('courses.status.heading', { number: revision.number, status: t('shell.state.published') });
    body = <p>{t('courses.status.publishedText')}</p>;
  } else if (revision.status === 'approved' && everPublished(course, revision)) {
    heading = t('courses.status.heading', { number: revision.number, status: t('courses.status.publishedBefore') });
    body = (
      <p>
        {course.archived
          ? t('courses.status.notReadArchived')
          : course.suspended
            ? t('courses.status.notReadSuspended')
            : t('courses.status.otherRead')}
      </p>
    );
  } else if (revision.status === 'approved') {
    heading = t('courses.status.heading', { number: revision.number, status: t('shell.state.awaitingPublication') });
    body = <p>{t('courses.status.approvedText')}</p>;
  } else {
    heading = t('courses.status.heading', { number: revision.number, status: COURSE_STATUS_LABELS[revision.status] });
    body = <p>{t('courses.status.leftDraftText')}</p>;
  }

  return (
    <section className="status-card" aria-labelledby="course-status-heading">
      <h2 id="course-status-heading">{heading}</h2>
      {body}
      {revision && <Feedback revision={revision} />}
      <div className="actions">
        {canStartDraft(course, revision) && (
          <button type="button" className="primary" onClick={() => void start()} disabled={busy || expired}>
            {!revision ? t('courses.status.createDraft') : revision.status === 'changes_requested' ? t('courses.status.createCorrected') : t('courses.status.createVersion')}
          </button>
        )}
        {!revision && (
          <button type="button" className="danger" onClick={() => setConfirm(true)} disabled={busy || expired}>
            {t('courses.editor.deleteCourse')}
          </button>
        )}
      </div>
      {error && (
        <p className="banner error" role="alert" tabIndex={-1} ref={errorRef}>
          {error}
        </p>
      )}
      {confirm && (
        <Dialog title={t('courses.editor.deleteTitle')} onCancel={() => setConfirm(false)} cancelLabel={t('courses.editor.keepCourse')} actions={[{ label: t('courses.editor.deleteCourse'), kind: 'danger', onClick: () => void remove() }]}>
          <p>{t('courses.status.deleteText')}</p>
        </Dialog>
      )}
    </section>
  );
}

/**
 * "A newer eligible revision is available": steps of the published course
 * whose lesson has a newer publishable revision. The owner chooses the
 * replacements and creates an amendment in one action; only one candidate
 * per course exists, so an open draft is reported (draft_exists).
 */
export function NewerRevisionsNotice({ course, draftOpen, onAmendment }: { course: Course; draftOpen: boolean; onAmendment: (created: ReviewedRevision) => void }) {
  const { api, expired } = useStudio();
  const review = useMemo(() => courseReviewApi(api), [api]);
  const [notice, setNotice] = useState<NewerRevisions | null>(null);
  const [loadError, setLoadError] = useState(false);
  const [chosen, setChosen] = useState<Set<number>>(new Set());
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<{ text: string; draftExists: boolean } | null>(null);
  const errorRef = useRef<HTMLDivElement>(null);
  const id = useId();

  useEffect(() => {
    let cancelled = false;
    review.newerRevisions(course.id).then(
      (n) => {
        if (cancelled) return;
        setNotice(n);
        setChosen(new Set(n.steps.map((s) => s.lessonId)));
      },
      () => !cancelled && setLoadError(true),
    );
    return () => {
      cancelled = true;
    };
  }, [review, course.id]);
  useEffect(() => {
    if (error) errorRef.current?.focus();
  }, [error]);

  if (loadError)
    return (
      <p className="banner error" role="alert">
        {t('courses.newer.loadError')}
      </p>
    );
  if (!notice || notice.steps.length === 0) return null;

  const create = async () => {
    setBusy(true);
    setError(null);
    try {
      const steps = notice.steps.filter((s) => chosen.has(s.lessonId)).map((s) => ({ lessonId: s.lessonId, revisionId: s.newerRevisionId }));
      onAmendment(await review.createAmendment(course.id, steps));
    } catch (e) {
      setError({ text: describeCourseWorkflowError(e), draftExists: isApiError(e, 'draft_exists') });
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="status-card warning" aria-labelledby={`${id}-heading`}>
      <h2 id={`${id}-heading`}>{t('courses.newer.heading')}</h2>
      <p>{t('courses.newer.text')}</p>
      <fieldset>
        <legend>{t('courses.newer.legend')}</legend>
        {notice.steps.map((s) => (
          <div key={s.lessonId} className="checkbox">
            <input
              type="checkbox"
              id={`${id}-${s.lessonId}`}
              checked={chosen.has(s.lessonId)}
              onChange={(e) =>
                setChosen((c) => {
                  const next = new Set(c);
                  if (e.target.checked) next.add(s.lessonId);
                  else next.delete(s.lessonId);
                  return next;
                })
              }
            />
            <label htmlFor={`${id}-${s.lessonId}`}>
              {t('courses.newer.step', { title: s.title, pinned: s.pinnedNumber, newer: s.newerNumber })}
            </label>
          </div>
        ))}
      </fieldset>
      {draftOpen && <p className="hint">{t('courses.newer.draftOpen')}</p>}
      <div className="actions">
        <button type="button" className="primary" onClick={() => void create()} disabled={busy || expired || chosen.size === 0}>
          {t('courses.newer.create')}
        </button>
      </div>
      {error && (
        <div className="banner error" role="alert" tabIndex={-1} ref={errorRef}>
          <p>{error.text}</p>
          {error.draftExists && !draftOpen && <p>{t('courses.newer.reloadForDraft')}</p>}
        </div>
      )}
    </section>
  );
}

/** The owner asks administrators to archive a submitted course, with a reason (recorded and listed for them). */
export function ArchiveRequest({ course }: { course: Course }) {
  const { api, expired } = useStudio();
  const review = useMemo(() => courseReviewApi(api), [api]);
  const [open, setOpen] = useState(false);
  const [reason, setReason] = useState('');
  const [fieldError, setFieldError] = useState<string | null>(null);
  const [asking, setAsking] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [done, setDone] = useState(false);
  const field = useRef<HTMLTextAreaElement>(null);
  const doneRef = useRef<HTMLParagraphElement>(null);
  useEffect(() => {
    if (open) field.current?.focus();
  }, [open]);
  useEffect(() => {
    if (done) doneRef.current?.focus();
  }, [done]);

  if (course.archived) return null;
  if (done)
    return (
      <p className="banner success" role="status" tabIndex={-1} ref={doneRef}>
        {t('courses.archive.done')}
      </p>
    );

  const send = async () => {
    setBusy(true);
    try {
      await review.requestArchive(course.id, reason.trim());
      setAsking(false);
      setDone(true);
    } catch (e) {
      setAsking(false);
      setError(describeCourseWorkflowError(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="danger-zone" aria-labelledby="archive-request-heading">
      <h2 id="archive-request-heading">{t('courses.archive.heading')}</h2>
      <p className="hint">{t('courses.archive.hint')}</p>
      {!open ? (
        <button type="button" onClick={() => setOpen(true)} disabled={expired}>
          {t('courses.archive.ask')}
        </button>
      ) : (
        <div className="reason-form">
          <NoteField
            id="archive-request-reason"
            label={t('courses.archive.reason')}
            hint={t('courses.archive.reasonHint')}
            value={reason}
            onChange={(v) => {
              setReason(v);
              if (fieldError && v.trim()) setFieldError(null);
            }}
            error={fieldError}
            fieldRef={field}
          />
          <div className="actions">
            <button
              type="button"
              className="danger"
              disabled={busy || expired}
              onClick={() => {
                setError(null);
                if (!reason.trim()) {
                  setFieldError(t('courses.archive.reasonRequired'));
                  field.current?.focus();
                  return;
                }
                setAsking(true);
              }}
            >
              {t('courses.archive.send')}
            </button>
            <button type="button" onClick={() => setOpen(false)}>
              {t('shell.cancel')}
            </button>
          </div>
        </div>
      )}
      {error && (
        <p className="banner error" role="alert">
          {error}
        </p>
      )}
      {asking && (
        <Dialog title={t('courses.archive.confirmTitle')} onCancel={() => setAsking(false)} cancelLabel={t('courses.archive.dontSend')} actions={[{ label: t('courses.archive.sendAction'), kind: 'danger', disabled: busy, onClick: () => void send() }]}>
          <p>{t('courses.archive.confirmText')}</p>
          <Quote>{reason.trim()}</Quote>
        </Dialog>
      )}
    </section>
  );
}
