import { signal } from '@preact/signals';
import { useEffect } from 'preact/hooks';
import { StatusDot } from '../components/status-dot';
import { addToast } from '../components/toast';
import { getConnections, createConnection, testConnection, deleteConnection, detectClaude, openaiDeviceStart, openaiDevicePoll } from '../api/client';

const providers = signal([]);

function groupByProvider(connections) {
  const map = {};
  for (const c of connections) {
    if (!map[c.provider]) {
      map[c.provider] = { id: c.provider, name: c.provider, type: c.provider, accounts: [] };
    }
    const state = c.state || 'idle';
    const status = (state === 'idle' || state === 'active') ? 'active'
      : (state === 'cooldown' || state === 'rate_limited') ? 'cooldown'
      : 'error';
    map[c.provider].accounts.push({
      id: c.id,
      label: c.name || c.id,
      status,
      cooldownUntil: null,
    });
  }
  return Object.values(map);
}

function loadConnections() {
  getConnections().then(data => {
    if (Array.isArray(data)) {
      providers.value = groupByProvider(data);
    }
  }).catch(() => {});
}

const showAddModal = signal(false);
const addProvider = signal('anthropic');
const addLabel = signal('');
const addApiKey = signal('');
const claudeDetect = signal(null);
const claudeDisclosure = signal(false);

// OpenAI OAuth state
const openaiDevice = signal(null); // { user_code, verification_uri }
const openaiPolling = signal(false);

function ProviderCard({ provider }) {
  const handleTest = (accountId) => {
    addToast(`Testing ${provider.name}...`, 'info');
    testConnection(accountId).then(() => {
      addToast(`${provider.name} connection OK`, 'success');
    }).catch(err => {
      addToast(`Test failed: ${err.message}`, 'error');
    });
  };

  return (
    <div style={{
      background: 'var(--bg-1)',
      border: '1px solid var(--border)',
      borderRadius: 'var(--radius-lg)',
      overflow: 'hidden',
    }}>
      <div style={{
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'space-between',
        padding: 'var(--space-lg)',
        borderBottom: '1px solid var(--border)',
      }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
          <span style={{ fontWeight: 500, fontSize: 14 }}>{provider.name}</span>
          <span style={{
            fontSize: 10,
            fontFamily: 'var(--font-mono)',
            color: 'var(--text-tertiary)',
            background: 'var(--bg-2)',
            padding: '2px 6px',
            borderRadius: 'var(--radius-sm)',
          }}>
            {provider.type}
          </span>
        </div>
        <span style={{ fontSize: 11, color: 'var(--text-tertiary)' }}>
          {provider.accounts.length} account{provider.accounts.length !== 1 ? 's' : ''}
        </span>
      </div>

      {provider.accounts.map(account => (
        <div
          key={account.id}
          style={{
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'space-between',
            padding: '10px var(--space-lg)',
            borderBottom: '1px solid var(--border)',
          }}
        >
          <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
            <StatusDot status={account.status} pulse={account.status === 'cooldown'} />
            <span style={{ fontSize: 13 }}>{account.label}</span>
            {account.cooldownUntil && (
              <span style={{
                fontSize: 11,
                fontFamily: 'var(--font-mono)',
                color: 'var(--status-yellow)',
              }}>
                {account.cooldownUntil}
              </span>
            )}
          </div>
          <button
            onClick={() => handleTest(account.id)}
            style={{
              fontSize: 11,
              color: 'var(--text-tertiary)',
              padding: '3px 8px',
              background: 'var(--bg-2)',
              border: '1px solid var(--border)',
              borderRadius: 'var(--radius-sm)',
              cursor: 'pointer',
              transition: 'var(--transition-fast)',
            }}
          >
            Test
          </button>
        </div>
      ))}
    </div>
  );
}

export function ProvidersPage() {
  useEffect(() => {
    loadConnections();
  }, []);

  return (
    <div style={{ padding: 'var(--space-2xl)', maxWidth: 960, width: '100%' }}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 'var(--space-xl)' }}>
        <h1 style={{ fontSize: 20, fontWeight: 600 }}>Providers</h1>
        <button
          onClick={() => {
            showAddModal.value = true;
            claudeDetect.value = null;
            claudeDisclosure.value = false;
            // Auto-check for Claude credentials + check if already connected
            Promise.all([detectClaude(), getConnections()]).then(([detect, conns]) => {
              const alreadyConnected = Array.isArray(conns) && conns.some(c => c.provider === 'anthropic' && c.auth_type === 'auto_detect');
              claudeDetect.value = { ...detect, _alreadyConnected: alreadyConnected };
            }).catch(() => { claudeDetect.value = { found: false }; });
          }}
          style={{
            padding: '6px 14px',
            fontSize: 13,
            color: 'var(--text-primary)',
            background: 'var(--accent)',
            borderRadius: 'var(--radius-md)',
            cursor: 'pointer',
            fontWeight: 500,
          }}
        >
          + Add Provider
        </button>
      </div>

      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-md)' }}>
        {providers.value.length === 0 ? (
          <div style={{ padding: 'var(--space-xl)', textAlign: 'center', color: 'var(--text-tertiary)', fontSize: 13, background: 'var(--bg-1)', border: '1px solid var(--border)', borderRadius: 'var(--radius-lg)' }}>
            No providers configured. Click "+ Add Provider" to get started.
          </div>
        ) : providers.value.map(p => (
          <ProviderCard key={p.id} provider={p} />
        ))}
      </div>

      {/* Add provider modal placeholder */}
      {showAddModal.value && (
        <div
          onClick={() => { showAddModal.value = false; }}
          style={{
            position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.6)',
            backdropFilter: 'blur(4px)', display: 'flex', alignItems: 'center',
            justifyContent: 'center', zIndex: 9000,
          }}
        >
          <div
            onClick={e => e.stopPropagation()}
            style={{
              background: 'var(--bg-1)', border: '1px solid var(--border-hover)',
              borderRadius: 'var(--radius-xl)', padding: 'var(--space-xl)',
              width: 400, maxWidth: '90vw',
            }}
          >
            <h2 style={{ fontSize: 16, fontWeight: 600, marginBottom: 'var(--space-lg)' }}>Add Provider</h2>
            <div style={{ marginBottom: 'var(--space-md)' }}>
              <label style={{ display: 'block', fontSize: 12, color: 'var(--text-tertiary)', marginBottom: 4 }}>Provider Type</label>
              <select
                value={addProvider.value}
                onChange={e => { addProvider.value = e.target.value; }}
                style={{
                  width: '100%', padding: '8px 10px', background: 'var(--bg-2)',
                  border: '1px solid var(--border)', borderRadius: 'var(--radius-md)',
                  color: 'var(--text-primary)', fontSize: 13,
                }}
              >
                <option value="anthropic">Anthropic</option>
                <option value="openai">OpenAI</option>
                <option value="openrouter">OpenRouter</option>
                <option value="gemini">Google (Gemini)</option>
                <option value="github-copilot">GitHub Copilot</option>
                <option value="ollama">Ollama</option>
              </select>
            </div>
            {/* Claude auto-detect card — only show if no auto_detect connection exists for anthropic */}
            {addProvider.value === 'anthropic' && claudeDetect.value?.found && !claudeDetect.value._alreadyConnected && (
              <div style={{
                marginBottom: 'var(--space-md)', padding: 'var(--space-md)',
                background: 'var(--bg-2)', border: '1px solid var(--border)',
                borderRadius: 'var(--radius-md)',
              }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 8 }}>
                  <span style={{ color: 'var(--status-green)', fontSize: 14 }}>&#10003;</span>
                  <span style={{ fontSize: 13, fontWeight: 500 }}>Claude Code detected</span>
                  {claudeDetect.value.subscription_type && (
                    <span style={{
                      fontSize: 10, fontFamily: 'var(--font-mono)', color: 'var(--text-tertiary)',
                      background: 'var(--bg-3)', padding: '2px 6px', borderRadius: 'var(--radius-sm)',
                    }}>
                      {claudeDetect.value.subscription_type}
                    </span>
                  )}
                </div>
                <label style={{ display: 'flex', alignItems: 'flex-start', gap: 8, fontSize: 11, color: 'var(--text-secondary)', cursor: 'pointer', lineHeight: 1.4 }}>
                  <input
                    type="checkbox"
                    checked={claudeDisclosure.value}
                    onChange={e => { claudeDisclosure.value = e.target.checked; }}
                    style={{ marginTop: 2 }}
                  />
                  <span>
                    I understand this uses Claude Code credentials. Anthropic's TOS restricts OAuth tokens
                    to Claude Code and Claude.ai. Anthropic does not officially support this configuration.
                  </span>
                </label>
                <div style={{ display: 'flex', gap: 8, marginTop: 'var(--space-md)' }}>
                  <button
                    disabled={!claudeDisclosure.value}
                    onClick={() => {
                      createConnection({
                        provider: 'anthropic',
                        name: 'Claude Code',
                        auth_type: 'auto_detect',
                      }).then(() => {
                        addToast('Claude Code connected', 'success');
                        showAddModal.value = false;
                        loadConnections();
                      }).catch(err => {
                        addToast('Failed: ' + err.message, 'error');
                      });
                    }}
                    style={{
                      flex: 1, padding: '6px 14px', fontSize: 12, fontWeight: 500,
                      color: 'var(--text-primary)', background: claudeDisclosure.value ? 'var(--accent)' : 'var(--bg-3)',
                      borderRadius: 'var(--radius-md)', cursor: claudeDisclosure.value ? 'pointer' : 'not-allowed',
                      opacity: claudeDisclosure.value ? 1 : 0.5,
                    }}
                  >
                    Connect with Claude Code
                  </button>
                </div>
              </div>
            )}

            {/* OpenAI OAuth device code card */}
            {addProvider.value === 'openai' && !openaiPolling.value && !openaiDevice.value && (
              <div style={{ marginBottom: 'var(--space-md)' }}>
                <button
                  onClick={() => {
                    openaiDeviceStart().then(data => {
                      openaiDevice.value = data;
                      openaiPolling.value = true;
                      addToast('Open the link in your browser to sign in', 'info');
                      // Start polling in background
                      openaiDevicePoll(data.user_code).then(() => {
                        addToast('OpenAI connected via OAuth', 'success');
                        openaiDevice.value = null;
                        openaiPolling.value = false;
                        showAddModal.value = false;
                        loadConnections();
                      }).catch(err => {
                        addToast('OAuth failed: ' + err.message, 'error');
                        openaiDevice.value = null;
                        openaiPolling.value = false;
                      });
                    }).catch(err => {
                      addToast('Failed: ' + err.message, 'error');
                    });
                  }}
                  style={{
                    width: '100%', padding: '8px 14px', fontSize: 13, fontWeight: 500,
                    color: 'var(--text-primary)', background: 'var(--bg-2)',
                    border: '1px solid var(--border)', borderRadius: 'var(--radius-md)',
                    cursor: 'pointer', textAlign: 'left',
                  }}
                >
                  Sign in with ChatGPT (OAuth)
                  <span style={{ fontSize: 11, color: 'var(--text-tertiary)', marginLeft: 8 }}>
                    Uses your ChatGPT subscription
                  </span>
                </button>
              </div>
            )}

            {/* OpenAI OAuth pending */}
            {addProvider.value === 'openai' && openaiDevice.value && (
              <div style={{
                marginBottom: 'var(--space-md)', padding: 'var(--space-md)',
                background: 'var(--accent-muted)', border: '1px solid var(--accent)',
                borderRadius: 'var(--radius-md)',
              }}>
                <div style={{ fontSize: 13, fontWeight: 500, marginBottom: 8 }}>
                  Open this link and enter the code:
                </div>
                <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 8 }}>
                  <a
                    href={openaiDevice.value.verification_uri}
                    target="_blank"
                    rel="noopener"
                    style={{ fontSize: 12, color: 'var(--accent)', fontFamily: 'var(--font-mono)', textDecoration: 'underline' }}
                  >
                    {openaiDevice.value.verification_uri}
                  </a>
                </div>
                <div style={{
                  fontSize: 24, fontFamily: 'var(--font-mono)', fontWeight: 700,
                  letterSpacing: '0.1em', color: 'var(--text-primary)', textAlign: 'center',
                  padding: '8px', background: 'var(--bg-0)', borderRadius: 'var(--radius-md)',
                }}>
                  {openaiDevice.value.user_code}
                </div>
                <div style={{ fontSize: 11, color: 'var(--text-tertiary)', marginTop: 8, textAlign: 'center' }}>
                  Waiting for authorization...
                </div>
              </div>
            )}

            {/* Separator if OAuth cards shown */}
            {(addProvider.value === 'openai' || (addProvider.value === 'anthropic' && claudeDetect.value?.found && !claudeDetect.value._alreadyConnected)) && (
              <div style={{ fontSize: 11, color: 'var(--text-tertiary)', textAlign: 'center', marginBottom: 'var(--space-md)' }}>
                — or use an API key —
              </div>
            )}

            <div style={{ marginBottom: 'var(--space-md)' }}>
              <label style={{ display: 'block', fontSize: 12, color: 'var(--text-tertiary)', marginBottom: 4 }}>Label</label>
              <input
                type="text"
                placeholder="e.g. Primary"
                value={addLabel.value}
                onInput={e => { addLabel.value = e.target.value; }}
                style={{
                  width: '100%', padding: '8px 10px', background: 'var(--bg-2)',
                  border: '1px solid var(--border)', borderRadius: 'var(--radius-md)',
                  color: 'var(--text-primary)', fontSize: 13,
                }}
              />
            </div>
            <div style={{ marginBottom: 'var(--space-lg)' }}>
              <label style={{ display: 'block', fontSize: 12, color: 'var(--text-tertiary)', marginBottom: 4 }}>
                API Key
                {addProvider.value === 'anthropic' && (
                  <span style={{ fontWeight: 400, marginLeft: 6 }}>
                    from <span style={{ color: 'var(--accent)' }}>console.anthropic.com</span>
                  </span>
                )}
              </label>
              <input
                type="password"
                placeholder="sk-..."
                value={addApiKey.value}
                onInput={e => { addApiKey.value = e.target.value; }}
                style={{
                  width: '100%', padding: '8px 10px', background: 'var(--bg-2)',
                  border: '1px solid var(--border)', borderRadius: 'var(--radius-md)',
                  color: 'var(--text-primary)', fontSize: 13, fontFamily: 'var(--font-mono)',
                }}
              />
            </div>
            <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end' }}>
              <button
                onClick={() => { showAddModal.value = false; }}
                style={{
                  padding: '6px 14px', fontSize: 13, color: 'var(--text-secondary)',
                  background: 'var(--bg-2)', border: '1px solid var(--border)',
                  borderRadius: 'var(--radius-md)', cursor: 'pointer',
                }}
              >
                Cancel
              </button>
              <button
                onClick={() => {
                  if (!addApiKey.value.trim()) {
                    addToast('API key is required', 'warning');
                    return;
                  }
                  createConnection({
                    provider: addProvider.value,
                    name: addLabel.value || addProvider.value,
                    auth_type: 'api_key',
                    api_key: addApiKey.value,
                  }).then(() => {
                    addToast('Provider added', 'success');
                    showAddModal.value = false;
                    addProvider.value = 'anthropic';
                    addLabel.value = '';
                    addApiKey.value = '';
                    loadConnections();
                  }).catch(err => {
                    addToast('Failed: ' + err.message, 'error');
                  });
                }}
                style={{
                  padding: '6px 14px', fontSize: 13, color: 'var(--text-primary)',
                  background: 'var(--accent)', borderRadius: 'var(--radius-md)',
                  cursor: 'pointer', fontWeight: 500,
                }}
              >
                Add
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
