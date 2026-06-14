package cmd

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

const updateRepo = "luizeduardocarvalho/genomehub"

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update genomehub to the latest release",
	Long: `Checks the latest GitHub release and, if newer than the running binary,
downloads the build for this OS/arch and replaces the current executable
in place. Re-run with sudo if the binary lives in a root-owned directory.`,
	RunE: runUpdate,
}

func init() {
	rootCmd.AddCommand(updateCmd)
}

func runUpdate(_ *cobra.Command, _ []string) error {
	// Latest release tag.
	var rel struct {
		TagName string `json:"tag_name"`
	}
	resp, err := http.Get("https://api.github.com/repos/" + updateRepo + "/releases/latest")
	if err != nil {
		return fmt.Errorf("check latest release: %w", err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return fmt.Errorf("decode release: %w", err)
	}
	if rel.TagName == "" {
		return fmt.Errorf("could not determine the latest release")
	}
	latest := strings.TrimPrefix(rel.TagName, "v")

	cur := strings.TrimPrefix(version, "v")
	if cur == latest {
		fmt.Printf("already up to date (v%s)\n", cur)
		return nil
	}
	fmt.Printf("updating %s → v%s ...\n", version, latest)

	// Build the asset URL for this platform.
	ext := "tar.gz"
	if runtime.GOOS == "windows" {
		ext = "zip"
	}
	asset := fmt.Sprintf("genomehub_%s_%s_%s.%s", latest, runtime.GOOS, runtime.GOARCH, ext)
	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", updateRepo, rel.TagName, asset)

	bin, err := downloadBinary(url, ext)
	if err != nil {
		return err
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.EvalSymlinks(exe)

	if err := replaceExecutable(exe, bin); err != nil {
		if os.IsPermission(err) {
			return fmt.Errorf("permission denied writing %s — re-run with sudo, or reinstall via the install script", exe)
		}
		return err
	}
	fmt.Printf("updated to v%s at %s\n", latest, exe)
	return nil
}

// downloadBinary fetches the release archive and returns the genomehub binary
// bytes from inside it (tar.gz on unix, zip on windows).
func downloadBinary(url, ext string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: status %d", url, resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	name := "genomehub"
	if runtime.GOOS == "windows" {
		name = "genomehub.exe"
	}

	if ext == "zip" {
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if filepath.Base(f.Name) == name {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return io.ReadAll(rc)
			}
		}
		return nil, fmt.Errorf("%s not found in archive", name)
	}

	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if filepath.Base(h.Name) == name {
			return io.ReadAll(tr)
		}
	}
	return nil, fmt.Errorf("%s not found in archive", name)
}

// replaceExecutable swaps the running binary for new bytes. On Unix a rename
// over the path is atomic; on Windows the running .exe can't be overwritten, so
// it is moved aside first.
func replaceExecutable(exe string, newBin []byte) error {
	dir := filepath.Dir(exe)
	tmp := filepath.Join(dir, ".genomehub.new")
	if err := os.WriteFile(tmp, newBin, 0o755); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		old := exe + ".old"
		os.Remove(old)
		if err := os.Rename(exe, old); err != nil {
			os.Remove(tmp)
			return err
		}
		if err := os.Rename(tmp, exe); err != nil {
			os.Rename(old, exe) // roll back
			return err
		}
		return nil
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
