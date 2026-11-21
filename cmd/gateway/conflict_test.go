package main

import (
	"math"
	"math/rand"
	"strings"
	"testing"

	"atc/internal/atcpb"
	"atc/internal/fsm"
)

// ac builds a mover from the same units the simulator reports.
func ac(cs string, x, y, alt, heading, speedKt float64) mover {
	return moverFrom(position{Callsign: cs, X: x, Y: y, Alt: alt, Heading: heading, Speed: speedKt}, 0)
}

func TestPredictHeadOn(t *testing.T) {
	a := ac("A", 80, 100, 30000, 90, 360)   // eastbound
	b := ac("B", 100, 100, 30000, 270, 360) // westbound, 20 NM away: closing at 0.2 NM/s
	hit, in, minNM := predict(a, b, 120)
	if !hit {
		t.Fatal("head-on at the same level must conflict")
	}
	if want := (20.0 - sepNM) / 0.2; math.Abs(in-want) > 0.01 {
		t.Fatalf("loss in %.2fs, want %.2fs", in, want)
	}
	if minNM > 0.01 {
		t.Fatalf("closest approach %.2f NM, want 0", minNM)
	}
	if hit, _, _ := predict(a, b, 60); hit {
		t.Fatal("loss at 75 s must not show in a 60 s look-ahead")
	}
}

func TestPredictVerticalSeparation(t *testing.T) {
	a := ac("A", 80, 100, 30000, 90, 360)
	b := ac("B", 100, 100, 31000, 270, 360) // exactly 1,000 ft: separated
	if hit, _, _ := predict(a, b, 120); hit {
		t.Fatal("1,000 ft apart is separated")
	}
	b.alt = 30500
	if hit, _, _ := predict(a, b, 120); !hit {
		t.Fatal("500 ft apart on a head-on course must conflict")
	}
}

func TestPredictClimbIntoLevel(t *testing.T) {
	a := ac("A", 100, 100, 30000, 90, 300)
	b := ac("B", 100, 102, 26000, 90, 300) // 2 NM apart, same track, 4,000 ft below
	if hit, _, _ := predict(a, b, 120); hit {
		t.Fatal("level and 4,000 ft apart: no conflict")
	}
	b.vz = 50 // climbing 3,000 ft/min: within 1,000 ft after 60 s
	hit, in, _ := predict(a, b, 120)
	if !hit || math.Abs(in-60) > 0.01 {
		t.Fatalf("climbing into its level: hit=%v in=%.1f, want true 60", hit, in)
	}
}

func TestPredictAlreadyLostAndDiverging(t *testing.T) {
	a := ac("A", 100, 100, 20000, 0, 400)
	b := ac("B", 103, 100, 20000, 0, 400) // 3 NM apart, same course and speed
	hit, in, _ := predict(a, b, 120)
	if !hit || in != 0 || !lostNow(a, b) {
		t.Fatalf("already lost: hit=%v in=%v", hit, in)
	}
	c := ac("C", 100, 110, 20000, 0, 400)
	d := ac("D", 100, 100, 20000, 180, 400) // 10 NM apart, flying away from each other
	if hit, _, _ := predict(c, d, 120); hit {
		t.Fatal("diverging aircraft must not conflict")
	}
}

// The grid may only skip pairs that cannot conflict: it must find exactly what
// an all-pairs check finds.
func TestGridMatchesBruteForce(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for trial := 0; trial < 20; trial++ {
		n := 50 + rng.Intn(350)
		ms := make([]mover, n)
		for i := range ms {
			ms[i] = ac(string(rune('A'+i%26))+strings.Repeat("x", i/26), rng.Float64()*200, rng.Float64()*200,
				float64(5000+1000*rng.Intn(35)), rng.Float64()*360, 250+rng.Float64()*230)
			ms[i].vz = (rng.Float64() - 0.5) * 60
		}
		got, checked, _ := detect(ms, 120)
		want := 0
		for i := range ms {
			for j := i + 1; j < n; j++ {
				if hit, _, _ := predict(ms[i], ms[j], 120); hit {
					want++
					_, _, key := pairKey(ms[i].cs, ms[j].cs)
					if got[key] == nil {
						t.Fatalf("trial %d: grid missed %s", trial, key)
					}
				}
			}
		}
		if len(got) != want {
			t.Fatalf("trial %d: grid found %d, brute force %d", trial, len(got), want)
		}
		if all := n * (n - 1) / 2; checked > all {
			t.Fatalf("grid checked %d pairs, more than all %d", checked, all)
		}
	}
}

func TestSuggestSeparatesWithoutNewConflict(t *testing.T) {
	a := ac("A", 80, 100, 30000, 90, 360)
	b := ac("B", 100, 100, 30000, 270, 360)
	blocker := ac("C", 100, 100, 32000, 0, 0) // parked at FL320 right where a climb to FL320 would go
	ms := []mover{a, b, blocker}
	found, _, _ := detect(ms, 120)
	c := found["A|B"]
	if c == nil {
		t.Fatal("A|B not detected")
	}
	byCS := map[string]mover{"A": a, "B": b, "C": blocker}
	s := suggest(c, byCS, ms, 120)
	if s == nil {
		t.Fatal("no suggestion")
	}
	if s.Instruction == "CLIMB FL320" {
		t.Fatal("suggested a level that conflicts with C")
	}
	// apply it and check the pair and C are all clear
	moved := byCS[s.Callsign]
	var fl int
	if _, err := fmtSscan(s.Instruction, &fl); err != nil {
		t.Fatalf("bad instruction %q", s.Instruction)
	}
	moved.alt = float64(fl * 100)
	for _, o := range ms {
		if o.cs != moved.cs {
			if hit, _, _ := predict(moved, o, 120); hit {
				t.Fatalf("%s still conflicts with %s after %s", moved.cs, o.cs, s.Instruction)
			}
		}
	}
}

func TestSuggestRespectsCeiling(t *testing.T) {
	a := ac("A", 80, 100, 41000, 90, 360)
	b := ac("B", 100, 100, 41000, 270, 360)
	ms := []mover{a, b}
	found, _, _ := detect(ms, 120)
	s := suggest(found["A|B"], map[string]mover{"A": a, "B": b}, ms, 120)
	if s == nil || !strings.HasPrefix(s.Instruction, "DESCEND FL390") {
		t.Fatalf("at FL410 the only way is down, got %+v", s)
	}
}

// fmtSscan reads the flight level out of "CLIMB FL340" / "DESCEND FL280".
func fmtSscan(instr string, fl *int) (int, error) {
	i := strings.Index(instr, "FL")
	n := 0
	for _, r := range instr[i+2:] {
		n = n*10 + int(r-'0')
	}
	*fl = n
	return 1, nil
}

// A committed clearance comes back to the simulator in the reply to its next
// position report, and keeps coming back (the simulator de-duplicates by ID).
func TestCommittedClearanceReturnsToSimulator(t *testing.T) {
	h := newHarness(t)
	h.own("ATC001", "sector-1", fsm.Active)
	_, c1 := h.post(t, "ATC001", "CLIMB FL340")
	_, c2 := h.post(t, "ATC001", "HEADING 090")
	h.own("ATC002", "sector-2", fsm.Active)
	h.setLeader("sector-2", false)
	h.post(t, "ATC002", "DESCEND FL200") // pending: must not be flown yet

	g := h.g
	g.aircraft, g.outbox, g.owners = map[string]position{}, map[string]map[string]position{}, map[string]fsm.Owner{}
	g.jobs = make(chan job, 8)
	g.geo.Store(fallbackGeo)
	client := edgeClient(t, g)
	batch := &atcpb.PositionBatch{Positions: []*atcpb.Position{
		{Callsign: "ATC001", Seq: 1, XNm: 20, YNm: 150, AltFt: 30000, HeadingDeg: 0, SpeedKt: 400},
		{Callsign: "ATC002", Seq: 1, XNm: 150, YNm: 150, AltFt: 30000, HeadingDeg: 0, SpeedKt: 400},
	}}
	for round := 0; round < 2; round++ {
		reply, err := client.ReportPositions(edgeCtx(t), batch)
		if err != nil {
			t.Fatal(err)
		}
		got := reply.Instructions
		if len(got) != 2 || got[0].ClearanceId != c1.ID || got[0].Instruction != "CLIMB FL340" ||
			got[1].ClearanceId != c2.ID || got[1].Callsign != "ATC001" {
			t.Fatalf("round %d: reply %+v", round, got)
		}
	}
}
