package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/luizcarvalho/genome-hub/internal/httpapi"
	"github.com/luizcarvalho/genome-hub/internal/store"
	"github.com/spf13/cobra"
)

var (
	nodeTracker   string
	nodeAddr      string
	nodeAdvertise string
	nodeCatalog   string
	nodeID        string
	nodeHeartbeat time.Duration
)

var nodeCmd = &cobra.Command{
	Use:   "node",
	Short: "Run a long-lived peer node: serve blobs, announce to the tracker, heartbeat",
	Long: `Starts a participating node (ADR 0003): serves segments/manifests/deltas like
'serve', announces what it holds to the tracker, and heartbeats to stay in peer
lists. On shutdown it deregisters cleanly. This is the unit of participation — a
researcher's opt-in session or an always-on institutional seeder.

Example:
  genomehub node --tracker http://tracker:9000 --addr :8080 \
    --advertise http://my-node:8080 --catalog ./catalog`,
	RunE: runNode,
}

func init() {
	nodeCmd.Flags().StringVar(&nodeTracker, "tracker", "", "tracker base URL (required)")
	nodeCmd.Flags().StringVar(&nodeAddr, "addr", ":8080", "listen address")
	nodeCmd.Flags().StringVar(&nodeAdvertise, "advertise", "", "URL peers use to reach this node (default http://localhost<addr>)")
	nodeCmd.Flags().StringVar(&nodeCatalog, "catalog", ".", "directory of manifests/deltas to serve")
	nodeCmd.Flags().StringVar(&nodeID, "id", "", "node id (default: advertised URL)")
	nodeCmd.Flags().DurationVar(&nodeHeartbeat, "heartbeat", 30*time.Second, "heartbeat interval")
	nodeCmd.MarkFlagRequired("tracker")
	rootCmd.AddCommand(nodeCmd)
}

func runNode(_ *cobra.Command, _ []string) error {
	advertise := nodeAdvertise
	if advertise == "" {
		advertise = "http://localhost" + nodeAddr
	}
	id := nodeID
	if id == "" {
		id = advertise
	}
	tracker := strings.TrimRight(nodeTracker, "/")

	s, err := store.Open(storeDir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer s.Close()

	cat, err := httpapi.ScanCatalog(nodeCatalog)
	if err != nil {
		return fmt.Errorf("scan catalog: %w", err)
	}

	srv := &http.Server{Addr: nodeAddr, Handler: httpapi.NewHandler(s, cat, eventsPath())}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "serve error: %v\n", err)
		}
	}()
	fmt.Fprintf(os.Stderr, "node %s serving %s, tracker %s\n", id, advertise, tracker)

	prevHeld := -1
	announce := func() {
		hashes, err := s.ListHashes()
		if err != nil {
			fmt.Fprintf(os.Stderr, "list hashes: %v\n", err)
			return
		}
		body := announceBody{NodeID: id, Address: advertise, Kind: "node", Hashes: hashes}
		if err := trackerPost(tracker, "/announce", body); err != nil {
			fmt.Fprintf(os.Stderr, "announce failed: %v\n", err)
			return
		}
		// log only when the held count changes, so re-announces are quiet
		if len(hashes) != prevHeld {
			fmt.Fprintf(os.Stderr, "announced %d held segments\n", len(hashes))
			prevHeld = len(hashes)
		}
	}
	announce()

	// heartbeat loop + signal handling
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ticker := time.NewTicker(nodeHeartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, "\nleaving network...")
			_ = trackerPost(tracker, "/leave", announceBody{NodeID: id})
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			srv.Shutdown(shutCtx)
			cancel()
			return nil
		case <-ticker.C:
			// re-announce content each tick so the tracker reflects segments
			// gained since startup (a node that downloaded data becomes a seeder
			// for it); announce also refreshes liveness.
			announce()
		}
	}
}

type announceBody struct {
	NodeID  string   `json:"node_id"`
	Address string   `json:"address"`
	Kind    string   `json:"kind,omitempty"`
	Hashes  []string `json:"hashes,omitempty"`
}

// trackerPost POSTs a JSON body and treats any non-2xx as an error.
func trackerPost(base, path string, body announceBody) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	resp, err := http.Post(base+path, "application/json", bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s: status %d", path, resp.StatusCode)
	}
	return nil
}
