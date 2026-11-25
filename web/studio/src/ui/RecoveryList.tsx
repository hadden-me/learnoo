// The labelled list of unsaved-edit recovery copies (recovery.ts), with
// restore (only where a draft is open), copy and delete.
import { useState } from 'react';
import { recoveryText } from '../drafts';
import { formatDateTime } from '../labels';
import { removeRecovery, type RecoveryEntry } from '../recovery';
import type { Lang } from '../schema/lesson';

export function RecoveryList({
  entries,
  currentRevisionId,
  language,
  onRestore,
  onChanged,
  note,
}: {
  entries: RecoveryEntry[];
  currentRevisionId: number | null;
  language: Lang;
  /** Absent when no draft is open: the copies can then be copied or deleted only. */
  onRestore?: (entry: RecoveryEntry) => void;
  onChanged: () => void;
  note?: string;
}) {
  const [copied, setCopied] = useState<string | null>(null);
  const [fallback, setFallback] = useState<string | null>(null);
  if (entries.length === 0) return null;
  const copy = async (entry: RecoveryEntry) => {
    const text = recoveryText(entry.data, language);
    try {
      await navigator.clipboard.writeText(text);
      setFallback(null);
      setCopied(entry.key);
    } catch {
      // Clipboard refused (permissions, insecure context): show the text to copy by hand.
      setFallback(text);
    }
  };
  return (
    <section className="recovery" aria-labelledby="recovery-heading">
      <h2 id="recovery-heading">Copies de secours locales (non enregistrées)</h2>
      <p className="hint">Ces modifications n’ont jamais été enregistrées sur le serveur. Elles ne sont conservées que dans ce navigateur (14 jours au plus, effacées à la déconnexion), pour éviter de les perdre.</p>
      {note && <p>{note}</p>}
      <ul>
        {entries.map((e) => (
          <li key={e.key}>
            <p>
              Copie du {formatDateTime(e.savedAt)}
              {e.revisionId !== null && e.revisionId !== currentRevisionId
                ? `, faite sur ${e.revisionNumber ? `la révision ${e.revisionNumber}` : 'une autre révision'} (version ${e.baseVersion})`
                : e.baseVersion !== null
                  ? `, faite à partir de la version ${e.baseVersion}`
                  : ''}
              {e.reason === 'conflict' ? ' (gardée lors d’un conflit)' : ''} : « {e.data.title || 'sans titre'} »
            </p>
            <div className="actions">
              {onRestore && (
                <button type="button" onClick={() => onRestore(e)}>
                  Restaurer dans l’éditeur
                </button>
              )}
              <button type="button" onClick={() => void copy(e)}>
                Copier
              </button>
              <button
                type="button"
                onClick={() => {
                  removeRecovery(localStorage, e.key);
                  onChanged();
                }}
              >
                Supprimer cette copie
              </button>
            </div>
            {copied === e.key && <p role="status">Copie placée dans le presse-papiers.</p>}
          </li>
        ))}
      </ul>
      {fallback !== null && (
        <div className="copy-fallback">
          <p className="hint" id="recovery-copy-hint">
            Le navigateur refuse l’accès au presse-papiers. Sélectionnez le texte ci-dessous puis copiez-le.
          </p>
          <textarea readOnly dir="ltr" rows={8} value={fallback} aria-describedby="recovery-copy-hint" aria-label="Texte de la copie de secours" onFocus={(ev) => ev.currentTarget.select()} />
          <button type="button" onClick={() => setFallback(null)}>
            Fermer
          </button>
        </div>
      )}
    </section>
  );
}
