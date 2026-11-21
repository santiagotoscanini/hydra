// Loaded by Node before the app (NODE_OPTIONS=--require) in a service marked
// `routed: node`. One copy of that service serves every worktree, so each
// request has to keep the worktree it came in for on the calls it makes.
//
//   in:  the router sets X-Hydra-Worktree on a request it passes in.
//   out: calls made while serving that request, to an address in
//        HYDRA_ROUTED_ORIGINS, carry the same header, and the router sends
//        them to that worktree's copy of the target.
//
// Values a proxy can't route (a queue name, a Temporal namespace) are listed
// in HYDRA_PER_REQUEST and arrive per request in X-Hydra-Upstreams. An app
// can read them with globalThis.__hydra.upstream('ENV_NAME'); for Temporal
// this hook switches the namespace itself (see below), so the app is left
// as it is.
'use strict'
const { AsyncLocalStorage } = require('node:async_hooks')
const http = require('node:http')
const dc = require('node:diagnostics_channel')
const Module = require('node:module')
const path = require('node:path')

const TAG = 'x-hydra-worktree'
const UPSTREAMS = 'x-hydra-upstreams'
const origins = (process.env.HYDRA_ROUTED_ORIGINS || '').split(',').filter(Boolean)
const routed = (origin) => origins.includes(String(origin))
const perRequest = (process.env.HYDRA_PER_REQUEST || '').split(',').filter(Boolean)
const als = new AsyncLocalStorage()
const state = { cache: false, temporal: false }

// The value to use instead of `value` for the request being served: when
// `value` is what a per-request variable holds in this process (main's queue
// name, say), the worktree's value of that variable.
function perRequestValue(value) {
  const ups = als.getStore()?.upstreams
  if (!ups) return value
  for (const env of perRequest) {
    if (process.env[env] === value && ups[env] !== undefined) return ups[env]
  }
  return value
}

globalThis.__hydra = {
  worktree: () => als.getStore()?.tag,
  upstream: (name) => als.getStore()?.upstreams?.[name],
}

function parseUpstreams(header) {
  const out = {}
  for (const pair of String(header || '').split(';')) {
    const i = pair.indexOf('=')
    if (i > 0) out[pair.slice(0, i).trim()] = decodeURIComponent(pair.slice(i + 1))
  }
  return out
}

// In: every request (and websocket upgrade) is handled inside its own store.
const emit = http.Server.prototype.emit
http.Server.prototype.emit = function (event, req, ...rest) {
  if ((event !== 'request' && event !== 'upgrade') || !req || !req.headers) {
    return emit.call(this, event, req, ...rest)
  }
  const store = { tag: req.headers[TAG], upstreams: parseUpstreams(req.headers[UPSTREAMS]) }
  if (event === 'request' && req.url === '/__hydra/probe') {
    return als.run(store, () => (probe(rest[0]), true))
  }
  return als.run(store, () => emit.call(this, event, req, ...rest))
}

// Out: fetch (undici announces each request here, whoever wrapped fetch) ...
dc.subscribe('undici:request:create', ({ request }) => {
  if (!routed(request.origin)) return
  request.addHeader('x-hydra-hook', '1')
  const tag = als.getStore()?.tag
  if (tag) request.addHeader(TAG, tag)
})
// ... and node:http / node:https clients, before the request is sent.
dc.subscribe('http.client.request.created', ({ request }) => {
  const origin = `${request.protocol}//${request.getHeader('host') || request.host}`
  if (!routed(origin)) return
  request.setHeader('x-hydra-hook', '1')
  const tag = als.getStore()?.tag
  if (tag) request.setHeader(TAG, tag)
})

// Next's data cache (fetch with force-cache or revalidate, unstable_cache) is
// keyed without the worktree, so one worktree's answer would be served to
// the others. Keep a set of entries per worktree.
const load = Module._load
Module._load = function (request) {
  const exported = load.apply(this, arguments)
  try {
    const Cache = exported && exported.IncrementalCache
    if (Cache && !Cache.__hydra && typeof Cache.prototype.get === 'function') {
      Cache.__hydra = true
      const { get, set } = Cache.prototype
      const key = (k) => {
        const tag = als.getStore()?.tag
        return tag ? `hydra-${tag}-${k}` : k
      }
      Cache.prototype.get = function (k, ctx) {
        return get.call(this, ctx && ctx.kind === 'FETCH' ? key(k) : k, ctx)
      }
      Cache.prototype.set = function (k, data, ctx) {
        return set.call(this, data && data.kind === 'FETCH' ? key(k) : k, data, ctx)
      }
      state.cache = true
    }
    if (perRequest.length) patchTemporal(exported)
  } catch {}
  return exported
}

// Temporal clients keep their namespace in `options.namespace` and read it
// on every call. Each client class is replaced by a subclass whose options
// answer with the worktree's namespace while one of its requests is being
// served: main's copy of the app then starts workflows where the worktree's
// own worker picks them up. Outside a request (or untagged) it is main's.
function patchTemporal(exported) {
  if (!exported || typeof exported !== 'object' || exported.__hydraTemporal) return
  for (const key of Object.keys(exported)) {
    const desc = Object.getOwnPropertyDescriptor(exported, key)
    const C = desc && 'value' in desc ? desc.value : undefined
    if (typeof C !== 'function' || !/Client$/.test(key) || C.__hydra || !isTemporalClient(C) || !desc.writable) continue
    const Patched = class extends C {
      constructor(...args) {
        super(...args)
        dynamicNamespace(this.options)
      }
    }
    Object.defineProperty(Patched, 'name', { value: C.name })
    Patched.__hydra = true
    exported[key] = Patched
    state.temporal = true
  }
  Object.defineProperty(exported, '__hydraTemporal', { value: true })
}

function isTemporalClient(C) {
  for (let p = Object.getPrototypeOf(C); p && p !== Function.prototype; p = Object.getPrototypeOf(p)) {
    if (p.name === 'BaseClient' && typeof p.prototype.withDeadline === 'function') return true
  }
  return false
}

function dynamicNamespace(options) {
  if (!options || typeof options !== 'object') return
  const desc = Object.getOwnPropertyDescriptor(options, 'namespace')
  if (!desc || !('value' in desc)) return // absent, or already done
  let base = desc.value
  Object.defineProperty(options, 'namespace', {
    enumerable: true,
    configurable: true,
    get: () => perRequestValue(base),
    set: (v) => { base = v },
  })
}

// Checks the switch on the Temporal SDK this app resolves: a client built
// with main's namespace has to answer with the worktree's inside a request.
let probeClient
function checkTemporal() {
  const env = perRequest.find((e) => process.env[e] !== undefined)
  if (!env) return 'unused'
  let Client
  try {
    Client = Module.createRequire(path.join(process.cwd(), 'noop.js'))('@temporalio/client').Client
  } catch {
    return 'unused' // not a Temporal app; the values are the app's to read
  }
  try {
    probeClient = probeClient || new Client({ namespace: process.env[env] })
    const want = 'hydra-probe'
    const got = als.run({ upstreams: { [env]: want } }, () =>
      [probeClient.options, probeClient.workflow.options, probeClient.schedule.options].map((o) => o.namespace),
    )
    if (got.some((v) => v !== want)) return `broken: the Temporal client kept namespace ${JSON.stringify(got[0])}`
    if (als.exit(() => probeClient.workflow.options.namespace) !== process.env[env]) return 'broken: the namespace outside a request changed'
    return 'ok'
  } catch (err) {
    return `broken: ${err && err.message}`
  }
}

// The router calls this to check the whole path before it lets a worktree
// use this copy: the tag it sent must come back from each routed address.
async function probe(res) {
  const calls = await Promise.all(
    origins.map(async (origin) => {
      try {
        const r = await fetch(`${origin}/__hydra/echo`, { cache: 'no-store' })
        return { origin, tag: (await r.json()).tag }
      } catch (err) {
        return { origin, error: String((err && err.cause && err.cause.message) || err) }
      }
    }),
  )
  res.setHeader('content-type', 'application/json')
  res.end(JSON.stringify({ hook: true, cache: state.cache, temporal: checkTemporal(), calls }))
}
