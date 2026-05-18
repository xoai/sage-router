import { signal, computed } from '@preact/signals';
import { useEffect } from 'preact/hooks';
import { getKeys, deleteKey } from '../api/client';
import { KeyFacets, readURLFilter } from '../components/key-facets';
import { KeyCreateWizard } from '../components/key-create-wizard';
import { KeyEditModal } from '../components/key-edit-modal';
import { fmtCost, fmtTimestamp } from '../utils/format';
import { addToast } from '../components/toast';

// KeysPage — dedicated API Keys management page.
//
// Cycle 20260516-keys-management-redesign. Replaces the inline
// per-key UI on the Connect page (which doesn't scale for teams of
// 20-30+ keys). Backed by GET /api/keys with the new pagination/
// filter envelope {items, total, limit, offset}.
//
// AC-A1..A5, AC-B1..B7, AC-C4..C5, AC-D1, AC-E1 of the spec.

const PAGE_SIZE = 25;

// Module-level signals — single instance of the page is mounted.
const keys = signal([]);
const total = signal(0);
const offset = signal(0);
const loading = signal(true);
const filter = signal(readURLFilter()); // initial filter from URL
const wizardOpen = signal(false);
const editingKey = signal(null);
const deletingKey = signal(null);
const deleteInput = signal('');
const kebabOpenFor = signal(null); // row id with kebab menu open
// Sort state — AC-A2 post-review. SortField "" = server default
// created_at DESC; non-empty values are server-validated against the
// whitelist {name, created_at}. Click-to-toggle pattern: first click
// = asc, repeat click = desc, third click = back to default.
const sortField = signal('');
const sortDir = signal('');

function buildQueryParams() {
  const f = filter.value;
  const params = { limit: PAGE_SIZE, offset: offset.value };
  if (f.search) params.search = f.search;
  if (f.routing) params.routing = f.routing;
  if (f.has_budget != null) params.has_budget = String(f.has_budget);
  if (sortField.value) {
    params.sort = sortField.value;
    params.dir = sortDir.value || 'asc';
  }
  return params;
}

// handleSortClick — click cycle: default → asc → desc → default.
// Clicking a different column resets to that column's asc.
function handleSortClick(field) {
  if (sortField.value !== field) {
    sortField.value = field;
    sortDir.value = 'asc';
  } else if (sortDir.value === 'asc') {
    sortDir.value = 'desc';
  } else {
    // From desc → clear (return to server default)
    sortField.value = '';
    sortDir.value = '';
  }
  offset.value = 0; // sort change resets pagination, same as filter change
  loadKeys();
}

function sortIndicator(field) {
  if (sortField.value !== field) return '';
  return sortDir.value === 'asc' ? ' ↑' : ' ↓';
}

function loadKeys() {
  loading.value = true;
  getKeys(buildQueryParams()).then(res => {
    keys.value = Array.isArray(res?.items) ? res.items : [];
    total.value = res?.total ?? 0;
    loading.value = false;
  }).catch(() => {
    loading.value = false;
    addToast('Failed to load keys', 'error');
  });
}

function handleFilterChange(next) {
  filter.value = next;
  offset.value = 0; // AC-C5: filter change resets pagination
  loadKeys();
}

function handlePageChange(newOffset) {
  offset.value = newOffset;
  loadKeys();
}

function handleDeleteConfirm() {
  const k = deletingKey.value;
  if (!k) return;
  // Require the user to type the prefix exactly.
  if (deleteInput.value !== k.prefix) {
    addToast('Type the prefix exactly to confirm', 'warning');
    return;
  }
  deleteKey(k.id).then(() => {
    addToast(`Deleted ${k.name}`, 'success');
    deletingKey.value = null;
    deleteInput.value = '';
    loadKeys();
  }).catch(err => {
    addToast('Delete failed: ' + (err?.message || 'unknown'), 'error');
  });
}

const hasAnyFilter = computed(() =>
  filter.value.search || filter.value.routing || filter.value.has_budget != null
);

const pageNumber = computed(() => Math.floor(offset.value / PAGE_SIZE) + 1);
const totalPages = computed(() => Math.max(1, Math.ceil(total.value / PAGE_SIZE)));

export function KeysPage() {
  useEffect(() => { loadKeys(); }, []);

  return (
    <div style={{ padding: 'var(--space-2xl)', maxWidth: 1100, width: '100%' }}>
      {/* Header */}
      <div style={{
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'space-between',
        marginBottom: 'var(--space-lg)',
      }}>
        <h1 style={{ fontSize: 20, fontWeight: 600 }}>API Keys</h1>
        <button
          onClick={() => { wizardOpen.value = true; }}
          style={{
            padding: '8px 14px',
            fontSize: 13,
            background: 'var(--accent)',
            color: 'var(--text-primary)',
            border: 'none',
            borderRadius: 'var(--radius-md)',
            cursor: 'pointer',
            fontWeight: 500,
          }}
        >
          + New Key
        </button>
      </div>

      {/* Facets */}
      <KeyFacets value={filter.value} onChange={handleFilterChange} />

      {/* Table */}
      <div style={{
        background: 'var(--bg-1)',
        border: '1px solid var(--border)',
        borderRadius: 'var(--radius-lg)',
        overflow: 'hidden',
      }}>
        <table style={{ width: '100%', borderCollapse: 'collapse' }}>
          <thead>
            <tr style={{ fontSize: 11, textTransform: 'uppercase', letterSpacing: '0.05em' }}>
              <th
                onClick={() => handleSortClick('name')}
                style={{ ...th, cursor: 'pointer', userSelect: 'none' }}
                title="Click to sort by Name"
              >
                Name{sortIndicator('name')}
              </th>
              <th style={th}>Prefix</th>
              <th
                onClick={() => handleSortClick('created_at')}
                style={{ ...th, cursor: 'pointer', userSelect: 'none' }}
                title="Click to sort by Created"
              >
                Created{sortIndicator('created_at')}
              </th>
              <th style={{ ...th, textAlign: 'right' }}>Budget</th>
              <th style={{ ...th, textAlign: 'right' }}>Rate Limit</th>
              <th style={{ ...th, textAlign: 'right' }}>Actions</th>
            </tr>
          </thead>
          <tbody>
            {loading.value && (
              <tr>
                <td colSpan={6} style={{ padding: '32px 16px', textAlign: 'center', color: 'var(--text-tertiary)', fontSize: 13 }}>
                  Loading…
                </td>
              </tr>
            )}
            {!loading.value && keys.value.length === 0 && (
              <tr>
                <td colSpan={6} style={{ padding: '32px 16px', textAlign: 'center', color: 'var(--text-tertiary)', fontSize: 13 }}>
                  {hasAnyFilter.value ? (
                    <>
                      <div>No keys match the current filter.</div>
                      <button
                        onClick={() => handleFilterChange({ search: '', routing: '', has_budget: null })}
                        style={{
                          marginTop: 8,
                          padding: '4px 10px',
                          fontSize: 12,
                          background: 'var(--bg-2)',
                          border: '1px solid var(--border)',
                          borderRadius: 'var(--radius-md)',
                          color: 'var(--text-secondary)',
                          cursor: 'pointer',
                        }}
                      >
                        Clear filters
                      </button>
                    </>
                  ) : (
                    <>
                      <div style={{ fontSize: 14, marginBottom: 4 }}>No API keys yet.</div>
                      <div>Create one to start routing requests through sage-router.</div>
                    </>
                  )}
                </td>
              </tr>
            )}
            {!loading.value && keys.value.map(k => (
              <tr key={k.id} style={{ borderTop: '1px solid var(--border)' }}>
                <td style={td}>
                  <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', maxWidth: 200, display: 'inline-block' }}>
                    {k.name}
                  </span>
                </td>
                <td style={{ ...td, fontFamily: 'var(--font-mono)', fontSize: 11, color: 'var(--text-tertiary)' }}>
                  {k.prefix}…
                </td>
                <td style={{ ...td, fontSize: 12, color: 'var(--text-tertiary)' }} title={k.created_at}>
                  {fmtTimestamp(k.created_at)}
                </td>
                <td style={{ ...td, textAlign: 'right', fontFamily: 'var(--font-mono)', fontSize: 12 }}>
                  {k.budget_monthly > 0 ? (
                    <>
                      {fmtCost(k.budget_monthly)}<span style={{ color: 'var(--text-tertiary)' }}>/mo</span>
                      {k.budget_hard_limit && (
                        <span title="Hard limit — requests blocked at cap" style={{ marginLeft: 4 }}>🔒</span>
                      )}
                    </>
                  ) : (
                    <span style={{ color: 'var(--text-tertiary)' }}>unlimited</span>
                  )}
                </td>
                <td style={{ ...td, textAlign: 'right', fontFamily: 'var(--font-mono)', fontSize: 12 }}>
                  {k.rate_limit_rpm > 0 ? `${k.rate_limit_rpm} RPM` : <span style={{ color: 'var(--text-tertiary)' }}>—</span>}
                </td>
                <td style={{ ...td, textAlign: 'right', position: 'relative' }}>
                  <button
                    onClick={() => { editingKey.value = k; }}
                    style={{
                      padding: '3px 10px',
                      fontSize: 11,
                      background: 'var(--bg-2)',
                      color: 'var(--text-secondary)',
                      border: '1px solid var(--border)',
                      borderRadius: 'var(--radius-sm)',
                      cursor: 'pointer',
                      marginRight: 4,
                    }}
                  >
                    Edit
                  </button>
                  <button
                    onClick={() => {
                      kebabOpenFor.value = kebabOpenFor.value === k.id ? null : k.id;
                    }}
                    style={{
                      padding: '3px 8px',
                      fontSize: 11,
                      background: 'var(--bg-2)',
                      color: 'var(--text-secondary)',
                      border: '1px solid var(--border)',
                      borderRadius: 'var(--radius-sm)',
                      cursor: 'pointer',
                    }}
                    aria-label="More actions"
                    title="More actions"
                  >
                    ⋯
                  </button>
                  {kebabOpenFor.value === k.id && (
                    <>
                      {/* Backdrop catches outside-click. */}
                      <div
                        onClick={() => { kebabOpenFor.value = null; }}
                        style={{
                          position: 'fixed', inset: 0, zIndex: 999,
                          background: 'transparent',
                        }}
                      />
                      <div style={{
                        position: 'absolute',
                        right: 0,
                        top: '100%',
                        marginTop: 2,
                        minWidth: 120,
                        background: 'var(--bg-1)',
                        border: '1px solid var(--border)',
                        borderRadius: 'var(--radius-md)',
                        boxShadow: '0 4px 12px rgba(0,0,0,0.3)',
                        zIndex: 1000,
                      }}>
                        <button
                          onClick={() => {
                            kebabOpenFor.value = null;
                            deletingKey.value = k;
                            deleteInput.value = '';
                          }}
                          style={{
                            display: 'block',
                            width: '100%',
                            padding: '6px 12px',
                            fontSize: 12,
                            background: 'transparent',
                            color: 'var(--status-red)',
                            border: 'none',
                            textAlign: 'left',
                            cursor: 'pointer',
                          }}
                        >
                          Delete
                        </button>
                      </div>
                    </>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>

        {/* Paginator */}
        {!loading.value && total.value > 0 && (
          <div style={{
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'space-between',
            padding: 'var(--space-md)',
            borderTop: '1px solid var(--border)',
            fontSize: 12,
            color: 'var(--text-tertiary)',
          }}>
            <span>
              Showing {offset.value + 1}-{Math.min(offset.value + PAGE_SIZE, total.value)} of {total.value}
            </span>
            <div style={{ display: 'flex', gap: 4 }}>
              <button
                onClick={() => handlePageChange(Math.max(0, offset.value - PAGE_SIZE))}
                disabled={offset.value === 0}
                style={pagerBtn(offset.value === 0)}
              >
                ← Prev
              </button>
              <span style={{ padding: '4px 8px', color: 'var(--text-secondary)' }}>
                {pageNumber.value} / {totalPages.value}
              </span>
              <button
                onClick={() => handlePageChange(offset.value + PAGE_SIZE)}
                disabled={offset.value + PAGE_SIZE >= total.value}
                style={pagerBtn(offset.value + PAGE_SIZE >= total.value)}
              >
                Next →
              </button>
            </div>
          </div>
        )}
      </div>

      {/* Create wizard */}
      {wizardOpen.value && (
        <KeyCreateWizard
          onClose={() => { wizardOpen.value = false; }}
          onCreated={() => { wizardOpen.value = false; loadKeys(); }}
        />
      )}

      {/* Edit modal */}
      {editingKey.value && (
        <KeyEditModal
          keyRow={editingKey.value}
          onClose={() => { editingKey.value = null; }}
          onSaved={() => { editingKey.value = null; loadKeys(); }}
        />
      )}

      {/* Delete confirmation */}
      {deletingKey.value && (
        <div
          onClick={() => { deletingKey.value = null; }}
          style={{
            position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.6)',
            backdropFilter: 'blur(4px)', display: 'flex', alignItems: 'center',
            justifyContent: 'center', zIndex: 9000,
          }}
        >
          <div
            onClick={e => e.stopPropagation()}
            style={{
              background: 'var(--bg-1)',
              border: '1px solid var(--border-hover)',
              borderRadius: 'var(--radius-xl)',
              padding: 'var(--space-xl)',
              width: 460,
              maxWidth: '92vw',
            }}
          >
            <h3 style={{ fontSize: 15, fontWeight: 600, marginBottom: 'var(--space-md)' }}>
              Delete API key?
            </h3>
            <div style={{ fontSize: 12, color: 'var(--text-secondary)', marginBottom: 'var(--space-md)', lineHeight: 1.4 }}>
              This permanently revokes <strong>{deletingKey.value.name}</strong>. Any tool
              currently using this key will fail next request. Type the prefix to confirm:
            </div>
            <div style={{
              fontFamily: 'var(--font-mono)',
              fontSize: 12,
              color: 'var(--text-tertiary)',
              marginBottom: 6,
            }}>
              {deletingKey.value.prefix}
            </div>
            <input
              type="text"
              value={deleteInput.value}
              onInput={e => { deleteInput.value = e.target.value; }}
              autoFocus
              style={{
                width: '100%',
                padding: '8px 10px',
                background: 'var(--bg-2)',
                border: '1px solid var(--border)',
                borderRadius: 'var(--radius-md)',
                color: 'var(--text-primary)',
                fontSize: 13,
                fontFamily: 'var(--font-mono)',
                marginBottom: 'var(--space-lg)',
              }}
            />
            <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end' }}>
              <button
                onClick={() => { deletingKey.value = null; }}
                style={{
                  padding: '6px 14px', fontSize: 13,
                  color: 'var(--text-secondary)',
                  background: 'var(--bg-2)',
                  border: '1px solid var(--border)',
                  borderRadius: 'var(--radius-md)',
                  cursor: 'pointer',
                }}
              >
                Cancel
              </button>
              <button
                onClick={handleDeleteConfirm}
                disabled={deleteInput.value !== deletingKey.value.prefix}
                style={{
                  padding: '6px 14px', fontSize: 13,
                  color: 'var(--text-primary)',
                  background: deleteInput.value === deletingKey.value.prefix ? 'var(--status-red)' : 'var(--bg-2)',
                  border: deleteInput.value === deletingKey.value.prefix ? 'none' : '1px solid var(--border)',
                  borderRadius: 'var(--radius-md)',
                  cursor: deleteInput.value === deletingKey.value.prefix ? 'pointer' : 'not-allowed',
                  fontWeight: 500,
                  opacity: deleteInput.value === deletingKey.value.prefix ? 1 : 0.5,
                }}
              >
                Delete
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

const th = {
  padding: '8px 16px',
  textAlign: 'left',
  fontWeight: 500,
  color: 'var(--text-tertiary)',
};

const td = {
  padding: '10px 16px',
  fontSize: 13,
};

function pagerBtn(disabled) {
  return {
    padding: '4px 10px',
    fontSize: 12,
    background: 'var(--bg-2)',
    color: disabled ? 'var(--text-tertiary)' : 'var(--text-secondary)',
    border: '1px solid var(--border)',
    borderRadius: 'var(--radius-md)',
    cursor: disabled ? 'not-allowed' : 'pointer',
    opacity: disabled ? 0.5 : 1,
  };
}
