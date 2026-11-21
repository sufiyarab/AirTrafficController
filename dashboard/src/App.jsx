import { useEffect, useRef, useState } from 'react'

const SIZE = 200 // NM, must match the gateway's airspace
const PX = 900 // drawing coordinates; the canvas is redrawn at whatever size it is shown, so it stays sharp
const SIDE_MIN = 300 // px kept for the side panel when the radar is dragged wider
const RADAR_MIN = 320
const SIZE_KEY = 'atc.radarSize'

// Radar size that fits the window, as before the divider existed.
function fitSize() {
  return Math.max(RADAR_MIN, Math.min(PX, window.innerHeight - 34, window.innerWidth - SIDE_MIN - 48))
}

// Radar size in CSS pixels: chosen by dragging the divider, remembered in this
// browser (localStorage can be unavailable, so it is optional).
function useRadarSize() {
  const [size, setSize] = useState(() => {
    try {
      const v = Number(localStorage.getItem(SIZE_KEY))
      if (v >= RADAR_MIN) return v
    } catch { /* no storage: use the fitted size */ }
    return fitSize()
  })
  const choose = (v) => {
    const clamped = Math.round(Math.max(RADAR_MIN, Math.min(v, window.innerWidth - SIDE_MIN - 48)))
    setSize(clamped)
    try { localStorage.setItem(SIZE_KEY, String(clamped)) } catch { /* ignore */ }
  }
  const reset = () => {
    try { localStorage.removeItem(SIZE_KEY) } catch { /* ignore */ }
    setSize(fitSize())
  }
  return [size, choose, reset]
}

// Vertical divider between the radar and the side panel. Drag to resize the
// radar; double-click to fit it to the window again.
function Divider({ size, onResize, onReset }) {
  const start = useRef(null)
  return (
    <div className="divider" role="separator" aria-orientation="vertical" aria-valuenow={size}
      title="Drag to resize the radar · double-click to fit the window"
      onPointerDown={(e) => { e.currentTarget.setPointerCapture(e.pointerId); start.current = { x: e.clientX, size } }}
      onPointerMove={(e) => start.current && onResize(start.current.size + e.clientX - start.current.x)}
      onPointerUp={() => { start.current = null }}
      onDoubleClick={onReset} />
  )
}
const CLICK_RADIUS = 18 // screen pixels, whatever size the canvas is shown at
const LABEL_ALL_UP_TO = 40 // above this many aircraft, "all labels" can be unticked to label only the ones that matter (see wantsLabel)
const POPOVER_W = 300 // px, matches .popover in style.css

// Receives the gateway's picture over a WebSocket, reconnecting on drop.
function usePicture() {
  const [pic, setPic] = useState(null)
  const [connected, setConnected] = useState(false)
  useEffect(() => {
    let ws, timer, closed = false
    const open = () => {
      const proto = location.protocol === 'https:' ? 'wss' : 'ws'
      ws = new WebSocket(`${proto}://${location.host}/ws`)
      ws.onopen = () => setConnected(true)
      ws.onmessage = (e) => setPic(JSON.parse(e.data))
      ws.onclose = () => {
        setConnected(false)
        if (!closed) timer = setTimeout(open, 1000)
      }
    }
    open()
    return () => { closed = true; clearTimeout(timer); ws && ws.close() }
  }, [])
  return [pic, connected]
}

// Aircraft colour: red for SUSPECT (failed the receiver cross-check), amber
// while TRANSFERRING, grey while UNVERIFIED (not enough receiver coverage
// yet), otherwise green.
function aircraftColor(ac) {
  if (ac.verify === 'SUSPECT') return '#ff4d4d'
  if (ac.status === 'TRANSFERRING') return '#ffb000'
  if (ac.verify === 'UNVERIFIED') return '#8a9a90'
  return '#b8f5c8'
}

// With many aircraft, a label on every one hides the picture. Label the ones a
// controller is looking at or should look at; the rest show as a dot and a
// leader line.
function wantsLabel(ac, n, allLabels, selected, hovered, inConflict) {
  return allLabels || n <= LABEL_ALL_UP_TO || ac.callsign === selected || ac.callsign === hovered ||
    ac.verify === 'SUSPECT' || ac.status === 'TRANSFERRING' || inConflict.has(ac.callsign)
}

// Conflict colours: red once separation is lost, amber while only predicted.
const LOSS_COLOR = '#ff4d4d'
const PREDICTED_COLOR = '#ffb000'


// Radar canvas, plus the clearance popover floating next to the selected
// aircraft (children). The popover follows the aircraft on every update and
// flips to its left side, or upwards, near the edges.
function Radar({ pic, selected, onSelect, coverage, allLabels, size, children }) {
  const ref = useRef(null)
  const wrap = useRef(null)
  const [hovered, setHovered] = useState(null)
  const dpr = window.devicePixelRatio || 1
  const [, setResized] = useState(0)
  useEffect(() => { // re-place the popover when the canvas is resized
    const ro = new ResizeObserver(() => setResized((n) => n + 1))
    ro.observe(ref.current)
    return () => ro.disconnect()
  }, [])
  useEffect(() => {
    const ctx = ref.current.getContext('2d')
    // Draw in PX coordinates, scaled to the canvas's real pixels.
    const k = ref.current.width / PX
    ctx.setTransform(k, 0, 0, k, 0, 0)
    const s = PX / SIZE
    const toY = (y) => PX - y * s // north up
    ctx.clearRect(0, 0, PX, PX)

    ctx.strokeStyle = '#1f5a3a'
    ctx.fillStyle = '#3c8a5c'
    ctx.font = '12px monospace'
    for (const sec of pic?.sectors ?? []) {
      const [x, y, w, h] = sec.box
      const draining = sec.status === 'DRAINING' // amber and dashed while being merged away
      ctx.strokeStyle = draining ? '#ffb000' : '#1f5a3a'
      ctx.fillStyle = draining ? '#ffb000' : '#3c8a5c'
      ctx.setLineDash(draining ? [6, 4] : [])
      ctx.strokeRect(x * s, toY(y + h), w * s, h * s)
      ctx.setLineDash([])
      ctx.fillText(`${sec.name} · ${sec.load ?? 0} ac${draining ? ' · draining' : ''}`, x * s + 6, toY(y + h) + 16)
    }
    ctx.strokeStyle = '#1f5a3a'
    ctx.beginPath() // tower circle, matches the gateway's 15 NM radius
    ctx.arc(100 * s, toY(100), 15 * s, 0, 2 * Math.PI)
    ctx.stroke()

    // Ground receivers: triangles, grey when the gateway stopped hearing them.
    for (const rx of pic?.receivers ?? []) {
      const x = rx.x * s, y = toY(rx.y)
      const color = rx.up ? '#4fc3f7' : '#666'
      if (coverage) {
        ctx.strokeStyle = rx.up ? 'rgba(79,195,247,0.25)' : 'rgba(120,120,120,0.2)'
        ctx.setLineDash([4, 4])
        ctx.beginPath(); ctx.arc(x, y, rx.range * s, 0, 2 * Math.PI); ctx.stroke()
        ctx.setLineDash([])
      }
      ctx.fillStyle = color
      ctx.beginPath(); ctx.moveTo(x, y - 7); ctx.lineTo(x - 6, y + 5); ctx.lineTo(x + 6, y + 5); ctx.closePath(); ctx.fill()
      const label = rx.up ? rx.id : `${rx.id} DOWN`
      const w = ctx.measureText(label).width
      ctx.fillText(label, x + 8 + w > PX ? x - 8 - w : x + 8, y + 4) // keep labels on the canvas
    }

    // Conflicts (gateway conflict.go): a line between the two aircraft, solid
    // red when separation is already lost, dashed amber while predicted.
    const conflicts = pic?.conflicts?.active ?? []
    const inConflict = new Set(conflicts.flatMap((c) => [c.a, c.b]))
    const pos = Object.fromEntries((pic?.aircraft ?? []).map((a) => [a.callsign, a]))
    for (const c of conflicts) {
      const a = pos[c.a], b = pos[c.b]
      if (!a || !b) continue
      const color = c.loss ? LOSS_COLOR : PREDICTED_COLOR
      ctx.strokeStyle = color
      ctx.lineWidth = 2
      ctx.setLineDash(c.loss ? [] : [6, 4])
      ctx.beginPath(); ctx.moveTo(a.x * s, toY(a.y)); ctx.lineTo(b.x * s, toY(b.y)); ctx.stroke()
      ctx.setLineDash([])
      for (const m of [a, b]) { ctx.beginPath(); ctx.arc(m.x * s, toY(m.y), 7, 0, 2 * Math.PI); ctx.stroke() }
      ctx.lineWidth = 1
      ctx.fillStyle = color
      // below-left of the midpoint: callsign labels sit above-right of each aircraft
      const tag = c.loss ? 'LOSS' : `${Math.round(c.in_s)}s`
      ctx.fillText(tag, ((a.x + b.x) / 2) * s - 10 - ctx.measureText(tag).width, toY((a.y + b.y) / 2) + 18)
    }

    const n = pic?.aircraft?.length ?? 0
    for (const ac of pic?.aircraft ?? []) {
      const x = ac.x * s, y = toY(ac.y)
      const color = aircraftColor(ac)
      ctx.fillStyle = color
      ctx.fillRect(x - 2, y - 2, 4, 4)
      const r = (ac.heading * Math.PI) / 180 // velocity leader line
      ctx.strokeStyle = color
      ctx.beginPath(); ctx.moveTo(x, y); ctx.lineTo(x + 10 * Math.sin(r), y - 10 * Math.cos(r)); ctx.stroke()
      ctx.fillStyle = ac.verify === 'SUSPECT' ? '#ff4d4d' : '#7fd69a'
      if (wantsLabel(ac, n, allLabels, selected, hovered, inConflict)) {
        ctx.fillText(`${ac.callsign}${ac.verify === 'SUSPECT' ? '?' : ''} ${Math.round(ac.alt / 100)}`, x + 5, y - 5)
      }
      if (ac.callsign === selected) { // ring around the aircraft being cleared
        ctx.strokeStyle = '#ffffff'
        ctx.beginPath(); ctx.arc(x, y, 9, 0, 2 * Math.PI); ctx.stroke()
      }
      ctx.strokeStyle = '#1f5a3a'
    }
  }, [pic, selected, coverage, allLabels, hovered, size])

  // The aircraft nearest to a mouse event, within CLICK_RADIUS screen pixels.
  // The canvas can be shown smaller than PX (max-width: 100%), so the event is
  // scaled back to canvas pixels.
  const nearest = (e) => {
    const rect = ref.current.getBoundingClientRect()
    const k = PX / rect.width
    const cx = (e.clientX - rect.left) * k
    const cy = (e.clientY - rect.top) * k
    const s = PX / SIZE
    let best = null, bestD = CLICK_RADIUS * k
    for (const ac of pic?.aircraft ?? []) {
      const d = Math.hypot(ac.x * s - cx, PX - ac.y * s - cy)
      if (d < bestD) { best = ac.callsign; bestD = d }
    }
    return best
  }

  // Popover position in CSS pixels, from the aircraft's position on the canvas
  // as it is shown now.
  let pop = null
  const ac = (pic?.aircraft ?? []).find((a) => a.callsign === selected)
  if (ac && children && ref.current && wrap.current) {
    const rect = ref.current.getBoundingClientRect()
    const box = wrap.current.getBoundingClientRect()
    const k = rect.width / PX, s = PX / SIZE
    const ax = rect.left - box.left + ac.x * s * k
    const ay = rect.top - box.top + (PX - ac.y * s) * k
    const right = ax + 16 + POPOVER_W <= box.width
    pop = {
      left: right ? ax + 16 : Math.max(0, ax - 16 - POPOVER_W),
      top: ay > box.height * 0.6 ? undefined : Math.max(0, ay - 24),
      bottom: ay > box.height * 0.6 ? Math.max(0, box.height - ay - 24) : undefined,
    }
  }

  return (
    <div className="radar-wrap" ref={wrap}>
      <canvas ref={ref} width={Math.round(size * dpr)} height={Math.round(size * dpr)}
        style={{ width: size, height: size, cursor: hovered ? 'pointer' : 'crosshair' }}
        onClick={(e) => onSelect(nearest(e))}
        onMouseMove={(e) => setHovered(nearest(e))}
        onMouseLeave={() => setHovered(null)} />
      {pop && <div className="popover" style={pop} role="dialog" aria-label={`clearance for ${selected}`}>
        <button className="close" onClick={() => onSelect(null)} aria-label="close">×</button>
        {children}
      </div>}
    </div>
  )
}

const PRESETS = ['CLIMB FL350', 'DESCEND FL250', 'HEADING 090', 'HEADING 270', 'REDUCE SPEED 250']

// Controller login. The token lives only in React state (not localStorage),
// so reloading the page logs out; fine for a demo, and nothing for another
// script on the page to steal later.
function Login({ pic, session, setSession }) {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState(null)
  if (!pic?.auth) return null // gateway runs with AUTH=off

  const sec = pic.security
  const counters = sec && <div className="hint">
    {sec.logins} logins · {sec.failed_logins} failed · {sec.unauthorized} refused (no login) · {sec.forbidden} refused (wrong sector)
  </div>

  if (session) {
    const mins = Math.max(0, Math.round((session.expires_at_ms - (pic.time ?? Date.now())) / 60000))
    return (
      <div className="login">
        <h2>controller</h2>
        <div className="row">
          <span>{session.username} · {session.role} · {session.role === 'supervisor' ? 'all sectors' : session.sectors.join(', ')} · {mins} min left</span>
          <button onClick={() => setSession(null)}>Log out</button>
        </div>
        {counters}
      </div>
    )
  }

  const submit = async (e) => {
    e.preventDefault()
    setError(null)
    try {
      const res = await fetch('/api/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ username, password }),
      })
      const body = await res.json()
      if (!res.ok) { setError(body.error ?? `login failed (${res.status})`); return }
      setPassword('')
      setSession(body)
    } catch (err) {
      setError(`gateway unreachable: ${err.message}`)
    }
  }
  return (
    <form className="login" onSubmit={submit}>
      <h2>controller login</h2>
      <div className="row">
        <input value={username} onChange={(e) => setUsername(e.target.value)} placeholder="username" autoComplete="username" />
        <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} placeholder="password" autoComplete="current-password" />
        <button type="submit" disabled={!username || !password}>Log in</button>
      </div>
      {error && <div className="result rejected">{error}</div>}
      {counters}
    </form>
  )
}

// Why the logged-in controller cannot clear this aircraft, or null. The
// gateway enforces all of these anyway; this only says why up front.
function blockedReason(pic, ac, session) {
  if (ac?.verify === 'SUSPECT') return 'failed position verification (SUSPECT)'
  if (!pic?.auth) return null
  if (!session) return 'log in to issue clearances'
  if (session.role !== 'supervisor' && ac?.owner && !session.sectors.includes(ac.owner)) {
    return `controlled by ${ac.owner}, not one of your sectors`
  }
  return null
}

// Clearance form for the selected aircraft.
function ClearanceForm({ pic, selected, session, setSession }) {
  const [instruction, setInstruction] = useState('')
  const [issuedBy, setIssuedBy] = useState('ctrl-demo') // only used while login is off
  const [result, setResult] = useState(null)
  const ac = (pic?.aircraft ?? []).find((a) => a.callsign === selected)

  const send = async (text) => {
    setResult({ status: 'sending' })
    const headers = { 'Content-Type': 'application/json' }
    if (session) headers.Authorization = `Bearer ${session.token}`
    try {
      const res = await fetch('/api/clearance', {
        method: 'POST',
        headers,
        body: JSON.stringify({ callsign: selected, instruction: text, issued_by: issuedBy }),
      })
      if (res.status === 401) setSession(null) // token expired or gateway restarted: log in again
      setResult(await res.json())
    } catch (err) {
      setResult({ status: 'rejected', reason: `gateway unreachable: ${err.message}` })
    }
  }

  if (!selected) return null
  const blocked = blockedReason(pic, ac, session)
  return (
    <div className="clearance-form">
      <h2>clearance · {selected}</h2>
      {ac
        ? <div className="hint">owner {ac.owner || '-'} · {ac.status || 'unclaimed'} · FL{Math.round(ac.alt / 100)} · hdg {Math.round(ac.heading)}{ac.verify ? ` · ${ac.verify}` : ''}</div>
        : <div className="hint">aircraft no longer on the radar</div>}
      {blocked && <div className="result rejected">clearances blocked: {blocked}</div>}
      <div className="row">
        <input value={instruction} onChange={(e) => setInstruction(e.target.value)}
          onKeyDown={(e) => e.key === 'Enter' && instruction.trim() && send(instruction)}
          placeholder="e.g. DESCEND FL250" maxLength={64} />
        <button onClick={() => send(instruction)} disabled={!instruction.trim() || !!blocked}>Send</button>
      </div>
      <div className="row presets">
        {PRESETS.map((p) => <button key={p} onClick={() => send(p)} disabled={!!blocked}>{p}</button>)}
      </div>
      {!pic?.auth && <div className="row">
        <label>controller <input value={issuedBy} onChange={(e) => setIssuedBy(e.target.value)} /></label>
      </div>}
      {result && <div className={`result ${result.status}`}>{describe(result)}</div>}
    </div>
  )
}

function describe(c) {
  if (c.error) return `rejected: ${c.error}`
  switch (c.status) {
    case 'sending': return 'sending...'
    case 'committed': return `committed in ${c.sector} at log index ${c.index}`
    case 'pending': return `pending: ${c.reason} (waiting on ${c.waiting_on})`
    default: return `${c.status}: ${c.reason ?? ''}`
  }
}

// Recent clearances from the gateway; buffered ones update live.
function Clearances({ pic }) {
  const list = pic?.clearances ?? []
  const st = pic?.clearance_stats
  const queued = Object.entries(pic?.clearance_queues ?? {})
  return (
    <div>
      <h2>clearances</h2>
      {st && <div className="hint">
        {st.committed} committed · {st.rejected} rejected · {st.expired} expired · {st.buffered} ever buffered
        {queued.length > 0 && ` · buffered now: ${queued.map(([c, n]) => `${c} ${n}`).join(', ')}`}
      </div>}
      <table><tbody>
        {list.slice(0, 10).map((c) => (
          <tr key={c.id} className={c.status}>
            <td>{c.callsign}</td><td>{c.instruction}</td>
            <td>{c.status}{c.index ? ` #${c.index}` : ''}</td>
            <td>{c.sector ?? ''}</td>
          </tr>
        ))}
      </tbody></table>
    </div>
  )
}

// Elastic sectors: each sector's load, buttons to split or merge by hand, and
// what the controller has done recently. The gateway splits a sector that stays
// over the limit into the idle standby cluster, and merges it back when quiet.
function Elastic({ pic, session }) {
  const el = pic?.elastic
  const [msg, setMsg] = useState(null)
  if (!el || !el.sectors?.length) return null
  const call = async (action, sector) => {
    setMsg('sending...')
    try {
      const headers = { 'Content-Type': 'application/json' }
      if (session) headers.Authorization = `Bearer ${session.token}` // splitting needs a supervisor login
      const res = await fetch(`/api/elastic/${action}`, {
        method: 'POST',
        headers,
        body: JSON.stringify({ sector }),
      })
      const body = await res.json()
      setMsg(res.ok ? `${action} ${sector}: ok` : `${action} ${sector}: ${body.error}`)
    } catch (err) {
      setMsg(`gateway unreachable: ${err.message}`)
    }
  }
  return (
    <div className="elastic">
      <h2>elastic sectors</h2>
      <div className="hint">
        split at {el.split_at} aircraft · merge at {el.merge_at} · {el.splits} splits · {el.merges} merges · map v{el.version}
        {el.enabled ? '' : ' · automatic control off'}
      </div>
      <table><tbody>
        {el.sectors.map((s) => (
          <tr key={s.name} className={s.status}>
            <td>{s.name}</td><td>{s.status}</td><td>{s.load} ac</td>
            <td>
              {s.status === 'ACTIVE' && <button onClick={() => call('split', s.name)}>split</button>}
              {s.status === 'ACTIVE' && s.spare && <button onClick={() => call('merge', s.name)}>merge</button>}
            </td>
          </tr>
        ))}
      </tbody></table>
      {msg && <div className="hint">{msg}</div>}
      <table><tbody>
        {el.events.slice(0, 6).map((e) => (
          <tr key={`${e.time_ms}-${e.kind}-${e.sector}`} className={e.kind === 'error' ? 'rejected' : ''}>
            <td>{new Date(e.time_ms).toLocaleTimeString()}</td><td>{e.kind}</td><td>{e.sector}</td><td>{e.detail}</td>
          </tr>
        ))}
      </tbody></table>
    </div>
  )
}

// Spoofed-aircraft alerts: aircraft whose claimed position disagrees with
// the ranges the ground receivers measured (gateway verify.go).
function Alerts({ pic, coverage, setCoverage, onSelect }) {
  const v = pic?.verify
  if (!v) return null // verification off (no receivers configured)
  const alerts = pic.alerts ?? []
  const counts = { VERIFIED: 0, UNVERIFIED: 0, SUSPECT: 0 }
  for (const a of pic.aircraft ?? []) counts[a.verify] = (counts[a.verify] ?? 0) + 1
  const rxs = pic.receivers ?? []
  return (
    <div>
      <h2>position verification {alerts.length > 0 && <span className="notready">{alerts.length} SUSPECT</span>}</h2>
      <div className="hint">
        {counts.VERIFIED} verified · {counts.UNVERIFIED} unverified · {counts.SUSPECT} suspect ·
        receivers {rxs.filter((r) => r.up).length}/{rxs.length} up ·{' '}
        <label><input type="checkbox" checked={coverage} onChange={(e) => setCoverage(e.target.checked)} /> coverage</label>
      </div>
      {alerts.length > 0 && <table><tbody>
        {alerts.map((a) => (
          <tr key={a.callsign} className="rejected" onClick={() => onSelect(a.callsign)} style={{ cursor: 'pointer' }}>
            <td>{a.callsign}?</td>
            <td>{a.reason}</td>
            <td>{((Date.now() - a.since_ms) / 1000).toFixed(0)}s</td>
          </tr>
        ))}
      </tbody></table>}
    </div>
  )
}

// Live safety invariants (gateway safety.go). Each rule is checked every second
// against what the clusters actually hold. Green means it has held at every
// check since the gateway started; red stays red and shows what broke it.
function Safety({ pic }) {
  const s = pic?.safety
  if (!s) return null
  const secs = Math.max(0, Math.floor((pic.time - s.since_ms) / 1000))
  const age = secs < 120 ? `${secs} s` : `${Math.floor(secs / 60)} min`
  return (
    <div className="safety">
      <h2>
        safety invariants{' '}
        {s.ok ? <span className="ready">ALL HOLD</span> : <span className="notready">VIOLATED</span>}
      </h2>
      <div className="hint">checked every second for {age}</div>
      <table><tbody>
        {s.invariants.map((i) => (
          <tr key={i.id} className={i.ok ? 'inv-ok' : 'inv-bad'} title={i.rule}>
            <td className="inv-mark">{i.ok ? '\u2713' : '\u2717'}</td>
            <td>
              {i.name}
              <div className="hint">{i.ok ? i.detail : i.last_violation}</div>
            </td>
            <td className="inv-count">{i.checks} checks{i.violations > 0 ? ` · ${i.violations} broken` : ''}</td>
          </tr>
        ))}
      </tbody></table>
      {s.recent?.length > 0 && <table><tbody>
        {s.recent.slice(0, 5).map((v) => (
          <tr key={`${v.ms}-${v.id}-${v.msg}`} className="rejected">
            <td>{new Date(v.ms).toLocaleTimeString()}</td><td>{v.msg}</td>
          </tr>
        ))}
      </tbody></table>}
    </div>
  )
}

// Conflicts (gateway conflict.go): pairs predicted to come within 5 NM and
// 1,000 ft in the look-ahead window, soonest first, each with a suggested
// climb or descent. The button issues it as an ordinary clearance, so login,
// sector rights and the owner sector's Raft log all apply.
function Conflicts({ pic, session, setSession, onSelect }) {
  const [results, setResults] = useState({}) // conflict key -> clearance answer
  const cf = pic?.conflicts
  if (!cf) return null
  const st = cf.stats
  const byCS = Object.fromEntries((pic.aircraft ?? []).map((a) => [a.callsign, a]))
  const issue = async (c) => {
    const sg = c.suggestion
    setResults((r) => ({ ...r, [c.key]: { status: 'sending' } }))
    const headers = { 'Content-Type': 'application/json' }
    if (session) headers.Authorization = `Bearer ${session.token}`
    try {
      const res = await fetch('/api/clearance', {
        method: 'POST', headers,
        body: JSON.stringify({ callsign: sg.callsign, instruction: sg.instruction, issued_by: 'conflict-advisory' }),
      })
      if (res.status === 401) setSession(null)
      const body = await res.json()
      setResults((r) => ({ ...r, [c.key]: body }))
    } catch (err) {
      setResults((r) => ({ ...r, [c.key]: { status: 'rejected', reason: `gateway unreachable: ${err.message}` } }))
    }
  }
  return (
    <div className="conflicts">
      <h2>conflicts (5 NM / 1,000 ft, {st.lookahead_s}s look-ahead){' '}
        {cf.active.length > 0 && <span className="notready">{cf.active.length} active</span>}
      </h2>
      <div className="hint">
        grid checked {st.pairs_checked} of {st.all_pairs} pairs in {st.last_ms.toFixed(2)} ms · {st.detected} detected ·{' '}
        {st.resolved} cleared before a loss · {st.losses} losses (avg warning {st.avg_warning_s.toFixed(0)}s)
      </div>
      {cf.active.length === 0 && <div className="hint">no conflicts predicted</div>}
      <table><tbody>
        {cf.active.map((c) => {
          const sg = c.suggestion
          const blocked = sg ? blockedReason(pic, byCS[sg.callsign], session) : null
          const r = results[c.key]
          return (
            <tr key={c.key} className={c.loss ? 'conflict-loss' : 'conflict-predicted'}>
              <td style={{ cursor: 'pointer' }} onClick={() => onSelect(c.a)}>{c.a} / {c.b}</td>
              <td>{c.loss ? 'LOSS' : `in ${Math.round(c.in_s)}s`}</td>
              <td>{c.min_nm.toFixed(1)} NM · {Math.round(c.vert_ft)} ft</td>
              <td>
                {sg
                  ? <button onClick={() => issue(c)} disabled={!!blocked} title={blocked ?? `issued in ${sg.sector || 'its sector'}`}>
                      {sg.callsign} {sg.instruction}
                    </button>
                  : <span className="hint">no safe level</span>}
                {r && <div className={`hint ${r.status}`}>{r.status}{r.reason ? `: ${r.reason}` : ''}</div>}
              </td>
            </tr>
          )
        })}
      </tbody></table>
    </div>
  )
}

function Clusters({ pic }) {
  const names = Object.keys(pic?.clusters ?? {}).sort()
  return names.map((c) => (
    <div key={c}>
      <h2>{c} <ReadyTag r={pic.ready?.[c]} /></h2>
      <table><tbody>
        {pic.clusters[c].map((n) => (
          <tr key={n.id} className={n.up ? n.state : 'down'}>
            <td>{n.id}</td><td>{n.up ? n.state : 'DOWN'}</td><td>term {n.term ?? '-'}</td><td>commit {n.commit_index ?? '-'}</td>
          </tr>
        ))}
      </tbody></table>
    </div>
  ))
}

// Gateway's Ready flag for one cluster: one leader, recently polled.
function ReadyTag({ r }) {
  if (!r) return null
  if (r.ready) return <span className="ready">READY</span>
  const secs = r.since_ms ? ((Date.now() - r.since_ms) / 1000).toFixed(1) : null
  return <span className="notready">NOT READY ({r.reason}{secs ? `, ${secs}s` : ''})</span>
}

function Handoffs({ pic }) {
  const h = pic?.handoff
  if (!h) return null
  const moving = (pic.aircraft ?? []).filter((a) => a.status === 'TRANSFERRING').length
  return (
    <div>
      <h2>handoffs (registry, 3-phase)</h2>
      <table><tbody>
        <tr><td>completed</td><td>{h.handoffs}</td><td>avg {h.avg_ms?.toFixed(1)} ms</td></tr>
        <tr><td>in progress</td><td>{moving}</td><td>queued {h.queued}</td></tr>
        <tr><td>aborted</td><td>{h.aborted}</td><td>recovered {h.recovered}</td></tr>
        <tr><td>deferred</td><td>{h.deferred ?? 0}</td><td>target not ready</td></tr>
        <tr><td>multicast</td><td>{h.multicast_batches} sent</td><td>{h.multicast_dropped} dropped</td></tr>
      </tbody></table>
    </div>
  )
}

// One line in the side panel for the selected aircraft; the form itself is
// in the popover next to it on the radar.
function Selected({ pic, selected, onSelect }) {
  if (!selected) return <div className="hint">Click an aircraft on the radar to issue a clearance.</div>
  const ac = (pic?.aircraft ?? []).find((a) => a.callsign === selected)
  return (
    <div className="hint selected-line">
      selected <b>{selected}</b>
      {ac ? ` · ${ac.owner || 'unclaimed'} · FL${Math.round(ac.alt / 100)}${ac.verify ? ` · ${ac.verify}` : ''}` : ' · no longer on the radar'}
      {' '}<button onClick={() => onSelect(null)}>clear</button>
    </div>
  )
}

export default function App() {
  const [pic, connected] = usePicture()
  const [selected, setSelected] = useState(null)
  const [coverage, setCoverage] = useState(false)
  const [radarSize, setRadarSize, fitRadar] = useRadarSize()
  const [allLabels, setAllLabels] = useState(true) // every aircraft named; untick to declutter a busy radar
  // Esc closes the clearance popover.
  useEffect(() => {
    if (!selected) return
    const onKey = (e) => e.key === 'Escape' && setSelected(null)
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [selected])
  // A click outside the radar and the popover closes it. The radar's own
  // click already selects another aircraft or nothing; the side panel's
  // alert list selects after this runs (mousedown comes before click).
  useEffect(() => {
    if (!selected) return
    const onDown = (e) => { if (!e.target.closest('.radar-wrap')) setSelected(null) }
    document.addEventListener('mousedown', onDown)
    return () => document.removeEventListener('mousedown', onDown)
  }, [selected])
  const [session, setSession] = useState(null) // login response: token, username, role, sectors
  // Drop the session once the token has expired, rather than waiting for a 401.
  useEffect(() => {
    if (session && pic?.time > session.expires_at_ms) setSession(null)
  }, [pic, session])
  return (
    <div className="layout">
      <Radar pic={pic} selected={selected} onSelect={setSelected} coverage={coverage} allLabels={allLabels} size={radarSize}>
        <ClearanceForm key={selected} pic={pic} selected={selected} session={session} setSession={setSession} />
      </Radar>
      <Divider size={radarSize} onResize={setRadarSize} onReset={fitRadar} />
      <div className="side">
        <h1>Air Traffic Controller but Different</h1>
        <div className="status">{connected ? 'live' : 'connecting to gateway...'} · {pic?.aircraft?.length ?? 0} aircraft
          {(pic?.aircraft?.length ?? 0) > LABEL_ALL_UP_TO && <label className="toggle">
            <input type="checkbox" checked={allLabels} onChange={(e) => setAllLabels(e.target.checked)} /> all labels
          </label>}
        </div>
        <Login pic={pic} session={session} setSession={setSession} />
        <Selected pic={pic} selected={selected} onSelect={setSelected} />
        <Conflicts pic={pic} session={session} setSession={setSession} onSelect={setSelected} />
        <Alerts pic={pic} coverage={coverage} setCoverage={setCoverage} onSelect={setSelected} />
        <Clearances pic={pic} />
        <Handoffs pic={pic} />
        <Elastic pic={pic} session={session} />
        <Safety pic={pic} />
        <Clusters pic={pic} />
      </div>
    </div>
  )
}
