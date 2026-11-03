<script>
  import { TYPE_GROUPS, isNumeric, usesLength, isListType, isTimeType } from './schema.js';

  // Edits one column. Used for the single-column dialog and for each row when creating a table.
  let { column = $bindable(), showPrimary = false } = $props();

  function setBase(base) {
    column.base = base;
    column.length = base === 'varchar' ? '255' : base === 'char' ? '32' : base === 'decimal' ? '10,2' : '';
    if (!isNumeric(base)) {
      column.unsigned = false;
      column.autoIncrement = false;
    }
    if (!isTimeType(base)) {
      column.onUpdate = false;
      if (column.defaultKind === 'current_timestamp') column.defaultKind = '';
    }
  }

  function setAuto(on) {
    column.autoIncrement = on;
    if (on) {
      column.nullable = false;
      column.defaultKind = '';
      if (showPrimary) column.primary = true;
    }
  }

  const known = $derived(TYPE_GROUPS.some(([, types]) => types.includes(column.base)));
</script>

<div class="form-grid">
  <label class="field">Name <input class="mono" bind:value={column.name} required autocomplete="off" spellcheck="false" /></label>
  <label class="field">Type
    <select value={column.base} onchange={(e) => setBase(e.target.value)}>
      {#if !known}<option value={column.base}>{column.base}</option>{/if}
      {#each TYPE_GROUPS as [group, types]}
        <optgroup label={group}>
          {#each types as t}<option value={t}>{t}</option>{/each}
        </optgroup>
      {/each}
    </select>
  </label>
  {#if usesLength(column.base)}
    <label class="field">{isListType(column.base) ? 'Allowed values' : 'Length'}
      <input class="mono" bind:value={column.length} placeholder={isListType(column.base) ? "'a','b','c'" : 'Optional'} required={isListType(column.base)} />
    </label>
  {/if}
  <label class="field">Default
    <select bind:value={column.defaultKind} disabled={column.autoIncrement}>
      <option value="">None</option>
      <option value="null" disabled={!column.nullable}>NULL</option>
      <option value="value">A value...</option>
      {#if isTimeType(column.base)}<option value="current_timestamp">Current time</option>{/if}
    </select>
  </label>
  {#if column.defaultKind === 'value'}
    <label class="field">Default value <input class="mono" bind:value={column.defaultValue} /></label>
  {/if}
</div>
<div class="row small" style="gap:14px;margin-top:10px">
  <label class="row"><input type="checkbox" bind:checked={column.nullable} disabled={column.autoIncrement} /> Allow NULL</label>
  {#if isNumeric(column.base)}
    <label class="row"><input type="checkbox" bind:checked={column.unsigned} /> Unsigned</label>
    {#if column.base !== 'decimal' && column.base !== 'float' && column.base !== 'double'}
      <label class="row"><input type="checkbox" checked={column.autoIncrement} onchange={(e) => setAuto(e.target.checked)} /> Auto increment</label>
    {/if}
  {/if}
  {#if isTimeType(column.base)}
    <label class="row"><input type="checkbox" bind:checked={column.onUpdate} /> Update on every change</label>
  {/if}
  {#if showPrimary}
    <label class="row"><input type="checkbox" bind:checked={column.primary} /> Primary key</label>
  {/if}
</div>
<label class="field" style="margin-top:10px">Comment (optional) <input bind:value={column.comment} /></label>
