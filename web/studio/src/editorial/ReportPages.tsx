// Error reports filed by readers (LRN-015): listed, read and closed by
// reviewers of the lesson's subject and by administrators, through
// GET/POST /api/review/error-reports. A report never changes the lesson:
// a correction is a new revision that goes through review and publication.
// The reporter's message is user text, always shown as plain text.
import { useRef, useState } from 'react';
import { isApiError } from '../api/client';
import type { ErrorReport, ReportStatus } from '../api/editorial';
import { describeEditorialError } from '../api/editorialMessages';
import { useStudio } from '../context';
import { paths } from '../editorialRoutes';
import { formatDateTime } from '../labels';
import { Dialog } from '../ui/Dialog';
import { Link } from '../ui/Link';
import { ErrorPage, LoadingPage, NoteField, Quote, SubjectName, useEditorial, useLoad, useSubjects } from './common';
import { canSeePublication } from '../spaces';
import { t } from '../i18n';

export const REPORT_STATUS_LABELS: Record<ReportStatus, string> = {
  open: t('review.report.status.open'),
  resolved: t('review.report.status.resolved'),
  dismissed: t('review.report.status.dismissed'),
};

const FILTERS: Array<{ value: ReportStatus | ''; label: string }> = [
  { value: 'open', label: t('review.report.status.open') },
  { value: 'resolved', label: t('review.report.filter.resolved') },
  { value: 'dismissed', label: t('review.report.filter.dismissed') },
  { value: '', label: t('review.report.filter.all') },
];

/** A one-line excerpt of a report message (plain text). */
export function excerpt(message: string, max = 160): string {
  const flat = message.replace(/\s+/g, ' ').trim();
  return flat.length <= max ? flat : `${[...flat].slice(0, max).join('').trimEnd()}…`;
}

export function ReportListPage() {
  const ed = useEditorial();
  const subjects = useSubjects();
  const [status, setStatus] = useState<ReportStatus | ''>('open');
  const [reports] = useLoad(() => ed.reports(status || undefined), [ed, status]);
  return (
    <main className="page" id="main">
      <h1 tabIndex={-1}>{t('shell.space.reports')}</h1>
      <p className="hint">{t('review.report.hint')}</p>
      <div className="fields inline-select">
        <label htmlFor="report-status">{t('review.report.show')}</label>
        <select id="report-status" value={status} onChange={(e) => setStatus(e.target.value as ReportStatus | '')}>
          {FILTERS.map((f) => (
            <option key={f.value} value={f.value}>
              {f.label}
            </option>
          ))}
        </select>
      </div>
      {reports.state === 'loading' && <p role="status">{t('review.report.loadingList')}</p>}
      {reports.state === 'error' && (
        <p className="banner error" role="alert">
          {reports.message}
        </p>
      )}
      {reports.state === 'ready' && reports.data.length === 0 && <p>{t('review.report.emptyList')}</p>}
      {reports.state === 'ready' && reports.data.length > 0 && (
        <ul className="lesson-list" aria-label={t('shell.space.reports')}>
          {reports.data.map((r) => (
            <li key={r.id} className="lesson-card">
              <h2>
                <Link to={paths.report(r.id)}>
                  {t('review.report.number', { id: r.id })} · <bdi>{r.revisionTitle}</bdi>
                </Link>
              </h2>
              <p className="lesson-meta">
                <span className={`state state-report-${r.status}`}>{REPORT_STATUS_LABELS[r.status]}</span>
                {!r.currentlyPublished && <span className="state">{t('review.report.notPublished')}</span>}
              </p>
              <p className="lesson-context">
                <SubjectName subjects={subjects} id={r.subjectId} /> · {t('review.report.context', { number: r.revisionNumber, date: formatDateTime(r.createdAt) })}
              </p>
              <p className="excerpt" dir="auto">
                {excerpt(r.message)}
              </p>
            </li>
          ))}
        </ul>
      )}
    </main>
  );
}

type Closing = 'resolve' | 'dismiss';

export function ReportPage({ reportId }: { reportId: number }) {
  const ed = useEditorial();
  const { spaces } = useStudio();
  const subjects = useSubjects();
  const [loaded, reload] = useLoad(() => ed.report(reportId), [ed, reportId]);
  const [note, setNote] = useState('');
  const [noteError, setNoteError] = useState<string | null>(null);
  const [asking, setAsking] = useState<Closing | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<{ message: string; conflict: boolean } | null>(null);
  const [done, setDone] = useState<string | null>(null);
  const field = useRef<HTMLTextAreaElement>(null);

  if (loaded.state === 'loading') return <LoadingPage label={t('review.report.loading')} />;
  if (loaded.state === 'error')
    return (
      <ErrorPage
        title={t('review.report.notFoundTitle')}
        message={loaded.notFound ? t('review.report.notFound') : loaded.message}
        back={{ to: paths.reports, label: t('review.report.back') }}
      />
    );
  const report: ErrorReport = loaded.data;

  const ask = (action: Closing) => {
    if (!note.trim()) {
      setNoteError(t('review.error.note_required'));
      field.current?.focus();
      return;
    }
    setNoteError(null);
    setError(null);
    setAsking(action);
  };

  const close = async (action: Closing) => {
    setBusy(true);
    try {
      await ed.resolveReport(report.id, action, note);
      setAsking(null);
      setNote('');
      setDone(action === 'resolve' ? t('review.report.resolvedDone') : t('review.report.dismissedDone'));
      await reload();
    } catch (e) {
      setAsking(null);
      setError({ message: describeEditorialError(e), conflict: isApiError(e, 'not_open') });
      if (isApiError(e, 'note_required') || isApiError(e, 'invalid_note')) field.current?.focus();
    } finally {
      setBusy(false);
    }
  };

  return (
    <main className="page" id="main">
      <p className="breadcrumb">
        <Link to={paths.reports}>{t('shell.space.reports')}</Link>
      </p>
      <h1 tabIndex={-1}>{t('review.report.number', { id: report.id })}</h1>
      <p className="lesson-meta">
        <span className={`state state-report-${report.status}`}>{REPORT_STATUS_LABELS[report.status]}</span>
      </p>
      <dl className="facts">
        <div>
          <dt>{t('review.fact.lesson')}</dt>
          <dd dir="auto">{report.revisionTitle}</dd>
        </div>
        <div>
          <dt>{t('review.fact.subject')}</dt>
          <dd><SubjectName subjects={subjects} id={report.subjectId} /></dd>
        </div>
        <div>
          <dt>{t('review.report.readRevision')}</dt>
          <dd>
            {t(report.currentlyPublished ? 'review.report.stillPublished' : 'review.report.noLongerPublished', { number: report.revisionNumber })}
          </dd>
        </div>
        <div>
          <dt>{t('review.report.receivedOn')}</dt>
          <dd>{formatDateTime(report.createdAt)}</dd>
        </div>
      </dl>
      <section className="panel" aria-labelledby="message-heading">
        <h2 id="message-heading">{t('review.report.message')}</h2>
        <Quote>{report.message}</Quote>
        <ul className="link-list">
          <li>
            <a href={paths.readerLesson(report.lessonPublicId)}>{t('review.report.openLesson')}</a>
          </li>
          {canSeePublication(spaces) && (
            <li>
              <Link to={paths.publication(report.lessonPublicId)}>{t('review.lessonPublicationHistory')}</Link>
            </li>
          )}
        </ul>
        <p className="hint">{t('review.report.correctionHint')}</p>
      </section>
      {done && (
        <p className="banner success" role="status">
          {done}
        </p>
      )}
      {error && (
        <div className="banner error" role="alert">
          <p>{error.message}</p>
          {error.conflict && (
            <button type="button" onClick={() => void reload().then(() => setError(null))}>
              {t('review.report.showCurrent')}
            </button>
          )}
        </div>
      )}
      {report.resolution ? (
        <section className="status-card" aria-labelledby="resolution-heading">
          <h2 id="resolution-heading">{REPORT_STATUS_LABELS[report.resolution.action === 'resolve' ? 'resolved' : 'dismissed']}</h2>
          <p className="hint">{t('review.report.closedOn', { date: formatDateTime(report.resolution.occurredAt) })}</p>
          <Quote>{report.resolution.note}</Quote>
        </section>
      ) : (
        <section className="status-card" aria-labelledby="close-heading">
          <h2 id="close-heading">{t('review.report.handle')}</h2>
          <NoteField
            id="report-note"
            label={t('review.report.noteLabel')}
            hint={t('review.report.noteHint')}
            value={note}
            onChange={(v) => {
              setNote(v);
              if (noteError && v.trim()) setNoteError(null);
            }}
            error={noteError}
            fieldRef={field}
          />
          <div className="actions">
            <button type="button" className="primary" onClick={() => ask('resolve')} disabled={busy}>
              {t('review.report.resolveButton')}
            </button>
            <button type="button" onClick={() => ask('dismiss')} disabled={busy}>
              {t('review.report.dismissButton')}
            </button>
          </div>
        </section>
      )}
      {asking && (
        <Dialog
          title={asking === 'resolve' ? t('review.report.resolveTitle') : t('review.report.dismissTitle')}
          onCancel={() => setAsking(null)}
          cancelLabel={t('review.report.backToReport')}
          actions={[{ label: asking === 'resolve' ? t('review.report.resolve') : t('review.report.dismiss'), kind: 'primary', disabled: busy, onClick: () => void close(asking) }]}
        >
          <p>{t('review.report.closeText')}</p>
          <Quote>{note.trim()}</Quote>
        </Dialog>
      )}
    </main>
  );
}
