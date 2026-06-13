package cmd

import (
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "earmark",
	Short: "earmark: audiobook knowledge layer — chunk, embed, and search transcripts",
}

func GetRootCmd() *cobra.Command {
	return rootCmd
}
