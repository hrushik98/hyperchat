import test from 'node:test'
import assert from 'node:assert/strict'
import { readStream } from './stream.js'

function streamResponse(text, split = 1) {
  const bytes = new TextEncoder().encode(text)
  return new Response(new ReadableStream({
    start(controller) {
      for (let i = 0; i < bytes.length; i += split) controller.enqueue(bytes.slice(i, i + split))
      controller.close()
    },
  }))
}

test('decodes Unicode and CRLF SSE events split across every byte', async () => {
  const events = []
  await readStream(streamResponse(': keep-alive\r\n\r\ndata: {"choices":[{"delta":{"content":"Hello 🌱"}}]}\r\n\r\ndata: [DONE]\r\n\r\n'), event => events.push(event))
  assert.equal(events[0].choices[0].delta.content, 'Hello 🌱')
  assert.equal(events.length, 1)
})

test('propagates an API error during a stream', async () => {
  await assert.rejects(readStream(streamResponse('data: {"error":{"message":"Try again"}}\n\n'), () => {}), /Try again/)
})

test('detects a dropped connection instead of silently completing', async () => {
  await assert.rejects(readStream(streamResponse('data: {"choices":[{"delta":{"content":"Partial"}}]}\n\n'), () => {}), /connection ended/)
})

test('handles several events in one chunk and DONE without a trailing newline', async () => {
  const events = []
  await readStream(streamResponse('data: {"choices":[{"delta":{"reasoning_content":"Thinking"}}]}\n\ndata: {"choices":[{"delta":{"content":"Answer"}}]}\n\ndata: [DONE]', 999), event => events.push(event))
  assert.equal(events.length, 2)
  assert.equal(events[1].choices[0].delta.content, 'Answer')
})

test('honors cancellation', async () => {
  const controller = new AbortController()
  controller.abort()
  await assert.rejects(readStream(streamResponse('data: [DONE]\n\n'), () => {}, controller.signal), { name: 'AbortError' })
})
