import { useCallback, useEffect, useRef, useState } from 'react';
import { Link } from 'react-router-dom';
import { formatClock, formatDuration, formatTime, stripANSI } from '../format.js';

// MAX_LINES bounds the DOM, not the stream. The server retains a fixed number of
// bytes, but a live watcher receives output for as long as it stays connected, so
// without a cap a chatty command grows the page until the tab dies. Oldest lines
// go first, which is also the order they stop mattering in.
const MAX_LINES = 2000;

// Close code the server uses for a watcher that fell behind. It is not an error
// to show the operator: reconnecting from the last sequence number recovers, and
// the resulting gap is reported by the server itself.
const CLOSE_SLOW_CONSUMER = 4000;

const RECONNECT_DELAY_MS = 1000;

// appendChunk merges consecutive output of the same stream into one item rather
// than one per WebSocket frame: a command printing a thousand short lines would
// otherwise create a thousand DOM nodes for what is visually one block of text.
function appendChunk(items, event) {
  const text = stripANSI(event.data || '');
  if (!text) return items;

  const last = items[items.length - 1];
  if (last && last.kind === event.kind && last.execId === event.exec_id) {
    const merged = items.slice(0, -1);
    merged.push({ ...last, text: last.text + text, seq: event.seq });
    return merged;
  }
  return items.concat({
    key: 'chunk-' + event.seq,
    kind: event.kind,
    execId: event.exec_id,
    text,
    seq: event.seq,
  });
}

function reduceEvent(items, event) {
  switch (event.kind) {
    case 'stdout':
    case 'stderr':
      return appendChunk(items, event);
    case 'started':
      return items.concat({
        key: 'started-' + event.seq,
        kind: 'started',
        execId: event.exec_id,
        command: event.command,
        workDir: event.work_dir,
        at: event.at,
        seq: event.seq,
      });
    case 'finished':
      return items.concat({
        key: 'finished-' + event.seq,
        kind: 'finished',
        execId: event.exec_id,
        exitCode: event.exit_code || 0,
        durationMs: event.duration_ms || 0,
        truncated: event.truncated,
        reason: event.reason,
        seq: event.seq,
      });
    case 'killed':
    case 'blocked':
    case 'released':
    case 'gap':
      return items.concat({
        key: event.kind + '-' + event.seq + '-' + (event.at || ''),
        kind: event.kind,
        byActor: event.by_actor,
        reason: event.reason,
        missed: event.missed_events,
        at: event.at,
        seq: event.seq,
      });
    default:
      return items;
  }
}

function trim(items) {
  return items.length > MAX_LINES ? items.slice(items.length - MAX_LINES) : items;
}

function Line({ item }) {
  switch (item.kind) {
    case 'started':
      return (
        <div className="term-command">
          {/* The rendered argument vector, quoted by the server where an
              argument contains whitespace — there is no shell, so the argument
              boundaries are what a reader needs to see. */}
          <span className="term-prompt">$</span>{' '}
          <span className="term-command-text">{item.command}</span>
          {item.workDir ? <span className="term-workdir"> (in {item.workDir})</span> : null}
          <span className="term-clock">{formatClock(item.at)}</span>
        </div>
      );
    case 'finished':
      return (
        <div className={'term-exit' + (item.exitCode === 0 ? ' ok' : ' fail')}>
          exit {item.exitCode} in {formatDuration(item.durationMs)}
          {item.truncated ? ' · output truncated in the tool result' : ''}
          {item.reason ? ' · ' + item.reason : ''}
        </div>
      );
    case 'stderr':
      return <pre className="term-out stderr">{item.text}</pre>;
    case 'stdout':
      return <pre className="term-out">{item.text}</pre>;
    case 'killed':
      return (
        <div className="term-notice killed">
          processes killed by {item.byActor}
          {item.reason ? ': ' + item.reason : ''}
        </div>
      );
    case 'blocked':
      return (
        <div className="term-notice blocked">
          sandbox blocked by {item.byActor}
          {item.reason ? ': ' + item.reason : ''}
        </div>
      );
    case 'released':
      return <div className="term-notice released">sandbox released by {item.byActor}</div>;
    case 'gap':
      return (
        <div className="term-notice gap">
          {item.missed} earlier event{item.missed === 1 ? '' : 's'} were not retained and cannot be
          shown
        </div>
      );
    default:
      return null;
  }
}

export default function Terminal({ sandboxId, role }) {
  const [sandbox, setSandbox] = useState(null);
  const [items, setItems] = useState([]);
  const [status, setStatus] = useState('connecting');
  const [error, setError] = useState(null);
  const [reason, setReason] = useState('');
  const [busy, setBusy] = useState(false);

  const lastSeq = useRef(0);
  const socket = useRef(null);
  const retryTimer = useRef(null);
  const closedByUs = useRef(false);
  const scroller = useRef(null);
  const pinnedToBottom = useRef(true);

  const loadSandbox = useCallback(() => {
    fetch('/api/sandboxes/' + encodeURIComponent(sandboxId))
      .then((res) => {
        if (!res.ok) throw new Error('sandbox not found');
        return res.json();
      })
      .then(setSandbox)
      .catch((err) => setError(String(err)));
  }, [sandboxId]);

  useEffect(loadSandbox, [loadSandbox]);

  useEffect(() => {
    closedByUs.current = false;

    function connect() {
      const scheme = window.location.protocol === 'https:' ? 'wss://' : 'ws://';
      const suffix = lastSeq.current > 0 ? '?after=' + lastSeq.current : '';
      const ws = new WebSocket(
        scheme +
          window.location.host +
          '/api/sandboxes/' +
          encodeURIComponent(sandboxId) +
          '/stream' +
          suffix,
      );
      socket.current = ws;

      ws.onopen = () => setStatus('live');
      ws.onmessage = (message) => {
        let event;
        try {
          event = JSON.parse(message.data);
        } catch (err) {
          return;
        }
        if (event.seq) lastSeq.current = event.seq;
        // A block or release changes the header, not just the log.
        if (event.kind === 'blocked' || event.kind === 'released') loadSandbox();
        setItems((current) => trim(reduceEvent(current, event)));
      };
      ws.onclose = (closeEvent) => {
        if (closedByUs.current) return;
        setStatus(closeEvent.code === CLOSE_SLOW_CONSUMER ? 'catching up' : 'reconnecting');
        retryTimer.current = window.setTimeout(
          connect,
          closeEvent.code === CLOSE_SLOW_CONSUMER ? 0 : RECONNECT_DELAY_MS,
        );
      };
    }

    connect();

    return () => {
      closedByUs.current = true;
      if (retryTimer.current) window.clearTimeout(retryTimer.current);
      if (socket.current) socket.current.close();
    };
  }, [sandboxId, loadSandbox]);

  // Follow the output only while the operator is already at the bottom, so
  // scrolling up to read something doesn't get yanked away by the next chunk.
  useEffect(() => {
    const el = scroller.current;
    if (el && pinnedToBottom.current) {
      el.scrollTop = el.scrollHeight;
    }
  }, [items]);

  function onScroll() {
    const el = scroller.current;
    if (!el) return;
    pinnedToBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
  }

  function block() {
    setBusy(true);
    setError(null);
    fetch('/api/sandboxes/' + encodeURIComponent(sandboxId) + '/block', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ reason }),
    })
      .then(async (res) => {
        if (!res.ok) {
          const body = await res.json().catch(() => ({}));
          throw new Error(body.error || 'block failed (' + res.status + ')');
        }
        setReason('');
        loadSandbox();
      })
      .catch((err) => setError(String(err)))
      .finally(() => setBusy(false));
  }

  function release() {
    setBusy(true);
    setError(null);
    fetch('/api/sandboxes/' + encodeURIComponent(sandboxId) + '/block', { method: 'DELETE' })
      .then(async (res) => {
        if (!res.ok) {
          const body = await res.json().catch(() => ({}));
          throw new Error(body.error || 'release failed (' + res.status + ')');
        }
        loadSandbox();
      })
      .catch((err) => setError(String(err)))
      .finally(() => setBusy(false));
  }

  const block_ = sandbox && sandbox.block;

  return (
    <div>
      <Link className="back-link" to="/sandboxes">
        &larr; All sandboxes
      </Link>

      {error ? <div className="callout">{error}</div> : null}

      <div className="term-header">
        <h2 className="section-title">{sandbox ? sandbox.display_name : sandboxId}</h2>
        <span className={'stream-status ' + status.replace(' ', '-')}>{status}</span>
        <span className="spacer" />
        {sandbox ? (
          <span className="metric">
            {sandbox.running_commands} running &middot; {sandbox.watchers} watching
          </span>
        ) : null}
      </div>

      {block_ ? (
        <div className="callout block-notice">
          <strong>Blocked</strong> by {block_.blocked_by_name} at {formatTime(block_.blocked_at)} &mdash;{' '}
          {block_.reason}
          {block_.killed_processes > 0
            ? ' (' + block_.killed_processes + ' process group(s) killed)'
            : ''}
        </div>
      ) : null}

      {/* Absent rather than disabled for a viewer: a control that exists only to
          be refused is a worse answer than not offering it. */}
      {role === 'admin' ? (
        <div className="stop-bar">
          {block_ ? (
            <button type="button" onClick={release} disabled={busy}>
              Release sandbox
            </button>
          ) : (
            <>
              <input
                type="text"
                className="grow"
                placeholder="Why is this being stopped? (required)"
                value={reason}
                onChange={(event) => setReason(event.target.value)}
              />
              <button
                type="button"
                className="danger"
                onClick={block}
                disabled={busy || reason.trim() === ''}
              >
                Stop sandbox
              </button>
            </>
          )}
        </div>
      ) : null}

      <div className="terminal" ref={scroller} onScroll={onScroll}>
        {items.length === 0 ? (
          <div className="empty-state">No output retained. Live output will appear here.</div>
        ) : (
          items.map((item) => <Line key={item.key} item={item} />)
        )}
      </div>
    </div>
  );
}
