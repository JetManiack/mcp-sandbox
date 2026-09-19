import { useCallback, useEffect, useState } from 'react';
import { useParams } from 'react-router-dom';
import Terminal from '../../components/Terminal.jsx';
import { formatTime } from '../../format.js';

const REFRESH_MS = 5000;

function SandboxList() {
  const [rows, setRows] = useState(null);
  const [error, setError] = useState(null);

  const load = useCallback(() => {
    fetch('/api/sandboxes')
      .then((res) => {
        if (!res.ok) throw new Error('failed to load sandboxes (' + res.status + ')');
        return res.json();
      })
      .then((data) => {
        setRows(data || []);
        setError(null);
      })
      .catch((err) => setError(String(err)));
  }, []);

  useEffect(() => {
    load();
    const timer = window.setInterval(load, REFRESH_MS);
    return () => window.clearInterval(timer);
  }, [load]);

  if (error) return <div className="callout">{error}</div>;
  if (rows === null) return <div className="empty-state">Loading…</div>;

  return (
    <div>
      <h2 className="section-title">Sandboxes</h2>

      {rows.length === 0 ? (
        <div className="empty-state">
          No agents registered yet. Create one under Agents to give it a sandbox.
        </div>
      ) : (
        <div className="sandbox-grid">
          {rows.map((row) => (
            <a className="sandbox-card" key={row.actor_id} href={'/sandbox/' + row.actor_id}>
              <div className="beacon-row">
                <span
                  className={'beacon' + (row.running_commands > 0 ? ' active' : '')}
                  aria-hidden="true"
                />
                <span className="name">{row.display_name}</span>
              </div>

              {row.block ? (
                <span className="status-badge error">blocked</span>
              ) : (
                <span className="status-badge ok">{row.live ? 'live' : 'idle'}</span>
              )}

              <span className="metric">
                {row.running_commands} running · {row.watchers} watching
              </span>

              {row.block ? (
                <span className="block-summary">
                  {row.block.reason} — {row.block.blocked_by_name},{' '}
                  {formatTime(row.block.blocked_at)}
                </span>
              ) : null}

              {!row.live && !row.block ? (
                <span className="metric dim">no sandbox yet this run</span>
              ) : null}
            </a>
          ))}
        </div>
      )}
    </div>
  );
}

function SandboxTerminal() {
  const { id } = useParams();
  // role is fetched from /api/me by the Shell/App — here we read from a context
  // or fall back to 'viewer' safely. The terminal itself asks its own /api/sandboxes/:id.
  const [role, setRole] = useState('viewer');
  useEffect(() => {
    fetch('/api/me')
      .then((r) => r.json())
      .then((u) => setRole(u.role || 'viewer'))
      .catch(() => {});
  }, []);
  return <Terminal sandboxId={id} role={role} />;
}

// SandboxPage renders either the sandbox list or a single terminal, depending
// on whether the :id param is present. The router wires up both routes.
export default function SandboxPage() {
  const { id } = useParams();
  return id ? <SandboxTerminal /> : <SandboxList />;
}
