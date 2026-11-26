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
import { CoursePublishQueue } from './CourseReviewPages';
import { t, tn } from '../i18n';

export const STALE_BASE_EXPLANATION = t('review.publish.staleBase');

export function PublishQueuePage() {
  const ed = useEditorial();
  const [queue] = useLoad(() => ed.publishQueue(), [ed]);
  return (
    <main className="page" id="main">
      <h1 tabIndex={-1}>{t('shell.space.publish')}</h1>
      <p className="hint">{t('review.publish.hint')}</p>
      <h2>{t('review.lessons')}</h2>
      {queue.state === 'loading' && <p role="status">{t('review.publish.loading')}</p>}
      {queue.state === 'error' && (
        <p className="banner error" role="alert">
          {queue.message}
        </p>
      )}
      {queue.state === 'ready' && queue.data.length === 0 && <p>{t('review.publish.empty')}</p>}
      {queue.state === 'ready' && queue.data.length > 0 && (
        <ul className="lesson-list" aria-label={t('review.publish.list')}>
          {queue.data.map((item) => (
            <li key={item.revisionId} className="lesson-card">
              <h3 lang={item.language} dir={dirOf(item.language)}>
                <Link to={paths.publishRevision(item.revisionId)}>{item.title}</Link>
              </h3>
              <p className="lesson-meta">
                <span className="state state-approved">{t('shell.status.approved')}</span>
                {item.staleBase && <span className="state state-changes">{t('review.publish.staleBadge')}</span>}
                <span className="state">{item.publishedRevisionId === null ? t('review.publish.first') : t('review.replacesPublished')}</span>
              </p>
              <p className="lesson-context">
                {t('review.publish.context', {
                  subject: item.subjectCode,
                  language: LANGUAGE_LABELS[item.language],
                  number: item.number,
                  author: item.authorName,
                  approver: item.approverName,
                  date: formatDateTime(item.approvedAt),
                })}
              </p>
              <p className="lesson-context">
                <Link to={paths.publication(item.lessonPublicId)}>{t('review.lessonPublicationHistory')}</Link>
              </p>
            </li>
          ))}
        </ul>
      )}
      <CoursePublishQueue />
    </main>
  );
}

export function PublishRevisionPage({ revisionId }: { revisionId: number }) {
  const ed = useEditorial();
  // The revision and its queue facts come together, whatever the queue's bound.
  const [loaded] = useLoad<{ revision: Revision; item: PublishQueueItem }>(() => ed.publishRevision(revisionId), [ed, revisionId]);
  const [asking, setAsking] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [published, setPublished] = useState<Publication | null>(null);

  if (loaded.state === 'loading') return <LoadingPage label={t('review.loadingRevision')} />;
  if (loaded.state === 'error')
    return (
      <ErrorPage
        title={t('review.revisionUnavailable')}
        message={loaded.notFound ? t('review.publish.notFound') : loaded.message}
        back={{ to: paths.publishQueue, label: t('review.backToPublishQueue') }}
      />
    );
  const { revision, item } = loaded.data;
  const stale = item.staleBase;
  const firstPublication = item.publishedRevisionId === null;

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
        <Link to={paths.publishQueue}>{t('shell.space.publish')}</Link>
      </p>
      <h1 tabIndex={-1} lang={revision.language} dir={dirOf(revision.language)}>
        {revision.title}
      </h1>
      <p className="lesson-meta">
        <span className={`state ${published ? 'state-published' : 'state-approved'}`}>{published ? t('shell.state.published') : t('shell.state.awaitingPublication')}</span>
      </p>
      {(
        <dl className="facts">
          <div>
            <dt>{t('review.fact.subject')}</dt>
            <dd>{item.subjectCode}</dd>
          </div>
          <div>
            <dt>{t('review.fact.language')}</dt>
            <dd>{LANGUAGE_LABELS[item.language]}</dd>
          </div>
          <div>
            <dt>{t('review.fact.author')}</dt>
            <dd>{item.authorName}</dd>
          </div>
          <div>
            <dt>{t('review.publish.approvedBy')}</dt>
            <dd>
              {t('review.fact.byOn', { name: item.approverName, date: formatDateTime(item.approvedAt) })}
            </dd>
          </div>
          <div>
            <dt>{t('review.fact.effect')}</dt>
            <dd>{firstPublication ? t('review.publish.firstEffect') : t('review.publish.replaceEffect')}</dd>
          </div>
        </dl>
      )}
      {revision.review?.comment && (
        <section className="feedback" aria-label={t('review.reviewerComment')}>
          <p>{t('review.reviewerCommentLabel')}</p>
          <blockquote dir="auto">{revision.review.comment}</blockquote>
        </section>
      )}
      {published ? (
        <section className="status-card" aria-labelledby="published-heading" role="status">
          <h2 id="published-heading">{t('review.publish.done')}</h2>
          <p>{t('review.publish.doneText', { date: formatDateTime(published.publishedAt) })}</p>
          <ul className="link-list">
            <li>
              <a href={paths.readerLesson(item.lessonPublicId)}>{t('review.publish.viewLesson')}</a>
            </li>
            <li>
              <Link to={paths.publication(item.lessonPublicId)}>{t('review.publicationHistory')}</Link>
            </li>
            <li>
              <Link to={paths.publishQueue}>{t('review.backToPublishQueue')}</Link>
            </li>
          </ul>
        </section>
      ) : stale ? (
        <section className="status-card warning" aria-labelledby="stale-heading">
          <h2 id="stale-heading">{t('review.publishImpossible')}</h2>
          <p>{STALE_BASE_EXPLANATION}</p>
        </section>
      ) : (
        <section className="status-card" aria-labelledby="publish-heading">
          <h2 id="publish-heading">{t('review.publish')}</h2>
          <p>{t('review.publish.text')}</p>
          <button type="button" className="primary" onClick={() => setAsking(true)} disabled={busy}>
            {t('review.publish.button')}
          </button>
        </section>
      )}
      {error && (
        <p className="banner error" role="alert">
          {error}
        </p>
      )}
      {revision.mediaIds.length > 0 && (
        <p className="hint">{tn('review.publish.media', revision.mediaIds.length)}</p>
      )}
      <section className="panel" aria-labelledby="preview-heading">
        <h2 id="preview-heading">{t('review.previewHeading')}</h2>
        <CanonicalPreview content={revision.content} testId="publish-preview" mediaRoute="publish" />
      </section>
      {asking && (
        <Dialog
          title={t('review.publish.dialogTitle', { title: revision.title })}
          onCancel={() => setAsking(false)}
          cancelLabel={t('review.dontPublish')}
          actions={[{ label: t('review.publish'), kind: 'primary', disabled: busy, onClick: () => void publish() }]}
        >
          <p>
            {firstPublication ? t('review.publish.firstText') : t('review.publish.replaceText')}
          </p>
        </Dialog>
      )}
    </main>
  );
}
