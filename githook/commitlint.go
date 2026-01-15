package main

import (
	"fmt"
	"os"
	"regexp"
)

func main() {
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Printf("\033[1;31m✗ Error reading commit message: %v\033[0m\n", err)
		os.Exit(1)
	}
	msg := string(data)

	// regex for the commit msgs
	re := regexp.MustCompile(`^(feat|fix|docs|style|refactor|perf|test|chore)(\([^)]+\))?: .+`)
	if !re.MatchString(msg) {
		fmt.Println("\033[1;31m─────────────────────────────────────────────────────────────\033[0m")
		fmt.Println("\033[1;31m✗ Commit message does not follow Conventional Commits format.\033[0m")
		fmt.Println("\033[1;31m─────────────────────────────────────────────────────────────\033[0m")
		fmt.Println("\033[1;33mFormat:\033[0m <type>(<scope>): <description>")
		fmt.Println("\033[1;33mOptions for <type>:\033[0m")
		fmt.Println("  feat      - a new feature")
		fmt.Println("  fix       - a bug fix")
		fmt.Println("  docs      - documentation only changes")
		fmt.Println("  style     - changes that do not affect meaning (whitespace, formatting)")
		fmt.Println("  refactor  - code change that neither fixes a bug nor adds a feature")
		fmt.Println("  perf      - performance improvement")
		fmt.Println("  test      - adding or correcting tests")
		fmt.Println("  chore     - other changes that don't modify src or test files")
		fmt.Println("\033[1;33mExamples:\033[0m")
		fmt.Println("  feat(parser): add ability to parse arrays")
		fmt.Println("  fix(auth): handle expired tokens")
		fmt.Println("  docs: update README with usage examples")
		os.Exit(1)
	}
	fmt.Println("\033[1;32m✓ Commit message format looks good!\033[0m")
}
