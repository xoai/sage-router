import { signal, computed, useSignal } from '@preact/signals';
import { useEffect, useRef } from 'preact/hooks';

// KeyFacets — faceted search bar for the /keys page.
//
// Composes:
//   - Search text input (debounced 300ms)
//   - Routing strategy <select>
//   - Budget status <select>
//   - Active filter chips with ✕ to remove
//   - "Save preset" button + preset dropdown
//
// State management:
//   - Parent owns the filter signal; this component is controlled.
//   - URL state sync: on mount, read window.location.search; on change,
//     history.replaceState (not pushState — back-button leaves /keys
//     entirely rather than undoing filters, matches the in-page filter
//     mental model). Spec AC-B7 + plan m6.
//
// Preset storage:
//   - localStorage key 'sage-router:keys-filter-presets:v1'
//   - Each preset: {name, search, routing, has_budget}
//   - Max 10 presets; LRU pop on new save
//
// AC-B1..B7 of 20260516-keys-management-redesign.

const PRESETS_KEY = 'sage-router:keys-filter-presets:v1';
const MAX_PRESETS = 10;
const SEARCH_DEBOUNCE_MS = 300;

function loadPresets() {
  try {
    const raw = localStorage.getItem(PRESETS_KEY);
    if (!raw) return [];
    const parsed = JSON.parse(raw);
    if (!Array.isArray(parsed)) return [];
    return parsed;
  } catch {
    return [];
  }
}

function savePresets(presets) {
  try {
    localStorage.setItem(PRESETS_KEY, JSON.stringify(presets.slice(0, MAX_PRESETS)));
  } catch {
    // Quota / disabled storage — silently no-op. Presets aren't load-bearing.
  }
}

// presetsList lives at module scope so all instances share state. The
// /keys page typically only mounts one instance; module-level avoids
// re-reading localStorage on every render.
const presetsList = signal(loadPresets());

// Build query string from filter, omitting empty/false/null fields.
function filterToQS(filter) {
  const params = new URLSearchParams();
  if (filter.search) params.set('q', filter.search);
  if (filter.routing) params.set('routing', filter.routing);
  if (filter.has_budget != null) params.set('has_budget', String(filter.has_budget));
  return params.toString();
}

function qsToFilter(searchString) {
  const params = new URLSearchParams(searchString);
  const filter = { search: '', routing: '', has_budget: null };
  if (params.get('q')) filter.search = params.get('q');
  if (params.get('routing')) filter.routing = params.get('routing');
  const hb = params.get('has_budget');
  if (hb === 'true') filter.has_budget = true;
  else if (hb === 'false') filter.has_budget = false;
  return filter;
}

// Read URL filter on mount; call before parent's initial fetch.
export function readURLFilter() {
  return qsToFilter(window.location.search);
}

// Update URL silently (no history entry).
function syncURL(filter) {
  const qs = filterToQS(filter);
  const url = window.location.pathname + (qs ? '?' + qs : '');
  history.replaceState({}, '', url);
}

export function KeyFacets({ value, onChange }) {
  const filter = value;
  const showPresetDropdown = useSignal(false);

  // Debounced search input — keeps a local signal for fast typing
  // feedback, debounces the parent's onChange.
  const searchLocal = useSignal(filter.search);
  useEffect(() => { searchLocal.value = filter.search; }, [filter.search]);

  // useRef holds the timer handle across renders — a `let` inside the
  // body would re-init to null each render, leaving the previous
  // setTimeout uncancellable and firing one server fetch per keystroke.
  const searchTimerRef = useRef(null);
  const onSearchInput = (e) => {
    searchLocal.value = e.target.value;
    if (searchTimerRef.current) clearTimeout(searchTimerRef.current);
    searchTimerRef.current = setTimeout(() => {
      onChange({ ...filter, search: searchLocal.value });
    }, SEARCH_DEBOUNCE_MS);
  };

  // Sync URL when filter changes.
  useEffect(() => {
    syncURL(filter);
  }, [filter.search, filter.routing, filter.has_budget]);

  const hasAnyFacet = computed(() =>
    filter.search || filter.routing || filter.has_budget != null
  );

  const chips = [];
  if (filter.routing) chips.push({ key: 'routing', label: `Routing: ${filter.routing}` });
  if (filter.has_budget === true) chips.push({ key: 'has_budget', label: 'Budget: capped' });
  if (filter.has_budget === false) chips.push({ key: 'has_budget', label: 'Budget: uncapped' });

  const removeChip = (key) => {
    if (key === 'routing') onChange({ ...filter, routing: '' });
    if (key === 'has_budget') onChange({ ...filter, has_budget: null });
  };

  const handleSavePreset = () => {
    const name = prompt('Preset name?');
    if (!name?.trim()) return;
    const preset = {
      name: name.trim(),
      search: filter.search,
      routing: filter.routing,
      has_budget: filter.has_budget,
    };
    // LRU: most-recent first; cap at MAX_PRESETS.
    const next = [preset, ...presetsList.value.filter(p => p.name !== preset.name)].slice(0, MAX_PRESETS);
    presetsList.value = next;
    savePresets(next);
  };

  const handleApplyPreset = (preset) => {
    // REPLACE current facet state with the preset's full snapshot
    // (spec-review m1: not merge).
    onChange({
      search: preset.search || '',
      routing: preset.routing || '',
      has_budget: preset.has_budget ?? null,
    });
    showPresetDropdown.value = false;
  };

  const handleDeletePreset = (name) => {
    const next = presetsList.value.filter(p => p.name !== name);
    presetsList.value = next;
    savePresets(next);
  };

  return (
    <div style={{
      background: 'var(--bg-1)',
      border: '1px solid var(--border)',
      borderRadius: 'var(--radius-lg)',
      padding: 'var(--space-md)',
      marginBottom: 'var(--space-lg)',
    }}>
      {/* Row 1: search + filter dropdowns */}
      <div style={{ display: 'flex', gap: 'var(--space-sm)', alignItems: 'center', flexWrap: 'wrap' }}>
        <input
          type="text"
          placeholder="Search by name..."
          value={searchLocal.value}
          onInput={onSearchInput}
          style={{
            flex: '1 1 220px',
            padding: '6px 10px',
            background: 'var(--bg-2)',
            border: '1px solid var(--border)',
            borderRadius: 'var(--radius-md)',
            color: 'var(--text-primary)',
            fontSize: 13,
          }}
        />
        <select
          value={filter.routing}
          onChange={e => onChange({ ...filter, routing: e.target.value })}
          style={{
            padding: '6px 10px',
            background: 'var(--bg-2)',
            border: '1px solid var(--border)',
            borderRadius: 'var(--radius-md)',
            color: 'var(--text-primary)',
            fontSize: 12,
          }}
        >
          <option value="">Routing: all</option>
          <option value="default">Default</option>
          <option value="fast">fast</option>
          <option value="balanced">balanced</option>
          <option value="cheap">cheap</option>
          <option value="best">best</option>
        </select>
        <select
          value={filter.has_budget == null ? '' : String(filter.has_budget)}
          onChange={e => {
            const v = e.target.value;
            onChange({ ...filter, has_budget: v === '' ? null : v === 'true' });
          }}
          style={{
            padding: '6px 10px',
            background: 'var(--bg-2)',
            border: '1px solid var(--border)',
            borderRadius: 'var(--radius-md)',
            color: 'var(--text-primary)',
            fontSize: 12,
          }}
        >
          <option value="">Budget: all</option>
          <option value="true">Has cap</option>
          <option value="false">No cap</option>
        </select>

        {/* Save preset button — only when any facet is active */}
        {hasAnyFacet.value && (
          <button
            onClick={handleSavePreset}
            style={{
              padding: '6px 10px',
              background: 'var(--bg-2)',
              border: '1px solid var(--border)',
              borderRadius: 'var(--radius-md)',
              color: 'var(--text-secondary)',
              fontSize: 12,
              cursor: 'pointer',
            }}
          >
            Save preset
          </button>
        )}

        {/* Preset dropdown — only when presets exist */}
        {presetsList.value.length > 0 && (
          <div style={{ position: 'relative' }}>
            <button
              onClick={() => { showPresetDropdown.value = !showPresetDropdown.value; }}
              style={{
                padding: '6px 10px',
                background: 'var(--bg-2)',
                border: '1px solid var(--border)',
                borderRadius: 'var(--radius-md)',
                color: 'var(--text-secondary)',
                fontSize: 12,
                cursor: 'pointer',
              }}
            >
              Presets ▾
            </button>
            {showPresetDropdown.value && (
              <div style={{
                position: 'absolute',
                top: '100%',
                right: 0,
                marginTop: 4,
                minWidth: 200,
                background: 'var(--bg-1)',
                border: '1px solid var(--border)',
                borderRadius: 'var(--radius-md)',
                boxShadow: '0 4px 12px rgba(0,0,0,0.3)',
                zIndex: 10,
              }}>
                {presetsList.value.map(p => (
                  <div
                    key={p.name}
                    style={{
                      display: 'flex',
                      alignItems: 'center',
                      gap: 8,
                      padding: '6px 10px',
                      fontSize: 12,
                      borderBottom: '1px solid var(--border)',
                    }}
                  >
                    <button
                      onClick={() => handleApplyPreset(p)}
                      style={{
                        flex: 1,
                        background: 'none',
                        color: 'var(--text-primary)',
                        textAlign: 'left',
                        cursor: 'pointer',
                        padding: 0,
                      }}
                    >
                      {p.name}
                    </button>
                    <button
                      onClick={() => handleDeletePreset(p.name)}
                      style={{
                        background: 'none',
                        color: 'var(--text-tertiary)',
                        cursor: 'pointer',
                        padding: 0,
                        fontSize: 14,
                      }}
                      title="Delete preset"
                    >
                      ✕
                    </button>
                  </div>
                ))}
              </div>
            )}
          </div>
        )}
      </div>

      {/* Row 2: active chips */}
      {chips.length > 0 && (
        <div style={{
          display: 'flex',
          gap: 6,
          marginTop: 'var(--space-sm)',
          flexWrap: 'wrap',
        }}>
          {chips.map(c => (
            <span
              key={c.key}
              style={{
                display: 'inline-flex',
                alignItems: 'center',
                gap: 4,
                padding: '2px 8px',
                background: 'var(--bg-2)',
                border: '1px solid var(--border)',
                borderRadius: 'var(--radius-sm)',
                fontSize: 11,
                color: 'var(--text-secondary)',
              }}
            >
              {c.label}
              <button
                onClick={() => removeChip(c.key)}
                style={{
                  background: 'none',
                  color: 'var(--text-tertiary)',
                  cursor: 'pointer',
                  padding: 0,
                  fontSize: 12,
                  marginLeft: 2,
                }}
              >
                ✕
              </button>
            </span>
          ))}
        </div>
      )}
    </div>
  );
}
