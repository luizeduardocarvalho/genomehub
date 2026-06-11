package cmd

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/luizeduardocarvalho/genomehub/internal/httpapi"
	"github.com/luizeduardocarvalho/genomehub/internal/jobs"
	"github.com/spf13/cobra"
)

var (
	coordAddr    string
	coordCatalog string
	coordMemDir  string
	coordTimeout time.Duration
)

var coordinatorCmd = &cobra.Command{
	Use:   "coordinator",
	Short: "Run the MEM-finding job queue and verify submitted MEMs against local genomes",
	Long: `Holds the distributed MEM-finding queue (ADR 0002 §5). Workers claim alignment
jobs, run minimap2, and submit MEMs; the coordinator re-verifies every MEM by exact
byte comparison against its own copy of the genomes (reconstructed from its catalog)
— so workers are untrusted. Validated MEMs are written to --mem-dir.

Endpoints:
  POST /jobs/enqueue {target, query, preset, min_exact}
  POST /jobs/claim {worker}
  POST /jobs/heartbeat {job_id, worker}
  POST /jobs/submit {job_id, worker, mems}
  GET  /jobs`,
	RunE: runCoordinator,
}

func init() {
	coordinatorCmd.Flags().StringVar(&coordAddr, "addr", ":9100", "listen address")
	coordinatorCmd.Flags().StringVar(&coordCatalog, "catalog", ".", "catalog of manifests (genomes available for verification)")
	coordinatorCmd.Flags().StringVar(&coordMemDir, "mem-dir", "mems", "directory for validated MEMs")
	coordinatorCmd.Flags().DurationVar(&coordTimeout, "timeout", 10*time.Minute, "claimed-job lease before reclaim")
	rootCmd.AddCommand(coordinatorCmd)
}

func runCoordinator(_ *cobra.Command, _ []string) error {
	cat, err := httpapi.ScanCatalog(coordCatalog)
	if err != nil {
		return fmt.Errorf("scan catalog: %w", err)
	}

	// Resolver reconstructs a genome from its manifest + the local store, for
	// MEM verification. Results are cached by the coordinator.
	resolve := func(assembly string) (map[string][]byte, error) {
		path, ok := cat.Manifests[assembly]
		if !ok {
			return nil, fmt.Errorf("no manifest for assembly %q", assembly)
		}
		chroms, err := reconstructFromManifest(path, false)
		if err != nil {
			return nil, err
		}
		m := make(map[string][]byte, len(chroms))
		for _, c := range chroms {
			m[c.Name] = c.Sequence
		}
		return m, nil
	}

	q := jobs.NewQueue(coordTimeout)
	coord := jobs.NewCoordinator(q, resolve, coordMemDir)

	fmt.Fprintf(os.Stderr, "coordinator on %s (catalog %s, mems → %s)\n", coordAddr, coordCatalog, coordMemDir)
	fmt.Fprintf(os.Stderr, "  genomes available: %d manifests\n", len(cat.Manifests))
	return http.ListenAndServe(coordAddr, coord.Handler())
}
