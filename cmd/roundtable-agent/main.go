package main

import (
	"context"
	"fmt"
	"os"

	"github.com/qiz029/roundtable/internal/agentcli"
)

func main() {
	if err := agentcli.Run(context.Background(), os.Args[1:], agentcli.Options{}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
