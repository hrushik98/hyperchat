// Decode SSE across arbitrary HTTP and UTF-8 chunk boundaries.
export async function readStream(response, onEvent, signal) {
  if (!response.body) throw new Error('Streaming is unavailable in this browser.')
  const reader = response.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''
  let complete = false
  function dispatch(frame) {
    const data = frame.split('\n').filter(line => line.startsWith('data:'))
      .map(line => line.slice(5).trimStart()).join('\n')
    if (!data) return
    if (data === '[DONE]') { complete = true; return }
    const event = JSON.parse(data)
    if (event.error) throw new Error(event.error.message || 'DeepSeek could not finish the response.')
    onEvent(event)
  }
  try {
    while (!complete) {
      if (signal?.aborted) throw new DOMException('Stopped', 'AbortError')
      const { value, done } = await reader.read()
      buffer += decoder.decode(value, { stream: !done })
      // Normalize CRLF only after appending, so a split CRLF stays intact.
      buffer = buffer.replace(/\r\n/g, '\n')
      let end
      while ((end = buffer.indexOf('\n\n')) !== -1) {
        dispatch(buffer.slice(0, end))
        buffer = buffer.slice(end + 2)
        if (complete) break
      }
      if (done) {
        if (buffer.trim()) dispatch(buffer)
        break
      }
    }
    if (!complete) throw new Error('The connection ended before the response finished. Please retry.')
  } finally {
    await reader.cancel().catch(() => {})
    reader.releaseLock()
  }
}
