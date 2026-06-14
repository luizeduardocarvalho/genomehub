package cmd

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/luizeduardocarvalho/genomehub/internal/events"
	"github.com/luizeduardocarvalho/genomehub/internal/manifest"
	"github.com/luizeduardocarvalho/genomehub/internal/store"
	"github.com/spf13/cobra"
)

var (
	publishManifest string
	publishServer   string
	publishParallel int
)

var publishCmd = &cobra.Command{
	Use:   "publish",
	Short: "Push a locally-imported genome (segments + manifest) to a remote origin",
	Long: `Uploads the segments referenced by a local manifest to a remote origin, then
publishes the manifest itself. Only segments the origin is missing are sent
(content-addressed dedup, in reverse), so re-publishing or publishing a genome
that shares sequence with one already there transfers little.

The origin re-hashes every uploaded segment (your push is untrusted) and accepts
the manifest only once all its segments are present. If the origin enforces a
token, set --auth-token or GENOMEHUB_TOKEN.

Example:
  genomehub import --fasta my.fa --organism "Glycine max" --assembly MySoy --output MySoy.manifest.json
  genomehub publish --manifest MySoy.manifest.json --server https://origin:8443`,
	RunE: runPublish,
}

func init() {
	publishCmd.Flags().StringVar(&publishManifest, "manifest", "", "manifest JSON of the genome to publish (required)")
	publishCmd.Flags().StringVar(&publishServer, "server", "", "origin base URL, e.g. https://origin:8443 (required)")
	publishCmd.Flags().IntVar(&publishParallel, "parallel", 8, "number of segments to upload concurrently")
	publishCmd.MarkFlagRequired("manifest")
	publishCmd.MarkFlagRequired("server")
	rootCmd.AddCommand(publishCmd)
}

func runPublish(_ *cobra.Command, _ []string) error {
	raw, err := os.ReadFile(publishManifest)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	m, err := manifest.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	if m.Assembly == "" {
		return fmt.Errorf("manifest has empty assembly")
	}
	base := strings.TrimRight(publishServer, "/")

	s, err := store.Open(storeDir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer s.Close()

	// Unique segment hashes referenced by the manifest, in first-seen order.
	seen := map[string]struct{}{}
	var hashes []string
	for _, c := range m.Chromosomes {
		for _, seg := range c.Segments {
			h := strings.TrimPrefix(seg.Hash, "blake3:")
			if _, ok := seen[h]; ok {
				continue
			}
			seen[h] = struct{}{}
			hashes = append(hashes, h)
		}
	}

	fmt.Fprintf(os.Stderr, "publishing %s (%d unique segments) to %s\n", m.Assembly, len(hashes), base)

	var uploaded, present, sentBytes atomic.Int64
	var firstErr error
	var errMu sync.Mutex
	fail := func(e error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = e
		}
		errMu.Unlock()
	}

	sem := make(chan struct{}, publishParallel)
	var wg sync.WaitGroup
	for _, h := range hashes {
		errMu.Lock()
		stop := firstErr != nil
		errMu.Unlock()
		if stop {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(h string) {
			defer wg.Done()
			defer func() { <-sem }()
			switch has, err := originHasSegment(base, h); {
			case err != nil:
				fail(fmt.Errorf("probe %s: %w", h, err))
				return
			case has:
				present.Add(1)
				return
			}
			data, err := s.Get(h)
			if err != nil {
				fail(fmt.Errorf("segment %s not in local store: %w", h, err))
				return
			}
			if err := uploadSegment(base, h, data); err != nil {
				fail(fmt.Errorf("upload %s: %w", h, err))
				return
			}
			uploaded.Add(1)
			sentBytes.Add(int64(len(data)))
		}(h)
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}

	// All segments present on the origin — now publish the manifest.
	if err := uploadManifest(base, m.Assembly, raw); err != nil {
		return fmt.Errorf("publish manifest: %w", err)
	}

	fmt.Fprintf(os.Stderr, "published %s: %d segments uploaded (%s), %d already present\n",
		m.Assembly, uploaded.Load(), fmtBytes(sentBytes.Load()), present.Load())
	events.Append(eventsPath(), events.Event{
		Op:       events.Publish,
		Assembly: m.Assembly,
		Bytes:    sentBytes.Load(),
		Segments: int(uploaded.Load()),
		Note:     "to " + base,
	})
	return nil
}

// originHasSegment probes HEAD /segments/{hash}; 200 = present, 404 = missing.
func originHasSegment(base, hash string) (bool, error) {
	req, err := http.NewRequest(http.MethodHead, base+"/segments/"+hash, nil)
	if err != nil {
		return false, err
	}
	resp, err := cmdHTTP.Do(req)
	if err != nil {
		return false, err
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("status %d", resp.StatusCode)
	}
}

func uploadSegment(base, hash string, data []byte) error {
	resp, err := cmdHTTP.Post(base+"/segments/"+hash, "application/octet-stream", bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return statusError(resp)
	}
	return nil
}

func uploadManifest(base, assembly string, raw []byte) error {
	resp, err := cmdHTTP.Post(base+"/genomes/"+assembly+"/manifest", "application/json", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return statusError(resp)
	}
	return nil
}

// statusError turns a non-2xx response into an error carrying the server's body
// (e.g. "401 unauthorized", "422 N referenced segments missing").
func statusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return fmt.Errorf("status %d: %s", resp.StatusCode, msg)
}
