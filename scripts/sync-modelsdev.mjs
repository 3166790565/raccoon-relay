#!/usr/bin/env node
/**
 * 生成 models.dev 的精简索引快照（仓库内检入，构建时 embed 进网关）。
 *
 * 口径与 internal/modelmeta/reduce.go 逐字一致（那边是运行期刷新链路的孪生
 * 实现）：改这里就要改那边，否则「刷新一次之后档位全变回通用表」—— 这种
 * 退化不报错、不抛异常，只是悄悄发生。
 *
 * 用法：node scripts/sync-modelsdev.mjs
 */
import { writeFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

const API_URL = 'https://models.dev/api.json'
const MODELS_URL = 'https://models.dev/models.json'
const OUT = fileURLToPath(new URL('../internal/modelmeta/modelsdev.json', import.meta.url))
const TIMEOUT_MS = 120_000

/** 索引键：取 id 末段（小写 + 只留字母数字）。与 Go 的 modelmeta.KeyOf 一致。 */
function keyOf(id) {
  const tail = String(id ?? '').split('/').pop() ?? ''
  return tail.toLowerCase().replace(/[^a-z0-9]/g, '')
}

function labOf(id) {
  const parts = String(id ?? '').split('/')
  return parts.length > 1 ? parts[0] : ''
}

async function getJson(url) {
  const response = await fetch(url, {
    signal: AbortSignal.timeout(TIMEOUT_MS),
    headers: { 'user-agent': 'raccoon-relay-modelsdev-sync' },
  })
  if (!response.ok) throw new Error(`${url} → HTTP ${response.status}`)
  return response.json()
}

function isChatModel(entry) {
  const type = entry?.type
  if (typeof type === 'string' && type !== 'chat') return false
  const output = entry?.modalities?.output
  if (Array.isArray(output) && !output.includes('text')) return false
  return true
}

function limitOf(value) {
  return typeof value === 'number' && Number.isFinite(value) && value > 0
    ? Math.trunc(value)
    : undefined
}

function modalitiesOf(entry) {
  const input = entry?.modalities?.input
  return Array.isArray(input) ? input : []
}

function levelsOf(entry) {
  const options = Array.isArray(entry?.reasoning_options) ? entry.reasoning_options : []
  const effort = options.find((option) => option?.type === 'effort')
  const values = Array.isArray(effort?.values)
    ? effort.values.filter((level) => typeof level === 'string' && level.trim() !== '')
    : []
  return values.length ? values : undefined
}

function recordOf(canonical, entry) {
  const input = modalitiesOf(entry)
  const record = {
    canonical,
    reasoning: entry.reasoning === true,
    toolCall: entry.tool_call === true,
    images: input.includes('image'),
    video: input.includes('video'),
  }
  const context = limitOf(entry.limit?.context)
  const output = limitOf(entry.limit?.output)
  if (context !== undefined) record.context = context
  if (output !== undefined) record.output = output
  return record
}

async function main() {
  const [api, models] = await Promise.all([getJson(API_URL), getJson(MODELS_URL)])
  const index = new Map()

  for (const [canonicalId, entry] of Object.entries(models ?? {})) {
    if (!isChatModel(entry)) continue
    const key = keyOf(canonicalId)
    if (!key) continue
    index.set(key, recordOf(canonicalId, entry))
  }

  const providers = Object.entries(api ?? {}).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
  const picked = new Map()
  for (const [providerId, provider] of providers) {
    for (const entry of Object.values(provider?.models ?? {})) {
      const canonical = typeof entry?.canonical_model_id === 'string' && entry.canonical_model_id
        ? entry.canonical_model_id
        : entry?.id
      const key = keyOf(canonical)
      if (!key) continue
      const levels = levelsOf(entry)
      if (!levels) continue
      const isLab = labOf(canonical) === providerId
      const previous = picked.get(key)
      if (previous?.isLab) continue
      if (previous && !isLab) continue
      picked.set(key, { levels, isLab })
    }
  }
  for (const [key, value] of picked) {
    const record = index.get(key)
    if (!record) continue
    record.levels = value.levels
  }

  const sorted = {}
  for (const key of [...index.keys()].sort()) sorted[key] = index.get(key)
  const payload = {
    generatedAt: Date.now(),
    source: 'https://models.dev',
    models: sorted,
  }
  writeFileSync(OUT, `${JSON.stringify(payload)}\n`)
  console.log(`写出 ${OUT}：${index.size} 条模型索引`)
}

main().catch((error) => {
  console.error(error)
  process.exit(1)
})
