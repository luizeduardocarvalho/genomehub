package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// cliConfig is the persisted user config (~/.genomehub/config.json). It supplies
// fallbacks for values otherwise passed by flag/env — today the publish token,
// extensible to server/tracker/verify-key later.
type cliConfig struct {
	AuthToken string `json:"auth_token,omitempty"`
}

func configPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".genomehub", "config.json")
}

func loadCLIConfig() cliConfig {
	var c cliConfig
	data, err := os.ReadFile(configPath())
	if err != nil {
		return c // missing/unreadable config is just empty
	}
	_ = json.Unmarshal(data, &c)
	return c
}

func saveCLIConfig(c cliConfig) error {
	p := configPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o600) // contains a secret → owner-only
}

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Get/set persisted settings (e.g. the publish token)",
	Long: `Stores settings in ~/.genomehub/config.json so you don't repeat flags.

  genomehub config set auth-token <TOKEN>   # save the publish token (owner-only file)
  genomehub config get auth-token
  genomehub config path

Resolution order for the token: --auth-token flag, then GENOMEHUB_TOKEN env,
then this config file.`,
}

var configSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set a config value (keys: auth-token)",
	Args:  cobra.ExactArgs(2),
	RunE: func(_ *cobra.Command, args []string) error {
		c := loadCLIConfig()
		switch args[0] {
		case "auth-token":
			c.AuthToken = args[1]
		default:
			return fmt.Errorf("unknown key %q (supported: auth-token)", args[0])
		}
		if err := saveCLIConfig(c); err != nil {
			return err
		}
		fmt.Printf("saved %s to %s\n", args[0], configPath())
		return nil
	},
}

var configGetCmd = &cobra.Command{
	Use:   "get [key]",
	Short: "Print a config value (or all)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		c := loadCLIConfig()
		masked := c.AuthToken
		if masked != "" {
			masked = masked[:min(6, len(masked))] + "…" // don't echo the full secret
		}
		if len(args) == 0 {
			fmt.Printf("auth-token: %s\n", masked)
			return nil
		}
		switch args[0] {
		case "auth-token":
			fmt.Println(masked)
		default:
			return fmt.Errorf("unknown key %q", args[0])
		}
		return nil
	},
}

var configPathCmd = &cobra.Command{
	Use:   "path",
	Short: "Print the config file path",
	Run:   func(_ *cobra.Command, _ []string) { fmt.Println(configPath()) },
}

func init() {
	configCmd.AddCommand(configSetCmd, configGetCmd, configPathCmd)
	rootCmd.AddCommand(configCmd)
}
