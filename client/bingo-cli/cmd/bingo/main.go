package main

import (
	"fmt"
	"os"
	"time"

	"github.com/briandowns/spinner"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "bingo",
	Short: "Bingo CLI - Go Concurrency Debugger",
	Long:  `Bingo CLI is a command-line interface for debugging Go concurrency issues with real-time visualization.`,
	Run: func(cmd *cobra.Command, args []string) {
		s := spinner.New(spinner.CharSets[14], 100*time.Millisecond)
		s.Suffix = " Waiting for Bingo server connection..."
		s.Start()

		// TODO: replace with actual connection logic
		time.Sleep(3 * time.Second)

		s.Stop()
		fmt.Println("✓ Connected successfully!")
	},
}

var helpCmd = &cobra.Command{
	Use:   "help",
	Short: "Display help information",
	Run: func(cmd *cobra.Command, args []string) {
		rootCmd.Help()
	},
}

func init() {
	rootCmd.AddCommand(helpCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
