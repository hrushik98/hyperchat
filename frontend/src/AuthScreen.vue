<script setup>
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { Check, Copy, Eye, EyeOff, LockKeyhole } from '@lucide/vue'

const props = defineProps({ setupRequired: Boolean, initialMode: { type: String, default: '' } })
const emit = defineEmits(['unlocked', 'cancel'])
const mode = ref(props.setupRequired ? 'setup' : props.initialMode || 'login')
const username = ref('')
const password = ref('')
const confirmation = ref('')
const recovery = ref('')
const showPassword = ref(false)
const busy = ref(false)
const error = ref('')
const recoveryKey = ref('')
const savedKey = ref(false)
const copied = ref(false)
const usernameInput = ref(null)
const heading = ref(null)
let unlockedAccount

const creating = computed(() => mode.value !== 'login')
const title = computed(() => recoveryKey.value ? 'Keep your recovery key.' : mode.value === 'setup' ? 'Make yourself at home.' : mode.value === 'reset' ? 'A fresh set of keys.' : 'Welcome back.')
const description = computed(() => mode.value === 'setup' ? 'Create the owner account for this installation. Your master password keeps your conversations private.' : mode.value === 'reset' ? 'Use your recovery key to choose a new username and master password. Your conversations will stay here.' : 'Enter your username and master password to unlock your workspace.')

watch(mode, async () => {
  password.value = ''
  confirmation.value = ''
  recovery.value = ''
  error.value = ''
  showPassword.value = false
  await nextTick()
  usernameInput.value?.focus()
})
onMounted(() => usernameInput.value?.focus())

async function submit() {
  if (busy.value) return
  error.value = ''
  if (creating.value && [...password.value].length < 12) { error.value = 'Choose a master password of at least 12 characters.'; return }
  if (new TextEncoder().encode(password.value).length > 72) { error.value = 'Your master password must be at most 72 bytes. Use fewer characters.'; return }
  if (creating.value && password.value !== confirmation.value) { error.value = 'The master passwords do not match.'; return }
  busy.value = true
  const controller = new AbortController()
  const timeout = setTimeout(() => controller.abort(), 15000)
  try {
    const response = await fetch(`/api/auth/${mode.value}`, {
      method: 'POST', credentials: 'same-origin', signal: controller.signal,
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ username: username.value.trim(), password: password.value, ...(mode.value === 'reset' ? { recoveryKey: recovery.value.trim() } : {}) }),
    })
    const result = await response.json()
    if (!response.ok) {
      if (response.status === 409 && mode.value === 'setup') {
        mode.value = 'login'
        await nextTick()
      }
      throw new Error(result.error?.message || 'Could not unlock the workspace. Please retry.')
    }
    if (result.authenticated !== true || !result.username || (creating.value && !result.recoveryKey)) throw new Error('Could not confirm your account. Reload the page and sign in.')
    password.value = ''
    confirmation.value = ''
    recovery.value = ''
    if (result.recoveryKey) {
      recoveryKey.value = result.recoveryKey
      unlockedAccount = { username: result.username }
      await nextTick()
      heading.value?.focus()
    } else emit('unlocked', { username: result.username })
  } catch (cause) { error.value = cause.name === 'AbortError' ? 'The server took too long. Reload the page to check whether your account was saved.' : cause.message }
  finally { clearTimeout(timeout); busy.value = false }
}

async function copyKey() {
  try { await navigator.clipboard.writeText(recoveryKey.value); copied.value = true }
  catch { error.value = 'Select and copy the recovery key, or download it below.' }
}

function downloadKey() {
  const url = URL.createObjectURL(new Blob([`Harper recovery key\n\nUsername: ${unlockedAccount.username}\nRecovery key: ${recoveryKey.value}\n\nKeep this private. It can reset your owner account. Each reset replaces this key.\n`], { type: 'text/plain;charset=utf-8' }))
  const link = document.createElement('a')
  link.href = url
  link.download = 'harper-recovery-key.txt'
  link.click()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}

function continueToChat() {
  if (!savedKey.value) return
  recoveryKey.value = ''
  emit('unlocked', unlockedAccount)
  unlockedAccount = null
}
</script>

<template>
  <main class="auth-page">
    <section class="auth-panel" aria-labelledby="auth-title">
      <a class="brand auth-brand" href="/"><img src="/favicon.svg" alt="" />harper<span class="brand-dot">.</span></a>
      <div class="auth-eyebrow"><LockKeyhole :size="14" /><span>YOUR PRIVATE WORKSPACE</span></div>
      <h1 id="auth-title" ref="heading" tabindex="-1">{{ title }}</h1>
      <template v-if="recoveryKey">
        <p class="auth-description">Save this somewhere private. You’ll need it if you forget your username or master password. This key is shown only once.</p>
        <code class="recovery-key">{{ recoveryKey }}</code>
        <div class="recovery-actions"><button type="button" @click="copyKey"><Check v-if="copied" :size="15" /><Copy v-else :size="15" />{{ copied ? 'Copied' : 'Copy key' }}</button><button type="button" @click="downloadKey">Download key</button></div>
        <p v-if="error" class="auth-error" role="alert">{{ error }}</p>
        <label class="recovery-saved"><input v-model="savedKey" type="checkbox" />I saved my recovery key somewhere safe.</label>
        <button class="auth-submit" :disabled="!savedKey" @click="continueToChat">Open my workspace</button>
      </template>
      <template v-else>
        <p class="auth-description">{{ description }}</p>
        <form class="auth-form" @submit.prevent="submit">
          <label v-if="mode === 'reset'" for="recovery">Recovery key</label>
          <input v-if="mode === 'reset'" id="recovery" v-model="recovery" name="recoveryKey" type="text" autocomplete="off" spellcheck="false" placeholder="HARPER-…" :disabled="busy" required />
          <label for="username">{{ mode === 'reset' ? 'New username' : 'Username' }}</label>
          <input id="username" ref="usernameInput" v-model="username" name="username" type="text" autocomplete="username" autocapitalize="none" spellcheck="false" pattern="[a-zA-Z0-9_.\-]{1,64}" maxlength="64" :disabled="busy" required :aria-describedby="creating ? 'username-hint' : undefined" />
          <p v-if="creating" id="username-hint" class="auth-hint">Letters, numbers, dots, underscores and hyphens.</p>
          <label for="password">{{ mode === 'reset' ? 'New master password' : 'Master password' }}</label>
          <div class="password-field"><input id="password" v-model="password" name="password" :type="showPassword ? 'text' : 'password'" :autocomplete="creating ? 'new-password' : 'current-password'" :disabled="busy" required :aria-describedby="creating ? 'password-hint' : undefined" /><button type="button" :aria-label="showPassword ? 'Hide master password' : 'Show master password'" :aria-pressed="showPassword" @click="showPassword = !showPassword"><EyeOff v-if="showPassword" :size="18" /><Eye v-else :size="18" /></button></div>
          <p v-if="creating" id="password-hint" class="auth-hint">At least 12 characters. A memorable passphrase works well.</p>
          <label v-if="creating" for="confirm-password">Confirm master password</label>
          <input v-if="creating" id="confirm-password" v-model="confirmation" name="confirmation" :type="showPassword ? 'text' : 'password'" autocomplete="new-password" :disabled="busy" required />
          <p v-if="error" class="auth-error" role="alert">{{ error }}</p>
          <button class="auth-submit" type="submit" :disabled="busy">{{ busy ? 'Please wait…' : mode === 'setup' ? 'Create my account' : mode === 'reset' ? 'Reset my account' : 'Unlock workspace' }}</button>
        </form>
        <button v-if="mode === 'login'" class="auth-link" type="button" :disabled="busy" @click="mode = 'reset'">Forgot your username or password?</button>
        <button v-if="mode === 'reset'" class="auth-link" type="button" :disabled="busy" @click="mode = 'login'; emit('cancel')">Back to sign in</button>
        <p v-if="mode === 'reset'" class="auth-hint auth-reset-note">Resetting signs out other browsers and gives you a new recovery key.</p>
        <p v-if="mode === 'setup'" class="auth-hint auth-reset-note">One installation, one owner. You’ll receive a recovery key next.</p>
      </template>
    </section>
  </main>
</template>
