import { signal, effect } from '@preact/signals';
import { useRef, useEffect } from 'preact/hooks';

const isOpen = signal(false);
const query = signal('');

const commands = [
  { id: 'overview', label: 'Go to Overview', section: 'Navigation', path: '/' },
  { id: 'providers', label: 'Go to Providers', section: 'Navigation', path: '/providers' },
  { id: 'models', label: 'Go to Models', section: 'Navigation', path: '/models' },
  { id: 'usage', label: 'Go to Usage', section: 'Navigation', path: '/usage' },
  { id: 'routing', label: 'Go to Routing', section: 'Navigation', path: '/routing' },
  { id: 'settings', label: 'Go to Settings', section: 'Navigation', path: '/settings' },
  { id: 'connect', label: 'Go to Connect', section: 'Navigation', path: '/connect' },
];

export function openPalette() {
  query.value = '';
  isOpen.value = true;
}

export function closePalette() {
  isOpen.value = false;
}

// Listen for Cmd+K / Ctrl+K globally
if (typeof window !== 'undefined') {
  window.addEventListener('keydown', (e) => {
    if ((e.metaKey || e.ctrlKey) && e.key === 'k') {
      e.preventDefault();
      isOpen.value ? closePalette() : openPalette();
    }
    if (e.key === 'Escape' && isOpen.value) {
      closePalette();
    }
  });
}

export function CommandPalette({ onNavigate }) {
  const inputRef = useRef(null);
  const selectedIdx = signal(0);

  useEffect(() => {
    if (isOpen.value && inputRef.current) {
      inputRef.current.focus();
    }
  }, [isOpen.value]);

  if (!isOpen.value) return null;

  const filtered = commands.filter(c =>
    c.label.toLowerCase().includes(query.value.toLowerCase())
  );

  const handleSelect = (cmd) => {
    closePalette();
    if (cmd.path && onNavigate) {
      onNavigate(cmd.path);
    }
  };

  const handleKeyDown = (e) => {
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      selectedIdx.value = Math.min(selectedIdx.value + 1, filtered.length - 1);
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      selectedIdx.value = Math.max(selectedIdx.value - 1, 0);
    } else if (e.key === 'Enter' && filtered[selectedIdx.value]) {
      handleSelect(filtered[selectedIdx.value]);
    }
  };

  return (
    <div
      onClick={() => closePalette()}
      style={{
        position: 'fixed',
        inset: 0,
        background: 'rgba(0,0,0,0.6)',
        backdropFilter: 'blur(4px)',
        display: 'flex',
        alignItems: 'flex-start',
        justifyContent: 'center',
        paddingTop: '20vh',
        zIndex: 9999,
      }}
    >
      <div
        onClick={e => e.stopPropagation()}
        style={{
          width: 480,
          maxWidth: '90vw',
          background: 'var(--bg-1)',
          border: '1px solid var(--border-hover)',
          borderRadius: 'var(--radius-xl)',
          overflow: 'hidden',
          boxShadow: '0 16px 64px rgba(0,0,0,0.5)',
        }}
      >
        <div style={{ padding: '12px 16px', borderBottom: '1px solid var(--border)' }}>
          <input
            ref={inputRef}
            type="text"
            placeholder="Type a command..."
            value={query.value}
            onInput={e => { query.value = e.target.value; selectedIdx.value = 0; }}
            onKeyDown={handleKeyDown}
            style={{
              width: '100%',
              fontSize: 14,
              color: 'var(--text-primary)',
              background: 'transparent',
            }}
          />
        </div>
        <div style={{ maxHeight: 320, overflowY: 'auto', padding: '4px 0' }}>
          {filtered.length === 0 && (
            <div style={{ padding: '16px', color: 'var(--text-tertiary)', textAlign: 'center', fontSize: 13 }}>
              No results
            </div>
          )}
          {filtered.map((cmd, i) => (
            <button
              key={cmd.id}
              onClick={() => handleSelect(cmd)}
              style={{
                display: 'flex',
                alignItems: 'center',
                gap: 10,
                width: '100%',
                padding: '8px 16px',
                fontSize: 13,
                color: i === selectedIdx.value ? 'var(--text-primary)' : 'var(--text-secondary)',
                background: i === selectedIdx.value ? 'var(--bg-2)' : 'transparent',
                textAlign: 'left',
                cursor: 'pointer',
                transition: 'var(--transition-fast)',
              }}
            >
              <span style={{ color: 'var(--text-tertiary)', fontSize: 11, fontFamily: 'var(--font-mono)' }}>
                {cmd.section}
              </span>
              <span>{cmd.label}</span>
            </button>
          ))}
        </div>
      </div>
    </div>
  );
}
