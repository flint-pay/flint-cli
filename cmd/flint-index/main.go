// flint-index generates the private startup index from the reviewed snapshot.
package main

import (
	"github.com/flint-pay/flint-cli/internal/cli"
	"log"
)

func main() {
	if err := cli.GenerateStartupIndex("../spec/startup.json"); err != nil {
		log.Fatal(err)
	}
}
