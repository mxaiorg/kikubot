package services

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"kikubot/internal/config"
	"net/http"
	"os"
	"path/filepath"
)

// ExtractTextFromBytes sends raw bytes to Tika and returns the extracted text.
func ExtractTextFromBytes(data []byte, filename string) (string, error) {
	req, err := http.NewRequest(http.MethodPut, config.TikaUrl, bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("tika: create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if filename != "" {
		req.Header.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("tika: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("tika: status %d: %s", resp.StatusCode, body)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("tika: decode response: %w", err)
	}

	content, ok := result["X-TIKA:content"].(string)
	if !ok {
		return "", fmt.Errorf("tika: X-TIKA:content not found in response")
	}
	return content, nil
}

// ExtractTextFromFile reads a file and sends it to Tika for text extraction.
func ExtractTextFromFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("tika: read file: %w", err)
	}
	return ExtractTextFromBytes(data, filepath.Base(path))
}
