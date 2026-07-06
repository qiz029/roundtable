package roundtable

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeepSeekTranslationProviderBuildsNonThinkingRequest(t *testing.T) {
	var authHeader string
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("path = %q, want /chat/completions", r.URL.Path)
		}
		authHeader = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": "{\"title\":\"你好 `code`\",\"body\":\"保留 https://example.com\"}"}},
			},
			"model": "deepseek-v4-flash",
			"usage": map[string]any{"prompt_tokens": 7, "completion_tokens": 5},
		})
	}))
	defer server.Close()

	provider, err := NewDeepSeekTranslationProvider(DeepSeekTranslationProviderOptions{
		APIKey:     "test-deepseek-key",
		APIBaseURL: server.URL,
		Model:      "deepseek-v4-flash",
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	result, err := provider.Translate(context.Background(), TranslationProviderRequest{
		ResourceType:   "question",
		ResourceID:     "qst_test",
		SourceLanguage: "en",
		TargetLanguage: "zh-CN",
		Title:          "Hello `code`",
		Body:           "Visit https://example.com",
	})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	if authHeader != "Bearer test-deepseek-key" {
		t.Fatalf("authorization header = %q", authHeader)
	}
	if got := payload["model"]; got != "deepseek-v4-flash" {
		t.Fatalf("model = %#v", got)
	}
	thinking := payload["thinking"].(map[string]any)
	if got := thinking["type"]; got != "disabled" {
		t.Fatalf("thinking.type = %#v", got)
	}
	responseFormat := payload["response_format"].(map[string]any)
	if got := responseFormat["type"]; got != "json_object" {
		t.Fatalf("response_format.type = %#v", got)
	}
	messages := payload["messages"].([]any)
	system := messages[0].(map[string]any)
	if !strings.Contains(system["content"].(string), "Preserve Markdown") {
		t.Fatalf("system prompt did not include preservation rules: %q", system["content"])
	}
	user := messages[1].(map[string]any)
	var userPayload map[string]any
	if err := json.Unmarshal([]byte(user["content"].(string)), &userPayload); err != nil {
		t.Fatalf("decode user payload: %v", err)
	}
	if got := userPayload["source_language"]; got != "en" {
		t.Fatalf("source_language = %#v", got)
	}
	if got := userPayload["target_language"]; got != "zh-CN" {
		t.Fatalf("target_language = %#v", got)
	}
	if got := result.Provider; got != "deepseek" {
		t.Fatalf("provider = %q", got)
	}
	if got := result.Model; got != "deepseek-v4-flash" {
		t.Fatalf("model = %q", got)
	}
	if got := result.InputTokens; got != 7 {
		t.Fatalf("input tokens = %d", got)
	}
	if got := result.OutputTokens; got != 5 {
		t.Fatalf("output tokens = %d", got)
	}
	if got := result.Title; got != "你好 `code`" {
		t.Fatalf("title = %q", got)
	}
	if got := result.Body; got != "保留 https://example.com" {
		t.Fatalf("body = %q", got)
	}
}

func TestDeepSeekTranslationProviderRequiresKeyAndHidesItOnErrors(t *testing.T) {
	if _, err := NewDeepSeekTranslationProvider(DeepSeekTranslationProviderOptions{}); err == nil {
		t.Fatal("expected missing api key error")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad upstream", http.StatusBadGateway)
	}))
	defer server.Close()
	provider, err := NewDeepSeekTranslationProvider(DeepSeekTranslationProviderOptions{
		APIKey:     "test-secret-key",
		APIBaseURL: server.URL,
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	_, err = provider.Translate(context.Background(), TranslationProviderRequest{
		ResourceType:   "question",
		ResourceID:     "qst_test",
		SourceLanguage: "en",
		TargetLanguage: "zh-CN",
		Title:          "Hello",
		Body:           "World",
	})
	if err == nil {
		t.Fatal("expected upstream error")
	}
	if strings.Contains(err.Error(), "test-secret-key") {
		t.Fatalf("error leaked api key: %v", err)
	}
}
