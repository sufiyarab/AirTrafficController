package main

// Closing the loop: the simulated aircraft fly the clearances that commit.
//
// When a clearance commits in its owner sector's Raft log, the gateway adds it
// to that aircraft's recent instructions (the last flyableKeep). Every reply
// to the simulator's per-tick position report (gRPC ReportPositions, or
// POST /api/positions) lists the recent instructions of each aircraft in the
// batch, oldest first. The simulator applies each one once, by clearance ID,
// so repeating them in every reply is harmless and a lost reply is covered by
// the next one. No extra messages:
// the instructions ride on the reply the simulator already waits for.

import (
	"atc/internal/atcpb"
)

const flyableKeep = 5 // recent clearances per aircraft repeated to the simulator

type flyable struct {
	ID          string `json:"clearance_id"`
	Callsign    string `json:"callsign"`
	Instruction string `json:"instruction"`
}

// recordFlyableLocked keeps c as its aircraft's latest instruction. Call with
// g.mu held, right after the clearance committed.
func (g *gateway) recordFlyableLocked(c *clearance) {
	if g.flyables == nil {
		g.flyables = map[string][]flyable{}
	}
	for _, f := range g.flyables[c.Callsign] {
		if f.ID == c.ID {
			return // a retried commit of the same clearance
		}
	}
	list := append(g.flyables[c.Callsign], flyable{ID: c.ID, Callsign: c.Callsign, Instruction: c.Instruction})
	if len(list) > flyableKeep {
		list = list[len(list)-flyableKeep:]
	}
	g.flyables[c.Callsign] = list
}

// instructionsFor returns the recent committed instructions of each callsign,
// oldest first.
func (g *gateway) instructionsFor(callsigns []string) []flyable {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []flyable
	for _, cs := range callsigns {
		out = append(out, g.flyables[cs]...)
	}
	return out
}

func instructionsPB(fs []flyable) *atcpb.Instructions {
	out := &atcpb.Instructions{}
	for _, f := range fs {
		out.Instructions = append(out.Instructions, &atcpb.Instruction{Callsign: f.Callsign, ClearanceId: f.ID, Instruction: f.Instruction})
	}
	return out
}
