package agentcli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type itemsResponse struct {
	Items []map[string]any `json:"items"`
}

func runLoop(ctx context.Context, args []string, opts Options) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(opts.Stderr)
	once := fs.Bool("once", false, "Process one invitation and exit")
	command := fs.String("exec", "", "External command that reads JSON from stdin")
	interval := fs.Duration("interval", 30*time.Second, "Polling interval")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*command) == "" {
		return errors.New("--exec is required")
	}

	for {
		processed, err := runOnce(ctx, opts, strings.TrimSpace(*command))
		if err != nil {
			return err
		}
		if *once {
			if !processed {
				_, _ = fmt.Fprintln(opts.Stdout, "no invitations")
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(*interval):
		}
	}
}

func runOnce(ctx context.Context, opts Options, command string) (bool, error) {
	cfg, err := loadConfig(opts.HomeDir)
	if err != nil {
		return false, err
	}
	invitations, err := apiRequest[itemsResponse](ctx, cfg, http.MethodGet, "/api/v1/agent/invitations", nil)
	if err != nil {
		return false, err
	}
	if len(invitations.Items) == 0 {
		return false, nil
	}

	invitation := invitations.Items[0]
	question, ok := invitation["question"].(map[string]any)
	if !ok {
		return false, errors.New("invitation response did not include question")
	}
	questionID, ok := question["id"].(string)
	if !ok || questionID == "" {
		return false, errors.New("invitation response did not include question id")
	}
	invitationID, _ := invitation["id"].(string)

	payload := map[string]any{
		"invitation": invitation,
		"question":   question,
		"answers_url": fmt.Sprintf(
			"/api/v1/agent/questions/%s/answers",
			questionID,
		),
	}
	stdin, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}
	stdout, stderr, err := opts.Exec(ctx, command, stdin)
	if len(stderr) > 0 {
		_, _ = opts.Stderr.Write(stderr)
	}
	if err != nil {
		return false, err
	}
	body := strings.TrimSpace(string(stdout))
	if body == "" {
		_, _ = fmt.Fprintln(opts.Stdout, "external command produced no answer")
		return true, nil
	}

	answer, err := apiRequest[map[string]any](ctx, cfg, http.MethodPost, "/api/v1/agent/questions/"+questionID+"/answers", map[string]any{
		"invitation_id": invitationID,
		"body":          body,
	})
	if err != nil {
		return false, err
	}
	if id, _ := answer["id"].(string); id != "" {
		_, _ = fmt.Fprintf(opts.Stdout, "submitted answer %s\n", id)
	}
	return true, nil
}
