package cmd

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/luizeduardocarvalho/genomehub/internal/httpapi"
	"github.com/luizeduardocarvalho/genomehub/internal/sign"
	"github.com/luizeduardocarvalho/genomehub/internal/store"
	trackerpkg "github.com/luizeduardocarvalho/genomehub/internal/tracker"
	"github.com/spf13/cobra"
)

var (
	nodeTracker   string
	nodeAddr      string
	nodeAdvertise string
	nodeCatalog   string
	nodeID        string
	nodeHeartbeat time.Duration
	nodeRegistry  string
	nodeTLSCert   string
	nodeTLSKey    string
	nodeSignKey   string
	nodeRate      float64
	nodeCacheMax  string
	nodeIdentity  string
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
	nodeCmd.Flags().StringVar(&nodeRegistry, "registry", "", "upstream registry URL (origin) for the /discover endpoint")
	nodeCmd.Flags().StringVar(&nodeTLSCert, "tls-cert", "", "PEM certificate file; enables HTTPS when set with --tls-key")
	nodeCmd.Flags().StringVar(&nodeTLSKey, "tls-key", "", "PEM private key file; enables HTTPS when set with --tls-cert")
	nodeCmd.Flags().StringVar(&nodeSignKey, "sign-key", "", "ed25519 private key file (from `keygen`); signs served manifests")
	nodeCmd.Flags().Float64Var(&nodeRate, "rate", 0, "max requests/second per client IP (0 = unlimited)")
	nodeCmd.Flags().StringVar(&nodeCacheMax, "cache-max", "", "bounded LRU cache size, e.g. 50GB (empty = unbounded)")
	nodeCmd.Flags().StringVar(&nodeIdentity, "identity", "", "ed25519 private key file (from `keygen`); node id becomes its public key and announces are signed")
	nodeCmd.MarkFlagRequired("tracker")
	rootCmd.AddCommand(nodeCmd)
}

func runNode(_ *cobra.Command, _ []string) error {
	if (nodeTLSCert == "") != (nodeTLSKey == "") {
		return fmt.Errorf("--tls-cert and --tls-key must be set together")
	}
	tlsOn := nodeTLSCert != "" && nodeTLSKey != ""

	advertise := nodeAdvertise
	if advertise == "" {
		scheme := "http"
		if tlsOn {
			scheme = "https"
		}
		advertise = scheme + "://localhost" + nodeAddr
	}
	id := nodeID
	if id == "" {
		id = advertise
	}

	// Stable cryptographic identity: the node id becomes its public key and
	// announce/leave are signed, so no other node can announce or leave as it.
	var idSigner *sign.Signer
	if nodeIdentity != "" {
		sg, lerr := sign.LoadSigner(nodeIdentity)
		if lerr != nil {
			return fmt.Errorf("load identity: %w", lerr)
		}
		idSigner = sg
		if nodeID != "" {
			fmt.Fprintln(os.Stderr, "  note: --identity overrides --id")
		}
		id = sg.PublicHex()
		fmt.Fprintf(os.Stderr, "  identity: %s\n", id)
	}
	tracker := strings.TrimRight(nodeTracker, "/")

	s, err := store.Open(storeDir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer s.Close()

	if err := applyCacheLimit(s, nodeCacheMax); err != nil {
		return err
	}

	cat, err := httpapi.ScanCatalog(nodeCatalog)
	if err != nil {
		return fmt.Errorf("scan catalog: %w", err)
	}
	mergeManifestCache(cat)

	opts, err := signerOpts(cat, nodeSignKey)
	if err != nil {
		return err
	}
	h := httpapi.RateLimit(nodeRate, httpapi.CORS(httpapi.ControlAuth(authToken, httpapi.NewHandler(s, cat, eventsPath(), nodeRegistry, manifestCacheDir(), tracker, opts...))))
	warnIfControlPlaneOpen()
	srv := &http.Server{Addr: nodeAddr, Handler: h}
	go func() {
		if err := listenAndServe(srv, nodeTLSCert, nodeTLSKey); err != nil && err != http.ErrServerClosed {
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
		signBody("announce", &body, idSigner)
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
			lb := announceBody{NodeID: id, Address: advertise}
			signBody("leave", &lb, idSigner)
			_ = trackerPost(tracker, "/leave", lb)
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
	NodeID    string   `json:"node_id"`
	Address   string   `json:"address"`
	Kind      string   `json:"kind,omitempty"`
	Hashes    []string `json:"hashes,omitempty"`
	Timestamp int64    `json:"ts,omitempty"`
	Signature string   `json:"sig,omitempty"`
}

// signBody fills the timestamp and ed25519 signature over the tracker's
// canonical message, proving this node owns its identity. No-op without an
// identity key (unsigned announce, backward compatible).
func signBody(op string, b *announceBody, sg *sign.Signer) {
	if sg == nil {
		return
	}
	b.Timestamp = time.Now().Unix()
	msg := trackerpkg.CanonicalMessage(op, b.NodeID, b.Address, b.Timestamp, trackerpkg.HashesDigest(b.Hashes))
	b.Signature = hex.EncodeToString(sg.Sign(msg))
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
