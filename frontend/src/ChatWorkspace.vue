<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { ArrowDown, ArrowDownToLine, ArrowUp, Check, ChevronDown, Code2, Copy, Lightbulb, Menu, MessageCircle, Paperclip, PenLine, Plus, RefreshCw, Search, Sparkles, Square, Trash2, X } from '@lucide/vue'
import { marked } from 'marked'
import DOMPurify from 'dompurify'
import hljs from 'highlight.js/lib/common'
import { readStream } from './stream'
import { authFetch } from './auth'

defineProps({ username: { type: String, default: '' } })
const emit = defineEmits(['logout', 'reset'])

const STORAGE_KEY = 'harper.chats.v1'
const PREFERENCES_KEY = 'harper.preferences.v1'
const chats = ref([])
const activeId = ref(null)
const draft = ref('')
const pendingImages = ref([])
const imageInput = ref(null)
const attachmentError = ref('')
const models = ref([])
const model = ref('')
const thinking = ref(false)
const loadingModels = ref(true)
const connectionError = ref('')
const storageError = ref('')
const historyError = ref('')
const loadingChats = ref(true)
const busy = ref(false)
const sidebarOpen = ref(false)
const search = ref('')
const composer = ref(null)
const scrollArea = ref(null)
const followOutput = ref(true)
const copied = ref(null)
const deleteId = ref(null)
let controller
let copyTimer
let modelController
let historyController
let settingsQueue = Promise.resolve()

const activeChat = computed(() => chats.value.find(chat => chat.id === activeId.value))
const messages = computed(() => activeChat.value?.messages || [])
const visibleChats = computed(() => [...chats.value].sort((a, b) => b.updatedAt - a.updatedAt)
  .filter(chat => chat.title.toLowerCase().includes(search.value.toLowerCase())))
const modelName = computed(() => models.value.find(item => item.id === model.value)?.name || model.value || 'Select a model')
function supportsVision(item) { return item?.input_modalities?.length ? item.input_modalities.includes('image') : item?.id === 'deepseek-flash' }
const selectedSupportsVision = computed(() => supportsVision(models.value.find(item => item.id === model.value)))
const chatHasImages = computed(() => messages.value.some(item => item.attachments?.length))
const needsVision = computed(() => pendingImages.value.length > 0 || chatHasImages.value)
const canSend = computed(() => (draft.value.trim() || pendingImages.value.length) && (!needsVision.value || selectedSupportsVision.value) && model.value && !busy.value && !loadingModels.value && !connectionError.value && !loadingChats.value && !historyError.value)
const suggestions = [
  { icon: PenLine, label: 'Find the right words', prompt: 'Help me write a short, thoughtful email. Ask me who it is for and what I want to say.' },
  { icon: Code2, label: 'Build something', prompt: 'I want to build a small, useful project this weekend. Help me find an idea and plan the first steps.' },
  { icon: Lightbulb, label: 'Follow an idea', prompt: 'Help me brainstorm a creative idea. Ask me a few questions to get started.' },
  { icon: Search, label: 'Make it make sense', prompt: 'Help me understand a complex topic. Ask me what I am curious about, then explain it simply.' },
]

marked.use({ renderer: {
  code({ text, lang }) {
    const language = (lang || '').split(/\s/)[0]
    const highlighted = language && hljs.getLanguage(language)
      ? hljs.highlight(text, { language }).value
      : text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    return `<pre><code class="hljs">${highlighted}</code></pre>`
  },
} })

function markdown(content) {
  // Older browser engines must fall back to escaped text, never raw HTML.
  if (!DOMPurify.isSupported) return (content || '').replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
  return DOMPurify.sanitize(marked.parse(content || ''), {
    FORBID_TAGS: ['img', 'style', 'input', 'form', 'iframe'],
    FORBID_ATTR: ['style'],
  })
}

async function api(url, options = {}) {
  const response = await authFetch(url, { ...options, headers: { 'Content-Type': 'application/json', ...options.headers } })
  const result = response.status === 204 ? null : await response.json().catch(() => ({}))
  if (!response.ok) throw new Error(result.error?.message || `Request failed (${response.status}). Please retry.`)
  return result
}

function persistPreferences() {
  try {
    localStorage.setItem(PREFERENCES_KEY, JSON.stringify({ activeId: activeId.value, model: model.value, thinking: thinking.value }))
  } catch { /* Preferences are optional. Transcripts are stored by the backend. */ }
}

async function loadChats() {
  if (busy.value || historyController) return
  historyController = new AbortController()
  const timeout = setTimeout(() => historyController?.abort(), 30000)
  loadingChats.value = true
  historyError.value = ''
  try {
    let legacy = null
    let preferences = null
    try {
      legacy = JSON.parse(localStorage.getItem(STORAGE_KEY) || 'null')
      preferences = JSON.parse(localStorage.getItem(PREFERENCES_KEY) || 'null') || legacy
    } catch { storageError.value = 'Old browser history could not be read. Your PostgreSQL chats are still available.' }
    const signal = historyController.signal
    if (legacy?.chats?.length) {
      await api('/api/chats/import', { method: 'POST', signal, body: JSON.stringify({ chats: legacy.chats }) })
    }
    const result = await api('/api/chats', { signal })
    if (!Array.isArray(result.chats)) throw new Error('The server returned invalid chat history. Please retry.')
    chats.value = result.chats
    // Only remove the browser copy after the import and database read succeed.
    if (legacy) { try { localStorage.removeItem(STORAGE_KEY) } catch { /* Import is idempotent. */ } }
    activeId.value = chats.value.some(chat => chat.id === preferences?.activeId) ? preferences.activeId : null
    if (preferences?.model && models.value.some(item => item.id === preferences.model)) model.value = preferences.model
    thinking.value = Boolean(preferences?.thinking)
    if (activeChat.value) {
      model.value = activeChat.value.model || model.value
      thinking.value = Boolean(activeChat.value.thinking)
    }
    persistPreferences()
    scrollToEnd(true)
  } catch (error) {
    historyError.value = error.name === 'AbortError' ? 'Chat storage took too long to respond. Please retry.' : error.message
  } finally {
    clearTimeout(timeout)
    loadingChats.value = false
    historyController = null
  }
}

async function refreshChat(chat, localStatus) {
  try {
    const saved = await api(`/api/chats/${encodeURIComponent(chat.id)}`)
    if (saved.messages?.at(-1)?.status === 'streaming' && localStatus && localStatus !== 'streaming') saved.messages.at(-1).status = localStatus
    Object.assign(chat, saved)
    return true
  } catch (error) {
    storageError.value = `Could not reload the saved conversation: ${error.message}`
  }
}

watch([activeId, model, thinking], persistPreferences)

watch([model, thinking], () => {
  if (activeChat.value && !busy.value && !loadingChats.value && (activeChat.value.model !== model.value || activeChat.value.thinking !== thinking.value)) {
    activeChat.value.model = model.value
    activeChat.value.thinking = thinking.value
    const id = activeChat.value.id
    const settings = { model: model.value, thinking: thinking.value }
    settingsQueue = settingsQueue.then(() => api(`/api/chats/${encodeURIComponent(id)}`, { method: 'PATCH', body: JSON.stringify(settings) }))
      .catch(error => { storageError.value = `Could not save conversation settings: ${error.message}` })
  }
})

watch(draft, () => nextTick(() => {
  if (composer.value) {
    composer.value.style.height = 'auto'
    composer.value.style.height = `${Math.min(composer.value.scrollHeight, 200)}px`
  }
}))

async function loadModels(refresh = false) {
  modelController?.abort()
  modelController = new AbortController()
  const timeout = setTimeout(() => modelController.abort(), 25000)
  loadingModels.value = true
  connectionError.value = ''
  try {
    const response = await authFetch(`/api/models${refresh === true ? '?refresh=1' : ''}`, { signal: modelController.signal })
    const result = await response.json()
    if (!response.ok) throw new Error(result.error?.message || 'Could not load models.')
    models.value = result.data.filter(item => item.id)
    if (!models.value.length) throw new Error('No DeepSeek models are available for this account.')
    if (!models.value.some(item => item.id === model.value)) {
      model.value = models.value.find(item => item.id === 'deepseek-flash')?.id || models.value[0].id
    }
  } catch (error) {
    connectionError.value = error.name === 'AbortError' ? 'DeepSeek took too long to connect. Try again.' : error.message
  } finally {
    clearTimeout(timeout)
    loadingModels.value = false
  }
}

function removeImage(id) {
  const item = pendingImages.value.find(image => image.id === id)
  if (item) URL.revokeObjectURL(item.preview)
  pendingImages.value = pendingImages.value.filter(image => image.id !== id)
}

function clearImages() {
  for (const item of [...pendingImages.value]) removeImage(item.id)
  attachmentError.value = ''
}

function addImages(files) {
  attachmentError.value = ''
  if (busy.value) return
  if (!selectedSupportsVision.value) { attachmentError.value = 'Choose a model marked Vision to attach images.'; return }
  for (const file of files) {
    if (pendingImages.value.length >= 4) { attachmentError.value = 'Attach up to four images per message.'; break }
    if (!['image/jpeg', 'image/png', 'image/gif', 'image/webp'].includes(file.type)) { attachmentError.value = 'Use JPEG, PNG, GIF, or WebP images.'; continue }
    if (!file.size || file.size > 8 * 1024 * 1024) { attachmentError.value = 'Each image must be between 1 byte and 8 MB.'; continue }
    pendingImages.value.push({ id: crypto.randomUUID(), file, name: file.name, preview: URL.createObjectURL(file) })
  }
}

function chooseImages(event) { addImages([...event.target.files]); event.target.value = '' }
function pasteImages(event) {
  const files = [...(event.clipboardData?.files || [])].filter(file => file.type.startsWith('image/'))
  if (files.length) { event.preventDefault(); addImages(files) }
}
function dropImages(event) { addImages([...(event.dataTransfer?.files || [])]) }
function imageURL(item) { return item.preview || `/api/images/${encodeURIComponent(item.id)}` }

function stop() { controller?.abort() }

function newChat() {
  stop()
  activeId.value = null
  clearImages()
  draft.value = ''
  sidebarOpen.value = false
  deleteId.value = null
  nextTick(() => composer.value?.focus())
}

function selectChat(chat) {
  stop()
  activeId.value = chat.id
  clearImages()
  model.value = chat.model || model.value
  thinking.value = Boolean(chat.thinking)
  draft.value = ''
  sidebarOpen.value = false
  deleteId.value = null
  followOutput.value = true
  scrollToEnd()
}

async function removeChat(id) {
  if (activeId.value === id) stop()
  try {
    await api(`/api/chats/${encodeURIComponent(id)}`, { method: 'DELETE' })
    if (activeId.value === id) newChat()
    chats.value = chats.value.filter(chat => chat.id !== id)
    deleteId.value = null
  } catch (error) { storageError.value = `Could not delete this chat: ${error.message}` }
}

function trackScroll() {
  const el = scrollArea.value
  if (el) followOutput.value = el.scrollHeight - el.scrollTop - el.clientHeight < 100
}

async function scrollToEnd(force = false) {
  await nextTick()
  if (scrollArea.value && (force || followOutput.value)) scrollArea.value.scrollTop = scrollArea.value.scrollHeight
}

async function send(text = draft.value, regenerate = false) {
  const queued = regenerate ? [] : [...pendingImages.value]
  const originalDraft = text
  text = text.trim() || (queued.length ? 'Describe these images.' : '')
  if (needsVision.value && !selectedSupportsVision.value) { attachmentError.value = 'This conversation needs a model marked Vision.'; return }
  if (!text || busy.value || !model.value || loadingModels.value || connectionError.value || loadingChats.value || historyError.value) return
  if (!activeChat.value) {
    const chat = { id: crypto.randomUUID(), title: text.slice(0, 54), model: model.value,
      thinking: thinking.value, updatedAt: Date.now(), messages: [] }
    chats.value.unshift(chat)
    activeId.value = chat.id
  }
  const chat = activeChat.value
  chat.updatedAt = Date.now()
  chat.model = model.value
  chat.thinking = thinking.value
  const previousMessages = [...chat.messages]
  let committed = false
  if (regenerate) chat.messages.pop()
  else chat.messages.push({ id: crypto.randomUUID(), role: 'user', content: text, attachments: queued.map(({ id, name, preview }) => ({ id, name, preview })) })
  chat.messages.push({ id: crypto.randomUUID(), role: 'assistant', content: '', reasoning: '',
    model: modelName.value, status: 'streaming', error: '' })
  let answer = chat.messages[chat.messages.length - 1]
  draft.value = ''
  busy.value = true
  followOutput.value = true
  controller = new AbortController()
  const signal = controller.signal
  await scrollToEnd(true)
  try {
    const payload = { chatId: chat.id, model: chat.model, messages: [{ role: 'user', content: text }], thinking: chat.thinking, regenerate }
    let body = JSON.stringify(payload)
    let headers = { 'Content-Type': 'application/json' }
    if (queued.length) {
      body = new FormData()
      body.append('request', JSON.stringify(payload))
      for (const item of queued) body.append('images', item.file, item.name)
      headers = {}
    }
    const response = await authFetch('/api/chat', { method: 'POST', headers, signal, body })
    if (!response.ok) {
      const result = await response.json().catch(() => ({}))
      throw new Error(result.error?.message || `Request failed (${response.status}). Please retry.`)
    }
    await readStream(response, event => {
      if (event.chat) {
        committed = true
        for (const item of queued) removeImage(item.id)
        Object.assign(chat, event.chat)
        answer = chat.messages[chat.messages.length - 1]
      }
      const choice = event.choices?.[0]
      if (choice?.delta?.content) answer.content += choice.delta.content
      if (choice?.delta?.reasoning_content) answer.reasoning += choice.delta.reasoning_content
      if (choice?.finish_reason === 'length') answer.notice = 'The response reached the model’s output limit. Ask it to continue.'
      if (choice?.finish_reason === 'content_filter') answer.notice = 'DeepSeek stopped this response due to its content policy.'
      scrollToEnd()
    }, signal)
    if (!answer.content.trim()) throw new Error('DeepSeek returned an empty answer. Please retry.')
    answer.status = 'complete'
  } catch (error) {
    answer.status = error.name === 'AbortError' ? 'stopped' : 'error'
    if (error.name !== 'AbortError') answer.error = error.message
  } finally {
    const reloaded = await refreshChat(chat, answer.status)
    if (queued.length) {
      const persisted = committed || reloaded && chat.messages.some(item => !previousMessages.some(previous => previous.id === item.id) && item.role === 'user' && item.content === text && item.attachments?.some(image => !image.preview))
      if (persisted) { for (const item of queued) removeImage(item.id) }
      else {
        chat.messages = previousMessages
        if (activeId.value === chat.id) { draft.value = originalDraft; attachmentError.value = answer.error || 'Upload was stopped. Your images are ready to send again.' }
      }
    }
    busy.value = false
    controller = null
    persistPreferences()
    if (activeId.value === chat.id) {
      scrollToEnd()
      nextTick(() => composer.value?.focus())
    }
  }
}

function retry() {
  if (busy.value) return
  const chat = activeChat.value
  if (!chat || chat.messages.at(-1)?.role !== 'assistant') return
  const user = chat.messages.at(-2)
  if (user?.role !== 'user' || connectionError.value || loadingModels.value || loadingChats.value || historyError.value) return
  send(user.content, true)
}

async function copyMessage(item) {
  try {
    await navigator.clipboard.writeText(item.content)
    copied.value = item.id
    clearTimeout(copyTimer)
    copyTimer = setTimeout(() => { copied.value = null }, 1800)
  } catch {
    storageError.value = 'Could not access the clipboard. Select the text to copy it.'
  }
}

function exportChat() {
  if (!activeChat.value) return
  const text = `# ${activeChat.value.title}\n\n` + messages.value.map(item =>
    `## ${item.role === 'user' ? 'You' : 'DeepSeek'}\n\n${item.content}${item.attachments?.length ? '\n\n' + item.attachments.map(image => `![${image.name.replace(/[\[\]\n]/g, '')}](${location.origin}/api/images/${encodeURIComponent(image.id)})`).join('\n') : ''}${item.error ? `\n\nError: ${item.error}` : ''}`).join('\n\n')
  const url = URL.createObjectURL(new Blob([text], { type: 'text/markdown;charset=utf-8' }))
  const link = document.createElement('a')
  link.href = url
  link.download = `${activeChat.value.title.replace(/[^a-z0-9]+/gi, '-').slice(0, 48) || 'harper-chat'}.md`
  link.click()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}

function composerKeydown(event) {
  if (event.key === 'Enter' && !event.shiftKey && !event.isComposing) {
    event.preventDefault()
    send()
  }
}

function keyboardShortcuts(event) {
  if ((event.metaKey || event.ctrlKey) && event.shiftKey && event.key.toLowerCase() === 'o') {
    event.preventDefault()
    newChat()
  }
  if (event.key === 'Escape') sidebarOpen.value = false
}

onMounted(() => {
  loadChats()
  loadModels()
  scrollToEnd(true)
  window.addEventListener('keydown', keyboardShortcuts)
})
onBeforeUnmount(() => {
  stop()
  clearImages()
  modelController?.abort()
  historyController?.abort()
  clearTimeout(copyTimer)
  persistPreferences()
  window.removeEventListener('keydown', keyboardShortcuts)
})
</script>

<template>
  <div class="app-shell">
    <button v-if="sidebarOpen" class="sidebar-backdrop" aria-label="Close navigation" @click="sidebarOpen = false"></button>
    <aside id="chat-navigation" class="sidebar" :class="{ 'is-open': sidebarOpen }" :inert="!sidebarOpen" aria-label="Chat navigation">
      <div class="brand-row">
        <a class="brand" href="/" @click.prevent="newChat"><img src="/favicon.svg" alt="" />harper<span class="brand-dot">.</span></a>
        <button class="icon-button" aria-label="Close navigation" @click="sidebarOpen = false"><X :size="19" /></button>
      </div>
      <button class="new-chat" @click="newChat"><Plus :size="18" /><span>New conversation</span><span class="shortcut" aria-hidden="true">↗</span></button>
      <div class="history-heading"><span>YOUR CONVERSATIONS</span><span>{{ chats.length.toString().padStart(2, '0') }}</span></div>
      <label v-if="chats.length > 5" class="history-search"><Search :size="15" /><input v-model="search" placeholder="Find a conversation" aria-label="Find a conversation" /></label>
      <nav class="history-list" aria-label="Saved conversations">
        <div v-for="chat in visibleChats" :key="chat.id" class="history-item" :class="{ selected: chat.id === activeId }">
          <div v-if="deleteId === chat.id" class="delete-confirm"><span>Delete this chat?</span><button @click="removeChat(chat.id)">Delete</button><button class="icon-button" aria-label="Cancel deletion" @click="deleteId = null"><X :size="14" /></button></div>
          <template v-else>
            <button class="history-select" :aria-current="chat.id === activeId ? 'page' : undefined" @click="selectChat(chat)"><MessageCircle :size="16" /><span>{{ chat.title }}</span></button>
            <button class="history-delete icon-button" :aria-label="`Delete ${chat.title}`" @click="deleteId = chat.id"><Trash2 :size="14" /></button>
          </template>
        </div>
        <div v-if="!chats.length" class="history-empty"><MessageCircle :size="22" :stroke-width="1.4" /><p v-if="loadingChats">Loading your conversations…</p><p v-else>A fresh page.<br />Your conversations will live here.</p></div>
        <p v-else-if="!visibleChats.length" class="no-results">No conversations found.</p>
      </nav>
      <div class="sidebar-bottom">
        <div class="local-note"><span class="local-avatar">{{ username.slice(0, 1).toUpperCase() || 'H' }}</span><div><strong>{{ username || 'Your local workspace' }}</strong><span>Your private workspace</span></div></div>
        <div class="account-actions"><button @click="emit('reset')">Reset account</button><button @click="emit('logout')">Lock workspace</button></div>
        <div class="connection-status" :class="{ offline: connectionError }"><span class="status-dot"></span><span>{{ loadingModels ? 'Connecting to DeepSeek' : connectionError ? 'Connection needs attention' : 'DeepSeek connected' }}</span><button class="icon-button" aria-label="Refresh connection" :disabled="loadingModels || busy" @click="loadModels(true)"><RefreshCw :size="13" :class="{ spinning: loadingModels }" /></button></div>
      </div>
    </aside>

    <main class="workspace">
      <header class="workspace-header">
        <button class="icon-button" aria-label="Open navigation" title="Conversations" :aria-expanded="sidebarOpen" aria-controls="chat-navigation" @click="sidebarOpen = true"><Menu :size="20" /></button>
        <button class="icon-button" aria-label="New conversation" title="New conversation" @click="newChat"><PenLine :size="19" /></button>
      </header>

      <div v-if="connectionError" class="notice connection-notice" role="alert"><span>{{ connectionError }}</span><button :disabled="loadingModels" @click="loadModels(true)">Try again <RefreshCw :size="14" /></button></div>
      <div v-if="historyError" class="notice" role="alert"><span>{{ historyError }}</span><button :disabled="loadingChats || busy" @click="loadChats">Retry loading chats <RefreshCw :size="14" /></button></div>
      <div v-if="storageError" class="notice" role="status"><span>{{ storageError }}</span><button class="icon-button" aria-label="Dismiss notice" @click="storageError = ''"><X :size="16" /></button></div>

      <div ref="scrollArea" class="conversation-scroll" :class="{ 'is-empty': !messages.length }" @scroll="trackScroll">
        <section v-if="!messages.length" class="welcome">
          <h1>What can I help with?</h1>
          <div class="suggestions">
            <button v-for="suggestion in suggestions" :key="suggestion.label" :disabled="busy || loadingModels || !!connectionError || loadingChats || !!historyError" @click="send(suggestion.prompt)"><component :is="suggestion.icon" :size="18" :stroke-width="1.6" /><span>{{ suggestion.label }}</span></button>
          </div>
        </section>

        <section v-else class="messages" aria-label="Conversation">
          <article v-for="(item, index) in messages" :key="item.id" class="message" :class="item.role">
            <div class="message-body">
              <template v-if="item.role === 'user'">
                <div v-if="item.attachments?.length" class="message-images"><a v-for="image in item.attachments" :key="image.id" :href="imageURL(image)" target="_blank" rel="noopener noreferrer"><img :src="imageURL(image)" :alt="image.name" loading="lazy" /><span>{{ image.name }}</span></a></div>
                <p class="user-text">{{ item.content }}</p>
              </template>
              <template v-else>
                <details v-if="item.reasoning" class="reasoning"><summary><Sparkles :size="14" />{{ item.status === 'streaming' && !item.content ? 'Thinking through it…' : 'Thinking process' }}<ChevronDown :size="14" /></summary><div>{{ item.reasoning }}</div></details>
                <div v-if="item.content" class="markdown" v-html="markdown(item.content)"></div>
                <div v-if="item.status === 'streaming' && !item.content" class="typing-indicator" role="status" :aria-label="item.reasoning ? 'DeepSeek is thinking' : 'DeepSeek is replying'"><span></span><span></span><span></span></div>
                <p v-if="item.error" class="message-error" role="alert">{{ item.error }}</p>
                <p v-if="item.notice" class="message-notice">{{ item.notice }}</p>
                <div v-if="item.status !== 'streaming'" class="message-actions">
                  <button v-if="item.content" :aria-label="copied === item.id ? 'Copied' : 'Copy response'" :title="copied === item.id ? 'Copied' : 'Copy response'" @click="copyMessage(item)"><Check v-if="copied === item.id" :size="18" /><Copy v-else :size="18" /><span class="sr-only">{{ copied === item.id ? 'Copied' : 'Copy' }}</span></button>
                  <button v-if="item.content" aria-label="Export conversation as Markdown" title="Export conversation" @click="exportChat"><ArrowDownToLine :size="18" /></button>
                  <button v-if="index === messages.length - 1" :aria-label="item.error || item.status === 'stopped' ? 'Retry' : 'Regenerate'" :title="item.error || item.status === 'stopped' ? 'Retry' : 'Regenerate'" :disabled="busy || loadingModels || !!connectionError || loadingChats || !!historyError" @click="retry"><RefreshCw :size="17" /><span class="sr-only">{{ item.error || item.status === 'stopped' ? 'Retry' : 'Regenerate' }}</span></button>
                  <span v-if="item.status === 'stopped'" class="stopped-label">Stopped</span>
                </div>
              </template>
            </div>
          </article>
        </section>
      </div>

      <div class="composer-region" :class="{ 'welcome-composer': !messages.length }">
        <button v-if="!followOutput && messages.length" class="scroll-latest" @click="followOutput = true; scrollToEnd(true)"><ArrowDown :size="16" /> Latest message</button>
        <p class="composer-caption">DeepSeek can make mistakes. Check important info.</p>
        <form class="composer" @submit.prevent="send()" @dragover.prevent @drop.prevent="dropImages">
          <input ref="imageInput" class="image-input" type="file" accept="image/jpeg,image/png,image/gif,image/webp" multiple aria-label="Choose images" @change="chooseImages" />
          <div v-if="pendingImages.length" class="pending-images"><div v-for="image in pendingImages" :key="image.id" class="pending-image"><img :src="image.preview" :alt="image.name" /><span>{{ image.name }}</span><button type="button" :disabled="busy" :aria-label="`Remove ${image.name}`" @click="removeImage(image.id)"><X :size="14" /></button></div></div>
          <p v-if="attachmentError" class="attachment-notice" role="alert">{{ attachmentError }}</p>
          <p v-else-if="needsVision && !selectedSupportsVision" class="attachment-notice" role="status">Choose a model marked Vision to continue this conversation.</p>
          <button type="button" class="composer-new icon-button" aria-label="Start a new conversation" title="New conversation" @click="newChat"><Plus :size="24" :stroke-width="1.7" /></button>
          <textarea ref="composer" v-model="draft" aria-label="Message DeepSeek" placeholder="Ask DeepSeek" rows="1" :disabled="busy" @keydown="composerKeydown" @paste="pasteImages"></textarea>
          <div class="composer-toolbar">
            <div class="model-picker"><select v-model="model" aria-label="DeepSeek model" :disabled="busy || loadingModels || !!connectionError || loadingChats || !!historyError"><option v-if="!models.length" value="">{{ loadingModels ? 'Connecting…' : 'No models available' }}</option><option v-if="model && !models.some(item => item.id === model)" :value="model">{{ model }}</option><option v-for="item in models" :key="item.id" :value="item.id" :disabled="needsVision && !supportsVision(item)">{{ item.name || item.id }}{{ supportsVision(item) ? ' · Vision' : '' }}</option></select><ChevronDown :size="14" /></div>
            <button type="button" class="attach-button icon-button" aria-label="Attach images" :disabled="busy || !selectedSupportsVision" :title="selectedSupportsVision ? 'Attach images' : 'Choose a Vision model to attach images'" @click="imageInput?.click()"><Paperclip :size="19" /></button>
            <button type="button" class="thinking-toggle icon-button" :class="{ active: thinking }" :aria-pressed="thinking" aria-label="Think deeper" :disabled="busy" :title="thinking ? 'Thinking mode on' : 'Enable thinking mode'" @click="thinking = !thinking"><Sparkles :size="20" /></button>
            <button v-if="busy" type="button" class="send-button stop-button" aria-label="Stop generating" title="Stop generating" @click="stop"><Square :size="16" fill="currentColor" /></button>
            <button v-else type="submit" class="send-button" :disabled="!canSend" aria-label="Send message" title="Send message"><ArrowUp :size="21" /></button>
          </div>
        </form>
      </div>
    </main>
  </div>
</template>
