// Command eval prints how the sale detector does on the labelled chats.
//
//	go run ./cmd/eval [path/to/chats.json]
package main

import (
	"fmt"
	"os"

	"github.com/khushisinghal21/salessignal/eval"
)

func main() {
	path := "eval/chats.json"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	set, err := eval.Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	rep, err := eval.Run(set)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(rep)
}
