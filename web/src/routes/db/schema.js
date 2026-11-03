// Shared by the table creation and structure forms.

export const TYPE_GROUPS = [
  ['Numbers', ['int', 'bigint', 'smallint', 'tinyint', 'mediumint', 'decimal', 'float', 'double', 'bit']],
  ['Text', ['varchar', 'char', 'text', 'tinytext', 'mediumtext', 'longtext', 'json', 'enum', 'set']],
  ['Date and time', ['datetime', 'timestamp', 'date', 'time', 'year']],
  ['Binary', ['blob', 'tinyblob', 'mediumblob', 'longblob', 'binary', 'varbinary']],
];

const NUMERIC = new Set(['int', 'bigint', 'smallint', 'tinyint', 'mediumint', 'decimal', 'float', 'double']);
const NEEDS_LENGTH = { varchar: '255', char: '32', binary: '16', varbinary: '255', decimal: '10,2', bit: '1' };
const NO_LENGTH = new Set([
  'text', 'tinytext', 'mediumtext', 'longtext', 'json', 'blob', 'tinyblob', 'mediumblob', 'longblob', 'date', 'year', 'float', 'double',
]);

export const isNumeric = (base) => NUMERIC.has(base);
export const usesLength = (base) => !NO_LENGTH.has(base);
export const isListType = (base) => base === 'enum' || base === 'set';
export const isTimeType = (base) => base === 'timestamp' || base === 'datetime';

// Splits a column type like "int(11) unsigned" or "enum('a','b')" for editing.
export function parseType(type) {
  const m = /^(\w+)(?:\s*\((.*)\))?(.*)$/s.exec(type || 'int');
  const base = m[1].toLowerCase();
  return { base, length: m[2] ?? '', unsigned: /unsigned/i.test(m[3] || '') };
}

export function composeType({ base, length, unsigned }) {
  let out = base;
  const size = String(length ?? '').trim() || NEEDS_LENGTH[base] || '';
  if (size && usesLength(base)) out += `(${size})`;
  if (unsigned && isNumeric(base)) out += ' unsigned';
  return out;
}

// A blank column for the forms.
export function newColumn(overrides = {}) {
  return {
    name: '', base: 'varchar', length: '255', unsigned: false, nullable: true,
    defaultKind: '', defaultValue: '', onUpdate: false, autoIncrement: false, comment: '', primary: false,
    ...overrides,
  };
}

// From the server's description of an existing column to the form's fields.
export function columnToForm(c) {
  return {
    ...newColumn(),
    ...parseType(c.type),
    name: c.name,
    nullable: c.nullable,
    defaultKind: c.defaultKind,
    defaultValue: c.defaultValue,
    onUpdate: /on update/i.test(c.extra || ''),
    autoIncrement: /auto_increment/i.test(c.extra || ''),
    comment: c.comment || '',
  };
}

// From the form's fields to what the server expects.
export function formToColumn(f) {
  return {
    name: f.name.trim(),
    type: composeType(f),
    nullable: f.nullable,
    defaultKind: f.defaultKind,
    defaultValue: f.defaultValue,
    onUpdate: f.onUpdate,
    autoIncrement: f.autoIncrement,
    comment: f.comment,
    primary: f.primary,
  };
}

export function defaultLabel(c) {
  if (c.defaultKind === 'value') return c.defaultValue === '' ? "''" : c.defaultValue;
  if (c.defaultKind === 'null') return 'NULL';
  if (c.defaultKind === 'current_timestamp') return 'CURRENT_TIMESTAMP';
  return '';
}
