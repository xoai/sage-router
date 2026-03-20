import { useState, useEffect, useCallback } from "react";

// ── Design Tokens ──
const tokens = {
  bg: { base: "#0a0a0b", surface: "#111113", raised: "#1a1a1e", overlay: "#232328" },
  text: { primary: "#ededef", secondary: "#8b8b8e", tertiary: "#5c5c60" },
  accent: { base: "#3b82f6", hover: "#2563eb", subtle: "rgba(59,130,246,0.06)" },
  status: { active: "#22c55e", warning: "#eab308", error: "#ef4444" },
  border: "rgba(255,255,255,0.04)",
  borderHover: "rgba(255,255,255,0.08)",
  font: { mono: "'JetBrains Mono', 'SF Mono', monospace", sans: "system-ui, -apple-system, sans-serif" },
  radius: "8px",
};

const StatusDot = ({ status }) => {
  const color = status === "active" ? tokens.status.active : status === "cooldown" ? tokens.status.warning : tokens.status.error;
  return (
    <span style={{ width: 8, height: 8, borderRadius: "50%", background: color, display: "inline-block", flexShrink: 0, boxShadow: `0 0 6px ${color}40` }} />
  );
};

const CopyButton = ({ text }) => {
  const [copied, setCopied] = useState(false);
  return (
    <button onClick={() => { navigator.clipboard?.writeText(text); setCopied(true); setTimeout(() => setCopied(false), 1500); }}
      style={{ background: "none", border: `1px solid ${tokens.border}`, color: copied ? tokens.status.active : tokens.text.tertiary, padding: "2px 8px", borderRadius: 4, cursor: "pointer", fontFamily: tokens.font.mono, fontSize: 11, transition: "all 0.15s" }}>
      {copied ? "✓ Copied" : "Copy"}
    </button>
  );
};

const Sparkline = ({ data, color = tokens.accent.base }) => {
  const max = Math.max(...data, 1);
  const w = 120, h = 28;
  const points = data.map((v, i) => `${(i / (data.length - 1)) * w},${h - (v / max) * h}`).join(" ");
  return (
    <svg width={w} height={h} style={{ display: "block" }}>
      <polyline points={points} fill="none" stroke={color} strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" />
      <polyline points={`0,${h} ${points} ${w},${h}`} fill={`${color}10`} stroke="none" />
    </svg>
  );
};

// ── Mock Data ──
const mockProviders = [
  { name: "Claude Code", accounts: [
    { email: "user@gmail.com", status: "active", usedToday: 3, limitToday: 5, lastUsed: "2min ago", priority: 1, auth: "OAuth" },
    { email: "work@company.com", status: "cooldown", cooldownSec: 90, model: "sonnet-4", priority: 2, auth: "OAuth" },
  ]},
  { name: "OpenAI", accounts: [
    { email: "Production Key", status: "active", costToday: "$2.41", key: "sk-...7xQ", priority: 1, auth: "API Key" },
  ]},
  { name: "Gemini CLI", accounts: [
    { email: "dev@gmail.com", status: "active", lastUsed: "5min ago", priority: 1, auth: "OAuth" },
  ]},
  { name: "iFlow", accounts: [
    { email: "Free Account", status: "active", lastUsed: "12min ago", priority: 1, auth: "OAuth" },
  ]},
];

const recentRequests = [
  { time: "2s ago", model: "cc/sonnet-4", status: "ok", latency: "1.2s", tokens: "3.2K", cost: "$0.012" },
  { time: "15s ago", model: "openai/gpt-4o", status: "ok", latency: "0.8s", tokens: "1.1K", cost: "$0.003" },
  { time: "32s ago", model: "cc/sonnet-4", status: "fallback", latency: "2.1s", tokens: "-", cost: "→ ag" },
  { time: "1m ago", model: "combo/fast", status: "ok", latency: "0.6s", tokens: "0.8K", cost: "$0.001" },
  { time: "2m ago", model: "ag/sonnet-4", status: "ok", latency: "1.8s", tokens: "2.4K", cost: "Free" },
];

const sparkData = [2,3,5,8,12,15,18,14,10,7,4,2,1,1,2,4,8,13,18,22,19,15,10,7];

const pages = ["Overview", "Providers", "Models", "Usage", "Settings", "Connect"];
const pageIcons = ["⚡", "🔌", "🔀", "📊", "⚙", "📋"];

// ── Countdown Hook ──
function useCountdown(initial) {
  const [sec, setSec] = useState(initial);
  useEffect(() => {
    if (sec <= 0) return;
    const t = setInterval(() => setSec(s => Math.max(0, s - 1)), 1000);
    return () => clearInterval(t);
  }, [sec > 0]);
  return sec;
}

const CooldownTimer = ({ seconds }) => {
  const remaining = useCountdown(seconds);
  const m = Math.floor(remaining / 60);
  const s = remaining % 60;
  return <span style={{ fontFamily: tokens.font.mono, fontSize: 12, color: tokens.status.warning }}>{m}m {String(s).padStart(2, "0")}s</span>;
};

// ── Command Palette ──
const CommandPalette = ({ open, onClose }) => {
  const [query, setQuery] = useState("");
  const commands = ["Add provider", "Create combo", "Copy API key", "Test all connections", "Go to Usage", "Go to Settings", "Export database"];
  const filtered = commands.filter(c => c.toLowerCase().includes(query.toLowerCase()));

  if (!open) return null;
  return (
    <div style={{ position: "fixed", inset: 0, background: "rgba(0,0,0,0.6)", zIndex: 100, display: "flex", justifyContent: "center", paddingTop: 120, backdropFilter: "blur(4px)" }} onClick={onClose}>
      <div onClick={e => e.stopPropagation()} style={{ width: 480, background: tokens.bg.surface, border: `1px solid ${tokens.borderHover}`, borderRadius: 12, overflow: "hidden", boxShadow: "0 24px 48px rgba(0,0,0,0.4)", maxHeight: 400 }}>
        <div style={{ padding: "12px 16px", borderBottom: `1px solid ${tokens.border}` }}>
          <input autoFocus value={query} onChange={e => setQuery(e.target.value)} placeholder="Type a command..." style={{ width: "100%", background: "none", border: "none", color: tokens.text.primary, fontSize: 14, fontFamily: tokens.font.sans, outline: "none" }} />
        </div>
        <div style={{ padding: 8, maxHeight: 300, overflow: "auto" }}>
          {filtered.map((cmd, i) => (
            <div key={cmd} style={{ padding: "8px 12px", borderRadius: 6, cursor: "pointer", color: i === 0 ? tokens.text.primary : tokens.text.secondary, background: i === 0 ? tokens.accent.subtle : "none", fontSize: 13, fontFamily: tokens.font.sans, display: "flex", alignItems: "center", gap: 8 }}
              onMouseEnter={e => { e.currentTarget.style.background = tokens.accent.subtle; }}
              onMouseLeave={e => { if (i !== 0) e.currentTarget.style.background = "none"; }}>
              <span style={{ color: tokens.text.tertiary }}>→</span> {cmd}
            </div>
          ))}
        </div>
      </div>
    </div>
  );
};

// ── Provider Card ──
const ProviderCard = ({ provider }) => (
  <div style={{ background: tokens.bg.surface, border: `1px solid ${tokens.border}`, borderRadius: tokens.radius, padding: 16, marginBottom: 8, transition: "border-color 0.15s" }}
    onMouseEnter={e => e.currentTarget.style.borderColor = tokens.borderHover}
    onMouseLeave={e => e.currentTarget.style.borderColor = tokens.border}>
    <div style={{ fontSize: 13, fontWeight: 600, color: tokens.text.primary, marginBottom: 10, fontFamily: tokens.font.sans, display: "flex", alignItems: "center", gap: 8 }}>
      {provider.name}
      <span style={{ fontSize: 11, color: tokens.text.tertiary, fontWeight: 400 }}>{provider.accounts.length} account{provider.accounts.length > 1 ? "s" : ""}</span>
    </div>
    {provider.accounts.map((acc, i) => (
      <div key={i} style={{ display: "flex", alignItems: "center", gap: 10, padding: "6px 0", borderTop: i > 0 ? `1px solid ${tokens.border}` : "none", marginTop: i > 0 ? 6 : 0 }}>
        <StatusDot status={acc.status} />
        <div style={{ flex: 1, minWidth: 0 }}>
          <div style={{ fontSize: 13, color: tokens.text.primary, fontFamily: tokens.font.mono, whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis" }}>{acc.email}</div>
          <div style={{ fontSize: 11, color: tokens.text.tertiary, marginTop: 2, fontFamily: tokens.font.sans }}>
            Priority {acc.priority} · {acc.auth}{acc.lastUsed ? ` · ${acc.lastUsed}` : ""}
          </div>
        </div>
        <div style={{ display: "flex", alignItems: "center", gap: 8, flexShrink: 0 }}>
          {acc.status === "cooldown" && <CooldownTimer seconds={acc.cooldownSec} />}
          {acc.status === "active" && acc.usedToday !== undefined && (
            <span style={{ fontSize: 11, fontFamily: tokens.font.mono, color: tokens.text.secondary }}>{acc.usedToday}/{acc.limitToday} today</span>
          )}
          {acc.costToday && <span style={{ fontSize: 11, fontFamily: tokens.font.mono, color: tokens.text.secondary }}>{acc.costToday}</span>}
          <button style={{ background: tokens.bg.raised, border: `1px solid ${tokens.border}`, color: tokens.text.secondary, padding: "3px 10px", borderRadius: 4, cursor: "pointer", fontSize: 11, fontFamily: tokens.font.sans }}>Test</button>
        </div>
      </div>
    ))}
  </div>
);

// ── Overview Page ──
const OverviewPage = () => (
  <div>
    <div style={{ display: "grid", gridTemplateColumns: "repeat(3, 1fr)", gap: 12, marginBottom: 20 }}>
      {[
        { label: "Status", value: <><StatusDot status="active" /> <span>5 active</span> <span style={{ color: tokens.status.warning, marginLeft: 8 }}>⚠ 1 cooldown</span></>, },
        { label: "Today", value: "142 requests · $1.24 · 98.6% success" },
        { label: "This Month", value: "3,847 requests · $18.72 · 97.2% success" },
      ].map((stat, i) => (
        <div key={i} style={{ background: tokens.bg.surface, border: `1px solid ${tokens.border}`, borderRadius: tokens.radius, padding: 14 }}>
          <div style={{ fontSize: 11, color: tokens.text.tertiary, marginBottom: 6, fontFamily: tokens.font.sans, textTransform: "uppercase", letterSpacing: 0.5 }}>{stat.label}</div>
          <div style={{ fontSize: 13, color: tokens.text.primary, fontFamily: stat.label === "Status" ? tokens.font.sans : tokens.font.mono, display: "flex", alignItems: "center", gap: 6 }}>{stat.value}</div>
        </div>
      ))}
    </div>
    <div style={{ background: tokens.bg.surface, border: `1px solid ${tokens.border}`, borderRadius: tokens.radius, padding: 16, marginBottom: 16 }}>
      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", marginBottom: 10 }}>
        <span style={{ fontSize: 13, color: tokens.text.secondary, fontFamily: tokens.font.sans }}>Requests (24h)</span>
        <Sparkline data={sparkData} />
      </div>
      <div style={{ display: "flex", justifyContent: "space-between", fontSize: 10, color: tokens.text.tertiary, fontFamily: tokens.font.mono }}>
        <span>12am</span><span>6am</span><span>12pm</span><span>6pm</span><span>now</span>
      </div>
    </div>
    <div style={{ background: tokens.bg.surface, border: `1px solid ${tokens.border}`, borderRadius: tokens.radius, padding: 16 }}>
      <div style={{ fontSize: 12, color: tokens.text.tertiary, marginBottom: 10, fontFamily: tokens.font.sans, textTransform: "uppercase", letterSpacing: 0.5 }}>Recent Requests</div>
      {recentRequests.map((req, i) => (
        <div key={i} style={{ display: "flex", alignItems: "center", gap: 12, padding: "5px 0", borderTop: i > 0 ? `1px solid ${tokens.border}` : "none", fontSize: 12, fontFamily: tokens.font.mono }}>
          <span style={{ width: 52, color: tokens.text.tertiary, flexShrink: 0 }}>{req.time}</span>
          <span style={{ width: 130, color: tokens.text.primary, flexShrink: 0 }}>{req.model}</span>
          <span style={{ width: 16 }}>{req.status === "ok" ? <span style={{ color: tokens.status.active }}>✓</span> : <span style={{ color: tokens.status.warning }}>⚠</span>}</span>
          <span style={{ width: 40, color: tokens.text.secondary }}>{req.latency}</span>
          <span style={{ width: 48, color: tokens.text.secondary, textAlign: "right" }}>{req.tokens}</span>
          <span style={{ flex: 1, color: req.cost === "Free" ? tokens.status.active : tokens.text.secondary, textAlign: "right" }}>{req.cost}</span>
        </div>
      ))}
    </div>
  </div>
);

// ── Providers Page ──
const ProvidersPage = () => (
  <div>
    <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", marginBottom: 16 }}>
      <div style={{ fontSize: 12, color: tokens.text.tertiary, fontFamily: tokens.font.sans }}>Drag to reorder priority. Higher = tried first.</div>
      <button style={{ background: tokens.accent.base, color: "#fff", border: "none", padding: "6px 14px", borderRadius: 6, cursor: "pointer", fontSize: 12, fontFamily: tokens.font.sans, fontWeight: 500 }}>+ Add Provider</button>
    </div>
    {mockProviders.map((p, i) => <ProviderCard key={i} provider={p} />)}
  </div>
);

// ── Connect Page ──
const ConnectPage = () => (
  <div>
    <div style={{ background: tokens.bg.surface, border: `1px solid ${tokens.border}`, borderRadius: tokens.radius, padding: 16, marginBottom: 12 }}>
      <div style={{ fontSize: 13, color: tokens.text.secondary, marginBottom: 12, fontFamily: tokens.font.sans }}>Your API endpoint</div>
      <div style={{ display: "flex", gap: 8, alignItems: "center" }}>
        <code style={{ background: tokens.bg.raised, padding: "8px 12px", borderRadius: 6, fontFamily: tokens.font.mono, fontSize: 13, color: tokens.accent.base, flex: 1, border: `1px solid ${tokens.border}` }}>http://localhost:20128/v1</code>
        <CopyButton text="http://localhost:20128/v1" />
      </div>
    </div>
    <div style={{ background: tokens.bg.surface, border: `1px solid ${tokens.border}`, borderRadius: tokens.radius, padding: 16, marginBottom: 12 }}>
      <div style={{ fontSize: 13, color: tokens.text.secondary, marginBottom: 12, fontFamily: tokens.font.sans }}>API Key</div>
      <div style={{ display: "flex", gap: 8, alignItems: "center" }}>
        <code style={{ background: tokens.bg.raised, padding: "8px 12px", borderRadius: 6, fontFamily: tokens.font.mono, fontSize: 13, color: tokens.text.primary, flex: 1, border: `1px solid ${tokens.border}` }}>sk-a8f2c1d9-xk7m2p-9e3b4a71</code>
        <CopyButton text="sk-a8f2c1d9-xk7m2p-9e3b4a71" />
      </div>
    </div>
    {[
      { tool: "Claude Code", cmd: "claude config set --api-base http://localhost:20128/v1 --api-key sk-a8f2c1d9-xk7m2p-9e3b4a71" },
      { tool: "Codex CLI", cmd: "export OPENAI_BASE_URL=http://localhost:20128/v1\nexport OPENAI_API_KEY=sk-a8f2c1d9-xk7m2p-9e3b4a71" },
      { tool: "Cursor", cmd: "Settings → Models → OpenAI API Base → http://localhost:20128/v1" },
    ].map((item, i) => (
      <div key={i} style={{ background: tokens.bg.surface, border: `1px solid ${tokens.border}`, borderRadius: tokens.radius, padding: 14, marginBottom: 8 }}>
        <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", marginBottom: 8 }}>
          <span style={{ fontSize: 13, fontWeight: 500, color: tokens.text.primary, fontFamily: tokens.font.sans }}>{item.tool}</span>
          <CopyButton text={item.cmd} />
        </div>
        <pre style={{ margin: 0, padding: 10, background: tokens.bg.base, borderRadius: 6, fontFamily: tokens.font.mono, fontSize: 11, color: tokens.text.secondary, whiteSpace: "pre-wrap", lineHeight: 1.6, border: `1px solid ${tokens.border}` }}>{item.cmd}</pre>
      </div>
    ))}
  </div>
);

// ── Main App ──
export default function SageRouterDashboard() {
  const [activePage, setActivePage] = useState(0);
  const [cmdOpen, setCmdOpen] = useState(false);
  const [sidebarHover, setSidebarHover] = useState(-1);

  useEffect(() => {
    const handler = (e) => { if ((e.metaKey || e.ctrlKey) && e.key === "k") { e.preventDefault(); setCmdOpen(o => !o); } if (e.key === "Escape") setCmdOpen(false); };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, []);

  const pageContent = activePage === 0 ? <OverviewPage /> : activePage === 1 ? <ProvidersPage /> : activePage === 5 ? <ConnectPage /> : (
    <div style={{ color: tokens.text.tertiary, fontSize: 13, fontFamily: tokens.font.sans, padding: 40, textAlign: "center" }}>
      {pages[activePage]} page — click Overview, Providers, or Connect to see live demos
    </div>
  );

  return (
    <div style={{ display: "flex", height: "100vh", background: tokens.bg.base, color: tokens.text.primary, fontFamily: tokens.font.sans, overflow: "hidden" }}>
      {/* Sidebar */}
      <div style={{ width: 200, background: tokens.bg.surface, borderRight: `1px solid ${tokens.border}`, display: "flex", flexDirection: "column", flexShrink: 0 }}>
        <div style={{ padding: "16px 14px", borderBottom: `1px solid ${tokens.border}`, display: "flex", alignItems: "center", gap: 8 }}>
          <div style={{ width: 22, height: 22, borderRadius: 6, background: `linear-gradient(135deg, ${tokens.accent.base}, #8b5cf6)`, display: "flex", alignItems: "center", justifyContent: "center", fontSize: 11, fontWeight: 700, color: "#fff" }}>S</div>
          <span style={{ fontSize: 13, fontWeight: 600, letterSpacing: -0.3 }}>Sage Router</span>
          <span style={{ fontSize: 10, color: tokens.text.tertiary, marginLeft: "auto", fontFamily: tokens.font.mono }}>v1.0</span>
        </div>
        <nav style={{ padding: "8px 6px", flex: 1 }}>
          {pages.map((page, i) => (
            <div key={page} onClick={() => setActivePage(i)}
              onMouseEnter={() => setSidebarHover(i)} onMouseLeave={() => setSidebarHover(-1)}
              style={{ padding: "7px 10px", borderRadius: 6, cursor: "pointer", fontSize: 13, display: "flex", alignItems: "center", gap: 8, marginBottom: 1, transition: "all 0.1s",
                background: activePage === i ? tokens.accent.subtle : sidebarHover === i ? "rgba(255,255,255,0.02)" : "none",
                color: activePage === i ? tokens.text.primary : tokens.text.secondary }}>
              <span style={{ fontSize: 13, width: 20, textAlign: "center" }}>{pageIcons[i]}</span>
              {page}
            </div>
          ))}
        </nav>
        <div style={{ padding: "10px 14px", borderTop: `1px solid ${tokens.border}`, fontSize: 11, color: tokens.text.tertiary }}>
          <span style={{ fontFamily: tokens.font.mono, background: tokens.bg.raised, padding: "2px 5px", borderRadius: 3, marginRight: 4 }}>⌘K</span> Command palette
        </div>
      </div>

      {/* Main content */}
      <div style={{ flex: 1, overflow: "auto" }}>
        <div style={{ padding: "20px 28px", maxWidth: 800 }}>
          <h1 style={{ fontSize: 18, fontWeight: 600, marginBottom: 20, letterSpacing: -0.3, display: "flex", alignItems: "center", gap: 10 }}>
            {pageIcons[activePage]} {pages[activePage]}
          </h1>
          {pageContent}
        </div>
      </div>

      <CommandPalette open={cmdOpen} onClose={() => setCmdOpen(false)} />
    </div>
  );
}
