import { useCallback, useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { formatTime } from '../format.js';

export default function Agents() {
  const [agents, setAgents] = useState(null);
  const [tokens, setTokens] = useState({});
  const [issued, setIssued] = useState(null);
  const [name, setName] = useState('');
  const [error, setError] = useState(null);
  const navigate = useNavigate();

  const load = useCallback(() => {
    fetch('/api/actors')
      .then((res) => {
        if (!res.ok) throw new Error('failed to load agents (' + res.status + ')');
        return res.json();
      })
      .then((data) => {
        setAgents(data || []);
        setError(null);
      })
      .catch((err) => setError(String(err)));
  }, []);

  useEffect(load, [load]);

  function failed(res, fallback) {
    return res
      .json()
      .catch(() => ({}))
      .then((body) => {
        throw new Error(body.error || fallback + ' (' + res.status + ')');
      });
  }

  function createAgent() {
    fetch('/api/actors', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ display_name: name }),
    })
      .then((res) => (res.ok ? res.json() : failed(res, 'could not create agent')))
      .then(() => {
        setName('');
        load();
      })
      .catch((err) => setError(String(err)));
  }

  function issueToken(agentId) {
    fetch('/api/actors/' + encodeURIComponent(agentId) + '/credentials', { method: 'POST' })
      .then((res) => (res.ok ? res.json() : failed(res, 'could not issue token')))
      .then((body) => {
        setIssued({ agentId, token: body.token });
        loadTokens(agentId);
        load();
      })
      .catch((err) => setError(String(err)));
  }

  function loadTokens(agentId) {
    fetch('/api/actors/' + encodeURIComponent(agentId) + '/credentials')
      .then((res) => (res.ok ? res.json() : failed(res, 'could not load tokens')))
      .then((list) => setTokens((current) => ({ ...current, [agentId]: list })))
      .catch((err) => setError(String(err)));
  }

  function revokeToken(agentId, tokenId) {
    fetch('/api/credentials/' + encodeURIComponent(tokenId), {
      method: 'DELETE',
    })
      .then((res) => {
        if (!res.ok) return failed(res, 'could not revoke token');
        loadTokens(agentId);
        load();
      })
      .catch((err) => setError(String(err)));
  }

  if (error) {
    return (
      <div>
        <h2 className="section-title">Agents</h2>
        <div className="callout">{error}</div>
        <button type="button" onClick={load}>
          Retry
        </button>
      </div>
    );
  }
  if (agents === null) return <div className="empty-state">Loading…</div>;

  return (
    <div>
      <h2 className="section-title">Agents</h2>

      {issued ? (
        <div className="transmission">
          <span className="label">New token — copy it now, it is not shown again</span>
          <code>{issued.token}</code>
          <button type="button" onClick={() => setIssued(null)}>
            Dismiss
          </button>
        </div>
      ) : null}

      <div className="dispatch-bar">
        <input
          type="text"
          placeholder="Agent name"
          value={name}
          onChange={(event) => setName(event.target.value)}
        />
        <button type="button" className="primary" onClick={createAgent} disabled={name.trim() === ''}>
          Register agent
        </button>
      </div>

      {agents.length === 0 ? (
        <div className="empty-state">No agents yet.</div>
      ) : (
        <div className="agent-grid">
          {agents.map((agent) => (
            <div className="agent-card" key={agent.id}>
              <div className="beacon-row">
                <span
                  className={'beacon' + (agent.has_active_token ? ' active' : '')}
                  aria-hidden="true"
                />
                <span className="name">{agent.display_name}</span>
                <span className={'status-label' + (agent.has_active_token ? ' active' : '')}>
                  {agent.has_active_token ? 'has token' : 'no token'}
                </span>
              </div>
              <span className="agent-id">{agent.id}</span>

              <div className="actions">
                <button type="button" onClick={() => issueToken(agent.id)}>
                  Issue token
                </button>
                <button type="button" onClick={() => loadTokens(agent.id)}>
                  Show tokens
                </button>
                <button type="button" className="metric" onClick={() => navigate('/sandbox/' + agent.id)}>
                  Open terminal
                </button>
              </div>

              {tokens[agent.id] ? (
                <ul className="token-list">
                  {tokens[agent.id].length === 0 ? <li>no tokens issued</li> : null}
                  {tokens[agent.id].map((cred) => (
                    <li key={cred.id}>
                      <span>{cred.label ? cred.label + ' — ' : ''}{formatTime(cred.created_at)}</span>
                      {cred.revoked_at ? (
                        <span className="revoked">revoked</span>
                      ) : (
                        <button type="button" onClick={() => revokeToken(agent.id, cred.id)}>
                          Revoke
                        </button>
                      )}
                    </li>
                  ))}
                </ul>
              ) : null}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
