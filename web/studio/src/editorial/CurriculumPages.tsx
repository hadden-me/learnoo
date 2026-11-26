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
import { formatDateTime } from '../labels';
import { navigate } from '../router';
import { Dialog } from '../ui/Dialog';
import { Link } from '../ui/Link';
import { AdminPage, LoadFailed, OutcomeBanner, failure, useAdmin, type Outcome } from './adminCommon';
import { useLoad } from './common';

/** Validates an entry form; returns the fields to send or a French error. */
export function entryFields(nameFr: string, nameAr: string, order: string): EntryFields | { error: string } {
  const fr = nameFr.trim();
  const ar = nameAr.trim();
  if (!fr || !ar) return { error: 'Les noms français et arabe sont obligatoires.' };
  const n = Number(order);
  if (order.trim() === '' || !Number.isInteger(n) || n < 0 || n > 9999) return { error: 'L’ordre doit être un nombre entier de 0 à 9999.' };
  return { nameFr: fr, nameAr: ar, displayOrder: n };
}

const CODE_HINT = 'Code : lettres minuscules, chiffres et tirets (63 au plus), sans tiret au début. Il ne pourra plus être changé ni réutilisé.';

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
          <label htmlFor={`${id}-code`}>Code</label>
          <p className="hint" id={`${id}-code-hint`}>
            {CODE_HINT}
          </p>
          <input id={`${id}-code`} dir="ltr" autoComplete="off" spellCheck={false} value={code} aria-describedby={`${id}-code-hint`} onChange={(e) => setCode(e.target.value)} />
        </div>
      )}
      <div className="fields">
        <label htmlFor={`${id}-fr`}>Nom français</label>
        <input id={`${id}-fr`} value={fr} maxLength={200} onChange={(e) => setFr(e.target.value)} />
      </div>
      <div className="fields">
        <label htmlFor={`${id}-ar`}>Nom arabe</label>
        <input id={`${id}-ar`} lang="ar" dir="rtl" value={ar} maxLength={200} onChange={(e) => setAr(e.target.value)} />
      </div>
      <div className="fields">
        <label htmlFor={`${id}-order`}>Ordre d’affichage</label>
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
      if (retire.kind === 'level') await act(() => admin.retireLevel(retire.entry.id, retire.entry.version), `Niveau ${retire.entry.code} retiré.`);
      else await act(() => admin.retireSection(retire.entry.id, retire.entry.version), `Section ${retire.entry.code} retirée.`);
    } catch (e) {
      setOutcome(failure(e));
    } finally {
      setBusy(false);
      setRetire(null);
    }
  };

  return (
    <AdminPage
      title="Programmes"
      intro={
        <p className="hint">
          Niveaux, sections et leurs éditions de programme. Les codes ne changent jamais et rien n’est supprimé : un élément obsolète est retiré, définitivement. Chaque changement est
          enregistré.
        </p>
      }
    >
      <OutcomeBanner outcome={outcome} onReload={() => void reload()} />
      <details className="tools">
        <summary>Nouveau niveau</summary>
        <EntryForm label="Nouveau niveau" submitLabel="Créer le niveau" withCode onSubmit={(code, f) => act(() => admin.createLevel(code, f), `Niveau ${code} créé.`)} />
      </details>
      {loaded.state === 'loading' && <p role="status">Chargement des niveaux…</p>}
      {loaded.state === 'error' && <LoadFailed message={loaded.message} onRetry={() => void reload()} />}
      {loaded.state === 'ready' && loaded.data.length === 0 && <p>Aucun niveau pour l’instant. Créez le premier ci-dessus.</p>}
      {loaded.state === 'ready' && loaded.data.length > 0 && (
        <ul className="lesson-list" aria-label="Niveaux">
          {loaded.data.map((l) => (
            <li key={l.id} className="lesson-card">
              <h2>
                {l.nameFr} <bdi lang="ar" dir="rtl">{l.nameAr}</bdi>
              </h2>
              <p className="lesson-meta">
                <span className="state">
                  Niveau <code>{l.code}</code>
                </span>
                <span className="state">Ordre {l.displayOrder}</span>
                {l.retiredAt && <span className="state state-withdrawn">Retiré</span>}
              </p>
              <details className="tools">
                <summary>Modifier le niveau {l.code}</summary>
                <EntryForm
                  key={l.version}
                  label={`Modifier le niveau ${l.code}`}
                  submitLabel="Enregistrer le niveau"
                  withCode={false}
                  initial={l}
                  onSubmit={(_, f) => act(() => admin.updateLevel(l.id, l.version, f), `Niveau ${l.code} enregistré.`)}
                />
                {!l.retiredAt && (
                  <button type="button" className="danger" onClick={() => setRetire({ kind: 'level', entry: l })}>
                    Retirer le niveau {l.code}
                  </button>
                )}
              </details>
              <h3>Sections</h3>
              {l.sections.length === 0 ? (
                <p>Aucune section.</p>
              ) : (
                <ul className="lesson-list" aria-label={`Sections du niveau ${l.code}`}>
                  {l.sections.map((s) => (
                    <li key={s.id} className="lesson-card">
                      <h4>
                        <Link to={paths.adminSection(s.id)}>
                          {s.nameFr} <bdi lang="ar" dir="rtl">{s.nameAr}</bdi>
                        </Link>
                      </h4>
                      <p className="lesson-meta">
                        <span className="state">
                          Section <code>{s.code}</code>
                        </span>
                        <span className="state">Ordre {s.displayOrder}</span>
                        {s.retiredAt && <span className="state state-withdrawn">Retirée</span>}
                      </p>
                      <p>
                        <Link to={paths.adminSection(s.id)}>Éditions de la section {s.code}</Link>
                      </p>
                      <details className="tools">
                        <summary>Modifier la section {s.code}</summary>
                        <EntryForm
                          key={s.version}
                          label={`Modifier la section ${s.code}`}
                          submitLabel="Enregistrer la section"
                          withCode={false}
                          initial={s}
                          onSubmit={(_, f) => act(() => admin.updateSection(s.id, s.version, f), `Section ${s.code} enregistrée.`)}
                        />
                        {!s.retiredAt && (
                          <button type="button" className="danger" onClick={() => setRetire({ kind: 'section', entry: s })}>
                            Retirer la section {s.code}
                          </button>
                        )}
                      </details>
                    </li>
                  ))}
                </ul>
              )}
              {!l.retiredAt && (
                <details className="tools">
                  <summary>Nouvelle section dans {l.code}</summary>
                  <EntryForm
                    label={`Nouvelle section dans ${l.code}`}
                    submitLabel="Créer la section"
                    withCode
                    onSubmit={(code, f) => act(() => admin.createSection(l.id, code, f), `Section ${code} créée.`)}
                  />
                </details>
              )}
            </li>
          ))}
        </ul>
      )}
      {retire && (
        <Dialog
          title={retire.kind === 'level' ? `Retirer le niveau ${retire.entry.code} ?` : `Retirer la section ${retire.entry.code} ?`}
          onCancel={() => setRetire(null)}
          actions={[{ label: 'Retirer définitivement', kind: 'danger', disabled: busy, onClick: () => void confirmRetire() }]}
        >
          <p>Le retrait est définitif. L’élément reste lisible mais ne peut plus recevoir de section ou d’édition ni être sélectionné.</p>
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
      <AdminPage title="Section">
        <p role="status">Chargement des éditions…</p>
      </AdminPage>
    );
  if (loaded.state === 'error')
    return (
      <AdminPage title="Section">
        <LoadFailed message={loaded.message} onRetry={() => void reload()} />
        <p>
          <Link to={paths.adminCurriculum}>Revenir aux programmes</Link>
        </p>
      </AdminPage>
    );
  const { section, editions } = loaded.data;

  const create = async (e: FormEvent) => {
    e.preventDefault();
    if (!/^[a-z0-9][a-z0-9-]{0,62}$/.test(code.trim())) return setError(CODE_HINT);
    if (!fr.trim() || !ar.trim()) return setError('Les libellés français et arabe sont obligatoires.');
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
    <AdminPage title={`Éditions de la section ${section.nameFr}`}>
      <p className="breadcrumb">
        <Link to={paths.adminCurriculum}>Programmes</Link>
      </p>
      <p className="lesson-meta">
        <span className="state">
          Section <code>{section.code}</code>
        </span>
        <bdi lang="ar" dir="rtl">
          {section.nameAr}
        </bdi>
        {section.retiredAt && <span className="state state-withdrawn">Retirée</span>}
      </p>
      {editions.length === 0 ? (
        <p>Aucune édition pour cette section.</p>
      ) : (
        <ul className="lesson-list" aria-label="Éditions">
          {editions.map((e) => (
            <li key={e.id} className="lesson-card">
              <h2>
                <Link to={paths.adminEdition(e.id)}>{e.labelFr}</Link>
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
          <h2 id={`${id}-title`}>Nouvelle édition</h2>
          <form onSubmit={create} noValidate aria-label="Nouvelle édition">
            <div className="fields">
              <label htmlFor={`${id}-code`}>Code</label>
              <p className="hint" id={`${id}-code-hint`}>
                {CODE_HINT}
              </p>
              <input id={`${id}-code`} dir="ltr" autoComplete="off" spellCheck={false} value={code} aria-describedby={`${id}-code-hint`} onChange={(e) => setCode(e.target.value)} />
            </div>
            <div className="fields">
              <label htmlFor={`${id}-fr`}>Libellé français</label>
              <input id={`${id}-fr`} value={fr} maxLength={200} onChange={(e) => setFr(e.target.value)} />
            </div>
            <div className="fields">
              <label htmlFor={`${id}-ar`}>Libellé arabe</label>
              <input id={`${id}-ar`} lang="ar" dir="rtl" value={ar} maxLength={200} onChange={(e) => setAr(e.target.value)} />
            </div>
            {error && (
              <p className="banner error" role="alert">
                {error}
              </p>
            )}
            <button type="submit" className="primary" disabled={busy}>
              Créer l’édition
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
      <AdminPage title="Édition">
        <p role="status">Chargement de l’édition…</p>
      </AdminPage>
    );
  if (loaded.state === 'error')
    return (
      <AdminPage title="Édition">
        <LoadFailed message={loaded.message} onRetry={() => void reload()} />
        <p>
          <Link to={paths.adminCurriculum}>Revenir aux programmes</Link>
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
    if (pending.kind === 'makeCurrent') void run(() => admin.makeCurrent(edition.id, edition.version, current), 'Édition mise en vigueur.');
    else if (pending.kind === 'retire') void run(() => admin.retireEdition(edition.id, edition.version), 'Édition retirée.');
    else void run(() => admin.removeAssignment(edition.id, pending.assignment.id, edition.version), `Matière ${pending.assignment.subjectCode} retirée de l’édition.`);
  };

  const saveLabels = (e: FormEvent) => {
    e.preventDefault();
    const fr = (labelFr ?? edition.labelFr).trim();
    const ar = (labelAr ?? edition.labelAr).trim();
    if (!fr || !ar) return setOutcome({ kind: 'error', message: 'Les libellés français et arabe sont obligatoires.' });
    void run(async () => {
      await admin.renameEdition(edition.id, edition.version, fr, ar);
      setLabelFr(null);
      setLabelAr(null);
    }, 'Libellés enregistrés.');
  };

  const addSubject = (e: FormEvent) => {
    e.preventDefault();
    if (!subjectId) return setOutcome({ kind: 'error', message: 'Choisissez une matière.' });
    void run(async () => {
      await admin.addAssignment(edition.id, edition.version, Number(subjectId));
      setSubjectId('');
    }, 'Matière ajoutée à l’édition.');
  };

  return (
    <AdminPage title={`Édition ${edition.labelFr}`}>
      <p className="breadcrumb">
        <Link to={paths.adminCurriculum}>Programmes</Link> › <Link to={paths.adminSection(section.id)}>Section {section.nameFr}</Link>
      </p>
      <dl className="facts">
        <div>
          <dt>État</dt>
          <dd>{EDITION_STATE_LABELS[edition.state]}</dd>
        </div>
        <div>
          <dt>Code</dt>
          <dd>
            <code>{edition.code}</code>
          </dd>
        </div>
        <div>
          <dt>Libellé arabe</dt>
          <dd lang="ar" dir="rtl">
            {edition.labelAr}
          </dd>
        </div>
        {edition.currentSince && (
          <div>
            <dt>En vigueur depuis</dt>
            <dd>{formatDateTime(edition.currentSince)}</dd>
          </div>
        )}
      </dl>
      <OutcomeBanner outcome={outcome} onReload={() => void reload()} />

      <section aria-labelledby={`${id}-state`}>
        <h2 id={`${id}-state`}>État de l’édition</h2>
        {draft && (
          <>
            <p>Brouillon : les matières et listes de chapitres sont modifiables. Une fois en vigueur, elles ne changent plus ; l’édition en vigueur actuelle de la section sera remplacée.</p>
            <button type="button" className="primary" disabled={busy} onClick={() => setPending({ kind: 'makeCurrent' })}>
              Mettre en vigueur
            </button>
          </>
        )}
        {(edition.state === 'current' || edition.state === 'superseded') && (
          <>
            <p>Les matières et les chapitres de cette édition ne peuvent plus changer. Pour modifier le programme, créez une nouvelle édition.</p>
            <button type="button" className="danger" disabled={busy} onClick={() => setPending({ kind: 'retire' })}>
              Retirer l’édition
            </button>
          </>
        )}
        {edition.state === 'retired' && <p>Cette édition est retirée.</p>}
      </section>

      <section aria-labelledby={`${id}-labels`}>
        <h2 id={`${id}-labels`}>Libellés</h2>
        <form onSubmit={saveLabels} noValidate aria-label="Libellés de l’édition">
          <div className="fields">
            <label htmlFor={`${id}-lfr`}>Libellé français</label>
            <input id={`${id}-lfr`} value={labelFr ?? edition.labelFr} maxLength={200} onChange={(e) => setLabelFr(e.target.value)} />
          </div>
          <div className="fields">
            <label htmlFor={`${id}-lar`}>Libellé arabe</label>
            <input id={`${id}-lar`} lang="ar" dir="rtl" value={labelAr ?? edition.labelAr} maxLength={200} onChange={(e) => setLabelAr(e.target.value)} />
          </div>
          <button type="submit" disabled={busy}>
            Enregistrer les libellés
          </button>
        </form>
      </section>

      <section aria-labelledby={`${id}-subjects`}>
        <h2 id={`${id}-subjects`}>Matières et chapitres</h2>
        {catalogue.state === 'error' && <LoadFailed message={`Catalogue indisponible. ${catalogue.message}`} onRetry={() => void reload()} />}
        {edition.assignments.length === 0 ? (
          <p>Aucune matière dans cette édition.</p>
        ) : (
          <ul className="lesson-list" aria-label="Matières de l’édition">
            {edition.assignments.map((a) => (
              <li key={`${a.id}-${edition.version}`} className="lesson-card">
                <AssignmentCard
                  assignment={a}
                  subject={subjects.find((s) => s.id === a.subjectId)}
                  draft={draft}
                  busy={busy}
                  onSave={(ids) => run(() => admin.setChapters(edition.id, a.id, edition.version, ids), `Liste des chapitres de ${a.subjectCode} enregistrée.`)}
                  onRemove={() => setPending({ kind: 'remove', assignment: a })}
                />
              </li>
            ))}
          </ul>
        )}
        {draft && (
          <form className="find-lesson" onSubmit={addSubject}>
            <div className="fields">
              <label htmlFor={`${id}-subject`}>Matière à ajouter</label>
              <select id={`${id}-subject`} value={subjectId} onChange={(e) => setSubjectId(e.target.value)} disabled={catalogue.state !== 'ready'}>
                <option value="">{catalogue.state === 'loading' ? 'Chargement des matières…' : addable.length ? 'Choisir une matière' : 'Aucune matière disponible'}</option>
                {addable.map((s) => (
                  <option key={s.id} value={s.id}>
                    {entryLabel(s, 'fr').text} ({s.code})
                  </option>
                ))}
              </select>
            </div>
            <button type="submit" disabled={busy || !subjectId}>
              Ajouter la matière
            </button>
          </form>
        )}
      </section>

      {pending && (
        <Dialog
          title={
            pending.kind === 'makeCurrent'
              ? 'Mettre cette édition en vigueur ?'
              : pending.kind === 'retire'
                ? 'Retirer cette édition ?'
                : `Retirer la matière ${pending.assignment.subjectCode} de l’édition ?`
          }
          onCancel={() => setPending(null)}
          actions={[
            {
              label: pending.kind === 'makeCurrent' ? 'Mettre en vigueur' : 'Retirer',
              kind: pending.kind === 'makeCurrent' ? 'primary' : 'danger',
              disabled: busy,
              onClick: confirm,
            },
          ]}
        >
          <p>
            {pending.kind === 'makeCurrent'
              ? current !== null
                ? 'L’édition actuellement en vigueur de la section sera remplacée. Les matières et chapitres de celle-ci ne pourront plus changer. Cette action ne peut pas être annulée.'
                : 'Les matières et chapitres de cette édition ne pourront plus changer. Cette action ne peut pas être annulée.'
              : pending.kind === 'retire'
                ? 'Le retrait est définitif. La section n’aura plus d’édition en vigueur si c’était celle-ci.'
                : 'La matière et sa liste de chapitres sont retirées de ce brouillon.'}
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
    return c ? `${entryLabel(c, 'fr').text} (${c.code})` : (fallback?.code ?? `chapitre n° ${chapterId}`);
  };
  const addable = (subject?.chapters ?? []).filter((c) => c.retiredAt === null && !list.includes(c.id));
  const changed = list.join(',') !== assignment.chapters.map((c) => c.chapterId).join(',');
  const label = subject ? entryLabel(subject, 'fr') : { text: assignment.subjectCode, lang: null };

  return (
    <>
      <h3>
        <CatalogueName label={label} /> <code>{assignment.subjectCode}</code>
      </h3>
      <p className="lesson-meta">{assignment.selectable ? <span className="state state-published">Sélectionnable</span> : <span className="state">Non sélectionnable</span>}</p>
      {list.length === 0 ? (
        <p>Aucun chapitre.</p>
      ) : (
        <ol className="chapter-order" aria-label={`Chapitres de ${assignment.subjectCode}`}>
          {list.map((chapterId, i) => (
            <li key={chapterId}>
              <span>{name(chapterId)}</span>
              {draft && (
                <span className="row-actions">
                  <button type="button" disabled={busy || i === 0} onClick={() => setList(move(list, i, -1))} aria-label={`Monter ${name(chapterId)}`}>
                    Monter
                  </button>
                  <button type="button" disabled={busy || i === list.length - 1} onClick={() => setList(move(list, i, 1))} aria-label={`Descendre ${name(chapterId)}`}>
                    Descendre
                  </button>
                  <button type="button" disabled={busy} onClick={() => setList(list.filter((x) => x !== chapterId))} aria-label={`Enlever ${name(chapterId)}`}>
                    Enlever
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
              <label htmlFor={`${id}-chapter`}>Chapitre à ajouter ({assignment.subjectCode})</label>
              <select id={`${id}-chapter`} value={toAdd} onChange={(e) => setToAdd(e.target.value)}>
                <option value="">{addable.length ? 'Choisir un chapitre' : 'Aucun chapitre disponible'}</option>
                {addable.map((c) => (
                  <option key={c.id} value={c.id}>
                    {entryLabel(c, 'fr').text} ({c.code})
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
              Ajouter le chapitre
            </button>
          </div>
          <p className="actions">
            <button type="button" className="primary" disabled={busy || !changed} onClick={() => void onSave(list)}>
              Enregistrer la liste des chapitres
            </button>{' '}
            <button type="button" className="danger" disabled={busy} onClick={onRemove}>
              Retirer la matière {assignment.subjectCode}
            </button>
          </p>
        </>
      )}
    </>
  );
}
