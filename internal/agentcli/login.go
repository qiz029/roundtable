package agentcli

import (
	"errors"
	"flag"
	"fmt"
	"strings"
)

func runLogin(args []string, opts Options) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(opts.Stderr)
	apiURL := fs.String("api-url", "", "Roundtable API URL")
	token := fs.String("token", "", "Agent token")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*apiURL) == "" {
		return errors.New("--api-url is required")
	}
	if strings.TrimSpace(*token) == "" {
		return errors.New("--token is required")
	}
	cfg := config{
		APIURL: strings.TrimRight(strings.TrimSpace(*apiURL), "/"),
		Token:  strings.TrimSpace(*token),
	}
	if err := saveConfig(opts.HomeDir, cfg); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(opts.Stdout, "agent profile saved")
	return nil
}
