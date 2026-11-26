// Curriculum placement of a course draft: the selected assignments (an
// edition's assignment of the course's subject) and a level → section
// picker that offers only the section's current edition, and only when it
// assigns the course's subject and the assignment can be newly selected.
// Superseded or retired placements carried from an earlier version are
// shown flagged; retired entries are never offered. The server checks every
// placement again on save (422 placement_invalid).
import { useEffect, useId, useRef, useState } from 'react';
import type { CoursePlacement, CourseApi, Edition, Level } from '../api/courses';
import { describeCourseError } from '../api/courseMessages';
import { CatalogueName, catalogueLabel, labelAttrs, type CatalogueLabel, type Lang } from '../catalogue';
import { EDITION_STATE_LABELS } from './courseLabels';

export type PlacementInfo = Pick<CoursePlacement, 'assignmentId' | 'editionId' | 'editionState' | 'selectable' | 'retired'>;

export interface PlacementNames {
  level: CatalogueLabel | null;
  section: CatalogueLabel | null;
  edition: CatalogueLabel | null;
}

export function placementNames(edition: Edition | undefined, levels: Level[], lang: Lang): PlacementNames {
  if (!edition) return { level: null, section: null, edition: null };
  for (const level of levels) {
    const section = level.sections.find((s) => s.id === edition.sectionId);
    if (section)
      return {
        level: catalogueLabel(level.code, level.nameFr, level.nameAr, lang),
        section: catalogueLabel(section.code, section.nameFr, section.nameAr, lang),
        edition: catalogueLabel(edition.code, edition.labelFr, edition.labelAr, lang),
      };
  }
  return { level: null, section: null, edition: catalogueLabel(edition.code, edition.labelFr, edition.labelAr, lang) };
}

/** "Niveau › Section › Édition" in running French text. */
export function PlacementName({ names, fallback }: { names: PlacementNames; fallback: string }) {
  const parts = [names.level, names.section, names.edition].filter((p): p is CatalogueLabel => p !== null);
  if (parts.length === 0) return <>{fallback}</>;
  return (
    <>
      {parts.map((p, i) => (
        <span key={i}>
          {i > 0 && ' › '}
          <CatalogueName label={p} />
        </span>
      ))}
    </>
  );
}

type Offer =
  | { state: 'idle' }
  | { state: 'loading' }
  | { state: 'error'; message: string }
  | { state: 'none'; message: string }
  | { state: 'ready'; edition: Edition; assignmentId: number };

export function PlacementSection({
  courses,
  language,
  subjectId,
  levels,
  assignmentIds,
  placements,
  editions,
  onAdd,
  onRemove,
  invalid,
  refused,
  carried,
  editionsFailed,
  onRetryEditions,
}: {
  courses: CourseApi;
  language: Lang;
  subjectId: number;
  levels: Level[];
  assignmentIds: number[];
  placements: Record<number, PlacementInfo>;
  editions: Record<number, Edition>;
  onAdd: (info: PlacementInfo, edition: Edition) => void;
  onRemove: (assignmentId: number) => void;
  /** The last save was refused for its placement. */
  invalid: boolean;
  /** Placements the next save will refuse (not carried, no longer selectable). */
  refused: Set<number>;
  /** Placements unchanged from the course's published version. */
  carried: Set<number>;
  /** Some editions could not be read (names and chapters unknown). */
  editionsFailed: boolean;
  onRetryEditions: () => void;
}) {
  const ids = { level: useId(), section: useId(), heading: useId() };
  const openLevels = levels.filter((l) => !l.retiredAt && l.sections.some((s) => !s.retiredAt));
  const [levelId, setLevelId] = useState<number | null>(null);
  const [sectionId, setSectionId] = useState<number | null>(null);
  const [offer, setOffer] = useState<Offer>({ state: 'idle' });
  const sectionRef = useRef<HTMLSelectElement>(null);
  const level = openLevels.find((l) => l.id === levelId) ?? null;
  const sections = level ? level.sections.filter((s) => !s.retiredAt) : [];

  useEffect(() => {
    if (sectionId === null) {
      setOffer({ state: 'idle' });
      return;
    }
    let cancelled = false;
    setOffer({ state: 'loading' });
    (async () => {
      try {
        const { editions: list } = await courses.sectionEditions(sectionId);
        const current = list.find((e) => e.state === 'current');
        if (!current) {
          if (!cancelled) setOffer({ state: 'none', message: 'Cette section n’a pas d’édition en vigueur : aucun classement ne peut y être choisi pour le moment.' });
          return;
        }
        const edition = await courses.edition(current.id);
        const assignment = edition.assignments?.find((a) => a.subjectId === subjectId);
        if (cancelled) return;
        if (!assignment) setOffer({ state: 'none', message: 'L’édition en vigueur de cette section ne comprend pas la matière du cours.' });
        else if (!assignment.selectable) setOffer({ state: 'none', message: 'Cette édition ne peut pas être choisie : sa section, son niveau ou la matière a été retiré.' });
        else setOffer({ state: 'ready', edition, assignmentId: assignment.id });
      } catch (e) {
        if (!cancelled) setOffer({ state: 'error', message: describeCourseError(e) });
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [courses, sectionId, subjectId]);

  return (
    <section className="panel course-section" aria-labelledby={ids.heading}>
      <h2 id={ids.heading}>Classement dans le programme</h2>
      <p className="hint">
        Choisissez le niveau, la section et l’édition du programme officiel où le cours s’inscrit. Seule l’édition en vigueur d’une section peut être ajoutée ; un cours peut être classé dans plusieurs sections. Le classement est obligatoire pour soumettre le cours, pas pour enregistrer un brouillon.
      </p>
      {assignmentIds.length === 0 ? (
        <p>Aucun classement choisi.</p>
      ) : (
        <ul className="placement-list" aria-label="Classements choisis">
          {assignmentIds.map((id) => {
            const info = placements[id];
            const names = placementNames(info ? editions[info.editionId] : undefined, levels, language);
            return (
              <li key={id} className={refused.has(id) || info?.retired ? 'placement flagged' : info && info.editionState !== 'current' ? 'placement carried' : 'placement'}>
                <p>
                  <PlacementName names={names} fallback={`Classement n° ${id}`} />
                  {info && <span className="hint"> ({EDITION_STATE_LABELS[info.editionState]})</span>}
                </p>
                {refused.has(id) && (
                  <p className="field-error">
                    Le prochain enregistrement sera refusé : cette édition ne peut plus être choisie (elle n’est plus en vigueur, ou sa section, son niveau ou la matière a été retiré). Retirez ce classement, puis ajoutez l’édition en vigueur.
                  </p>
                )}
                {!refused.has(id) && carried.has(id) && info?.retired && (
                  <p className="field-error">Retiré du programme : ce classement, conservé depuis la version publiée du cours, devra être retiré ou remplacé avant la soumission du cours.</p>
                )}
                {!refused.has(id) && carried.has(id) && info && !info.retired && info.editionState === 'superseded' && (
                  <p className="hint">Édition remplacée, conservée depuis la version publiée du cours. Si vous la retirez, elle ne pourra plus être choisie de nouveau.</p>
                )}
                <button type="button" onClick={() => onRemove(id)} aria-label={`Retirer le classement ${names.edition?.text ?? id}`}>
                  Retirer ce classement
                </button>
              </li>
            );
          })}
        </ul>
      )}
      {editionsFailed && (
        <div className="banner error" role="alert">
          <p>Les détails d’une édition du programme n’ont pas pu être lus : son nom et ses chapitres officiels ne sont pas affichés. Les chapitres des modules sont conservés.</p>
          <button type="button" onClick={onRetryEditions}>
            Réessayer
          </button>
        </div>
      )}
      {invalid && <p className="field-error">Le dernier enregistrement a été refusé à cause du classement : vérifiez les classements et les chapitres officiels des modules.</p>}

      <fieldset className="placement-picker">
        <legend>Ajouter un classement</legend>
        {openLevels.length === 0 ? (
          <p>Aucun niveau du programme n’est disponible. Un administrateur doit d’abord créer le programme.</p>
        ) : (
          <>
            <div className="fields">
              <label htmlFor={ids.level}>Niveau</label>
              <select
                id={ids.level}
                value={levelId ?? ''}
                onChange={(e) => {
                  setLevelId(e.target.value ? Number(e.target.value) : null);
                  setSectionId(null);
                }}
              >
                <option value="">Choisir un niveau</option>
                {openLevels.map((l) => {
                  const label = catalogueLabel(l.code, l.nameFr, l.nameAr, language);
                  return (
                    <option key={l.id} value={l.id} {...labelAttrs(label)}>
                      {label.text}
                    </option>
                  );
                })}
              </select>
            </div>
            <div className="fields">
              <label htmlFor={ids.section}>Section</label>
              <select ref={sectionRef} id={ids.section} value={sectionId ?? ''} disabled={!level} onChange={(e) => setSectionId(e.target.value ? Number(e.target.value) : null)}>
                <option value="">{level ? 'Choisir une section' : 'Choisissez d’abord un niveau'}</option>
                {sections.map((s) => {
                  const label = catalogueLabel(s.code, s.nameFr, s.nameAr, language);
                  return (
                    <option key={s.id} value={s.id} {...labelAttrs(label)}>
                      {label.text}
                    </option>
                  );
                })}
              </select>
            </div>
            <div aria-live="polite">
              {offer.state === 'loading' && <p role="status">Recherche de l’édition en vigueur…</p>}
              {offer.state === 'none' && <p>{offer.message}</p>}
              {offer.state === 'error' && (
                <p className="banner error" role="alert">
                  {offer.message}
                </p>
              )}
              {offer.state === 'ready' &&
                (assignmentIds.includes(offer.assignmentId) ? (
                  <p>
                    L’édition en vigueur « <CatalogueName label={catalogueLabel(offer.edition.code, offer.edition.labelFr, offer.edition.labelAr, language)} /> » est déjà choisie.
                  </p>
                ) : (
                  <button
                    type="button"
                    onClick={() => {
                      onAdd({ assignmentId: offer.assignmentId, editionId: offer.edition.id, editionState: 'current', selectable: true, retired: false }, offer.edition);
                      setSectionId(null);
                      // The button disappears with the offer: keep keyboard focus in the picker.
                      sectionRef.current?.focus();
                    }}
                  >
                    Ajouter l’édition en vigueur « {catalogueLabel(offer.edition.code, offer.edition.labelFr, offer.edition.labelAr, language).text} »
                  </button>
                ))}
            </div>
          </>
        )}
      </fieldset>
    </section>
  );
}
