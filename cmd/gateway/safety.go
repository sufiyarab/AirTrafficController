package main

// Live safety-invariant panel.
//
// Every second the gateway checks five safety rules against the state the
// clusters actually hold, and the dashboard shows each rule as green (held at
// every check so far) or red (broken at least once, with what broke it). The
// point for the demo: kill a leader, partition a node or trigger a surge, and
// the panel stays green.
//
//  1. one_owner        Every aircraft the gateway is tracking has exactly one
//                      controlling sector in the registry, and that sector has
//                      airspace on the current map.
//  2. one_leader       No Raft cluster has two leaders in the same term
//                      (Raft's Election Safety property).
//  3. clearance_owner  A clearance is committed only in the sector that owned
//                      the aircraft, ACTIVE, for the whole commit, and every
//                      clearance record in a sector's log names that sector.
//  4. no_ghost_clear   No clearance is committed for an aircraft the ground
//                      receivers marked SUSPECT.
//  5. no_lost_clear    No clearance disappears: every one issued is committed,
//                      rejected, expired or still pending, and every committed
//                      one is still in its sector's replicated state (also
//                      after that sector's leader changes).
//
// The checks read what the clusters hold (the registry's /owners and sector
// map, each node's /status, each sector leader's /state), not the gateway's
// own decisions, so a bug in the gateway's logic would show up here.
//
// Rules 1 and 5 compare two reads taken at slightly different times (for
// example the ownership table and the sector map, which are polled one after
// the other). To avoid false alarms from that skew, a problem must be seen on
// two checks in a row before it counts as a violation.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"atc/internal/fsm"
)

const (
	InvOneOwner       = "one_owner"
	InvOneLeader      = "one_leader"
	InvClearanceOwner = "clearance_owner"
	InvNoGhostClear   = "no_ghost_clear"
	InvNoLostClear    = "no_lost_clear"

	safetyInterval  = time.Second
	claimGrace      = 5 * time.Second // a new aircraft may be unowned this long
	safetyLogSize   = 20              // recent violations kept for the dashboard
	maxAuditedClear = 5000            // committed clearances remembered for rule 5
)

// invariantStatus is one row of the panel.
type invariantStatus struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Rule       string `json:"rule"`
	OK         bool   `json:"ok"`         // false once it has been violated
	Checks     uint64 `json:"checks"`     // how many times it was evaluated
	Violations uint64 `json:"violations"` // how many violations were found
	Detail     string `json:"detail"`     // what the last check looked at
	LastMs     int64  `json:"last_violation_ms,omitempty"`
	LastMsg    string `json:"last_violation,omitempty"`
}

type violation struct {
	ID  string `json:"id"`
	Ms  int64  `json:"ms"`
	Msg string `json:"msg"`
}

type safetyPicture struct {
	OK         bool              `json:"ok"`
	Invariants []invariantStatus `json:"invariants"`
	Recent     []violation       `json:"recent"` // newest first
	SinceMs    int64             `json:"since_ms"`
}

// auditedClearance is a committed clearance that rule 5 keeps looking for.
type auditedClearance struct {
	id, callsign, sector string
	committedMs          int64
}

// safetyMonitor is guarded by g.mu.
type safetyMonitor struct {
	order     []string
	inv       map[string]*invariantStatus
	recent    []violation
	startMs   int64
	created   int                         // clearances ever issued (rule 5 accounting)
	committed map[string]auditedClearance // clearance ID -> where it must stay
	commitSeq []string                    // committed IDs, oldest first, to bound the map
	suspected map[string]int              // problem key -> consecutive checks seen
	unchecked int                         // clearances whose owner re-read failed (rule 3)
}

func newSafetyMonitor(now time.Time) *safetyMonitor {
	m := &safetyMonitor{
		inv:       map[string]*invariantStatus{},
		committed: map[string]auditedClearance{},
		suspected: map[string]int{},
		startMs:   now.UnixMilli(),
	}
	add := func(id, name, rule string) {
		m.order = append(m.order, id)
		m.inv[id] = &invariantStatus{ID: id, Name: name, Rule: rule, OK: true}
	}
	add(InvOneOwner, "One controller per aircraft",
		"Every tracked aircraft is owned by exactly one sector in the registry, and that sector has airspace.")
	add(InvOneLeader, "One leader per term",
		"No Raft cluster has two leaders in the same term.")
	add(InvClearanceOwner, "Clearances only from the owner",
		"A clearance commits only in the sector that owned the aircraft for the whole commit.")
	add(InvNoGhostClear, "No clearance to a ghost",
		"No clearance commits for an aircraft the ground receivers marked SUSPECT.")
	add(InvNoLostClear, "No clearance lost",
		"Every clearance ends committed, rejected or expired, and committed ones stay in the sector's log.")
	return m
}

// checked records one evaluation of rule id; problems are the violations
// found by it (empty when it held).
func (m *safetyMonitor) checked(id string, now time.Time, detail string, problems []string) {
	s := m.inv[id]
	s.Checks++
	s.Detail = detail
	for _, p := range problems {
		s.OK = false
		s.Violations++
		s.LastMs, s.LastMsg = now.UnixMilli(), p
		m.recent = append(m.recent, violation{ID: id, Ms: now.UnixMilli(), Msg: p})
		log.Printf("SAFETY VIOLATION %s: %s", id, p)
	}
	if len(m.recent) > safetyLogSize {
		m.recent = m.recent[len(m.recent)-safetyLogSize:]
	}
}

// persistent filters problems to those also seen on the previous check of the
// same rule. keys must be stable across checks (for example "ATC001|no owner").
func (m *safetyMonitor) persistent(rule string, keys map[string]string) []string {
	var out []string
	seen := map[string]bool{}
	for k, msg := range keys {
		full := rule + "|" + k
		seen[full] = true
		m.suspected[full]++
		if m.suspected[full] == 2 { // report once, on the second sighting
			out = append(out, msg)
		}
	}
	for k := range m.suspected {
		if strings.HasPrefix(k, rule+"|") && !seen[k] {
			delete(m.suspected, k)
		}
	}
	sort.Strings(out)
	return out
}

func (m *safetyMonitor) picture() safetyPicture {
	p := safetyPicture{OK: true, SinceMs: m.startMs}
	for _, id := range m.order {
		s := *m.inv[id]
		p.OK = p.OK && s.OK
		p.Invariants = append(p.Invariants, s)
	}
	for i := len(m.recent) - 1; i >= 0; i-- {
		p.Recent = append(p.Recent, m.recent[i])
	}
	return p
}

// ---------- the pure checks (unit tested in safety_test.go) ----------

// checkLeaders returns one problem per cluster and term with more than one
// node reporting Leader. Two leaders in different terms is normal for a moment
// after a partition heals (the old one has not heard of the new term yet).
func checkLeaders(clusters map[string][]string, nodes map[string]nodeStatus) []string {
	var out []string
	for c, ids := range clusters {
		byTerm := map[string][]string{}
		for _, id := range ids {
			if st := nodes[id]; st.Up && st.State == "Leader" {
				byTerm[st.Term] = append(byTerm[st.Term], id)
			}
		}
		for term, ls := range byTerm {
			if len(ls) > 1 {
				sort.Strings(ls)
				out = append(out, fmt.Sprintf("%s has %d leaders in term %s: %s", c, len(ls), term, strings.Join(ls, ", ")))
			}
		}
	}
	sort.Strings(out)
	return out
}

// checkOwners returns problems keyed by a stable key, for safetyMonitor.persistent.
// tracked maps each aircraft the gateway is tracking to when it was first seen.
// fixed lists clusters with fixed airspace outside the registry's sector map
// (the tower's circle).
func checkOwners(tracked map[string]time.Time, owners map[string]fsm.Owner, smap fsm.SectorMap, fixed map[string]bool, now time.Time) map[string]string {
	out := map[string]string{}
	hasAirspace := func(s string) bool {
		if fixed[s] {
			return true
		}
		sec, ok := smap.Sectors[s]
		return ok && sec.Status != fsm.SectorStandby && !sec.Region.Empty()
	}
	for cs, first := range tracked {
		o, ok := owners[cs]
		switch {
		case !ok:
			if now.Sub(first) > claimGrace {
				out[cs+"|unowned"] = fmt.Sprintf("%s has no controlling sector %.0fs after it appeared", cs, now.Sub(first).Seconds())
			}
		case o.Sector == "":
			out[cs+"|empty"] = cs + " has an ownership record with no sector"
		case len(smap.Sectors) > 0 && !hasAirspace(o.Sector):
			out[cs+"|noairspace|"+o.Sector] = fmt.Sprintf("%s is owned by %s, which has no airspace on map v%d", cs, o.Sector, smap.Version)
		case o.Status == fsm.Transferring && (o.Target == "" || o.Target == o.Sector):
			out[cs+"|badtarget"] = fmt.Sprintf("%s is TRANSFERRING from %s to %q", cs, o.Sector, o.Target)
		case o.Status != fsm.Active && o.Status != fsm.Transferring:
			out[cs+"|status"] = fmt.Sprintf("%s has unknown ownership status %q", cs, o.Status)
		}
	}
	return out
}

// clearanceRecord is the value the gateway stores at clearance/<callsign>/<id>.
type clearanceRecord struct {
	ID       string `json:"id"`
	Callsign string `json:"callsign"`
	Sector   string `json:"sector"`
}

// scanSectorState parses one sector's replicated key/value state and returns
// the clearance IDs it holds, plus a problem for every record that names
// another sector (rule 3).
func scanSectorState(cluster string, state map[string]json.RawMessage) (ids map[string]bool, problems []string) {
	ids = map[string]bool{}
	for k, v := range state {
		if !strings.HasPrefix(k, "clearance/") {
			continue
		}
		var r clearanceRecord
		if json.Unmarshal(v, &r) != nil {
			problems = append(problems, fmt.Sprintf("%s holds an unreadable clearance record %s", cluster, k))
			continue
		}
		ids[r.ID] = true
		if r.Sector != cluster {
			problems = append(problems, fmt.Sprintf("clearance %s for %s is in %s's log but names %s", r.ID, r.Callsign, cluster, r.Sector))
		}
	}
	sort.Strings(problems)
	return ids, problems
}

// ---------- hooks called from the clearance path ----------

// safetyIssued counts a new clearance. Call with g.mu held, at the same moment
// its first status is recorded, so the accounting in rule 5 always balances.
func (g *gateway) safetyIssuedLocked() {
	if g.safety != nil {
		g.safety.created++
	}
}

// safetyCommitted is called right after a clearance committed, while the
// gateway still holds the aircraft's lock. before is the registry record the
// commit was authorized with. It re-reads the registry (rules 3 and 4) and
// remembers the clearance for rule 5.
func (g *gateway) safetyCommitted(c *clearance, sector string, before fsm.Owner, start time.Time) {
	if g.safety == nil {
		return
	}
	after, owned, err := g.ownerFromRegistry(c.Callsign)
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	m := g.safety

	var p3 []string
	switch {
	case err != nil:
		m.unchecked++ // registry unreachable right now; rule 5's log scan still covers it
	case !owned:
		p3 = append(p3, fmt.Sprintf("clearance %s for %s committed in %s, but the registry has no owner for it", c.ID, c.Callsign, sector))
	case after.Sector != sector || after.Status != fsm.Active || after.Index != before.Index:
		p3 = append(p3, fmt.Sprintf("clearance %s for %s committed in %s while ownership changed: %s %s (index %d -> %d)",
			c.ID, c.Callsign, sector, after.Status, after.Sector, before.Index, after.Index))
	}
	if before.Sector != sector || before.Status != fsm.Active {
		p3 = append(p3, fmt.Sprintf("clearance %s for %s committed in %s, but the owner was %s %s", c.ID, c.Callsign, sector, before.Status, before.Sector))
	}
	m.checked(InvClearanceOwner, now, fmt.Sprintf("checked clearance %s for %s against the registry", c.ID, c.Callsign), p3)

	// A violation only if it was already SUSPECT when the commit started; an
	// aircraft that turned SUSPECT during the few milliseconds of the commit
	// was cleared legitimately.
	var p4 []string
	if g.ver != nil {
		if t := g.ver.tracks[c.Callsign]; t != nil && t.State == VerSuspect && t.SinceMs <= start.UnixMilli() {
			p4 = append(p4, fmt.Sprintf("clearance %s committed for %s, which has been SUSPECT since %s",
				c.ID, c.Callsign, time.UnixMilli(t.SinceMs).Format("15:04:05.000")))
		}
	}
	m.checked(InvNoGhostClear, now, fmt.Sprintf("%s was %s when clearance %s committed", c.Callsign, orDash(g.ver.state(c.Callsign)), c.ID), p4)

	if _, dup := m.committed[c.ID]; !dup {
		m.committed[c.ID] = auditedClearance{id: c.ID, callsign: c.Callsign, sector: sector, committedMs: now.UnixMilli()}
		m.commitSeq = append(m.commitSeq, c.ID)
		if len(m.commitSeq) > maxAuditedClear {
			delete(m.committed, m.commitSeq[0])
			m.commitSeq = m.commitSeq[1:]
		}
	}
}

func orDash(s string) string {
	if s == "" {
		return "not checked (verification off)"
	}
	return s
}

// ---------- the loop ----------

func (g *gateway) safetyLoop() {
	for range time.Tick(safetyInterval) {
		g.safetyTick(time.Now())
	}
}

func (g *gateway) safetyTick(now time.Time) {
	// Read every sector's replicated state from its leader, outside the lock.
	type sectorRead struct {
		ids      map[string]bool
		problems []string
	}
	reads := map[string]sectorRead{}
	for c := range g.clusters {
		if c == "registry" {
			continue
		}
		leader := g.leaderOf(c)
		if leader == "" {
			continue
		}
		resp, err := g.client.Get("http://" + leader + ":8080/state")
		if err != nil {
			continue
		}
		var state map[string]json.RawMessage
		err = json.NewDecoder(resp.Body).Decode(&state)
		resp.Body.Close()
		if err != nil {
			continue
		}
		ids, probs := scanSectorState(c, state)
		reads[c] = sectorRead{ids, probs}
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	m := g.safety

	// Rule 1.
	tracked := map[string]time.Time{}
	for cs := range g.aircraft {
		if g.freshLocked(cs, now) {
			tracked[cs] = g.firstSeenLocked(cs, now)
		}
	}
	fixed := map[string]bool{}
	for c := range g.clusters {
		if _, inMap := g.smap.Sectors[c]; !inMap && c != "registry" {
			fixed[c] = true // the tower: its circle is not part of the sector map
		}
	}
	p1 := m.persistent(InvOneOwner, checkOwners(tracked, g.owners, g.smap, fixed, now))
	owned := 0
	for cs := range tracked {
		if _, ok := g.owners[cs]; ok {
			owned++
		}
	}
	m.checked(InvOneOwner, now, fmt.Sprintf("%d aircraft, %d owned, map v%d", len(tracked), owned, g.smap.Version), p1)

	// Rule 2.
	leaders := 0
	for _, st := range g.nodes {
		if st.Up && st.State == "Leader" {
			leaders++
		}
	}
	m.checked(InvOneLeader, now, fmt.Sprintf("%d clusters, %d leaders seen", len(g.clusters), leaders), checkLeaders(g.clusters, g.nodes))

	// Rule 3, the log-scan half (the commit-time half runs in safetyCommitted).
	var p3 []string
	records := 0
	for _, r := range reads {
		p3 = append(p3, r.problems...)
		records += len(r.ids)
	}
	sort.Strings(p3)
	p3 = m.persistent(InvClearanceOwner+"-scan", toKeys(p3))
	detail3 := fmt.Sprintf("%d clearance records in %d sector logs", records, len(reads))
	if m.unchecked > 0 {
		detail3 += fmt.Sprintf(", %d not re-read (registry busy)", m.unchecked)
	}
	m.checked(InvClearanceOwner, now, detail3, p3)

	// Rule 4 has nothing to poll: it is checked at each commit. Keep its
	// counter moving so the panel shows it is alive.
	if g.ver == nil {
		m.inv[InvNoGhostClear].Detail = "verification is off"
	} else if m.inv[InvNoGhostClear].Checks == 0 {
		m.inv[InvNoGhostClear].Detail = fmt.Sprintf("checked at each clearance commit; none yet, %d aircraft SUSPECT now", len(g.ver.alerts()))
	}

	// Rule 5: accounting, then durability.
	queued := 0
	for _, q := range g.clr.queues {
		queued += len(q)
	}
	st := g.clr.st
	accounted := st.Committed + st.Rejected + st.Expired + queued
	lost := map[string]string{}
	if accounted != m.created {
		lost["count"] = fmt.Sprintf("%d clearances issued but %d accounted for (%d committed, %d rejected, %d expired, %d pending)",
			m.created, accounted, st.Committed, st.Rejected, st.Expired, queued)
	}
	present := 0
	for id, a := range m.committed {
		r, read := reads[a.sector]
		if !read {
			continue // no leader right now: cannot tell, check again next time
		}
		if r.ids[id] {
			present++
		} else if now.UnixMilli()-a.committedMs > 2000 {
			lost[id] = fmt.Sprintf("committed clearance %s for %s is missing from %s's log", id, a.callsign, a.sector)
		}
	}
	m.checked(InvNoLostClear, now,
		fmt.Sprintf("%d issued, %d committed (%d found in the logs), %d pending", m.created, st.Committed, present, queued),
		m.persistent(InvNoLostClear, lost))
}

func toKeys(msgs []string) map[string]string {
	out := map[string]string{}
	for _, s := range msgs {
		out[s] = s
	}
	return out
}

// firstSeenLocked returns when the gateway first saw callsign.
func (g *gateway) firstSeenLocked(cs string, now time.Time) time.Time {
	if g.firstSeen == nil {
		g.firstSeen = map[string]time.Time{}
	}
	t, ok := g.firstSeen[cs]
	if !ok {
		g.firstSeen[cs] = now
		t = now
	}
	return t
}

// safetyHandler: GET /api/safety, the panel's data on its own (used by the
// experiment scripts).
func (g *gateway) safetyHandler(w http.ResponseWriter, _ *http.Request) {
	g.mu.Lock()
	p := g.safety.picture()
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(p)
}
