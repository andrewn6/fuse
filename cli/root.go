package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/folsomintel/fuse/cli/config"
	fuse "github.com/folsomintel/fuse/sdks/go"
)

// version is the cli version, also sent as the user agent. stamped at release
// time via -ldflags "-X main.version=...".
var version = "dev"

// outputTable and outputJSON are the supported --output modes.
const (
	outputTable = "table"
	outputJSON  = "json"
)

// appState holds process-wide cli state resolved from persistent flags and the
// config file. it is populated in the root PersistentPreRunE.
type appState struct {
	cfg        *config.Config
	output     string // table | json
	ctxName    string // --context override (empty = stored current)
	configPath string // --config override (empty = default path)

	// exitCode is the exit status of a guest command run by `environment exec`
	// or `environment shell`. It travels here rather than back through RunE
	// because a guest command that fails is not a CLI error: returning one
	// would make fang render "Error: ..." above output the user already saw.
	exitCode int
}

var app = &appState{}

// client builds an sdk client for the active context.
func (a *appState) client() (*fuse.Client, *config.Context, error) {
	cur, err := a.cfg.Current(a.ctxName)
	if err != nil {
		return nil, nil, err
	}
	cl, err := fuse.New(cur.BaseURL, cur.Token, fuse.WithUserAgent("fuse-cli/"+version))
	if err != nil {
		return nil, nil, err
	}
	return cl, cur, nil
}

// isJSON reports whether output should be machine-readable json.
func (a *appState) isJSON() bool { return a.output == outputJSON }

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:     "fuse",
		Version: version,
		Short:   "Manage Fuse hosts, environments, snapshots, and API keys",
		Long: "fuse is the operator cli for the Fuse orchestrator.\n\n" +
			"Connect to an orchestrator with `fuse connect`, then select a host with\n" +
			"`fuse host <id>` to scope environment and snapshot commands to it.",
		SilenceErrors:     true, // fang renders errors
		SilenceUsage:      true,
		PersistentPreRunE: loadState,
	}

	root.PersistentFlags().StringVarP(&app.output, "output", "o", outputTable,
		"output format: table | json")
	root.PersistentFlags().StringVar(&app.ctxName, "context", "",
		"context to use for this invocation (overrides the active context)")
	root.PersistentFlags().StringVar(&app.configPath, "config", "",
		"path to the config file (default: $XDG_CONFIG_HOME/fuse/config.yaml)")

	root.AddCommand(
		newQuickstartCmd(),
		newConnectCmd(),
		newContextCmd(),
		newHostsCmd(),
		newHostCmd(),
		newEnvironmentCmd(),
		newSnapshotCmd(),
		newAPIKeysCmd(),
		newMetricsCmd(),
		newUpCmd(),
		newLocalCmd(),
		newBuildCmd(),
		newInitCmd(),
		newCompileCmd(),
		newValidateCmd(),
	)
	return root
}

// loadState validates flags and loads the config before any command runs.
func loadState(cmd *cobra.Command, _ []string) error {
	switch app.output {
	case outputTable, outputJSON:
	default:
		return fmt.Errorf("invalid --output %q: want table or json", app.output)
	}
	cfg, err := config.Load(app.configPath)
	if err != nil {
		return err
	}
	app.cfg = cfg
	return nil
}
