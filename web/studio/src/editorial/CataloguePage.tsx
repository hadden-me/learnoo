// Catalogue display names and order (LMS-B10, migrations/0012): French and
// Arabic names and the display order of subjects and chapters. Codes never
// change and adding entries stays an operator command. Each change is
// audited by the server with old and new values.
import { useId, useState, type FormEvent } from 'react';
import type { CatalogueEntry } from '../api/client';
import { t } from '../i18n';
import { AdminPage, LoadFailed, OutcomeBanner, failure, useAdmin, type Outcome } from './adminCommon';
import { useLoad } from './common';

/** What to send for an entry's edited names and order; an error when a name was emptied (names can only be replaced). */
export function entryChanges(entry: Pick<CatalogueEntry, 'nameFr' | 'nameAr' | 'sortOrder'>, fr: string, ar: string, order: string) {
  const names: { nameFr?: string; nameAr?: string } = {};
  const f = fr.trim();
  const a = ar.trim();
  if (!f && entry.nameFr) return { error: t('admin.catalogue.nameErased') } as const;
  if (!a && entry.nameAr) return { error: t('admin.catalogue.nameErased') } as const;
  if (f && f !== entry.nameFr) names.nameFr = f;
  if (a && a !== entry.nameAr) names.nameAr = a;
  const n = Number(order);
  if (order.trim() === '' || !Number.isInteger(n) || n < 0 || n > 9999) return { error: t('admin.orderInvalid') } as const;
  return { names: Object.keys(names).length ? names : null, order: n !== entry.sortOrder ? n : null } as const;
}

export function CataloguePage() {
  const admin = useAdmin();
  const [loaded, reload] = useLoad(() => admin.catalogue(), [admin]);
  return (
    <AdminPage
      title={t('admin.nav.catalogue')}
      intro={<p className="hint">{t('admin.catalogue.intro')}</p>}
    >
      {loaded.state === 'loading' && <p role="status">{t('admin.catalogue.loading')}</p>}
      {loaded.state === 'error' && <LoadFailed message={loaded.message} onRetry={() => void reload()} />}
      {loaded.state === 'ready' && loaded.data.length === 0 && <p>{t('admin.catalogue.empty')}</p>}
      {loaded.state === 'ready' && loaded.data.length > 0 && (
        <ul className="lesson-list" aria-label={t('admin.catalogue.subjects')}>
          {loaded.data.map((s) => (
            <li key={s.id} className="lesson-card">
              <h2>
                {t('admin.catalogue.subject')} <code>{s.code}</code> {s.retiredAt && <span className="state">{t('admin.catalogue.subjectRetired')}</span>}
              </h2>
              <EntryEditor
                entry={s}
                formLabel={t('admin.catalogue.subjectForm', { code: s.code })}
                save={async (names, order) => {
                  if (names) await admin.setSubjectNames(s.code, names);
                  if (order !== null) await admin.setSubjectOrder(s.code, order);
                }}
                onSaved={reload}
              />
              <details className="tools">
                <summary>{t('admin.catalogue.chapters', { count: s.chapters.length })}</summary>
                {s.chapters.length === 0 ? (
                  <p>{t('admin.noChapter')}</p>
                ) : (
                  <ul className="lesson-list" aria-label={t('admin.chaptersOf', { code: s.code })}>
                    {s.chapters.map((c) => (
                      <li key={c.id} className="lesson-card">
                        <h3>
                          {t('admin.catalogue.chapter')} <code>{c.code}</code> {c.retiredAt && <span className="state">{t('admin.catalogue.chapterRetired')}</span>}
                        </h3>
                        <EntryEditor
                          entry={c}
                          formLabel={t('admin.catalogue.chapterForm', { code: c.code })}
                          save={async (names, order) => {
                            if (names) await admin.setChapterNames(s.code, c.code, names);
                            if (order !== null) await admin.setChapterOrder(s.code, c.code, order);
                          }}
                          onSaved={reload}
                        />
                      </li>
                    ))}
                  </ul>
                )}
              </details>
            </li>
          ))}
        </ul>
      )}
    </AdminPage>
  );
}

function EntryEditor({
  entry,
  formLabel,
  save,
  onSaved,
}: {
  entry: CatalogueEntry;
  /** The form's accessible name. */
  formLabel: string;
  save: (names: { nameFr?: string; nameAr?: string } | null, order: number | null) => Promise<void>;
  onSaved: () => Promise<void>;
}) {
  const id = useId();
  const [fr, setFr] = useState(entry.nameFr ?? '');
  const [ar, setAr] = useState(entry.nameAr ?? '');
  const [order, setOrder] = useState(String(entry.sortOrder));
  const [busy, setBusy] = useState(false);
  const [outcome, setOutcome] = useState<Outcome>(null);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const c = entryChanges(entry, fr, ar, order);
    if ('error' in c) return setOutcome({ kind: 'error', message: c.error ?? '' });
    if (!c.names && c.order === null) return setOutcome({ kind: 'success', message: t('admin.catalogue.noChange') });
    setBusy(true);
    setOutcome(null);
    try {
      await save(c.names, c.order);
      setOutcome({ kind: 'success', message: t('admin.catalogue.saved') });
      await onSaved();
    } catch (err) {
      setOutcome(failure(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form className="entry-form" onSubmit={submit} noValidate aria-label={formLabel}>
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
      <OutcomeBanner outcome={outcome} />
      <button type="submit" disabled={busy}>
        {t('admin.save')}
      </button>
    </form>
  );
}
