package agentcli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"strings"
)

func runProxyList(ctx context.Context, args []string, opts Options, name string, path string) error {
	if len(args) == 1 && args[0] == "list" {
		cfg, err := loadConfig(opts.HomeDir)
		if err != nil {
			return err
		}
		resp, err := apiRequest[map[string]any](ctx, cfg, http.MethodGet, path, nil)
		if err != nil {
			return err
		}
		return writePrettyJSON(opts.Stdout, resp)
	}
	if name == "questions" && len(args) == 2 && args[0] == "show" {
		cfg, err := loadConfig(opts.HomeDir)
		if err != nil {
			return err
		}
		resp, err := apiRequest[map[string]any](ctx, cfg, http.MethodGet, "/api/v1/agent/questions/"+args[1], nil)
		if err != nil {
			return err
		}
		return writePrettyJSON(opts.Stdout, resp)
	}
	return fmt.Errorf("usage: %s list", name)
}

func runAnswers(ctx context.Context, args []string, opts Options) error {
	if len(args) == 0 {
		return errors.New("answers subcommand is required")
	}
	switch args[0] {
	case "list":
		return runAnswerList(ctx, args[1:], opts)
	case "submit":
		return runAnswerSubmit(ctx, args[1:], opts)
	case "like":
		return runAnswerLike(ctx, args[1:], opts, http.MethodPost)
	case "unlike":
		return runAnswerLike(ctx, args[1:], opts, http.MethodDelete)
	default:
		return fmt.Errorf("unknown answers command %q", args[0])
	}
}

func runAnswerList(ctx context.Context, args []string, opts Options) error {
	fs := flag.NewFlagSet("answers list", flag.ContinueOnError)
	fs.SetOutput(opts.Stderr)
	questionID := fs.String("question", "", "Question ID")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*questionID) == "" {
		return errors.New("--question is required")
	}
	cfg, err := loadConfig(opts.HomeDir)
	if err != nil {
		return err
	}
	resp, err := apiRequest[map[string]any](ctx, cfg, http.MethodGet, "/api/v1/agent/questions/"+strings.TrimSpace(*questionID)+"/answers", nil)
	if err != nil {
		return err
	}
	return writePrettyJSON(opts.Stdout, resp)
}

func runAnswerSubmit(ctx context.Context, args []string, opts Options) error {
	fs := flag.NewFlagSet("answers submit", flag.ContinueOnError)
	fs.SetOutput(opts.Stderr)
	questionID := fs.String("question", "", "Question ID")
	invitationID := fs.String("invitation", "", "Invitation ID")
	body := fs.String("body", "", "Answer body")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*questionID) == "" {
		return errors.New("--question is required")
	}
	if strings.TrimSpace(*body) == "" {
		return errors.New("--body is required")
	}
	cfg, err := loadConfig(opts.HomeDir)
	if err != nil {
		return err
	}
	resp, err := apiRequest[map[string]any](ctx, cfg, http.MethodPost, "/api/v1/agent/questions/"+strings.TrimSpace(*questionID)+"/answers", map[string]any{
		"invitation_id": strings.TrimSpace(*invitationID),
		"body":          strings.TrimSpace(*body),
	})
	if err != nil {
		return err
	}
	return writePrettyJSON(opts.Stdout, resp)
}

func runAnswerLike(ctx context.Context, args []string, opts Options, method string) error {
	if len(args) != 1 {
		return errors.New("answer id is required")
	}
	cfg, err := loadConfig(opts.HomeDir)
	if err != nil {
		return err
	}
	resp, err := apiRequest[map[string]any](ctx, cfg, method, "/api/v1/agent/answers/"+args[0]+"/like", nil)
	if err != nil {
		return err
	}
	return writePrettyJSON(opts.Stdout, resp)
}
