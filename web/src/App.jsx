import { BrowserRouter, Navigate, Route, Routes } from 'react-router-dom';
import { useCurrentUser } from './currentUser.js';
import Shell from './components/Shell.jsx';
import { domainPages } from './pages/domain/index.js';
import Agents from './pages/Agents.jsx';
import History from './pages/History.jsx';

// Build the full page table: domain pages that everyone sees plus admin-only
// pages. adminOnly is stripped from descriptors before they reach Shell so Shell
// does not need to know about roles.
function buildPages(role) {
  const all = [
    ...domainPages,
    { path: '/agents', label: 'Agents', Component: Agents, adminOnly: true },
    { path: '/history', label: 'History', Component: History, adminOnly: true },
  ];
  // Filter out admin-only entries for non-admin viewers; keep hidden routes for
  // everyone so a direct URL still works when the user has access.
  return all.filter((p) => !p.adminOnly || role === 'admin');
}

export default function App() {
  const { user, error } = useCurrentUser();

  if (error) {
    return (
      <div className="login-gate">
        <p>You need to log in to watch sandbox terminals.</p>
        <a href="/auth/login">Log in</a>
      </div>
    );
  }

  if (!user) return <div className="empty-state">Loading…</div>;

  const pages = buildPages(user.role);

  return (
    <BrowserRouter>
      <Shell pages={pages} user={user}>
        <Routes>
          <Route path="/" element={<Navigate to="/sandboxes" replace />} />
          {pages.map(({ path, Component }) => (
            <Route key={path} path={path} element={<Component />} />
          ))}
          <Route path="*" element={<Navigate to="/sandboxes" replace />} />
        </Routes>
      </Shell>
    </BrowserRouter>
  );
}
