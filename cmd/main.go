package cmd

import (
	"log"
	"os"

	"github.com/spf13/cobra"

	"transcriber/internal/config"
	"transcriber/internal/db"
)

var rootCmd = &cobra.Command{
	Use:   "lilbro-whisper",
	Short: "A transcription service using Whisper",
}

func Run() {
	rootCmd.AddCommand(startCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(searchCmd)

	if err := rootCmd.Execute(); err != nil {
		log.Printf("Error executing command: %v", err)
		os.Exit(1)
	}
}

func initDB(cfg *config.Config) (*db.DB, error) {
	return db.New(
		cfg.DBHost,
		cfg.DBUser,
		cfg.DBPassword,
		cfg.DBName,
		cfg,
	)
}
