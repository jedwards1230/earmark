package cmd

import (
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "lil-whisper",
	Short: "Audiobook transcription service: chunk, embed, and search transcripts",
}

func GetRootCmd() *cobra.Command {
	return rootCmd
}
