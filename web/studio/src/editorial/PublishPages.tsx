// Publisher space: approved revisions awaiting publication
// (GET /api/publish/queue) and one revision read through the canonical
// preview, published after confirmation (POST /api/revisions/{id}/publish).
// A revision whose base is no longer the published revision is flagged
// before the publisher tries; the server refuses it (stale_base) anyway.
import { useState } from 'react';
import type { Revision } from '../api/client';
import type { Publication, PublishQueueItem } from '../api/editorial';
import { describeEditorialError } from '../api/editorialMessages';
import { paths } from '../editorialRoutes';
import { LANGUAGE_LABELS, formatDateTime } from '../labels';
import { Dialog } from '../ui/Dialog';
import { Link } from '../ui/Link';
import { CanonicalPreview, ErrorPage, LoadingPage, dirOf, useEditorial, useLoad } from './common';

export const STALE_BASE_EXPLANATION =
  'La version publiée de la leçon a changé depuis la rédaction de cette révision (dépublication ou restauration) : le serveur refusera de la publier. Une révision réconciliée doit être rédigée et relue ; un administrateur peut retirer celle-ci pour débloquer la leçon.';

export function PublishQueuePage() {
  const ed = useEditorial();
  const [queue] = useLoad(() => ed.publishQueue(), [ed]);
  return (
    <main className="page" id="main">
      <h1 tabIndex={-1}>Publication</h1>
      <p className="hint">Révisions approuvées par un relecteur et jamais publiées, les plus anciennes d’abord. La publication remplace la version que lisent les élèves.</p>
      {queue.state === 'loading' && <p role="status">Chargement des révisions à publier…</p>}
      {queue.state === 'error' && (
        <p className="banner error" role="alert">
          {queue.message}
        </p>
      )}
      {queue.state === 'ready' && queue.data.length === 0 && <p>Aucune révision approuvée n’attend de publication.</p>}
      {queue.state === 'ready' && queue.data.length > 0 && (
        <ul className="lesson-list" aria-label="Révisions approuvées à publier">
          {queue.data.map((item) => (
            <li key={item.revisionId} className="lesson-card">
              <h2 lang={item.language} dir={dirOf(item.language)}>
                <Link to={paths.publishRevision(item.revisionId)}>{item.title}</Link>
              </h2>
              <p className="lesson-meta">
                <span className="state state-approved">Approuvée</span>
                {item.staleBase && <span className="state state-changes">Base périmée : publication impossible</span>}
                <span className="state">{item.publishedRevisionId === null ? 'Première publication' : 'Remplacera la version publiée'}</span>
              </p>
              <p className="lesson-context">
                {item.subjectCode} · {LANGUAGE_LABELS[item.language]} · révision {item.number} · par {item.authorName} · approuvée par {item.approverName} le {formatDateTime(item.approvedAt)}
              </p>
              <p className="lesson-context">
                <Link to={paths.publication(item.lessonPublicId)}>Historique de publication de la leçon</Link>
              </p>
            </li>
          ))}
        </ul>
      )}
    </main>
  );
}

export function PublishRevisionPage({ revisionId }: { revisionId: number }) {
  const ed = useEditorial();
  const [loaded] = useLoad<{ revision: Revision; item: PublishQueueItem | null }>(async () => {
    const [revision, queue] = await Promise.all([ed.publishRevision(revisionId), ed.publishQueue()]);
    return { revision, item: queue.find((q) => q.revisionId === revisionId) ?? null };
  }, [ed, revisionId]);
  const [asking, setAsking] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [published, setPublished] = useState<Publication | null>(null);

  if (loaded.state === 'loading') return <LoadingPage label="Chargement de la révision…" />;
  if (loaded.state === 'error')
    return (
      <ErrorPage
        title="Révision indisponible"
        message={loaded.notFound ? 'Cette révision n’attend pas de publication : elle n’existe pas, n’est pas approuvée, a déjà été publiée ou a été retirée.' : loaded.message}
        back={{ to: paths.publishQueue, label: 'Revenir aux révisions à publier' }}
      />
    );
  const { revision, item } = loaded.data;
  const stale = item?.staleBase ?? false;

  const publish = async () => {
    setBusy(true);
    try {
      setPublished(await ed.publish(revision.id));
      setAsking(false);
      setError(null);
    } catch (e) {
      setAsking(false);
      setError(describeEditorialError(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <main className="page" id="main">
      <p className="breadcrumb">
        <Link to={paths.publishQueue}>Publication</Link>
      </p>
      <h1 tabIndex={-1} lang={revision.language} dir={dirOf(revision.language)}>
        {revision.title}
      </h1>
      <p className="lesson-meta">
        <span className={`state ${published ? 'state-published' : 'state-approved'}`}>{published ? 'Publiée' : 'Approuvée, en attente de publication'}</span>
      </p>
      {item && (
        <dl className="facts">
          <div>
            <dt>Matière</dt>
            <dd>{item.subjectCode}</dd>
          </div>
          <div>
            <dt>Langue</dt>
            <dd>{LANGUAGE_LABELS[item.language]}</dd>
          </div>
          <div>
            <dt>Auteur</dt>
            <dd>{item.authorName}</dd>
          </div>
          <div>
            <dt>Approuvée par</dt>
            <dd>
              {item.approverName}, le {formatDateTime(item.approvedAt)}
            </dd>
          </div>
          <div>
            <dt>Effet</dt>
            <dd>{item.publishedRevisionId === null ? 'Première publication de la leçon.' : 'Remplacera la version publiée actuelle.'}</dd>
          </div>
        </dl>
      )}
      {revision.review?.comment && (
        <section className="feedback" aria-label="Commentaire du relecteur">
          <p>Commentaire du relecteur :</p>
          <blockquote dir="auto">{revision.review.comment}</blockquote>
        </section>
      )}
      {published ? (
        <section className="status-card" aria-labelledby="published-heading" role="status">
          <h2 id="published-heading">Révision publiée</h2>
          <p>Les élèves lisent maintenant cette version (publiée le {formatDateTime(published.publishedAt)}).</p>
          <ul className="link-list">
            {item && (
              <li>
                <a href={paths.readerLesson(item.lessonPublicId)}>Voir la leçon publiée</a>
              </li>
            )}
            {item && (
              <li>
                <Link to={paths.publication(item.lessonPublicId)}>Historique de publication</Link>
              </li>
            )}
            <li>
              <Link to={paths.publishQueue}>Revenir aux révisions à publier</Link>
            </li>
          </ul>
        </section>
      ) : stale ? (
        <section className="status-card warning" aria-labelledby="stale-heading">
          <h2 id="stale-heading">Publication impossible</h2>
          <p>{STALE_BASE_EXPLANATION}</p>
        </section>
      ) : (
        <section className="status-card" aria-labelledby="publish-heading">
          <h2 id="publish-heading">Publier</h2>
          <p>Relisez la leçon ci-dessous. La publication est immédiate pour les élèves ; une version antérieure pourra être restaurée en cas de problème.</p>
          <button type="button" className="primary" onClick={() => setAsking(true)} disabled={busy}>
            Publier cette révision…
          </button>
        </section>
      )}
      {error && (
        <p className="banner error" role="alert">
          {error}
        </p>
      )}
      {revision.mediaIds.length > 0 && (
        <p className="hint">
          {revision.mediaIds.length === 1 ? '1 image jointe' : `${revision.mediaIds.length} images jointes`} (relue par le relecteur ; elle n’apparaît pas dans la leçon publiée tant que le format de leçon ne
          permet pas de la placer dans le texte).
        </p>
      )}
      <section className="panel" aria-labelledby="preview-heading">
        <h2 id="preview-heading">Leçon telle que l’élève la lira</h2>
        <CanonicalPreview content={revision.content} testId="publish-preview" />
      </section>
      {asking && (
        <Dialog
          title={`Publier « ${revision.title} » ?`}
          onCancel={() => setAsking(false)}
          cancelLabel="Ne pas publier"
          actions={[{ label: 'Publier', kind: 'primary', disabled: busy, onClick: () => void publish() }]}
        >
          <p>
            {item?.publishedRevisionId === null
              ? 'C’est la première publication de cette leçon : les élèves invités pourront la lire immédiatement.'
              : 'Cette révision remplacera immédiatement la version que lisent les élèves.'}
          </p>
        </Dialog>
      )}
    </main>
  );
}
