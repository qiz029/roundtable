package main

import (
	"context"
	"fmt"
	"os"

	"github.com/qiz029/roundtable/internal/agentcli"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	if err := agentcli.Run(context.Background(), os.Args[1:], agentcli.Options{
		Version: agentcli.VersionInfo{
			Version: version,
			Commit:  commit,
			Date:    date,
		},
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
