package cmd

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
)

var storeDir string
var cmdStart time.Time

// insecureTLS, when set, makes every outbound HTTPS request skip certificate
// verification. It exists for self-signed certs in development/testing; never
// use it against a real origin. Because it patches the default transport, all
// commands that use http.Get/http.Post/http.DefaultClient inherit it without
// per-call wiring.
var insecureTLS bool

// authToken gates the mutating control plane (/actions/*). Resolved from the
// --auth-token flag or the GENOMEHUB_TOKEN env var (env preferred — flags are
// visible in `ps`). On serve/node it is the token enforced; on client commands
// it is the token sent. Empty means auth disabled.
var authToken string

// bearerTransport attaches the configured token to outbound cmdHTTP requests so
// dash/control/work calls to /actions/* authenticate without per-call wiring.
// It delegates to its wrapped RoundTripper, so the --insecure patch on the
// default transport still applies.
type bearerTransport struct {
	rt    http.RoundTripper
	token string
}

func (b *bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Header.Get("Authorization") == "" {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return b.rt.RoundTrip(r)
}

var rootCmd = &cobra.Command{
	Use:   "genomehub",
	Short: "Content-addressable genomic data protocol",
	PersistentPreRun: func(_ *cobra.Command, _ []string) {
		cmdStart = time.Now()
		if insecureTLS {
			if t, ok := http.DefaultTransport.(*http.Transport); ok {
				if t.TLSClientConfig == nil {
					t.TLSClientConfig = &tls.Config{}
				}
				t.TLSClientConfig.InsecureSkipVerify = true
			}
			fmt.Fprintln(os.Stderr, "warning: --insecure set, TLS certificate verification disabled")
		}
		if authToken == "" {
			authToken = os.Getenv("GENOMEHUB_TOKEN")
		}
		if authToken == "" {
			authToken = loadCLIConfig().AuthToken // persisted fallback
		}
		if authToken != "" {
			cmdHTTP.Transport = &bearerTransport{rt: http.DefaultTransport, token: authToken}
		}
	},
	PersistentPostRun: func(_ *cobra.Command, _ []string) {
		fmt.Fprintf(os.Stderr, "elapsed: %s\n", time.Since(cmdStart).Round(time.Millisecond))
	},
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func indexDir() string {
	return filepath.Join(filepath.Dir(storeDir), "index")
}

func alnCacheDir() string {
	return filepath.Join(filepath.Dir(storeDir), "aligncache")
}

func sketchDir() string {
	return filepath.Join(filepath.Dir(storeDir), "sketches")
}

// eventsPath is the local activity log (import/download history) kept beside the
// store, tailed by a running node into its /status.
func eventsPath() string {
	return filepath.Join(filepath.Dir(storeDir), "events.jsonl")
}

// manifestCacheDir holds manifests fetched by `download`, kept beside the store.
// A node merges these into its catalog so a pure cache peer can name (and show
// coverage for) the genomes whose segments it holds — answering "am I seeding X?"
// even when it was started with an empty catalog.
func manifestCacheDir() string {
	return filepath.Join(filepath.Dir(storeDir), "manifests")
}

func init() {
	home, _ := os.UserHomeDir()
	defaultStore := filepath.Join(home, ".genomehub", "segments")
	rootCmd.PersistentFlags().StringVar(&storeDir, "store", defaultStore, "segment store directory")
	rootCmd.PersistentFlags().BoolVar(&insecureTLS, "insecure", false, "skip TLS certificate verification for outbound requests (self-signed certs only)")
	rootCmd.PersistentFlags().StringVar(&authToken, "auth-token", "", "bearer token for the control plane; enforced by serve/node, sent by clients (env GENOMEHUB_TOKEN preferred)")
}
