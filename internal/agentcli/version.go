package agentcli

import "fmt"

func runVersion(opts Options) error {
	_, err := fmt.Fprintf(
		opts.Stdout,
		"roundtable-agent version %s commit %s built %s\n",
		opts.Version.Version,
		opts.Version.Commit,
		opts.Version.Date,
	)
	return err
}
