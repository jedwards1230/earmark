package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jedwards1230/lil-whisper/internal/version"
)

var (
	checkUpdate = flag.Bool("check", false, "check for available updates")
	noCache     = flag.Bool("no-cache", false, "skip cache and force fresh update check")
)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Show version information for lil-whisper.\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	info := version.GetInfo()
	fmt.Print(info.String())

	if *checkUpdate {
		fmt.Println("Checking for updates...")
		
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		result, err := version.CheckForUpdates(ctx, !*noCache)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error checking for updates: %v\n", err)
			os.Exit(1)
		}

		if result.HasUpdate {
			fmt.Printf("\n🎉 Update available!\n")
			fmt.Printf("%s\n", result.UpdateMessage)
			
			if result.UseReleases && result.LatestVersion != "" {
				fmt.Printf("Latest version: %s\n", result.LatestVersion)
			} else if result.LatestCommit != "" {
				fmt.Printf("Latest commit: %s\n", result.LatestCommit[:7])
			}
			
			fmt.Printf("\nRun 'lil-whisper update' to update.\n")
		} else {
			fmt.Printf("\n✅ You are running the latest version.\n")
		}
		
		if !*noCache {
			fmt.Printf("(Last checked: %s)\n", result.CheckedAt.Format("2006-01-02 15:04:05"))
		}
	}
}