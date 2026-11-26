// Reviewer space: the queue of lessons and courses in review in the
// reviewer's subjects (GET /api/review/queue and ?type=course), filtered by
// content type, subject and decision, a page at a time, and the review of
// one lesson revision (courses: CourseReviewPages.tsx), read
// through the canonical preview, with approve (optional comment) and
// request changes (required comment), each confirmed first. The server
// decides who may review what (assignment per subject, no self-review);
// this page only explains its answers.
import { useId, useMemo, useRef, useState } from 'react';
import { isApiError, type Revision, type Subject } from '../api/client';
import type { ReviewQueueItem } from '../api/editorial';
import { courseReviewApi, type CourseReviewItem } from '../api/courseReview';
import { describeBlocked, filterQueue, mergeQueue, paginate, submissionAge, type QueueDecision, type QueueType } from '../courses/workflow';
import { describeEditorialError } from '../api/editorialMessages';
import { setFlash, takeFlash, useStudio } from '../context';
import { paths } from '../editorialRoutes';
import { LANGUAGE_LABELS, formatDateTime } from '../labels';
import { navigate } from '../router';
import { Dialog } from '../ui/Dialog';
import { Link } from '../ui/Link';
import { CatalogueName, catalogueLabel, entryLabel, type CatalogueLabel } from '../catalogue';
import { t, tn } from '../i18n';
import { CanonicalPreview, ErrorPage, LoadingPage, NoteField, Quote, SubjectName, dirOf, subjectLabel, useEditorial, useLoad, useSubjects } from './common';

/** Queue items grouped by subject, in catalogue order (display order, then code); subjects missing from the catalogue last, by ID. */
/** Queue page size: large, as the queues are bounded to 200 each by the server. */
export const QUEUE_PAGE_SIZE = 50;

export function groupBySubject<T extends { subjectId: number }>(items: T[], subjects: Map<number, Subject>): Array<{ subjectId: number; items: T[] }> {
  const groups = new Map<number, T[]>();
  for (const item of items) groups.set(item.subjectId, [...(groups.get(item.subjectId) ?? []), item]);
  const rank = (id: number) => {
    const s = subjects.get(id);
    return s ? [0, s.sortOrder, s.code] as const : [1, 0, ''] as const;
  };
  return [...groups.entries()]
    .sort(([a], [b]) => {
      const [ka, oa, ca] = rank(a);
      const [kb, ob, cb] = rank(b);
      return ka - kb || oa - ob || (ca < cb ? -1 : ca > cb ? 1 : 0) || a - b;
    })
    .map(([subjectId, list]) => ({ subjectId, items: list }));
}

export function ReviewQueuePage() {
  const ed = useEditorial();
  const { api } = useStudio();
  const cr = useMemo(() => courseReviewApi(api), [api]);
  const subjects = useSubjects();
  const [queue] = useLoad(async () => {
    const [lessons, courses] = await Promise.all([ed.reviewQueue(), cr.reviewQueue()]);
    return mergeQueue(lessons, courses);
  }, [ed, cr]);
  const [flash] = useState(takeFlash);
  const [type, setType] = useState<QueueType>('all');
  const [subjectId, setSubjectId] = useState<number | null>(null);
  const [decision, setDecision] = useState<QueueDecision>('all');
  const [page, setPage] = useState(0);
  const ids = { type: useId(), subject: useId(), decision: useId(), results: useId() };
  const listTop = useRef<HTMLHeadingElement>(null);

  const all = queue.state === 'ready' ? queue.data : [];
  const filtered = filterQueue(all, { type, subjectId, decision });
  const shown = paginate(filtered, page, QUEUE_PAGE_SIZE);
  const subjectIds = [...new Set(all.map((e) => e.subjectId))];
  const goTo = (p: number) => {
    setPage(p);
    requestAnimationFrame(() => listTop.current?.focus());
  };

  return (
    <main className="page" id="main">
      <h1 tabIndex={-1}>{t('shell.space.review')}</h1>
      <p className="hint">{t('review.queue.hint')}</p>
      {flash && (
        <p className="banner success" role="status">
          {flash}
        </p>
      )}
      {queue.state === 'loading' && <p role="status">{t('review.queue.loading')}</p>}
      {queue.state === 'error' && (
        <p className="banner error" role="alert">
          {queue.message}
        </p>
      )}
      {queue.state === 'ready' && all.length === 0 && <p>{t('review.queue.empty')}</p>}
      {queue.state === 'ready' && all.length > 0 && (
        <>
          <form className="queue-filters" onSubmit={(e) => e.preventDefault()} aria-label={t('review.queue.filters')}>
            <div className="fields">
              <label htmlFor={ids.type}>{t('review.queue.type')}</label>
              <select id={ids.type} value={type} onChange={(e) => { setType(e.target.value as QueueType); setPage(0); }}>
                <option value="all">{t('review.queue.type.all')}</option>
                <option value="lesson">{t('review.lessons')}</option>
                <option value="course">{t('review.courses')}</option>
              </select>
            </div>
            <div className="fields">
              <label htmlFor={ids.subject}>{t('review.fact.subject')}</label>
              <select id={ids.subject} value={subjectId ?? ''} onChange={(e) => { setSubjectId(e.target.value ? Number(e.target.value) : null); setPage(0); }}>
                <option value="">{t('review.queue.allSubjects')}</option>
                {subjectIds.map((id) => (
                  <option key={id} value={id}>
                    {subjectLabel(subjects, id)}
                  </option>
                ))}
              </select>
            </div>
            <div className="fields">
              <label htmlFor={ids.decision}>{t('review.queue.decision')}</label>
              <select id={ids.decision} value={decision} onChange={(e) => { setDecision(e.target.value as QueueDecision); setPage(0); }}>
                <option value="all">{t('review.queue.decision.all')}</option>
                <option value="mine">{t('review.queue.decision.mine')}</option>
                <option value="other">{t('review.queue.decision.other')}</option>
                <option value="blocked">{t('review.queue.decision.blocked')}</option>
              </select>
            </div>
          </form>
          <p id={ids.results} role="status">
            {filtered.length === 0
              ? t('review.queue.noMatch')
              : shown.pages > 1
                ? tn('review.queue.resultsPaged', filtered.length, { page: shown.page + 1, pages: shown.pages })
                : tn('review.queue.results', filtered.length)}
          </p>
          <h2 className="visually-hidden" tabIndex={-1} ref={listTop}>
            {t('review.queue.resultsHeading')}
          </h2>
          {groupBySubject(shown.items, subjects).map((group) => (
            <section key={group.subjectId} aria-labelledby={`subject-${group.subjectId}`}>
              <h2 id={`subject-${group.subjectId}`}>
                <SubjectName subjects={subjects} id={group.subjectId} /> ({group.items.length})
              </h2>
              <ul className="lesson-list" aria-label={t('review.queue.subjectList', { subject: subjectLabel(subjects, group.subjectId) })}>
                {group.items.map((entry) => (entry.type === 'lesson' ? <QueueCard key={entry.id} item={entry.item} /> : <CourseQueueCard key={entry.id} item={entry.item} />))}
              </ul>
            </section>
          ))}
          {shown.pages > 1 && (
            <nav className="pager" aria-label={t('review.queue.pages')}>
              <button type="button" onClick={() => goTo(shown.page - 1)} disabled={shown.page === 0}>
                {t('review.queue.previous')}
              </button>
              <span>{t('review.queue.page', { page: shown.page + 1, pages: shown.pages })}</span>
              <button type="button" onClick={() => goTo(shown.page + 1)} disabled={shown.page >= shown.pages - 1}>
                {t('review.queue.next')}
              </button>
            </nav>
          )}
        </>
      )}
    </main>
  );
}

function QueueCard({ item }: { item: ReviewQueueItem }) {
  return (
    <li className="lesson-card">
      <h3 lang={item.language} dir={dirOf(item.language)}>
        <Link to={paths.reviewRevision(item.revisionId)}>{item.title || t('review.revisionNo', { id: item.revisionId })}</Link>
      </h3>
      <p className="lesson-meta">
        <span className="state kind-label">{t('review.kind.lesson')}</span>
        <span className="state state-review">{t('shell.status.in_review')}</span>
      </p>
      <p className="lesson-context">
        {t('review.queue.lessonContext', {
          number: item.number,
          language: LANGUAGE_LABELS[item.language],
          author: item.authorName,
          date: formatDateTime(item.submittedAt),
          age: submissionAge(item.submittedAt),
        })}
      </p>
    </li>
  );
}

function CourseQueueCard({ item }: { item: CourseReviewItem }) {
  const blocked = item.blocked ? describeBlocked(item.blocked) : null;
  return (
    <li className="lesson-card">
      <h3 lang={item.language} dir={dirOf(item.language)}>
        <Link to={paths.reviewCourse(item.revisionId)}>{item.title || t('review.courseNo', { id: item.courseId })}</Link>
      </h3>
      <p className="lesson-meta">
        <span className="state kind-label">{item.kind === 'amendment' ? t('review.kind.amendment') : t('review.kind.course')}</span>
        <span className="state state-review">{t('shell.status.in_review')}</span>
        {blocked ? (
          <span className="state state-changes">{t('review.queue.blocked')}</span>
        ) : item.eligible ? (
          <span className="state state-approved">{t('review.queue.canDecide')}</span>
        ) : (
          <span className="state">{t('review.queue.decision.other')}</span>
        )}
      </p>
      <p className="lesson-context">
        {t('review.queue.courseContext', {
          number: item.number,
          language: LANGUAGE_LABELS[item.language],
          author: item.authorName,
          date: formatDateTime(item.submittedAt),
          age: submissionAge(item.submittedAt),
        })}
      </p>
      {blocked && (
        <div className="lesson-context">
          <p>{blocked.summary}</p>
          {blocked.details.length > 0 && (
            <ul>
              {blocked.details.map((d) => (
                <li key={d}>{d}</li>
              ))}
            </ul>
          )}
        </div>
      )}
    </li>
  );
}

interface ReviewData {
  revision: Revision;
  authorName: string | null;
  subject: CatalogueLabel | null;
  chapter: CatalogueLabel | null;
}

type Decision = 'approve' | 'request_changes';

export function ReviewRevisionPage({ revisionId }: { revisionId: number }) {
  const { api, session } = useStudio();
  const ed = useEditorial();
  const [loaded] = useLoad<ReviewData>(async () => {
    const revision = await ed.reviewRevision(revisionId);
    // Context only: a failure here does not block the review.
    const [queue, subjects] = await Promise.all([ed.reviewQueue(revision.subjectId).catch(() => []), api.subjects().catch(() => [])]);
    // Names in the revision's language, else the other, else the code.
    const entry = subjects.find((s) => s.id === revision.subjectId);
    const subject = entry ? entryLabel(entry, revision.language) : null;
    let chapter: CatalogueLabel | null = null;
    if (entry && revision.chapterId !== null) {
      const c = (await api.chapters(entry.code).catch(() => null))?.chapters.find((ch) => ch.id === revision.chapterId);
      chapter = c ? catalogueLabel(c.code, c.nameFr, c.nameAr, revision.language) : null;
    }
    return { revision, authorName: queue.find((q) => q.revisionId === revisionId)?.authorName ?? null, subject, chapter };
  }, [ed, api, revisionId]);

  if (loaded.state === 'loading') return <LoadingPage label={t('review.loadingRevision')} />;
  if (loaded.state === 'error')
    return (
      <ErrorPage
        title={t('review.revisionUnavailable')}
        message={
          loaded.notFound
            ? t('review.revision.notFound')
            : loaded.message
        }
        back={{ to: paths.reviewQueue, label: t('review.backToQueue') }}
      />
    );
  const { revision, authorName, subject, chapter } = loaded.data;
  const own = revision.authorId === session.user.id;
  const dir = dirOf(revision.language);
  return (
    <main className="page" id="main">
      <p className="breadcrumb">
        <Link to={paths.reviewQueue}>{t('shell.space.review')}</Link>
      </p>
      <h1 tabIndex={-1} lang={revision.language} dir={dir}>
        {revision.title}
      </h1>
      <p className="lesson-meta">
        <span className="state state-review">{t('shell.status.in_review')}</span>
      </p>
      <dl className="facts">
        <div>
          <dt>{t('review.fact.author')}</dt>
          <dd>{own ? t('review.you') : (authorName ?? t('review.accountNo', { id: revision.authorId }))}</dd>
        </div>
        <div>
          <dt>{t('review.fact.subject')}</dt>
          <dd>
            {subject ? <CatalogueName label={subject} /> : t('shell.subjectNumber', { id: revision.subjectId })}
            {revision.chapterId !== null && (
              <>
                {' · '}
                {chapter ? <CatalogueName label={chapter} /> : t('review.chapterNo', { id: revision.chapterId })}
              </>
            )}
          </dd>
        </div>
        <div>
          <dt>{t('review.fact.language')}</dt>
          <dd>{LANGUAGE_LABELS[revision.language]}</dd>
        </div>
        <div>
          <dt>{t('review.fact.revision')}</dt>
          <dd>
            {revision.submittedAt
              ? t('review.numberSubmitted', { number: revision.number, date: formatDateTime(revision.submittedAt) })
              : t('review.numberOnly', { number: revision.number })}
          </dd>
        </div>
        {revision.prerequisites && (
          <div>
            <dt>{t('review.fact.prerequisites')}</dt>
            <dd lang={revision.language} dir={dir} className="pre-line">
              {revision.prerequisites}
            </dd>
          </div>
        )}
      </dl>
      {own ? (
        <section className="status-card" aria-labelledby="own-heading">
          <h2 id="own-heading">{t('review.revision.ownHeading')}</h2>
          <p>{t('review.revision.ownText')}</p>
        </section>
      ) : (
        <DecisionForm revision={revision} />
      )}
      {revision.mediaIds.length > 0 && (
        <section className="panel" aria-labelledby="media-heading">
          <h2 id="media-heading">{t('review.media.heading', { count: revision.mediaIds.length })}</h2>
          <ul className="media-list" aria-label={t('review.media.list')}>
            {revision.mediaIds.map((id) => (
              <li key={id}>
                <img src={`/api/review/media/${id}`} alt={t('review.media.alt', { id })} />
                <span>{t('review.media.caption', { id })}</span>
              </li>
            ))}
          </ul>
        </section>
      )}
      <section className="panel" aria-labelledby="preview-heading">
        <h2 id="preview-heading">{t('review.previewHeading')}</h2>
        <CanonicalPreview content={revision.content} testId="review-preview" mediaRoute="review" />
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
      setFieldError(t('review.decision.commentRequired'));
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
          ? t('review.decision.approved', { title: revision.title })
          : t('review.decision.changesRequested', { title: revision.title }),
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
      <h2 id="decision-heading">{t('review.decision.heading')}</h2>
      <NoteField
        id="review-comment"
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
          {t('review.decision.approveButton')}
        </button>
        <button type="button" onClick={() => ask('request_changes')} disabled={busy}>
          {t('review.decision.requestButton')}
        </button>
      </div>
      {error && (
        <div className="banner error" role="alert">
          <p>{error}</p>
          <p>
            <Link to={paths.reviewQueue}>{t('review.backToQueue')}</Link>
          </p>
        </div>
      )}
      {asking && (
        <Dialog
          title={asking === 'approve' ? t('review.decision.approveTitle', { number: revision.number }) : t('review.decision.requestTitle')}
          onCancel={() => setAsking(null)}
          cancelLabel={t('review.backToReview')}
          actions={[
            {
              label: asking === 'approve' ? t('review.decision.approve') : t('review.decision.send'),
              kind: 'primary',
              disabled: busy,
              onClick: () => void decide(asking),
            },
          ]}
        >
          <p>
            {asking === 'approve' ? t('review.decision.approveText') : t('review.decision.requestText')}
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
