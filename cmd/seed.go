package cmd

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/luizeduardocarvalho/genomehub/internal/sign"
	"github.com/spf13/cobra"
)

// Reference network defaults so `genomehub seed` works with no flags. Override
// for a different deployment.
const (
	defaultTracker  = "https://genomehub.duckdns.org:9000"
	defaultRegistry = "https://genomehub.duckdns.org:8443"
)

var (
	seedTracker   string
	seedRegistry  string
	seedAddr      string
	seedAdvertise string
	seedDir       string
	seedTunnel    bool
)

var seedCmd = &cobra.Command{
	Use:   "seed",
	Short: "Become a seeder in one command (auto-setup + run a node)",
	Long: `Turns this machine into a seeder with a single command: it creates its data
directory and identity key if missing, then runs a node that announces to the
tracker and serves whatever segments it holds.

Reachability: peers fetch over HTTP, so a seeder must be reachable.
  - Public host (lab server/VPS): pass --advertise https://your-host:PORT
  - Behind NAT (laptop): pass --tunnel to auto-create a Cloudflare quick tunnel
    (requires 'cloudflared' on PATH) so you get a public URL automatically.

Without --advertise or --tunnel you still download/cache, but won't serve others.

Examples:
  genomehub seed --tunnel                          # laptop, auto public URL
  genomehub seed --advertise https://lab:8443      # public server`,
	RunE: runSeed,
}

func init() {
	home, _ := os.UserHomeDir()
	seedCmd.Flags().StringVar(&seedTracker, "tracker", defaultTracker, "tracker URL")
	seedCmd.Flags().StringVar(&seedRegistry, "registry", defaultRegistry, "origin/registry URL (for discovery)")
	seedCmd.Flags().StringVar(&seedAddr, "addr", ":8080", "local listen address")
	seedCmd.Flags().StringVar(&seedAdvertise, "advertise", "", "public URL peers use to reach you (omit when using --tunnel)")
	seedCmd.Flags().StringVar(&seedDir, "dir", filepath.Join(home, ".genomehub"), "data directory (store + catalog + identity)")
	seedCmd.Flags().BoolVar(&seedTunnel, "tunnel", false, "auto-create a Cloudflare quick tunnel for public reachability (needs cloudflared)")
	rootCmd.AddCommand(seedCmd)
}

func runSeed(cmd *cobra.Command, args []string) error {
	store := filepath.Join(seedDir, "store")
	catalog := filepath.Join(seedDir, "catalog")
	for _, d := range []string{store, catalog} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
	}

	// Identity: generate once, reuse thereafter (stable node id).
	idPath := filepath.Join(seedDir, "id.key")
	if _, err := os.Stat(idPath); os.IsNotExist(err) {
		s, err := sign.Generate()
		if err != nil {
			return err
		}
		if err := os.WriteFile(idPath, []byte(s.PrivateHex()+"\n"), 0o600); err != nil {
			return err
		}
		_ = os.WriteFile(filepath.Join(seedDir, "id.pub"), []byte(s.PublicHex()+"\n"), 0o644)
		fmt.Fprintf(os.Stderr, "generated identity: %s\n", idPath)
	}

	advertise := seedAdvertise
	if seedTunnel {
		url, stop, err := startQuickTunnel(seedAddr)
		if err != nil {
			return err
		}
		defer stop()
		advertise = url
		fmt.Fprintf(os.Stderr, "tunnel: serving publicly at %s\n", url)
	}
	if advertise == "" {
		fmt.Fprintln(os.Stderr, "warning: no --advertise and no --tunnel — you'll cache genomes but can't serve peers. Use --tunnel (laptop) or --advertise https://your-host (public server).")
	}

	// A tunneled node is internet-reachable, so lock its control plane: without a
	// token, anyone could hit /actions/* through the tunnel. Reads stay open.
	if authToken == "" {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		authToken = hex.EncodeToString(b)
	}

	// Drive the existing node runner via its globals.
	storeDir = store
	nodeTracker = seedTracker
	nodeRegistry = seedRegistry
	nodeAddr = seedAddr
	nodeAdvertise = advertise
	nodeCatalog = catalog
	nodeIdentity = idPath
	return runNode(cmd, args)
}

// startQuickTunnel launches a Cloudflare quick tunnel pointing at the local node
// and returns the public https URL once cloudflared prints it. The returned stop
// function tears the tunnel down.
func startQuickTunnel(addr string) (string, func(), error) {
	if _, err := exec.LookPath("cloudflared"); err != nil {
		return "", nil, fmt.Errorf("cloudflared not found in PATH; install it (https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/) or use --advertise with a public host")
	}
	local := addr
	if strings.HasPrefix(local, ":") {
		local = "127.0.0.1" + local
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := exec.CommandContext(ctx, "cloudflared", "tunnel", "--url", "http://"+local, "--no-autoupdate")
	stderr, err := c.StderrPipe()
	if err != nil {
		cancel()
		return "", nil, err
	}
	if err := c.Start(); err != nil {
		cancel()
		return "", nil, err
	}

	re := regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)
	urlCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		sent := false
		for sc.Scan() { // keep draining so cloudflared doesn't block on a full pipe
			if !sent {
				if m := re.FindString(sc.Text()); m != "" {
					urlCh <- m
					sent = true
				}
			}
		}
	}()

	select {
	case u := <-urlCh:
		return u, func() { cancel() }, nil
	case <-time.After(30 * time.Second):
		cancel()
		return "", nil, fmt.Errorf("timed out waiting for the cloudflared tunnel URL")
	}
}
