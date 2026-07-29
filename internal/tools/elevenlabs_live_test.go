//go:build live
// +build live

// Live integration test for the ElevenLabs post verb. Runs only with:
//
//	ELEVENLABS_API_KEY=xi_xxx go test -tags=live ./internal/tools/ -run TestLiveElevenLabs -v
//
// Burns ElevenLabs credits (one short sentence per run) — gated behind the
// build tag and env var. The voice is taken from GET /v1/voices, so no account
// specific ID is baked into the test.
//
// If ffprobe is on PATH the generated file is probed for a real duration;
// otherwise that assertion is skipped and only the byte count is checked.

package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/smnhffmnn/mux/internal/config"
)

func liveElevenLabs(t *testing.T) *ElevenLabs {
	t.Helper()
	key := os.Getenv("ELEVENLABS_API_KEY")
	if key == "" {
		t.Skip("ELEVENLABS_API_KEY not set")
	}
	e, err := NewElevenLabs(config.Connection{
		Name:  "live-elevenlabs",
		Type:  config.TypeElevenLabs,
		Token: key,
	}, nil)
	if err != nil {
		t.Fatalf("NewElevenLabs: %v", err)
	}
	return e
}

// liveElevenLabsJSON runs a GET and returns the JSON payload behind the status line.
func liveElevenLabsJSON(t *testing.T, e *ElevenLabs, path string, out any) {
	t.Helper()
	res, err := e.handleGet(context.Background(), toolRequest(map[string]any{"path": path}))
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	text := resultText(t, res)
	if res.IsError {
		t.Fatalf("GET %s failed: %s", path, text)
	}
	if !strings.HasPrefix(text, "HTTP 200") {
		t.Fatalf("GET %s: %s", path, text)
	}
	_, payload, ok := strings.Cut(text, "\n\n")
	if !ok {
		t.Fatalf("GET %s: no payload in result:\n%s", path, text)
	}
	if err := json.Unmarshal([]byte(payload), out); err != nil {
		t.Fatalf("GET %s: decode payload: %v", path, err)
	}
}

func TestLiveElevenLabs_Subscription(t *testing.T) {
	e := liveElevenLabs(t)

	var sub struct {
		Tier                     string `json:"tier"`
		CharacterCount           int    `json:"character_count"`
		CharacterLimit           int    `json:"character_limit"`
		AllowedToExtendCharacter bool   `json:"can_extend_character_limit"`
	}
	liveElevenLabsJSON(t, e, "/v1/user/subscription", &sub)

	t.Logf("tier=%s characters=%d/%d", sub.Tier, sub.CharacterCount, sub.CharacterLimit)
	if sub.CharacterLimit == 0 {
		t.Error("character_limit = 0, want a real subscription payload")
	}
}

func TestLiveElevenLabs_TextToSpeech(t *testing.T) {
	e := liveElevenLabs(t)

	var voices struct {
		Voices []struct {
			VoiceID string `json:"voice_id"`
			Name    string `json:"name"`
		} `json:"voices"`
	}
	liveElevenLabsJSON(t, e, "/v1/voices", &voices)
	if len(voices.Voices) == 0 {
		t.Fatal("GET /v1/voices returned no voices")
	}
	voice := voices.Voices[0]
	t.Logf("using voice %s (%s)", voice.Name, voice.VoiceID)

	target := filepath.Join(t.TempDir(), "live-tts.mp3")
	body, err := json.Marshal(map[string]string{
		"text":     "Kurzer deutscher Testsatz mit Umlauten: Öl, Ähre, Übung.",
		"model_id": "eleven_multilingual_v2",
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	res, err := e.handlePost(context.Background(), toolRequest(map[string]any{
		"path":        "/v1/text-to-speech/" + voice.VoiceID,
		"body":        string(body),
		"output_file": target,
	}))
	if err != nil {
		t.Fatalf("handlePost: %v", err)
	}
	text := resultText(t, res)
	if res.IsError {
		t.Fatalf("text-to-speech failed: %s", text)
	}
	t.Log(text)

	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat audio file: %v", err)
	}
	if info.Size() < 1024 {
		t.Fatalf("audio file is %d bytes — too small to be speech", info.Size())
	}

	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skipf("ffprobe not on PATH — wrote %d bytes to %s, duration unverified", info.Size(), target)
	}
	out, err := exec.Command(ffprobe,
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		target,
	).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	duration, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		t.Fatalf("ffprobe duration %q: %v", out, err)
	}
	if duration <= 0 {
		t.Fatalf("ffprobe duration = %v, want > 0", duration)
	}
	t.Logf("playable audio: %.2fs, %d bytes", duration, info.Size())
}

func TestLiveElevenLabs_OutputDirFallback(t *testing.T) {
	e := liveElevenLabs(t)

	var voices struct {
		Voices []struct {
			VoiceID string `json:"voice_id"`
		} `json:"voices"`
	}
	liveElevenLabsJSON(t, e, "/v1/voices", &voices)
	if len(voices.Voices) == 0 {
		t.Fatal("GET /v1/voices returned no voices")
	}

	body, err := json.Marshal(map[string]string{
		"text":     "Kurzer Test ohne Zieldatei.",
		"model_id": "eleven_multilingual_v2",
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	res, err := e.handlePost(context.Background(), toolRequest(map[string]any{
		"path": "/v1/text-to-speech/" + voices.Voices[0].VoiceID,
		"body": string(body),
	}))
	if err != nil {
		t.Fatalf("handlePost: %v", err)
	}
	text := resultText(t, res)
	if res.IsError {
		t.Fatalf("text-to-speech failed: %s", text)
	}

	path := savedPath(t, text)
	t.Cleanup(func() { os.Remove(path) })
	if filepath.Ext(path) != ".mp3" {
		t.Errorf("extension = %q, want .mp3", filepath.Ext(path))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat audio file: %v", err)
	}
	if info.Size() < 1024 {
		t.Errorf("audio file is %d bytes — too small to be speech", info.Size())
	}
	t.Logf("output-dir fallback wrote %d bytes to %s", info.Size(), path)
}
