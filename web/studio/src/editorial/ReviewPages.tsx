// Reviewer space: the queue of revisions in review in the reviewer's
// subjects (GET /api/review/queue) and the review of one revision, read
// through the canonical preview, with approve (optional comment) and
// request changes (required comment), each confirmed first. The server
// decides who may review what (assignment per subject, no self-review);
// this page only explains its answers.
import { useRef, useState } from 'react';
import { isApiError, type Revision } from '../api/client';
import type { ReviewQueueItem } from '../api/editorial';
import { describeEditorialError } from '../api/editorialMessages';
import { setFlash, takeFlash, useStudio } from '../context';
import { paths } from '../editorialRoutes';
import { LANGUAGE_LABELS, formatDateTime } from '../labels';
import { navigate } from '../router';
import { Dialog } from '../ui/Dialog';
import { Link } from '../ui/Link';
import { CanonicalPreview, ErrorPage, LoadingPage, NoteField, Quote, dirOf, subjectLabel, useEditorial, useLoad, useSubjectCodes } from './common';

/** Queue items grouped by subject, subjects in code order (then ID). */
export function groupBySubject<T extends { subjectId: number }>(items: T[], codes: Map<number, string>): Array<{ subjectId: number; items: T[] }> {
  const groups = new Map<number, T[]>();
  for (const item of items) groups.set(item.subjectId, [...(groups.get(item.subjectId) ?? []), item]);
  return [...groups.entries()]
    .sort(([a], [b]) => subjectLabel(codes, a).localeCompare(subjectLabel(codes, b)) || a - b)
    .map(([subjectId, list]) => ({ subjectId, items: list }));
}

export function ReviewQueuePage() {
  const ed = useEditorial();
  const codes = useSubjectCodes();
  const [queue] = useLoad(() => ed.reviewQueue(), [ed]);
  const [flash] = useState(takeFlash);
  return (
    <main className="page" id="main">
      <h1 tabIndex={-1}>Relecture</h1>
      <p className="hint">Révisions soumises dans les matières dont vous êtes relecteur, les plus anciennes d’abord. Vos propres révisions n’y figurent pas : un autre relecteur doit les relire.</p>
      {flash && (
        <p className="banner success" role="status">
          {flash}
        </p>
      )}
      {queue.state === 'loading' && <p role="status">Chargement de la file de relecture…</p>}
      {queue.state === 'error' && (
        <p className="banner error" role="alert">
          {queue.message}
        </p>
      )}
      {queue.state === 'ready' && queue.data.length === 0 && <p>Aucune révision n’attend de relecture dans vos matières.</p>}
      {queue.state === 'ready' &&
        groupBySubject(queue.data, codes).map((group) => (
          <section key={group.subjectId} aria-labelledby={`subject-${group.subjectId}`}>
            <h2 id={`subject-${group.subjectId}`}>
              {subjectLabel(codes, group.subjectId)} ({group.items.length})
            </h2>
            <ul className="lesson-list" aria-label={`À relire en ${subjectLabel(codes, group.subjectId)}`}>
              {group.items.map((item) => (
                <QueueCard key={item.revisionId} item={item} />
              ))}
            </ul>
          </section>
        ))}
    </main>
  );
}

function QueueCard({ item }: { item: ReviewQueueItem }) {
  return (
    <li className="lesson-card">
      <h3 lang={item.language} dir={dirOf(item.language)}>
        <Link to={paths.reviewRevision(item.revisionId)}>{item.title || `Révision n° ${item.revisionId}`}</Link>
      </h3>
      <p className="lesson-meta">
        <span className="state state-review">En relecture</span>
      </p>
      <p className="lesson-context">
        Révision {item.number} · {LANGUAGE_LABELS[item.language]} · par {item.authorName} · soumise le {formatDateTime(item.submittedAt)}
      </p>
    </li>
  );
}

interface ReviewData {
  revision: Revision;
  authorName: string | null;
  subjectCode: string | null;
  chapterCode: string | null;
}

type Decision = 'approve' | 'request_changes';

export function ReviewRevisionPage({ revisionId }: { revisionId: number }) {
  const { api, session } = useStudio();
  const ed = useEditorial();
  const [loaded] = useLoad<ReviewData>(async () => {
    const revision = await ed.reviewRevision(revisionId);
    // Context only: a failure here does not block the review.
    const [queue, subjects] = await Promise.all([ed.reviewQueue(revision.subjectId).catch(() => []), api.subjects().catch(() => [])]);
    const subjectCode = subjects.find((s) => s.id === revision.subjectId)?.code ?? null;
    let chapterCode: string | null = null;
    if (subjectCode && revision.chapterId !== null) {
      chapterCode = (await api.chapters(subjectCode).catch(() => null))?.chapters.find((c) => c.id === revision.chapterId)?.code ?? null;
    }
    return { revision, authorName: queue.find((q) => q.revisionId === revisionId)?.authorName ?? null, subjectCode, chapterCode };
  }, [ed, api, revisionId]);

  if (loaded.state === 'loading') return <LoadingPage label="Chargement de la révision…" />;
  if (loaded.state === 'error')
    return (
      <ErrorPage
        title="Révision indisponible"
        message={
          loaded.notFound
            ? 'Cette révision n’est pas en relecture dans l’une de vos matières : elle n’existe pas, a déjà été décidée, ou relève d’une matière que vous ne relisez pas.'
            : loaded.message
        }
        back={{ to: paths.reviewQueue, label: 'Revenir à la file de relecture' }}
      />
    );
  const { revision, authorName, subjectCode, chapterCode } = loaded.data;
  const own = revision.authorId === session.user.id;
  const dir = dirOf(revision.language);
  return (
    <main className="page" id="main">
      <p className="breadcrumb">
        <Link to={paths.reviewQueue}>Relecture</Link>
      </p>
      <h1 tabIndex={-1} lang={revision.language} dir={dir}>
        {revision.title}
      </h1>
      <p className="lesson-meta">
        <span className="state state-review">En relecture</span>
      </p>
      <dl className="facts">
        <div>
          <dt>Auteur</dt>
          <dd>{own ? 'Vous' : (authorName ?? `compte n° ${revision.authorId}`)}</dd>
        </div>
        <div>
          <dt>Matière</dt>
          <dd>
            {subjectCode ?? `matière n° ${revision.subjectId}`}
            {revision.chapterId !== null && ` · ${chapterCode ?? `chapitre n° ${revision.chapterId}`}`}
          </dd>
        </div>
        <div>
          <dt>Langue</dt>
          <dd>{LANGUAGE_LABELS[revision.language]}</dd>
        </div>
        <div>
          <dt>Révision</dt>
          <dd>
            n° {revision.number}
            {revision.submittedAt && `, soumise le ${formatDateTime(revision.submittedAt)}`}
          </dd>
        </div>
        {revision.prerequisites && (
          <div>
            <dt>Prérequis</dt>
            <dd lang={revision.language} dir={dir} className="pre-line">
              {revision.prerequisites}
            </dd>
          </div>
        )}
      </dl>
      {own ? (
        <section className="status-card" aria-labelledby="own-heading">
          <h2 id="own-heading">Vous êtes l’auteur de cette révision</h2>
          <p>Vous pouvez la lire, mais pas la relire : le serveur refuse qu’un auteur approuve ou renvoie sa propre révision. Un autre relecteur de la matière doit s’en charger.</p>
        </section>
      ) : (
        <DecisionForm revision={revision} />
      )}
      {revision.mediaIds.length > 0 && (
        <section className="panel" aria-labelledby="media-heading">
          <h2 id="media-heading">Images jointes ({revision.mediaIds.length})</h2>
          <ul className="media-list" aria-label="Images jointes">
            {revision.mediaIds.map((id) => (
              <li key={id}>
                <img src={`/api/review/media/${id}`} alt={`Image jointe n° ${id}`} />
                <span>Image n° {id}</span>
              </li>
            ))}
          </ul>
        </section>
      )}
      <section className="panel" aria-labelledby="preview-heading">
        <h2 id="preview-heading">Leçon telle que l’élève la lira</h2>
        <CanonicalPreview content={revision.content} testId="review-preview" />
      </section>
    </main>
  );
}

function DecisionForm({ revision }: { revision: Revision }) {
  const ed = useEditorial();
  const [comment, setComment] = useState('');
  const [fieldError, setFieldError] = useState<string | null>(null);
  const [asking, setAsking] = useState<Decision | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const field = useRef<HTMLTextAreaElement>(null);

  const ask = (decision: Decision) => {
    setError(null);
    if (decision === 'request_changes' && !comment.trim()) {
      setFieldError('Expliquez les modifications demandées : le commentaire est obligatoire pour renvoyer la révision.');
      field.current?.focus();
      return;
    }
    setFieldError(null);
    setAsking(decision);
  };

  const decide = async (decision: Decision) => {
    setBusy(true);
    try {
      if (decision === 'approve') await ed.approve(revision.id, comment);
      else await ed.requestChanges(revision.id, comment);
      setFlash(
        decision === 'approve'
          ? `Révision approuvée : « ${revision.title} ». Elle attend maintenant sa publication.`
          : `Modifications demandées pour « ${revision.title} ». L’auteur verra votre commentaire.`,
      );
      navigate(paths.reviewQueue, { force: true });
    } catch (e) {
      setAsking(null);
      setError(describeEditorialError(e));
      if (isApiError(e, 'comment_required') || isApiError(e, 'invalid_comment')) field.current?.focus();
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="status-card decision" aria-labelledby="decision-heading">
      <h2 id="decision-heading">Votre décision</h2>
      <NoteField
        id="review-comment"
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
          Approuver…
        </button>
        <button type="button" onClick={() => ask('request_changes')} disabled={busy}>
          Demander des modifications…
        </button>
      </div>
      {error && (
        <div className="banner error" role="alert">
          <p>{error}</p>
          <p>
            <Link to={paths.reviewQueue}>Revenir à la file de relecture</Link>
          </p>
        </div>
      )}
      {asking && (
        <Dialog
          title={asking === 'approve' ? `Approuver la révision ${revision.number} ?` : `Demander des modifications ?`}
          onCancel={() => setAsking(null)}
          cancelLabel="Revenir à la relecture"
          actions={[
            {
              label: asking === 'approve' ? 'Approuver' : 'Envoyer la demande',
              kind: 'primary',
              disabled: busy,
              onClick: () => void decide(asking),
            },
          ]}
        >
          <p>
            {asking === 'approve'
              ? 'La révision sera proposée aux éditeurs pour publication. La décision est définitive : elle ne pourra plus être relue.'
              : 'La révision sera renvoyée à son auteur avec votre commentaire ; il rédigera une version corrigée. La décision est définitive.'}
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
