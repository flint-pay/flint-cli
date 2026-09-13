package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/flint-pay/flint-cli/internal/contract"
)

func main() {
	entries, err := contract.Generate()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(entries); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
