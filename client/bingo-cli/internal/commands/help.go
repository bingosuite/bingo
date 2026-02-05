package commands

import (
	"fmt"

	"github.com/spf13/cobra"
)

// HelpCommand represents the help command
var HelpCommand = &cobra.Command{
	Use:   "help",
	Short: "Display help information about available commands",
	Long:  `The help command provides information about the available commands and their usage in the bingo CLI application.`,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("Available commands:")
		fmt.Println("  help       Display help information about available commands")
		// Add other commands here as needed
	},
}
