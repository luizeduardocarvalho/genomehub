package cmd

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/luizeduardocarvalho/genomehub/internal/httpapi"
	"github.com/luizeduardocarvalho/genomehub/internal/tracker"
	"github.com/spf13/cobra"
)

var (
	trackerAddr      string
	trackerTimeout   time.Duration
	trackerRequireID bool
	trackerVerifyAnn bool
)

var trackerCmd = &cobra.Command{
	Use:   "tracker",
	Short: "Run the peer tracker (stateless hash -> nodes index + liveness)",
	Long: `Coordinates the peer network. Nodes announce what they hold and heartbeat to
stay live; clients ask which nodes have a given segment hash. Knows nothing about
genome structure; state is in-memory and rebuilt from announces.

Endpoints:
  POST /announce    {node_id, address, kind, hashes}
  POST /heartbeat   {node_id}
  POST /leave       {node_id}
  GET  /peers/{hash}   who holds this hash
  GET  /nodes          all known nodes + liveness
  GET  /healthz`,
	RunE: runTracker,
}

func init() {
	trackerCmd.Flags().StringVar(&trackerAddr, "addr", ":9000", "listen address")
	trackerCmd.Flags().DurationVar(&trackerTimeout, "timeout", tracker.DefaultTimeout, "heartbeat timeout before a node is dropped")
	trackerCmd.Flags().BoolVar(&trackerRequireID, "require-identity", false, "reject unsigned announce/leave (nodes must run with --identity)")
	trackerCmd.Flags().BoolVar(&trackerVerifyAnn, "verify-announce", false, "spot-check that announcing nodes actually serve a sample of the hashes they claim")
	rootCmd.AddCommand(trackerCmd)
}

func runTracker(_ *cobra.Command, _ []string) error {
	reg := tracker.NewRegistry(trackerTimeout)
	reg.RequireIdentity = trackerRequireID
	reg.VerifyAnnounce = trackerVerifyAnn
	fmt.Fprintf(os.Stderr, "tracker on %s (timeout %s)\n", trackerAddr, trackerTimeout)
	if trackerRequireID {
		fmt.Fprintln(os.Stderr, "  identity: required (unsigned announce/leave rejected)")
	}
	if trackerVerifyAnn {
		fmt.Fprintln(os.Stderr, "  verify:   announced segments are spot-checked")
	}
	return http.ListenAndServe(trackerAddr, httpapi.CORS(reg.Handler()))
}
