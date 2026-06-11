// Package jobs is the distributed MEM-finding queue (ADR 0002 §5). A coordinator
// holds pending alignment jobs; worker nodes claim them, run minimap2, and submit
// the MEMs they found. The coordinator re-verifies every submitted MEM against
// its own copy of the genomes — so workers are untrusted: a false MEM fails the
// exact-match re-check and is dropped, and reputation only ever rewards useful
// work, never gates correctness (ADR 0002 §2).
//
// Slice 1 is pair-level: one job = align one genome pair. Spatial tiling (one job
// per overlapping window) is a later optimisation behind the same queue.
package jobs

import (
	"fmt"
	"sync"
	"time"
)

type State string

const (
	Pending State = "pending"
	Claimed State = "claimed"
	Done    State = "done"
)

// Job is one alignment unit: find MEMs between Target and Query. When QueryChrom
// is set the job is a tile — align only that query chromosome against the target,
// so a genome pair splits into independent, parallelisable, faster jobs.
type Job struct {
	ID         string    `json:"id"`
	Target     string    `json:"target"` // assembly name
	Query      string    `json:"query"`
	QueryChrom string    `json:"query_chrom,omitempty"` // tile: empty = whole genome
	Preset     string    `json:"preset"`
	MinExact   int       `json:"min_exact"`
	State      State     `json:"state"`
	Worker     string    `json:"worker,omitempty"`
	ClaimedAt  time.Time `json:"claimed_at,omitempty"`
	Found      int       `json:"found"` // MEMs submitted
	Valid      int       `json:"valid"` // MEMs that passed re-verification
}

// MEM is an exact-match claim between a target and query genome.
type MEM struct {
	TargetChrom string `json:"target_chrom"`
	TargetStart int    `json:"target_start"`
	QueryChrom  string `json:"query_chrom"`
	QueryStart  int    `json:"query_start"`
	Length      int    `json:"length"`
	Strand      string `json:"strand"` // "+" or "-"
}

// Queue is the in-memory job store on the coordinator.
type Queue struct {
	mu      sync.Mutex
	jobs    map[string]*Job
	order   []string // FIFO of job ids
	timeout time.Duration
	seq     int
}

func NewQueue(timeout time.Duration) *Queue {
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	return &Queue{jobs: map[string]*Job{}, timeout: timeout}
}

// Enqueue adds a pending job. queryChrom is the tile (empty = whole genome).
func (q *Queue) Enqueue(target, query, queryChrom, preset string, minExact int) *Job {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.seq++
	id := fmt.Sprintf("job-%d", q.seq)
	j := &Job{ID: id, Target: target, Query: query, QueryChrom: queryChrom, Preset: preset, MinExact: minExact, State: Pending}
	q.jobs[id] = j
	q.order = append(q.order, id)
	return j
}

// Claim hands the oldest pending job to worker, marking it claimed.
func (q *Queue) Claim(worker string) (Job, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.reclaimLocked()
	for _, id := range q.order {
		j := q.jobs[id]
		if j.State == Pending {
			j.State = Claimed
			j.Worker = worker
			j.ClaimedAt = time.Now()
			return *j, true
		}
	}
	return Job{}, false
}

// Heartbeat refreshes a claimed job's lease; false if it's not claimed by worker.
func (q *Queue) Heartbeat(id, worker string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	j, ok := q.jobs[id]
	if !ok || j.State != Claimed || j.Worker != worker {
		return false
	}
	j.ClaimedAt = time.Now()
	return true
}

// Complete marks a claimed job done with its found/valid counts.
func (q *Queue) Complete(id, worker string, found, valid int) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	j, ok := q.jobs[id]
	if !ok || j.State != Claimed || j.Worker != worker {
		return false
	}
	j.State = Done
	j.Found = found
	j.Valid = valid
	return true
}

// reclaimLocked returns timed-out claimed jobs to pending. Caller holds mu.
func (q *Queue) reclaimLocked() {
	for _, j := range q.jobs {
		if j.State == Claimed && time.Since(j.ClaimedAt) > q.timeout {
			j.State = Pending
			j.Worker = ""
		}
	}
}

// GC reclaims abandoned jobs; run periodically.
func (q *Queue) GC() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.reclaimLocked()
}

// Get returns a copy of a job by id.
func (q *Queue) Get(id string) (Job, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	j, ok := q.jobs[id]
	if !ok {
		return Job{}, false
	}
	return *j, true
}

// List returns a snapshot of all jobs in FIFO order.
func (q *Queue) List() []Job {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]Job, 0, len(q.order))
	for _, id := range q.order {
		out = append(out, *q.jobs[id])
	}
	return out
}
