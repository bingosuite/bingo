package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "bingo",
	Short: "Bingo CLI - A command-line interface for playing Bingo",
	Long:  `Bingo CLI is a command-line application that allows users to play Bingo and manage their games.`,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("Welcome to Bingo CLI! Use the help command to see available options.")
	},
}

// Execute runs the root command.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

// init initializes the root command and its subcommands.
func init() {
	// Here you can add subcommands to the root command
}
