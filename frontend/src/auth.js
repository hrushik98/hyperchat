// DeepSeek may also return 401. Only an explicit local session error locks the
// workspace, so API-key errors retain the usual connection recovery controls.
export async function authFetch(url, options = {}) {
  const response = await fetch(url, { credentials: 'same-origin', ...options })
  if (response.status === 401) {
    const result = await response.clone().json().catch(() => ({}))
    if (result.error?.code === 'UNAUTHENTICATED') {
      window.dispatchEvent(new Event('harper:unauthorized'))
    }
  }
  return response
}
