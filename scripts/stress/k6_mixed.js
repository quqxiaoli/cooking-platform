// scripts/stress/k6_mixed.js — 全面压测方案 Phase 6 混合场景
//
// 按真实流量配比 (§3) 并发跑 9 个场景，constant-arrival-rate executor。
// 比单个 wrk -R 高级在于：每个场景可以独立配 RPS，互不抢连接，
// 真正模拟「同一时间内不同端点按比例并发」的生产流量画像。
//
// 用法（在压测客户端机器上）：
//   k6 run \
//     -e BASE_URL=https://mellowck.com \
//     -e TOTAL_RPS=200 \
//     -e DURATION=10m \
//     scripts/stress/k6_mixed.js
//
// 前置：先跑 scripts/stress/seed_stress_data.sh 生成 /tmp/stress_pool.json
//
// 安装 k6（macOS）：  brew install k6
// 安装 k6（Linux）：  https://k6.io/docs/getting-started/installation/

import http from 'k6/http'
import { check, fail } from 'k6'
import { randomItem } from 'https://jslib.k6.io/k6-utils/1.4.0/index.js'

// ── 池数据 ─────────────────────────────────────────────────────────────────
// open() 在 init 阶段同步读文件，所有 VU 共享同一份数据
const pool = JSON.parse(open('/tmp/stress_pool.json'))
const USERS = pool.users   // [{phone, id, token}]
const POSTS = pool.posts   // [{id, scene_tag, author_id, title}]

if (USERS.length === 0 || POSTS.length === 0) {
  throw new Error('stress pool empty — re-run seed_stress_data.sh')
}

// 提取所有出现过的 scene_tag，用于 S7（避免压到空场景）
const SCENES = [...new Set(POSTS.map(p => p.scene_tag))]

// ── 配置 ───────────────────────────────────────────────────────────────────
const BASE_URL  = __ENV.BASE_URL  || 'https://mellowck.com'
const TOTAL_RPS = parseInt(__ENV.TOTAL_RPS || '200', 10)
const DURATION  = __ENV.DURATION  || '10m'

// 流量配比（§3，合计 100%）
const MIX = {
  feed:        40,  // S2  GET /feed
  detail:      25,  // S3  GET /posts/:id
  search:      10,  // S4  GET /search
  like:        12,  // S5  POST /posts/:id/like
  delete_like:  3,  // S6  DELETE /posts/:id/like
  scene_feed:   5,  // S7  GET /feed?scene_tag=N
  user_posts:   3,  // S8  GET /users/:id/posts
  follow:       2,  // S9  POST /users/:id/follow
}

// 每场景 rate = TOTAL_RPS × pct / 100，最小 1（k6 不接受 rate=0）
function rateFor(pct) {
  return Math.max(1, Math.round(TOTAL_RPS * pct / 100))
}

// preAllocatedVUs = rate × 2（假设 P99 ≤ 500ms）。maxVUs 给 3 倍 safety net。
function vusFor(rate) {
  return { preAllocatedVUs: Math.max(10, rate * 2), maxVUs: Math.max(20, rate * 4) }
}

function scenario(execName, pct, startTime) {
  const rate = rateFor(pct)
  return {
    executor:        'constant-arrival-rate',
    exec:            execName,
    rate:            rate,
    timeUnit:        '1s',
    duration:        DURATION,
    startTime:       startTime,
    ...vusFor(rate),
  }
}

// 全部场景同时 startTime=0 启动；用 5 个 ramp window 避免冷启动
export const options = {
  scenarios: {
    feed:        scenario('scenarioFeed',        MIX.feed,        '0s'),
    detail:      scenario('scenarioDetail',      MIX.detail,      '0s'),
    search:      scenario('scenarioSearch',      MIX.search,      '0s'),
    like:        scenario('scenarioLike',        MIX.like,        '0s'),
    delete_like: scenario('scenarioDeleteLike',  MIX.delete_like, '0s'),
    scene_feed:  scenario('scenarioSceneFeed',   MIX.scene_feed,  '0s'),
    user_posts:  scenario('scenarioUserPosts',   MIX.user_posts,  '0s'),
    follow:      scenario('scenarioFollow',      MIX.follow,      '0s'),
  },
  // 全局 SLO 阈值，触及就标红
  thresholds: {
    'http_req_duration{scenario:feed}':       ['p(99)<500'],
    'http_req_duration{scenario:detail}':     ['p(99)<500'],
    'http_req_duration{scenario:search}':     ['p(99)<800'],
    'http_req_duration{scenario:like}':       ['p(99)<300'],
    'http_req_failed':                        ['rate<0.01'],
  },
  // 摘要输出加 trend
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
}

// ── helpers ────────────────────────────────────────────────────────────────
function pickUser()  { return randomItem(USERS) }
function pickPost()  { return randomItem(POSTS) }
function pickScene() { return randomItem(SCENES) }
function pickFollowee(followerId) {
  // 防自关注：自旋找一个不同的 user
  let u
  do { u = pickUser() } while (u.id === followerId)
  return u
}

function check2xx(resp, name) {
  check(resp, {
    [`${name} status 2xx`]: r => r.status >= 200 && r.status < 300,
  })
}

// ── 8 个 scenario exec 函数 ─────────────────────────────────────────────────
export function scenarioFeed() {
  const resp = http.get(`${BASE_URL}/api/v1/feed?size=20`, {
    tags: { scenario: 'feed' },
  })
  check2xx(resp, 'feed')
}

export function scenarioDetail() {
  const post = pickPost()
  const resp = http.get(`${BASE_URL}/api/v1/posts/${post.id}`, {
    tags: { scenario: 'detail' },
  })
  check2xx(resp, 'detail')
}

export function scenarioSearch() {
  // 5 个热点关键词，随机挑一个
  const kws = ['verify', 'stress', 'test', 'feed', '搜索']
  const q = randomItem(kws)
  const resp = http.get(`${BASE_URL}/api/v1/search?q=${encodeURIComponent(q)}&size=20`, {
    tags: { scenario: 'search' },
  })
  check2xx(resp, 'search')
}

export function scenarioLike() {
  const user = pickUser()
  const post = pickPost()
  const resp = http.post(`${BASE_URL}/api/v1/posts/${post.id}/like`, null, {
    headers: { Authorization: `Bearer ${user.token}` },
    tags: { scenario: 'like' },
  })
  check2xx(resp, 'like')
}

export function scenarioDeleteLike() {
  const user = pickUser()
  const post = pickPost()
  const resp = http.del(`${BASE_URL}/api/v1/posts/${post.id}/like`, null, {
    headers: { Authorization: `Bearer ${user.token}` },
    tags: { scenario: 'delete_like' },
  })
  check2xx(resp, 'delete_like')
}

export function scenarioSceneFeed() {
  const s = pickScene()
  const resp = http.get(`${BASE_URL}/api/v1/feed?scene_tag=${s}&size=20`, {
    tags: { scenario: 'scene_feed' },
  })
  check2xx(resp, 'scene_feed')
}

export function scenarioUserPosts() {
  const u = pickUser()
  const resp = http.get(`${BASE_URL}/api/v1/users/${u.id}/posts?size=20`, {
    tags: { scenario: 'user_posts' },
  })
  check2xx(resp, 'user_posts')
}

export function scenarioFollow() {
  const follower = pickUser()
  const followee = pickFollowee(follower.id)
  const resp = http.post(`${BASE_URL}/api/v1/users/${followee.id}/follow`, null, {
    headers: { Authorization: `Bearer ${follower.token}` },
    tags: { scenario: 'follow' },
  })
  check2xx(resp, 'follow')
}

// ── setup / teardown 钩子（仅打印）─────────────────────────────────────────
export function setup() {
  const total = Object.values(MIX).reduce((a, b) => a + b, 0)
  console.log(`[k6_mixed] BASE_URL=${BASE_URL} TOTAL_RPS=${TOTAL_RPS} DURATION=${DURATION}`)
  console.log(`[k6_mixed] USERS=${USERS.length} POSTS=${POSTS.length} SCENES=${SCENES.length}`)
  console.log(`[k6_mixed] mix total = ${total}% (should be 100)`)
}
