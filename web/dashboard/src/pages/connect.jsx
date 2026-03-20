import { signal, computed } from '@preact/signals';
import { useEffect } from 'preact/hooks';
import { CopyButton } from '../components/copy-button';
import { getKeys, createKey, updateKey, deleteKey, getModels, getCombos } from '../api/client';
import { addToast } from '../components/toast';

const endpointUrl = signal(window.location.origin);
const activeTool = signal('claude-code');

// ── Delete confirmation state ──
const deleteConfirmKey = signal(null);   // key object being confirmed
const deleteConfirmInput = signal('');

// ── Edit key state ──
const editingKey = signal(null);
const editForm = signal({});

// ── API Keys state ──
const allKeys = signal([]);
const selectedKeyId = signal('');
const newKeyName = signal('');
const newKeyValue = signal(null);   // full key, only after creation
const newKeyId = signal(null);      // id of just-created key
const creatingKey = signal(false);

// ── Model selection state ──
const availableModels = signal([]);
const allCombos = signal([]);
const modelMode = signal('single');
const selectedModel = signal('');
const selectedCombo = signal('');

const tools = [
  { id: 'claude-code', label: 'Claude Code' },
  { id: 'codex', label: 'Codex CLI' },
  { id: 'cursor', label: 'Cursor' },
  { id: 'cline', label: 'Cline' },
  { id: 'windsurf', label: 'Windsurf' },
  { id: 'continue', label: 'Continue' },
  { id: 'aider', label: 'Aider' },
  { id: 'antigravity', label: 'Antigravity' },
  { id: 'openclaw', label: 'OpenClaw' },
  { id: 'opencode', label: 'OpenCode' },
  { id: 'generic', label: 'Generic OpenAI API' },
];

// ── Computed: active key display for instructions ──
const activeKeyDisplay = computed(() => {
  // If a key was just created and is selected, show the full key
  if (newKeyValue.value && newKeyId.value === selectedKeyId.value) {
    return newKeyValue.value;
  }
  const key = allKeys.value.find(k => k.id === selectedKeyId.value);
  if (!key) return '<your-api-key>';
  return key.prefix + '********************************';
});

const activeModel = computed(() => {
  if (modelMode.value === 'combo') return selectedCombo.value || '<combo-name>';
  return selectedModel.value || '<provider/model>';
});

// ── Loaders ──

function loadKeys() {
  getKeys().then(data => {
    if (Array.isArray(data)) {
      allKeys.value = data;
      // Auto-select newly created key, or first key if none selected
      if (newKeyId.value && data.find(k => k.id === newKeyId.value)) {
        selectedKeyId.value = newKeyId.value;
      } else if (data.length > 0 && !selectedKeyId.value) {
        selectedKeyId.value = data[0].id;
      }
    }
  }).catch(() => {});
}

function loadModels() {
  getModels().then(data => {
    if (Array.isArray(data)) {
      availableModels.value = data;
      if (data.length > 0 && !selectedModel.value) {
        selectedModel.value = data[0].id;
      }
    }
  }).catch(() => {});
}

function loadCombos() {
  getCombos().then(data => {
    if (Array.isArray(data)) {
      allCombos.value = data;
      if (data.length > 0 && !selectedCombo.value) {
        selectedCombo.value = data[0].name;
      }
    }
  }).catch(() => {});
}

function getNextKeyName() {
  const existing = allKeys.value.map(k => k.name);
  if (!existing.includes('Default')) return 'Default';
  let i = 2;
  while (existing.includes(`Default ${i}`)) i++;
  return `Default ${i}`;
}

function handleCreateKey() {
  const name = newKeyName.value.trim() || getNextKeyName();
  creatingKey.value = true;
  createKey({ name }).then(data => {
    newKeyValue.value = data.key;
    newKeyId.value = data.id;
    selectedKeyId.value = data.id;
    newKeyName.value = '';
    addToast('API key created — copy it now', 'success');
    creatingKey.value = false;
    loadKeys();
  }).catch(err => {
    addToast('Failed: ' + err.message, 'error');
    creatingKey.value = false;
  });
}

function requestDeleteKey(key) {
  deleteConfirmKey.value = key;
  deleteConfirmInput.value = '';
}

function confirmDeleteKey() {
  const key = deleteConfirmKey.value;
  if (!key || deleteConfirmInput.value !== key.name) return;

  deleteKey(key.id).then(() => {
    allKeys.value = allKeys.value.filter(k => k.id !== key.id);
    if (selectedKeyId.value === key.id) {
      selectedKeyId.value = allKeys.value.length > 0 ? allKeys.value[0].id : '';
      newKeyValue.value = null;
      newKeyId.value = null;
    }
    deleteConfirmKey.value = null;
    deleteConfirmInput.value = '';
    addToast('Key deleted', 'info');
    loadKeys();
  }).catch(err => {
    addToast('Failed: ' + err.message, 'error');
    deleteConfirmKey.value = null;
  });
}

function cancelDeleteKey() {
  deleteConfirmKey.value = null;
  deleteConfirmInput.value = '';
}

function openEditKey(key) {
  editingKey.value = key;
  editForm.value = {
    name: key.name || '',
    budget_monthly: key.budget_monthly || 0,
    budget_hard_limit: key.budget_hard_limit || false,
    allowed_models: key.allowed_models || '*',
    rate_limit_rpm: key.rate_limit_rpm || 0,
    routing_strategy: key.routing_strategy || '',
  };
}

function saveEditKey() {
  const key = editingKey.value;
  if (!key) return;
  updateKey(key.id, editForm.value).then(() => {
    addToast('Key updated', 'success');
    editingKey.value = null;
    loadKeys();
  }).catch(err => {
    addToast('Failed: ' + err.message, 'error');
  });
}

function cancelEditKey() {
  editingKey.value = null;
}

// ── Components ──

function CodeBlock({ code, lang = '' }) {
  return (
    <div style={{
      position: 'relative', background: 'var(--bg-0)',
      border: '1px solid var(--border)', borderRadius: 'var(--radius-md)', overflow: 'hidden',
    }}>
      {lang && (
        <div style={{ padding: '4px 12px', fontSize: 10, fontFamily: 'var(--font-mono)', color: 'var(--text-tertiary)', borderBottom: '1px solid var(--border)', textTransform: 'uppercase', letterSpacing: '0.05em' }}>
          {lang}
        </div>
      )}
      <pre style={{ padding: '12px 16px', fontSize: 12, fontFamily: 'var(--font-mono)', lineHeight: 1.6, overflow: 'auto', color: 'var(--text-primary)', margin: 0, whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>
        {code}
      </pre>
      <div style={{ position: 'absolute', top: lang ? 28 : 6, right: 6 }}>
        <CopyButton text={code} label="" />
      </div>
    </div>
  );
}

function getInstructions(tool) {
  const url = endpointUrl.value;
  const key = activeKeyDisplay.value;
  const model = activeModel.value;

  // Most tools use OpenAI-compatible config
  const openaiEnv = `export OPENAI_BASE_URL="${url}/v1"\nexport OPENAI_API_KEY="${key}"`;

  switch (tool) {
    case 'claude-code':
      return {
        title: 'Claude Code',
        description: 'Set environment variables, then launch Claude Code.',
        steps: [
          { label: 'Set environment variables', code: `export ANTHROPIC_BASE_URL="${url}"\nexport ANTHROPIC_API_KEY="${key}"\nexport ANTHROPIC_MODEL="${model}"`, lang: 'bash' },
          { label: 'Run', code: 'claude', lang: 'bash' },
        ],
      };
    case 'codex':
      return {
        title: 'Codex CLI',
        description: 'Point Codex CLI at Sage Router.',
        steps: [
          { label: 'Set environment variables', code: openaiEnv, lang: 'bash' },
          { label: 'Run', code: `codex --model "${model}"`, lang: 'bash' },
        ],
      };
    case 'cursor':
      return {
        title: 'Cursor',
        description: 'Settings > Models > OpenAI API configuration.',
        steps: [
          { label: 'OpenAI API Key', code: key, lang: 'text' },
          { label: 'Override OpenAI Base URL', code: `${url}/v1`, lang: 'text' },
          { label: 'Model name', code: model, lang: 'text' },
        ],
      };
    case 'cline':
      return {
        title: 'Cline',
        description: 'Set API Provider to "OpenAI Compatible" in Cline settings.',
        steps: [
          { label: 'Base URL', code: `${url}/v1`, lang: 'text' },
          { label: 'API Key', code: key, lang: 'text' },
          { label: 'Model', code: model, lang: 'text' },
        ],
      };
    case 'windsurf':
      return {
        title: 'Windsurf',
        description: 'Configure Windsurf to use Sage Router as an OpenAI-compatible endpoint.',
        steps: [
          { label: 'In Windsurf settings, add OpenAI-compatible provider', code: '', lang: '' },
          { label: 'Base URL', code: `${url}/v1`, lang: 'text' },
          { label: 'API Key', code: key, lang: 'text' },
          { label: 'Model', code: model, lang: 'text' },
        ],
      };
    case 'continue':
      return {
        title: 'Continue',
        description: 'Add to ~/.continue/config.json or VS Code settings.',
        steps: [
          { label: 'Add to config.json models array', code: JSON.stringify({ title: "Sage Router", provider: "openai", model: model, apiBase: `${url}/v1`, apiKey: key }, null, 2), lang: 'json' },
        ],
      };
    case 'aider':
      return {
        title: 'Aider',
        description: 'Set environment variables, then run aider.',
        steps: [
          { label: 'Set environment variables', code: openaiEnv, lang: 'bash' },
          { label: 'Run', code: `aider --model "openai/${model}"`, lang: 'bash' },
        ],
      };
    case 'antigravity':
      return {
        title: 'Antigravity',
        description: 'Configure Antigravity to route through Sage Router.',
        steps: [
          { label: 'Set environment variables', code: `export ANTHROPIC_BASE_URL="${url}"\nexport ANTHROPIC_API_KEY="${key}"`, lang: 'bash' },
          { label: 'Run', code: 'antigravity', lang: 'bash' },
        ],
      };
    case 'openclaw':
      return {
        title: 'OpenClaw',
        description: 'Configure OpenClaw to use Sage Router.',
        steps: [
          { label: 'Set environment variables', code: openaiEnv, lang: 'bash' },
          { label: 'Run', code: `openclaw --model "${model}"`, lang: 'bash' },
        ],
      };
    case 'opencode':
      return {
        title: 'OpenCode',
        description: 'Configure OpenCode to use Sage Router.',
        steps: [
          { label: 'Set environment variables', code: openaiEnv, lang: 'bash' },
          { label: 'Run', code: 'opencode', lang: 'bash' },
        ],
      };
    case 'generic':
      return {
        title: 'Generic OpenAI API',
        description: 'Use with any OpenAI-compatible client or SDK.',
        steps: [
          { label: 'cURL example', code: `curl ${url}/v1/chat/completions \\\n  -H "Authorization: Bearer ${key}" \\\n  -H "Content-Type: application/json" \\\n  -d '{\n    "model": "${model}",\n    "messages": [{"role": "user", "content": "Hello"}]\n  }'`, lang: 'bash' },
          { label: 'Python (openai SDK)', code: `from openai import OpenAI\n\nclient = OpenAI(\n    base_url="${url}/v1",\n    api_key="${key}",\n)\n\nresponse = client.chat.completions.create(\n    model="${model}",\n    messages=[{"role": "user", "content": "Hello"}],\n)`, lang: 'python' },
        ],
      };
    default:
      return { title: '', description: '', steps: [] };
  }
}

// ── Page ──

export function ConnectPage() {
  useEffect(() => {
    loadKeys();
    loadModels();
    loadCombos();
  }, []);

  const instructions = getInstructions(activeTool.value);

  return (
    <div style={{ padding: 'var(--space-2xl)', maxWidth: 760, width: '100%' }}>
      <h1 style={{ fontSize: 20, fontWeight: 600, marginBottom: 'var(--space-xl)' }}>Connect</h1>

      {/* Step 1: API Key */}
      <section style={{
        background: 'var(--bg-1)', border: '1px solid var(--border)',
        borderRadius: 'var(--radius-lg)', padding: 'var(--space-lg)',
        marginBottom: 'var(--space-lg)',
      }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 'var(--space-md)' }}>
          <span style={{
            display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
            width: 22, height: 22, borderRadius: '50%', background: 'var(--accent)',
            fontSize: 11, fontWeight: 700, color: 'var(--text-primary)',
          }}>1</span>
          <span style={{ fontSize: 14, fontWeight: 600 }}>API Key</span>
          {allKeys.value.length > 0 && (
            <span style={{ fontSize: 11, color: 'var(--text-tertiary)', marginLeft: 'auto' }}>
              {allKeys.value.length} key{allKeys.value.length !== 1 ? 's' : ''}
            </span>
          )}
        </div>

        {/* Key list */}
        {allKeys.value.length > 0 && (
          <div style={{ marginBottom: 'var(--space-md)' }}>
            {allKeys.value.map(k => (
              <div key={k.id} style={{
                display: 'flex', alignItems: 'center', justifyContent: 'space-between',
                padding: '6px 0', borderBottom: '1px solid var(--border)',
              }}>
                <label style={{ display: 'flex', alignItems: 'center', gap: 8, cursor: 'pointer', flex: 1 }}>
                  <input
                    type="radio"
                    name="apikey"
                    checked={selectedKeyId.value === k.id}
                    onChange={() => {
                      selectedKeyId.value = k.id;
                      // Clear the full key display if switching away from newly created key
                      if (newKeyId.value !== k.id) {
                        newKeyValue.value = null;
                        newKeyId.value = null;
                      }
                    }}
                  />
                  <span style={{ fontSize: 13, fontWeight: 500 }}>{k.name}</span>
                  <code style={{ fontSize: 11, color: 'var(--text-tertiary)', background: 'var(--bg-2)', padding: '2px 6px', borderRadius: 'var(--radius-sm)' }}>
                    {k.prefix}
                  </code>
                  {k.budget_monthly > 0 && (
                    <span style={{ fontSize: 9, background: 'var(--accent-muted)', color: 'var(--accent)', padding: '1px 5px', borderRadius: 8 }}>
                      ${k.budget_monthly}/mo{k.budget_hard_limit ? ' hard' : ''}
                    </span>
                  )}
                  {k.rate_limit_rpm > 0 && (
                    <span style={{ fontSize: 9, background: 'var(--accent-muted)', color: 'var(--accent)', padding: '1px 5px', borderRadius: 8 }}>
                      {k.rate_limit_rpm} rpm
                    </span>
                  )}
                  {k.allowed_models && k.allowed_models !== '*' && (
                    <span style={{ fontSize: 9, background: 'var(--accent-muted)', color: 'var(--accent)', padding: '1px 5px', borderRadius: 8 }}>
                      ACL
                    </span>
                  )}
                </label>
                <div style={{ display: 'flex', gap: 4 }}>
                  <button
                    onClick={() => openEditKey(k)}
                    style={{ fontSize: 10, color: 'var(--text-secondary)', padding: '2px 6px', background: 'var(--bg-2)', border: '1px solid var(--border)', borderRadius: 'var(--radius-sm)', cursor: 'pointer' }}
                  >
                    Edit
                  </button>
                  <button
                    onClick={() => requestDeleteKey(k)}
                    style={{ fontSize: 10, color: 'var(--status-red)', padding: '2px 6px', background: 'var(--bg-2)', border: '1px solid var(--border)', borderRadius: 'var(--radius-sm)', cursor: 'pointer' }}
                  >
                    Delete
                  </button>
                </div>
              </div>
            ))}
          </div>
        )}

        {/* Create key */}
        <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
          <input
            type="text"
            placeholder={`Key name (default: ${getNextKeyName()})`}
            value={newKeyName.value}
            onInput={e => { newKeyName.value = e.target.value; }}
            onKeyDown={e => { if (e.key === 'Enter') handleCreateKey(); }}
            style={{
              flex: 1, padding: '7px 10px', background: 'var(--bg-2)',
              border: '1px solid var(--border)', borderRadius: 'var(--radius-md)',
              color: 'var(--text-primary)', fontSize: 12,
            }}
          />
          <button
            onClick={handleCreateKey}
            disabled={creatingKey.value}
            style={{
              padding: '7px 14px', fontSize: 12, fontWeight: 500, whiteSpace: 'nowrap',
              color: 'var(--text-primary)', background: 'var(--accent)',
              borderRadius: 'var(--radius-md)', cursor: 'pointer',
              opacity: creatingKey.value ? 0.6 : 1,
            }}
          >
            {creatingKey.value ? 'Creating...' : '+ New Key'}
          </button>
        </div>

        {/* Newly created key */}
        {newKeyValue.value && (
          <div style={{
            marginTop: 'var(--space-md)', padding: 'var(--space-md)',
            background: 'var(--accent-muted)', borderRadius: 'var(--radius-md)',
            border: '1px solid var(--accent)',
          }}>
            <div style={{ fontSize: 11, color: 'var(--accent)', marginBottom: 4 }}>
              Copy this key now — it won't be shown again.
            </div>
            <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
              <code style={{ fontSize: 12, fontFamily: 'var(--font-mono)', flex: 1, wordBreak: 'break-all' }}>
                {newKeyValue.value}
              </code>
              <CopyButton text={newKeyValue.value} label="Copy" />
            </div>
          </div>
        )}
      </section>

      {/* Step 2: Choose Model */}
      <section style={{
        background: 'var(--bg-1)', border: '1px solid var(--border)',
        borderRadius: 'var(--radius-lg)', padding: 'var(--space-lg)',
        marginBottom: 'var(--space-lg)',
      }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 'var(--space-md)' }}>
          <span style={{
            display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
            width: 22, height: 22, borderRadius: '50%', background: 'var(--accent)',
            fontSize: 11, fontWeight: 700, color: 'var(--text-primary)',
          }}>2</span>
          <span style={{ fontSize: 14, fontWeight: 600 }}>Choose Model</span>
        </div>

        {/* Mode tabs */}
        <div style={{
          display: 'flex', gap: 2, marginBottom: 'var(--space-md)',
          background: 'var(--bg-2)', padding: 3, borderRadius: 'var(--radius-md)',
        }}>
          {[
            { id: 'single', label: 'Single Model' },
            { id: 'combo', label: 'Combo (Fallback)' },
          ].map(m => (
            <button
              key={m.id}
              onClick={() => { modelMode.value = m.id; }}
              style={{
                flex: 1, padding: '6px 10px', fontSize: 12, fontWeight: 500,
                color: modelMode.value === m.id ? 'var(--text-primary)' : 'var(--text-tertiary)',
                background: modelMode.value === m.id ? 'var(--bg-3)' : 'transparent',
                borderRadius: 'var(--radius-sm)', cursor: 'pointer',
              }}
            >
              {m.label}
            </button>
          ))}
        </div>

        {modelMode.value === 'single' && (
          <select
            value={selectedModel.value}
            onChange={e => { selectedModel.value = e.target.value; }}
            style={{
              width: '100%', padding: '8px 10px', background: 'var(--bg-2)',
              border: '1px solid var(--border)', borderRadius: 'var(--radius-md)',
              color: 'var(--text-primary)', fontSize: 13, fontFamily: 'var(--font-mono)',
            }}
          >
            {availableModels.value.map(m => (
              <option key={m.id} value={m.id}>
                {m.id}{m.display_name ? ` — ${m.display_name}` : ''}
              </option>
            ))}
          </select>
        )}

        {modelMode.value === 'combo' && (
          allCombos.value.length > 0 ? (
            <div>
              <select
                value={selectedCombo.value}
                onChange={e => { selectedCombo.value = e.target.value; }}
                style={{
                  width: '100%', padding: '8px 10px', background: 'var(--bg-2)',
                  border: '1px solid var(--border)', borderRadius: 'var(--radius-md)',
                  color: 'var(--text-primary)', fontSize: 13, fontFamily: 'var(--font-mono)',
                }}
              >
                {allCombos.value.map(c => (
                  <option key={c.id} value={c.name}>{c.name}</option>
                ))}
              </select>
              {(() => {
                const combo = allCombos.value.find(c => c.name === selectedCombo.value);
                if (!combo || !combo.models) return null;
                return (
                  <div style={{ marginTop: 8, display: 'flex', gap: 4, flexWrap: 'wrap', alignItems: 'center' }}>
                    {combo.models.map((m, i) => (
                      <span key={i} style={{ display: 'flex', alignItems: 'center', gap: 4 }}>
                        <span style={{ fontSize: 11, fontFamily: 'var(--font-mono)', color: 'var(--text-secondary)', background: 'var(--bg-3)', padding: '2px 6px', borderRadius: 'var(--radius-sm)' }}>{m}</span>
                        {i < combo.models.length - 1 && <span style={{ fontSize: 10, color: 'var(--text-tertiary)' }}>→</span>}
                      </span>
                    ))}
                  </div>
                );
              })()}
            </div>
          ) : (
            <div style={{ fontSize: 12, color: 'var(--text-tertiary)', padding: '8px 0' }}>
              No combos defined yet. Create one in the <span style={{ color: 'var(--accent)' }}>Models</span> page.
            </div>
          )
        )}
      </section>

      {/* Step 3: Connect your tool */}
      <section style={{
        background: 'var(--bg-1)', border: '1px solid var(--border)',
        borderRadius: 'var(--radius-lg)', padding: 'var(--space-lg)',
      }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 'var(--space-md)' }}>
          <span style={{
            display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
            width: 22, height: 22, borderRadius: '50%', background: 'var(--accent)',
            fontSize: 11, fontWeight: 700, color: 'var(--text-primary)',
          }}>3</span>
          <span style={{ fontSize: 14, fontWeight: 600 }}>Connect Your Tool</span>
        </div>

        {/* Tool dropdown */}
        <select
          value={activeTool.value}
          onChange={e => { activeTool.value = e.target.value; }}
          style={{
            width: '100%', padding: '8px 10px', background: 'var(--bg-2)',
            border: '1px solid var(--border)', borderRadius: 'var(--radius-md)',
            color: 'var(--text-primary)', fontSize: 13, marginBottom: 'var(--space-lg)',
          }}
        >
          {tools.map(t => (
            <option key={t.id} value={t.id}>{t.label}</option>
          ))}
        </select>

        <h3 style={{ fontSize: 15, fontWeight: 600, marginBottom: 4 }}>{instructions.title}</h3>
        <p style={{ fontSize: 12, color: 'var(--text-secondary)', marginBottom: 'var(--space-lg)' }}>
          {instructions.description}
        </p>

        <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-md)' }}>
          {instructions.steps.map((step, i) => (
            <div key={i}>
              <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 4 }}>
                <span style={{ fontSize: 12, color: 'var(--text-secondary)' }}>{step.label}</span>
              </div>
              {step.code && <CodeBlock code={step.code} lang={step.lang} />}
            </div>
          ))}
        </div>
      </section>

      {/* Edit key modal */}
      {editingKey.value && (
        <div style={{
          position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.6)',
          display: 'flex', alignItems: 'center', justifyContent: 'center', zIndex: 1000,
        }} onClick={cancelEditKey}>
          <div style={{
            background: 'var(--bg-1)', border: '1px solid var(--border)',
            borderRadius: 'var(--radius-lg)', padding: 'var(--space-xl)',
            width: 440, maxWidth: '90vw',
          }} onClick={e => e.stopPropagation()}>
            <h3 style={{ fontSize: 16, fontWeight: 600, marginBottom: 'var(--space-md)' }}>
              Edit Key: {editingKey.value.name}
            </h3>

            <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-md)' }}>
              <label style={{ fontSize: 12, color: 'var(--text-secondary)' }}>
                Name
                <input type="text" value={editForm.value.name}
                  onInput={e => { editForm.value = { ...editForm.value, name: e.target.value }; }}
                  style={{ display: 'block', width: '100%', marginTop: 4, padding: '7px 10px', background: 'var(--bg-2)', border: '1px solid var(--border)', borderRadius: 'var(--radius-md)', color: 'var(--text-primary)', fontSize: 13, boxSizing: 'border-box' }}
                />
              </label>

              <div style={{ display: 'flex', gap: 12 }}>
                <label style={{ fontSize: 12, color: 'var(--text-secondary)', flex: 1 }}>
                  Monthly Budget ($)
                  <input type="number" step="0.01" min="0" value={editForm.value.budget_monthly}
                    onInput={e => { editForm.value = { ...editForm.value, budget_monthly: parseFloat(e.target.value) || 0 }; }}
                    style={{ display: 'block', width: '100%', marginTop: 4, padding: '7px 10px', background: 'var(--bg-2)', border: '1px solid var(--border)', borderRadius: 'var(--radius-md)', color: 'var(--text-primary)', fontSize: 13, boxSizing: 'border-box' }}
                  />
                </label>
                <label style={{ fontSize: 12, color: 'var(--text-secondary)', display: 'flex', alignItems: 'flex-end', gap: 6, paddingBottom: 8 }}>
                  <input type="checkbox" checked={editForm.value.budget_hard_limit}
                    onChange={e => { editForm.value = { ...editForm.value, budget_hard_limit: e.target.checked }; }}
                  />
                  Hard limit
                </label>
              </div>

              <label style={{ fontSize: 12, color: 'var(--text-secondary)' }}>
                Allowed Models
                <span style={{ fontSize: 10, color: 'var(--text-tertiary)', marginLeft: 4 }}>* = all, or comma-separated: anthropic/*,openai/gpt-4o</span>
                <input type="text" value={editForm.value.allowed_models}
                  onInput={e => { editForm.value = { ...editForm.value, allowed_models: e.target.value }; }}
                  style={{ display: 'block', width: '100%', marginTop: 4, padding: '7px 10px', background: 'var(--bg-2)', border: '1px solid var(--border)', borderRadius: 'var(--radius-md)', color: 'var(--text-primary)', fontSize: 13, fontFamily: 'var(--font-mono)', boxSizing: 'border-box' }}
                />
              </label>

              <div style={{ display: 'flex', gap: 12 }}>
                <label style={{ fontSize: 12, color: 'var(--text-secondary)', flex: 1 }}>
                  Rate Limit (rpm)
                  <input type="number" min="0" value={editForm.value.rate_limit_rpm}
                    onInput={e => { editForm.value = { ...editForm.value, rate_limit_rpm: parseInt(e.target.value) || 0 }; }}
                    placeholder="0 = unlimited"
                    style={{ display: 'block', width: '100%', marginTop: 4, padding: '7px 10px', background: 'var(--bg-2)', border: '1px solid var(--border)', borderRadius: 'var(--radius-md)', color: 'var(--text-primary)', fontSize: 13, boxSizing: 'border-box' }}
                  />
                </label>
                <label style={{ fontSize: 12, color: 'var(--text-secondary)', flex: 1 }}>
                  Routing Strategy
                  <select value={editForm.value.routing_strategy}
                    onChange={e => { editForm.value = { ...editForm.value, routing_strategy: e.target.value }; }}
                    style={{ display: 'block', width: '100%', marginTop: 4, padding: '7px 10px', background: 'var(--bg-2)', border: '1px solid var(--border)', borderRadius: 'var(--radius-md)', color: 'var(--text-primary)', fontSize: 13, boxSizing: 'border-box' }}
                  >
                    <option value="">Default</option>
                    <option value="fast">Fast</option>
                    <option value="cheap">Cheap</option>
                    <option value="best">Best</option>
                    <option value="balanced">Balanced</option>
                  </select>
                </label>
              </div>
            </div>

            <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end', marginTop: 'var(--space-lg)' }}>
              <button onClick={cancelEditKey}
                style={{ padding: '7px 14px', fontSize: 12, color: 'var(--text-secondary)', background: 'var(--bg-2)', border: '1px solid var(--border)', borderRadius: 'var(--radius-md)', cursor: 'pointer' }}
              >Cancel</button>
              <button onClick={saveEditKey}
                style={{ padding: '7px 14px', fontSize: 12, fontWeight: 500, color: 'var(--text-primary)', background: 'var(--accent)', borderRadius: 'var(--radius-md)', cursor: 'pointer' }}
              >Save</button>
            </div>
          </div>
        </div>
      )}

      {/* Delete confirmation modal */}
      {deleteConfirmKey.value && (
        <div style={{
          position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.6)',
          display: 'flex', alignItems: 'center', justifyContent: 'center', zIndex: 1000,
        }} onClick={cancelDeleteKey}>
          <div style={{
            background: 'var(--bg-1)', border: '1px solid var(--border)',
            borderRadius: 'var(--radius-lg)', padding: 'var(--space-xl)',
            width: 400, maxWidth: '90vw',
          }} onClick={e => e.stopPropagation()}>
            <h3 style={{ fontSize: 16, fontWeight: 600, marginBottom: 4, color: 'var(--status-red)' }}>
              Delete API Key
            </h3>
            <p style={{ fontSize: 13, color: 'var(--text-secondary)', marginBottom: 'var(--space-md)' }}>
              This will permanently revoke the key <strong>{deleteConfirmKey.value.name}</strong> ({deleteConfirmKey.value.prefix}...).
              Any tools using this key will stop working.
            </p>
            <p style={{ fontSize: 12, color: 'var(--text-tertiary)', marginBottom: 8 }}>
              Type <strong>{deleteConfirmKey.value.name}</strong> to confirm:
            </p>
            <input
              type="text"
              value={deleteConfirmInput.value}
              onInput={e => { deleteConfirmInput.value = e.target.value; }}
              onKeyDown={e => { if (e.key === 'Enter') confirmDeleteKey(); }}
              placeholder={deleteConfirmKey.value.name}
              autoFocus
              style={{
                width: '100%', padding: '8px 10px', background: 'var(--bg-2)',
                border: '1px solid var(--border)', borderRadius: 'var(--radius-md)',
                color: 'var(--text-primary)', fontSize: 13, marginBottom: 'var(--space-md)',
                boxSizing: 'border-box',
              }}
            />
            <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end' }}>
              <button
                onClick={cancelDeleteKey}
                style={{
                  padding: '7px 14px', fontSize: 12, color: 'var(--text-secondary)',
                  background: 'var(--bg-2)', border: '1px solid var(--border)',
                  borderRadius: 'var(--radius-md)', cursor: 'pointer',
                }}
              >Cancel</button>
              <button
                onClick={confirmDeleteKey}
                disabled={deleteConfirmInput.value !== deleteConfirmKey.value.name}
                style={{
                  padding: '7px 14px', fontSize: 12, fontWeight: 500,
                  color: '#fff', background: deleteConfirmInput.value === deleteConfirmKey.value.name ? 'var(--status-red)' : 'var(--bg-3)',
                  borderRadius: 'var(--radius-md)', cursor: deleteConfirmInput.value === deleteConfirmKey.value.name ? 'pointer' : 'not-allowed',
                  opacity: deleteConfirmInput.value === deleteConfirmKey.value.name ? 1 : 0.5,
                }}
              >Delete Key</button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
