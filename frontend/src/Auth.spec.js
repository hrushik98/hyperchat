import { mount, flushPromises } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App.vue'
import { authFetch } from './auth'

let wrapper
let status
let authReply
let workspaceReply
const recoveryKey = 'HARPER-TEST-RECOVERY-KEY'

beforeEach(() => {
  localStorage.clear()
  status = { setupRequired: false, authenticated: false }
  authReply = () => Response.json({ authenticated: true, username: 'owner' })
  workspaceReply = url => url.startsWith('/api/models')
    ? Response.json({ data: [{ id: 'deepseek-flash', name: 'DeepSeek Flash' }] })
    : Response.json({ chats: [] })
  vi.stubGlobal('fetch', vi.fn(async (url, options = {}) => {
    if (url === '/api/auth/status') return Response.json(status)
    if (url === '/api/auth/logout') return new Response(null, { status: 204 })
    if (url.startsWith('/api/auth/')) return authReply(url, options)
    return workspaceReply(url, options)
  }))
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
  document.body.innerHTML = ''
})

async function open() {
  wrapper = mount(App, { attachTo: document.body })
  await flushPromises()
}

async function credentials(password = 'correct horse battery', confirmation = password) {
  await wrapper.get('#username').setValue('owner')
  await wrapper.get('#password').setValue(password)
  if (wrapper.find('#confirm-password').exists()) await wrapper.get('#confirm-password').setValue(confirmation)
  await wrapper.get('.auth-form').trigger('submit')
  await flushPromises()
}

describe('Owner account screens', () => {
  it('does not fetch private history or models until the account is unlocked', async () => {
    await open()
    expect(wrapper.text()).toContain('Welcome back.')
    expect(wrapper.find('textarea').exists()).toBe(false)
    expect(fetch.mock.calls.map(([url]) => url)).toEqual(['/api/auth/status'])
    await credentials()
    expect(wrapper.find('textarea').exists()).toBe(true)
    expect(fetch.mock.calls.map(([url]) => url)).toContain('/api/chats')
    expect(localStorage.getItem('harper.password')).toBeNull()
    expect(JSON.stringify(localStorage)).not.toContain('correct horse battery')
  })

  it('creates the first owner and requires saving the recovery key before opening chat', async () => {
    status = { setupRequired: true, authenticated: false }
    authReply = () => Response.json({ authenticated: true, username: 'owner', recoveryKey })
    await open()
    expect(wrapper.text()).toContain('Make yourself at home.')
    await credentials('short')
    expect(wrapper.get('.auth-error').text()).toContain('at least 12')
    expect(fetch).toHaveBeenCalledTimes(1)
    await credentials('correct horse battery', 'different password')
    expect(wrapper.get('.auth-error').text()).toContain('do not match')
    expect(fetch).toHaveBeenCalledTimes(1)
    await credentials()
    expect(wrapper.get('.recovery-key').text()).toBe(recoveryKey)
    expect(wrapper.get('.auth-submit').element.disabled).toBe(true)
    expect(wrapper.find('textarea').exists()).toBe(false)
    expect(fetch.mock.calls.map(([url]) => url)).toEqual(['/api/auth/status', '/api/auth/setup'])
    await wrapper.get('input[type="checkbox"]').setValue(true)
    await wrapper.get('.auth-submit').trigger('click')
    await flushPromises()
    expect(wrapper.find('textarea').exists()).toBe(true)
    expect(wrapper.find('.recovery-key').exists()).toBe(false)
    expect(JSON.stringify(localStorage)).not.toContain(recoveryKey)
  })

  it('shows invalid login and recovery errors without exposing chat', async () => {
    authReply = () => Response.json({ error: { message: 'Incorrect username or master password.' } }, { status: 401 })
    await open()
    await credentials()
    expect(wrapper.get('.auth-error').text()).toContain('Incorrect username')
    expect(wrapper.find('textarea').exists()).toBe(false)
    await wrapper.get('.auth-link').trigger('click')
    expect(wrapper.get('#password').element.value).toBe('')
    await wrapper.get('#recovery').setValue('wrong-key')
    authReply = () => Response.json({ error: { message: 'The recovery key is incorrect.' } }, { status: 401 })
    await credentials()
    expect(wrapper.get('.auth-error').text()).toBe('The recovery key is incorrect.')
    expect(wrapper.find('textarea').exists()).toBe(false)
  })

  it('returns a losing concurrent setup browser to login and keeps the explanation visible', async () => {
    status = { setupRequired: true, authenticated: false }
    authReply = () => Response.json({ error: { message: 'This installation already has an owner. Sign in or reset your account.' } }, { status: 409 })
    await open()
    await credentials()
    expect(wrapper.text()).toContain('Welcome back.')
    expect(wrapper.get('.auth-error').text()).toContain('already has an owner')
    expect(wrapper.find('#confirm-password').exists()).toBe(false)
    expect(wrapper.get('#password').element.value).toBe('')
  })

  it('resets forgotten credentials using recovery and presents the replacement key', async () => {
    await open()
    await wrapper.get('.auth-link').trigger('click')
    await wrapper.get('#recovery').setValue('old-recovery-key')
    authReply = () => Response.json({ authenticated: true, username: 'owner', recoveryKey })
    await credentials()
    const [url, options] = fetch.mock.calls.find(([url]) => url === '/api/auth/reset')
    expect(url).toBe('/api/auth/reset')
    expect(JSON.parse(options.body)).toEqual({ username: 'owner', password: 'correct horse battery', recoveryKey: 'old-recovery-key' })
    expect(wrapper.get('.recovery-key').text()).toBe(recoveryKey)
    expect(wrapper.find('textarea').exists()).toBe(false)
  })

  it('restores a session and locks the workspace by revoking its cookie', async () => {
    status = { setupRequired: false, authenticated: true, username: 'owner' }
    await open()
    expect(wrapper.find('textarea').exists()).toBe(true)
    const lock = wrapper.findAll('.account-actions button').find(button => button.text() === 'Lock workspace')
    await lock.trigger('click')
    await flushPromises()
    expect(fetch.mock.calls.find(([url]) => url === '/api/auth/logout')[1].method).toBe('POST')
    expect(wrapper.text()).toContain('Welcome back.')
    expect(wrapper.find('textarea').exists()).toBe(false)
  })

  it('removes private content when the backend reports an expired session', async () => {
    status = { setupRequired: false, authenticated: true, username: 'owner' }
    await open()
    workspaceReply = () => Response.json({ error: { code: 'UNAUTHENTICATED', message: 'Sign in.' } }, { status: 401 })
    await wrapper.get('[aria-label="Refresh connection"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('textarea').exists()).toBe(false)
    expect(wrapper.text()).toContain('Welcome back.')
  })

  it('keeps DeepSeek API-key errors inside the signed-in workspace', async () => {
    status = { setupRequired: false, authenticated: true, username: 'owner' }
    workspaceReply = url => url.startsWith('/api/models')
      ? Response.json({ error: { message: 'DeepSeek rejected the API key.' } }, { status: 401 })
      : Response.json({ chats: [] })
    await open()
    expect(wrapper.get('.connection-notice').text()).toContain('DeepSeek rejected')
    expect(wrapper.find('textarea').exists()).toBe(true)
    expect(wrapper.find('.auth-form').exists()).toBe(false)
  })

  it('fails closed when account storage cannot be checked and retries successfully', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => Response.json({ error: { message: 'Storage unavailable' } }, { status: 503 })))
    await open()
    expect(wrapper.get('.auth-error').text()).toContain('Storage unavailable')
    expect(wrapper.find('.auth-form').exists()).toBe(false)
    expect(wrapper.find('textarea').exists()).toBe(false)
    vi.stubGlobal('fetch', vi.fn(async () => Response.json(status)))
    await wrapper.get('.auth-submit').trigger('click')
    await flushPromises()
    expect(wrapper.find('.auth-form').exists()).toBe(true)
  })

  it('hides and shows the password only when requested', async () => {
    await open()
    expect(wrapper.get('#password').attributes('type')).toBe('password')
    await wrapper.get('[aria-label="Show master password"]').trigger('click')
    expect(wrapper.get('#password').attributes('type')).toBe('text')
    await wrapper.get('[aria-label="Hide master password"]').trigger('click')
    expect(wrapper.get('#password').attributes('type')).toBe('password')
  })

  it('preserves the original error response when inspecting session failures', async () => {
    workspaceReply = () => Response.json({ error: { code: 'UNAUTHENTICATED', message: 'Sign in.' } }, { status: 401 })
    const response = await authFetch('/api/chats')
    expect((await response.json()).error.message).toBe('Sign in.')
  })
})
