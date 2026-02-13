#!/usr/bin/env node
// Dumps the offered-course catalogue from the portal WebSocket into a static
// JSON API under docs/, which GitHub Pages serves for the sniper to read.
//
// Run once per semester, after the course list is finalised:
//
//   EDU_TOKEN='<Authorization header value>' node tools/catalogue/dump.mjs
//
// Requires Node 22+ for the global WebSocket. No dependencies.

import { writeFileSync, mkdirSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..', '..')
const OUT = resolve(ROOT, 'docs', 'api')
const WS_URL = 'wss://my.edu.sharif.edu/api/ws?token='
const TIMEOUT_MS = 60000

const token = (process.env.EDU_TOKEN || '').trim()
if (!token) {
  console.error('EDU_TOKEN is not set.')
  console.error('Log in at https://my.edu.sharif.edu, copy the Authorization')
  console.error('request header from any API call, then re-run:')
  console.error("  EDU_TOKEN='<token>' node tools/catalogue/dump.mjs")
  process.exit(2)
}

// Only these fields are ever copied out of a course record. Everything the
// socket sends about the logged-in student (id, favorites, jobs, enrolled
// courses) lives in a different frame type and is never read here.
function compact(course) {
  return {
    u: course.units,
    v: course.isVariable ? 1 : 0,
    t: String(course.title || '').trim(),
    c: course.capacity,
  }
}

const label = process.env.SEMESTER || ''
const ws = new WebSocket(WS_URL + encodeURIComponent(token))
const started = Date.now()
let done = false

const fail = (msg, code = 1) => {
  console.error('error: ' + msg)
  process.exit(code)
}

const timer = setTimeout(() => {
  if (!done) fail(`no catalogue received within ${TIMEOUT_MS / 1000}s`)
}, TIMEOUT_MS)

ws.onerror = () => fail('websocket connection failed')

ws.onclose = (e) => {
  if (!done) fail(`socket closed before the catalogue arrived (code ${e.code})`)
}

ws.onmessage = (ev) => {
  let frame
  try {
    frame = JSON.parse(ev.data)
  } catch {
    return
  }

  if (frame.type === 'userState') {
    // Personal data. Read nothing from it, and never write it anywhere.
    return
  }
  if (frame.type !== 'listUpdate' || !Array.isArray(frame.message)) return

  const list = frame.message
  // Deltas are small arrays of changed courses. Wait for the full snapshot.
  if (list.length < 100) {
    console.log(`ignoring a ${list.length}-course delta, waiting for the full list`)
    return
  }

  done = true
  clearTimeout(timer)

  const courses = {}
  let variable = 0
  for (const c of list) {
    if (!c || typeof c.id !== 'string') continue
    if (typeof c.units !== 'number') fail(`course ${c.id} has no units field`)
    courses[c.id] = compact(c)
    if (courses[c.id].v) variable++
  }

  const ids = Object.keys(courses)
  if (ids.length < 100) fail(`only ${ids.length} courses parsed, refusing to publish`)

  const meta = {
    generatedAt: new Date().toISOString(),
    courseCount: ids.length,
    variableUnitCourses: variable,
    semester: label,
    source: 'my.edu.sharif.edu websocket listUpdate',
    note: 'capacity is a snapshot taken at generation time and goes stale quickly',
    fields: { u: 'units', v: 'isVariable', t: 'title', c: 'capacity at dump time' },
  }

  // Belt and braces: the serialised payload must not contain the token or any
  // student identifier before it is written to a file that gets committed.
  const payload = JSON.stringify(courses)
  if (payload.includes(token)) fail('refusing to write: token leaked into output')
  for (const key of ['studentId', 'favorites', 'remainingActions', 'unitsLimit']) {
    if (payload.includes(key)) fail(`refusing to write: found ${key} in output`)
  }

  mkdirSync(OUT, { recursive: true })
  writeFileSync(resolve(OUT, 'courses.json'), payload + '\n')
  writeFileSync(resolve(OUT, 'meta.json'), JSON.stringify(meta, null, 2) + '\n')

  const kb = (payload.length / 1024).toFixed(0)
  console.log(`wrote docs/api/courses.json  ${ids.length} courses, ${kb} KB`)
  console.log(`wrote docs/api/meta.json     generatedAt ${meta.generatedAt}`)
  console.log(`took ${((Date.now() - started) / 1000).toFixed(1)}s`)
  console.log('\nreview the diff, then commit and push so Pages picks it up.')
  ws.close()
  process.exit(0)
}
