package main

// Conflict detection and resolution advisories (5 NM / 1,000 ft).
//
// Design (team decisions, Nov 19):
//   - Runs centrally in the gateway, which already receives every position
//     report, so it adds no messages. Pairs that straddle a sector boundary
//     need no special case.
//   - Uniform grid + look-ahead. Once a second the gateway puts every aircraft
//     in a grid cell whose side is the farthest two aircraft can close in on
//     each other within the look-ahead window plus the separation minimum.
//     Two aircraft that are not in the same or adjacent cells cannot conflict
//     within the window, so only those pairs are tested. Expected time
//     O(n + k) for n aircraft and k nearby pairs, instead of O(n^2) for all
//     pairs.
//   - Each candidate pair is projected forward on straight lines (heading,
//     ground speed and the climb/descent rate seen between reports) for
//     LOOKAHEAD seconds. It is a conflict if, at some moment in the window,
//     the pair is both within 5 NM horizontally and within 1,000 ft
//     vertically. The intervals where each holds are solved exactly (a
//     quadratic for the horizontal distance, a linear inequality for the
//     vertical), so there is no time-stepping error.
//   - A conflict gets one suggested resolution: a climb or descent of at least
//     2,000 ft for one of the two aircraft, chosen so that the new level does
//     not create a conflict with any aircraft near it. The controller issues it
//     with one click as an ordinary clearance (clearance.go), so it goes
//     through the owner sector's Raft log, controller authorization and the
//     safety checks like any other clearance. The gateway never issues it
//     on its own.
//
// SUSPECT (spoofed) aircraft are left out: a conflict with a ghost is not a
// conflict, and a clearance to one is refused anyway.

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"time"
)

const (
	sepNM          = 5.0    // horizontal separation minimum
	sepFt          = 1000.0 // vertical separation minimum
	resolveFt      = 2000.0 // a resolution moves one aircraft at least this far
	maxFL          = 410    // highest flight level a resolution may use
	minFL          = 50     // lowest
	conflictTick   = time.Second
	conflictRecent = 20 // ended conflicts kept for the dashboard
)

// ---------- configuration ----------

type conflictConfig struct {
	Lookahead time.Duration // CONFLICT_LOOKAHEAD_S, default 120
}

func loadConflictConfig() conflictConfig {
	s := 120
	if v, err := strconv.Atoi(os.Getenv("CONFLICT_LOOKAHEAD_S")); err == nil && v >= 0 {
		s = v
	}
	return conflictConfig{Lookahead: time.Duration(s) * time.Second}
}

// ---------- kinematics ----------

// mover is one aircraft as the detector sees it: position in NM, velocity in
// NM per second, altitude in feet and vertical rate in feet per second.
type mover struct {
	cs         string
	x, y, alt  float64
	vx, vy, vz float64
	speedKt    float64
	sector     string // owner, for the suggestion
}

func moverFrom(p position, vz float64) mover {
	rad := p.Heading * math.Pi / 180 // heading clockwise from north, as in sim.py
	v := p.Speed / 3600
	return mover{cs: p.Callsign, x: p.X, y: p.Y, alt: p.Alt, vx: v * math.Sin(rad), vy: v * math.Cos(rad),
		vz: vz, speedKt: p.Speed, sector: p.Owner}
}

// interval is a closed time range [lo, hi] in seconds; empty if lo > hi.
type interval struct{ lo, hi float64 }

func (a interval) empty() bool { return a.lo > a.hi }

func (a interval) and(b interval) interval {
	return interval{math.Max(a.lo, b.lo), math.Min(a.hi, b.hi)}
}

// horizontalWindow returns when, within [0, T], the horizontal distance
// between a and b is below sepNM. With relative position d and relative
// velocity v, |d + v t|^2 < sep^2 is a quadratic in t.
func horizontalWindow(a, b mover, T float64) interval {
	dx, dy := b.x-a.x, b.y-a.y
	vx, vy := b.vx-a.vx, b.vy-a.vy
	A := vx*vx + vy*vy
	B := 2 * (dx*vx + dy*vy)
	C := dx*dx + dy*dy - sepNM*sepNM
	if A < 1e-12 { // same velocity: the distance never changes
		if C < 0 {
			return interval{0, T}
		}
		return interval{1, 0}
	}
	disc := B*B - 4*A*C
	if disc < 0 {
		return interval{1, 0}
	}
	r := math.Sqrt(disc)
	return interval{(-B - r) / (2 * A), (-B + r) / (2 * A)}.and(interval{0, T})
}

// verticalWindow returns when, within [0, T], |dz + vz t| < sepFt.
func verticalWindow(a, b mover, T float64) interval {
	dz, vz := b.alt-a.alt, b.vz-a.vz
	if math.Abs(vz) < 1e-9 {
		if math.Abs(dz) < sepFt {
			return interval{0, T}
		}
		return interval{1, 0}
	}
	t1, t2 := (-sepFt-dz)/vz, (sepFt-dz)/vz
	if t1 > t2 {
		t1, t2 = t2, t1
	}
	return interval{t1, t2}.and(interval{0, T})
}

// predict reports whether a and b lose separation within T seconds, when it
// starts (0 = already lost), and the horizontal distance at the closest point
// inside the conflict window.
func predict(a, b mover, T float64) (bool, float64, float64) {
	w := horizontalWindow(a, b, T).and(verticalWindow(a, b, T))
	if w.empty() {
		return false, 0, 0
	}
	// closest approach, clamped into the window
	dx, dy := b.x-a.x, b.y-a.y
	vx, vy := b.vx-a.vx, b.vy-a.vy
	t := w.lo
	if v2 := vx*vx + vy*vy; v2 > 1e-12 {
		t = math.Min(math.Max(-(dx*vx+dy*vy)/v2, w.lo), w.hi)
	}
	return true, w.lo, math.Hypot(dx+vx*t, dy+vy*t)
}

// lostNow is the instantaneous check: within 5 NM and 1,000 ft right now.
func lostNow(a, b mover) bool {
	return math.Hypot(b.x-a.x, b.y-a.y) < sepNM && math.Abs(b.alt-a.alt) < sepFt
}

// ---------- the grid ----------

type cell struct{ i, j int }

// candidatePairs returns every pair in the same or an adjacent cell, each
// once, with the first index lower than the second, and the cell size used.
// The cell side is the most two aircraft can close in on each other within T
// seconds (both at the fastest speed present, head on) plus the separation
// minimum, so no pair outside adjacent cells can conflict within T.
func candidatePairs(ms []mover, T float64) ([][2]int, float64) {
	vmax := 0.0
	for _, m := range ms {
		vmax = math.Max(vmax, m.speedKt/3600)
	}
	side := sepNM + 2*vmax*T
	grid := map[cell][]int{}
	for i, m := range ms {
		c := cell{int(math.Floor(m.x / side)), int(math.Floor(m.y / side))}
		grid[c] = append(grid[c], i)
	}
	var out [][2]int
	for c, members := range grid {
		for di := -1; di <= 1; di++ {
			for dj := -1; dj <= 1; dj++ {
				other, ok := grid[cell{c.i + di, c.j + dj}]
				if !ok {
					continue
				}
				for _, a := range members {
					for _, b := range other {
						if a < b { // each unordered pair once, from whichever cell holds a
							out = append(out, [2]int{a, b})
						}
					}
				}
			}
		}
	}
	return out, side
}

// ---------- detection ----------

// conflict is one predicted or actual loss of separation.
type conflict struct {
	A          string      `json:"a"`
	B          string      `json:"b"`
	Key        string      `json:"key"`
	Loss       bool        `json:"loss"`       // separation already lost
	InS        float64     `json:"in_s"`       // seconds until it is lost (0 if lost)
	MinNM      float64     `json:"min_nm"`     // horizontal distance at the closest point
	VertFt     float64     `json:"vert_ft"`    // vertical distance now
	Sectors    [2]string   `json:"sectors"`    // owner of A, owner of B
	Suggestion *suggestion `json:"suggestion"` // nil if no safe level was found
	FirstMs    int64       `json:"first_ms"`   // when it was first predicted
	LossMs     int64       `json:"loss_ms"`    // when separation was first lost (0 if not yet)
	WarningS   float64     `json:"warning_s"`  // seconds of warning before the loss (set at LossMs)
}

// suggestion is the resolution advisory: one clearance for one aircraft.
type suggestion struct {
	Callsign    string `json:"callsign"`
	Instruction string `json:"instruction"` // e.g. "CLIMB FL340", sent as-is to /api/clearance
	Sector      string `json:"sector"`      // who must issue it
}

type conflictStats struct {
	Ticks        uint64  `json:"ticks"`
	Aircraft     int     `json:"aircraft"`      // checked in the last tick
	PairsChecked int     `json:"pairs_checked"` // candidate pairs the grid produced in the last tick
	AllPairs     int     `json:"all_pairs"`     // n(n-1)/2, what a brute-force check would test
	CellNM       float64 `json:"cell_nm"`
	LastMs       float64 `json:"last_ms"` // detector run time, last tick
	AvgMs        float64 `json:"avg_ms"`
	Detected     int     `json:"detected"` // conflicts ever predicted
	Losses       int     `json:"losses"`   // conflicts that became an actual loss of separation
	Unwarned     int     `json:"unwarned"` // losses with no warning before them
	AvgWarningS  float64 `json:"avg_warning_s"`
	Resolved     int     `json:"resolved"` // ended without ever becoming a loss
	LookaheadS   float64 `json:"lookahead_s"`
}

// conflictState is guarded by g.mu.
type conflictState struct {
	cfg     conflictConfig
	active  map[string]*conflict // pair key -> conflict
	recent  []conflict           // ended, newest last
	st      conflictStats
	lastAlt map[string]altSample // callsign -> previous altitude, for the vertical rate
}

type altSample struct {
	alt float64
	at  time.Time
}

func newConflictState(cfg conflictConfig) *conflictState {
	return &conflictState{cfg: cfg, active: map[string]*conflict{}, lastAlt: map[string]altSample{},
		st: conflictStats{LookaheadS: cfg.Lookahead.Seconds()}}
}

func pairKey(a, b string) (string, string, string) {
	if a > b {
		a, b = b, a
	}
	return a, b, a + "|" + b
}

// detect finds every conflict among ms within T seconds. Pure: no gateway
// state, so it is unit tested directly.
func detect(ms []mover, T float64) (map[string]*conflict, int, float64) {
	pairs, side := candidatePairs(ms, T)
	out := map[string]*conflict{}
	for _, p := range pairs {
		a, b := ms[p[0]], ms[p[1]]
		hit, in, minNM := predict(a, b, T)
		if !hit {
			continue
		}
		if a.cs > b.cs {
			a, b = b, a
		}
		_, _, key := pairKey(a.cs, b.cs)
		out[key] = &conflict{A: a.cs, B: b.cs, Key: key, Loss: lostNow(a, b), InS: in, MinNM: minNM,
			VertFt: math.Abs(b.alt - a.alt), Sectors: [2]string{a.sector, b.sector}}
	}
	return out, len(pairs), side
}

// suggest picks one level change that separates the pair without creating a
// conflict with any other aircraft. Candidates, in order: the higher aircraft
// climbs, the lower descends, the lower climbs over the higher, the higher
// descends under the lower. The first that is within FL050-FL410 and is
// conflict-free over the look-ahead wins.
func suggest(c *conflict, byCS map[string]mover, ms []mover, T float64) *suggestion {
	a, b := byCS[c.A], byCS[c.B]
	hi, lo := a, b
	if b.alt > a.alt || (b.alt == a.alt && b.cs > a.cs) {
		hi, lo = b, a
	}
	upFrom := func(ref float64) int { return int(math.Ceil((ref+resolveFt)/1000)) * 10 }
	downFrom := func(ref float64) int { return int(math.Floor((ref-resolveFt)/1000)) * 10 }
	type cand struct {
		m  mover
		fl int
		up bool
	}
	cands := []cand{
		{hi, upFrom(math.Max(hi.alt, lo.alt)), true},
		{lo, downFrom(math.Min(hi.alt, lo.alt)), false},
		{lo, upFrom(math.Max(hi.alt, lo.alt)), true},
		{hi, downFrom(math.Min(hi.alt, lo.alt)), false},
	}
	for _, k := range cands {
		if k.fl > maxFL || k.fl < minFL {
			continue
		}
		moved := k.m
		moved.alt, moved.vz = float64(k.fl*100), 0 // assume it levels off at the new flight level
		clear := true
		for _, o := range ms {
			if o.cs == moved.cs {
				continue
			}
			if hit, _, _ := predict(moved, o, T); hit {
				clear = false
				break
			}
		}
		if clear {
			verb := "DESCEND"
			if k.up {
				verb = "CLIMB"
			}
			return &suggestion{Callsign: k.m.cs, Instruction: fmt.Sprintf("%s FL%03d", verb, k.fl), Sector: k.m.sector}
		}
	}
	return nil
}

// ---------- the loop ----------

func (g *gateway) conflictLoop() {
	for range time.Tick(conflictTick) {
		g.conflictTick(time.Now())
	}
}

func (g *gateway) conflictTick(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	cs := g.conflicts
	if cs == nil {
		return
	}
	start := time.Now()
	T := cs.cfg.Lookahead.Seconds()

	var ms []mover
	byCS := map[string]mover{}
	for callsign, p := range g.aircraft {
		if !g.freshLocked(callsign, now) || g.ver.state(callsign) == VerSuspect {
			continue
		}
		vz := 0.0
		if prev, ok := cs.lastAlt[callsign]; ok {
			if dt := now.Sub(prev.at).Seconds(); dt > 0.2 {
				vz = (p.Alt - prev.alt) / dt
			}
		}
		cs.lastAlt[callsign] = altSample{p.Alt, now}
		m := moverFrom(p, vz)
		ms = append(ms, m)
		byCS[callsign] = m
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].cs < ms[j].cs }) // deterministic order

	found, checked, side := detect(ms, T)
	nowMs := now.UnixMilli()
	for key, c := range found {
		prev, known := cs.active[key]
		if known {
			c.FirstMs, c.LossMs, c.WarningS = prev.FirstMs, prev.LossMs, prev.WarningS
		} else {
			c.FirstMs = nowMs
			cs.st.Detected++
		}
		if c.Loss && c.LossMs == 0 {
			c.LossMs = nowMs
			c.WarningS = float64(nowMs-c.FirstMs) / 1000
			cs.st.Losses++
			if c.WarningS == 0 {
				cs.st.Unwarned++
			}
			cs.st.AvgWarningS += (c.WarningS - cs.st.AvgWarningS) / float64(cs.st.Losses)
		}
		c.Suggestion = suggest(c, byCS, ms, T)
		cs.active[key] = c
	}
	for key, c := range cs.active {
		if _, still := found[key]; still {
			continue
		}
		if c.LossMs == 0 {
			cs.st.Resolved++
		}
		cs.recent = append(cs.recent, *c)
		delete(cs.active, key)
	}
	if len(cs.recent) > conflictRecent {
		cs.recent = cs.recent[len(cs.recent)-conflictRecent:]
	}
	for callsign := range cs.lastAlt {
		if _, ok := g.aircraft[callsign]; !ok {
			delete(cs.lastAlt, callsign)
		}
	}

	ms0 := float64(time.Since(start).Microseconds()) / 1000
	st := &cs.st
	st.Ticks++
	st.Aircraft, st.PairsChecked, st.AllPairs, st.CellNM = len(ms), checked, len(ms)*(len(ms)-1)/2, side
	st.LastMs = ms0
	st.AvgMs += (ms0 - st.AvgMs) / float64(st.Ticks)
}

// conflictPicture is what the dashboard gets.
type conflictPicture struct {
	Active []conflict    `json:"active"` // soonest first
	Recent []conflict    `json:"recent"` // ended, newest first
	Stats  conflictStats `json:"stats"`
}

func (g *gateway) conflictPictureLocked() *conflictPicture {
	cs := g.conflicts
	if cs == nil {
		return nil
	}
	p := &conflictPicture{Stats: cs.st, Active: []conflict{}, Recent: []conflict{}}
	for _, c := range cs.active {
		p.Active = append(p.Active, *c)
	}
	sort.Slice(p.Active, func(i, j int) bool {
		if p.Active[i].InS != p.Active[j].InS {
			return p.Active[i].InS < p.Active[j].InS
		}
		return p.Active[i].Key < p.Active[j].Key
	})
	for i := len(cs.recent) - 1; i >= 0; i-- {
		p.Recent = append(p.Recent, cs.recent[i])
	}
	return p
}
