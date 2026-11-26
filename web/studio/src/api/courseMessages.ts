// French explanations for the course draft codes (docs/API.md "Course
// drafts"). Codes shared with lessons fall back to describeError
// (messages.ts). For refused input the server's own message (English, it
// names the field, module or step) is added as a detail, as messages.ts does
// for the lesson validator.
import { ApiError } from './client';
import { describeError } from './messages';

const MESSAGES: Record<string, string> = {
  not_found: 'Introuvable : ce cours ou ce brouillon n’existe pas ou ne vous appartient pas.',
  version_conflict: 'Ce brouillon de cours a été modifié ailleurs (autre onglet ou appareil) depuis son ouverture. Rien n’a été écrasé.',
  not_draft: 'Cette version du cours a déjà été soumise : elle ne peut plus être modifiée.',
  active_candidate_exists:
    'Le cours a déjà une version en cours : un brouillon, une version en relecture ou une version approuvée qui attend sa publication. Un nouveau brouillon ne pourra être créé qu’après sa publication ou son retrait.',
  archived: 'Ce cours est archivé.',
  has_history: 'Ce cours a déjà été soumis au moins une fois : il est conservé avec son historique et ne peut pas être supprimé. Vous pouvez supprimer son brouillon en cours.',
  invalid_input: 'Un champ du cours est refusé (longueur, caractère non autorisé, ou image de couverture sans texte alternatif). Rien n’a été enregistré.',
  placement_invalid:
    'Classement refusé : une édition choisie ne peut plus être sélectionnée (elle n’est plus en vigueur, ou sa section, son niveau ou la matière a été retiré), ou le chapitre officiel d’un module n’appartient à aucune édition choisie. Rien n’a été enregistré.',
  invalid_step:
    'Une étape ne peut pas être rattachée : leçon inconnue, leçon d’un autre auteur qui n’est plus publiée ou révision non publiable, autre matière ou langue, ou leçon présente deux fois dans le cours. Rien n’a été enregistré.',
  unknown_media: 'L’image de couverture est introuvable ou ne vous appartient pas. Téléversez-la de nouveau.',
  unknown_subject: 'Matière inconnue.',
};

const DETAILED = new Set(['invalid_input', 'placement_invalid', 'invalid_step']);

/** A French sentence for a course failure. */
export function describeCourseError(error: unknown): string {
  if (!(error instanceof ApiError) || !(error.code in MESSAGES)) return describeError(error);
  const base = MESSAGES[error.code];
  return DETAILED.has(error.code) && error.message ? `${base} Détail du serveur : ${error.message}` : base;
}
