import { signal, computed } from '@preact/signals';
import { useEffect, useRef } from 'preact/hooks';
import { useLocation } from 'wouter-preact';
import { CopyButton } from '../components/copy-button';
import { getKeys, getModels, getCombos } from '../api/client';
import { parseEntries } from '../components/model-picker-helpers';
import { filterAllowed, pickInitialSelection, getConfigFile } from './connect-helpers';

// ConnectPage — read-only guide for setting up external tools/IDEs
// against sage-router. Cycle 20260518-connect-page-guide removed all
// key-management surfaces (KeyCreateWizard, '+ New', 'Manage keys →')
// — /keys is now the single home for key CRUD. This page is a pure
// picker → filtered model/combo → tool → config-snippet flow.

const endpointUrl = signal(window.location.origin);
const activeTool = signal('claude-code');

// API Keys state — picker only.
const allKeys = signal([]);
const selectedKeyId = signal('');

// Searchable picker state.
const keyPopoverOpen = signal(false);
const keySearch = signal('');

// Model selection state.
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

const activeKeyDisplay = computed(() => {
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
  getKeys().then(res => {
    const items = Array.isArray(res?.items) ? res.items : [];
    allKeys.value = items;
    if (items.length > 0 && !selectedKeyId.value) {
      selectedKeyId.value = items[0].id;
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

// Button styled as inline text-link — matches the dashboard's
// "navigate via setLocation" idiom while LOOKING like a link.
function LinkButton({ onClick, children }) {
  return (
    <button
      onClick={onClick}
      style={{
        background: 'transparent', border: 'none', padding: 0,
        color: 'var(--accent)', textDecoration: 'underline',
        cursor: 'pointer', font: 'inherit',
      }}
    >
      {children}
    </button>
  );
}

function getInstructions(tool) {
  const url = endpointUrl.value;
  const key = activeKeyDisplay.value;
  const model = activeModel.value;
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
        description: 'Configure the OpenAI-compatible base URL, then launch Codex.',
        steps: [
          { label: 'Set environment variables', code: openaiEnv, lang: 'bash' },
          { label: 'Run', code: 'codex', lang: 'bash' },
        ],
      };
    case 'cursor':
      return {
        title: 'Cursor',
        description: 'Settings → Models → Override OpenAI Base URL.',
        steps: [
          { label: 'OpenAI Base URL', code: `${url}/v1`, lang: '' },
          { label: 'API Key', code: key, lang: '' },
          { label: 'Model', code: model, lang: '' },
        ],
      };
    case 'cline':
      return {
        title: 'Cline (VSCode)',
        description: 'Settings → Cline → OpenAI-compatible provider.',
        steps: [
          { label: 'Base URL', code: `${url}/v1`, lang: '' },
          { label: 'API Key', code: key, lang: '' },
          { label: 'Model', code: model, lang: '' },
        ],
      };
    case 'windsurf':
      return {
        title: 'Windsurf',
        description: 'Settings → AI → OpenAI provider override.',
        steps: [
          { label: 'Base URL', code: `${url}/v1`, lang: '' },
          { label: 'API Key', code: key, lang: '' },
          { label: 'Model', code: model, lang: '' },
        ],
      };
    case 'continue':
      return {
        title: 'Continue',
        description: 'Add to ~/.continue/config.json or VS Code settings.',
        steps: [
          { label: 'Add to config.json models array', code: `{\n  "title": "Sage Router",\n  "provider": "openai",\n  "model": "${model}",\n  "apiBase": "${url}/v1",\n  "apiKey": "${key}"\n}`, lang: 'json' },
        ],
      };
    case 'aider':
      return {
        title: 'Aider',
        description: 'Set environment variables, then launch Aider.',
        steps: [
          { label: 'Set environment variables', code: openaiEnv, lang: 'bash' },
          { label: 'Run', code: `aider --model openai/${model}`, lang: 'bash' },
        ],
      };
    case 'antigravity':
      return {
        title: 'Antigravity',
        description: 'OpenAI-compatible provider.',
        steps: [
          { label: 'Base URL', code: `${url}/v1`, lang: '' },
          { label: 'API Key', code: key, lang: '' },
          { label: 'Model', code: model, lang: '' },
        ],
      };
    case 'openclaw':
    case 'opencode':
      return {
        title: tool === 'openclaw' ? 'OpenClaw' : 'OpenCode',
        description: 'Anthropic-compatible provider.',
        steps: [
          { label: 'Base URL', code: url, lang: '' },
          { label: 'API Key', code: key, lang: '' },
          { label: 'Model', code: model, lang: '' },
        ],
      };
    case 'generic':
      return {
        title: 'Generic OpenAI-compatible API',
        description: 'Most OpenAI SDK clients work out of the box.',
        steps: [
          { label: 'cURL', code: `curl ${url}/v1/chat/completions \\\n  -H "Authorization: Bearer ${key}" \\\n  -H "Content-Type: application/json" \\\n  -d '{\n    "model": "${model}",\n    "messages": [{"role": "user", "content": "Hello"}]\n  }'`, lang: 'bash' },
          { label: 'Python (openai SDK)', code: `from openai import OpenAI\n\nclient = OpenAI(\n    base_url="${url}/v1",\n    api_key="${key}",\n)\n\nresponse = client.chat.completions.create(\n    model="${model}",\n    messages=[{"role": "user", "content": "Hello"}],\n)`, lang: 'python' },
        ],
      };
    default:
      return { title: '', description: '', steps: [] };
  }
}

// ── Page ──

export function ConnectPage() {
  const [, setLocation] = useLocation();
  const popoverRef = useRef(null);
  const searchInputRef = useRef(null);

  useEffect(() => {
    loadKeys();
    loadModels();
    loadCombos();
  }, []);

  // Allowed-models filter derived from the selected key.
  const selectedKey = allKeys.value.find(k => k.id === selectedKeyId.value);
  const allowedEntries = parseEntries(selectedKey?.allowed_models || '*');
  const filteredModels = filterAllowed(availableModels.value, allowedEntries, 'id');
  const filteredCombos = filterAllowed(allCombos.value, allowedEntries, 'name');

  // Auto-reset when key OR catalog/combos change. The catalog deps
  // (availableModels.value, allCombos.value) cover the initial-load
  // race: loadKeys/loadModels/loadCombos fire in parallel from
  // useEffect(...,[]) above, and loadModels assigns
  // selectedModel.value = data[0].id unconditionally — if that
  // model isn't in the first key's allowed_models the user would
  // see a stale snippet on first paint without this dep. Effect
  // captures filteredModels/filteredCombos from render closure;
  // pickInitialSelection is idempotent (returns currentValue when
  // it's still valid), so no infinite loop.
  useEffect(() => {
    selectedModel.value = pickInitialSelection(selectedModel.value, filteredModels, 'id');
    selectedCombo.value = pickInitialSelection(selectedCombo.value, filteredCombos, 'name');
  }, [selectedKeyId.value, availableModels.value, allCombos.value]);

  // Popover: auto-focus search input on open, ESC + click-outside close.
  // Mirrors usage.jsx:151-173 idiom (cycle 20260517 T9b).
  useEffect(() => {
    if (!keyPopoverOpen.value) return undefined;
    // Auto-focus search input on open (NEW behavior — not inherited).
    setTimeout(() => searchInputRef.current?.focus(), 0);
    const onKey = e => { if (e.key === 'Escape') keyPopoverOpen.value = false; };
    const onClick = e => {
      if (popoverRef.current && !popoverRef.current.contains(e.target)) {
        keyPopoverOpen.value = false;
      }
    };
    document.addEventListener('keydown', onKey);
    // Belt-and-suspenders: trigger button calls e.stopPropagation;
    // setTimeout defers listener attach so the opening click doesn't
    // immediately close. Both guards intentional.
    const t = setTimeout(() => document.addEventListener('click', onClick), 0);
    return () => {
      clearTimeout(t);
      document.removeEventListener('keydown', onKey);
      document.removeEventListener('click', onClick);
    };
  }, [keyPopoverOpen.value]);

  // Filtered keys for the popover search.
  const filteredKeys = allKeys.value.filter(k => {
    if (!keySearch.value) return true;
    const q = keySearch.value.toLowerCase();
    return (k.name || '').toLowerCase().includes(q)
      || (k.prefix || '').toLowerCase().includes(q);
  });

  const triggerLabel = selectedKey
    ? `${selectedKey.name}  ${selectedKey.prefix}…`
    : 'Pick an API key';

  // Empty-state: zero keys → guide prose only, no Step 2/3 sections.
  if (allKeys.value.length === 0) {
    return (
      <div style={{ padding: 'var(--space-2xl)', maxWidth: 760, width: '100%' }}>
        <h1 style={{ fontSize: 20, fontWeight: 600, marginBottom: 'var(--space-xl)' }}>Connect</h1>
        <section style={{
          background: 'var(--bg-1)', border: '1px solid var(--border)',
          borderRadius: 'var(--radius-lg)', padding: 'var(--space-lg)',
        }}>
          <p style={{ fontSize: 13, color: 'var(--text-secondary)', margin: 0 }}>
            No API keys yet. Create one on the{' '}
            <LinkButton onClick={() => setLocation('/keys')}>Keys page</LinkButton>{' '}
            to see connection instructions here.
          </p>
        </section>
      </div>
    );
  }

  const instructions = getInstructions(activeTool.value);
  const configFile = getConfigFile(activeTool.value);
  const selectedCombo_obj = filteredCombos.find(c => c.name === selectedCombo.value);

  return (
    <div style={{ padding: 'var(--space-2xl)', maxWidth: 760, width: '100%' }}>
      <h1 style={{ fontSize: 20, fontWeight: 600, marginBottom: 'var(--space-xl)' }}>Connect</h1>

      {/* Step 1: Searchable API Key picker */}
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
          <span style={{ fontSize: 14, fontWeight: 600 }}>Choose API Key</span>
        </div>

        <div style={{ position: 'relative' }} ref={popoverRef}>
          <button
            onClick={e => { e.stopPropagation(); keyPopoverOpen.value = !keyPopoverOpen.value; }}
            style={{
              width: '100%', padding: '8px 10px', textAlign: 'left',
              background: 'var(--bg-2)', border: '1px solid var(--border)',
              borderRadius: 'var(--radius-md)', color: 'var(--text-primary)',
              fontSize: 13, fontFamily: 'var(--font-mono)', cursor: 'pointer',
              display: 'flex', alignItems: 'center', justifyContent: 'space-between',
            }}
          >
            <span>{triggerLabel}</span>
            <span style={{ fontSize: 10, color: 'var(--text-tertiary)' }}>▼</span>
          </button>

          {keyPopoverOpen.value && (
            <div style={{
              position: 'absolute', top: '100%', left: 0, right: 0, marginTop: 4,
              maxHeight: 320, overflowY: 'auto',
              background: 'var(--bg-2)', border: '1px solid var(--border)',
              borderRadius: 'var(--radius-md)', zIndex: 10,
              boxShadow: '0 4px 12px rgba(0,0,0,0.2)',
            }}>
              <div style={{ padding: 8, borderBottom: '1px solid var(--border)' }}>
                <input
                  ref={searchInputRef}
                  type="text"
                  placeholder="Search keys..."
                  value={keySearch.value}
                  onChange={e => { keySearch.value = e.target.value; }}
                  style={{
                    width: '100%', padding: '6px 10px',
                    background: 'var(--bg-1)', border: '1px solid var(--border)',
                    borderRadius: 'var(--radius-sm)', color: 'var(--text-primary)',
                    fontSize: 12,
                  }}
                />
              </div>
              <div style={{ padding: '4px 0' }}>
                {filteredKeys.length === 0 ? (
                  <div style={{ padding: '12px 16px', fontSize: 12, color: 'var(--text-tertiary)' }}>
                    No keys match
                  </div>
                ) : (
                  filteredKeys.map(k => {
                    const isSel = k.id === selectedKeyId.value;
                    return (
                      <div
                        key={k.id}
                        onClick={() => {
                          selectedKeyId.value = k.id;
                          keyPopoverOpen.value = false;
                          keySearch.value = '';
                        }}
                        style={{
                          padding: '8px 12px', cursor: 'pointer', fontSize: 13,
                          background: isSel ? 'var(--bg-3)' : 'transparent',
                          display: 'flex', alignItems: 'center', gap: 8,
                        }}
                      >
                        <span style={{ width: 12, fontSize: 11, color: 'var(--accent)' }}>
                          {isSel ? '●' : '○'}
                        </span>
                        <span style={{ flex: 1 }}>{k.name}</span>
                        <code style={{ fontSize: 10, color: 'var(--text-tertiary)' }}>{k.prefix}</code>
                      </div>
                    );
                  })
                )}
              </div>
              <div style={{ padding: 8, borderTop: '1px solid var(--border)', fontSize: 11 }}>
                <LinkButton onClick={() => setLocation('/keys')}>Manage keys on the Keys page</LinkButton>
              </div>
            </div>
          )}
        </div>
      </section>

      {/* Step 2: Choose Model or Combo */}
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
          filteredModels.length > 0 ? (
            <select
              value={selectedModel.value}
              onChange={e => { selectedModel.value = e.target.value; }}
              style={{
                width: '100%', padding: '8px 10px', background: 'var(--bg-2)',
                border: '1px solid var(--border)', borderRadius: 'var(--radius-md)',
                color: 'var(--text-primary)', fontSize: 13, fontFamily: 'var(--font-mono)',
              }}
            >
              {filteredModels.map(m => (
                <option key={m.id} value={m.id}>
                  {m.id}{m.display_name ? ` — ${m.display_name}` : ''}
                </option>
              ))}
            </select>
          ) : (
            <div style={{ fontSize: 12, color: 'var(--text-tertiary)', padding: '8px 0' }}>
              This key's allowed_models doesn't include any available models. Edit the key on the{' '}
              <LinkButton onClick={() => setLocation('/keys')}>Keys page</LinkButton>{' '}
              to allow more.
            </div>
          )
        )}

        {modelMode.value === 'combo' && (
          filteredCombos.length > 0 ? (
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
                {filteredCombos.map(c => (
                  <option key={c.id} value={c.name}>{c.name}</option>
                ))}
              </select>
              {selectedCombo_obj?.models && (
                <div style={{ marginTop: 8, display: 'flex', gap: 4, flexWrap: 'wrap', alignItems: 'center' }}>
                  {selectedCombo_obj.models.map((m, i) => (
                    <span key={i} style={{ display: 'flex', alignItems: 'center', gap: 4 }}>
                      <span style={{ fontSize: 11, fontFamily: 'var(--font-mono)', color: 'var(--text-secondary)', background: 'var(--bg-3)', padding: '2px 6px', borderRadius: 'var(--radius-sm)' }}>{m}</span>
                      {i < selectedCombo_obj.models.length - 1 && <span style={{ fontSize: 10, color: 'var(--text-tertiary)' }}>→</span>}
                    </span>
                  ))}
                </div>
              )}
            </div>
          ) : (
            <div style={{ fontSize: 12, color: 'var(--text-tertiary)', padding: '8px 0' }}>
              This key's allowed_models doesn't include any combos. Edit the key on the{' '}
              <LinkButton onClick={() => setLocation('/keys')}>Keys page</LinkButton>{' '}
              to allow more.
            </div>
          )
        )}
      </section>

      {/* Step 3: Connect Your Tool */}
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
        <p style={{ fontSize: 12, color: 'var(--text-secondary)', marginBottom: configFile ? 6 : 'var(--space-lg)' }}>
          {instructions.description}
        </p>

        {configFile && (
          <div style={{ fontSize: 11, color: 'var(--text-tertiary)', marginBottom: 'var(--space-lg)' }}>
            Where to paste: <span style={{ fontFamily: 'var(--font-mono)', color: 'var(--text-secondary)' }}>{configFile}</span>
          </div>
        )}

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
    </div>
  );
}
