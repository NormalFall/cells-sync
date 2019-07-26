// +build !app

package cmd

import (
	"os"

	"github.com/spf13/cobra"
)

var RootCmd = &cobra.Command{
	Use:   os.Args[0],
	Short: "Cells Sync desktop client",
	Long:  `Cells Sync Desktop Client`,
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		handleSignals()
	},
	Run: func(cmd *cobra.Command, args []string) {
		cmd.Usage()
	},
}
