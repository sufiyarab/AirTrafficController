package main

// Tests for metrics.go. The main one is checkExposition: it holds everything the
// gateway prints to the text format Prometheus parses (HELP/TYPE once per
// metric and before its samples, well-formed labels, numeric values, no
// duplicate series, cumulative histogram buckets that end at +Inf == _count).
// If the output breaks those rules, Prometheus rejects the whole scrape and
// every graph goes blank, so it is worth pinning.

import (
	"bytes"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"atc/internal/fsm"
)

var (
	sampleRe = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{(.*)\})? (\S+)$`)
	labelRe  = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"`)
)

// checkExposition fails the test for anything Prometheus would reject, and
// returns every sample as "name{labels}" -> value for the caller to assert on.
func checkExposition(t *testing.T, text string) map[string]float64 {
	t.Helper()
	typeOf := map[string]string{}
	helpSeen := map[string]bool{}
	samples := map[string]float64{}
	type bucket struct {
		le  float64
		val float64
	}
	buckets := map[string][]bucket{} // family|labels-without-le -> buckets in file order
	sums, counts := map[string]float64{}, map[string]float64{}

	for n, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		loc := func() string { return "line " + strconv.Itoa(n+1) + ": " + line }
		switch {
		case line == "":
			t.Errorf("%s: blank line", loc())
		case strings.HasPrefix(line, "# HELP "):
			f := strings.Fields(line)
			if len(f) < 4 {
				t.Errorf("%s: HELP without text", loc())
				continue
			}
			if helpSeen[f[2]] {
				t.Errorf("%s: second HELP for %s", loc(), f[2])
			}
			helpSeen[f[2]] = true
		case strings.HasPrefix(line, "# TYPE "):
			f := strings.Fields(line)
			if len(f) != 4 {
				t.Errorf("%s: bad TYPE", loc())
				continue
			}
			if _, dup := typeOf[f[2]]; dup {
				t.Errorf("%s: second TYPE for %s", loc(), f[2])
			}
			typeOf[f[2]] = f[3]
		case strings.HasPrefix(line, "#"):
			t.Errorf("%s: unknown comment", loc())
		default:
			m := sampleRe.FindStringSubmatch(line)
			if m == nil {
				t.Errorf("%s: not a sample", loc())
				continue
			}
			name, rawLabels, rawVal := m[1], m[2], m[3]
			val, err := strconv.ParseFloat(rawVal, 64)
			if err != nil || math.IsNaN(val) {
				t.Errorf("%s: bad value %q", loc(), rawVal)
				continue
			}
			// labels must be fully consumed by well-formed pairs
			pairs := labelRe.FindAllStringSubmatch(rawLabels, -1)
			rebuilt := make([]string, 0, len(pairs))
			labels := map[string]string{}
			for _, p := range pairs {
				rebuilt = append(rebuilt, p[0])
				labels[p[1]] = p[2]
			}
			if strings.Join(rebuilt, ",") != rawLabels {
				t.Errorf("%s: malformed labels %q", loc(), rawLabels)
			}
			// family: histogram series carry _bucket/_sum/_count suffixes
			family, suffix := name, ""
			for _, sfx := range []string{"_bucket", "_sum", "_count"} {
				if base := strings.TrimSuffix(name, sfx); base != name && typeOf[base] == "histogram" {
					family, suffix = base, sfx
				}
			}
			if _, ok := typeOf[family]; !ok {
				t.Errorf("%s: sample before (or without) a TYPE line for %s", loc(), family)
			}
			if !helpSeen[family] {
				t.Errorf("%s: sample before (or without) a HELP line for %s", loc(), family)
			}
			// duplicate series
			keys := make([]string, 0, len(labels))
			for k, v := range labels {
				keys = append(keys, k+"="+v)
			}
			sort.Strings(keys)
			id := name + "{" + strings.Join(keys, ",") + "}"
			if _, dup := samples[id]; dup {
				t.Errorf("%s: duplicate series %s", loc(), id)
			}
			samples[id] = val
			// histogram bookkeeping
			if typeOf[family] == "histogram" {
				var rest []string
				for _, k := range keys {
					if !strings.HasPrefix(k, "le=") {
						rest = append(rest, k)
					}
				}
				group := family + "|" + strings.Join(rest, ",")
				switch suffix {
				case "_bucket":
					le := math.Inf(1)
					if labels["le"] != "+Inf" {
						le, err = strconv.ParseFloat(labels["le"], 64)
						if err != nil {
							t.Errorf("%s: bad le %q", loc(), labels["le"])
						}
					}
					buckets[group] = append(buckets[group], bucket{le, val})
				case "_sum":
					sums[group] = val
				case "_count":
					counts[group] = val
				default:
					t.Errorf("%s: bare sample for histogram %s", loc(), family)
				}
			}
		}
	}
	for name := range typeOf {
		if !helpSeen[name] {
			t.Errorf("TYPE without HELP for %s", name)
		}
	}
	for group, bs := range buckets {
		for i := 1; i < len(bs); i++ {
			if bs[i].le <= bs[i-1].le {
				t.Errorf("%s: bucket bounds not increasing", group)
			}
			if bs[i].val < bs[i-1].val {
				t.Errorf("%s: buckets not cumulative", group)
			}
		}
		last := bs[len(bs)-1]
		if !math.IsInf(last.le, 1) {
			t.Errorf("%s: last bucket is not +Inf", group)
		}
		c, ok := counts[group]
		if !ok {
			t.Errorf("%s: no _count", group)
		} else if c != last.val {
			t.Errorf("%s: _count %v != +Inf bucket %v", group, c, last.val)
		}
		if _, ok := sums[group]; !ok {
			t.Errorf("%s: no _sum", group)
		}
	}
	return samples
}

func want(t *testing.T, got map[string]float64, series string, v float64) {
	t.Helper()
	g, ok := got[series]
	if !ok {
		t.Errorf("missing %s", series)
		return
	}
	if math.Abs(g-v) > 1e-9 {
		t.Errorf("%s = %v, want %v", series, g, v)
	}
}

func fullPicture() picture {
	return picture{
		Aircraft: []position{
			{Callsign: "A1", Status: "ACTIVE", Verify: VerVerified},
			{Callsign: "A2", Status: "ACTIVE", Verify: VerVerified},
			{Callsign: "A3", Status: "TRANSFERRING", Verify: VerSuspect},
			{Callsign: "A4", Status: "", Verify: VerUnverified}, // reported, not yet claimed
		},
		Elastic: elasticPicture{
			Enabled: true, SplitAt: 40, MergeAt: 20, Version: 9, Splits: 2, Merges: 1, Undrains: 3,
			Sectors: []sectorLoad{
				{Name: "sector-1", Status: "ACTIVE", Load: 42},
				{Name: "standby", Status: "DRAINING", Load: 3},
				{Name: "spare-2", Status: "STANDBY"},
			},
		},
		Ready:           map[string]readiness{"sector-1": {Ready: true}, "sector-2": {Ready: false}},
		ClearanceQueues: map[string]int{"sector-2": 3},
		Handoff:         stats{Handoffs: 7, Aborted: 1, Recovered: 2, Deferred: 4, Claims: 30, RPCs: 90, Queued: 5, McastBatches: 100, McastDropped: 6},
		ClearanceStats:  clearanceStats{Committed: 5, Rejected: 2, Expired: 1, Buffered: 3},
		Security:        securityStats{Logins: 4, FailedLogins: 2, Unauthorized: 1, Forbidden: 1},
		Verify:          &verifyStats{Passes: 10, Fails: 2, NoDecision: 1, Detected: 3, Cleared: 1},
		Conflicts: &conflictPicture{
			Active: []conflict{{}, {}},
			Stats:  conflictStats{Detected: 6, Resolved: 4, Losses: 1, Unwarned: 1, PairsChecked: 12, AllPairs: 4950, LastMs: 1.5, AvgMs: 2},
		},
		Safety: &safetyPicture{OK: false, Invariants: []invariantStatus{
			{ID: "one_owner", OK: true, Checks: 100},
			{ID: "one_leader", OK: false, Checks: 100, Violations: 2},
		}},
	}
}

func render(pic picture) string {
	var b bytes.Buffer
	writeMetrics(&b, pic)
	return b.String()
}

func TestMetricsValues(t *testing.T) {
	got := checkExposition(t, render(fullPicture()))

	want(t, got, `atc_aircraft_tracked{}`, 4)
	want(t, got, `atc_aircraft_by_status{status=ACTIVE}`, 2)
	want(t, got, `atc_aircraft_by_status{status=TRANSFERRING}`, 1)
	want(t, got, `atc_aircraft_by_status{status=UNOWNED}`, 1)
	want(t, got, `atc_aircraft_by_verify{state=SUSPECT}`, 1)
	want(t, got, `atc_aircraft_by_verify{state=VERIFIED}`, 2)

	want(t, got, `atc_sector_aircraft{sector=sector-1}`, 42)
	want(t, got, `atc_sector_status{sector=sector-1}`, 1)
	want(t, got, `atc_sector_status{sector=standby}`, 2)
	want(t, got, `atc_sector_status{sector=spare-2}`, 0)
	want(t, got, `atc_elastic_map_version{}`, 9)
	want(t, got, `atc_elastic_enabled{}`, 1)
	want(t, got, `atc_elastic_split_threshold{}`, 40)
	want(t, got, `atc_elastic_merge_threshold{}`, 20)
	want(t, got, `atc_elastic_actions_total{action=split}`, 2)
	want(t, got, `atc_elastic_actions_total{action=merge}`, 1)
	want(t, got, `atc_elastic_actions_total{action=undrain}`, 3)

	want(t, got, `atc_cluster_ready{cluster=sector-1}`, 1)
	want(t, got, `atc_cluster_ready{cluster=sector-2}`, 0)
	want(t, got, `atc_clearance_queue_length{cluster=sector-2}`, 3)
	want(t, got, `atc_clearance_queue_length{cluster=sector-1}`, 0) // idle clusters still report 0

	want(t, got, `atc_handoffs_total{}`, 7)
	want(t, got, `atc_handoffs_aborted_total{}`, 1)
	want(t, got, `atc_handoffs_recovered_total{}`, 2)
	want(t, got, `atc_handoffs_deferred_total{}`, 4)
	want(t, got, `atc_claims_total{}`, 30)
	want(t, got, `atc_ownership_rpcs_total{}`, 90)
	want(t, got, `atc_handoff_queue_length{}`, 5)
	want(t, got, `atc_multicast_batches_total{}`, 100)
	want(t, got, `atc_multicast_dropped_total{}`, 6)

	want(t, got, `atc_clearances_total{result=committed}`, 5)
	want(t, got, `atc_clearances_total{result=rejected}`, 2)
	want(t, got, `atc_clearances_total{result=expired}`, 1)
	want(t, got, `atc_clearances_buffered_total{}`, 3)

	want(t, got, `atc_logins_total{result=ok}`, 4)
	want(t, got, `atc_logins_total{result=failed}`, 2)
	want(t, got, `atc_clearance_refusals_total{reason=unauthorized}`, 1)
	want(t, got, `atc_clearance_refusals_total{reason=forbidden}`, 1)

	want(t, got, `atc_verify_evaluations_total{result=pass}`, 10)
	want(t, got, `atc_verify_evaluations_total{result=fail}`, 2)
	want(t, got, `atc_spoof_detected_total{}`, 3)
	want(t, got, `atc_spoof_cleared_total{}`, 1)

	want(t, got, `atc_conflicts_active{}`, 2)
	want(t, got, `atc_conflicts_detected_total{}`, 6)
	want(t, got, `atc_conflicts_resolved_total{}`, 4)
	want(t, got, `atc_losses_of_separation_total{}`, 1)
	want(t, got, `atc_losses_unwarned_total{}`, 1)
	want(t, got, `atc_conflict_pairs_checked{}`, 12)
	want(t, got, `atc_conflict_pairs_bruteforce{}`, 4950)
	want(t, got, `atc_conflict_detector_seconds{stat=last}`, 0.0015)
	want(t, got, `atc_conflict_detector_seconds{stat=avg}`, 0.002)

	want(t, got, `atc_safety_ok{}`, 0)
	want(t, got, `atc_safety_invariant_ok{invariant=one_owner}`, 1)
	want(t, got, `atc_safety_invariant_ok{invariant=one_leader}`, 0)
	want(t, got, `atc_safety_violations_total{invariant=one_leader}`, 2)
	want(t, got, `atc_safety_checks_total{invariant=one_owner}`, 100)
}

func TestMetricsOptionalSectionsAreSkipped(t *testing.T) {
	// A gateway with ghost detection off and no safety monitor or conflict detector
	// (as in tests, or a stripped-down deployment) must still print valid output.
	got := checkExposition(t, render(picture{}))
	for series := range got {
		for _, absent := range []string{"atc_safety_", "atc_conflict", "atc_verify_", "atc_spoof_"} {
			if strings.HasPrefix(series, absent) {
				t.Errorf("%s printed without its data source", series)
			}
		}
	}
	// the always-on core is still there, at zero
	want(t, got, `atc_aircraft_tracked{}`, 0)
	want(t, got, `atc_handoffs_total{}`, 0)
}

func TestHistogramBuckets(t *testing.T) {
	h := newHistogram()
	for _, v := range []float64{0.0005, 0.001, 0.003, 50, -1} { // -1 is clamped to 0
		h.observe(v)
	}
	var b bytes.Buffer
	b.WriteString("# HELP x_seconds t\n# TYPE x_seconds histogram\n")
	h.write(&b, "x_seconds", `path="p"`)
	got := checkExposition(t, b.String())

	want(t, got, `x_seconds_bucket{le=0.001,path=p}`, 3) // 0.0005, 0.001 (a bound counts in its own bucket), and 0
	want(t, got, `x_seconds_bucket{le=0.0025,path=p}`, 3)
	want(t, got, `x_seconds_bucket{le=0.005,path=p}`, 4)
	want(t, got, `x_seconds_bucket{le=30,path=p}`, 4) // 50 is above every bound
	want(t, got, `x_seconds_bucket{le=+Inf,path=p}`, 5)
	want(t, got, `x_seconds_count{path=p}`, 5)
	want(t, got, `x_seconds_sum{path=p}`, 50.0045)

	// without labels the braces still carry le
	var c bytes.Buffer
	c.WriteString("# HELP y_seconds t\n# TYPE y_seconds histogram\n")
	newHistogram().write(&c, "y_seconds", "")
	checkExposition(t, c.String())
	if !strings.Contains(c.String(), "y_seconds_count 0\n") {
		t.Errorf("unlabelled _count should have no braces:\n%s", c.String())
	}
}

func TestLabelEscaping(t *testing.T) {
	var b bytes.Buffer
	p := promWriter{&b}
	p.head("m", "gauge", "t")
	p.sample("m", 1, "k", "a\"b\\c\nd")
	got := checkExposition(t, b.String())
	if len(got) != 1 {
		t.Fatalf("got %v from %q", got, b.String())
	}
}

func TestMetricsEndpointOnALiveGateway(t *testing.T) {
	g, _ := newElasticGateway(testCfg()) // real registry state machine behind the fake endpoint
	g.refreshSectors()
	g.aircraft["X1"] = position{Callsign: "X1", Owner: "sector-1", Status: "ACTIVE"}

	rec := httptest.NewRecorder()
	g.metricsHandler(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Fatalf("content type %q", ct)
	}
	got := checkExposition(t, rec.Body.String())
	want(t, got, `atc_aircraft_tracked{}`, 1)
	want(t, got, `atc_sector_aircraft{sector=sector-1}`, 1)
	want(t, got, `atc_sector_status{sector=sector-1}`, 1)
	want(t, got, `atc_sector_status{sector=standby}`, 0) // the idle spare cluster
	if got[`atc_elastic_map_version{}`] == 0 {
		t.Error("the seeded sector map should have a version")
	}
}

func histN(h *histogram) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}

func TestClearanceLatencyIsRecorded(t *testing.T) {
	direct0, buffered0 := histN(clearanceLatencyDirect), histN(clearanceLatencyBuffered)

	h := newHarness(t)
	h.own("ATC001", "sector-2", fsm.Active)
	if code, c := h.post(t, "ATC001", "descend fl250"); code != http.StatusOK || c.Status != ClrCommitted {
		t.Fatalf("got %d %+v", code, c)
	}
	if d, b := histN(clearanceLatencyDirect)-direct0, histN(clearanceLatencyBuffered)-buffered0; d != 1 || b != 0 {
		t.Fatalf("a direct commit: direct +%d, buffered +%d", d, b)
	}

	// one that has to wait for its sector to become Ready
	h.own("ATC002", "sector-1", fsm.Active)
	h.setLeader("sector-1", false)
	if code, _ := h.post(t, "ATC002", "CLIMB FL300"); code != http.StatusAccepted {
		t.Fatalf("want pending, got %d", code)
	}
	if histN(clearanceLatencyDirect)-direct0 != 1 || histN(clearanceLatencyBuffered)-buffered0 != 0 {
		t.Fatal("a clearance still waiting must not be counted yet")
	}
	h.setLeader("sector-1", true)
	h.g.flushQueue("sector-1")
	if d, b := histN(clearanceLatencyDirect)-direct0, histN(clearanceLatencyBuffered)-buffered0; d != 1 || b != 1 {
		t.Fatalf("after the flush: direct +%d, buffered +%d", d, b)
	}
}
