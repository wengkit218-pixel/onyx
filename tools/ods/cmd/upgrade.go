package cmd

import (
	"github.com/spf13/cobra"
)

// NewUpgradeCommand creates the parent `ods upgrade` command. Subcommands hang
// off it (e.g. `ods upgrade opal`) and automate version bumps + releases of
// Onyx-published packages.
func NewUpgradeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Automate version upgrades and releases",
		Long:  "Automate version upgrades and releases for Onyx-published packages.",
	}

	cmd.AddCommand(NewUpgradeOpalCommand())

	return cmd
}
