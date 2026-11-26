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

export const REPORT_STATUS_LABELS: Record<ReportStatus, string> = { open: 'À traiter', resolved: 'Corrigé', dismissed: 'Écarté' };

const FILTERS: Array<{ value: ReportStatus | ''; label: string }> = [
  { value: 'open', label: 'À traiter' },
  { value: 'resolved', label: 'Corrigés' },
  { value: 'dismissed', label: 'Écartés' },
  { value: '', label: 'Tous' },
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
      <h1 tabIndex={-1}>Signalements</h1>
      <p className="hint">Erreurs signalées par les lecteurs sur les leçons publiées de vos matières. Un signalement ne modifie jamais la leçon : une correction passe par une nouvelle révision, relue puis publiée.</p>
      <div className="fields inline-select">
        <label htmlFor="report-status">Afficher</label>
        <select id="report-status" value={status} onChange={(e) => setStatus(e.target.value as ReportStatus | '')}>
          {FILTERS.map((f) => (
            <option key={f.value} value={f.value}>
              {f.label}
            </option>
          ))}
        </select>
      </div>
      {reports.state === 'loading' && <p role="status">Chargement des signalements…</p>}
      {reports.state === 'error' && (
        <p className="banner error" role="alert">
          {reports.message}
        </p>
      )}
      {reports.state === 'ready' && reports.data.length === 0 && <p>Aucun signalement dans cette catégorie.</p>}
      {reports.state === 'ready' && reports.data.length > 0 && (
        <ul className="lesson-list" aria-label="Signalements">
          {reports.data.map((r) => (
            <li key={r.id} className="lesson-card">
              <h2>
                <Link to={paths.report(r.id)}>
                  Signalement n° {r.id} · {r.revisionTitle}
                </Link>
              </h2>
              <p className="lesson-meta">
                <span className={`state state-report-${r.status}`}>{REPORT_STATUS_LABELS[r.status]}</span>
                {!r.currentlyPublished && <span className="state">Révision signalée plus publiée</span>}
              </p>
              <p className="lesson-context">
                <SubjectName subjects={subjects} id={r.subjectId} /> · révision {r.revisionNumber} · reçu le {formatDateTime(r.createdAt)}
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

  if (loaded.state === 'loading') return <LoadingPage label="Chargement du signalement…" />;
  if (loaded.state === 'error')
    return (
      <ErrorPage
        title="Signalement introuvable"
        message={loaded.notFound ? 'Ce signalement n’existe pas ou concerne une matière que vous ne relisez pas.' : loaded.message}
        back={{ to: paths.reports, label: 'Revenir aux signalements' }}
      />
    );
  const report: ErrorReport = loaded.data;

  const ask = (action: Closing) => {
    if (!note.trim()) {
      setNoteError('Rédigez une note de traitement : elle est obligatoire.');
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
      setDone(action === 'resolve' ? 'Signalement marqué comme corrigé.' : 'Signalement écarté.');
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
        <Link to={paths.reports}>Signalements</Link>
      </p>
      <h1 tabIndex={-1}>Signalement n° {report.id}</h1>
      <p className="lesson-meta">
        <span className={`state state-report-${report.status}`}>{REPORT_STATUS_LABELS[report.status]}</span>
      </p>
      <dl className="facts">
        <div>
          <dt>Leçon</dt>
          <dd dir="auto">{report.revisionTitle}</dd>
        </div>
        <div>
          <dt>Matière</dt>
          <dd><SubjectName subjects={subjects} id={report.subjectId} /></dd>
        </div>
        <div>
          <dt>Révision lue par le lecteur</dt>
          <dd>
            n° {report.revisionNumber} · {report.currentlyPublished ? 'toujours la version publiée' : 'n’est plus la version publiée'}
          </dd>
        </div>
        <div>
          <dt>Reçu le</dt>
          <dd>{formatDateTime(report.createdAt)}</dd>
        </div>
      </dl>
      <section className="panel" aria-labelledby="message-heading">
        <h2 id="message-heading">Message du lecteur</h2>
        <Quote>{report.message}</Quote>
        <ul className="link-list">
          <li>
            <a href={paths.readerLesson(report.lessonPublicId)}>Ouvrir la leçon publiée dans le lecteur</a>
          </li>
          {canSeePublication(spaces) && (
            <li>
              <Link to={paths.publication(report.lessonPublicId)}>Historique de publication de la leçon</Link>
            </li>
          )}
        </ul>
        <p className="hint">Une correction se fait par une nouvelle révision de l’auteur, relue puis publiée ; le signalement ne modifie pas la leçon.</p>
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
              Afficher l’état actuel
            </button>
          )}
        </div>
      )}
      {report.resolution ? (
        <section className="status-card" aria-labelledby="resolution-heading">
          <h2 id="resolution-heading">{report.resolution.action === 'resolve' ? 'Corrigé' : 'Écarté'}</h2>
          <p className="hint">Le {formatDateTime(report.resolution.occurredAt)}. Un signalement traité ne peut pas être rouvert.</p>
          <Quote>{report.resolution.note}</Quote>
        </section>
      ) : (
        <section className="status-card" aria-labelledby="close-heading">
          <h2 id="close-heading">Traiter le signalement</h2>
          <NoteField
            id="report-note"
            label="Note de traitement (obligatoire)"
            hint="Par exemple la révision qui corrige l’erreur, ou pourquoi le signalement est écarté. 2 000 caractères au maximum."
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
              Marquer comme corrigé…
            </button>
            <button type="button" onClick={() => ask('dismiss')} disabled={busy}>
              Écarter…
            </button>
          </div>
        </section>
      )}
      {asking && (
        <Dialog
          title={asking === 'resolve' ? 'Marquer le signalement comme corrigé ?' : 'Écarter le signalement ?'}
          onCancel={() => setAsking(null)}
          cancelLabel="Revenir au signalement"
          actions={[{ label: asking === 'resolve' ? 'Marquer comme corrigé' : 'Écarter', kind: 'primary', disabled: busy, onClick: () => void close(asking) }]}
        >
          <p>Un signalement traité ne peut pas être rouvert. Votre note est conservée avec lui.</p>
          <Quote>{note.trim()}</Quote>
        </Dialog>
      )}
    </main>
  );
}
