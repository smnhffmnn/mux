package tools

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

const geminiMaxBody = 10 * 1024 * 1024  // 10 MB (base64 images are large)
const geminiMaxImageRead = 20 * 1024 * 1024 // 20 MB per input image

// validModelName matches safe Gemini model identifiers.
var validModelName = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// DefaultGeminiInstructions are used when no custom instructions are set.
const DefaultGeminiInstructions = `Google Gemini API — multimodal AI with image generation.

IMPORTANT: Do NOT hardcode model names. Always call list_models first to discover
available models and their capabilities, then choose the appropriate model.

Tools:
- list_models — List available models with capabilities. Use this to find image-capable models.
  Tip: filter results by supportedGenerationMethods containing "generateContent".
- generate — Generate content (text, images, or both) using a specified model.

Image generation tips:
- Set response_modalities to "TEXT,IMAGE" for image output (required for image generation)
- Use aspect_ratio (e.g. "16:9") and image_size (e.g. "1K", "2K", "4K") to control output
- Generated images are automatically saved to disk; file paths are returned
- Image generation typically takes 10-30 seconds
- For image editing, pass existing images via the images parameter`

// Gemini wraps the Google Gemini API as MCP tools.
type Gemini struct {
	client    *http.Client
	baseURL   string
	apiKey    string
	outputDir string
}

// NewGemini creates a Gemini connection from config.
func NewGemini(conn config.Connection, dialer Dialer) (*Gemini, error) {
	if conn.Token == "" {
		return nil, fmt.Errorf("gemini: API key is required")
	}

	baseURL := strings.TrimRight(conn.URL, "/")
	if baseURL == "" {
		baseURL = "https://generativelanguage.googleapis.com/v1beta"
	}

	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	outputDir := filepath.Join(home, ".mux", "output")

	transport := &http.Transport{
		ResponseHeaderTimeout: 120 * time.Second,
	}
	if dialer != nil {
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		}
	}

	return &Gemini{
		client: &http.Client{
			Transport: transport,
			Timeout:   120 * time.Second,
		},
		baseURL:   baseURL,
		apiKey:    conn.Token,
		outputDir: outputDir,
	}, nil
}

// Tools returns the MCP tools for the Gemini connection.
func (g *Gemini) Tools() []ToolDef {
	return []ToolDef{
		{
			Tool: mcp.NewTool("list_models",
				mcp.WithDescription(
					"List available Google Gemini models with their capabilities. "+
						"Use this to discover model names before calling generate. "+
						"Returns model IDs, display names, and supported generation methods.",
				),
			),
			Handler: g.handleListModels,
		},
		{
			Tool: mcp.NewTool("generate",
				mcp.WithDescription(
					"Generate content using a Gemini model. Supports text, image generation, "+
						"and image editing. Text parts are returned inline; images are saved to disk "+
						"and file paths are returned.",
				),
				mcp.WithString("model",
					mcp.Required(),
					mcp.Description("Model ID from list_models (e.g. the id field). Use list_models to discover available models."),
				),
				mcp.WithString("prompt",
					mcp.Required(),
					mcp.Description("Text prompt for content generation."),
				),
				mcp.WithString("images",
					mcp.Description("Comma-separated list of image file paths for image editing (supports ~ for home dir). Max 20 MB per image."),
				),
				mcp.WithString("response_modalities",
					mcp.Description("Comma-separated output types: TEXT, IMAGE, or TEXT,IMAGE. Required for image generation. If omitted, the API decides based on the model."),
				),
				mcp.WithString("aspect_ratio",
					mcp.Description("Image aspect ratio: 1:1, 2:3, 3:2, 3:4, 4:3, 4:5, 5:4, 9:16, 16:9, 21:9, 1:4, 4:1, 1:8, 8:1"),
				),
				mcp.WithString("image_size",
					mcp.Description("Image output resolution: 512, 1K, 2K, or 4K (default: 1K)"),
				),
				mcp.WithString("output_dir",
					mcp.Description("Override output directory for saved images (default: ~/.mux/output/, supports ~ for home dir)."),
				),
			),
			Handler: g.handleGenerate,
		},
	}
}

func (g *Gemini) handleListModels(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, g.baseURL+"/models", nil)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid request: %v", err)), nil
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("x-goog-api-key", g.apiKey)

	resp, err := g.client.Do(httpReq)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("request failed: %v", err)), nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, geminiMaxBody))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("read response: %v", err)), nil
	}

	if resp.StatusCode != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body))), nil
	}

	// Parse and return compact summary
	var listResp struct {
		Models []struct {
			Name                       string   `json:"name"`
			DisplayName                string   `json:"displayName"`
			Description                string   `json:"description"`
			SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &listResp); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("parse models response: %v", err)), nil
	}

	type modelSummary struct {
		ID          string   `json:"id"`
		DisplayName string   `json:"displayName"`
		Description string   `json:"description"`
		Methods     []string `json:"methods"`
	}

	summaries := make([]modelSummary, 0, len(listResp.Models))
	for _, m := range listResp.Models {
		// Strip "models/" prefix for cleaner output
		id := strings.TrimPrefix(m.Name, "models/")
		summaries = append(summaries, modelSummary{
			ID:          id,
			DisplayName: m.DisplayName,
			Description: m.Description,
			Methods:     m.SupportedGenerationMethods,
		})
	}

	data, _ := json.MarshalIndent(summaries, "", "  ")
	return mcp.NewToolResultText(string(data)), nil
}

func (g *Gemini) handleGenerate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	model, _ := req.RequireString("model")
	if model == "" {
		return mcp.NewToolResultError("model is required — use list_models to discover available models"), nil
	}
	prompt, _ := req.RequireString("prompt")
	if prompt == "" {
		return mcp.NewToolResultError("prompt is required"), nil
	}

	// Validate model name to prevent URL injection
	cleanModel := strings.TrimPrefix(model, "models/")
	if !validModelName.MatchString(cleanModel) {
		return mcp.NewToolResultError(fmt.Sprintf("invalid model name %q: must contain only letters, digits, dots, hyphens, and underscores", model)), nil
	}
	model = "models/" + cleanModel

	// Build request body
	reqBody, err := g.buildGenerateRequest(req, prompt)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("build request: %v", err)), nil
	}

	reqJSON, err := json.Marshal(reqBody)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("marshal request: %v", err)), nil
	}

	url := fmt.Sprintf("%s/%s:generateContent", g.baseURL, model)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqJSON))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid request: %v", err)), nil
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("x-goog-api-key", g.apiKey)

	resp, err := g.client.Do(httpReq)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("request failed: %v", err)), nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, geminiMaxBody))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("read response: %v", err)), nil
	}

	if resp.StatusCode != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body))), nil
	}

	// Determine output directory
	outputDir := g.outputDir
	if override := req.GetString("output_dir", ""); override != "" {
		outputDir = config.ExpandHome(override)
	}

	return g.parseGenerateResponse(body, outputDir)
}

// buildGenerateRequest constructs the Gemini API request body.
func (g *Gemini) buildGenerateRequest(req mcp.CallToolRequest, prompt string) (map[string]any, error) {
	// Build content parts
	var parts []map[string]any
	parts = append(parts, map[string]any{"text": prompt})

	// Add images if provided (for editing)
	if imagesStr := req.GetString("images", ""); imagesStr != "" {
		for _, imgPath := range strings.Split(imagesStr, ",") {
			imgPath = strings.TrimSpace(imgPath)
			if imgPath == "" {
				continue
			}
			imgPath = config.ExpandHome(imgPath)

			// Check file size before reading
			info, err := os.Stat(imgPath)
			if err != nil {
				return nil, fmt.Errorf("stat image %s: %w", imgPath, err)
			}
			if info.Size() > geminiMaxImageRead {
				return nil, fmt.Errorf("image %s too large (%d bytes, max %d)", imgPath, info.Size(), geminiMaxImageRead)
			}

			data, err := os.ReadFile(imgPath)
			if err != nil {
				return nil, fmt.Errorf("read image %s: %w", imgPath, err)
			}
			mimeType := mimeFromPath(imgPath)
			parts = append(parts, map[string]any{
				"inlineData": map[string]any{
					"mimeType": mimeType,
					"data":     base64.StdEncoding.EncodeToString(data),
				},
			})
		}
	}

	body := map[string]any{
		"contents": []map[string]any{
			{"parts": parts},
		},
	}

	// Build generationConfig
	genConfig := map[string]any{}

	// Response modalities (only set when explicitly provided)
	if modalities := req.GetString("response_modalities", ""); modalities != "" {
		var modalList []string
		for _, m := range strings.Split(modalities, ",") {
			m = strings.TrimSpace(strings.ToUpper(m))
			if m != "" {
				modalList = append(modalList, m)
			}
		}
		if len(modalList) > 0 {
			genConfig["responseModalities"] = modalList
		}
	}

	// Image config
	imageConfig := map[string]any{}
	if ar := req.GetString("aspect_ratio", ""); ar != "" {
		imageConfig["aspectRatio"] = ar
	}
	if is := req.GetString("image_size", ""); is != "" {
		imageConfig["imageSize"] = is
	}
	if len(imageConfig) > 0 {
		genConfig["imageConfig"] = imageConfig
	}

	if len(genConfig) > 0 {
		body["generationConfig"] = genConfig
	}

	return body, nil
}

// parseGenerateResponse extracts text and images from the Gemini API response.
// Text parts are returned inline; images are decoded from base64 and saved to disk.
func (g *Gemini) parseGenerateResponse(body []byte, outputDir string) (*mcp.CallToolResult, error) {
	var resp struct {
		Candidates []struct {
			Content struct {
				Parts []json.RawMessage `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		PromptFeedback struct {
			BlockReason string `json:"blockReason"`
		} `json:"promptFeedback"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal(body, &resp); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("parse response: %v", err)), nil
	}

	if resp.Error.Message != "" {
		return mcp.NewToolResultError(fmt.Sprintf("Gemini API error %d: %s", resp.Error.Code, resp.Error.Message)), nil
	}

	if resp.PromptFeedback.BlockReason != "" {
		return mcp.NewToolResultError(fmt.Sprintf("prompt blocked: %s", resp.PromptFeedback.BlockReason)), nil
	}

	if len(resp.Candidates) == 0 {
		return mcp.NewToolResultError("no candidates in response"), nil
	}

	var textParts []string
	var imagePaths []string
	var warnings []string
	imageIdx := 0

	// Use timestamp with random suffix to avoid collisions on concurrent requests
	randBytes := make([]byte, 3)
	rand.Read(randBytes)
	filePrefix := fmt.Sprintf("%s-%x", time.Now().Format("20060102-150405"), randBytes)

	for _, candidate := range resp.Candidates {
		if r := candidate.FinishReason; r != "" && r != "STOP" {
			warnings = append(warnings, fmt.Sprintf("[finishReason: %s]", r))
		}
		for _, rawPart := range candidate.Content.Parts {
			// Try text part
			var textPart struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(rawPart, &textPart); err == nil && textPart.Text != "" {
				textParts = append(textParts, textPart.Text)
				continue
			}

			// Try inline data (image)
			var dataPart struct {
				InlineData struct {
					MimeType string `json:"mimeType"`
					Data     string `json:"data"`
				} `json:"inlineData"`
			}
			if err := json.Unmarshal(rawPart, &dataPart); err == nil && dataPart.InlineData.Data != "" {
				path, err := g.saveImage(dataPart.InlineData.Data, dataPart.InlineData.MimeType, outputDir, filePrefix, imageIdx)
				if err != nil {
					textParts = append(textParts, fmt.Sprintf("[image %d decode error: %v]", imageIdx, err))
				} else {
					imagePaths = append(imagePaths, path)
				}
				imageIdx++
			}
		}
	}

	// Build result
	var result strings.Builder
	for _, text := range textParts {
		result.WriteString(text)
		result.WriteString("\n")
	}
	if len(imagePaths) > 0 {
		result.WriteString("\nGenerated images:\n")
		for _, p := range imagePaths {
			result.WriteString(fmt.Sprintf("  %s\n", p))
		}
	}
	for _, w := range warnings {
		result.WriteString("\n" + w)
	}
	if len(textParts) == 0 && len(imagePaths) == 0 {
		return mcp.NewToolResultError("response contained no text or images"), nil
	}

	return mcp.NewToolResultText(strings.TrimSpace(result.String())), nil
}

// saveImage decodes base64 image data and writes it to a file.
func (g *Gemini) saveImage(b64Data, mimeType, outputDir, filePrefix string, idx int) (string, error) {
	data, err := base64.StdEncoding.DecodeString(b64Data)
	if err != nil {
		return "", fmt.Errorf("base64 decode: %w", err)
	}

	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return "", fmt.Errorf("create output dir: %w", err)
	}

	ext := extFromMime(mimeType)
	filename := fmt.Sprintf("%s-%d%s", filePrefix, idx, ext)
	path := filepath.Join(outputDir, filename)

	if err := os.WriteFile(path, data, 0644); err != nil {
		return "", fmt.Errorf("write file: %w", err)
	}

	return path, nil
}

// extFromMime returns a file extension for a MIME type.
func extFromMime(mime string) string {
	switch mime {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ".png" // Gemini typically returns PNG
	}
}

// mimeFromPath guesses a MIME type from a file extension.
func mimeFromPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	default:
		return "application/octet-stream"
	}
}
