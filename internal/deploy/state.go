// Package deploy runs the cluster lifecycle: lock, generate, build, bring up the
// fabric and apps on Docker Compose or Kubernetes, provision, verify, tear down.
package deploy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// RunState is <state>/run-state.json.
type RunState struct {
	Verb      string `json:"verb"`
	Target    string `json:"target"`
	PID       int    `json:"pid"`
	StartedAt string `json:"startedAt"`
	Host      string `json:"host"`
	Completed bool   `json:"completed"`
	Outcome   string `json:"outcome,omitempty"`
	Env       string `json:"env,omitempty"`
	Took      string `json:"took,omitempty"`
}

// Lock is the cluster run lock.
type Lock struct {
	path  string
	state string
	rec   RunState
}

// ErrLockHeld is returned when another live process holds the lock.
var ErrLockHeld = errors.New("lock held")

func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// AcquireLock takes the cluster lock. A live holder blocks unless force; a dead
// (stale) holder is taken over silently. A holder on another host counts as live.
func AcquireLock(stateDir, verb, target, env string, force bool) (*Lock, error) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	p := filepath.Join(stateDir, "run.lock")
	if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
		var cur RunState
		if json.Unmarshal(b, &cur) == nil && cur.PID != os.Getpid() {
			live := cur.Host != host || alive(cur.PID)
			if live && !force {
				return nil, fmt.Errorf("%w by pid %d on %s (%s %s since %s); pass --force to take it over",
					ErrLockHeld, cur.PID, cur.Host, cur.Verb, cur.Target, cur.StartedAt)
			}
		}
	}
	l := &Lock{path: p, state: stateDir, rec: RunState{Verb: verb, Target: target, Env: env, PID: os.Getpid(),
		StartedAt: time.Now().UTC().Format(time.RFC3339), Host: host}}
	b, _ := json.Marshal(l.rec)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		return nil, err
	}
	l.writeRunState(false, "")
	return l, nil
}

func (l *Lock) writeRunState(done bool, outcome string) {
	rs := l.rec
	rs.Completed, rs.Outcome = done, outcome
	if done {
		if t, err := time.Parse(time.RFC3339, rs.StartedAt); err == nil {
			rs.Took = time.Since(t).Round(time.Second).String()
		}
	}
	b, _ := json.Marshal(rs)
	_ = os.WriteFile(filepath.Join(l.state, "run-state.json"), b, 0o644)
}

// Release records the outcome, appends a change-ledger record and frees the lock.
func (l *Lock) Release(outcome string) {
	if l == nil {
		return
	}
	l.writeRunState(true, outcome)
	AppendSpool(l.state, map[string]any{
		"at": time.Now().UTC().Format(time.RFC3339), "verb": l.rec.Verb, "target": l.rec.Target,
		"env": l.rec.Env, "outcome": outcome, "host": l.rec.Host, "startedAt": l.rec.StartedAt,
	})
	_ = os.Remove(l.path)
}

// AppendSpool appends one JSON record to the local change ledger.
func AppendSpool(stateDir string, rec map[string]any) {
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(stateDir, "events-spool.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(b, '\n'))
}

// ReadRunState returns the last run state, if any.
func ReadRunState(stateDir string) (*RunState, error) {
	b, err := os.ReadFile(filepath.Join(stateDir, "run-state.json"))
	if err != nil {
		return nil, err
	}
	var rs RunState
	return &rs, json.Unmarshal(b, &rs)
}
