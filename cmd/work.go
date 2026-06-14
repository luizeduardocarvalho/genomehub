package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/luizeduardocarvalho/genomehub/internal/aligner"
	"github.com/luizeduardocarvalho/genomehub/internal/fasta"
	"github.com/luizeduardocarvalho/genomehub/internal/jobs"
	"github.com/luizeduardocarvalho/genomehub/internal/store"
	"github.com/spf13/cobra"
)

var (
	workCoordinator string
	workServer      string
	workTracker     string
	workID          string
	workThreads     int
	workOnce        bool
	workPoll        time.Duration
)

var workCmd = &cobra.Command{
	Use:   "work",
	Short: "Contribute compute: claim MEM-finding jobs, run minimap2, submit MEMs",
	Long: `A worker node (ADR 0002 §5). Claims alignment jobs from the coordinator,
reconstructs the two genomes (downloaded from the swarm), runs minimap2, extracts
exact-match MEMs, and submits them. The coordinator re-verifies them, so a buggy
or dishonest worker's MEMs are simply dropped.

Example:
  genomehub work --coordinator http://coord:9100 --server http://origin:8080`,
	RunE: runWork,
}

func init() {
	workCmd.Flags().StringVar(&workCoordinator, "coordinator", "", "coordinator URL (required)")
	workCmd.Flags().StringVar(&workServer, "server", "", "origin URL to fetch genomes from (required)")
	workCmd.Flags().StringVar(&workTracker, "tracker", "", "tracker URL for peer-served genome fetch (optional)")
	workCmd.Flags().StringVar(&workID, "id", "", "worker id (default: hostname)")
	workCmd.Flags().IntVar(&workThreads, "threads", 4, "minimap2 threads")
	workCmd.Flags().BoolVar(&workOnce, "once", false, "process one job then exit")
	workCmd.Flags().DurationVar(&workPoll, "poll", 5*time.Second, "how long to wait between empty claims")
	workCmd.MarkFlagRequired("coordinator")
	workCmd.MarkFlagRequired("server")
	rootCmd.AddCommand(workCmd)
}

func runWork(_ *cobra.Command, _ []string) error {
	id := workID
	if id == "" {
		if h, err := os.Hostname(); err == nil {
			id = h
		} else {
			id = "worker"
		}
	}
	coord := strings.TrimRight(workCoordinator, "/")

	s, err := store.Open(storeDir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer s.Close()
	d := &downloader{
		server:   strings.TrimRight(workServer, "/"),
		tracker:  strings.TrimRight(workTracker, "/"),
		store:    s,
		parallel: 8,
	}

	// A signal-cancelled context so Ctrl-C kills the running minimap2 instead of
	// orphaning it.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	tmp, err := os.MkdirTemp("", "ghwork")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	for {
		if ctx.Err() != nil {
			return nil
		}
		job, ok, err := claimJob(coord, id)
		if err != nil {
			return fmt.Errorf("claim: %w", err)
		}
		if !ok {
			if workOnce {
				fmt.Fprintln(os.Stderr, "no jobs available")
				return nil
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(workPoll):
			}
			continue
		}
		fmt.Fprintf(os.Stderr, "claimed %s: align %s × %s (%s, min-exact %d)\n",
			job.ID, job.Target, job.Query, job.Preset, job.MinExact)

		mems, err := d.runJob(ctx, job, tmp)
		if err != nil {
			if ctx.Err() != nil {
				fmt.Fprintln(os.Stderr, "\ninterrupted; minimap2 stopped")
				return nil
			}
			return fmt.Errorf("job %s: %w", job.ID, err)
		}
		res, err := submitMEMs(coord, job.ID, id, mems)
		if err != nil {
			return fmt.Errorf("submit: %w", err)
		}
		fmt.Printf("job %s done: %d MEMs found, %d verified by coordinator\n", job.ID, res.Found, res.Valid)

		if workOnce {
			return nil
		}
	}
}

// runJob reconstructs both genomes, aligns them, and extracts MEMs. For a tiled
// job (job.QueryChrom set) only that query chromosome is written and aligned.
// The alignment is bound to ctx so worker shutdown stops minimap2.
func (d *downloader) runJob(ctx context.Context, job jobs.Job, tmp string) ([]jobs.MEM, error) {
	tFA := filepath.Join(tmp, job.Target+".fa")
	qFA := filepath.Join(tmp, job.Query+"_"+job.QueryChrom+".fa")
	if err := d.genomeToFile(job.Target, tFA); err != nil {
		return nil, fmt.Errorf("fetch target: %w", err)
	}

	qchroms, err := d.fetchGenome(job.Query)
	if err != nil {
		return nil, fmt.Errorf("fetch query: %w", err)
	}
	if job.QueryChrom != "" {
		qchroms = filterChrom(qchroms, job.QueryChrom)
		if len(qchroms) == 0 {
			return nil, fmt.Errorf("query chromosome %q not found", job.QueryChrom)
		}
	}
	if err := fasta.Write(qFA, qchroms); err != nil {
		return nil, err
	}

	cfg := aligner.Config{MinLength: job.MinExact, MinIdentity: 0, Preset: job.Preset, Threads: workThreads, WithCIGAR: true}
	blocks, err := aligner.RunContext(ctx, tFA, qFA, cfg)
	if err != nil {
		return nil, err
	}

	var mems []jobs.MEM
	for _, b := range blocks {
		for _, em := range aligner.ExtractExactMatches(b, job.MinExact) {
			mems = append(mems, jobs.MEM{
				TargetChrom: b.TargetName, TargetStart: em.TargetStart,
				QueryChrom: b.QueryName, QueryStart: em.QueryStart,
				Length: em.Len(), Strand: b.Strand,
			})
		}
	}
	return mems, nil
}

func (d *downloader) genomeToFile(assembly, path string) error {
	chroms, err := d.fetchGenome(assembly)
	if err != nil {
		return err
	}
	return fasta.Write(path, chroms)
}

// filterChrom returns only the named chromosome.
func filterChrom(chroms []fasta.Chromosome, name string) []fasta.Chromosome {
	for _, c := range chroms {
		if c.Name == name {
			return []fasta.Chromosome{c}
		}
	}
	return nil
}

// ── coordinator client ────────────────────────────────────────────────────────

func claimJob(coord, worker string) (jobs.Job, bool, error) {
	body, status, err := postJSON(coord+"/jobs/claim", map[string]string{"Worker": worker})
	if err != nil {
		return jobs.Job{}, false, err
	}
	if status == http.StatusNoContent {
		return jobs.Job{}, false, nil
	}
	if status != http.StatusOK {
		return jobs.Job{}, false, fmt.Errorf("status %d", status)
	}
	var j jobs.Job
	if err := json.Unmarshal(body, &j); err != nil {
		return jobs.Job{}, false, err
	}
	return j, true, nil
}

type submitResult struct {
	Found int `json:"found"`
	Valid int `json:"valid"`
}

func submitMEMs(coord, jobID, worker string, mems []jobs.MEM) (submitResult, error) {
	var res submitResult
	body, status, err := postJSON(coord+"/jobs/submit", map[string]any{
		"job_id": jobID, "worker": worker, "mems": mems,
	})
	if err != nil {
		return res, err
	}
	if status != http.StatusOK {
		return res, fmt.Errorf("status %d", status)
	}
	return res, json.Unmarshal(body, &res)
}

// cmdHTTP bounds CLI/TUI calls (polling, actions) so a dead node surfaces an
// error instead of hanging the command.
var cmdHTTP = &http.Client{Timeout: 20 * time.Second}

func postJSON(url string, v any) ([]byte, int, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, 0, err
	}
	resp, err := cmdHTTP.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return body, resp.StatusCode, nil
}
