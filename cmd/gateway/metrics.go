package main

// Prometheus metrics for the gateway: GET /metrics.
//
// The Raft nodes already export their own health (leader flag, term, commit
// index; cmd/node/main.go). This file exports the rest, so Grafana can graph the
// whole system: sector load, handoffs, clearances, conflicts, spoof detection,
// logins and the live safety invariants.
//
// Almost everything is read from the same snapshot the radar uses
// (g.snapshot()), so the graphs cannot disagree with the dashboard. Counters in
// the snapshot only ever go up while the gateway runs, so they are exported as
// Prometheus counters (use rate()/increase() on them); a gateway restart resets
// them to zero, which rate() handles.
//
// The two latency histograms are the exception. The snapshot only holds
// "last" and "average", which cannot show a median or a tail, so the gateway
// records every handoff and every clearance into a histogram at the moment it
// finishes (handoffLatency, clearanceLatency*). Prometheus turns those into
// p50/p95/p99 with histogram_quantile().
//
// /metrics is a read-only endpoint like /api/state and the nodes' /metrics, so
// it stays open when MTLS=on.

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ---------- latency histograms ----------

// latencyBuckets are the upper bounds in seconds: fine at the low end, where
// handoffs and direct clearances live (a few ms), and wide enough at the top for
// a clearance that waited in the standby buffer.
var latencyBuckets = []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

type histogram struct {
	mu     sync.Mutex
	counts []uint64 // counts[i]: observations in (bounds[i-1], bounds[i]]; the last slot is above every bound
	sum    float64
	n      uint64
}

func newHistogram() *histogram { return &histogram{counts: make([]uint64, len(latencyBuckets)+1)} }

// observe records one duration in seconds.
func (h *histogram) observe(sec float64) {
	if sec < 0 {
		sec = 0
	}
	i := sort.SearchFloat64s(latencyBuckets, sec) // first bound >= sec
	h.mu.Lock()
	h.counts[i]++
	h.sum += sec
	h.n++
	h.mu.Unlock()
}

// write emits the _bucket, _sum and _count series. labels is "" or e.g. `path="direct"`.
func (h *histogram) write(w io.Writer, name, labels string) {
	h.mu.Lock()
	counts := append([]uint64(nil), h.counts...)
	sum, n := h.sum, h.n
	h.mu.Unlock()

	prefix, suffix := "", ""
	if labels != "" {
		prefix, suffix = labels+",", "{"+labels+"}"
	}
	var cum uint64
	for i, b := range latencyBuckets {
		cum += counts[i]
		fmt.Fprintf(w, "%s_bucket{%sle=%q} %d\n", name, prefix, strconv.FormatFloat(b, 'g', -1, 64), cum)
	}
	cum += counts[len(latencyBuckets)]
	fmt.Fprintf(w, "%s_bucket{%sle=\"+Inf\"} %d\n", name, prefix, cum)
	fmt.Fprintf(w, "%s_sum%s %s\n", name, suffix, num(sum))
	fmt.Fprintf(w, "%s_count%s %d\n", name, suffix, n)
}

// Package-level, so the code that finishes a handoff or a clearance records into
// them with one line and tests that build a bare gateway need no extra setup.
var (
	handoffLatency           = newHistogram() // one 3-phase handoff, start to commit
	clearanceLatencyDirect   = newHistogram() // clearance request to commit, owner sector was Ready
	clearanceLatencyBuffered = newHistogram() // same, but it waited in the standby buffer first
)

// ---------- text format helpers ----------

type promWriter struct{ w io.Writer }

// head writes the HELP and TYPE lines for one metric. Call it once per name.
func (p promWriter) head(name, typ, help string) {
	fmt.Fprintf(p.w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

// sample writes one series: name{k="v",...} value. kv is key, value, key, value...
func (p promWriter) sample(name string, v float64, kv ...string) {
	fmt.Fprintf(p.w, "%s%s %s\n", name, labelSet(kv), num(v))
}

func labelSet(kv []string) string {
	if len(kv) == 0 {
		return ""
	}
	parts := make([]string, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		parts = append(parts, kv[i]+`="`+escapeLabel(kv[i+1])+`"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func escapeLabel(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return strings.ReplaceAll(s, "\n", `\n`)
}

func num(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sectorStatusCode lets Grafana draw a sector's life as a state timeline.
func sectorStatusCode(status string) float64 {
	switch status {
	case "ACTIVE":
		return 1
	case "DRAINING":
		return 2
	}
	return 0 // STANDBY
}

// ---------- the endpoint ----------

func (g *gateway) metricsHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	writeMetrics(w, g.snapshot())
}

// writeMetrics renders one snapshot. It is a plain function of its input so a
// test can feed it a hand-made picture.
func writeMetrics(w io.Writer, pic picture) {
	p := promWriter{w}

	// --- aircraft ---
	byStatus := map[string]int{"ACTIVE": 0, "TRANSFERRING": 0, "UNOWNED": 0}
	byVerify := map[string]int{VerVerified: 0, VerUnverified: 0, VerSuspect: 0}
	for _, a := range pic.Aircraft {
		switch a.Status {
		case "ACTIVE", "TRANSFERRING":
			byStatus[a.Status]++
		default:
			byStatus["UNOWNED"]++ // reported but not yet claimed in the registry
		}
		if a.Verify != "" {
			byVerify[a.Verify]++
		}
	}
	p.head("atc_aircraft_tracked", "gauge", "Aircraft the gateway is currently tracking.")
	p.sample("atc_aircraft_tracked", float64(len(pic.Aircraft)))
	p.head("atc_aircraft_by_status", "gauge", "Tracked aircraft by ownership status in the registry.")
	for _, k := range sortedKeys(byStatus) {
		p.sample("atc_aircraft_by_status", float64(byStatus[k]), "status", k)
	}
	p.head("atc_aircraft_by_verify", "gauge", "Tracked aircraft by position-verification state (ghost detection).")
	for _, k := range sortedKeys(byVerify) {
		p.sample("atc_aircraft_by_verify", float64(byVerify[k]), "state", k)
	}

	// --- sectors and elastic control ---
	el := pic.Elastic
	p.head("atc_sector_aircraft", "gauge", "Aircraft each sector controls right now.")
	for _, s := range el.Sectors {
		p.sample("atc_sector_aircraft", float64(s.Load), "sector", s.Name)
	}
	p.head("atc_sector_status", "gauge", "Sector state: 0 STANDBY (idle spare), 1 ACTIVE, 2 DRAINING (being merged away).")
	for _, s := range el.Sectors {
		p.sample("atc_sector_status", sectorStatusCode(s.Status), "sector", s.Name)
	}
	p.head("atc_elastic_map_version", "gauge", "Version (log index) of the registry's sector map; changes on every split or merge.")
	p.sample("atc_elastic_map_version", float64(el.Version))
	p.head("atc_elastic_enabled", "gauge", "1 when the automatic split/merge loop is on.")
	p.sample("atc_elastic_enabled", b2f(el.Enabled))
	p.head("atc_elastic_split_threshold", "gauge", "Aircraft in one sector at or above which it is split.")
	p.sample("atc_elastic_split_threshold", float64(el.SplitAt))
	p.head("atc_elastic_merge_threshold", "gauge", "A spare sector and its neighbour together at or below this many aircraft are merged.")
	p.sample("atc_elastic_merge_threshold", float64(el.MergeAt))
	p.head("atc_elastic_actions_total", "counter", "Completed sector reconfigurations.")
	p.sample("atc_elastic_actions_total", float64(el.Splits), "action", "split")
	p.sample("atc_elastic_actions_total", float64(el.Merges), "action", "merge")
	p.sample("atc_elastic_actions_total", float64(el.Undrains), "action", "undrain")

	// --- clusters as the gateway sees them ---
	p.head("atc_cluster_ready", "gauge", "1 when the cluster has exactly one working leader (the Ready flag).")
	for _, c := range sortedKeys(pic.Ready) {
		p.sample("atc_cluster_ready", b2f(pic.Ready[c].Ready), "cluster", c)
	}
	p.head("atc_clearance_queue_length", "gauge", "Clearances waiting in the gateway's buffer for a cluster to become Ready.")
	// Every cluster gets a sample, 0 when nothing waits, so the graph shows a
	// flat line while idle instead of "no data".
	queued := map[string]int{}
	for c := range pic.Ready {
		queued[c] = 0
	}
	for c, n := range pic.ClearanceQueues {
		queued[c] = n
	}
	for _, c := range sortedKeys(queued) {
		p.sample("atc_clearance_queue_length", float64(queued[c]), "cluster", c)
	}

	// --- handoffs ---
	h := pic.Handoff
	p.head("atc_handoffs_total", "counter", "Completed sector-to-sector handoffs.")
	p.sample("atc_handoffs_total", float64(h.Handoffs))
	p.head("atc_handoffs_aborted_total", "counter", "Handoffs rolled back (target had no majority, or a crash).")
	p.sample("atc_handoffs_aborted_total", float64(h.Aborted))
	p.head("atc_handoffs_recovered_total", "counter", "Handoffs found half-done after a crash and finished or rolled back.")
	p.sample("atc_handoffs_recovered_total", float64(h.Recovered))
	p.head("atc_handoffs_deferred_total", "counter", "Handoffs not started because the target sector was not Ready (counted per report).")
	p.sample("atc_handoffs_deferred_total", float64(h.Deferred))
	p.head("atc_claims_total", "counter", "First-sighting ownership claims.")
	p.sample("atc_claims_total", float64(h.Claims))
	p.head("atc_ownership_rpcs_total", "counter", "Ownership RPCs sent to the registry and sectors (begin, accept, commit, abort, claim).")
	p.sample("atc_ownership_rpcs_total", float64(h.RPCs))
	p.head("atc_handoff_queue_length", "gauge", "Handoff and claim jobs waiting for a worker.")
	p.sample("atc_handoff_queue_length", float64(h.Queued))
	p.head("atc_multicast_batches_total", "counter", "Position batches delivered to Raft replicas.")
	p.sample("atc_multicast_batches_total", float64(h.McastBatches))
	p.head("atc_multicast_dropped_total", "counter", "Position batches that failed to reach a replica.")
	p.sample("atc_multicast_dropped_total", float64(h.McastDropped))
	p.head("atc_handoff_duration_seconds", "histogram", "Time for one 3-phase handoff through the registry, begin to commit.")
	handoffLatency.write(w, "atc_handoff_duration_seconds", "")

	// --- clearances ---
	cs := pic.ClearanceStats
	p.head("atc_clearances_total", "counter", "Clearances by final result.")
	p.sample("atc_clearances_total", float64(cs.Committed), "result", "committed")
	p.sample("atc_clearances_total", float64(cs.Rejected), "result", "rejected")
	p.sample("atc_clearances_total", float64(cs.Expired), "result", "expired")
	p.head("atc_clearances_buffered_total", "counter", "Clearances that ever had to wait in the buffer.")
	p.sample("atc_clearances_buffered_total", float64(cs.Buffered))
	p.head("atc_clearance_latency_seconds", "histogram", "Clearance request to Raft commit. path=buffered means it waited for its sector to become Ready.")
	clearanceLatencyDirect.write(w, "atc_clearance_latency_seconds", `path="direct"`)
	clearanceLatencyBuffered.write(w, "atc_clearance_latency_seconds", `path="buffered"`)

	// --- login and authorization ---
	sec := pic.Security
	p.head("atc_logins_total", "counter", "Controller logins.")
	p.sample("atc_logins_total", float64(sec.Logins), "result", "ok")
	p.sample("atc_logins_total", float64(sec.FailedLogins), "result", "failed")
	p.head("atc_clearance_refusals_total", "counter", "Clearances refused before reaching Raft.")
	p.sample("atc_clearance_refusals_total", float64(sec.Unauthorized), "reason", "unauthorized")
	p.sample("atc_clearance_refusals_total", float64(sec.Forbidden), "reason", "forbidden")

	// --- spoofed-aircraft detection ---
	if v := pic.Verify; v != nil {
		p.head("atc_verify_evaluations_total", "counter", "Position checks against the ground receivers' measured ranges.")
		p.sample("atc_verify_evaluations_total", float64(v.Passes), "result", "pass")
		p.sample("atc_verify_evaluations_total", float64(v.Fails), "result", "fail")
		p.sample("atc_verify_evaluations_total", float64(v.NoDecision), "result", "no_decision")
		p.head("atc_spoof_detected_total", "counter", "Aircraft that turned SUSPECT.")
		p.sample("atc_spoof_detected_total", float64(v.Detected))
		p.head("atc_spoof_cleared_total", "counter", "Aircraft that went from SUSPECT back to VERIFIED.")
		p.sample("atc_spoof_cleared_total", float64(v.Cleared))
	}

	// --- conflict detection ---
	if c := pic.Conflicts; c != nil {
		st := c.Stats
		p.head("atc_conflicts_active", "gauge", "Predicted losses of separation right now.")
		p.sample("atc_conflicts_active", float64(len(c.Active)))
		p.head("atc_conflicts_detected_total", "counter", "Conflicts ever predicted.")
		p.sample("atc_conflicts_detected_total", float64(st.Detected))
		p.head("atc_conflicts_resolved_total", "counter", "Conflicts that ended without becoming a loss of separation.")
		p.sample("atc_conflicts_resolved_total", float64(st.Resolved))
		p.head("atc_losses_of_separation_total", "counter", "Conflicts that became an actual loss of separation.")
		p.sample("atc_losses_of_separation_total", float64(st.Losses))
		p.head("atc_losses_unwarned_total", "counter", "Losses of separation with no warning before them.")
		p.sample("atc_losses_unwarned_total", float64(st.Unwarned))
		p.head("atc_conflict_pairs_checked", "gauge", "Candidate pairs the grid produced in the last detector run.")
		p.sample("atc_conflict_pairs_checked", float64(st.PairsChecked))
		p.head("atc_conflict_pairs_bruteforce", "gauge", "Pairs a brute-force n(n-1)/2 check would have tested in the last run.")
		p.sample("atc_conflict_pairs_bruteforce", float64(st.AllPairs))
		p.head("atc_conflict_detector_seconds", "gauge", "Detector run time: last run and running average.")
		p.sample("atc_conflict_detector_seconds", st.LastMs/1000, "stat", "last")
		p.sample("atc_conflict_detector_seconds", st.AvgMs/1000, "stat", "avg")
	}

	// --- live safety invariants ---
	if s := pic.Safety; s != nil {
		p.head("atc_safety_ok", "gauge", "1 while every safety invariant has held at every check so far.")
		p.sample("atc_safety_ok", b2f(s.OK))
		p.head("atc_safety_invariant_ok", "gauge", "1 while this invariant has never been violated.")
		for _, inv := range s.Invariants {
			p.sample("atc_safety_invariant_ok", b2f(inv.OK), "invariant", inv.ID)
		}
		p.head("atc_safety_checks_total", "counter", "Times each invariant was evaluated.")
		for _, inv := range s.Invariants {
			p.sample("atc_safety_checks_total", float64(inv.Checks), "invariant", inv.ID)
		}
		p.head("atc_safety_violations_total", "counter", "Violations found for each invariant.")
		for _, inv := range s.Invariants {
			p.sample("atc_safety_violations_total", float64(inv.Violations), "invariant", inv.ID)
		}
	}
}
