package cmd

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/dustin/go-humanize"
	"github.com/luizeduardocarvalho/genomehub/internal/httpapi"
	"github.com/luizeduardocarvalho/genomehub/internal/sign"
	"github.com/luizeduardocarvalho/genomehub/internal/store"
	"github.com/spf13/cobra"
)

var (
	serveAddr     string
	serveCatalog  string
	serveRegistry string
	serveTLSCert  string
	serveTLSKey   string
	serveSignKey  string
	serveRate     float64
	serveCacheMax string
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Serve segments, manifests and deltas over HTTP (origin or peer node)",
	Long: `Starts a read-only HTTP node over the local segment store and a catalog of
manifests/deltas. Clients fetch segments by content hash and re-verify them, so
an origin and a peer expose the identical API.

Endpoints:
  GET /segments/{hash}              raw content-addressed segment bytes
  GET /genomes/{assembly}/manifest  manifest JSON
  GET /deltas/{assembly}            delta blob
  GET /catalog                      what this node can serve
  GET /healthz`,
	RunE: runServe,
}

func init() {
	serveCmd.Flags().StringVar(&serveAddr, "addr", ":8080", "listen address")
	serveCmd.Flags().StringVar(&serveCatalog, "catalog", ".", "directory of *.manifest.json and *.delta.* files to serve")
	serveCmd.Flags().StringVar(&serveRegistry, "registry", "", "upstream registry URL for the /discover endpoint (default: self)")
	serveCmd.Flags().StringVar(&serveTLSCert, "tls-cert", "", "PEM certificate file; enables HTTPS when set with --tls-key")
	serveCmd.Flags().StringVar(&serveTLSKey, "tls-key", "", "PEM private key file; enables HTTPS when set with --tls-cert")
	serveCmd.Flags().StringVar(&serveSignKey, "sign-key", "", "ed25519 private key file (from `keygen`); signs served manifests")
	serveCmd.Flags().Float64Var(&serveRate, "rate", 0, "max requests/second per client IP (0 = unlimited)")
	serveCmd.Flags().StringVar(&serveCacheMax, "cache-max", "", "bounded LRU cache size, e.g. 50GB (empty = unbounded; do not set on an origin)")
	rootCmd.AddCommand(serveCmd)
}

func runServe(_ *cobra.Command, _ []string) error {
	s, err := store.Open(storeDir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer s.Close()

	if err := applyCacheLimit(s, serveCacheMax); err != nil {
		return err
	}

	cat, err := httpapi.ScanCatalog(serveCatalog)
	if err != nil {
		return fmt.Errorf("scan catalog %s: %w", serveCatalog, err)
	}
	mergeManifestCache(cat)

	fmt.Fprintf(os.Stderr, "serving on %s\n", serveAddr)
	fmt.Fprintf(os.Stderr, "  store:   %s\n", storeDir)
	fmt.Fprintf(os.Stderr, "  catalog: %s (%d manifests, %d deltas)\n", serveCatalog, len(cat.Manifests), len(cat.Deltas))
	for a := range cat.Manifests {
		fmt.Fprintf(os.Stderr, "    manifest: %s\n", a)
	}
	for a := range cat.Deltas {
		fmt.Fprintf(os.Stderr, "    delta:    %s\n", a)
	}

	opts, err := signerOpts(cat, serveSignKey)
	if err != nil {
		return err
	}
	h := httpapi.RateLimit(serveRate, httpapi.ControlAuth(authToken, httpapi.NewHandler(s, cat, eventsPath(), serveRegistry, manifestCacheDir(), "", opts...)))
	warnIfControlPlaneOpen()
	return listenAndServe(&http.Server{Addr: serveAddr, Handler: h}, serveTLSCert, serveTLSKey)
}

// applyCacheLimit parses a human size (e.g. "50GB") and bounds the store to it
// as an LRU cache. Empty leaves the store unbounded (the origin default).
func applyCacheLimit(s *store.Store, sizeStr string) error {
	if strings.TrimSpace(sizeStr) == "" {
		return nil
	}
	n, err := humanize.ParseBytes(sizeStr)
	if err != nil {
		return fmt.Errorf("parse --cache-max %q: %w", sizeStr, err)
	}
	if err := s.SetCacheLimit(int64(n)); err != nil {
		return fmt.Errorf("set cache limit: %w", err)
	}
	cur, max := s.CacheStats()
	fmt.Fprintf(os.Stderr, "  cache:   bounded to %s (currently %s)\n", humanize.Bytes(uint64(max)), humanize.Bytes(uint64(cur)))
	return nil
}

// signerOpts loads the signing key (if any), signs any catalog manifests that
// lack a signature, and returns the handler option enabling signing. Shared by
// serve and node.
func signerOpts(cat *httpapi.Catalog, signKey string) ([]httpapi.Option, error) {
	if signKey == "" {
		return nil, nil
	}
	sg, err := sign.LoadSigner(signKey)
	if err != nil {
		return nil, fmt.Errorf("load sign key: %w", err)
	}
	n, err := httpapi.SignCatalogManifests(cat, sg)
	if err != nil {
		return nil, fmt.Errorf("sign catalog: %w", err)
	}
	fmt.Fprintf(os.Stderr, "  signing: enabled (pubkey %s); signed %d catalog manifest(s)\n", sg.PublicHex(), n)
	return []httpapi.Option{httpapi.WithSigner(sg)}, nil
}

// warnIfControlPlaneOpen prints a loud warning when no token gates the mutating
// /actions/* endpoints. Anyone who can reach the node could then trigger
// downloads, deletes, or unseeds — set --auth-token (or GENOMEHUB_TOKEN).
func warnIfControlPlaneOpen() {
	if authToken == "" {
		fmt.Fprintln(os.Stderr, "  WARNING: control plane (/actions/*) is UNAUTHENTICATED; set --auth-token or GENOMEHUB_TOKEN")
	} else {
		fmt.Fprintln(os.Stderr, "  auth:    control plane requires bearer token")
	}
}

// listenAndServe starts srv with TLS when both cert and key are supplied, plain
// HTTP otherwise. Supplying only one of the pair is a configuration error.
func listenAndServe(srv *http.Server, certFile, keyFile string) error {
	switch {
	case certFile != "" && keyFile != "":
		fmt.Fprintf(os.Stderr, "  TLS:     enabled (%s)\n", certFile)
		return srv.ListenAndServeTLS(certFile, keyFile)
	case certFile != "" || keyFile != "":
		return fmt.Errorf("--tls-cert and --tls-key must be set together")
	default:
		return srv.ListenAndServe()
	}
}

// mergeManifestCache overlays manifests previously fetched by `download` (kept in
// manifestCacheDir) onto the node's catalog, without overriding catalog entries.
// This lets a pure cache peer — started with an empty catalog — still name the
// genomes it holds segments for, so SEEDING shows real coverage instead of
// "nothing". Best-effort: a missing/empty cache dir is simply a no-op.
func mergeManifestCache(cat *httpapi.Catalog) {
	cached, err := httpapi.ScanCatalog(manifestCacheDir())
	if err != nil {
		return
	}
	for a, p := range cached.Manifests {
		if _, ok := cat.Manifests[a]; !ok {
			cat.Manifests[a] = p
		}
	}
}
