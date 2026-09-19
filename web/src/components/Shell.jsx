import { NavLink } from 'react-router-dom';

// Shell renders the persistent chrome: sticky top-bar with nav links drawn from
// the pages prop, user identity, and logout; the main content area holds children.
//
// pages is an array of { path, label, hidden? } descriptors. Hidden entries are
// not shown in the nav (they exist only as routes).
export default function Shell({ pages, user, children }) {
  function handleLogout() {
    fetch('/auth/logout', { method: 'POST' }).then(() => {
      window.location.href = '/';
    });
  }

  const navPages = pages.filter((p) => !p.hidden && p.label);

  return (
    <>
      <header className="site">
        <h1>go-ai-executor</h1>
        <nav className="nav-tabs">
          {navPages.map((p) => (
            <NavLink
              key={p.path}
              to={p.path}
              className={({ isActive }) => (isActive ? 'active' : undefined)}
            >
              {p.label}
            </NavLink>
          ))}
          <span className="spacer" />
          <span className="whoami">
            {user.display_name} &middot; {user.role}
          </span>
          <button type="button" className="logout-link" onClick={handleLogout}>
            Log out
          </button>
        </nav>
      </header>
      <main>{children}</main>
    </>
  );
}
