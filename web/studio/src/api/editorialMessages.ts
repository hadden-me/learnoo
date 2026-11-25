// French explanations for the review, publication, administration and
// error-report codes (docs/API.md). Codes shared with authoring fall back to
// describeError (messages.ts). Each says what happened and that nothing was
// changed when the server guarantees it.
import { ApiError } from './client';
import { describeError } from './messages';

const MESSAGES: Record<string, string> = {
  forbidden: 'Votre compte n’a pas la permission nécessaire pour cette action. Le serveur l’a refusée ; rien n’a été modifié.',
  not_found: 'Introuvable : cet élément n’existe pas, n’est plus disponible ici (par exemple une révision qui n’est plus en relecture) ou ne relève pas de vos matières.',
  self_review: 'Vous êtes l’auteur de cette révision : vous ne pouvez ni l’approuver ni demander des modifications. Un autre relecteur de la matière doit la relire.',
  not_in_review: 'Cette révision n’est plus en relecture : une décision a déjà été enregistrée (peut-être par un autre relecteur). Rien n’a été écrit.',
  comment_required: 'Expliquez les modifications demandées dans un commentaire : il est obligatoire.',
  invalid_comment: 'Commentaire refusé : 5 000 caractères au maximum, sans caractère nul.',
  not_approved: 'Seule une révision approuvée peut être publiée ou retirée.',
  already_published: 'Cette révision a déjà été publiée. Pour remettre en ligne une version antérieure, utilisez la restauration.',
  stale_base:
    'Publication refusée : la version publiée de la leçon a changé depuis la rédaction de cette révision (dépublication ou restauration). Rien n’a été publié. Une révision réconciliée doit être rédigée et relue ; en attendant, un administrateur peut retirer celle-ci.',
  withdrawn: 'Cette révision a été retirée : elle ne peut plus jamais être publiée ni restaurée.',
  already_withdrawn: 'Cette révision est déjà retirée.',
  stale_pointer:
    'La publication de cette leçon a changé depuis l’ouverture de la page (action d’une autre personne ou d’un autre onglet). Rien n’a été modifié. Rechargez la page pour voir l’état actuel avant de recommencer.',
  not_published: 'Cette leçon n’a pas de version publiée : il n’y a rien à dépublier.',
  never_published: 'Cette révision n’a jamais été publiée : elle ne peut pas être restaurée. Une révision approuvée jamais publiée passe par la publication.',
  already_current: 'Cette révision est déjà la version publiée.',
  ineligible: 'Restauration impossible : le contenu de cette révision ne respecte plus le format de leçon actuel ou une image jointe est introuvable.',
  reason_required: 'Indiquez un motif : il est obligatoire et sera conservé dans l’historique.',
  invalid_reason: 'Motif refusé : 2 000 caractères au maximum, en texte valide.',
  not_open: 'Ce signalement a déjà été traité (peut-être par un autre relecteur). Rien n’a été écrit.',
  note_required: 'Rédigez une note de traitement : elle est obligatoire.',
  invalid_note: 'Note refusée : 2 000 caractères au maximum, sans caractère de contrôle (hors retours à la ligne) ni caractère de mise en forme bidirectionnelle.',
  immutable: 'Modification refusée par une protection de la base de données. Rien n’a été écrit ; rechargez la page.',
  archived: 'Cette leçon est archivée : elle ne peut plus être publiée.',
};

/** A French sentence for an editorial failure. */
export function describeEditorialError(error: unknown): string {
  if (error instanceof ApiError && error.code in MESSAGES) return MESSAGES[error.code];
  return describeError(error);
}
