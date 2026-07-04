package agentcli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
)

const installerURL = "https://github.com/qiz029/roundtable/releases/latest/download/install.sh"

func runUpdate(ctx context.Context, args []string, opts Options) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(opts.Stderr)
	version := fs.String("version", "", "Install a specific roundtable-agent version")
	installDir := fs.String("install-dir", "", "Install directory")
	dryRun := fs.Bool("dry-run", false, "Print the installer command without running it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: update [--version VERSION] [--install-dir DIR] [--dry-run]")
	}

	command := updateCommand(strings.TrimSpace(*version), strings.TrimSpace(*installDir))
	if *dryRun {
		_, err := fmt.Fprintln(opts.Stdout, command)
		return err
	}

	stdout, stderr, err := opts.Exec(ctx, command, nil)
	if len(stdout) > 0 {
		if _, writeErr := opts.Stdout.Write(stdout); writeErr != nil {
			return writeErr
		}
	}
	if len(stderr) > 0 {
		if _, writeErr := opts.Stderr.Write(stderr); writeErr != nil {
			return writeErr
		}
	}
	if err != nil {
		return fmt.Errorf("update failed: %w", err)
	}
	return nil
}

func updateCommand(version string, installDir string) string {
	env := []string{}
	if version != "" {
		env = append(env, "ROUNDTABLE_AGENT_VERSION="+shellQuote(version))
	}
	if installDir != "" {
		env = append(env, "ROUNDTABLE_INSTALL_DIR="+shellQuote(installDir))
	}
	prefix := ""
	if len(env) > 0 {
		prefix = strings.Join(env, " ") + " "
	}
	return "tmp=\"$(mktemp)\" && curl -fsSL " + installerURL + " -o \"$tmp\" && " + prefix + "bash \"$tmp\"; status=$?; rm -f \"$tmp\"; exit $status"
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
