import { useSignal } from '@preact/signals';
import { useEffect, useRef } from 'preact/hooks';
import { createPortal } from 'preact/compat';
// Fragment is auto-injected by vite.config.js jsxInject — do NOT import explicitly.
import { getModels, getCombos } from '../api/client';
import {
  parseEntries,
  projectChips,
  addEntry,
  addEntries,
  removeEntry,
  reorderEntries,
  setAllAvailable,
  clearAllAvailable,
  normalizeModels,
  commit,
} from './model-picker-helpers.js';

// ModelPicker — searchable multi-select for the API-key allowed-models field.
//
// Backend matcher contract at internal/server/routes_v1.go:1684-1703 accepts
// comma-separated lists of `*`, `provider/*`, or exact `provider/model-id`.
// This picker emits that exact string format; no schema or backend change.
//
// Selection sources:
//   - Wildcard presets (one per active provider, e.g. "anthropic/*")
//   - Combos from /api/combos (clicking expands per "Both" decision:
//     adds combo NAME and all combo.models to the value)
//   - Individual models from /api/models (active connections only)
//   - "All available models" shortcut emits bare `*`
//
// useSignal discipline per `[339f4d5b3d57…]`: signals are per-mount stable.
// Backdrop click via fixed-position layer with stopPropagation per
// `[41f8388fbc9e…]` modal-close discipline (don't bubble into wizard's
// outer backdrop at key-create-wizard.jsx:210).

export function ModelPicker({ value, onChange, disabled = false }) {
  const models = useSignal([]);
  const combos = useSignal([]);
  const loading = useSignal(true);
  const modelsError = useSignal(null);
  const combosError = useSignal(null);
  const searchQuery = useSignal('');
  const popoverOpen = useSignal(false);
  // Bug 2 fix (cycle 20260516-picker-bugs): popover is portaled to
  // document.body to escape the wizard modal's overflow:auto context at
  // key-create-wizard.jsx:218-224 (and the same shape in key-edit-modal.jsx).
  // anchorRect is computed from the search input's getBoundingClientRect
  // each time popoverOpen flips OR any element scrolls (capture-phase
  // listener catches scrolls on the wizard's inner card too).
  const anchorRect = useSignal(null);
  const inputRef = useRef(null);
  const popoverRef = useRef(null);
  // Minor #1 fold (code-review a1bfc9c22c049b058): chip [✕] clicks fall
  // outside both inputRef and popoverRef, so the document-level mousedown
  // listener used to dismiss the popover when the user removed a chip
  // mid-exploration. chipRowRef is a 3rd "inside picker" anchor.
  const chipRowRef = useRef(null);

  useEffect(() => {
    Promise.allSettled([getModels(), getCombos()]).then(([modelsRes, combosRes]) => {
      if (modelsRes.status === 'fulfilled') {
        // Bug 1 fix (cycle 20260516-picker-bugs): /api/models returns bare
        // array with `provider` field, not OpenAI-compat wrapped shape.
        // normalizeModels bridges the shape; contract pinned in helpers tests.
        models.value = normalizeModels(modelsRes.value);
      } else {
        modelsError.value = modelsRes.reason?.message || 'failed to load models';
      }
      if (combosRes.status === 'fulfilled') {
        combos.value = Array.isArray(combosRes.value) ? combosRes.value : [];
      } else {
        combosError.value = combosRes.reason?.message || 'failed to load combos';
      }
      loading.value = false;
    });
  }, []);

  const bothFailed = modelsError.value && combosError.value;
  const chips = projectChips(value || '', models.value, combos.value);
  const selectedSet = new Set(parseEntries(value || ''));

  function handleAdd(entry) {
    if (disabled) return;
    onChange(addEntry(value || '', entry));
  }

  function handleAddMany(entries) {
    if (disabled) return;
    onChange(addEntries(value || '', entries));
  }

  function handleRemove(entry) {
    if (disabled) return;
    if (entry === '*') {
      onChange(clearAllAvailable(value || ''));
    } else {
      onChange(removeEntry(value || '', entry));
    }
  }

  function handleSetAll() {
    if (disabled) return;
    onChange(setAllAvailable());
    popoverOpen.value = false;
  }

  // ===== Drag-drop reorder (cycle 20260516-routing-strategy-ux M4) =====
  //
  // Native HTML5 drag/drop (zero new deps). Each chip is draggable; the
  // entire chip-row hosts drop targets. State: dragFromIndex (the index of
  // the chip currently being dragged), dropTargetIndex + dropSide (which
  // gap shows the 2px drop indicator). Reorder is performed via the pure
  // reorderEntries helper (single chip) or an inline block-splice for
  // combo blocks per AC-B1..B4. Floor invariant inherited from commit().
  const dragFromIndex = useSignal(null);
  const dropTargetIndex = useSignal(null);
  const dropSide = useSignal(null); // 'before' | 'after'

  // getDragBlock — set-membership combo block detection (AC-B4).
  // For a combo chip at fromIndex, walk forward accumulating consecutive
  // entries whose key is a member of combo.models. Stops at first non-member.
  // Returns [fromIndex] for non-combo chips.
  function getDragBlock(fromIndex) {
    const chip = chips[fromIndex];
    if (!chip || chip.kind !== 'combo' || !chip.members || chip.members.length === 0) {
      return [fromIndex];
    }
    const memberSet = new Set(chip.members);
    const indices = [fromIndex];
    for (let i = fromIndex + 1; i < chips.length; i++) {
      if (memberSet.has(chips[i].key)) {
        indices.push(i);
      } else {
        break; // AC-B3: stops at first non-member (manually-moved member won't drag)
      }
    }
    return indices;
  }

  function handleDragStart(e, fromIndex) {
    if (disabled) return;
    // Avoid drag on the lone-* chip (no reorder possible with 1 chip).
    if (chips.length <= 1) return;
    dragFromIndex.value = fromIndex;
    if (e.dataTransfer) {
      e.dataTransfer.effectAllowed = 'move';
      // Firefox requires non-empty data to initiate drag.
      e.dataTransfer.setData('text/plain', String(fromIndex));
    }
  }

  function handleDragOver(e, toIndex) {
    if (disabled || dragFromIndex.value === null) return;
    e.preventDefault(); // required to allow drop
    if (e.dataTransfer) e.dataTransfer.dropEffect = 'move';
    const rect = e.currentTarget.getBoundingClientRect();
    const mid = rect.left + rect.width / 2;
    const side = e.clientX < mid ? 'before' : 'after';
    if (dropTargetIndex.value !== toIndex || dropSide.value !== side) {
      dropTargetIndex.value = toIndex;
      dropSide.value = side;
    }
  }

  function handleDrop(e, toIndex) {
    if (disabled || dragFromIndex.value === null) return;
    e.preventDefault();
    const from = dragFromIndex.value;
    const side = dropSide.value || 'before';
    const block = getDragBlock(from);
    // Translate 'before' / 'after' the target chip into a target slot index
    // in the entries array (before chip at toIndex = slot toIndex; after = toIndex+1).
    let targetSlot = side === 'after' ? toIndex + 1 : toIndex;

    // No-op cases: dropping into the same span the block already occupies.
    if (block.includes(targetSlot) || block.includes(targetSlot - 1)) {
      handleDragEnd();
      return;
    }

    const entries = parseEntries(value || '');
    // Adjust slot to account for the block being removed first if from < target.
    const blockBeforeTarget = block.filter(i => i < targetSlot).length;
    const adjustedTarget = targetSlot - blockBeforeTarget;
    const blockEntries = block.map(i => entries[i]);
    const remaining = entries.filter((_, i) => !block.includes(i));
    remaining.splice(adjustedTarget, 0, ...blockEntries);
    onChange(commit(value || '', remaining));
    handleDragEnd();
  }

  function handleDragEnd() {
    dragFromIndex.value = null;
    dropTargetIndex.value = null;
    dropSide.value = null;
  }

  // Keyboard reorder (AC-C1..C4). ↑/↓ on a focused chip swaps with neighbor.
  function handleChipKeyDown(e, index) {
    if (disabled) return;
    if (chips.length <= 1) return;
    if (e.key === 'ArrowUp' && index > 0) {
      e.preventDefault();
      e.stopPropagation();
      onChange(reorderEntries(value || '', index, index - 1));
    } else if (e.key === 'ArrowDown' && index < chips.length - 1) {
      e.preventDefault();
      e.stopPropagation();
      onChange(reorderEntries(value || '', index, index + 1));
    }
  }

  // Major #2 fold (prior cycle): ESC closes popover before bubbling to
  // wizard's window-level ESC handler at key-create-wizard.jsx:67-82.
  function handleRootKeyDown(e) {
    if (e.key === 'Escape' && popoverOpen.value) {
      popoverOpen.value = false;
      e.stopPropagation();
    }
  }

  // Bug 2 fix: when popover opens, compute anchor rect from input and
  // listen for scroll/resize to keep popover anchored.
  // Capture-phase ('scroll', ..., true) fires on ALL element scrolls
  // including the wizard's inner card overflow:auto — window-target alone
  // would miss those.
  useEffect(() => {
    if (!popoverOpen.value) return;
    function reposition() {
      if (!inputRef.current) return;
      const r = inputRef.current.getBoundingClientRect();
      anchorRect.value = { top: r.bottom + 4, left: r.left, width: r.width };
    }
    reposition();
    window.addEventListener('scroll', reposition, true);
    window.addEventListener('resize', reposition);
    return () => {
      window.removeEventListener('scroll', reposition, true);
      window.removeEventListener('resize', reposition);
      // Minor #2 fold (code-review a1bfc9c22c049b058): clear stale rect on
      // close so subsequent open recomputes synchronously before render
      // (eliminates one-frame flicker showing prior position).
      anchorRect.value = null;
    };
  }, [popoverOpen.value]);

  // Bug 2 fix: outside-click dismiss via document-level mousedown capture
  // listener — replaces the prior full-viewport backdrop element which
  // (post-portal) would have intercepted ALL wizard clicks. Capture phase
  // fires before bubble-phase handlers on parent elements. We do NOT call
  // stopPropagation — wizard's own bubble-phase outside-click handler at
  // key-create-wizard.jsx:210 (handleClose) still fires for clicks on the
  // wizard's outer backdrop. Two dismissals can co-occur for outside-wizard
  // clicks (picker closes + wizard close-confirm) — predictable per design.
  useEffect(() => {
    if (!popoverOpen.value) return;
    function onMousedown(e) {
      if (popoverRef.current?.contains(e.target)) return;
      if (inputRef.current?.contains(e.target)) return;
      if (chipRowRef.current?.contains(e.target)) return;
      popoverOpen.value = false;
    }
    document.addEventListener('mousedown', onMousedown, { capture: true });
    return () => document.removeEventListener('mousedown', onMousedown, { capture: true });
  }, [popoverOpen.value]);

  // Loading state
  if (loading.value) {
    return <div style={{ fontSize: 12, color: 'var(--text-tertiary)', padding: '8px 0' }}>Loading models…</div>;
  }

  // AC-G6: both endpoints failed → free-text fallback
  if (bothFailed) {
    return (
      <div>
        <div style={{ fontSize: 11, color: 'var(--status-red)', marginBottom: 4 }}>
          Failed to load models. Enter pattern manually:
        </div>
        <input
          type="text"
          placeholder="* = all models, anthropic/*, openai/gpt-4o-mini"
          value={value || ''}
          onInput={e => onChange(commit('', parseEntries(e.target.value)))}
          disabled={disabled}
          style={inputStyle}
        />
      </div>
    );
  }

  return (
    <div style={{ position: 'relative' }} onKeyDown={handleRootKeyDown}>
      {/* Chip row — ref'd so chip [✕] clicks don't trigger outside-click
          dismissal (Minor #1 fold from cycle 20260516-picker-bugs).
          Chips are also drag-drop reorder targets and respond to ↑/↓
          keyboard (cycle 20260516-routing-strategy-ux M4). */}
      <div ref={chipRowRef} style={chipRowStyle}>
        {chips.map((chip, index) => {
          const isDragging = dragFromIndex.value === index;
          const showBefore = dropTargetIndex.value === index && dropSide.value === 'before';
          const showAfter = dropTargetIndex.value === index && dropSide.value === 'after';
          return (
            <Fragment key={chip.key}>
              {showBefore && <DropIndicator />}
              {renderChip(chip, index, {
                onRemove: handleRemove,
                onDragStart: handleDragStart,
                onDragOver: handleDragOver,
                onDrop: handleDrop,
                onDragEnd: handleDragEnd,
                onKeyDown: handleChipKeyDown,
              }, disabled, isDragging)}
              {showAfter && <DropIndicator />}
            </Fragment>
          );
        })}
      </div>

      {/* Search input */}
      <input
        ref={inputRef}
        type="text"
        placeholder="Search models (e.g. claude, gpt-4)…"
        value={searchQuery.value}
        onInput={e => { searchQuery.value = e.target.value; }}
        onFocus={() => { if (!disabled) popoverOpen.value = true; }}
        disabled={disabled}
        style={inputStyle}
      />

      {/* Popover — portaled to document.body to escape wizard modal's
          overflow:auto context. zIndex 9050 sits above wizard backdrop
          (9000) and below confirm overlays (9100). Outside-click dismiss
          via document-level mousedown listener (useEffect above) — NOT
          a backdrop element, which would intercept all wizard clicks. */}
      {popoverOpen.value && anchorRect.value && createPortal(
        <div
          ref={popoverRef}
          onClick={e => e.stopPropagation()}
          style={{
            ...popoverStyle,
            position: 'fixed',
            top: anchorRect.value.top,
            left: anchorRect.value.left,
            width: anchorRect.value.width,
            zIndex: 9050,
          }}
        >
          {renderPopoverSections({
            models: models.value,
            combos: combos.value,
            modelsError: modelsError.value,
            combosError: combosError.value,
            searchQuery: searchQuery.value,
            selectedSet,
            onSelectAll: handleSetAll,
            onAddEntry: handleAdd,
            onAddEntries: handleAddMany,
          })}
        </div>,
        document.body,
      )}
    </div>
  );
}

// ===== Chip rendering =====

// DropIndicator — 2px vertical bar between chips showing where a dragged
// chip will land. Cycle 20260516-routing-strategy-ux M4 q3.
function DropIndicator() {
  return (
    <span
      aria-hidden
      style={{
        display: 'inline-block',
        width: 2,
        height: 18,
        background: 'var(--accent, #3b82f6)',
        alignSelf: 'center',
        margin: '0 2px',
        borderRadius: 1,
      }}
    />
  );
}

// renderChip — drag-aware + keyboard-reorderable. The `index` is the chip's
// position in the chips array (= parseEntries(value) index for non-empty/
// non-lone-* states), used by drag handlers to compute splice positions.
// AC-A1: draggable={!disabled}. AC-A2: opacity 0.5 while being dragged.
// AC-C1..C4: tabIndex={0} + onKeyDown for ↑/↓.
function renderChip(chip, index, handlers, disabled, isDragging) {
  const baseChipStyle = {
    display: 'inline-flex',
    alignItems: 'center',
    gap: 6,
    padding: '3px 8px',
    borderRadius: 'var(--radius-sm)',
    fontSize: 12,
    fontFamily: chip.kind === 'all' ? 'inherit' : 'var(--font-mono)',
    border: '1px solid var(--border)',
    background: chip.kind === 'all' ? 'var(--accent-muted)' : 'var(--bg-2)',
    color: 'var(--text-primary)',
    opacity: isDragging ? 0.4 : (chip.dangling ? 0.5 : 1),
    cursor: disabled || chip.kind === 'all' ? 'default' : 'grab',
  };
  const tooltip = chip.dangling
    ? (chip.kind === 'wildcard'
        ? `${chip.label}: provider not currently active`
        : chip.kind === 'combo'
          ? `Combo's underlying models not currently active`
          : `Not available in active connections`)
    : (chip.kind === 'combo' && chip.members ? `Members: ${chip.members.join(', ')}` : undefined);

  // The lone 'all' chip (single * representation) is not reorderable —
  // there's nothing to reorder with. Disable drag and keyboard nav on it.
  const isReorderable = !disabled && chip.kind !== 'all';

  return (
    <span
      style={baseChipStyle}
      title={tooltip}
      draggable={isReorderable}
      tabIndex={isReorderable ? 0 : -1}
      onDragStart={isReorderable ? e => handlers.onDragStart(e, index) : undefined}
      onDragOver={isReorderable ? e => handlers.onDragOver(e, index) : undefined}
      onDrop={isReorderable ? e => handlers.onDrop(e, index) : undefined}
      onDragEnd={isReorderable ? handlers.onDragEnd : undefined}
      onKeyDown={isReorderable ? e => handlers.onKeyDown(e, index) : undefined}
    >
      {chip.kind === 'combo' && <span style={{ fontSize: 10, color: 'var(--text-tertiary)' }}>◇</span>}
      {chip.label}
      {!disabled && (
        <button
          onClick={() => handlers.onRemove(chip.key)}
          tabIndex={0}
          aria-label={`Remove ${chip.label}`}
          style={{
            background: 'none',
            border: 'none',
            color: 'var(--text-tertiary)',
            cursor: 'pointer',
            padding: 0,
            fontSize: 13,
            lineHeight: 1,
            marginLeft: 2,
          }}
        >
          ✕
        </button>
      )}
    </span>
  );
}

// ===== Popover sections =====

function renderPopoverSections({ models, combos, modelsError, combosError, searchQuery, selectedSet, onSelectAll, onAddEntry, onAddEntries }) {
  const allSelected = selectedSet.has('*');
  const activeProviders = [...new Set(models.map(m => m.owned_by))].sort();
  const q = searchQuery.toLowerCase().trim();
  const filteredModels = q
    ? models.filter(m => m.id.toLowerCase().includes(q))
    : models;
  const modelsByProvider = groupBy(filteredModels, 'owned_by');
  // Bug 3 fix (cycle 20260516-picker-bugs): combos previously rendered
  // unfiltered while models filtered by searchQuery — confusing UX. Match
  // combo by name OR any member-model id so the single search input
  // filters everything. Member-match means typing a model name surfaces
  // combos that include that model — useful UX bonus.
  const filteredCombos = q
    ? combos.filter(c =>
        c.name.toLowerCase().includes(q) ||
        (c.models || []).some(id => id.toLowerCase().includes(q))
      )
    : combos;

  // AC-D4: empty popover when no data
  if (
    !modelsError &&
    !combosError &&
    models.length === 0 &&
    combos.length === 0
  ) {
    return (
      <>
        {renderRow({
          label: 'All available models',
          isDisabled: allSelected,
          handler: onSelectAll,
        })}
        <div style={emptyStateStyle}>
          No active providers — connect one on the Connections page.
        </div>
      </>
    );
  }

  return (
    <>
      {/* Section 1: Quick selects */}
      {renderRow({
        label: 'All available models',
        isDisabled: allSelected,
        handler: onSelectAll,
        sectionHint: 'Quick select',
      })}

      {/* Section 2: Wildcard presets (hidden if modelsError) */}
      {!modelsError && activeProviders.length > 0 && (
        <>
          <div style={sectionDividerStyle}>Provider wildcards</div>
          {activeProviders.map(p => {
            const entry = `${p}/*`;
            return renderRow({
              key: entry,
              label: `${displayNameInline(p)} — all models`,
              hint: entry,
              isDisabled: selectedSet.has(entry),
              handler: () => onAddEntry(entry),
            });
          })}
        </>
      )}
      {modelsError && (
        <div style={errorRowStyle}>Models failed to load: {modelsError}</div>
      )}

      {/* Section 3: Combos — Bug 3 fix uses filteredCombos. Empty-state
          renders when section HAD content (combos.length > 0) but query
          filtered to zero — mirrors Models section pattern. */}
      {!combosError && combos.length > 0 && (
        <>
          <div style={sectionDividerStyle}>Combos</div>
          {filteredCombos.map(c => {
            const memberPreview = (c.models || []).slice(0, 3).join(', ') + ((c.models || []).length > 3 ? '…' : '');
            return renderRow({
              key: c.name,
              label: c.name,
              hint: `→ ${memberPreview}`,
              isDisabled: selectedSet.has(c.name),
              handler: () => onAddEntries([c.name, ...(c.models || [])]),
            });
          })}
          {filteredCombos.length === 0 && q && (
            <div style={emptyStateStyle}>No matching combos for "{searchQuery}".</div>
          )}
        </>
      )}
      {combosError && (
        <div style={errorRowStyle}>Combos failed to load: {combosError}</div>
      )}

      {/* Section 4: Models grouped by provider */}
      {!modelsError && (
        <>
          <div style={sectionDividerStyle}>Models</div>
          {Object.keys(modelsByProvider).sort().map(provider => (
            <div key={provider}>
              <div style={providerHeaderStyle}>{displayNameInline(provider)}</div>
              {modelsByProvider[provider].map(m => renderRow({
                key: m.id,
                label: m.id,
                isDisabled: selectedSet.has(m.id),
                handler: () => onAddEntry(m.id),
              }))}
            </div>
          ))}
          {filteredModels.length === 0 && q && (
            <div style={emptyStateStyle}>No matches for “{searchQuery}”.</div>
          )}
        </>
      )}
    </>
  );
}

// renderRow — clickable popover row with full keyboard a11y per AC-G8 (m9 fold).
function renderRow({ key, label, hint, isDisabled, handler, sectionHint }) {
  return (
    <div
      key={key || label}
      role="button"
      tabIndex={isDisabled ? -1 : 0}
      aria-disabled={isDisabled}
      onClick={() => { if (!isDisabled) handler(); }}
      onKeyDown={e => {
        if ((e.key === 'Enter' || e.key === ' ') && !isDisabled) {
          e.preventDefault();
          handler();
        }
      }}
      style={{
        padding: '6px 10px',
        fontSize: 12,
        color: isDisabled ? 'var(--text-tertiary)' : 'var(--text-primary)',
        cursor: isDisabled ? 'not-allowed' : 'pointer',
        opacity: isDisabled ? 0.5 : 1,
        display: 'flex',
        justifyContent: 'space-between',
        alignItems: 'center',
        gap: 8,
        outline: 'none',
      }}
      onMouseEnter={e => { if (!isDisabled) e.currentTarget.style.background = 'var(--bg-2)'; }}
      onMouseLeave={e => { e.currentTarget.style.background = 'transparent'; }}
    >
      <span>{label}</span>
      {hint && <span style={{ fontSize: 10, color: 'var(--text-tertiary)', fontFamily: 'var(--font-mono)' }}>{hint}</span>}
      {sectionHint && !hint && <span style={{ fontSize: 10, color: 'var(--text-tertiary)' }}>{sectionHint}</span>}
    </div>
  );
}

// ===== Helpers (inline; not in helpers.js since they touch render-time data) =====

function groupBy(arr, key) {
  const out = {};
  for (const item of arr) {
    const k = item[key] || 'unknown';
    (out[k] ||= []).push(item);
  }
  return out;
}

function displayNameInline(provider) {
  // Duplicate of helpers' displayName — render context needs it inline.
  return { anthropic: 'Anthropic', openai: 'OpenAI', gemini: 'Gemini', openrouter: 'OpenRouter', 'github-copilot': 'GitHub Copilot', ollama: 'Ollama' }[provider] || provider;
}

// ===== Styles =====

const inputStyle = {
  width: '100%',
  padding: '8px 10px',
  background: 'var(--bg-2)',
  border: '1px solid var(--border)',
  borderRadius: 'var(--radius-md)',
  color: 'var(--text-primary)',
  fontSize: 13,
  boxSizing: 'border-box',
};

const chipRowStyle = {
  display: 'flex',
  flexWrap: 'wrap',
  gap: 4,
  marginBottom: 6,
  minHeight: 24,
};

// popoverStyle holds visual-only props. Positioning (top, left, width)
// is set inline at render time from anchorRect; zIndex set inline (9050).
// Bug 2 fix (cycle 20260516-picker-bugs).
const popoverStyle = {
  background: 'var(--bg-1)',
  border: '1px solid var(--border)',
  borderRadius: 'var(--radius-md)',
  boxShadow: '0 4px 12px rgba(0,0,0,0.3)',
  maxHeight: 320,
  overflowY: 'auto',
};

const sectionDividerStyle = {
  padding: '8px 10px 4px',
  fontSize: 10,
  fontWeight: 600,
  color: 'var(--text-tertiary)',
  textTransform: 'uppercase',
  letterSpacing: 0.5,
  borderTop: '1px solid var(--border)',
  marginTop: 4,
  background: 'var(--bg-2)',
};

const providerHeaderStyle = {
  padding: '4px 10px',
  fontSize: 10,
  fontWeight: 600,
  color: 'var(--text-secondary)',
  background: 'rgba(255,255,255,0.02)',
};

const emptyStateStyle = {
  padding: '12px 10px',
  fontSize: 11,
  color: 'var(--text-tertiary)',
  fontStyle: 'italic',
};

const errorRowStyle = {
  padding: '8px 10px',
  fontSize: 11,
  color: 'var(--status-red)',
};
