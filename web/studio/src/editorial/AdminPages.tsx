// Administrator space (the plan's minimal emergency actions): find a lesson,
// then on its publication page withdraw a revision, unpublish the lesson
// in an emergency, or restore a revision published before. Restoration is
// also open to publishers. Every action names the published revision this
// page loaded (expectedPublishedRevisionId); if anyone changed it since,
// the server refuses with stale_pointer and nothing changes.
import { useRef, useState, type FormEvent } from 'react';
import { isApiError } from '../api/client';
import type { LessonPublication, LessonRevisionState } from '../api/editorial';
import { describeEditorialError } from '../api/editorialMessages';
import { useStudio } from '../context';
import { lessonRefFromInput, paths } from '../editorialRoutes';
import { t } from '../i18n';
import { LANGUAGE_LABELS, formatDateTime, formatTime } from '../labels';
import { navigate } from '../router';
import { Dialog } from '../ui/Dialog';
import { Link } from '../ui/Link';
import { ErrorPage, LoadingPage, NoteField, Quote, dirOf, useEditorial, useLoad } from './common';
import { AdminNav } from './adminCommon';
import { canUnpublish, describeEvent, describeSubmission, revisionActions, revisionStateLabel } from './policy';

export function AdminLessonsPage() {
  const ed = useEditorial();
  const [lessons] = useLoad(() => ed.adminLessons(), [ed]);
  const [ref, setRef] = useState('');
  const [refError, setRefError] = useState<string | null>(null);
  const input = useRef<HTMLInputElement>(null);

  const find = (e: FormEvent) => {
    e.preventDefault();
    const parsed = lessonRefFromInput(ref);
    if (!parsed) {
      setRefError(t('admin.lessons.refError'));
      input.current?.focus();
      return;
    }
    navigate(paths.publication(parsed));
  };

  return (
    <main className="page" id="main">
      <AdminNav />
      <h1 tabIndex={-1}>{t('shell.space.admin')}</h1>
      <p className="hint">{t('admin.lessons.intro')}</p>
      <form className="find-lesson" onSubmit={find} noValidate>
        <div className="fields">
          <label htmlFor="lesson-ref">{t('admin.lessons.find')}</label>
          <p className="hint" id="lesson-ref-hint">
            {t('admin.lessons.findHint')}
          </p>
          {refError && (
            <p className="field-error" id="lesson-ref-error">
              {refError}
            </p>
          )}
          <input
            id="lesson-ref"
            ref={input}
            value={ref}
            dir="ltr"
            autoComplete="off"
            spellCheck={false}
            aria-invalid={refError ? true : undefined}
            aria-describedby={`lesson-ref-hint${refError ? ' lesson-ref-error' : ''}`}
            onChange={(e) => setRef(e.target.value)}
          />
        </div>
        <button type="submit">{t('admin.lessons.open')}</button>
      </form>
      <h2>{t('admin.lessons.list')}</h2>
      {lessons.state === 'loading' && <p role="status">{t('admin.lessons.loading')}</p>}
      {lessons.state === 'error' && (
        <p className="banner error" role="alert">
          {lessons.message}
        </p>
      )}
      {lessons.state === 'ready' && lessons.data.length === 0 && <p>{t('admin.lessons.empty')}</p>}
      {lessons.state === 'ready' && lessons.data.length > 0 && (
        <ul className="lesson-list" aria-label={t('admin.lessons.list')}>
          {lessons.data.map((l) => (
            <li key={l.lessonId} className="lesson-card">
              <h3 lang={l.language} dir={dirOf(l.language)}>
                <Link to={paths.publication(l.publicId)}>{l.title}</Link>
              </h3>
              <p className="lesson-meta">
                {l.publishedRevisionId !== null ? <span className="state state-published">{t('shell.state.published')}</span> : <span className="state">{t('admin.lessons.unpublished')}</span>}
                {l.pendingRevisionId !== null && <span className="state state-approved">{t('admin.lessons.pending')}</span>}
                {l.archived && <span className="state">{t('admin.lessons.archived')}</span>}
              </p>
              <p className="lesson-context">
                {l.subjectCode} · {LANGUAGE_LABELS[l.language]} · {t('admin.lessons.lastChange', { date: formatDateTime(l.lastChange) })}
              </p>
            </li>
          ))}
        </ul>
      )}
    </main>
  );
}

type Pending =
  | { kind: 'withdraw'; revision: LessonRevisionState }
  | { kind: 'restore'; revision: LessonRevisionState }
  | { kind: 'unpublish' };

export function LessonPublicationPage({ lessonRef }: { lessonRef: string }) {
  const ed = useEditorial();
  const { spaces } = useStudio();
  const [loadedAt, setLoadedAt] = useState(() => new Date());
  const [loaded, reload] = useLoad(async () => {
    const lp = await ed.lessonPublication(lessonRef);
    setLoadedAt(new Date());
    return lp;
  }, [ed, lessonRef]);
  // Which inline reason form is open: a revision ID (withdrawal) or 'unpublish'.
  const [form, setForm] = useState<number | 'unpublish' | null>(null);
  const [reason, setReason] = useState('');
  const [reasonError, setReasonError] = useState<string | null>(null);
  const [pending, setPending] = useState<Pending | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<{ message: string; stale: boolean } | null>(null);
  const [done, setDone] = useState<string | null>(null);
  const reasonRef = useRef<HTMLTextAreaElement>(null);
  const doneRef = useRef<HTMLParagraphElement>(null);

  if (loaded.state === 'loading') return <LoadingPage label={t('admin.lesson.loading')} />;
  if (loaded.state === 'error')
    return (
      <ErrorPage
        title={t('admin.lesson.notFoundTitle')}
        message={loaded.notFound ? t('admin.lesson.notFound') : loaded.message}
        back={spaces.administrator ? { to: paths.adminLessons, label: t('admin.lesson.backAdmin') } : { to: paths.publishQueue, label: t('admin.lesson.backPublish') }}
      />
    );
  const lesson: LessonPublication = loaded.data;
  const byId = new Map(lesson.revisions.map((r) => [r.id, r]));
  const numberOf = (id: number | null) => (id === null ? '—' : byId.has(id) ? String(byId.get(id)!.number) : t('admin.lesson.numberId', { id }));
  const current = lesson.publishedRevisionId !== null ? byId.get(lesson.publishedRevisionId) : undefined;
  const contentTitle = current?.title ?? lesson.revisions[0]?.title;
  const title = contentTitle ?? t('admin.lesson.fallbackTitle', { id: lesson.lessonId });
  const expectedLabel = lesson.publishedRevisionId === null ? t('admin.lesson.expectedNone') : t('admin.lesson.expectedRevision', { number: numberOf(lesson.publishedRevisionId) });

  const openForm = (which: number | 'unpublish') => {
    setForm(which);
    setReason('');
    setReasonError(null);
    setError(null);
    requestAnimationFrame(() => reasonRef.current?.focus());
  };

  const askWithReason = (next: Pending) => {
    if (!reason.trim()) {
      setReasonError(t('admin.reasonRequired'));
      reasonRef.current?.focus();
      return;
    }
    setReasonError(null);
    setPending(next);
  };

  const run = async (action: Pending) => {
    setBusy(true);
    const expected = lesson.publishedRevisionId;
    try {
      if (action.kind === 'withdraw') {
        const w = await ed.withdraw(action.revision.id, reason, expected);
        const withdrawn = t('admin.lesson.withdrawnDone', { number: action.revision.number });
        setDone(w.unpublication ? `${withdrawn} ${t('admin.lesson.withdrawnWasPublished')}` : withdrawn);
      } else if (action.kind === 'unpublish') {
        await ed.unpublish(lesson.lessonId, reason, expected!);
        setDone(t('admin.lesson.unpublishedDone'));
      } else {
        await ed.restore(action.revision.id, expected);
        setDone(t('admin.lesson.restoredDone', { number: action.revision.number }));
      }
      setPending(null);
      setForm(null);
      setReason('');
      setError(null);
      await reload();
      // The button that was pressed may be gone (withdrawn revision, closed form): move focus to the outcome.
      requestAnimationFrame(() => doneRef.current?.focus());
    } catch (e) {
      setPending(null);
      setDone(null);
      setError({ message: describeEditorialError(e), stale: isApiError(e, 'stale_pointer') });
      if (isApiError(e, 'reason_required') || isApiError(e, 'invalid_reason')) reasonRef.current?.focus();
    } finally {
      setBusy(false);
    }
  };

  const reasonForm = (which: number | 'unpublish', submitLabel: string, onSubmit: () => void) =>
    form === which && (
      <form
        className="reason-form"
        onSubmit={(e) => {
          e.preventDefault();
          onSubmit();
        }}
        noValidate
      >
        <NoteField
          id={`reason-${which}`}
          label={t('admin.lesson.reasonLabel')}
          hint={t('admin.lesson.reasonHint')}
          value={reason}
          onChange={(v) => {
            setReason(v);
            if (reasonError && v.trim()) setReasonError(null);
          }}
          error={reasonError}
          fieldRef={reasonRef}
        />
        <div className="actions">
          <button type="submit" className="danger">
            {submitLabel}
          </button>
          <button type="button" onClick={() => setForm(null)}>
            {t('shell.cancel')}
          </button>
        </div>
      </form>
    );

  return (
    <main className="page" id="main">
      <p className="breadcrumb">
        {spaces.administrator ? <Link to={paths.adminLessons}>{t('shell.space.admin')}</Link> : <Link to={paths.publishQueue}>{t('shell.space.publish')}</Link>}
      </p>
      <h1 tabIndex={-1} lang={contentTitle !== undefined ? lesson.language : undefined} dir={contentTitle !== undefined ? dirOf(lesson.language) : undefined}>
        {title}
      </h1>
      <p className="lesson-context">
        {lesson.subjectCode} · {LANGUAGE_LABELS[lesson.language]} · {t('admin.lesson.number', { id: lesson.lessonId })} · {t('admin.lesson.publicId')} <code>{lesson.publicId}</code>
        {lesson.archived && ` · ${t('admin.lesson.archivedLower')}`}
      </p>
      <section className="status-card safeguard" aria-labelledby="pointer-heading">
        <h2 id="pointer-heading">{current ? t('admin.lesson.publishedVersion', { number: current.number }) : t('admin.lesson.noPublished')}</h2>
        <p>{t('admin.lesson.loadedAt', { time: formatTime(loadedAt), expected: expectedLabel })}</p>
        <div className="actions">
          {current && (
            <a className="button" href={paths.readerLesson(lesson.publicId)}>
              {t('admin.lesson.viewPublished')}
            </a>
          )}
          <button type="button" onClick={() => void reload().then(() => setError(null))}>
            {t('admin.reloadState')}
          </button>
          {canUnpublish(lesson, spaces) && form !== 'unpublish' && (
            <button type="button" className="danger" onClick={() => openForm('unpublish')}>
              {t('admin.lesson.unpublishUrgent')}
            </button>
          )}
        </div>
        {reasonForm('unpublish', t('admin.lesson.unpublishSubmit'), () => askWithReason({ kind: 'unpublish' }))}
      </section>
      {done && (
        <p className="banner success" role="status" tabIndex={-1} ref={doneRef}>
          {done}
        </p>
      )}
      {error && (
        <div className="banner error" role="alert">
          <p>{error.message}</p>
          {error.stale && (
            <button type="button" onClick={() => void reload().then(() => setError(null))}>
              {t('admin.reloadState')}
            </button>
          )}
        </div>
      )}
      <h2>{lesson.view === 'administrator' ? t('admin.lesson.revisions') : t('admin.lesson.approvedRevisions')}</h2>
      {lesson.revisions.length === 0 && <p>{lesson.view === 'administrator' ? t('admin.lesson.noSubmitted') : t('admin.lesson.noApproved')}</p>}
      <ul className="lesson-list" aria-label={t('admin.lesson.revisionsList')}>
        {lesson.revisions.map((r) => {
          const state = revisionStateLabel(lesson, r);
          const can = revisionActions(lesson, r, spaces);
          return (
            <li key={r.id} className="lesson-card" aria-label={t('admin.lesson.revision', { number: r.number })}>
              <h3>
                {t('admin.lesson.revision', { number: r.number })} ·{' '}
                <span lang={lesson.language} dir={dirOf(lesson.language)}>
                  {r.title}
                </span>
              </h3>
              <p className="lesson-meta">
                <span className={`state state-${state.tone}`}>{state.label}</span>
              </p>
              <p className="lesson-context">
                {r.authorName ? t('admin.lesson.by', { name: r.authorName }) : t('admin.lesson.approved')}
                {!r.submission && r.submittedAt && t('admin.lesson.submittedOn', { date: formatDateTime(r.submittedAt) })}
                {r.approverName && r.approvedAt && ` · ${t('admin.lesson.approvedBy', { name: r.approverName, date: formatDateTime(r.approvedAt) })}`}
                {!r.approverName && r.approvedAt && ` ${t('admin.lesson.approvedOn', { date: formatDateTime(r.approvedAt) })}`}
              </p>
              {r.submission && <p className="lesson-context">{describeSubmission(r, formatDateTime)}</p>}
              {r.withdrawn && !r.withdrawal && <p className="withdrawal">{t('admin.lesson.withdrawn')}</p>}
              {r.withdrawal && (
                <div className="withdrawal">
                  <p>{t('admin.lesson.withdrawnBy', { name: r.withdrawal.withdrawnByName, date: formatDateTime(r.withdrawal.withdrawnAt) })}</p>
                  <Quote>{r.withdrawal.reason}</Quote>
                </div>
              )}
              {(can.restore || (can.withdraw && form !== r.id)) && (
                <div className="actions">
                  {can.restore && (
                    <button type="button" onClick={() => setPending({ kind: 'restore', revision: r })}>
                      {t('admin.lesson.restoreThis')}
                    </button>
                  )}
                  {can.withdraw && form !== r.id && (
                    <button type="button" className="danger" onClick={() => openForm(r.id)}>
                      {t('admin.lesson.withdrawThis')}
                    </button>
                  )}
                </div>
              )}
              {reasonForm(r.id, t('admin.lesson.withdrawSubmit'), () => askWithReason({ kind: 'withdraw', revision: r }))}
            </li>
          );
        })}
      </ul>
      {lesson.view === 'publisher' && (
        <p className="hint">{t('admin.lesson.publisherView')}</p>
      )}
      {lesson.view === 'administrator' && <h2>{t('admin.lesson.publicationHistory')}</h2>}
      {lesson.view === 'publisher' ? null : lesson.history.length === 0 ? (
        <p>{t('admin.lesson.neverPublished')}</p>
      ) : (
        <ol className="history" aria-label={t('admin.lesson.historyList')}>
          {lesson.history.map((e, i) => (
            <li key={i}>
              <p>
                <strong>{describeEvent(e, numberOf)}</strong>
              </p>
              <p className="hint">{t('admin.lesson.byOn', { name: e.actorName, date: formatDateTime(e.occurredAt) })}</p>
              {e.reason && <Quote>{e.reason}</Quote>}
            </li>
          ))}
        </ol>
      )}
      {pending && (
        <Dialog
          title={
            pending.kind === 'withdraw'
              ? t('admin.lesson.confirmWithdraw', { number: pending.revision.number })
              : pending.kind === 'restore'
                ? t('admin.lesson.confirmRestore', { number: pending.revision.number })
                : t('admin.lesson.confirmUnpublish')
          }
          onCancel={() => setPending(null)}
          cancelLabel={t('admin.lesson.changeNothing')}
          actions={[
            {
              label: pending.kind === 'withdraw' ? t('admin.lesson.withdrawForGood') : pending.kind === 'restore' ? t('admin.lesson.restore') : t('admin.lesson.unpublish'),
              kind: pending.kind === 'restore' ? 'primary' : 'danger',
              disabled: busy,
              onClick: () => void run(pending),
            },
          ]}
        >
          {pending.kind === 'withdraw' && (
            <p>
              {t('admin.lesson.withdrawEffect')}
              {lesson.publishedRevisionId === pending.revision.id ? ` ${t('admin.lesson.withdrawPublishedEffect')}` : ''}
            </p>
          )}
          {pending.kind === 'unpublish' && <p>{t('admin.lesson.unpublishEffect')}</p>}
          {pending.kind === 'restore' && <p>{t('admin.lesson.restoreEffect')}</p>}
          <p>{t('admin.lesson.expected', { expected: expectedLabel })}</p>
          {pending.kind !== 'restore' && (
            <>
              <p>{t('admin.reasonColon')}</p>
              <Quote>{reason.trim()}</Quote>
            </>
          )}
        </Dialog>
      )}
    </main>
  );
}
