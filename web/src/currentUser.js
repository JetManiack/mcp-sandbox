import { useEffect, useState } from 'react';

export function useCurrentUser() {
  const [user, setUser] = useState(null);
  const [error, setError] = useState(null);

  useEffect(() => {
    fetch('/api/me')
      .then((res) => {
        if (!res.ok) {
          throw new Error('not authenticated');
        }
        return res.json();
      })
      .then(setUser)
      .catch((err) => setError(String(err)));
  }, []);

  return { user, error };
}
