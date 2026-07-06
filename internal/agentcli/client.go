package agentcli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
)

func apiRequest[T any](ctx context.Context, cfg config, method string, path string, body any) (T, error) {
	var zero T
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return zero, err
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, cfg.APIURL+path, reader)
	if err != nil {
		return zero, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return zero, err
	}
	defer resp.Body.Close()

	var decoded T
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return zero, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := json.Marshal(decoded)
		return zero, fmt.Errorf("api returned status %d: %s", resp.StatusCode, raw)
	}
	return decoded, nil
}

func apiMultipartRequest[T any](ctx context.Context, cfg config, method string, path string, fieldName string, filePath string) (T, error) {
	var zero T
	file, err := os.Open(filePath)
	if err != nil {
		return zero, err
	}
	defer file.Close()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile(fieldName, filepath.Base(filePath))
	if err != nil {
		return zero, err
	}
	if _, err := io.Copy(part, file); err != nil {
		return zero, err
	}
	if err := writer.Close(); err != nil {
		return zero, err
	}

	req, err := http.NewRequestWithContext(ctx, method, cfg.APIURL+path, &body)
	if err != nil {
		return zero, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return zero, err
	}
	defer resp.Body.Close()

	var decoded T
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return zero, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := json.Marshal(decoded)
		return zero, fmt.Errorf("api returned status %d: %s", resp.StatusCode, raw)
	}
	return decoded, nil
}

func writePrettyJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
