package main

// nodeClient is every call the gateway makes to a Raft node. The rest of the
// gateway only uses this interface, so the transport underneath (HTTP/JSON
// today, gRPC with mutual TLS next) can change without touching the handoff,
// clearance, elastic or safety code.
//
// Each call has its own short timeout (one second, as before), so callers do
// not pass a context.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"atc/internal/fsm"
)

type nodeClient interface {
	Status(id string) (nodeStatus, error)
	// Apply commits cmd on node id, which must be its cluster's leader.
	// Errors: *notLeaderError, *rejectedError (committed but the state
	// machine refused it), or any other error (unreachable, commit timeout).
	Apply(id string, cmd fsm.Command) (applyReply, error)
	Track(id string, batch []position) error
	Owners(id string) (map[string]fsm.Owner, error)
	// Sectors returns the registry's sector map. ok is false when the node
	// does not serve one (built before the sector map existed).
	Sectors(id string) (m fsm.SectorMap, ok bool, err error)
	State(id string) (map[string]json.RawMessage, error)
}

// applyReply is a committed command: its Raft log index (the total order) and
// the state machine's result as JSON (an fsm.Owner, an fsm.SectorMap, or null).
type applyReply struct {
	Index  uint64
	Result json.RawMessage
}

// notLeaderError means the node is a follower. LeaderID is its view of the
// current leader, possibly empty during an election.
type notLeaderError struct{ LeaderID string }

func (e *notLeaderError) Error() string { return "not leader (leader is " + e.LeaderID + ")" }

// rejectedError means the node committed the command but the state machine
// refused it, so retrying will not help.
type rejectedError struct{ msg string }

func (e *rejectedError) Error() string { return e.msg }

// ---------- HTTP/JSON implementation (the original transport) ----------

type httpNodes struct{ client *http.Client }

func newHTTPNodes(timeout time.Duration) *httpNodes {
	return &httpNodes{client: &http.Client{Timeout: timeout}}
}

func (h *httpNodes) url(id, path string) string { return "http://" + id + ":8080" + path }

func (h *httpNodes) getJSON(id, path string, out any) (int, error) {
	resp, err := h.client.Get(h.url(id, path))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
}

func (h *httpNodes) Status(id string) (nodeStatus, error) {
	st := nodeStatus{ID: id}
	code, err := h.getJSON(id, "/status", &st)
	if err == nil && code != http.StatusOK {
		err = fmt.Errorf("status: HTTP %d", code)
	}
	return st, err
}

func (h *httpNodes) Apply(id string, cmd fsm.Command) (applyReply, error) {
	body, _ := json.Marshal(cmd)
	resp, err := h.client.Post(h.url(id, "/apply"), "application/json", bytes.NewReader(body))
	if err != nil {
		return applyReply{}, err
	}
	defer resp.Body.Close()
	var res struct {
		Error    string          `json:"error"`
		LeaderID string          `json:"leader_id"`
		Index    uint64          `json:"index"`
		Result   json.RawMessage `json:"result"`
	}
	json.NewDecoder(resp.Body).Decode(&res)
	switch resp.StatusCode {
	case http.StatusOK:
		return applyReply{Index: res.Index, Result: res.Result}, nil
	case http.StatusMisdirectedRequest:
		return applyReply{}, &notLeaderError{res.LeaderID}
	case http.StatusBadRequest:
		return applyReply{}, &rejectedError{res.Error}
	default: // 503: commit failed or timed out
		return applyReply{}, fmt.Errorf("%d %s", resp.StatusCode, res.Error)
	}
}

func (h *httpNodes) Track(id string, batch []position) error {
	body, _ := json.Marshal(batch)
	resp, err := h.client.Post(h.url(id, "/track"), "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (h *httpNodes) Owners(id string) (map[string]fsm.Owner, error) {
	var owners map[string]fsm.Owner
	code, err := h.getJSON(id, "/owners", &owners)
	if err == nil && code != http.StatusOK {
		err = fmt.Errorf("owners: HTTP %d", code)
	}
	return owners, err
}

func (h *httpNodes) Sectors(id string) (fsm.SectorMap, bool, error) {
	var m fsm.SectorMap
	code, err := h.getJSON(id, "/sectors", &m)
	return m, err == nil && code == http.StatusOK, err
}

func (h *httpNodes) State(id string) (map[string]json.RawMessage, error) {
	var state map[string]json.RawMessage
	code, err := h.getJSON(id, "/state", &state)
	if err == nil && code != http.StatusOK {
		err = fmt.Errorf("state: HTTP %d", code)
	}
	return state, err
}
