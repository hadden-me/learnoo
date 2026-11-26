// Catalogue display names and order (LMS-B10, migrations/0012): French and
// Arabic names and the display order of subjects and chapters. Codes never
// change and adding entries stays an operator command. Each change is
// audited by the server with old and new values.
import { useId, useState, type FormEvent } from 'react';
import type { CatalogueEntry } from '../api/client';
import { AdminPage, LoadFailed, OutcomeBanner, failure, useAdmin, type Outcome } from './adminCommon';
import { useLoad } from './common';

/** What to send for an entry's edited names and order; an error when a name was emptied (names can only be replaced). */
export function entryChanges(entry: Pick<CatalogueEntry, 'nameFr' | 'nameAr' | 'sortOrder'>, fr: string, ar: string, order: string) {
  const names: { nameFr?: string; nameAr?: string } = {};
  const f = fr.trim();
  const a = ar.trim();
  if (!f && entry.nameFr) return { error: 'Un nom ne peut pas être effacé, seulement remplacé.' } as const;
  if (!a && entry.nameAr) return { error: 'Un nom ne peut pas être effacé, seulement remplacé.' } as const;
  if (f && f !== entry.nameFr) names.nameFr = f;
  if (a && a !== entry.nameAr) names.nameAr = a;
  const n = Number(order);
  if (order.trim() === '' || !Number.isInteger(n) || n < 0 || n > 9999) return { error: 'L’ordre doit être un nombre entier de 0 à 9999.' } as const;
  return { names: Object.keys(names).length ? names : null, order: n !== entry.sortOrder ? n : null } as const;
}

export function CataloguePage() {
  const admin = useAdmin();
  const [loaded, reload] = useLoad(() => admin.catalogue(), [admin]);
  return (
    <AdminPage
      title="Catalogue"
      intro={
        <p className="hint">
          Noms d’affichage (français et arabe) et ordre des matières et des chapitres. Les codes ne changent jamais ; un nom peut être remplacé mais pas effacé. Chaque changement est
          enregistré avec l’ancienne et la nouvelle valeur.
        </p>
      }
    >
      {loaded.state === 'loading' && <p role="status">Chargement du catalogue…</p>}
      {loaded.state === 'error' && <LoadFailed message={loaded.message} onRetry={() => void reload()} />}
      {loaded.state === 'ready' && loaded.data.length === 0 && <p>Le catalogue est vide. Les matières et chapitres sont ajoutés par les commandes d’exploitation.</p>}
      {loaded.state === 'ready' && loaded.data.length > 0 && (
        <ul className="lesson-list" aria-label="Matières">
          {loaded.data.map((s) => (
            <li key={s.id} className="lesson-card">
              <h2>
                Matière <code>{s.code}</code> {s.retiredAt && <span className="state">Retirée</span>}
              </h2>
              <EntryEditor
                entry={s}
                kind="la matière"
                save={async (names, order) => {
                  if (names) await admin.setSubjectNames(s.code, names);
                  if (order !== null) await admin.setSubjectOrder(s.code, order);
                }}
                onSaved={reload}
              />
              <details className="tools">
                <summary>Chapitres ({s.chapters.length})</summary>
                {s.chapters.length === 0 ? (
                  <p>Aucun chapitre.</p>
                ) : (
                  <ul className="lesson-list" aria-label={`Chapitres de ${s.code}`}>
                    {s.chapters.map((c) => (
                      <li key={c.id} className="lesson-card">
                        <h3>
                          Chapitre <code>{c.code}</code> {c.retiredAt && <span className="state">Retiré</span>}
                        </h3>
                        <EntryEditor
                          entry={c}
                          kind="le chapitre"
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
  kind,
  save,
  onSaved,
}: {
  entry: CatalogueEntry;
  kind: string;
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
    if (!c.names && c.order === null) return setOutcome({ kind: 'success', message: 'Aucun changement.' });
    setBusy(true);
    setOutcome(null);
    try {
      await save(c.names, c.order);
      setOutcome({ kind: 'success', message: 'Enregistré.' });
      await onSaved();
    } catch (err) {
      setOutcome(failure(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form className="entry-form" onSubmit={submit} noValidate aria-label={`Noms et ordre de ${kind} ${entry.code}`}>
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
      <OutcomeBanner outcome={outcome} />
      <button type="submit" disabled={busy}>
        Enregistrer
      </button>
    </form>
  );
}
