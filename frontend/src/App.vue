<script setup>
import { onBeforeUnmount, onMounted, ref } from 'vue'
import ChatWorkspace from './ChatWorkspace.vue'
import AuthScreen from './AuthScreen.vue'

const loading = ref(true)
const error = ref('')
const setupRequired = ref(false)
const authenticated = ref(false)
const username = ref('')
const resetting = ref(false)
const locking = ref(false)
let statusController

async function checkStatus() {
  statusController?.abort()
  statusController = new AbortController()
  const timeout = setTimeout(() => statusController?.abort(), 15000)
  loading.value = true
  error.value = ''
  try {
    const response = await fetch('/api/auth/status', { credentials: 'same-origin', signal: statusController.signal })
    const result = await response.json()
    if (!response.ok) throw new Error(result.error?.message || 'Could not check your account. Please retry.')
    if (typeof result.setupRequired !== 'boolean' || typeof result.authenticated !== 'boolean') throw new Error('Could not check your account. Please retry.')
    setupRequired.value = result.setupRequired
    authenticated.value = result.authenticated
    username.value = result.username || ''
  } catch (cause) {
    authenticated.value = false
    error.value = cause.name === 'AbortError' ? 'The server took too long to respond. Please retry.' : cause.message
  } finally {
    clearTimeout(timeout)
    loading.value = false
  }
}

function unlock(result) {
  setupRequired.value = false
  resetting.value = false
  username.value = result.username
  authenticated.value = true
  error.value = ''
}

function sessionExpired() {
  authenticated.value = false
  username.value = ''
  resetting.value = false
}

async function lock() {
  if (locking.value) return
  locking.value = true
  try {
    const response = await fetch('/api/auth/logout', { method: 'POST', credentials: 'same-origin' })
    if (!response.ok) throw new Error('Could not lock the workspace. Please retry.')
    sessionExpired()
    error.value = ''
  } catch (cause) { error.value = cause.message }
  finally { locking.value = false }
}

onMounted(() => {
  window.addEventListener('harper:unauthorized', sessionExpired)
  checkStatus()
})
onBeforeUnmount(() => {
  statusController?.abort()
  window.removeEventListener('harper:unauthorized', sessionExpired)
})
</script>

<template>
  <main v-if="loading || (!authenticated && error)" class="auth-page">
    <section class="auth-panel" aria-live="polite">
      <a class="brand auth-brand" href="/"><img src="/favicon.svg" alt="" />harper<span class="brand-dot">.</span></a>
      <p v-if="loading" class="auth-description" role="status">Opening your workspace…</p>
      <template v-else><p class="auth-error" role="alert">{{ error }}</p><button class="auth-submit" @click="checkStatus">Try again</button></template>
    </section>
  </main>
  <template v-else-if="authenticated && !resetting">
    <div v-if="error" class="notice" role="alert"><span>{{ error }}</span><button :disabled="locking" @click="lock">Retry locking</button></div>
    <ChatWorkspace :username="username" @logout="lock" @reset="resetting = true" />
  </template>
  <AuthScreen v-else :setup-required="setupRequired" :initial-mode="resetting ? 'reset' : ''" @unlocked="unlock" @cancel="resetting = false" />
</template>
