// Curriculum administration (LMS-B02 API, LMS-B10 pages): levels and their
// sections, the editions of a section, and an edition's subject assignments
// with ordered official chapters, making it current and retiring it. Every
// write names the version this page loaded (expectedVersion); if anyone
// changed the entry since, the server refuses with version_conflict and
// nothing is written. Retirement is never undone.
import { useId, useState, type FormEvent } from 'react';
import {
  EDITION_STATE_LABELS,
  currentEditionId,
  describeAdminError,
  move,
  type AdminSubject,
  type Assignment,
  type EntryFields,
  type Level,
  type Section,
} from '../api/admin';
import { CatalogueName, entryLabel } from '../catalogue';
import { paths } from '../editorialRoutes';
import { locale, t } from '../i18n';
import { formatDateTime } from '../labels';
import { navigate } from '../router';
import { Dialog } from '../ui/Dialog';
import { Link } from '../ui/Link';
import { AdminPage, LoadFailed, OutcomeBanner, failure, useAdmin, type Outcome } from './adminCommon';
import { useLoad } from './common';

/** Validates an entry form; returns the fields to send or an error in the interface language. */
export function entryFields(nameFr: string, nameAr: string, order: string): EntryFields | { error: string } {
  const fr = nameFr.trim();
  const ar = nameAr.trim();
  if (!fr || !ar) return { error: t('admin.curriculum.namesRequired') };
  const n = Number(order);
  if (order.trim() === '' || !Number.isInteger(n) || n < 0 || n > 9999) return { error: t('admin.orderInvalid') };
  return { nameFr: fr, nameAr: ar, displayOrder: n };
}

const CODE_HINT = t('admin.curriculum.codeHint');

/** A French and an Arabic name: the interface language's first, the other isolated with its own language. */
function BothNames({ fr, ar }: { fr: string; ar: string }) {
  return locale() === 'ar' ? (
    <>
      {ar}{' '}
      <bdi lang="fr" dir="ltr">
        {fr}
      </bdi>
    </>
  ) : (
    <>
      {fr}{' '}
      <bdi lang="ar" dir="rtl">
        {ar}
      </bdi>
    </>
  );
}

/** The name in the interface language (both are required). */
const uiName = (fr: string, ar: string) => (locale() === 'ar' ? ar : fr);

/** The other language's name, with its lang and dir. */
function OtherName({ fr, ar }: { fr: string; ar: string }) {
  return locale() === 'ar' ? (
    <bdi lang="fr" dir="ltr">
      {fr}
    </bdi>
  ) : (
    <bdi lang="ar" dir="rtl">
      {ar}
    </bdi>
  );
}

/** Create (with a code) or edit (names and order) a level or section. */
function EntryForm({
  label,
  submitLabel,
  withCode,
  initial,
  onSubmit,
}: {
  label: string;
  submitLabel: string;
  withCode: boolean;
  initial?: { nameFr: string; nameAr: string; displayOrder: number };
  onSubmit: (code: string, fields: EntryFields) => Promise<void>;
}) {
  const id = useId();
  const [code, setCode] = useState('');
  const [fr, setFr] = useState(initial?.nameFr ?? '');
  const [ar, setAr] = useState(initial?.nameAr ?? '');
  const [order, setOrder] = useState(String(initial?.displayOrder ?? 0));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const f = entryFields(fr, ar, order);
    if ('error' in f) return setError(f.error);
    if (withCode && !/^[a-z0-9][a-z0-9-]{0,62}$/.test(code.trim())) return setError(CODE_HINT);
    setBusy(true);
    setError(null);
    try {
      await onSubmit(code.trim(), f);
      if (withCode) {
        setCode('');
        setFr('');
        setAr('');
        setOrder('0');
      }
    } catch (err) {
      setError(describeAdminError(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <form className="entry-form" onSubmit={submit} noValidate aria-label={label}>
      {withCode && (
        <div className="fields">
          <label htmlFor={`${id}-code`}>{t('admin.code')}</label>
          <p className="hint" id={`${id}-code-hint`}>
            {CODE_HINT}
          </p>
          <input id={`${id}-code`} dir="ltr" autoComplete="off" spellCheck={false} value={code} aria-describedby={`${id}-code-hint`} onChange={(e) => setCode(e.target.value)} />
        </div>
      )}
      <div className="fields">
        <label htmlFor={`${id}-fr`}>{t('admin.nameFr')}</label>
        <input id={`${id}-fr`} lang="fr" dir="ltr" value={fr} maxLength={200} onChange={(e) => setFr(e.target.value)} />
      </div>
      <div className="fields">
        <label htmlFor={`${id}-ar`}>{t('admin.nameAr')}</label>
        <input id={`${id}-ar`} lang="ar" dir="rtl" value={ar} maxLength={200} onChange={(e) => setAr(e.target.value)} />
      </div>
      <div className="fields">
        <label htmlFor={`${id}-order`}>{t('admin.displayOrder')}</label>
        <input id={`${id}-order`} type="number" min={0} max={9999} inputMode="numeric" value={order} onChange={(e) => setOrder(e.target.value)} />
      </div>
      {error && (
        <p className="banner error" role="alert">
          {error}
        </p>
      )}
      <button type="submit" className={withCode ? 'primary' : undefined} disabled={busy}>
        {submitLabel}
      </button>
    </form>
  );
}

type Retire = { kind: 'level'; entry: Level } | { kind: 'section'; entry: Section };

export function CurriculumPage() {
  const admin = useAdmin();
  const [loaded, reload] = useLoad(() => admin.levels(), [admin]);
  const [outcome, setOutcome] = useState<Outcome>(null);
  const [retire, setRetire] = useState<Retire | null>(null);
  const [busy, setBusy] = useState(false);

  const act = async (fn: () => Promise<unknown>, done: string) => {
    setOutcome(null);
    await fn();
    setOutcome({ kind: 'success', message: done });
    await reload();
  };
  const confirmRetire = async () => {
    if (!retire) return;
    setBusy(true);
    try {
      if (retire.kind === 'level') await act(() => admin.retireLevel(retire.entry.id, retire.entry.version), t('admin.curriculum.levelRetired', { code: retire.entry.code }));
      else await act(() => admin.retireSection(retire.entry.id, retire.entry.version), t('admin.curriculum.sectionRetired', { code: retire.entry.code }));
    } catch (e) {
      setOutcome(failure(e));
    } finally {
      setBusy(false);
      setRetire(null);
    }
  };

  return (
    <AdminPage
      title={t('admin.nav.curriculum')}
      intro={<p className="hint">{t('admin.curriculum.intro')}</p>}
    >
      <OutcomeBanner outcome={outcome} onReload={() => void reload()} />
      <details className="tools">
        <summary>{t('admin.curriculum.newLevel')}</summary>
        <EntryForm
          label={t('admin.curriculum.newLevel')}
          submitLabel={t('admin.curriculum.createLevel')}
          withCode
          onSubmit={(code, f) => act(() => admin.createLevel(code, f), t('admin.curriculum.levelCreated', { code }))}
        />
      </details>
      {loaded.state === 'loading' && <p role="status">{t('admin.curriculum.loading')}</p>}
      {loaded.state === 'error' && <LoadFailed message={loaded.message} onRetry={() => void reload()} />}
      {loaded.state === 'ready' && loaded.data.length === 0 && <p>{t('admin.curriculum.empty')}</p>}
      {loaded.state === 'ready' && loaded.data.length > 0 && (
        <ul className="lesson-list" aria-label={t('admin.curriculum.levels')}>
          {loaded.data.map((l) => (
            <li key={l.id} className="lesson-card">
              <h2>
                <BothNames fr={l.nameFr} ar={l.nameAr} />
              </h2>
              <p className="lesson-meta">
                <span className="state">
                  {t('admin.curriculum.level')} <code>{l.code}</code>
                </span>
                <span className="state">{t('admin.curriculum.order', { order: l.displayOrder })}</span>
                {l.retiredAt && <span className="state state-withdrawn">{t('admin.curriculum.levelRetiredTag')}</span>}
              </p>
              <details className="tools">
                <summary>{t('admin.curriculum.editLevel', { code: l.code })}</summary>
                <EntryForm
                  key={l.version}
                  label={t('admin.curriculum.editLevel', { code: l.code })}
                  submitLabel={t('admin.curriculum.saveLevel')}
                  withCode={false}
                  initial={l}
                  onSubmit={(_, f) => act(() => admin.updateLevel(l.id, l.version, f), t('admin.curriculum.levelSaved', { code: l.code }))}
                />
                {!l.retiredAt && (
                  <button type="button" className="danger" onClick={() => setRetire({ kind: 'level', entry: l })}>
                    {t('admin.curriculum.retireLevel', { code: l.code })}
                  </button>
                )}
              </details>
              <h3>{t('admin.curriculum.sections')}</h3>
              {l.sections.length === 0 ? (
                <p>{t('admin.curriculum.noSection')}</p>
              ) : (
                <ul className="lesson-list" aria-label={t('admin.curriculum.sectionsOf', { code: l.code })}>
                  {l.sections.map((s) => (
                    <li key={s.id} className="lesson-card">
                      <h4>
                        <Link to={paths.adminSection(s.id)}>
                          <BothNames fr={s.nameFr} ar={s.nameAr} />
                        </Link>
                      </h4>
                      <p className="lesson-meta">
                        <span className="state">
                          {t('admin.curriculum.section')} <code>{s.code}</code>
                        </span>
                        <span className="state">{t('admin.curriculum.order', { order: s.displayOrder })}</span>
                        {s.retiredAt && <span className="state state-withdrawn">{t('admin.curriculum.sectionRetiredTag')}</span>}
                      </p>
                      <p>
                        <Link to={paths.adminSection(s.id)}>{t('admin.curriculum.sectionEditions', { name: s.code })}</Link>
                      </p>
                      <details className="tools">
                        <summary>{t('admin.curriculum.editSection', { code: s.code })}</summary>
                        <EntryForm
                          key={s.version}
                          label={t('admin.curriculum.editSection', { code: s.code })}
                          submitLabel={t('admin.curriculum.saveSection')}
                          withCode={false}
                          initial={s}
                          onSubmit={(_, f) => act(() => admin.updateSection(s.id, s.version, f), t('admin.curriculum.sectionSaved', { code: s.code }))}
                        />
                        {!s.retiredAt && (
                          <button type="button" className="danger" onClick={() => setRetire({ kind: 'section', entry: s })}>
                            {t('admin.curriculum.retireSection', { code: s.code })}
                          </button>
                        )}
                      </details>
                    </li>
                  ))}
                </ul>
              )}
              {!l.retiredAt && (
                <details className="tools">
                  <summary>{t('admin.curriculum.newSection', { code: l.code })}</summary>
                  <EntryForm
                    label={t('admin.curriculum.newSection', { code: l.code })}
                    submitLabel={t('admin.curriculum.createSection')}
                    withCode
                    onSubmit={(code, f) => act(() => admin.createSection(l.id, code, f), t('admin.curriculum.sectionCreated', { code }))}
                  />
                </details>
              )}
            </li>
          ))}
        </ul>
      )}
      {retire && (
        <Dialog
          title={t(retire.kind === 'level' ? 'admin.curriculum.confirmRetireLevel' : 'admin.curriculum.confirmRetireSection', { code: retire.entry.code })}
          onCancel={() => setRetire(null)}
          actions={[{ label: t('admin.curriculum.retireForGood'), kind: 'danger', disabled: busy, onClick: () => void confirmRetire() }]}
        >
          <p>{t('admin.curriculum.retireEffect')}</p>
        </Dialog>
      )}
    </AdminPage>
  );
}

export function SectionPage({ sectionId }: { sectionId: number }) {
  const admin = useAdmin();
  const [loaded, reload] = useLoad(() => admin.editions(sectionId), [admin, sectionId]);
  const id = useId();
  const [code, setCode] = useState('');
  const [fr, setFr] = useState('');
  const [ar, setAr] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  if (loaded.state === 'loading')
    return (
      <AdminPage title={t('admin.curriculum.section')}>
        <p role="status">{t('admin.curriculum.loadingEditions')}</p>
      </AdminPage>
    );
  if (loaded.state === 'error')
    return (
      <AdminPage title={t('admin.curriculum.section')}>
        <LoadFailed message={loaded.message} onRetry={() => void reload()} />
        <p>
          <Link to={paths.adminCurriculum}>{t('admin.curriculum.back')}</Link>
        </p>
      </AdminPage>
    );
  const { section, editions } = loaded.data;

  const create = async (e: FormEvent) => {
    e.preventDefault();
    if (!/^[a-z0-9][a-z0-9-]{0,62}$/.test(code.trim())) return setError(CODE_HINT);
    if (!fr.trim() || !ar.trim()) return setError(t('admin.curriculum.labelsRequired'));
    setBusy(true);
    setError(null);
    try {
      const ed = await admin.createEdition(section.id, code.trim(), fr.trim(), ar.trim());
      navigate(paths.adminEdition(ed.id));
    } catch (err) {
      setError(describeAdminError(err));
      setBusy(false);
    }
  };

  return (
    <AdminPage title={t('admin.curriculum.sectionEditions', { name: uiName(section.nameFr, section.nameAr) })}>
      <p className="breadcrumb">
        <Link to={paths.adminCurriculum}>{t('admin.nav.curriculum')}</Link>
      </p>
      <p className="lesson-meta">
        <span className="state">
          {t('admin.curriculum.section')} <code>{section.code}</code>
        </span>
        <OtherName fr={section.nameFr} ar={section.nameAr} />
        {section.retiredAt && <span className="state state-withdrawn">{t('admin.curriculum.sectionRetiredTag')}</span>}
      </p>
      {editions.length === 0 ? (
        <p>{t('admin.curriculum.noEdition')}</p>
      ) : (
        <ul className="lesson-list" aria-label={t('admin.curriculum.editions')}>
          {editions.map((e) => (
            <li key={e.id} className="lesson-card">
              <h2>
                <Link to={paths.adminEdition(e.id)}>{uiName(e.labelFr, e.labelAr)}</Link>
              </h2>
              <p className="lesson-meta">
                <span className={`state${e.state === 'current' ? ' state-published' : e.state === 'retired' ? ' state-withdrawn' : ''}`}>{EDITION_STATE_LABELS[e.state]}</span>
                <span className="state">
                  <code>{e.code}</code>
                </span>
              </p>
            </li>
          ))}
        </ul>
      )}
      {!section.retiredAt && (
        <section aria-labelledby={`${id}-title`}>
          <h2 id={`${id}-title`}>{t('admin.curriculum.newEdition')}</h2>
          <form onSubmit={create} noValidate aria-label={t('admin.curriculum.newEdition')}>
            <div className="fields">
              <label htmlFor={`${id}-code`}>{t('admin.code')}</label>
              <p className="hint" id={`${id}-code-hint`}>
                {CODE_HINT}
              </p>
              <input id={`${id}-code`} dir="ltr" autoComplete="off" spellCheck={false} value={code} aria-describedby={`${id}-code-hint`} onChange={(e) => setCode(e.target.value)} />
            </div>
            <div className="fields">
              <label htmlFor={`${id}-fr`}>{t('admin.curriculum.labelFr')}</label>
              <input id={`${id}-fr`} lang="fr" dir="ltr" value={fr} maxLength={200} onChange={(e) => setFr(e.target.value)} />
            </div>
            <div className="fields">
              <label htmlFor={`${id}-ar`}>{t('admin.curriculum.labelAr')}</label>
              <input id={`${id}-ar`} lang="ar" dir="rtl" value={ar} maxLength={200} onChange={(e) => setAr(e.target.value)} />
            </div>
            {error && (
              <p className="banner error" role="alert">
                {error}
              </p>
            )}
            <button type="submit" className="primary" disabled={busy}>
              {t('admin.curriculum.createEdition')}
            </button>
          </form>
        </section>
      )}
    </AdminPage>
  );
}

type EditionPending = { kind: 'makeCurrent' } | { kind: 'retire' } | { kind: 'remove'; assignment: Assignment };

export function EditionPage({ editionId }: { editionId: number }) {
  const admin = useAdmin();
  const [loaded, reload] = useLoad(async () => {
    const edition = await admin.edition(editionId);
    const { section, editions } = await admin.editions(edition.sectionId);
    return { edition, section, current: currentEditionId(editions) };
  }, [admin, editionId]);
  const [catalogue] = useLoad(() => admin.catalogue(), [admin]);
  const [outcome, setOutcome] = useState<Outcome>(null);
  const [pending, setPending] = useState<EditionPending | null>(null);
  const [busy, setBusy] = useState(false);
  const [subjectId, setSubjectId] = useState('');
  const [labelFr, setLabelFr] = useState<string | null>(null);
  const [labelAr, setLabelAr] = useState<string | null>(null);
  const id = useId();

  if (loaded.state === 'loading')
    return (
      <AdminPage title={t('admin.edition.title')}>
        <p role="status">{t('admin.edition.loading')}</p>
      </AdminPage>
    );
  if (loaded.state === 'error')
    return (
      <AdminPage title={t('admin.edition.title')}>
        <LoadFailed message={loaded.message} onRetry={() => void reload()} />
        <p>
          <Link to={paths.adminCurriculum}>{t('admin.curriculum.back')}</Link>
        </p>
      </AdminPage>
    );
  const { edition, section, current } = loaded.data;
  const draft = edition.state === 'draft';
  const subjects: AdminSubject[] = catalogue.state === 'ready' ? catalogue.data : [];
  const assigned = new Set(edition.assignments.map((a) => a.subjectId));
  const addable = subjects.filter((s) => s.retiredAt === null && !assigned.has(s.id));

  const run = async (fn: () => Promise<unknown>, done: string) => {
    setBusy(true);
    setOutcome(null);
    try {
      await fn();
      setOutcome({ kind: 'success', message: done });
      await reload();
    } catch (e) {
      setOutcome(failure(e));
    } finally {
      setBusy(false);
      setPending(null);
    }
  };

  const confirm = () => {
    if (!pending) return;
    if (pending.kind === 'makeCurrent') void run(() => admin.makeCurrent(edition.id, edition.version, current), t('admin.edition.madeCurrent'));
    else if (pending.kind === 'retire') void run(() => admin.retireEdition(edition.id, edition.version), t('admin.edition.retired'));
    else void run(() => admin.removeAssignment(edition.id, pending.assignment.id, edition.version), t('admin.edition.subjectRemoved', { code: pending.assignment.subjectCode }));
  };

  const saveLabels = (e: FormEvent) => {
    e.preventDefault();
    const fr = (labelFr ?? edition.labelFr).trim();
    const ar = (labelAr ?? edition.labelAr).trim();
    if (!fr || !ar) return setOutcome({ kind: 'error', message: t('admin.curriculum.labelsRequired') });
    void run(async () => {
      await admin.renameEdition(edition.id, edition.version, fr, ar);
      setLabelFr(null);
      setLabelAr(null);
    }, t('admin.edition.labelsSaved'));
  };

  const addSubject = (e: FormEvent) => {
    e.preventDefault();
    if (!subjectId) return setOutcome({ kind: 'error', message: t('admin.chooseSubject') });
    void run(async () => {
      await admin.addAssignment(edition.id, edition.version, Number(subjectId));
      setSubjectId('');
    }, t('admin.edition.subjectAdded'));
  };

  return (
    <AdminPage title={t('admin.edition.titleNamed', { label: uiName(edition.labelFr, edition.labelAr) })}>
      <p className="breadcrumb">
        <Link to={paths.adminCurriculum}>{t('admin.nav.curriculum')}</Link> › <Link to={paths.adminSection(section.id)}>{t('admin.edition.section', { name: uiName(section.nameFr, section.nameAr) })}</Link>
      </p>
      <dl className="facts">
        <div>
          <dt>{t('admin.edition.state')}</dt>
          <dd>{EDITION_STATE_LABELS[edition.state]}</dd>
        </div>
        <div>
          <dt>{t('admin.code')}</dt>
          <dd>
            <code>{edition.code}</code>
          </dd>
        </div>
        <div>
          <dt>{locale() === 'ar' ? t('admin.curriculum.labelFr') : t('admin.curriculum.labelAr')}</dt>
          <dd {...(locale() === 'ar' ? { lang: 'fr', dir: 'ltr' } : { lang: 'ar', dir: 'rtl' })}>{locale() === 'ar' ? edition.labelFr : edition.labelAr}</dd>
        </div>
        {edition.currentSince && (
          <div>
            <dt>{t('admin.edition.currentSince')}</dt>
            <dd>{formatDateTime(edition.currentSince)}</dd>
          </div>
        )}
      </dl>
      <OutcomeBanner outcome={outcome} onReload={() => void reload()} />

      <section aria-labelledby={`${id}-state`}>
        <h2 id={`${id}-state`}>{t('admin.edition.stateTitle')}</h2>
        {draft && (
          <>
            <p>{t('admin.edition.draftText')}</p>
            <button type="button" className="primary" disabled={busy} onClick={() => setPending({ kind: 'makeCurrent' })}>
              {t('admin.edition.makeCurrent')}
            </button>
          </>
        )}
        {(edition.state === 'current' || edition.state === 'superseded') && (
          <>
            <p>{t('admin.edition.frozenText')}</p>
            <button type="button" className="danger" disabled={busy} onClick={() => setPending({ kind: 'retire' })}>
              {t('admin.edition.retire')}
            </button>
          </>
        )}
        {edition.state === 'retired' && <p>{t('admin.edition.retiredText')}</p>}
      </section>

      <section aria-labelledby={`${id}-labels`}>
        <h2 id={`${id}-labels`}>{t('admin.edition.labels')}</h2>
        <form onSubmit={saveLabels} noValidate aria-label={t('admin.edition.labelsForm')}>
          <div className="fields">
            <label htmlFor={`${id}-lfr`}>{t('admin.curriculum.labelFr')}</label>
            <input id={`${id}-lfr`} lang="fr" dir="ltr" value={labelFr ?? edition.labelFr} maxLength={200} onChange={(e) => setLabelFr(e.target.value)} />
          </div>
          <div className="fields">
            <label htmlFor={`${id}-lar`}>{t('admin.curriculum.labelAr')}</label>
            <input id={`${id}-lar`} lang="ar" dir="rtl" value={labelAr ?? edition.labelAr} maxLength={200} onChange={(e) => setLabelAr(e.target.value)} />
          </div>
          <button type="submit" disabled={busy}>
            {t('admin.edition.saveLabels')}
          </button>
        </form>
      </section>

      <section aria-labelledby={`${id}-subjects`}>
        <h2 id={`${id}-subjects`}>{t('admin.edition.subjects')}</h2>
        {catalogue.state === 'error' && <LoadFailed message={t('admin.edition.catalogueUnavailable', { message: catalogue.message })} onRetry={() => void reload()} />}
        {edition.assignments.length === 0 ? (
          <p>{t('admin.edition.noSubject')}</p>
        ) : (
          <ul className="lesson-list" aria-label={t('admin.edition.subjectsList')}>
            {edition.assignments.map((a) => (
              <li key={`${a.id}-${edition.version}`} className="lesson-card">
                <AssignmentCard
                  assignment={a}
                  subject={subjects.find((s) => s.id === a.subjectId)}
                  draft={draft}
                  busy={busy}
                  onSave={(ids) => run(() => admin.setChapters(edition.id, a.id, edition.version, ids), t('admin.edition.chaptersSaved', { code: a.subjectCode }))}
                  onRemove={() => setPending({ kind: 'remove', assignment: a })}
                />
              </li>
            ))}
          </ul>
        )}
        {draft && (
          <form className="find-lesson" onSubmit={addSubject}>
            <div className="fields">
              <label htmlFor={`${id}-subject`}>{t('admin.edition.subjectToAdd')}</label>
              <select id={`${id}-subject`} value={subjectId} onChange={(e) => setSubjectId(e.target.value)} disabled={catalogue.state !== 'ready'}>
                <option value="">{catalogue.state === 'loading' ? t('admin.loadingSubjects') : addable.length ? t('admin.chooseSubjectOption') : t('admin.noSubjectAvailable')}</option>
                {addable.map((s) => (
                  <option key={s.id} value={s.id}>
                    {entryLabel(s, locale()).text} ({s.code})
                  </option>
                ))}
              </select>
            </div>
            <button type="submit" disabled={busy || !subjectId}>
              {t('admin.edition.addSubject')}
            </button>
          </form>
        )}
      </section>

      {pending && (
        <Dialog
          title={
            pending.kind === 'makeCurrent'
              ? t('admin.edition.confirmMakeCurrent')
              : pending.kind === 'retire'
                ? t('admin.edition.confirmRetire')
                : t('admin.edition.confirmRemove', { code: pending.assignment.subjectCode })
          }
          onCancel={() => setPending(null)}
          actions={[
            {
              label: pending.kind === 'makeCurrent' ? t('admin.edition.makeCurrent') : pending.kind === 'retire' ? t('admin.edition.retireAction') : t('admin.edition.removeAction'),
              kind: pending.kind === 'makeCurrent' ? 'primary' : 'danger',
              disabled: busy,
              onClick: confirm,
            },
          ]}
        >
          <p>
            {pending.kind === 'makeCurrent'
              ? current !== null
                ? t('admin.edition.replaceText')
                : t('admin.edition.freezeText')
              : pending.kind === 'retire'
                ? t('admin.edition.retireText')
                : t('admin.edition.removeText')}
          </p>
        </Dialog>
      )}
    </AdminPage>
  );
}

function AssignmentCard({
  assignment,
  subject,
  draft,
  busy,
  onSave,
  onRemove,
}: {
  assignment: Assignment;
  subject: AdminSubject | undefined;
  draft: boolean;
  busy: boolean;
  onSave: (chapterIds: number[]) => Promise<void>;
  onRemove: () => void;
}) {
  const id = useId();
  const [list, setList] = useState(() => assignment.chapters.map((c) => c.chapterId));
  const [toAdd, setToAdd] = useState('');
  const chapters = new Map((subject?.chapters ?? []).map((c) => [c.id, c]));
  const name = (chapterId: number) => {
    const c = chapters.get(chapterId);
    const fallback = assignment.chapters.find((x) => x.chapterId === chapterId);
    return c ? `${entryLabel(c, locale()).text} (${c.code})` : (fallback?.code ?? t('admin.edition.chapterId', { id: chapterId }));
  };
  const addable = (subject?.chapters ?? []).filter((c) => c.retiredAt === null && !list.includes(c.id));
  const changed = list.join(',') !== assignment.chapters.map((c) => c.chapterId).join(',');
  const label = subject ? entryLabel(subject, locale()) : { text: assignment.subjectCode, lang: null };

  return (
    <>
      <h3>
        <CatalogueName label={label} /> <code>{assignment.subjectCode}</code>
      </h3>
      <p className="lesson-meta">{assignment.selectable ? <span className="state state-published">{t('admin.edition.selectable')}</span> : <span className="state">{t('admin.edition.notSelectable')}</span>}</p>
      {list.length === 0 ? (
        <p>{t('admin.noChapter')}</p>
      ) : (
        <ol className="chapter-order" aria-label={t('admin.chaptersOf', { code: assignment.subjectCode })}>
          {list.map((chapterId, i) => (
            <li key={chapterId}>
              <span>{name(chapterId)}</span>
              {draft && (
                <span className="row-actions">
                  <button type="button" disabled={busy || i === 0} onClick={() => setList(move(list, i, -1))} aria-label={t('admin.edition.moveUpNamed', { name: name(chapterId) })}>
                    {t('admin.edition.moveUp')}
                  </button>
                  <button type="button" disabled={busy || i === list.length - 1} onClick={() => setList(move(list, i, 1))} aria-label={t('admin.edition.moveDownNamed', { name: name(chapterId) })}>
                    {t('admin.edition.moveDown')}
                  </button>
                  <button type="button" disabled={busy} onClick={() => setList(list.filter((x) => x !== chapterId))} aria-label={t('admin.edition.dropNamed', { name: name(chapterId) })}>
                    {t('admin.edition.drop')}
                  </button>
                </span>
              )}
            </li>
          ))}
        </ol>
      )}
      {draft && (
        <>
          <div className="find-lesson">
            <div className="fields">
              <label htmlFor={`${id}-chapter`}>{t('admin.edition.chapterToAdd', { code: assignment.subjectCode })}</label>
              <select id={`${id}-chapter`} value={toAdd} onChange={(e) => setToAdd(e.target.value)}>
                <option value="">{addable.length ? t('admin.edition.chooseChapter') : t('admin.edition.noChapterAvailable')}</option>
                {addable.map((c) => (
                  <option key={c.id} value={c.id}>
                    {entryLabel(c, locale()).text} ({c.code})
                  </option>
                ))}
              </select>
            </div>
            <button
              type="button"
              disabled={busy || !toAdd}
              onClick={() => {
                setList([...list, Number(toAdd)]);
                setToAdd('');
              }}
            >
              {t('admin.edition.addChapter')}
            </button>
          </div>
          <p className="actions">
            <button type="button" className="primary" disabled={busy || !changed} onClick={() => void onSave(list)}>
              {t('admin.edition.saveChapters')}
            </button>{' '}
            <button type="button" className="danger" disabled={busy} onClick={onRemove}>
              {t('admin.edition.removeSubject', { code: assignment.subjectCode })}
            </button>
          </p>
        </>
      )}
    </>
  );
}
