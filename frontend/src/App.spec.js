import { mount, flushPromises } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import App from './ChatWorkspace.vue'

const STORAGE_KEY = 'harper.chats.v1'
const modelList = { data: [{ id: 'deepseek-flash', name: 'DeepSeek Flash', input_modalities: ['text', 'image'] }, { id: 'deepseek-v4-pro', name: 'DeepSeek Pro', input_modalities: ['text'] }] }
let wrapper
let requests
let savedChats

function complete(text = 'Hello from **DeepSeek**.') {
  const response = new Response(`data: ${JSON.stringify({ choices: [{ delta: { content: text } }] })}\n\ndata: [DONE]\n\n`, {
    headers: { 'Content-Type': 'text/event-stream' },
  })
  response.mockText = text
  return response
}

function mockFetch(reply = () => complete()) {
  vi.stubGlobal('fetch', vi.fn(async (url, options = {}) => {
    if (url.startsWith('/api/models')) return Response.json(modelList)
    if (url === '/api/chats/import') {
      for (const chat of JSON.parse(options.body).chats) {
        if (!savedChats.has(chat.id)) savedChats.set(chat.id, structuredClone(chat))
        for (const message of savedChats.get(chat.id).messages) if (message.status === 'streaming') message.status = 'stopped'
      }
      return Response.json({ imported: savedChats.size })
    }
    if (url === '/api/chats') return Response.json({ chats: [...savedChats.values()] })
    if (url.startsWith('/api/chats/')) {
      const id = decodeURIComponent(url.split('/').at(-1))
      if (options.method === 'DELETE') { savedChats.delete(id); return new Response(null, { status: 204 }) }
      if (options.method === 'PATCH') { Object.assign(savedChats.get(id), JSON.parse(options.body)); return new Response(null, { status: 204 }) }
      return savedChats.has(id) ? Response.json(savedChats.get(id)) : Response.json({ error: { message: 'Not found' } }, { status: 404 })
    }
    const multipart = options.body instanceof FormData
    const input = JSON.parse(multipart ? options.body.get('request') : options.body)
    requests.push(input)
    const text = input.messages.at(-1).content
    const chat = savedChats.get(input.chatId) || { id: input.chatId, title: text, model: input.model, thinking: input.thinking, updatedAt: Date.now(), messages: [] }
    savedChats.set(chat.id, chat)
    if (input.regenerate) chat.messages.pop()
    else chat.messages.push({ id: crypto.randomUUID(), role: 'user', content: text, attachments: multipart ? options.body.getAll('images').map(file => ({ id: crypto.randomUUID(), name: file.name, contentType: file.type })) : [] })
    const answer = { id: crypto.randomUUID(), role: 'assistant', content: '', status: 'streaming' }
    chat.messages.push(answer)
    const initial = JSON.stringify({ chat })
    options.signal.addEventListener('abort', () => { answer.status = 'stopped' })
    const response = await reply(options)
    if (!response.ok) {
      answer.status = 'error'
      answer.error = (await response.clone().json()).error.message
    } else if (response.mockText !== undefined) {
      answer.content = response.mockText
      answer.status = 'complete'
      return new Response(`event: chat\ndata: ${initial}\n\n${await response.text()}`, { headers: { 'Content-Type': 'text/event-stream' } })
    }
    return response
  }))
}

async function open() {
  wrapper = mount(App, { attachTo: document.body })
  await flushPromises()
}

async function send(text) {
  await wrapper.get('textarea').setValue(text)
  await wrapper.get('form').trigger('submit')
  await flushPromises()
}

beforeEach(() => { localStorage.clear(); requests = []; savedChats = new Map(); mockFetch() })
afterEach(() => { wrapper?.unmount(); wrapper = null; vi.unstubAllGlobals(); vi.restoreAllMocks(); document.body.innerHTML = '' })

describe('Harper conversations', () => {
  it('streams a reply, sanitizes Markdown, and continues the same server conversation', async () => {
    mockFetch(() => complete('**Safe answer** <script>alert(1)</script><img src=x onerror=alert(1)>'))
    await open()
    await send('Hello')
    expect(wrapper.get('.markdown strong').text()).toBe('Safe answer')
    expect(wrapper.find('.markdown script').exists()).toBe(false)
    expect(wrapper.find('.markdown img').exists()).toBe(false)
    expect(wrapper.findAll('article')).toHaveLength(2)
    await send('What did I say?')
    expect(requests[1].chatId).toBe(requests[0].chatId)
    expect(requests[1].messages).toEqual([{ role: 'user', content: 'What did I say?' }])
    expect(savedChats.get(requests[0].chatId).messages.map(item => item.role)).toEqual(['user', 'assistant', 'user', 'assistant'])
    expect(localStorage.getItem(STORAGE_KEY)).toBeNull()
  })

  it('preserves a failed prompt and retries without duplicating it', async () => {
    let attempts = 0
    mockFetch(() => ++attempts === 1 ? Response.json({ error: { message: 'Please try again' } }, { status: 429 }) : complete('Recovered'))
    await open()
    await send('A question worth keeping')
    expect(wrapper.get('.message-error').text()).toBe('Please try again')
    const retry = wrapper.findAll('.message-actions button').find(button => button.text() === 'Retry')
    await retry.trigger('click')
    await flushPromises()
    expect(wrapper.findAll('article')).toHaveLength(2)
    expect(requests[1].messages).toEqual([{ role: 'user', content: 'A question worth keeping' }])
    expect(wrapper.get('.markdown').text()).toBe('Recovered')
  })

  it('aborts an in-flight request and permits a fresh conversation', async () => {
    let signal
    mockFetch(options => {
      signal = options.signal
      return new Response(new ReadableStream({
        start(controller) {
          options.signal.addEventListener('abort', () => controller.error(new DOMException('Stopped', 'AbortError')))
        },
      }))
    })
    await open()
    await send('Take your time')
    expect(wrapper.find('[aria-label="Stop generating"]').exists()).toBe(true)
    await wrapper.get('[aria-label="Stop generating"]').trigger('click')
    await flushPromises()
    expect(signal.aborted).toBe(true)
    expect(wrapper.get('.stopped-label').text()).toBe('Stopped')
    await wrapper.get('.new-chat').trigger('click')
    expect(wrapper.find('h1').exists()).toBe(true)
    expect(wrapper.get('textarea').element.disabled).toBe(false)
  })

  it('restores local history and recovers interrupted replies', async () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify({ activeId: 'saved', model: 'deepseek-flash', chats: [{
      id: 'saved', title: 'Saved question', model: 'deepseek-v4-pro', thinking: true, updatedAt: Date.now(),
      messages: [{ id: 'u', role: 'user', content: 'Remember this' }, { id: 'a', role: 'assistant', content: 'Part of an answer', status: 'streaming' }],
    }] }))
    await open()
    expect(wrapper.get('select').element.value).toBe('deepseek-v4-pro')
    expect(wrapper.get('.thinking-toggle').attributes('aria-pressed')).toBe('true')
    expect(wrapper.get('.stopped-label').text()).toBe('Stopped')
    await send('Continue')
    expect(requests[0].thinking).toBe(true)
    expect(requests[0].model).toBe('deepseek-v4-pro')
    expect(savedChats.get('saved').messages[1].content).toBe('Part of an answer')
    wrapper.unmount()
    wrapper = null
    expect(localStorage.getItem(STORAGE_KEY)).toBeNull()
    expect(savedChats.get('saved').messages).toHaveLength(4)
  })

  it('shows connection errors and recovers after refreshing models', async () => {
    let attempts = 0
    vi.stubGlobal('fetch', vi.fn(async url => url === '/api/chats' ? Response.json({ chats: [] }) : ++attempts === 1
      ? Response.json({ error: { message: 'Invalid API key' } }, { status: 401 })
      : Response.json(modelList)))
    await open()
    expect(wrapper.get('.connection-notice').text()).toContain('Invalid API key')
    expect(wrapper.get('[aria-label="Send message"]').element.disabled).toBe(true)
    await wrapper.get('.connection-notice button').trigger('click')
    await flushPromises()
    expect(wrapper.find('.connection-notice').exists()).toBe(false)
    expect(wrapper.get('select').element.value).toBe('deepseek-flash')
  })

  it('keeps the browser copy and blocks sending when import fails', async () => {
    const legacy = { chats: [{ id: 'legacy', title: 'Keep me', model: 'deepseek-flash', messages: [] }] }
    localStorage.setItem(STORAGE_KEY, JSON.stringify(legacy))
    const original = fetch
    vi.stubGlobal('fetch', vi.fn((url, options) => url === '/api/chats/import'
      ? Promise.resolve(Response.json({ error: { message: 'Storage unavailable' } }, { status: 503 }))
      : original(url, options)))
    await open()
    expect(wrapper.text()).toContain('Storage unavailable')
    expect(JSON.parse(localStorage.getItem(STORAGE_KEY))).toEqual(legacy)
    expect(wrapper.get('[aria-label="Send message"]').element.disabled).toBe(true)
  })

  it('deletes history from the server and does not delete it when the API fails', async () => {
    await open()
    await send('Delete only after saving')
    const original = fetch
    vi.stubGlobal('fetch', vi.fn((url, options) => options?.method === 'DELETE'
      ? Promise.resolve(Response.json({ error: { message: 'Database offline' } }, { status: 503 }))
      : original(url, options)))
    await wrapper.get('.history-delete').trigger('click')
    await wrapper.get('.delete-confirm > button:not(.icon-button)').trigger('click')
    await flushPromises()
    expect(wrapper.findAll('.history-item')).toHaveLength(1)
    expect(wrapper.text()).toContain('Could not delete this chat')
    vi.stubGlobal('fetch', original)
    await wrapper.get('.delete-confirm > button:not(.icon-button)').trigger('click')
    await flushPromises()
    expect(wrapper.findAll('.history-item')).toHaveLength(0)
    expect(savedChats.size).toBe(0)
  })
})

async function attach(files) {
  Object.defineProperty(wrapper.get('input[type="file"]').element, 'files', { value: files, configurable: true })
  await wrapper.get('input[type="file"]').trigger('change')
}

describe('Image attachments', () => {
  beforeEach(() => {
    vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:preview')
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {})
  })

  it('previews images, uploads multipart, and keeps persisted images on follow-up and regeneration', async () => {
    await open()
    const file = new File(['image-test-bytes'], 'photo.png', { type: 'image/png' })
    await attach([file])
    expect(wrapper.get('.pending-image img').attributes('src')).toBe('blob:preview')
    expect(wrapper.get('option[value="deepseek-v4-pro"]').element.disabled).toBe(true)
    await send('What is in this image?')
    const upload = fetch.mock.calls.find(([url]) => url === '/api/chat')[1]
    expect(upload.body).toBeInstanceOf(FormData)
    expect(upload.body.getAll('images')[0].name).toBe('photo.png')
    expect(upload.headers['Content-Type']).toBeUndefined()
    expect(wrapper.find('.pending-image').exists()).toBe(false)
    const image = wrapper.get('.message-images img')
    expect(image.attributes('src')).toMatch(/^\/api\/images\//)
    expect(URL.revokeObjectURL).toHaveBeenCalledWith('blob:preview')
    await send('Look again')
    expect(fetch.mock.calls.filter(([url]) => url === '/api/chat')[1][1].body).not.toBeInstanceOf(FormData)
    const regenerate = wrapper.findAll('.message-actions button').find(button => button.text() === 'Regenerate')
    await regenerate.trigger('click')
    await flushPromises()
    expect(wrapper.findAll('.message-images img')).toHaveLength(1)
    expect(requests.at(-1).regenerate).toBe(true)
  })

  it('accepts image-only messages and rejects invalid or oversized files', async () => {
    await open()
    await attach([new File(['<svg/>'], 'image.svg', { type: 'image/svg+xml' })])
    expect(wrapper.get('.attachment-notice').text()).toContain('JPEG, PNG, GIF, or WebP')
    expect(wrapper.find('.pending-image').exists()).toBe(false)
    await attach([new File([new Uint8Array(8 * 1024 * 1024 + 1)], 'big.png', { type: 'image/png' })])
    expect(wrapper.get('.attachment-notice').text()).toContain('8 MB')
    await attach([new File(['png'], 'small.png', { type: 'image/png' })])
    expect(wrapper.get('[aria-label="Send message"]').element.disabled).toBe(false)
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(requests[0].messages[0].content).toBe('Describe these images.')
  })

  it('retains the draft and files when upload fails before a chat is saved', async () => {
    const original = fetch
    vi.stubGlobal('fetch', vi.fn((url, options) => url === '/api/chat'
      ? Promise.resolve(Response.json({ error: { message: 'Object storage unavailable' } }, { status: 503 }))
      : original(url, options)))
    await open()
    await attach([new File(['png'], 'retry.png', { type: 'image/png' })])
    await send('Keep my image')
    expect(wrapper.get('textarea').element.value).toBe('Keep my image')
    expect(wrapper.get('.pending-image img').attributes('alt')).toBe('retry.png')
    expect(wrapper.get('.attachment-notice').text()).toContain('Object storage unavailable')
    expect(wrapper.findAll('article')).toHaveLength(0)
    await wrapper.get('[aria-label="Remove retry.png"]').trigger('click')
    expect(wrapper.find('.pending-image').exists()).toBe(false)
  })
})
