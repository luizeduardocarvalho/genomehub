package cmd

import (
	"fmt"
	"net/http"
	"os"

	"github.com/luizeduardocarvalho/genomehub/internal/httpapi"
	"github.com/luizeduardocarvalho/genomehub/internal/store"
	"github.com/spf13/cobra"
)

var (
	serveAddr     string
	serveCatalog  string
	serveRegistry string
	serveTLSCert  string
	serveTLSKey   string
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
	rootCmd.AddCommand(serveCmd)
}

func runServe(_ *cobra.Command, _ []string) error {
	s, err := store.Open(storeDir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer s.Close()

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

	h := httpapi.NewHandler(s, cat, eventsPath(), serveRegistry, manifestCacheDir(), "")
	return listenAndServe(&http.Server{Addr: serveAddr, Handler: h}, serveTLSCert, serveTLSKey)
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
