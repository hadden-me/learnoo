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
import { t } from '../i18n';
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
          if (!cancelled) setOffer({ state: 'none', message: t('courses.placement.noCurrent') });
          return;
        }
        const edition = await courses.edition(current.id);
        const assignment = edition.assignments?.find((a) => a.subjectId === subjectId);
        if (cancelled) return;
        if (!assignment) setOffer({ state: 'none', message: t('courses.placement.noSubject') });
        else if (!assignment.selectable) setOffer({ state: 'none', message: t('courses.placement.notSelectable') });
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
      <h2 id={ids.heading}>{t('courses.placement.heading')}</h2>
      <p className="hint">{t('courses.placement.hint')}</p>
      {assignmentIds.length === 0 ? (
        <p>{t('courses.placement.none')}</p>
      ) : (
        <ul className="placement-list" aria-label={t('courses.placement.list')}>
          {assignmentIds.map((id) => {
            const info = placements[id];
            const names = placementNames(info ? editions[info.editionId] : undefined, levels, language);
            return (
              <li key={id} className={refused.has(id) || info?.retired ? 'placement flagged' : info && info.editionState !== 'current' ? 'placement carried' : 'placement'}>
                <p>
                  <PlacementName names={names} fallback={t('courses.placement.number', { id })} />
                  {info && <span className="hint"> ({EDITION_STATE_LABELS[info.editionState]})</span>}
                </p>
                {refused.has(id) && (
                  <p className="field-error">
                    {t('courses.placement.refused')}
                  </p>
                )}
                {!refused.has(id) && carried.has(id) && info?.retired && (
                  <p className="field-error">{t('courses.placement.retiredCarried')}</p>
                )}
                {!refused.has(id) && carried.has(id) && info && !info.retired && info.editionState === 'superseded' && (
                  <p className="hint">{t('courses.placement.supersededCarried')}</p>
                )}
                <button type="button" onClick={() => onRemove(id)} aria-label={t('courses.placement.removeLabel', { name: names.edition?.text ?? id })}>
                  {t('courses.placement.remove')}
                </button>
              </li>
            );
          })}
        </ul>
      )}
      {editionsFailed && (
        <div className="banner error" role="alert">
          <p>{t('courses.placement.editionsFailed')}</p>
          <button type="button" onClick={onRetryEditions}>
            {t('shell.retry')}
          </button>
        </div>
      )}
      {invalid && <p className="field-error">{t('courses.placement.invalid')}</p>}

      <fieldset className="placement-picker">
        <legend>{t('courses.placement.add')}</legend>
        {openLevels.length === 0 ? (
          <p>{t('courses.placement.noLevels')}</p>
        ) : (
          <>
            <div className="fields">
              <label htmlFor={ids.level}>{t('courses.placement.level')}</label>
              <select
                id={ids.level}
                value={levelId ?? ''}
                onChange={(e) => {
                  setLevelId(e.target.value ? Number(e.target.value) : null);
                  setSectionId(null);
                }}
              >
                <option value="">{t('courses.placement.chooseLevel')}</option>
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
              <label htmlFor={ids.section}>{t('courses.placement.section')}</label>
              <select ref={sectionRef} id={ids.section} value={sectionId ?? ''} disabled={!level} onChange={(e) => setSectionId(e.target.value ? Number(e.target.value) : null)}>
                <option value="">{level ? t('courses.placement.chooseSection') : t('courses.placement.levelFirst')}</option>
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
              {offer.state === 'loading' && <p role="status">{t('courses.placement.searching')}</p>}
              {offer.state === 'none' && <p>{offer.message}</p>}
              {offer.state === 'error' && (
                <p className="banner error" role="alert">
                  {offer.message}
                </p>
              )}
              {offer.state === 'ready' &&
                (assignmentIds.includes(offer.assignmentId) ? (
                  <p>
                    {t('courses.placement.alreadyBefore')}
                    <CatalogueName label={catalogueLabel(offer.edition.code, offer.edition.labelFr, offer.edition.labelAr, language)} />
                    {t('courses.placement.alreadyAfter')}
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
                    {t('courses.placement.addCurrent', { name: catalogueLabel(offer.edition.code, offer.edition.labelFr, offer.edition.labelAr, language).text })}
                  </button>
                ))}
            </div>
          </>
        )}
      </fieldset>
    </section>
  );
}
