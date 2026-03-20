const colors = {
  active: 'var(--status-green)',
  healthy: 'var(--status-green)',
  ok: 'var(--status-green)',
  cooldown: 'var(--status-yellow)',
  warning: 'var(--status-yellow)',
  error: 'var(--status-red)',
  offline: 'var(--status-red)',
  disabled: 'var(--text-tertiary)',
};

export function StatusDot({ status = 'active', size = 8, pulse = false }) {
  const color = colors[status] || colors.active;
  return (
    <span
      title={status}
      style={{
        display: 'inline-block',
        width: size,
        height: size,
        borderRadius: '50%',
        backgroundColor: color,
        flexShrink: 0,
        boxShadow: pulse ? `0 0 6px ${color}` : 'none',
        animation: pulse ? 'pulse 2s ease-in-out infinite' : 'none',
      }}
    />
  );
}
