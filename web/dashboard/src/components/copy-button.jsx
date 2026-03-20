import { signal } from '@preact/signals';

export function CopyButton({ text, label = 'Copy', style: extraStyle = {} }) {
  const copied = signal(false);

  const handleCopy = async () => {
    try {
      await navigator.clipboard.writeText(text);
      copied.value = true;
      setTimeout(() => { copied.value = false; }, 1500);
    } catch {
      // Fallback
      const ta = document.createElement('textarea');
      ta.value = text;
      ta.style.position = 'fixed';
      ta.style.opacity = '0';
      document.body.appendChild(ta);
      ta.select();
      document.execCommand('copy');
      document.body.removeChild(ta);
      copied.value = true;
      setTimeout(() => { copied.value = false; }, 1500);
    }
  };

  return (
    <button
      onClick={handleCopy}
      style={{
        display: 'inline-flex',
        alignItems: 'center',
        gap: 6,
        padding: '4px 10px',
        fontSize: 12,
        color: copied.value ? 'var(--status-green)' : 'var(--text-secondary)',
        background: 'var(--bg-2)',
        border: '1px solid var(--border)',
        borderRadius: 'var(--radius-sm)',
        cursor: 'pointer',
        transition: 'var(--transition-fast)',
        ...extraStyle,
      }}
    >
      {copied.value ? (
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="20 6 9 17 4 12" /></svg>
      ) : (
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="9" y="9" width="13" height="13" rx="2" /><path d="M5 15H4a2 2 0 01-2-2V4a2 2 0 012-2h9a2 2 0 012 2v1" /></svg>
      )}
      {label}
    </button>
  );
}
