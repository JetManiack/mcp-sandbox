import { useCallback, useEffect, useState } from 'react';
import { formatTime } from '../format.js';

const LIMIT = 100;

function durationLabel(ms) {
  if (ms === null || ms === undefined) return '—';
  if (ms < 1000) return ms + ' ms';
  return (ms / 1000).toFixed(1) + ' s';
}

function IsErrorPill({ isError }) {
  if (isError) return <span className="status-badge error">error</span>;
  return <span className="status-badge ok">ok</span>;
}

function RowDetail({ row }) {
  return (
    <tr className="tc-detail">
      <td colSpan={6}>
        <div className="tc-detail-inner">
          {row.input_json ? (
            <div>
              <span className="tc-label">Input</span>
              <pre className="tc-json">{row.input_json}</pre>
            </div>
          ) : null}
          {row.output_json ? (
            <div>
              <span className="tc-label">Output / error</span>
              <pre className="tc-json">{row.output_json}</pre>
            </div>
          ) : null}
          {!row.input_json && !row.output_json ? (
            <span className="metric">No detail captured for this call.</span>
          ) : null}
        </div>
      </td>
    </tr>
  );
}

export default function History() {
  const [rows, setRows] = useState(null);
  const [error, setError] = useState(null);
  const [expanded, setExpanded] = useState(null);

  const load = useCallback(() => {
    fetch('/api/tool-calls?limit=' + LIMIT)
      .then((res) => {
        if (!res.ok) throw new Error('failed to load tool calls (' + res.status + ')');
        return res.json();
      })
      .then((data) => {
        setRows(data || []);
        setError(null);
      })
      .catch((err) => setError(String(err)));
  }, []);

  useEffect(load, [load]);

  function toggle(id) {
    setExpanded((current) => (current === id ? null : id));
  }

  if (error) {
    return (
      <div>
        <h2 className="section-title">Tool call history</h2>
        <div className="callout">{error}</div>
        <button type="button" onClick={load}>
          Retry
        </button>
      </div>
    );
  }

  if (rows === null) return <div className="empty-state">Loading…</div>;

  return (
    <div>
      <h2 className="section-title">Tool call history</h2>
      <p className="metric">Showing the {LIMIT} most recent finished calls.</p>

      {rows.length === 0 ? (
        <div className="empty-state">No tool calls recorded yet.</div>
      ) : (
        <div className="tc-scroll">
          <table className="tc-table">
            <thead>
              <tr>
                <th>Called at</th>
                <th>Actor</th>
                <th>Tool</th>
                <th>Duration</th>
                <th>Exit</th>
                <th>Status</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((row) => [
                <tr
                  key={row.id}
                  className={'tc-row' + (expanded === row.id ? ' tc-row-open' : '')}
                  onClick={() => toggle(row.id)}
                >
                  <td className="metric">{formatTime(row.called_at)}</td>
                  <td>{row.actor_name || row.actor_id}</td>
                  <td className="tc-tool">{row.tool}</td>
                  <td className="metric">{durationLabel(row.duration_ms)}</td>
                  <td className="metric">{row.exit_code !== null && row.exit_code !== undefined ? row.exit_code : '—'}</td>
                  <td>
                    <IsErrorPill isError={row.is_error} />
                  </td>
                </tr>,
                expanded === row.id ? <RowDetail key={row.id + '-detail'} row={row} /> : null,
              ])}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
