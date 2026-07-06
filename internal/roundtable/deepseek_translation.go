package roundtable

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultDeepSeekTranslationModel = "deepseek-v4-flash"
const defaultDeepSeekAPIBaseURL = "https://api.deepseek.com"
const defaultDeepSeekInputCostMicrosPerMillion = 140000
const defaultDeepSeekOutputCostMicrosPerMillion = 280000

type DeepSeekTranslationProviderOptions struct {
	APIKey                     string
	APIBaseURL                 string
	Model                      string
	InputCostMicrosPerMillion  int
	OutputCostMicrosPerMillion int
	HTTPClient                 *http.Client
}

type DeepSeekTranslationProvider struct {
	apiKey                     string
	apiBaseURL                 string
	model                      string
	inputCostMicrosPerMillion  int
	outputCostMicrosPerMillion int
	client                     *http.Client
}

func NewDeepSeekTranslationProvider(opts DeepSeekTranslationProviderOptions) (*DeepSeekTranslationProvider, error) {
	apiKey := strings.TrimSpace(opts.APIKey)
	if apiKey == "" {
		return nil, errors.New("deepseek api key is required")
	}
	apiBaseURL := strings.TrimRight(strings.TrimSpace(opts.APIBaseURL), "/")
	if apiBaseURL == "" {
		apiBaseURL = defaultDeepSeekAPIBaseURL
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = defaultDeepSeekTranslationModel
	}
	inputCostMicrosPerMillion := opts.InputCostMicrosPerMillion
	if inputCostMicrosPerMillion <= 0 {
		inputCostMicrosPerMillion = defaultDeepSeekInputCostMicrosPerMillion
	}
	outputCostMicrosPerMillion := opts.OutputCostMicrosPerMillion
	if outputCostMicrosPerMillion <= 0 {
		outputCostMicrosPerMillion = defaultDeepSeekOutputCostMicrosPerMillion
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &DeepSeekTranslationProvider{
		apiKey:                     apiKey,
		apiBaseURL:                 apiBaseURL,
		model:                      model,
		inputCostMicrosPerMillion:  inputCostMicrosPerMillion,
		outputCostMicrosPerMillion: outputCostMicrosPerMillion,
		client:                     client,
	}, nil
}

func (p *DeepSeekTranslationProvider) Translate(ctx context.Context, req TranslationProviderRequest) (TranslationProviderResult, error) {
	if p == nil {
		return TranslationProviderResult{}, errors.New("deepseek translation provider is not configured")
	}
	payload := p.chatCompletionPayload(req)
	body, err := json.Marshal(payload)
	if err != nil {
		return TranslationProviderResult{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiBaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return TranslationProviderResult{}, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return TranslationProviderResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		return TranslationProviderResult{}, fmt.Errorf("deepseek translation request failed with status %d", resp.StatusCode)
	}

	var decoded deepSeekChatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return TranslationProviderResult{}, err
	}
	if len(decoded.Choices) == 0 {
		return TranslationProviderResult{}, errors.New("deepseek translation response had no choices")
	}
	translation, err := decodeDeepSeekTranslationContent(decoded.Choices[0].Message.Content)
	if err != nil {
		return TranslationProviderResult{}, err
	}
	model := strings.TrimSpace(decoded.Model)
	if model == "" {
		model = p.model
	}
	return TranslationProviderResult{
		Title:        translation.Title,
		Body:         translation.Body,
		Provider:     "deepseek",
		Model:        model,
		InputTokens:  decoded.Usage.PromptTokens,
		OutputTokens: decoded.Usage.CompletionTokens,
		CostMicros: calculateTranslationCostMicros(
			decoded.Usage.PromptTokens,
			decoded.Usage.CompletionTokens,
			p.inputCostMicrosPerMillion,
			p.outputCostMicrosPerMillion,
		),
	}, nil
}

func calculateTranslationCostMicros(inputTokens int, outputTokens int, inputCostMicrosPerMillion int, outputCostMicrosPerMillion int) int {
	return costMicrosForTokens(inputTokens, inputCostMicrosPerMillion) +
		costMicrosForTokens(outputTokens, outputCostMicrosPerMillion)
}

func costMicrosForTokens(tokens int, costMicrosPerMillion int) int {
	if tokens <= 0 || costMicrosPerMillion <= 0 {
		return 0
	}
	return int((int64(tokens)*int64(costMicrosPerMillion) + 999999) / 1000000)
}

func (p *DeepSeekTranslationProvider) chatCompletionPayload(req TranslationProviderRequest) map[string]any {
	userPayload := map[string]any{
		"resource_type":    req.ResourceType,
		"resource_id":      req.ResourceID,
		"source_language":  req.SourceLanguage,
		"target_language":  req.TargetLanguage,
		"title":            req.Title,
		"body":             req.Body,
		"response_schema":  map[string]any{"title": "string", "body": "string"},
		"empty_title_note": "Answer resources have an empty title and must return an empty title.",
	}
	userJSON, err := json.Marshal(userPayload)
	if err != nil {
		userJSON = []byte("{}")
	}
	return map[string]any{
		"model": p.model,
		"messages": []map[string]string{
			{
				"role":    "system",
				"content": deepSeekTranslationSystemPrompt(),
			},
			{
				"role":    "user",
				"content": string(userJSON),
			},
		},
		"thinking":        map[string]string{"type": "disabled"},
		"response_format": map[string]string{"type": "json_object"},
		"stream":          false,
	}
}

func deepSeekTranslationSystemPrompt() string {
	return strings.Join([]string{
		"You translate Roundtable content between English and Simplified Chinese.",
		"Thinking mode is disabled. Do not include reasoning or commentary.",
		"Return only a JSON object with string fields title and body.",
		"Preserve Markdown structure, headings, lists, tables, blockquotes, code fences, inline code, URLs, file paths, shell commands, JSON, IDs, tags, user names, agent names, and product names exactly unless they are ordinary prose that must be translated.",
		"Do not add facts, remove facts, summarize, censor, or answer the content.",
		"Keep answer titles empty when the input title is empty.",
	}, " ")
}

type deepSeekTranslationPayload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

type deepSeekChatCompletionResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Model string `json:"model"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func decodeDeepSeekTranslationContent(content string) (deepSeekTranslationPayload, error) {
	content = strings.TrimSpace(content)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)
	var payload deepSeekTranslationPayload
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return deepSeekTranslationPayload{}, err
	}
	return payload, nil
}
