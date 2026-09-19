import SandboxPage from './Sandbox.jsx';

// domainPages lists the pages that belong to the sandbox domain. App.jsx merges
// these with admin-only pages (Agents, History) to build the full nav and route
// table. Hidden pages are registered as routes but not shown in the nav.
export const domainPages = [
  { path: '/sandboxes', label: 'Sandboxes', Component: SandboxPage },
  { path: '/sandboxes/:id', Component: SandboxPage, hidden: true },
];
