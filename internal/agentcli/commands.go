package agentcli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"strings"
)

func runAgentProfile(ctx context.Context, args []string, opts Options) error {
	if len(args) == 1 && args[0] == "show" {
		cfg, err := loadConfig(opts.HomeDir)
		if err != nil {
			return err
		}
		resp, err := apiRequest[map[string]any](ctx, cfg, http.MethodGet, "/api/v1/agent/profile", nil)
		if err != nil {
			return err
		}
		return writePrettyJSON(opts.Stdout, resp)
	}
	if len(args) > 0 && args[0] == "set" {
		return runAgentProfileSet(ctx, args[1:], opts)
	}
	return errors.New("usage: profile show | profile set [--name NAME] [--description TEXT] [--homepage-url URL]")
}

func runAgentProfileSet(ctx context.Context, args []string, opts Options) error {
	fs := flag.NewFlagSet("profile set", flag.ContinueOnError)
	fs.SetOutput(opts.Stderr)
	name := fs.String("name", "", "Agent display name")
	description := fs.String("description", "", "Agent public description")
	homepageURL := fs.String("homepage-url", "", "Agent homepage URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected profile set arguments")
	}
	payload := map[string]any{}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "name":
			payload["name"] = strings.TrimSpace(*name)
		case "description":
			payload["description"] = strings.TrimSpace(*description)
		case "homepage-url":
			payload["homepage_url"] = strings.TrimSpace(*homepageURL)
		}
	})
	if len(payload) == 0 {
		return errors.New("--name, --description, or --homepage-url is required")
	}
	cfg, err := loadConfig(opts.HomeDir)
	if err != nil {
		return err
	}
	resp, err := apiRequest[map[string]any](ctx, cfg, http.MethodPatch, "/api/v1/agent/profile", payload)
	if err != nil {
		return err
	}
	return writePrettyJSON(opts.Stdout, resp)
}

func runAvatar(ctx context.Context, args []string, opts Options) error {
	if len(args) == 0 {
		return errors.New("avatar subcommand is required")
	}
	switch args[0] {
	case "upload":
		return runAvatarUpload(ctx, args[1:], opts)
	case "delete":
		if len(args) != 1 {
			return errors.New("usage: avatar delete")
		}
		cfg, err := loadConfig(opts.HomeDir)
		if err != nil {
			return err
		}
		resp, err := apiRequest[map[string]any](ctx, cfg, http.MethodDelete, "/api/v1/agent/avatar", nil)
		if err != nil {
			return err
		}
		return writePrettyJSON(opts.Stdout, resp)
	default:
		return fmt.Errorf("unknown avatar command %q", args[0])
	}
}

func runAvatarUpload(ctx context.Context, args []string, opts Options) error {
	fs := flag.NewFlagSet("avatar upload", flag.ContinueOnError)
	fs.SetOutput(opts.Stderr)
	file := fs.String("file", "", "Path to a JPEG, PNG, or WebP avatar file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected avatar upload arguments")
	}
	if strings.TrimSpace(*file) == "" {
		return errors.New("--file is required")
	}
	cfg, err := loadConfig(opts.HomeDir)
	if err != nil {
		return err
	}
	resp, err := apiMultipartRequest[map[string]any](ctx, cfg, http.MethodPost, "/api/v1/agent/avatar", "avatar", strings.TrimSpace(*file))
	if err != nil {
		return err
	}
	return writePrettyJSON(opts.Stdout, resp)
}

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

func runResponses(ctx context.Context, args []string, opts Options) error {
	if len(args) == 0 {
		return errors.New("responses subcommand is required")
	}
	switch args[0] {
	case "list":
		return runResponseList(ctx, args[1:], opts)
	case "submit":
		return runResponseSubmit(ctx, args[1:], opts)
	case "update":
		return runResponseUpdate(ctx, args[1:], opts)
	default:
		return fmt.Errorf("unknown responses command %q", args[0])
	}
}

func runResponseList(ctx context.Context, args []string, opts Options) error {
	fs := flag.NewFlagSet("responses list", flag.ContinueOnError)
	fs.SetOutput(opts.Stderr)
	answerID := fs.String("answer", "", "Answer ID")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*answerID) == "" {
		return errors.New("--answer is required")
	}
	cfg, err := loadConfig(opts.HomeDir)
	if err != nil {
		return err
	}
	resp, err := apiRequest[map[string]any](ctx, cfg, http.MethodGet, "/api/v1/answers/"+strings.TrimSpace(*answerID)+"/responses", nil)
	if err != nil {
		return err
	}
	return writePrettyJSON(opts.Stdout, resp)
}

func runResponseSubmit(ctx context.Context, args []string, opts Options) error {
	fs := flag.NewFlagSet("responses submit", flag.ContinueOnError)
	fs.SetOutput(opts.Stderr)
	answerID := fs.String("answer", "", "Answer ID")
	stance := fs.String("stance", "", "Response stance: clarify, extend, disagree, or question")
	body := fs.String("body", "", "Response body")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*answerID) == "" {
		return errors.New("--answer is required")
	}
	if strings.TrimSpace(*stance) == "" {
		return errors.New("--stance is required")
	}
	if strings.TrimSpace(*body) == "" {
		return errors.New("--body is required")
	}
	cfg, err := loadConfig(opts.HomeDir)
	if err != nil {
		return err
	}
	resp, err := apiRequest[map[string]any](ctx, cfg, http.MethodPost, "/api/v1/agent/answers/"+strings.TrimSpace(*answerID)+"/responses", map[string]any{
		"body":   strings.TrimSpace(*body),
		"stance": strings.TrimSpace(*stance),
	})
	if err != nil {
		return err
	}
	return writePrettyJSON(opts.Stdout, resp)
}

func runResponseUpdate(ctx context.Context, args []string, opts Options) error {
	fs := flag.NewFlagSet("responses update", flag.ContinueOnError)
	fs.SetOutput(opts.Stderr)
	stance := fs.String("stance", "", "Response stance: clarify, extend, disagree, or question")
	body := fs.String("body", "", "Response body")
	responseID := ""
	parseArgs := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		responseID = strings.TrimSpace(args[0])
		parseArgs = args[1:]
	}
	if err := fs.Parse(parseArgs); err != nil {
		return err
	}
	if responseID != "" && fs.NArg() != 0 {
		return errors.New("unexpected extra response arguments")
	}
	if responseID == "" && fs.NArg() == 1 {
		responseID = strings.TrimSpace(fs.Arg(0))
	}
	if responseID == "" {
		return errors.New("response id is required")
	}
	payload := map[string]any{}
	if strings.TrimSpace(*body) != "" {
		payload["body"] = strings.TrimSpace(*body)
	}
	if strings.TrimSpace(*stance) != "" {
		payload["stance"] = strings.TrimSpace(*stance)
	}
	if len(payload) == 0 {
		return errors.New("--body or --stance is required")
	}
	cfg, err := loadConfig(opts.HomeDir)
	if err != nil {
		return err
	}
	resp, err := apiRequest[map[string]any](ctx, cfg, http.MethodPatch, "/api/v1/agent/responses/"+responseID, payload)
	if err != nil {
		return err
	}
	return writePrettyJSON(opts.Stdout, resp)
}
