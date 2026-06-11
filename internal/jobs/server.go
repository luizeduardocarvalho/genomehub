package jobs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Resolver returns a genome's chromosomes as name -> sequence. The coordinator
// uses it to re-verify submitted MEMs against its own copy of the data.
type Resolver func(assembly string) (map[string][]byte, error)

// Coordinator is the HTTP front-end of the job queue plus MEM verification.
type Coordinator struct {
	q       *Queue
	resolve Resolver
	memDir  string

	mu    sync.Mutex
	cache map[string]map[string][]byte // assembly -> chrom -> seq
}

func NewCoordinator(q *Queue, resolve Resolver, memDir string) *Coordinator {
	return &Coordinator{q: q, resolve: resolve, memDir: memDir, cache: map[string]map[string][]byte{}}
}

// seqs returns a genome's sequences, caching the reconstruction.
func (c *Coordinator) seqs(assembly string) (map[string][]byte, error) {
	c.mu.Lock()
	if s, ok := c.cache[assembly]; ok {
		c.mu.Unlock()
		return s, nil
	}
	c.mu.Unlock()
	s, err := c.resolve(assembly)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.cache[assembly] = s
	c.mu.Unlock()
	return s, nil
}

// verify re-checks one MEM by exact byte comparison at the claimed coordinates.
// This is what makes workers untrusted: a false claim fails here and is dropped.
func verify(tseq, qseq map[string][]byte, m MEM) bool {
	t, ok := tseq[m.TargetChrom]
	if !ok {
		return false
	}
	q, ok := qseq[m.QueryChrom]
	if !ok {
		return false
	}
	if m.Length <= 0 ||
		m.TargetStart < 0 || m.TargetStart+m.Length > len(t) ||
		m.QueryStart < 0 || m.QueryStart+m.Length > len(q) {
		return false
	}
	tg := t[m.TargetStart : m.TargetStart+m.Length]
	qg := q[m.QueryStart : m.QueryStart+m.Length]
	if m.Strand == "-" {
		qg = revComp(qg)
	}
	return bytes.Equal(tg, qg)
}

func revComp(s []byte) []byte {
	out := make([]byte, len(s))
	for i, b := range s {
		var c byte
		switch b {
		case 'A', 'a':
			c = 'T'
		case 'C', 'c':
			c = 'G'
		case 'G', 'g':
			c = 'C'
		case 'T', 't':
			c = 'A'
		default:
			c = 'N'
		}
		out[len(s)-1-i] = c
	}
	return out
}

func (c *Coordinator) Handler() http.Handler {
	go func() {
		t := time.NewTicker(30 * time.Second)
		for range t.C {
			c.q.GC()
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })

	mux.HandleFunc("POST /jobs/enqueue", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Target, Query, Preset string
			MinExact              int  `json:"min_exact"`
			Tile                  bool `json:"tile"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.Target == "" || req.Query == "" {
			http.Error(w, "bad enqueue", http.StatusBadRequest)
			return
		}
		if req.Preset == "" {
			req.Preset = "asm5"
		}
		if req.MinExact == 0 {
			req.MinExact = 500
		}

		// Tiled: one job per query chromosome (parallelisable, streams progress).
		if req.Tile {
			qseq, err := c.seqs(req.Query)
			if err != nil {
				http.Error(w, "resolve query for tiling: "+err.Error(), http.StatusInternalServerError)
				return
			}
			chroms := make([]string, 0, len(qseq))
			for name := range qseq {
				chroms = append(chroms, name)
			}
			sort.Strings(chroms)
			created := make([]Job, 0, len(chroms))
			for _, ch := range chroms {
				created = append(created, *c.q.Enqueue(req.Target, req.Query, ch, req.Preset, req.MinExact))
			}
			writeJSON(w, created)
			return
		}
		writeJSON(w, []Job{*c.q.Enqueue(req.Target, req.Query, "", req.Preset, req.MinExact)})
	})

	mux.HandleFunc("POST /jobs/claim", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Worker string }
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.Worker == "" {
			http.Error(w, "bad claim", http.StatusBadRequest)
			return
		}
		j, ok := c.q.Claim(req.Worker)
		if !ok {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(w, j)
	})

	mux.HandleFunc("POST /jobs/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ JobID, Worker string }
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "bad heartbeat", http.StatusBadRequest)
			return
		}
		if !c.q.Heartbeat(req.JobID, req.Worker) {
			http.Error(w, "not your job", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /jobs/submit", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			JobID  string `json:"job_id"`
			Worker string `json:"worker"`
			MEMs   []MEM  `json:"mems"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "bad submit", http.StatusBadRequest)
			return
		}
		job, ok := c.q.Get(req.JobID)
		if !ok {
			http.Error(w, "unknown job", http.StatusNotFound)
			return
		}
		tseq, err := c.seqs(job.Target)
		if err != nil {
			http.Error(w, "resolve target: "+err.Error(), http.StatusInternalServerError)
			return
		}
		qseq, err := c.seqs(job.Query)
		if err != nil {
			http.Error(w, "resolve query: "+err.Error(), http.StatusInternalServerError)
			return
		}

		valid := make([]MEM, 0, len(req.MEMs))
		for _, m := range req.MEMs {
			if verify(tseq, qseq, m) {
				valid = append(valid, m)
			}
		}
		if !c.q.Complete(req.JobID, req.Worker, len(req.MEMs), len(valid)) {
			http.Error(w, "not your job", http.StatusConflict)
			return
		}
		if err := c.writeMEMs(job, valid); err != nil {
			fmt.Fprintf(os.Stderr, "write mems: %v\n", err)
		}
		fmt.Fprintf(os.Stderr, "job %s: %s×%s — %d submitted, %d verified\n",
			job.ID, job.Target, job.Query, len(req.MEMs), len(valid))
		writeJSON(w, map[string]int{"found": len(req.MEMs), "valid": len(valid)})
	})

	mux.HandleFunc("GET /jobs", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, c.q.List())
	})

	return mux
}

// writeMEMs persists the validated MEMs for a pair to memDir.
func (c *Coordinator) writeMEMs(job Job, mems []MEM) error {
	if c.memDir == "" {
		return nil
	}
	if err := os.MkdirAll(c.memDir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(mems, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(c.memDir, job.Target+"_"+job.Query+".mems.json")
	return os.WriteFile(path, data, 0o644)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
