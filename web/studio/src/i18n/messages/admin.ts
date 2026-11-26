// Studio messages: admin. Keys start with "admin.".
import type { ArTable, FrTable } from '../core';

export const fr = {} as const satisfies FrTable;

export const ar: ArTable<typeof fr> = {};
