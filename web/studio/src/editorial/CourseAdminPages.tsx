// Administrator course recovery (LMS-B06): course revisions no one may
// review (named gaps), owners' archive requests, every course, and one
// course's page with its revisions, audit trail and the recovery actions:
// suspend, restore, archive, unarchive, withdraw a revision, discard a
// draft and transfer. Each action asks for a reason (restore has none in
// the API) and a confirmation; pointer actions name the published revision
// this page loaded, so a concurrent change is refused (stale_pointer) and
// nothing changes. No page approves anything on a reviewer's behalf.
import { useEffect, useId, useMemo, useRef, useState } from 'react';
import { isApiError } from '../api/client';
import { courseReviewApi, type AdminCourse, type AdminCourseDetail, type AdminCourseRevision } from '../api/courseReview';
import { describeCourseWorkflowError } from '../api/courseReviewMessages';
import { useStudio } from '../context';
import { courseState, courseTitle } from '../courses/courseLabels';
import { fold } from '../courses/outline';
import { adminCourseActions, describeBlocked, lifecycleLabel, pointerLabel } from '../courses/workflow';
import { COURSE_STATUS_LABELS } from '../courses/courseLabels';
import { paths } from '../editorialRoutes';
import { t, tn } from '../i18n';
import { LANGUAGE_LABELS, formatDateTime } from '../labels';
import { Dialog } from '../ui/Dialog';
import { Link } from '../ui/Link';
import { AdminNav } from './adminCommon';
import { ErrorPage, LoadingPage, NoteField, Quote, dirOf, useLoad } from './common';

function useCourseAdmin() {
  const { api } = useStudio();
  return useMemo(() => courseReviewApi(api), [api]);
}

function CourseStateTag({ course }: { course: AdminCourse }) {
  const state = courseState(course);
  return <span className={`state state-${state.tone}`}>{state.label}</span>;
}

export function AdminCoursesPage() {
  const cr = useCourseAdmin();
  const [blocked] = useLoad(() => cr.blockedReviews(), [cr]);
  const [requests] = useLoad(() => cr.archiveRequests(), [cr]);
  const [list, setList] = useState<AdminCourse[] | null>(null);
  const [next, setNext] = useState<string | null>(null);
  const [listError, setListError] = useState<string | null>(null);
  const [loadingMore, setLoadingMore] = useState(false);
  const load = async (cursor: string | null) => {
    setLoadingMore(true);
    setListError(null);
    try {
      const page = await cr.adminCourses(cursor);
      setList((prev) => (cursor === null ? page.courses : [...(prev ?? []), ...page.courses]));
      setNext(page.nextCursor);
    } catch (e) {
      setListError(describeCourseWorkflowError(e));
    } finally {
      setLoadingMore(false);
    }
  };
  useEffect(() => {
    void load(null);
  }, [cr]);

  return (
    <main className="page" id="main">
      <AdminNav />
      <h1 tabIndex={-1}>{t('admin.courses.title')}</h1>
      <p className="hint">{t('admin.courses.intro')}</p>

      <section aria-labelledby="blocked-reviews-heading">
        <h2 id="blocked-reviews-heading">{t('admin.courses.blocked')}</h2>
        {blocked.state === 'loading' && <p role="status">{t('shell.loadingPage')}</p>}
        {blocked.state === 'error' && (
          <p className="banner error" role="alert">
            {blocked.message}
          </p>
        )}
        {blocked.state === 'ready' && blocked.data.revisions.length === 0 && <p>{t('admin.courses.noBlocked')}</p>}
        {blocked.state === 'ready' && blocked.data.revisions.length > 0 && (
          <ul className="lesson-list" aria-label={t('admin.courses.blockedList')}>
            {blocked.data.revisions.map((item) => {
              const b = item.blocked ? describeBlocked(item.blocked) : null;
              return (
                <li key={item.revisionId} className="lesson-card">
                  <h3 lang={item.language} dir={dirOf(item.language)}>
                    <Link to={paths.adminCourse(item.courseId)}>{courseTitle(item.title, item.courseId)}</Link>
                  </h3>
                  <p className="lesson-meta">
                    <span className="state state-changes">{t('admin.courses.blockedTag')}</span>
                  </p>
                  <p className="lesson-context">
                    {t('admin.course.version', { number: item.number })} · {t('admin.course.by', { name: item.authorName })} ·{' '}
                    {t('admin.course.submittedOn', { date: formatDateTime(item.submittedAt) })}
                  </p>
                  {b && (
                    <div className="lesson-context">
                      <p>{b.summary}</p>
                      {b.details.length > 0 && (
                        <ul>
                          {b.details.map((d) => (
                            <li key={d}>{d}</li>
                          ))}
                        </ul>
                      )}
                    </div>
                  )}
                </li>
              );
            })}
          </ul>
        )}
      </section>

      <section aria-labelledby="archive-requests-heading">
        <h2 id="archive-requests-heading">{t('admin.courses.archiveRequests')}</h2>
        {requests.state === 'loading' && <p role="status">{t('shell.loadingPage')}</p>}
        {requests.state === 'error' && (
          <p className="banner error" role="alert">
            {requests.message}
          </p>
        )}
        {requests.state === 'ready' && requests.data.length === 0 && <p>{t('admin.courses.noArchiveRequest')}</p>}
        {requests.state === 'ready' && requests.data.length > 0 && (
          <ul className="lesson-list" aria-label={t('admin.courses.archiveRequests')}>
            {requests.data.map((r) => (
              <li key={r.id} className="lesson-card">
                <h3>
                  <Link to={paths.adminCourse(r.courseId)}>{courseTitle(r.title, r.courseId)}</Link>
                </h3>
                <p className="lesson-meta">{r.archived ? <span className="state">{t('admin.courses.alreadyArchived')}</span> : <span className="state state-review">{t('admin.courses.toHandle')}</span>}</p>
                <p className="lesson-context">{t('admin.courses.requestedOn', { date: formatDateTime(r.occurredAt) })}</p>
                <Quote>{r.reason}</Quote>
              </li>
            ))}
          </ul>
        )}
      </section>

      <section aria-labelledby="all-courses-heading">
        <h2 id="all-courses-heading">{t('admin.courses.all')}</h2>
        {listError && (
          <div className="banner error" role="alert">
            <p>{listError}</p>
            <button type="button" onClick={() => void load(null)}>
              {t('shell.retry')}
            </button>
          </div>
        )}
        {list === null && !listError && <p role="status">{t('admin.courses.loading')}</p>}
        {list !== null && list.length === 0 && <p>{t('admin.courses.empty')}</p>}
        {list !== null && list.length > 0 && (
          <ul className="lesson-list" aria-label={t('admin.courses.all')}>
            {list.map((c) => (
              <li key={c.id} className="lesson-card">
                <h3 lang={c.language} dir={dirOf(c.language)}>
                  <Link to={paths.adminCourse(c.id)}>{courseTitle(c.title, c.id)}</Link>
                </h3>
                <p className="lesson-meta">
                  <CourseStateTag course={c} />
                  {c.publishedRevisionId !== null && <span className="state state-published">{t('admin.courses.readable')}</span>}
                </p>
                <p className="lesson-context">
                  {LANGUAGE_LABELS[c.language]} · {t('admin.courses.owner', { name: c.ownerName })}
                </p>
              </li>
            ))}
          </ul>
        )}
        {next && (
          <button type="button" onClick={() => void load(next)} disabled={loadingMore}>
            {t('admin.courses.more')}
          </button>
        )}
      </section>
    </main>
  );
}

type ActionKind = 'suspend' | 'restore' | 'archive' | 'unarchive' | 'withdraw' | 'discard' | 'transfer';

interface Pending {
  kind: ActionKind;
  revision?: AdminCourseRevision;
}

const ACTION_TITLES: Record<ActionKind, string> = {
  suspend: t('admin.course.title.suspend'),
  restore: t('admin.course.title.restore'),
  archive: t('admin.course.title.archive'),
  unarchive: t('admin.course.title.unarchive'),
  withdraw: t('admin.course.title.withdraw'),
  discard: t('admin.course.title.discard'),
  transfer: t('admin.course.title.transfer'),
};

const ACTION_EFFECTS: Record<ActionKind, string> = {
  suspend: t('admin.course.effect.suspend'),
  restore: t('admin.course.effect.restore'),
  archive: t('admin.course.effect.archive'),
  unarchive: t('admin.course.effect.unarchive'),
  withdraw: t('admin.course.effect.withdraw'),
  discard: t('admin.course.effect.discard'),
  transfer: t('admin.course.effect.transfer'),
};

function revisionState(d: AdminCourseDetail, r: AdminCourseRevision): string {
  if (r.withdrawnAt) return t('admin.course.state.withdrawn');
  if (d.course.publishedRevisionId === r.id) return t('admin.course.state.published');
  if (r.status === 'approved' && r.everPublished) return t('admin.course.state.publishedBefore');
  if (r.status === 'approved') return t('shell.state.awaitingPublication');
  return COURSE_STATUS_LABELS[r.status];
}

export function AdminCoursePage({ courseId }: { courseId: number }) {
  const cr = useCourseAdmin();
  const [loadedAt, setLoadedAt] = useState(() => new Date());
  const [loaded, reload] = useLoad(async () => {
    const [detail, newer] = await Promise.all([
      cr.adminCourse(courseId),
      cr.newerRevisions(courseId).then(
        (n) => ({ ok: true as const, n }),
        () => ({ ok: false as const, n: null }),
      ),
    ]);
    setLoadedAt(new Date());
    return { detail, newer: newer.n, newerFailed: !newer.ok };
  }, [cr, courseId]);
  const [pending, setPending] = useState<Pending | null>(null);
  const [reason, setReason] = useState('');
  const [reasonError, setReasonError] = useState<string | null>(null);
  const [target, setTarget] = useState('');
  const [authorQuery, setAuthorQuery] = useState('');
  const [targetError, setTargetError] = useState<string | null>(null);
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<{ text: string; stale: boolean } | null>(null);
  const [done, setDone] = useState<string | null>(null);
  const reasonRef = useRef<HTMLTextAreaElement>(null);
  const resultRef = useRef<HTMLDivElement>(null);
  const formId = useId();

  useEffect(() => {
    if (pending) reasonRef.current?.focus();
  }, [pending]);
  useEffect(() => {
    if (done || error) resultRef.current?.focus();
  }, [done, error]);

  if (loaded.state === 'loading') return <LoadingPage label={t('admin.course.loading')} />;
  if (loaded.state === 'error')
    return (
      <ErrorPage
        title={t('admin.course.notFoundTitle')}
        message={loaded.notFound ? t('admin.course.notFound') : loaded.message}
        back={{ to: paths.adminCourses, label: t('admin.courses.backTo') }}
      />
    );
  const { detail, newer, newerFailed } = loaded.data;
  const c = detail.course;
  const actions = adminCourseActions(detail);
  const byId = new Map(detail.revisions.map((r) => [r.id, r]));
  const numberOf = (id: number | null) => (id === null ? '—' : byId.has(id) ? t('admin.course.versionRef', { number: byId.get(id)!.number }) : t('admin.course.versionId', { id }));
  const needsReason = (k: ActionKind) => k !== 'restore';
  const q = fold(authorQuery.trim());
  const matchingAuthors = detail.authors.filter((a) => q === '' || fold(a.displayName).includes(q) || String(a.id) === target);

  const open = (p: Pending) => {
    setPending(p);
    setReason('');
    setReasonError(null);
    setTarget('');
    setAuthorQuery('');
    setTargetError(null);
    setError(null);
    setDone(null);
  };

  const check = () => {
    if (!pending) return;
    let ok = true;
    if (pending.kind === 'transfer' && !target) {
      setTargetError(t('admin.course.chooseOwner'));
      ok = false;
    }
    if (needsReason(pending.kind) && !reason.trim()) {
      setReasonError(t('admin.reasonRequired'));
      reasonRef.current?.focus();
      ok = false;
    }
    if (ok) setConfirming(true);
  };

  const run = async () => {
    if (!pending) return;
    setBusy(true);
    const text = reason.trim();
    const expected = c.publishedRevisionId;
    try {
      let message: string;
      switch (pending.kind) {
        case 'suspend':
          await cr.suspend(c.id, text, expected);
          message = t('admin.course.suspended');
          break;
        case 'restore':
          await cr.restore(c.id, pending.revision!.id, expected);
          message = t('admin.course.restored', { number: pending.revision!.number });
          break;
        case 'archive':
          await cr.archive(c.id, text, expected);
          message = t('admin.course.archived');
          break;
        case 'unarchive':
          await cr.unarchive(c.id, text);
          message = t('admin.course.unarchived');
          break;
        case 'withdraw':
          await cr.withdraw(pending.revision!.id, text);
          message = t('admin.course.withdrawn', { number: pending.revision!.number });
          break;
        case 'discard':
          await cr.discardDraft(pending.revision!.id, text);
          message = t('admin.course.discarded', { number: pending.revision!.number });
          break;
        case 'transfer': {
          const to = detail.authors.find((a) => a.id === Number(target));
          await cr.transfer(c.id, Number(target), text);
          message = t('admin.course.transferred', { name: to?.displayName ?? t('admin.course.accountId', { id: target }) });
          break;
        }
      }
      setConfirming(false);
      setPending(null);
      setDone(message);
      await reload();
    } catch (e) {
      setConfirming(false);
      setError({ text: describeCourseWorkflowError(e), stale: isApiError(e, 'stale_pointer') });
    } finally {
      setBusy(false);
    }
  };

  const revisionButtons = (r: AdminCourseRevision) => (
    <div className="actions">
      {actions.restore.includes(r) && (
        <button type="button" onClick={() => open({ kind: 'restore', revision: r })}>
          {t('admin.course.restoreVersion', { number: r.number })}
        </button>
      )}
      {actions.withdraw.includes(r) && (
        <button type="button" className="danger" onClick={() => open({ kind: 'withdraw', revision: r })}>
          {t('admin.course.withdrawVersion', { number: r.number })}
        </button>
      )}
      {actions.discard.includes(r) && (
        <button type="button" className="danger" onClick={() => open({ kind: 'discard', revision: r })}>
          {t('admin.course.discardDraft', { number: r.number })}
        </button>
      )}
    </div>
  );

  const pendingTitle = pending
    ? pending.revision
      ? t('admin.course.titleWithVersion', { title: ACTION_TITLES[pending.kind], number: pending.revision.number })
      : ACTION_TITLES[pending.kind]
    : '';

  return (
    <main className="page" id="main">
      <AdminNav />
      <p className="breadcrumb">
        <Link to={paths.adminCourses}>{t('admin.courses.title')}</Link>
      </p>
      <h1 tabIndex={-1} lang={c.language} dir={dirOf(c.language)}>
        {courseTitle(c.title, c.id)}
      </h1>
      <p className="lesson-meta">
        <CourseStateTag course={c} />
      </p>
      <dl className="facts">
        <div>
          <dt>{t('admin.course.owner')}</dt>
          <dd>{c.ownerName}</dd>
        </div>
        <div>
          <dt>{t('admin.course.language')}</dt>
          <dd>{LANGUAGE_LABELS[c.language]}</dd>
        </div>
        <div>
          <dt>{t('admin.course.readVersion')}</dt>
          <dd>{c.publishedRevisionId === null ? t('admin.course.none') : numberOf(c.publishedRevisionId)}</dd>
        </div>
        <div>
          <dt>{t('admin.course.loadedAt')}</dt>
          <dd>{formatDateTime(loadedAt.toISOString())}</dd>
        </div>
      </dl>

      <div aria-live="polite">
        {done && (
          <div className="banner success" role="status" tabIndex={-1} ref={resultRef}>
            <p>{done}</p>
          </div>
        )}
        {error && (
          <div className="banner error" role="alert" tabIndex={-1} ref={resultRef}>
            <p>{error.text}</p>
            {error.stale && (
              <button type="button" onClick={() => { setError(null); void reload(); }}>
                {t('admin.reloadState')}
              </button>
            )}
          </div>
        )}
      </div>

      {newerFailed && (
        <div className="banner error" role="alert">
          <p>{t('admin.course.newerFailed')}</p>
          <button type="button" onClick={() => void reload()}>
            {t('shell.retry')}
          </button>
        </div>
      )}
      {newer && newer.steps.length > 0 && (
        <section className="status-card warning" aria-labelledby="newer-heading">
          <h2 id="newer-heading">{t('admin.course.newerTitle')}</h2>
          <ul>
            {newer.steps.map((s) => (
              <li key={s.lessonId}>
                {t('admin.course.newerStep', { title: s.title, pinned: s.pinnedNumber, newer: s.newerNumber })}
              </li>
            ))}
          </ul>
          <p>{t('admin.course.newerOwner')}</p>
        </section>
      )}

      <section className="status-card safeguard" aria-labelledby="course-actions-heading">
        <h2 id="course-actions-heading">{t('admin.course.actions')}</h2>
        <div className="actions">
          {actions.suspend && (
            <button type="button" className="danger" onClick={() => open({ kind: 'suspend' })}>
              {t('admin.course.suspend')}
            </button>
          )}
          {actions.archive && (
            <button type="button" className="danger" onClick={() => open({ kind: 'archive' })}>
              {t('admin.course.archive')}
            </button>
          )}
          {actions.unarchive && (
            <button type="button" onClick={() => open({ kind: 'unarchive' })}>
              {t('admin.course.unarchive')}
            </button>
          )}
          <button type="button" onClick={() => open({ kind: 'transfer' })} disabled={!actions.transfer} aria-describedby={actions.transfer ? undefined : `${formId}-transfer`}>
            {t('admin.course.transfer')}
          </button>
        </div>
        {!actions.transfer && (
          <p className="hint" id={`${formId}-transfer`}>
            {t('admin.course.transferBlocked')}
          </p>
        )}
        {pending && (
          <form
            className="reason-form"
            aria-labelledby={`${formId}-title`}
            onSubmit={(e) => {
              e.preventDefault();
              check();
            }}
            noValidate
          >
            <h3 id={`${formId}-title`}>{pendingTitle}</h3>
            <p>{ACTION_EFFECTS[pending.kind]}</p>
            {pending.kind === 'transfer' && (
              <div className="fields">
                <label htmlFor={`${formId}-search`}>{t('admin.course.searchAuthor')}</label>
                <input id={`${formId}-search`} type="search" value={authorQuery} onChange={(e) => setAuthorQuery(e.target.value)} aria-describedby={`${formId}-count`} />
                <p className="hint" id={`${formId}-count`} role="status">
                  {tn('admin.course.activeAuthors', matchingAuthors.length)}
                  {detail.authors.length >= 200 ? ` ${t('admin.course.first200')}` : ''}
                </p>
                <label htmlFor={`${formId}-target`}>{t('admin.course.newOwner')}</label>
                {targetError && (
                  <p className="field-error" id={`${formId}-target-error`}>
                    {targetError}
                  </p>
                )}
                <select
                  id={`${formId}-target`}
                  value={target}
                  aria-invalid={targetError ? true : undefined}
                  aria-describedby={targetError ? `${formId}-target-error` : undefined}
                  onChange={(e) => {
                    setTarget(e.target.value);
                    setTargetError(null);
                  }}
                >
                  <option value="">{t('admin.course.chooseAuthor')}</option>
                  {matchingAuthors.map((a) => (
                    <option key={a.id} value={a.id}>
                      {a.displayName}
                    </option>
                  ))}
                </select>
              </div>
            )}
            {needsReason(pending.kind) && (
              <NoteField
                id={`${formId}-reason`}
                label={t('admin.course.reason')}
                hint={t('admin.course.reasonHint')}
                value={reason}
                onChange={(v) => {
                  setReason(v);
                  if (reasonError && v.trim()) setReasonError(null);
                }}
                error={reasonError}
                fieldRef={reasonRef}
              />
            )}
            <div className="actions">
              <button type="submit" className={pending.kind === 'restore' || pending.kind === 'unarchive' ? 'primary' : 'danger'}>
                {t('admin.course.continue')}
              </button>
              <button type="button" onClick={() => setPending(null)}>
                {t('shell.cancel')}
              </button>
            </div>
          </form>
        )}
      </section>

      <section className="panel" aria-labelledby="revisions-heading">
        <h2 id="revisions-heading">{t('admin.course.versions')}</h2>
        {detail.revisions.length === 0 ? (
          <p>{t('admin.course.noVersion')}</p>
        ) : (
          <ul className="lesson-list" aria-label={t('admin.course.versions')}>
            {detail.revisions.map((r) => (
              <li key={r.id} className="lesson-card">
                <h3>
                  {t('admin.course.version', { number: r.number })} · {revisionState(detail, r)}
                </h3>
                <p className="lesson-context">
                  {r.kind === 'amendment' ? t('admin.course.amendment') : t('admin.course.ordinary')} · {t('admin.course.by', { name: r.authorName })}
                  {r.submittedAt && ` · ${t('admin.course.submittedOn', { date: formatDateTime(r.submittedAt) })}`}
                  {r.review && ` · ${t(r.review.action === 'approve' ? 'admin.course.approvedBy' : 'admin.course.changesBy', { name: r.review.reviewerName })}`}
                </p>
                {(r.status === 'in_review' || r.status === 'approved') && r.withdrawnAt === null && (
                  <p className="lesson-context">
                    <Link to={paths.reviewCourse(r.id)}>{t('admin.course.read')}</Link>
                  </p>
                )}
                {revisionButtons(r)}
              </li>
            ))}
          </ul>
        )}
      </section>

      <section className="panel" aria-labelledby="history-heading">
        <h2 id="history-heading">{t('admin.history')}</h2>
        {detail.history.length === 0 && detail.lifecycle.length === 0 ? (
          <p>{t('admin.course.noHistory')}</p>
        ) : (
          <ul className="history" aria-label={t('admin.course.historyList')}>
            {[
              ...detail.history.map((e, i) => ({ at: e.occurredAt, key: `p-${i}`, label: e.publishedRevisionId !== null ? t('admin.course.pointerOf', { action: pointerLabel(e.action), version: numberOf(e.publishedRevisionId) }) : pointerLabel(e.action), actor: e.actorName, reason: e.reason })),
              ...detail.lifecycle.map((e) => ({ at: e.occurredAt, key: `l-${e.id}`, label: lifecycleLabel(e.action), actor: e.actorName, reason: e.reason })),
            ]
              .sort((a, b) => b.at.localeCompare(a.at))
              .map((e) => (
                <li key={e.key}>
                  <p>
                    {e.label} · {e.actor} · {formatDateTime(e.at)}
                  </p>
                  {e.reason && <Quote>{e.reason}</Quote>}
                </li>
              ))}
          </ul>
        )}
      </section>

      {confirming && pending && (
        <Dialog
          title={t('admin.course.confirmTitle', { title: pendingTitle })}
          onCancel={() => setConfirming(false)}
          cancelLabel={t('admin.course.doNothing')}
          actions={[{ label: ACTION_TITLES[pending.kind], kind: pending.kind === 'restore' || pending.kind === 'unarchive' ? 'primary' : 'danger', disabled: busy, onClick: () => void run() }]}
        >
          <p>{ACTION_EFFECTS[pending.kind]}</p>
          {needsReason(pending.kind) && (
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
