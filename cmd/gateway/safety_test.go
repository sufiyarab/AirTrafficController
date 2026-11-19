package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"atc/internal/fsm"
)

func TestCheckLeaders(t *testing.T) {
	clusters := map[string][]string{"sector-1": {"a", "b", "c"}}
	nodes := map[string]nodeStatus{
		"a": {Up: true, State: "Leader", Term: "3"},
		"b": {Up: true, State: "Leader", Term: "4"}, // old leader not yet told: fine
		"c": {Up: true, State: "Follower", Term: "4"},
	}
	if p := checkLeaders(clusters, nodes); len(p) != 0 {
		t.Fatalf("leaders in different terms are allowed, got %v", p)
	}
	nodes["c"] = nodeStatus{Up: true, State: "Leader", Term: "4"}
	p := checkLeaders(clusters, nodes)
	if len(p) != 1 || !strings.Contains(p[0], "term 4") {
		t.Fatalf("want one violation for term 4, got %v", p)
	}
	nodes["c"] = nodeStatus{Up: false, State: "Leader", Term: "4"} // unreachable: last answer does not count
	if p := checkLeaders(clusters, nodes); len(p) != 0 {
		t.Fatalf("a down node must not count, got %v", p)
	}
}

func testMap() fsm.SectorMap {
	return fsm.SectorMap{Version: 9, Sectors: map[string]fsm.Sector{
		"sector-1": {Name: "sector-1", Status: fsm.SectorActive, Region: fsm.Region{MinX: 0, MinY: 0, MaxX: 100, MaxY: 100}},
		"standby":  {Name: "standby", Status: fsm.SectorStandby},
	}}
}

func TestCheckOwners(t *testing.T) {
	now := time.Now()
	tracked := map[string]time.Time{
		"NEW": now.Add(-time.Second),      // just appeared: may be unowned
		"OLD": now.Add(-10 * time.Second), // should have been claimed by now
		"OK":  now.Add(-time.Minute),
		"SBY": now.Add(-time.Minute),
		"BAD": now.Add(-time.Minute),
		"TWR": now.Add(-time.Minute),
	}
	owners := map[string]fsm.Owner{
		"OK":  {Callsign: "OK", Status: fsm.Active, Sector: "sector-1"},
		"SBY": {Callsign: "SBY", Status: fsm.Active, Sector: "standby"},
		"BAD": {Callsign: "BAD", Status: fsm.Transferring, Sector: "sector-1", Target: "sector-1"},
		"TWR": {Callsign: "TWR", Status: fsm.Active, Sector: "tower"}, // fixed airspace, not on the map
	}
	p := checkOwners(tracked, owners, testMap(), map[string]bool{"tower": true}, now)
	for _, k := range []string{"OLD|unowned", "SBY|noairspace|standby", "BAD|badtarget"} {
		if _, ok := p[k]; !ok {
			t.Errorf("missing problem %s in %v", k, p)
		}
	}
	if len(p) != 3 {
		t.Errorf("want 3 problems, got %v", p)
	}
}

func TestPersistentNeedsTwoChecks(t *testing.T) {
	m := newSafetyMonitor(time.Now())
	if out := m.persistent("r", map[string]string{"x": "x broke"}); len(out) != 0 {
		t.Fatal("first sighting must not count")
	}
	if out := m.persistent("r", map[string]string{"x": "x broke"}); len(out) != 1 {
		t.Fatal("second sighting in a row must count")
	}
	if out := m.persistent("r", map[string]string{"x": "x broke"}); len(out) != 0 {
		t.Fatal("must be reported once, not every check")
	}
	m.persistent("r", nil) // went away
	if out := m.persistent("r", map[string]string{"x": "x broke"}); len(out) != 0 {
		t.Fatal("a gap resets the count")
	}
}

func TestScanSectorState(t *testing.T) {
	state := map[string]json.RawMessage{
		"clearance/A/1": json.RawMessage(`{"id":"1","callsign":"A","sector":"sector-1"}`),
		"clearance/B/2": json.RawMessage(`{"id":"2","callsign":"B","sector":"sector-2"}`),
		"accepted/C":    json.RawMessage(`{}`),
	}
	ids, probs := scanSectorState("sector-1", state)
	if !ids["1"] || !ids["2"] || len(ids) != 2 {
		t.Fatalf("ids %v", ids)
	}
	if len(probs) != 1 || !strings.Contains(probs[0], "names sector-2") {
		t.Fatalf("problems %v", probs)
	}
}

// ---------- against the fake clusters of clearance_flow_test.go ----------

// safetyHarness adds a /state endpoint to every fake sector (built from the
// commands it applied) and an optional hook that runs on each sector commit.
type safetyHarness struct {
	*harness
	onApply func(cmd fsm.Command)
	hidden  map[string]bool // clearance keys the fake sector "lost"
}

func newSafetyHarness(t *testing.T) *safetyHarness {
	sh := &safetyHarness{harness: newHarness(t), hidden: map[string]bool{}}
	sh.g.safety = newSafetyMonitor(time.Now())
	nodes := map[string]http.Handler{"registry-a": sh.registry.handler()}
	for name, f := range sh.sectors {
		inner := f.handler()
		f := f
		nodes[name+"-a"] = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "GET" && r.URL.Path == "/state" {
				f.mu.Lock()
				state := map[string]json.RawMessage{}
				for _, c := range f.applied {
					if !sh.hidden[c.Key] {
						state[c.Key] = c.Value
					}
				}
				f.mu.Unlock()
				json.NewEncoder(w).Encode(state)
				return
			}
			if r.Method == "POST" && r.URL.Path == "/apply" && sh.onApply != nil {
				var cmd fsm.Command
				json.NewDecoder(r.Body).Decode(&cmd)
				body, _ := json.Marshal(cmd)
				r.Body = http.NoBody
				r2 := r.Clone(r.Context())
				r2.Body = readCloser(body)
				inner.ServeHTTP(w, r2)
				sh.onApply(cmd)
				return
			}
			inner.ServeHTTP(w, r)
		})
	}
	sh.g.rpc = &httpNodes{client: &http.Client{Transport: routeTransport{nodes}, Timeout: time.Second}}
	return sh
}

type bodyReader struct{ *strings.Reader }

func (bodyReader) Close() error { return nil }

func readCloser(b []byte) bodyReader { return bodyReader{strings.NewReader(string(b))} }

func (sh *safetyHarness) inv(id string) invariantStatus {
	sh.g.mu.Lock()
	defer sh.g.mu.Unlock()
	return *sh.g.safety.inv[id]
}

func TestSafetyStaysGreenOnNormalClearances(t *testing.T) {
	sh := newSafetyHarness(t)
	sh.own("ATC001", "sector-1", fsm.Active)
	sh.own("ATC002", "sector-2", fsm.Active)
	sh.post(t, "ATC001", "CLIMB FL300")
	sh.post(t, "ATC999", "CLIMB FL300") // rejected: unclaimed
	sh.setLeader("sector-2", false)
	sh.post(t, "ATC002", "HEADING 090") // pending
	sh.g.safetyTick(time.Now())
	sh.setLeader("sector-2", true)
	sh.g.flushQueue("sector-2")
	for range 3 {
		sh.g.safetyTick(time.Now().Add(5 * time.Second))
	}

	sh.g.mu.Lock()
	p := sh.g.safety.picture()
	sh.g.mu.Unlock()
	if !p.OK {
		t.Fatalf("expected all green, got %+v", p)
	}
	if lost := sh.inv(InvNoLostClear); !strings.Contains(lost.Detail, "3 issued, 2 committed (2 found") {
		t.Fatalf("rule 5 detail: %q", lost.Detail)
	}
	if own := sh.inv(InvClearanceOwner); own.Checks < 2 {
		t.Fatalf("rule 3 should have been checked at each commit, got %d checks", own.Checks)
	}
}

func TestSafetyCatchesOwnershipChangeDuringCommit(t *testing.T) {
	sh := newSafetyHarness(t)
	sh.own("ATC010", "sector-1", fsm.Active)
	// Simulate a handoff racing the clearance: while sector-1 commits, the
	// registry moves the aircraft to sector-2.
	sh.onApply = func(fsm.Command) {
		sh.registry.mu.Lock()
		sh.registry.owners["ATC010"] = fsm.Owner{Callsign: "ATC010", Sector: "sector-2", Status: fsm.Active, Index: 42}
		sh.registry.mu.Unlock()
	}
	sh.post(t, "ATC010", "DESCEND FL200")
	if s := sh.inv(InvClearanceOwner); s.OK || s.Violations != 1 || !strings.Contains(s.LastMsg, "ownership changed") {
		t.Fatalf("want a rule 3 violation, got %+v", s)
	}
}

func TestSafetyCatchesLostClearance(t *testing.T) {
	sh := newSafetyHarness(t)
	sh.own("ATC020", "sector-1", fsm.Active)
	_, c := sh.post(t, "ATC020", "CLIMB FL350")
	later := time.Now().Add(5 * time.Second)
	sh.g.safetyTick(later)
	if !sh.inv(InvNoLostClear).OK {
		t.Fatal("present clearance flagged as lost")
	}
	sh.hidden["clearance/ATC020/"+c.ID] = true // the sector "forgets" it
	sh.g.safetyTick(later)
	if !sh.inv(InvNoLostClear).OK {
		t.Fatal("one missing read must not count yet")
	}
	sh.g.safetyTick(later)
	if s := sh.inv(InvNoLostClear); s.OK || !strings.Contains(s.LastMsg, "missing from sector-1") {
		t.Fatalf("want a rule 5 violation, got %+v", s)
	}
}

func TestSafetyCatchesClearanceToGhost(t *testing.T) {
	sh := newSafetyHarness(t)
	sh.g.ver = newVerifier(defaultVerifyConfig(), []*receiver{{ID: "receiver-a"}})
	start := time.Now()
	c := &clearance{ID: "x1", Callsign: "GHOST01"}
	sh.own("GHOST01", "sector-1", fsm.Active)
	owner := fsm.Owner{Callsign: "GHOST01", Sector: "sector-1", Status: fsm.Active}

	// Turned SUSPECT during the commit: allowed.
	sh.g.ver.tracks["GHOST01"] = &track{State: VerSuspect, SinceMs: start.UnixMilli() + 5}
	sh.g.safetyCommitted(c, "sector-1", owner, start)
	if !sh.inv(InvNoGhostClear).OK {
		t.Fatal("SUSPECT after the commit started must not count")
	}
	// Already SUSPECT before the commit: violation.
	sh.g.ver.tracks["GHOST01"].SinceMs = start.UnixMilli() - 1000
	sh.g.safetyCommitted(c, "sector-1", owner, start)
	if s := sh.inv(InvNoGhostClear); s.OK || s.Violations != 1 {
		t.Fatalf("want a rule 4 violation, got %+v", s)
	}
}
