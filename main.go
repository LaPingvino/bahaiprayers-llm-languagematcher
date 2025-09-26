package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var OllamaModel string = "gpt-oss"
var stopRequested int32                     // Atomic flag for graceful stop
var interruptCount int32                    // Count of interrupt signals received
var geminiQuotaExceeded int32               // Atomic flag for quota exceeded
var geminiArgErrors int32                   // Atomic counter for argument list too long errors
var ollamaAPIURL = "http://localhost:11434" // Ollama API endpoint

// Raw response storage for large responses
var storedRawResponses []string
var storedRawResponsesMutex sync.Mutex

// Session notes system for LLM experience accumulation
type SessionNote struct {
	Timestamp  time.Time
	Language   string
	NoteType   string // SUCCESS, FAILURE, PATTERN, STRATEGY, TIP
	Content    string
	PhelpsCode string  // Optional, for successful matches
	Confidence float64 // Optional, for confidence-related notes
}

var sessionNotes []SessionNote
var sessionNotesMutex sync.Mutex

// Transliteration corrections storage
type TranslitCorrection struct {
	PhelpsCode    string
	Confidence    float64
	CorrectedText string
	Language      string // ar-translit or fa-translit
}

var pendingTranslitCorrections []TranslitCorrection
var translitCorrectionsMutex sync.Mutex

//go:embed transliteration_standards.txt
var transliterationStandardsContent string

// Helper function to execute dolt commands in the correct directory
func execDoltCommand(args ...string) *exec.Cmd {
	cmd := exec.Command("dolt", args...)
	cmd.Dir = "bahaiwritings"
	return cmd
}

// Helper function to execute dolt query and return output with error handling
func execDoltQuery(query string) ([]byte, error) {
	cmd := execDoltCommand("sql", "-q", query)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("dolt query failed: %w: %s", err, string(output))
	}
	return output, nil
}

// Helper function to execute dolt CSV query and return output with error handling
func execDoltQueryCSV(query string) ([]byte, error) {
	cmd := execDoltCommand("sql", "-r", "csv", "-q", query)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("dolt CSV query failed: %w: %s", err, string(output))
	}
	return output, nil
}

// Helper function to execute dolt command and return error if it fails
func execDoltRun(args ...string) error {
	cmd := execDoltCommand(args...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("dolt command failed: %w", err)
	}
	return nil
}

// Function to get Bahá'í transliteration standards from embedded content
func getTransliterationStandards() []string {
	lines := strings.Split(transliterationStandardsContent, "\n")
	var results []string
	for _, line := range lines {
		// Include all lines, even empty ones for proper formatting
		results = append(results, line)
	}
	return results
}

// Helper function to truncate large responses and store them
func truncateAndStore(response string, source string) string {
	const maxDisplayLength = 500
	if len(response) > maxDisplayLength {
		storedRawResponsesMutex.Lock()
		storedRawResponses = append(storedRawResponses, fmt.Sprintf("=== %s Raw Response ===\n%s\n", source, response))
		storedRawResponsesMutex.Unlock()

		return fmt.Sprintf("%s... [TRUNCATED - %d more characters. Use -show-raw to see full responses at end]",
			response[:maxDisplayLength], len(response)-maxDisplayLength)
	}
	return response
}

// Clear old session notes (keep only recent ones)
func clearOldSessionNotes(maxAge time.Duration) int {
	sessionNotesMutex.Lock()
	defer sessionNotesMutex.Unlock()

	cutoff := time.Now().Add(-maxAge)
	var kept []SessionNote
	removed := 0

	for _, note := range sessionNotes {
		if note.Timestamp.After(cutoff) {
			kept = append(kept, note)
		} else {
			removed++
		}
	}

	sessionNotes = kept
	return removed
}

// Search session notes by content or type
func searchSessionNotes(query string, noteType string, language string) []SessionNote {
	sessionNotesMutex.Lock()
	defer sessionNotesMutex.Unlock()

	var matches []SessionNote
	queryLower := strings.ToLower(query)

	for _, note := range sessionNotes {
		// Filter by language if specified
		if language != "" && note.Language != language && note.Language != "" {
			continue
		}

		// Filter by note type if specified
		if noteType != "" && note.NoteType != strings.ToUpper(noteType) {
			continue
		}

		// Search in content if query provided
		if query != "" {
			contentLower := strings.ToLower(note.Content)
			if !strings.Contains(contentLower, queryLower) {
				continue
			}
		}

		matches = append(matches, note)
	}

	return matches
}

// Remove notes by type or language
func removeSessionNotes(noteType string, language string, olderThan time.Duration) int {
	sessionNotesMutex.Lock()
	defer sessionNotesMutex.Unlock()

	var kept []SessionNote
	removed := 0
	cutoff := time.Time{}

	if olderThan > 0 {
		cutoff = time.Now().Add(-olderThan)
	}

	for _, note := range sessionNotes {
		shouldRemove := false

		// Remove by type
		if noteType != "" && note.NoteType == strings.ToUpper(noteType) {
			shouldRemove = true
		}

		// Remove by language
		if language != "" && note.Language == language {
			shouldRemove = true
		}

		// Remove by age
		if olderThan > 0 && note.Timestamp.Before(cutoff) {
			shouldRemove = true
		}

		if shouldRemove {
			removed++
		} else {
			kept = append(kept, note)
		}
	}

	sessionNotes = kept
	return removed
}

// Get session notes statistics
func getSessionNotesStats() map[string]int {
	sessionNotesMutex.Lock()
	defer sessionNotesMutex.Unlock()

	stats := make(map[string]int)
	stats["total"] = len(sessionNotes)

	typeStats := make(map[string]int)
	langStats := make(map[string]int)

	for _, note := range sessionNotes {
		typeStats[note.NoteType]++
		if note.Language != "" {
			langStats[note.Language]++
		}
	}

	for noteType, count := range typeStats {
		stats["type_"+strings.ToLower(noteType)] = count
	}

	for lang, count := range langStats {
		stats["lang_"+lang] = count
	}

	return stats
}

// Add a session note
func addSessionNote(language, noteType, content, phelpsCode string, confidence float64) {
	sessionNotesMutex.Lock()
	defer sessionNotesMutex.Unlock()

	note := SessionNote{
		Timestamp:  time.Now(),
		Language:   language,
		NoteType:   noteType,
		Content:    content,
		PhelpsCode: phelpsCode,
		Confidence: confidence,
	}

	sessionNotes = append(sessionNotes, note)

	// Keep only the most recent 50 notes to avoid memory bloat
	if len(sessionNotes) > 50 {
		sessionNotes = sessionNotes[len(sessionNotes)-50:]
	}
}

// Get relevant session notes for a language
func getRelevantNotes(language string) []SessionNote {
	sessionNotesMutex.Lock()
	defer sessionNotesMutex.Unlock()

	var relevant []SessionNote

	// Get notes for the specific language first
	for _, note := range sessionNotes {
		if note.Language == language || note.Language == "" {
			relevant = append(relevant, note)
		}
	}

	// Add general notes that might be helpful
	for _, note := range sessionNotes {
		if note.Language != language && note.Language != "" &&
			(note.NoteType == "STRATEGY" || note.NoteType == "PATTERN") {
			relevant = append(relevant, note)
		}
	}

	// Return most recent 10 notes
	if len(relevant) > 10 {
		relevant = relevant[len(relevant)-10:]
	}

	return relevant
}

// Format notes for inclusion in LLM prompt
func formatNotesForPrompt(notes []SessionNote) string {
	if len(notes) == 0 {
		return ""
	}

	var formatted strings.Builder
	formatted.WriteString("\nSESSION EXPERIENCE NOTES:\n")
	formatted.WriteString("Here are insights from previous prayers in this session:\n")

	for _, note := range notes {
		timeAgo := time.Since(note.Timestamp).Round(time.Minute)
		switch note.NoteType {
		case "SUCCESS":
			if note.PhelpsCode != "" {
				formatted.WriteString(fmt.Sprintf("✅ SUCCESS (%v ago): %s [%s, confidence: %.0f%%]\n",
					timeAgo, note.Content, note.PhelpsCode, note.Confidence*100))
			} else {
				formatted.WriteString(fmt.Sprintf("✅ SUCCESS (%v ago): %s\n", timeAgo, note.Content))
			}
		case "FAILURE":
			formatted.WriteString(fmt.Sprintf("❌ FAILURE (%v ago): %s\n", timeAgo, note.Content))
		case "PATTERN":
			formatted.WriteString(fmt.Sprintf("🔍 PATTERN (%v ago): %s\n", timeAgo, note.Content))
		case "STRATEGY":
			formatted.WriteString(fmt.Sprintf("💡 STRATEGY (%v ago): %s\n", timeAgo, note.Content))
		case "TIP":
			formatted.WriteString(fmt.Sprintf("💭 TIP (%v ago): %s\n", timeAgo, note.Content))
		}
	}

	formatted.WriteString("\nUse these insights to improve your analysis.\n")
	return formatted.String()
}

// Function to display all stored raw responses
func showStoredRawResponses() {
	storedRawResponsesMutex.Lock()
	defer storedRawResponsesMutex.Unlock()

	if len(storedRawResponses) == 0 {
		return
	}

	fmt.Printf("\n%s\n", strings.Repeat("=", 80))
	fmt.Printf("FULL RAW RESPONSES (truncated during execution)\n")
	fmt.Printf("%s\n\n", strings.Repeat("=", 80))

	for i, response := range storedRawResponses {
		fmt.Printf("Response %d:\n%s\n", i+1, response)
		if i < len(storedRawResponses)-1 {
			fmt.Printf("%s\n", strings.Repeat("-", 40))
		}
	}

	fmt.Printf("%s\n", strings.Repeat("=", 80))
}

// Ollama API request/response structures
type OllamaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type OllamaChatRequest struct {
	Model    string          `json:"model"`
	Messages []OllamaMessage `json:"messages"`
	Stream   bool            `json:"stream"`
}

type OllamaChatResponse struct {
	Message OllamaMessage `json:"message"`
	Done    bool          `json:"done"`
}

type OllamaToolCall struct {
	Function struct {
		Name      string `json:"name"`
		Arguments struct {
			Arguments string `json:"arguments"`
			Function  string `json:"function"`
			Args      string `json:"args"`
			Call      string `json:"call"`
		} `json:"arguments"`
	} `json:"function"`
}

type OllamaMessageWithTools struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []OllamaToolCall `json:"tool_calls"`
}

type OllamaChatResponseWithTools struct {
	Message OllamaMessageWithTools `json:"message"`
	Done    bool                   `json:"done"`
}

type OllamaModelInfo struct {
	Name       string    `json:"name"`
	ModifiedAt time.Time `json:"modified_at"`
	Size       int64     `json:"size"`
}

type OllamaTagsResponse struct {
	Models []OllamaModelInfo `json:"models"`
}

// Helper function to call Ollama API with dynamic timeout
func CallOllama(prompt string, textLength int) (string, error) {
	return CallOllamaWithMessages([]OllamaMessage{
		{Role: "user", Content: prompt},
	}, textLength)
}

// Call Ollama API with conversation messages
func CallOllamaWithMessages(messages []OllamaMessage, textLength int) (string, error) {
	// Calculate dynamic timeout based on text length
	baseTimeout := 30 * time.Minute
	extraTime := time.Duration(textLength/1000) * 1 * time.Minute
	timeout := baseTimeout + extraTime

	if timeout < 30*time.Minute {
		timeout = 30 * time.Minute
	}
	if timeout > 90*time.Minute {
		timeout = 90 * time.Minute
	}

	log.Printf("Starting Ollama API with model %s (timeout: %v for %d chars)...", OllamaModel, timeout.Round(time.Second), textLength)

	// Prepare the request
	request := OllamaChatRequest{
		Model:    OllamaModel,
		Messages: messages,
		Stream:   false,
	}

	requestBody, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	// Create HTTP request
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", ollamaAPIURL+"/api/chat", bytes.NewBuffer(requestBody))
	if err != nil {
		return "", fmt.Errorf("failed to create HTTP request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")

	// Make the request
	client := &http.Client{}
	startTime := time.Now()

	resp, err := client.Do(httpReq)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("ollama API timed out after %v with model %s", timeout.Round(time.Second), OllamaModel)
		}
		return "", fmt.Errorf("ollama API request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("ollama API returned status %d: %s", resp.StatusCode, string(body))
	}

	// Read and parse response
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	// Try to parse as response with tool calls first
	var chatResponseWithTools OllamaChatResponseWithTools
	if err := json.Unmarshal(responseBody, &chatResponseWithTools); err == nil && len(chatResponseWithTools.Message.ToolCalls) > 0 {
		// Handle tool calls format
		var toolCallStrings []string
		for _, toolCall := range chatResponseWithTools.Message.ToolCalls {
			// Try to get arguments from different field names
			args := toolCall.Function.Arguments.Arguments
			if args == "" {
				args = toolCall.Function.Arguments.Args
			}

			switch toolCall.Function.Name {
			case "SEARCH":
				if args != "" {
					toolCallStrings = append(toolCallStrings, fmt.Sprintf("SEARCH:%s", args))
				}
			case "GET_FULL_TEXT":
				if args != "" {
					toolCallStrings = append(toolCallStrings, fmt.Sprintf("GET_FULL_TEXT:%s", args))
				}
			case "GET_FOCUS_TEXT":
				if args != "" {
					toolCallStrings = append(toolCallStrings, fmt.Sprintf("GET_FOCUS_TEXT:%s", args))
				}
			case "GET_PARTIAL_TEXT":
				if args != "" {
					toolCallStrings = append(toolCallStrings, fmt.Sprintf("GET_PARTIAL_TEXT:%s", args))
				}
			case "ADD_NOTE":
				if args != "" {
					toolCallStrings = append(toolCallStrings, fmt.Sprintf("ADD_NOTE:%s", args))
				}
			case "SEARCH_NOTES":
				if args != "" {
					toolCallStrings = append(toolCallStrings, fmt.Sprintf("SEARCH_NOTES:%s", args))
				}
			case "CLEAR_NOTES":
				if args != "" {
					toolCallStrings = append(toolCallStrings, fmt.Sprintf("CLEAR_NOTES:%s", args))
				}
			case "LIST_REFERENCE_LANGUAGES":
				toolCallStrings = append(toolCallStrings, "LIST_REFERENCE_LANGUAGES")
			case "SWITCH_REFERENCE_LANGUAGE":
				if args != "" {
					toolCallStrings = append(toolCallStrings, fmt.Sprintf("SWITCH_REFERENCE_LANGUAGE:%s", args))
				}
			case "EXTEND_ROUNDS":
				if args != "" {
					toolCallStrings = append(toolCallStrings, fmt.Sprintf("EXTEND_ROUNDS:%s", args))
				}
			case "FINAL_ANSWER":
				if args != "" {
					toolCallStrings = append(toolCallStrings, fmt.Sprintf("FINAL_ANSWER:%s", args))
				}
			case "GET_STATS":
				toolCallStrings = append(toolCallStrings, "GET_STATS")
			}
		}

		// Combine content and tool calls
		content := strings.TrimSpace(chatResponseWithTools.Message.Content)
		if len(toolCallStrings) > 0 {
			if content != "" {
				content += "\n" + strings.Join(toolCallStrings, "\n")
			} else {
				content = strings.Join(toolCallStrings, "\n")
			}
		}

		elapsed := time.Since(startTime)
		log.Printf("Ollama API completed successfully in %v", elapsed.Round(time.Second))
		return content, nil
	}

	// Fall back to standard response format
	var chatResponse OllamaChatResponse
	if err := json.Unmarshal(responseBody, &chatResponse); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	// Filter out thinking sections and extract the actual response
	content := filterThinkingFromResponse(chatResponse.Message.Content)

	elapsed := time.Since(startTime)
	log.Printf("Ollama API completed successfully in %v", elapsed.Round(time.Second))

	return content, nil
}

// Helper function to filter out thinking sections from model responses
func ensureGeminiSettings() error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	geminiDir := filepath.Join(homeDir, ".gemini")
	settingsPath := filepath.Join(geminiDir, "settings.json")

	// Create .gemini directory if it doesn't exist
	if err := os.MkdirAll(geminiDir, 0755); err != nil {
		return fmt.Errorf("failed to create .gemini directory: %w", err)
	}

	// Check if settings file exists and is recent enough
	if stat, err := os.Stat(settingsPath); err == nil {
		// If file exists and is less than 1 hour old, assume it's fine
		if time.Since(stat.ModTime()) < time.Hour {
			return nil
		}
	}

	// Create or update settings to disable tools
	settings := `{
  "tools": {
    "exclude": [
      "run_shell_command",
      "write_file",
      "read_file",
      "list_directory",
      "create_directory",
      "move_path",
      "copy_path",
      "delete_path",
      "google_web_search",
      "terminal",
      "edit_file"
    ],
    "allowed": [],
    "sandbox": false
  },
  "output": {
    "format": "text"
  },
  "ui": {
    "hideTips": true,
    "hideBanner": true,
    "hideFooter": true
  },
  "general": {
    "checkpointing": {
      "enabled": false
    }
  },
  "model": {
    "name": "gemini-2.5-flash",
    "summarizeToolOutput": {},
    "skipNextSpeakerCheck": true
  }
}`

	if err := os.WriteFile(settingsPath, []byte(settings), 0644); err != nil {
		return fmt.Errorf("failed to write settings file: %w", err)
	}

	log.Printf("Updated Gemini CLI settings at %s", settingsPath)
	return nil
}

func filterCLIOutput(output string) string {
	lines := strings.Split(output, "\n")
	var filteredLines []string

	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Skip empty lines
		if line == "" {
			continue
		}

		// Skip only very specific CLI messages that we know are noise
		if strings.HasPrefix(line, "Loaded cached credentials") ||
			strings.HasPrefix(line, "Loading") ||
			strings.HasPrefix(line, "Initializing") ||
			strings.HasPrefix(line, "Connected") ||
			strings.HasPrefix(line, "Using model") ||
			strings.Contains(line, "session started") ||
			strings.HasPrefix(line, "Press Ctrl+C") {
			continue
		}

		filteredLines = append(filteredLines, line)
	}

	return strings.Join(filteredLines, "\n")
}

func filterThinkingFromResponse(response string) string {
	lines := strings.Split(response, "\n")
	var filteredLines []string
	skipMode := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Skip thinking sections
		if strings.HasPrefix(trimmed, "Thinking...") {
			skipMode = true
			continue
		}
		if strings.HasPrefix(trimmed, "...done thinking") {
			skipMode = false
			continue
		}

		// If we're in skip mode, continue skipping
		if skipMode {
			continue
		}

		// Keep non-thinking lines
		if trimmed != "" {
			filteredLines = append(filteredLines, line)
		}
	}

	return strings.Join(filteredLines, "\n")
}

// Helper function to shell out to Gemini CLI
func CallGemini(messages []OllamaMessage) (string, error) {
	// Check if quota exceeded flag is already set
	if atomic.LoadInt32(&geminiQuotaExceeded) == 1 {
		return "", fmt.Errorf("gemini quota previously exceeded, skipping")
	}

	// Check if we've had too many argument list errors
	if atomic.LoadInt32(&geminiArgErrors) >= 2 {
		return "", fmt.Errorf("too many argument list errors, skipping gemini")
	}

	// Settings should already be configured from main(), but double-check
	// if this is the first call in the session
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".gemini", "settings.json")); os.IsNotExist(err) {
		if err := ensureGeminiSettings(); err != nil {
			log.Printf("Warning: failed to configure Gemini settings: %v", err)
		}
	}

	// Format full conversation history into a single prompt
	var fullPrompt strings.Builder
	for i, msg := range messages {
		if msg.Role == "user" {
			if i == 0 {
				// First message is the system prompt/task
				fullPrompt.WriteString(msg.Content)
			} else {
				// Subsequent user messages are system corrections/feedback
				fullPrompt.WriteString("\n\nSYSTEM FEEDBACK:\n")
				fullPrompt.WriteString(msg.Content)
			}
		} else if msg.Role == "assistant" {
			// Include previous assistant responses for context
			fullPrompt.WriteString("\n\nPREVIOUS RESPONSE:\n")
			fullPrompt.WriteString(msg.Content)
		}
	}

	promptStr := fullPrompt.String()
	const maxArgLength = 1500000 // ~1.5MB, well under 2MB system limit but safe

	var cmd *exec.Cmd

	// If prompt is too long, use a temporary file
	if len(promptStr) > maxArgLength {
		log.Printf("Prompt too long (%d bytes), using temporary file", len(promptStr))

		tempFile, err := ioutil.TempFile("", "gemini-prompt-*.txt")
		if err != nil {
			return "", fmt.Errorf("failed to create temp file: %w", err)
		}
		defer os.Remove(tempFile.Name())
		defer tempFile.Close()

		if _, err := tempFile.WriteString(promptStr); err != nil {
			return "", fmt.Errorf("failed to write to temp file: %w", err)
		}
		tempFile.Close()

		// Use -f flag to read from file instead of -p
		cmd = exec.Command("gemini",
			"-f", tempFile.Name(),
			"--approval-mode", "default")
	} else {
		// Use -p flag for compatibility (though deprecated, it still works reliably)
		cmd = exec.Command("gemini",
			"-p", promptStr,
			"--approval-mode", "default")
	}

	// Set environment to disable thinking for faster responses
	cmd.Env = append(os.Environ(), "GEMINI_MODEL=gemini-2.5-flash")

	output, err := cmd.CombinedOutput()
	if err != nil {
		errorStr := strings.ToLower(err.Error() + string(output))

		// Check for argument list too long error
		if strings.Contains(errorStr, "argument list too long") || strings.Contains(errorStr, "e2big") {
			atomic.AddInt32(&geminiArgErrors, 1)
			errorCount := atomic.LoadInt32(&geminiArgErrors)
			log.Printf("Argument list too long error (#%d) - prompt length: %d bytes", errorCount, len(promptStr))

			if errorCount >= 2 {
				log.Printf("Too many argument list errors, disabling Gemini for remainder of session")
				return "", fmt.Errorf("repeated argument list too long errors, disabling gemini")
			}
			return "", fmt.Errorf("argument list too long: %w", err)
		}

		// Check if this is a quota exceeded error
		if strings.Contains(errorStr, "quota") && (strings.Contains(errorStr, "exceeded") || strings.Contains(errorStr, "exhausted")) ||
			strings.Contains(errorStr, "429") ||
			strings.Contains(errorStr, "resource_exhausted") {
			atomic.StoreInt32(&geminiQuotaExceeded, 1)
			log.Printf("Gemini quota exceeded - disabling Gemini for remainder of session")
			return "", fmt.Errorf("gemini quota exceeded: %w", err)
		}

		// Check for authentication errors
		if strings.Contains(errorStr, "auth") || strings.Contains(errorStr, "api_key") ||
			strings.Contains(errorStr, "credential") || strings.Contains(errorStr, "unauthenticated") {
			return "", fmt.Errorf("gemini authentication error - please configure API key or login: %w", err)
		}

		return "", fmt.Errorf("error running gemini CLI: %v\nOutput: %s", err, string(output))
	}

	// Clean up the output by removing CLI interface elements
	response := string(output)
	response = filterCLIOutput(response)
	response = filterThinkingFromResponse(response)
	response = strings.TrimSpace(response)

	if response == "" {
		return "", fmt.Errorf("empty response from gemini")
	}

	return response, nil
}

// LLMResponse represents the parsed response from an LLM
type LLMResponse struct {
	PhelpsCode string
	Confidence float64
	Reasoning  string
}

// LLMCaller interface allows dependency injection for testing
type LLMCaller interface {
	CallGemini(messages []OllamaMessage) (string, error)
	CallOllama(prompt string, textLength int) (string, error)
}

// DefaultLLMCaller implements LLMCaller using the actual CLI tools
type DefaultLLMCaller struct{}

func (d DefaultLLMCaller) CallGemini(messages []OllamaMessage) (string, error) {
	return CallGemini(messages)
}

func (d DefaultLLMCaller) CallOllama(prompt string, textLength int) (string, error) {
	return CallOllama(prompt, textLength)
}

// Parse LLM response to extract Phelps code and confidence
func parseLLMResponse(response string) LLMResponse {
	lines := strings.Split(strings.TrimSpace(response), "\n")
	result := LLMResponse{Confidence: 0.0}

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(line), "phelps:") {
			result.PhelpsCode = strings.TrimSpace(strings.TrimPrefix(line, "phelps:"))
			result.PhelpsCode = strings.TrimSpace(strings.TrimPrefix(result.PhelpsCode, "Phelps:"))
		} else if strings.HasPrefix(strings.ToLower(line), "confidence:") {
			confStr := strings.TrimSpace(strings.TrimPrefix(line, "confidence:"))
			confStr = strings.TrimSpace(strings.TrimPrefix(confStr, "Confidence:"))
			confStr = strings.TrimSuffix(confStr, "%")
			if conf, err := strconv.ParseFloat(confStr, 64); err == nil {
				result.Confidence = conf / 100.0 // Convert percentage to decimal
			}
		} else if strings.HasPrefix(strings.ToLower(line), "reasoning:") {
			result.Reasoning = strings.TrimSpace(strings.TrimPrefix(line, "reasoning:"))
			result.Reasoning = strings.TrimSpace(strings.TrimPrefix(result.Reasoning, "Reasoning:"))
		}
	}

	return result
}

// Extract distinctive words from text (most unique/uncommon words)
func extractDistinctiveWords(text string, n int) []string {
	// Common words to exclude (stop words)
	stopWords := map[string]bool{
		"the": true, "a": true, "an": true, "and": true, "or": true, "but": true,
		"in": true, "on": true, "at": true, "to": true, "for": true, "of": true,
		"with": true, "by": true, "from": true, "up": true, "about": true, "into": true,
		"through": true, "during": true, "before": true, "after": true, "above": true,
		"below": true, "between": true, "among": true, "is": true, "are": true, "was": true,
		"were": true, "be": true, "been": true, "being": true, "have": true, "has": true,
		"had": true, "do": true, "does": true, "did": true, "will": true, "would": true,
		"could": true, "should": true, "may": true, "might": true, "must": true, "can": true,
		"this": true, "that": true, "these": true, "those": true, "i": true, "me": true,
		"my": true, "myself": true, "we": true, "our": true, "ours": true, "ourselves": true,
		"you": true, "your": true, "yours": true, "yourself": true, "yourselves": true,
		"he": true, "him": true, "his": true, "himself": true, "she": true, "her": true,
		"hers": true, "herself": true, "it": true, "its": true, "itself": true, "they": true,
		"them": true, "their": true, "theirs": true, "themselves": true, "what": true,
		"which": true, "who": true, "whom": true, "whose": true, "where": true, "when": true,
		"why": true, "how": true, "all": true, "any": true, "both": true, "each": true,
		"few": true, "more": true, "most": true, "other": true, "some": true, "such": true,
		"no": true, "nor": true, "not": true, "only": true, "own": true, "same": true,
		"so": true, "than": true, "too": true, "very": true, "just": true, "now": true,
		"o": true, "oh": true, "thy": true, "thee": true, "thou": true, "thine": true,
	}

	// Clean and split text into words
	text = strings.ToLower(text)
	// Remove punctuation and split
	words := strings.FieldsFunc(text, func(c rune) bool {
		return !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9'))
	})

	// Count word frequencies
	wordCount := make(map[string]int)
	for _, word := range words {
		if len(word) > 2 && !stopWords[word] {
			wordCount[word]++
		}
	}

	// Sort words by frequency (less frequent = more distinctive)
	type wordFreq struct {
		word string
		freq int
	}
	var sortedWords []wordFreq
	for word, freq := range wordCount {
		sortedWords = append(sortedWords, wordFreq{word, freq})
	}
	sort.Slice(sortedWords, func(i, j int) bool {
		if sortedWords[i].freq == sortedWords[j].freq {
			return sortedWords[i].word < sortedWords[j].word // Alphabetical for ties
		}
		return sortedWords[i].freq < sortedWords[j].freq // Less frequent first
	})

	// Return top n distinctive words
	result := make([]string, 0, n)
	for i := 0; i < len(sortedWords) && i < n; i++ {
		result = append(result, sortedWords[i].word)
	}
	return result
}

// Get first meaningful line from text (skip empty lines)
func getFirstLine(text string) string {
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" && len(line) > 10 {
			if len(line) > 100 {
				return line[:100] + "..."
			}
			return line
		}
	}
	if len(text) > 100 {
		return text[:100] + "..."
	}
	return text
}

// Build rich context for each Phelps code
func buildPhelpsContext(db Database, referenceLanguage string) map[string]string {
	phelpsContext := make(map[string]string)

	// Group writings by Phelps code
	phelpsWritings := make(map[string][]Writing)
	for _, writing := range db.Writing {
		if writing.Phelps != "" && writing.Language == referenceLanguage && writing.Text != "" {
			phelpsWritings[writing.Phelps] = append(phelpsWritings[writing.Phelps], writing)
		}
	}

	// If no reference language data, use any language
	if len(phelpsWritings) == 0 {
		for _, writing := range db.Writing {
			if writing.Phelps != "" && writing.Text != "" {
				phelpsWritings[writing.Phelps] = append(phelpsWritings[writing.Phelps], writing)
			}
		}
	}

	// Build context for each Phelps code
	for phelps, writings := range phelpsWritings {
		if len(writings) == 0 {
			continue
		}

		// Use the longest/most complete writing for context
		bestWriting := writings[0]
		for _, writing := range writings {
			if len(writing.Text) > len(bestWriting.Text) {
				bestWriting = writing
			}
		}

		name := bestWriting.Name
		if name == "" {
			name = "Untitled"
		}

		firstLine := getFirstLine(bestWriting.Text)
		distinctiveWords := extractDistinctiveWords(bestWriting.Text, 5)
		wordCount := len(strings.Fields(bestWriting.Text))
		charCount := len(bestWriting.Text)

		context := fmt.Sprintf("%s (%s) [%d words, %d chars] - Opening: \"%s\" - Key words: %s",
			phelps, name, wordCount, charCount, firstLine, strings.Join(distinctiveWords, ", "))

		phelpsContext[phelps] = context
	}

	return phelpsContext
}

// Build context map with version IDs as keys instead of Phelps codes
func buildVersionContext(db Database, referenceLanguage string) map[string]string {
	versionContext := make(map[string]string)

	// Get writings by reference language
	var relevantWritings []Writing
	for _, writing := range db.Writing {
		if writing.Language == referenceLanguage && writing.Text != "" {
			relevantWritings = append(relevantWritings, writing)
		}
	}

	// If no reference language data, use any language
	if len(relevantWritings) == 0 {
		for _, writing := range db.Writing {
			if writing.Text != "" {
				relevantWritings = append(relevantWritings, writing)
			}
		}
	}

	// Build context for each version
	for _, writing := range relevantWritings {
		wordCount := len(strings.Fields(writing.Text))
		charCount := len(writing.Text)

		opening := ""
		if len(writing.Text) > 50 {
			opening = writing.Text[:50] + "..."
		} else {
			opening = writing.Text
		}

		phelpsDisplay := writing.Phelps
		if phelpsDisplay == "" {
			phelpsDisplay = "NO_PHELPS"
		}

		context := fmt.Sprintf("%s (Version: %s) [%d words, %d chars] Opening: \"%s\" Name: %s",
			phelpsDisplay, writing.Version, wordCount, charCount, opening, writing.Name)

		versionContext[writing.Version] = context
	}

	return versionContext
}

// Unified search function that handles multiple combined criteria
func searchPrayersUnified(db Database, referenceLanguage string, searchStr string) []string {
	return searchPrayersUnifiedWithVersions(db, referenceLanguage, searchStr, false)
}

func searchPrayersUnifiedWithVersions(db Database, referenceLanguage string, searchStr string, returnVersions bool) []string {
	searchStr = strings.TrimSpace(searchStr)
	if searchStr == "" {
		return []string{"Error: No search criteria provided. Use: SEARCH:keywords,opening phrase,100-200,..."}
	}

	// Parse multiple search criteria intelligently
	criteria := parseSearchCriteria(searchStr)
	var allResults []SearchResult

	// Apply each search type found
	if len(criteria.Keywords) > 0 {
		if returnVersions {
			keywordResults := searchPrayersByKeywordsWithScoreVersions(db, referenceLanguage, criteria.Keywords)
			allResults = append(allResults, keywordResults...)
		} else {
			keywordResults := searchPrayersByKeywordsWithScore(db, referenceLanguage, criteria.Keywords)
			allResults = append(allResults, keywordResults...)
		}
	}

	if criteria.Opening != "" {
		if returnVersions {
			openingResults := searchPrayersByOpeningWithScoreVersions(db, referenceLanguage, criteria.Opening)
			allResults = append(allResults, openingResults...)
		} else {
			openingResults := searchPrayersByOpeningWithScore(db, referenceLanguage, criteria.Opening)
			allResults = append(allResults, openingResults...)
		}
	}

	if criteria.LengthRange != "" {
		if returnVersions {
			lengthResults := searchPrayersByLengthWithScoreVersions(db, referenceLanguage, criteria.LengthRange)
			allResults = append(allResults, lengthResults...)
		} else {
			lengthResults := searchPrayersByLengthWithScore(db, referenceLanguage, criteria.LengthRange)
			allResults = append(allResults, lengthResults...)
		}
	}

	if len(allResults) == 0 {
		return []string{"No search criteria recognized. Use keywords, 'opening phrase', or 100-200 range"}
	}

	// Combine and deduplicate results
	return combineSearchResults(allResults, 15)
}

// SearchCriteria holds parsed search parameters
type SearchCriteria struct {
	Keywords    []string
	Opening     string
	LengthRange string
}

// Parse search criteria from comma-separated input
func parseSearchCriteria(searchStr string) SearchCriteria {
	var criteria SearchCriteria

	// Split by commas and analyze each part
	parts := strings.Split(searchStr, ",")
	var remainingParts []string

	// First pass: identify length ranges
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		if isLengthRange(part) {
			criteria.LengthRange = part
		} else {
			remainingParts = append(remainingParts, part)
		}
	}

	// Second pass: identify opening phrases vs keywords
	if len(remainingParts) > 0 {
		// Try to identify multi-word opening phrases
		opening := detectOpeningPhrase(remainingParts)
		if opening != "" {
			criteria.Opening = opening
			// Remove opening phrase parts from remaining
			remainingParts = removeOpeningParts(remainingParts, opening)
		}

		// Remaining parts are keywords
		criteria.Keywords = sanitizeKeywords(strings.Join(remainingParts, ","))
	}

	return criteria
}

// Detect if consecutive parts form an opening phrase
func detectOpeningPhrase(parts []string) string {
	if len(parts) < 2 {
		return ""
	}

	// Common opening words in prayers
	openingIndicators := map[string]bool{
		"o": true, "oh": true, "lord": true, "god": true, "blessed": true,
		"praise": true, "glory": true, "thou": true, "thy": true,
	}

	// Look for sequences that start with opening words
	for i := 0; i < len(parts)-1; i++ {
		firstWord := strings.ToLower(parts[i])
		if openingIndicators[firstWord] {
			// Try different phrase lengths
			for length := 2; length <= min(6, len(parts)-i); length++ {
				phrase := strings.Join(parts[i:i+length], " ")
				// If phrase looks like an opening (has prayer words), return it
				if looksLikePrayerOpening(phrase) {
					return phrase
				}
			}
		}
	}

	return ""
}

// Check if text looks like a prayer opening
func looksLikePrayerOpening(text string) bool {
	text = strings.ToLower(text)
	prayerWords := []string{"lord", "god", "blessed", "praise", "glory", "thou", "thy", "thee"}

	wordCount := 0
	for _, word := range prayerWords {
		if strings.Contains(text, word) {
			wordCount++
		}
	}

	// Need at least 1 prayer word and reasonable length
	return wordCount >= 1 && len(strings.Fields(text)) >= 2 && len(strings.Fields(text)) <= 8
}

// Remove opening phrase parts from the list
func removeOpeningParts(parts []string, opening string) []string {
	openingWords := strings.Fields(opening)
	if len(openingWords) == 0 {
		return parts
	}

	// Find where the opening starts in parts
	for i := 0; i <= len(parts)-len(openingWords); i++ {
		match := true
		for j, word := range openingWords {
			if i+j >= len(parts) || strings.ToLower(parts[i+j]) != strings.ToLower(word) {
				match = false
				break
			}
		}
		if match {
			// Remove the opening phrase parts
			result := make([]string, 0, len(parts)-len(openingWords))
			result = append(result, parts[:i]...)
			result = append(result, parts[i+len(openingWords):]...)
			return result
		}
	}

	return parts
}

// Helper function to check if string is a length range
func isLengthRange(s string) bool {
	parts := strings.Split(s, "-")
	if len(parts) != 2 {
		return false
	}
	_, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	_, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	return err1 == nil && err2 == nil
}

// SearchResult represents a search result with scoring
type SearchResult struct {
	ID         string // Can be Phelps code or Version ID
	Context    string
	PhelpsCode string
	Score      int
	SearchType string
}

// Search prayers by keywords with scoring
func searchPrayersByKeywordsWithScore(db Database, referenceLanguage string, keywords []string) []SearchResult {
	phelpsContext := buildPhelpsContext(db, referenceLanguage)
	var results []SearchResult

	for phelps, context := range phelpsContext {
		score := 0
		contextLower := strings.ToLower(context)

		for _, keyword := range keywords {
			keywordLower := strings.ToLower(strings.TrimSpace(keyword))
			if strings.Contains(contextLower, keywordLower) {
				score += 2 // Higher weight for keyword matches
			}
		}

		if score > 0 {
			results = append(results, SearchResult{
				ID:         phelps,
				Context:    context,
				PhelpsCode: phelps,
				Score:      score,
				SearchType: "KEYWORDS",
			})
		}
	}

	return results
}

func searchPrayersByOpeningWithScoreVersions(db Database, referenceLanguage string, opening string) []SearchResult {
	versionContext := buildVersionContext(db, referenceLanguage)
	var results []SearchResult
	openingLower := strings.ToLower(strings.TrimSpace(opening))

	for version, context := range versionContext {
		if strings.Contains(context, "Opening: \"") {
			parts := strings.Split(context, "Opening: \"")
			if len(parts) > 1 {
				contextOpening := strings.Split(parts[1], "\"")[0]
				openingCtxLower := strings.ToLower(contextOpening)

				similarity := 0
				if strings.Contains(openingCtxLower, openingLower) {
					similarity = 15
				} else {
					// Check for partial matches
					openingWords := strings.Fields(openingLower)
					for _, word := range openingWords {
						if len(word) > 2 && strings.Contains(openingCtxLower, word) {
							similarity += 3
						}
					}
				}

				if similarity > 0 {
					results = append(results, SearchResult{
						ID:         version,
						Context:    context,
						PhelpsCode: "", // No Phelps code for version-based search
						Score:      similarity,
						SearchType: "OPENING",
					})
				}
			}
		}
	}

	return results
}

func searchPrayersByLengthWithScoreVersions(db Database, referenceLanguage string, lengthStr string) []SearchResult {
	parts := strings.Split(lengthStr, "-")
	if len(parts) != 2 {
		return []SearchResult{}
	}

	minLength, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	maxLength, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil {
		return []SearchResult{}
	}

	versionContext := buildVersionContext(db, referenceLanguage)
	var results []SearchResult

	for version, context := range versionContext {
		if strings.Contains(context, "words") {
			parts := strings.Split(context, "[")
			if len(parts) > 1 {
				wordPart := strings.Split(parts[1], " words")[0]
				if wordCount, err := strconv.Atoi(strings.TrimSpace(wordPart)); err == nil {
					if wordCount >= minLength && wordCount <= maxLength {
						results = append(results, SearchResult{
							ID:         version,
							Context:    context,
							PhelpsCode: "", // No Phelps code for version-based search
							Score:      5,  // Basic score for length match
							SearchType: "LENGTH",
						})
					}
				}
			}
		}
	}

	return results
}

func searchPrayersByKeywordsWithScoreVersions(db Database, referenceLanguage string, keywords []string) []SearchResult {
	versionContext := buildVersionContext(db, referenceLanguage)
	var results []SearchResult

	for version, context := range versionContext {
		score := 0
		contextLower := strings.ToLower(context)

		for _, keyword := range keywords {
			keywordLower := strings.ToLower(strings.TrimSpace(keyword))
			if strings.Contains(contextLower, keywordLower) {
				score += 10
			}
		}

		if score > 0 {
			results = append(results, SearchResult{
				ID:         version,
				Context:    context,
				PhelpsCode: "", // No Phelps code for version-based search
				Score:      score,
				SearchType: "KEYWORDS",
			})
		}
	}

	return results
}

// Search prayers by opening with scoring
func searchPrayersByOpeningWithScore(db Database, referenceLanguage string, opening string) []SearchResult {
	phelpsContext := buildPhelpsContext(db, referenceLanguage)
	var results []SearchResult
	openingLower := strings.ToLower(strings.TrimSpace(opening))

	for phelps, context := range phelpsContext {
		if strings.Contains(context, "Opening: \"") {
			parts := strings.Split(context, "Opening: \"")
			if len(parts) > 1 {
				contextOpening := strings.Split(parts[1], "\"")[0]
				openingCtxLower := strings.ToLower(contextOpening)

				// Simple similarity score
				openingWords := strings.Fields(openingLower)
				ctxWords := strings.Fields(openingCtxLower)

				commonWords := 0
				for _, word := range openingWords {
					for _, ctxWord := range ctxWords {
						if word == ctxWord && len(word) > 2 {
							commonWords++
							break
						}
					}
				}

				if commonWords > 0 {
					score := commonWords * 3 // Higher weight for opening matches
					results = append(results, SearchResult{
						ID:         phelps,
						Context:    context,
						PhelpsCode: phelps,
						Score:      score,
						SearchType: "OPENING",
					})
				}
			}
		}
	}

	return results
}

// Search prayers by length with scoring
func searchPrayersByLengthWithScore(db Database, referenceLanguage string, lengthStr string) []SearchResult {
	parts := strings.Split(lengthStr, "-")
	if len(parts) != 2 {
		return []SearchResult{}
	}

	minLength, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	maxLength, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil {
		return []SearchResult{}
	}

	phelpsContext := buildPhelpsContext(db, referenceLanguage)
	var results []SearchResult

	for phelps, context := range phelpsContext {
		if strings.Contains(context, "[") && strings.Contains(context, " words,") {
			// Extract word count from format like "[123 words, 456 chars]"
			start := strings.Index(context, "[")
			end := strings.Index(context, " words,")
			if start != -1 && end != -1 && end > start {
				wordCountStr := context[start+1 : end]
				length, err := strconv.Atoi(strings.TrimSpace(wordCountStr))

				if err == nil && length >= minLength && length <= maxLength {
					// Score based on how close to middle of range
					midRange := (minLength + maxLength) / 2
					distance := abs(length - midRange)
					maxDistance := (maxLength - minLength) / 2
					score := 5 - (distance * 5 / max(maxDistance, 1)) // Score 1-5
					if score < 1 {
						score = 1
					}

					results = append(results, SearchResult{
						ID:         phelps,
						Context:    context,
						PhelpsCode: phelps,
						Score:      score,
						SearchType: "LENGTH",
					})
				}
			}
		}
	}

	return results
}

// Combine and deduplicate search results
func combineSearchResults(results []SearchResult, limit int) []string {
	// Group by Phelps code and combine scores
	combined := make(map[string]SearchResult)

	for _, result := range results {
		if existing, exists := combined[result.PhelpsCode]; exists {
			// Combine scores and search types
			existing.Score += result.Score
			if existing.SearchType != result.SearchType {
				existing.SearchType += "+" + result.SearchType
			}
			combined[result.PhelpsCode] = existing
		} else {
			combined[result.PhelpsCode] = result
		}
	}

	// Convert to slice and sort by score
	var finalResults []SearchResult
	for _, result := range combined {
		finalResults = append(finalResults, result)
	}

	sort.Slice(finalResults, func(i, j int) bool {
		return finalResults[i].Score > finalResults[j].Score
	})

	// Format output
	var output []string
	for i, result := range finalResults {
		if limit > 0 && i >= limit {
			break
		}
		output = append(output, fmt.Sprintf("MATCH_%d (%s): %s",
			result.Score, result.SearchType, result.Context))
	}

	if len(output) == 0 {
		return []string{"No prayers found matching the search criteria."}
	}

	return output
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// Legacy search prayers by keywords
func searchPrayersByKeywords(db Database, referenceLanguage string, keywords []string, limit int) []string {
	// Cap the number of keywords to prevent argument list issues
	const maxKeywords = 50
	if len(keywords) > maxKeywords {
		log.Printf("Too many keywords (%d), capping at %d", len(keywords), maxKeywords)
		keywords = keywords[:maxKeywords]
	}
	phelpsContext := buildPhelpsContext(db, referenceLanguage)

	type phelpsScore struct {
		phelps  string
		context string
		score   int
	}

	var results []phelpsScore

	for phelps, context := range phelpsContext {
		score := 0
		contextLower := strings.ToLower(context)

		for _, keyword := range keywords {
			keywordLower := strings.ToLower(strings.TrimSpace(keyword))
			if strings.Contains(contextLower, keywordLower) {
				score++
			}
		}

		if score > 0 {
			results = append(results, phelpsScore{phelps, context, score})
		}
	}

	// Sort by score (highest first)
	sort.Slice(results, func(i, j int) bool {
		return results[i].score > results[j].score
	})

	// Return top results up to limit
	var output []string
	for i, result := range results {
		if limit > 0 && i >= limit {
			break
		}
		output = append(output, fmt.Sprintf("MATCH_%d: %s", result.score, result.context))
	}

	if len(output) == 0 {
		return []string{"No prayers found matching those keywords."}
	}

	return output
}

// Search prayers by length range
func searchPrayersByLength(db Database, referenceLanguage string, minWords, maxWords int, limit int) []string {
	phelpsContext := buildPhelpsContext(db, referenceLanguage)

	var results []string
	count := 0

	for _, context := range phelpsContext {
		// Extract word count from context string like "[60 words, 347 chars]"
		if strings.Contains(context, "words") {
			parts := strings.Split(context, "[")
			if len(parts) > 1 {
				wordPart := strings.Split(parts[1], " words")[0]
				if wordCount, err := strconv.Atoi(wordPart); err == nil {
					if wordCount >= minWords && wordCount <= maxWords {
						results = append(results, context)
						count++
						if limit > 0 && count >= limit {
							break
						}
					}
				}
			}
		}
	}

	if len(results) == 0 {
		// Add automatic failure note
		addSessionNote(referenceLanguage, "FAILURE",
			fmt.Sprintf("SEARCH_LENGTH:%d-%d returned no matches", minWords, maxWords), "", 0.0)
		return []string{fmt.Sprintf("No prayers found with %d-%d words.", minWords, maxWords)}
	}

	return results
}

// Search prayers by opening text similarity
func searchPrayersByOpening(db Database, referenceLanguage string, openingText string, limit int) []string {
	phelpsContext := buildPhelpsContext(db, referenceLanguage)
	openingLower := strings.ToLower(openingText)

	type phelpsMatch struct {
		context    string
		similarity int
	}

	var matches []phelpsMatch

	for _, context := range phelpsContext {
		// Extract opening text from context
		if strings.Contains(context, "Opening: \"") {
			parts := strings.Split(context, "Opening: \"")
			if len(parts) > 1 {
				opening := strings.Split(parts[1], "\"")[0]
				openingCtxLower := strings.ToLower(opening)

				// Simple similarity score based on common words
				openingWords := strings.Fields(openingLower)
				ctxWords := strings.Fields(openingCtxLower)

				commonWords := 0
				for _, word := range openingWords {
					for _, ctxWord := range ctxWords {
						if word == ctxWord && len(word) > 2 {
							commonWords++
							break
						}
					}
				}

				if commonWords > 0 {
					matches = append(matches, phelpsMatch{context, commonWords})
				}
			}
		}
	}

	// Sort by similarity (highest first)
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].similarity > matches[j].similarity
	})

	var results []string
	for i, match := range matches {
		if limit > 0 && i >= limit {
			break
		}
		results = append(results, fmt.Sprintf("SIMILARITY_%d: %s", match.similarity, match.context))
	}

	if len(results) == 0 {
		return []string{"No prayers found with similar opening text."}
	}

	return results
}

// sanitizeKeywords cleans and limits keyword lists to prevent argument overflow
func sanitizeKeywords(keywordStr string) []string {
	keywords := strings.Split(keywordStr, ",")
	var cleanKeywords []string
	const maxKeywords = 50

	for i, keyword := range keywords {
		if i >= maxKeywords {
			log.Printf("Too many keywords in list (%d), capping at %d", len(keywords), maxKeywords)
			break
		}
		trimmed := strings.TrimSpace(keyword)
		if trimmed != "" && len(trimmed) <= 100 { // Also cap individual keyword length
			cleanKeywords = append(cleanKeywords, trimmed)
		}
	}

	return cleanKeywords
}

func processLLMFunctionCall(db Database, referenceLanguage string, functionCall string) []string {
	return processLLMFunctionCallWithMode(db, referenceLanguage, functionCall, "")
}

func processLLMFunctionCallWithMode(db Database, referenceLanguage string, functionCall string, operationMode string) []string {
	functionCall = strings.TrimSpace(functionCall)

	// Use extensible system to find and execute the function
	for _, handler := range registeredFunctions {
		if handler.Validate(functionCall) && (operationMode == "" || handler.IsEnabledForMode(operationMode)) {
			return handler.Execute(db, referenceLanguage, functionCall)
		}
	}

	// Legacy handling for functions not yet migrated
	if strings.HasPrefix(functionCall, "SEARCH_KEYWORDS:") {
		keywordStr := strings.TrimPrefix(functionCall, "SEARCH_KEYWORDS:")
		cleanKeywords := sanitizeKeywords(keywordStr)

		results := searchPrayersByKeywords(db, referenceLanguage, cleanKeywords, 10)

		// Add automatic note if search was successful
		if len(results) > 0 && !strings.Contains(results[0], "No prayers found") {
			addSessionNote(referenceLanguage, "SUCCESS",
				fmt.Sprintf("SEARCH_KEYWORDS:%s found %d matches", keywordStr, len(results)), "", 0.0)
		} else {
			addSessionNote(referenceLanguage, "FAILURE",
				fmt.Sprintf("SEARCH_KEYWORDS:%s returned no matches", keywordStr), "", 0.0)
		}

		return results
	}

	if strings.HasPrefix(functionCall, "SEARCH_LENGTH:") {
		lengthStr := strings.TrimPrefix(functionCall, "SEARCH_LENGTH:")
		parts := strings.Split(lengthStr, "-")
		if len(parts) == 2 {
			if minWords, err1 := strconv.Atoi(strings.TrimSpace(parts[0])); err1 == nil {
				if maxWords, err2 := strconv.Atoi(strings.TrimSpace(parts[1])); err2 == nil {
					return searchPrayersByLength(db, referenceLanguage, minWords, maxWords, 10)
				}
			}
		}
		return []string{"Invalid length format. Use: SEARCH_LENGTH:min-max (e.g., SEARCH_LENGTH:50-100)"}
	}

	if strings.HasPrefix(functionCall, "SEARCH_OPENING:") {
		openingText := strings.TrimPrefix(functionCall, "SEARCH_OPENING:")
		results := searchPrayersByOpening(db, referenceLanguage, openingText, 10)

		// Add automatic note if search was successful
		if len(results) > 0 && !strings.Contains(results[0], "No prayers found") {
			addSessionNote(referenceLanguage, "SUCCESS",
				fmt.Sprintf("SEARCH_OPENING:'%s' found %d matches", openingText, len(results)), "", 0.0)
		} else {
			addSessionNote(referenceLanguage, "FAILURE",
				fmt.Sprintf("SEARCH_OPENING:'%s' returned no matches", openingText), "", 0.0)
		}

		return results
	}

	if strings.HasPrefix(functionCall, "GET_FULL_TEXT:") {
		phelpsCode := strings.TrimSpace(strings.TrimPrefix(functionCall, "GET_FULL_TEXT:"))
		return getFullTextByPhelps(db, referenceLanguage, phelpsCode)
	}

	if strings.HasPrefix(functionCall, "GET_FOCUS_TEXT:") {
		args := strings.TrimSpace(strings.TrimPrefix(functionCall, "GET_FOCUS_TEXT:"))
		return getFocusTextByPhelps(db, referenceLanguage, args)
	}

	if strings.HasPrefix(functionCall, "GET_PARTIAL_TEXT:") {
		args := strings.TrimSpace(strings.TrimPrefix(functionCall, "GET_PARTIAL_TEXT:"))
		return getPartialTextByPhelps(db, referenceLanguage, args)
	}

	if strings.HasPrefix(functionCall, "ADD_NOTE:") {
		args := strings.TrimSpace(strings.TrimPrefix(functionCall, "ADD_NOTE:"))
		parts := strings.SplitN(args, ",", 2)
		if len(parts) == 2 {
			noteType := strings.TrimSpace(parts[0])
			content := strings.TrimSpace(parts[1])

			// Validate note type
			validTypes := []string{"SUCCESS", "FAILURE", "PATTERN", "STRATEGY", "TIP"}
			isValid := false
			for _, validType := range validTypes {
				if strings.ToUpper(noteType) == validType {
					noteType = validType
					isValid = true
					break
				}
			}

			if !isValid {
				return []string{fmt.Sprintf("Invalid note type '%s'. Valid types: SUCCESS, FAILURE, PATTERN, STRATEGY, TIP", noteType)}
			}

			addSessionNote(referenceLanguage, noteType, content, "", 0.0)
			return []string{fmt.Sprintf("Note added: [%s] %s", noteType, content)}
		}
		return []string{"Invalid ADD_NOTE format. Use: ADD_NOTE:type,content (e.g., ADD_NOTE:PATTERN,French prayers often use 'Seigneur' for 'Lord')"}
	}

	if strings.HasPrefix(functionCall, "SEARCH_NOTES:") {
		query := strings.TrimSpace(strings.TrimPrefix(functionCall, "SEARCH_NOTES:"))

		// Parse query for filters like "type=PATTERN query text"
		parts := strings.Fields(query)
		var searchQuery, noteType, language string

		for i, part := range parts {
			if strings.HasPrefix(part, "type=") {
				noteType = strings.TrimPrefix(part, "type=")
				parts = append(parts[:i], parts[i+1:]...)
				break
			} else if strings.HasPrefix(part, "lang=") {
				language = strings.TrimPrefix(part, "lang=")
				parts = append(parts[:i], parts[i+1:]...)
				break
			}
		}

		if len(parts) > 0 {
			searchQuery = strings.Join(parts, " ")
		}

		matches := searchSessionNotes(searchQuery, noteType, language)

		if len(matches) == 0 {
			return []string{"No notes found matching your search criteria."}
		}

		var results []string
		results = append(results, fmt.Sprintf("Found %d matching notes:", len(matches)))

		for _, note := range matches {
			timeAgo := time.Since(note.Timestamp).Round(time.Minute)
			results = append(results, fmt.Sprintf("[%s] %s (%v ago): %s",
				note.NoteType, note.Language, timeAgo, note.Content))
		}

		return results
	}

	if strings.HasPrefix(functionCall, "CLEAR_NOTES:") {
		criteria := strings.TrimSpace(strings.TrimPrefix(functionCall, "CLEAR_NOTES:"))

		// Parse criteria like "type=FAILURE" or "older_than=30m" or "lang=fr"
		var noteType, language string
		var olderThan time.Duration

		parts := strings.Split(criteria, " ")
		for _, part := range parts {
			if strings.HasPrefix(part, "type=") {
				noteType = strings.TrimPrefix(part, "type=")
			} else if strings.HasPrefix(part, "lang=") {
				language = strings.TrimPrefix(part, "lang=")
			} else if strings.HasPrefix(part, "older_than=") {
				durationStr := strings.TrimPrefix(part, "older_than=")
				if duration, err := time.ParseDuration(durationStr); err == nil {
					olderThan = duration
				}
			}
		}

		removed := removeSessionNotes(noteType, language, olderThan)

		if removed == 0 {
			return []string{"No notes matched the removal criteria."}
		}

		return []string{fmt.Sprintf("Removed %d notes matching criteria: %s", removed, criteria)}
	}

	if strings.HasPrefix(functionCall, "FINAL_ANSWER:") {
		args := strings.TrimSpace(strings.TrimPrefix(functionCall, "FINAL_ANSWER:"))
		return processFinalAnswer(args)
	}

	if functionCall == "GET_STATS" {
		phelpsContext := buildPhelpsContext(db, referenceLanguage)
		stats := getSessionNotesStats()

		result := []string{
			fmt.Sprintf("Database contains %d prayers with context for matching.", len(phelpsContext)),
			fmt.Sprintf("Current reference language: %s", referenceLanguage),
			fmt.Sprintf("Session notes: %d total", stats["total"]),
		}

		if stats["total"] > 0 {
			result = append(result, "Note breakdown:")
			for noteType := range map[string]bool{"SUCCESS": true, "FAILURE": true, "PATTERN": true, "STRATEGY": true, "TIP": true} {
				key := "type_" + strings.ToLower(noteType)
				if count, exists := stats[key]; exists && count > 0 {
					result = append(result, fmt.Sprintf("  %s: %d", noteType, count))
				}
			}
		}

		return result
	}

	if functionCall == "LIST_REFERENCE_LANGUAGES" {
		return []string{listReferenceLanguages(db)}
	}

	if strings.HasPrefix(functionCall, "EXTEND_ROUNDS:") {
		reasonStr := strings.TrimSpace(strings.TrimPrefix(functionCall, "EXTEND_ROUNDS:"))
		if reasonStr == "" {
			return []string{"Error: EXTEND_ROUNDS requires a reason (e.g., EXTEND_ROUNDS:Making good progress, need more searches to confirm match)"}
		}

		// Check if reason is valid (not just trying to avoid making a decision)
		reasonLower := strings.ToLower(reasonStr)
		if strings.Contains(reasonLower, "don't know") || strings.Contains(reasonLower, "unsure") ||
			strings.Contains(reasonLower, "confused") || strings.Contains(reasonLower, "need help") {
			return []string{"Error: EXTEND_ROUNDS denied. Provide a specific reason about progress made and what you need to verify."}
		}

		// Must mention specific progress or verification needs
		validReasons := []string{"progress", "promising", "candidate", "verify", "confirm", "check", "narrow", "compare"}
		hasValidReason := false
		for _, validWord := range validReasons {
			if strings.Contains(reasonLower, validWord) {
				hasValidReason = true
				break
			}
		}

		if !hasValidReason {
			return []string{"Error: EXTEND_ROUNDS denied. You must explain what progress you've made or what specific verification you need."}
		}

		addSessionNote(referenceLanguage, "STRATEGY",
			fmt.Sprintf("Requested round extension: %s", reasonStr), "", 0.0)

		return []string{fmt.Sprintf("ROUNDS_EXTENDED:10:%s", reasonStr)}
	}

	if strings.HasPrefix(functionCall, "SWITCH_REFERENCE_LANGUAGE:") {
		newRefLang := strings.TrimSpace(strings.TrimPrefix(functionCall, "SWITCH_REFERENCE_LANGUAGE:"))

		// Validate that the language has prayers with Phelps codes
		hasReference := false
		count := 0
		for _, writing := range db.Writing {
			if writing.Language == newRefLang && writing.Phelps != "" && strings.TrimSpace(writing.Phelps) != "" {
				hasReference = true
				count++
			}
		}

		if !hasReference {
			return []string{fmt.Sprintf("Language '%s' has no prayers with Phelps codes. Use LIST_REFERENCE_LANGUAGES to see available options.", newRefLang)}
		}

		addSessionNote(referenceLanguage, "STRATEGY",
			fmt.Sprintf("Switched reference language from %s to %s (%d prayers available)", referenceLanguage, newRefLang, count), "", 0.0)

		return []string{fmt.Sprintf("REFERENCE_LANGUAGE_CHANGED:%s", newRefLang)}
	}

	return []string{fmt.Sprintf("Unknown function. Available functions: %s", generateConciseFunctionList())}
}

// Get full text of a prayer by Phelps code
func getFullTextByPhelps(db Database, referenceLanguage string, phelpsCode string) []string {
	phelpsCode = strings.TrimSpace(phelpsCode)
	if phelpsCode == "" {
		return []string{"Error: No Phelps code provided. Use: GET_FULL_TEXT:AB00001FIR"}
	}

	// Check if multiple codes were provided (comma-separated)
	if strings.Contains(phelpsCode, ",") {
		return []string{"Error: GET_FULL_TEXT only accepts one Phelps code. For multiple codes use: GET_FOCUS_TEXT:keyword,code1,code2,code3"}
	}

	// Find the prayer with the specified Phelps code in the reference language
	for _, writing := range db.Writing {
		if writing.Language == referenceLanguage && writing.Phelps == phelpsCode {
			if writing.Text == "" {
				return []string{fmt.Sprintf("Found Phelps code %s but text is empty.", phelpsCode)}
			}

			// Return the full text with some metadata
			result := fmt.Sprintf("FULL TEXT for %s (%s):\n\n%s", phelpsCode, writing.Name, writing.Text)
			return []string{result}
		}
	}

	// If not found, provide helpful suggestions
	var availablePhelps []string
	for _, writing := range db.Writing {
		if writing.Language == referenceLanguage && writing.Phelps != "" {
			availablePhelps = append(availablePhelps, writing.Phelps)
			if len(availablePhelps) >= 10 { // Limit suggestions to first 10
				break
			}
		}
	}

	if len(availablePhelps) > 0 {
		return []string{fmt.Sprintf("Phelps code '%s' not found. Available codes: %s", phelpsCode, strings.Join(availablePhelps[:min(5, len(availablePhelps))], ", "))}
	}

	return []string{fmt.Sprintf("Phelps code '%s' not found and no reference prayers available.", phelpsCode)}
}

// Get focused text context around a keyword from multiple Phelps codes
func getFocusTextByPhelps(db Database, referenceLanguage string, args string) []string {
	if strings.TrimSpace(args) == "" {
		return []string{"Error: No arguments provided. Use: GET_FOCUS_TEXT:keyword,phelps_code1,phelps_code2"}
	}

	parts := strings.Split(args, ",")
	if len(parts) < 2 {
		return []string{"Error: GET_FOCUS_TEXT requires format: keyword,phelps_code1,phelps_code2,... Special keywords: 'head' for beginning, 'tail' for end"}
	}

	keyword := strings.TrimSpace(parts[0])
	phelpsCodes := parts[1:]

	const maxCodes = 20 // Allow more codes since we're only returning context
	if len(phelpsCodes) > maxCodes {
		log.Printf("Too many Phelps codes requested (%d), capping at %d", len(phelpsCodes), maxCodes)
		phelpsCodes = phelpsCodes[:maxCodes]
	}

	var results []string
	var found []string
	var notFound []string

	for _, code := range phelpsCodes {
		code = strings.TrimSpace(code)
		if code == "" {
			continue
		}

		// Find the prayer
		var foundWriting *Writing
		for _, writing := range db.Writing {
			if writing.Language == referenceLanguage && writing.Phelps == code {
				foundWriting = &writing
				break
			}
		}

		if foundWriting != nil && foundWriting.Text != "" {
			found = append(found, code)
			context := getFocusContext(foundWriting.Text, keyword, foundWriting.Name, code)
			results = append(results, context)
		} else {
			notFound = append(notFound, code)
		}
	}

	// Add summary at the beginning
	if len(found) > 0 {
		summary := fmt.Sprintf("FOCUS TEXT for keyword '%s' in %d prayer(s): %s\n",
			keyword, len(found), strings.Join(found, ", "))
		results = append([]string{summary}, results...)
	}

	if len(notFound) > 0 {
		results = append(results, fmt.Sprintf("Not found: %s", strings.Join(notFound, ", ")))
	}

	if len(results) == 0 || (len(results) == 1 && strings.Contains(results[0], "Not found")) {
		return []string{fmt.Sprintf("No valid Phelps codes found for focus text search")}
	}

	return results
}

// Get context around a keyword in text (or head/tail)
func getFocusContext(text, keyword, prayerName, phelpsCode string) string {
	const contextLines = 3 // Lines of context around the keyword

	if keyword == "head" {
		// Return first few lines
		lines := strings.Split(text, "\n")
		endIdx := min(contextLines*2, len(lines))
		headText := strings.Join(lines[:endIdx], "\n")
		if len(lines) > endIdx {
			headText += "\n..."
		}
		return fmt.Sprintf("%s (%s) - HEAD:\n%s\n", phelpsCode, prayerName, headText)
	}

	if keyword == "tail" {
		// Return last few lines
		lines := strings.Split(text, "\n")
		startIdx := max(0, len(lines)-contextLines*2)
		tailText := strings.Join(lines[startIdx:], "\n")
		if startIdx > 0 {
			tailText = "...\n" + tailText
		}
		return fmt.Sprintf("%s (%s) - TAIL:\n%s\n", phelpsCode, prayerName, tailText)
	}

	// Search for keyword context
	lines := strings.Split(text, "\n")
	keywordLower := strings.ToLower(keyword)

	for i, line := range lines {
		if strings.Contains(strings.ToLower(line), keywordLower) {
			// Found keyword, get context
			startIdx := max(0, i-contextLines)
			endIdx := min(len(lines), i+contextLines+1)

			contextLines := lines[startIdx:endIdx]

			// Mark the line with the keyword
			for j, contextLine := range contextLines {
				if startIdx+j == i {
					contextLines[j] = ">>> " + contextLine + " <<<"
				}
			}

			contextText := strings.Join(contextLines, "\n")
			return fmt.Sprintf("%s (%s) - CONTEXT for '%s':\n%s\n", phelpsCode, prayerName, keyword, contextText)
		}
	}

	// Keyword not found
	return fmt.Sprintf("%s (%s) - Keyword '%s' not found\n", phelpsCode, prayerName, keyword)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Get partial text of a prayer by Phelps code with various paging options
func getPartialTextByPhelps(db Database, referenceLanguage string, args string) []string {
	parts := strings.Split(args, ",")
	if len(parts) < 2 {
		return []string{"Error: GET_PARTIAL_TEXT requires format: phelps_code,start-end OR phelps_code,from:word,to:word OR phelps_code,from:word OR phelps_code,to:word"}
	}

	phelpsCode := strings.TrimSpace(parts[0])
	if phelpsCode == "" {
		return []string{"Error: No Phelps code provided"}
	}

	// Find the prayer with the specified Phelps code
	var prayerText string
	var prayerName string
	for _, writing := range db.Writing {
		if writing.Language == referenceLanguage && writing.Phelps == phelpsCode {
			prayerText = writing.Text
			prayerName = writing.Name
			break
		}
	}

	if prayerText == "" {
		return []string{fmt.Sprintf("Phelps code '%s' not found", phelpsCode)}
	}

	// Parse the range/search parameters
	rangeParam := strings.TrimSpace(parts[1])

	// Handle character range format: start-end
	if strings.Contains(rangeParam, "-") && !strings.Contains(rangeParam, ":") {
		rangeParts := strings.Split(rangeParam, "-")
		if len(rangeParts) == 2 {
			start, err1 := strconv.Atoi(strings.TrimSpace(rangeParts[0]))
			end, err2 := strconv.Atoi(strings.TrimSpace(rangeParts[1]))

			if err1 != nil || err2 != nil {
				return []string{"Error: Invalid character range format. Use: start-end (e.g., 100-500)"}
			}

			if start < 0 {
				start = 0
			}
			if end > len(prayerText) {
				end = len(prayerText)
			}
			if start >= end {
				return []string{"Error: Start position must be less than end position"}
			}

			excerpt := prayerText[start:end]
			return []string{fmt.Sprintf("PARTIAL TEXT for %s (%s) [chars %d-%d]:\n\n%s", phelpsCode, prayerName, start, end, excerpt)}
		}
	}

	// Handle search term format: from:word, to:word, from:word,to:word
	var fromWord, toWord string

	// Parse additional parameters
	for i := 1; i < len(parts); i++ {
		param := strings.TrimSpace(parts[i])
		if strings.HasPrefix(param, "from:") {
			fromWord = strings.TrimSpace(strings.TrimPrefix(param, "from:"))
		} else if strings.HasPrefix(param, "to:") {
			toWord = strings.TrimSpace(strings.TrimPrefix(param, "to:"))
		}
	}

	// Apply search term filtering
	startIdx := 0
	endIdx := len(prayerText)

	if fromWord != "" {
		idx := strings.Index(strings.ToLower(prayerText), strings.ToLower(fromWord))
		if idx != -1 {
			startIdx = idx
		} else {
			return []string{fmt.Sprintf("Error: Start word '%s' not found in prayer text", fromWord)}
		}
	}

	if toWord != "" {
		searchText := prayerText[startIdx:]
		idx := strings.Index(strings.ToLower(searchText), strings.ToLower(toWord))
		if idx != -1 {
			endIdx = startIdx + idx + len(toWord)
		} else {
			return []string{fmt.Sprintf("Error: End word '%s' not found in prayer text", toWord)}
		}
	}

	if startIdx >= endIdx {
		return []string{"Error: Start position is after end position"}
	}

	excerpt := prayerText[startIdx:endIdx]

	var rangeDesc string
	if fromWord != "" && toWord != "" {
		rangeDesc = fmt.Sprintf("from '%s' to '%s'", fromWord, toWord)
	} else if fromWord != "" {
		rangeDesc = fmt.Sprintf("from '%s' to end", fromWord)
	} else if toWord != "" {
		rangeDesc = fmt.Sprintf("from start to '%s'", toWord)
	}

	return []string{fmt.Sprintf("PARTIAL TEXT for %s (%s) [%s]:\n\n%s", phelpsCode, prayerName, rangeDesc, excerpt)}
}

// Clean up invalid Phelps codes from the database
func cleanupInvalidPhelpsCode() (int, error) {
	// Define regex pattern for valid Phelps codes
	// Valid formats: AB12345, AB12345XYZ, ABU1234XYZ, BH12345XYZ, BHU1234XYZ, BB12345XYZ, BBU1234
	validPattern := `^[AB][BH]?U?[0-9]{4,5}[A-Z]*$`

	// Find invalid codes in match_attempts
	query1 := fmt.Sprintf("SELECT id, phelps_code FROM match_attempts WHERE phelps_code != 'UNKNOWN' AND phelps_code != '' AND phelps_code NOT REGEXP '%s'", validPattern)
	output1, err := execDoltQueryCSV(query1)
	if err != nil {
		return 0, fmt.Errorf("failed to query match_attempts: %w", err)
	}

	var invalidAttempts []struct {
		ID         string
		PhelpsCode string
	}

	// Parse dolt CSV output
	lines1 := strings.Split(strings.TrimSpace(string(output1)), "\n")
	if len(lines1) > 1 { // Skip header
		for i := 1; i < len(lines1); i++ {
			parts := strings.Split(lines1[i], ",")
			if len(parts) >= 2 {
				id := strings.Trim(parts[0], " \"")
				code := strings.Trim(parts[1], " \"")
				if id != "" && code != "" {
					invalidAttempts = append(invalidAttempts, struct {
						ID         string
						PhelpsCode string
					}{id, code})
				}
			}
		}
	}

	// Find invalid codes in writings
	query2 := fmt.Sprintf("SELECT version, phelps FROM writings WHERE phelps != '' AND phelps IS NOT NULL AND phelps NOT REGEXP '%s'", validPattern)
	output2, err := execDoltQueryCSV(query2)
	if err != nil {
		return 0, fmt.Errorf("failed to query writings: %w", err)
	}

	var invalidWritings []struct {
		Version    string
		PhelpsCode string
	}

	// Parse dolt CSV output
	lines2 := strings.Split(strings.TrimSpace(string(output2)), "\n")
	if len(lines2) > 1 { // Skip header
		for i := 1; i < len(lines2); i++ {
			parts := strings.Split(lines2[i], ",")
			if len(parts) >= 2 {
				version := strings.Trim(parts[0], " \"")
				code := strings.Trim(parts[1], " \"")
				if version != "" && code != "" {
					invalidWritings = append(invalidWritings, struct {
						Version    string
						PhelpsCode string
					}{version, code})
				}
			}
		}
	}

	totalCleaned := 0

	// Clean up match_attempts
	if len(invalidAttempts) > 0 {
		fmt.Printf("Found %d invalid codes in match_attempts:\n", len(invalidAttempts))
		for _, attempt := range invalidAttempts {
			fmt.Printf("  - ID %s: '%s'\n", attempt.ID, attempt.PhelpsCode)
		}

		for _, attempt := range invalidAttempts {
			updateQuery1 := fmt.Sprintf("UPDATE match_attempts SET phelps_code = '', result_type = 'failure', reasoning = CONCAT('Invalid Phelps code reset: %s - ', reasoning) WHERE id = %s", attempt.PhelpsCode, attempt.ID)
			if err := execDoltRun("sql", "-q", updateQuery1); err != nil {
				return totalCleaned, fmt.Errorf("failed to update match_attempt %s: %w", attempt.ID, err)
			}
			totalCleaned++
		}
	}

	// Clean up writings
	if len(invalidWritings) > 0 {
		fmt.Printf("Found %d invalid codes in writings:\n", len(invalidWritings))
		for _, writing := range invalidWritings {
			fmt.Printf("  - Version %s: '%s'\n", writing.Version, writing.PhelpsCode)
		}

		for _, writing := range invalidWritings {
			updateQuery2 := fmt.Sprintf("UPDATE writings SET phelps = '' WHERE version = '%s'", writing.Version)
			if err := execDoltRun("sql", "-q", updateQuery2); err != nil {
				return totalCleaned, fmt.Errorf("failed to update writing %s: %w", writing.Version, err)
			}
			totalCleaned++
		}
	}

	return totalCleaned, nil
}

// Check if a Phelps code exists in the database
func phelpsCodeExists(db Database, referenceLanguage string, phelpsCode string) bool {
	for _, writing := range db.Writing {
		if writing.Language == referenceLanguage && writing.Phelps == phelpsCode {
			return true
		}
	}
	return false
}

// Process final answer from tool call
func processFinalAnswer(args string) []string {
	parts := strings.Split(args, ",")
	if len(parts) < 3 {
		return []string{"Error: FINAL_ANSWER requires format: phelps_code,confidence,reasoning (e.g., FINAL_ANSWER:AB00001FIR,85,This prayer matches based on distinctive phrases)"}
	}

	phelpsCode := strings.TrimSpace(parts[0])
	confidenceStr := strings.TrimSpace(parts[1])
	reasoning := strings.TrimSpace(strings.Join(parts[2:], ","))

	confidence, err := strconv.ParseFloat(confidenceStr, 64)
	if err != nil {
		return []string{fmt.Sprintf("Error: Invalid confidence value '%s'. Must be a number 0-100", confidenceStr)}
	}

	// Convert percentage to decimal if needed
	if confidence > 1.0 {
		confidence = confidence / 100.0
	}

	if confidence < 0 || confidence > 1.0 {
		return []string{"Error: Confidence must be between 0-100 (or 0.0-1.0)"}
	}

	if phelpsCode == "" {
		return []string{"Error: Phelps code cannot be empty"}
	}

	if reasoning == "" {
		return []string{"Error: Reasoning cannot be empty"}
	}

	return []string{fmt.Sprintf("FINAL ANSWER RECEIVED:\nPhelps: %s\nConfidence: %.0f%%\nReasoning: %s", phelpsCode, confidence*100, reasoning)}
}

// Prepare legacy header with all prayer contexts (old method)
func prepareLLMHeaderLegacy(db Database, targetLanguage, referenceLanguage string) string {
	if targetLanguage == "" {
		targetLanguage = "English"
	}
	if referenceLanguage == "" {
		referenceLanguage = "English"
	}

	// Build rich context for all Phelps codes
	phelpsContext := buildPhelpsContext(db, referenceLanguage)

	var phelpsInfo []string
	for _, context := range phelpsContext {
		phelpsInfo = append(phelpsInfo, context)
	}
	sort.Strings(phelpsInfo)

	header := fmt.Sprintf(`You are an expert in Bahá'í writings and prayers. Your task is to match a prayer text in %s to known Phelps codes.

Known Phelps codes with detailed context (reference: %s):

%s

Instructions:
1. Analyze the provided prayer text in %s
2. Compare it with the context provided above (opening lines, key words, length, etc.)
3. Match it to the most appropriate Phelps code based on content similarity
4. Provide a confidence score (0-100%%)
5. Give detailed reasoning explaining the match

Response format:
Phelps: [CODE]
Confidence: [PERCENTAGE]
Reasoning: [Your detailed explanation comparing the text with the matched prayer's characteristics]

If you cannot find a match with reasonable confidence (>70%%), respond with:
Phelps: UNKNOWN
Confidence: 0
Reasoning: [Explanation of why no match was found, mentioning what you looked for]

`, targetLanguage, referenceLanguage, strings.Join(phelpsInfo, "\n"), targetLanguage)

	return header
}

// Prepare interactive header for LLM calls
func prepareLLMHeader(db Database, targetLanguage, referenceLanguage string) string {
	return prepareLLMHeaderWithMode(db, targetLanguage, referenceLanguage, "match")
}

func prepareLLMHeaderWithMode(db Database, targetLanguage, referenceLanguage string, operationMode string) string {
	if targetLanguage == "" {
		targetLanguage = "English"
	}
	if referenceLanguage == "" {
		referenceLanguage = "English"
	}

	phelpsContext := buildPhelpsContext(db, referenceLanguage)

	var modeGuidance string
	switch operationMode {
	case "match":
		modeGuidance = `MODE: MATCH ONLY
🎯 GOAL: Find existing Phelps codes. Use UNKNOWN if no match found.

SIMPLE WORKFLOW:
1. SEARCH:keywords,opening_phrase,length_range (e.g., SEARCH:lord,god,O Lord my God,100-200)
2. GET_FULL_TEXT:phelps_code (check top candidates)
3. FINAL_ANSWER:phelps_code,confidence,reasoning OR UNKNOWN if no match`

	case "match-add":
		modeGuidance = `MODE: MATCH-ADD
🎯 GOAL: Try matching first. If no match found, create new code from inventory.

SIMPLE WORKFLOW:
Step 1 - TRY MATCHING:
1. SEARCH:keywords,opening_phrase,length_range
2. GET_FULL_TEXT:phelps_code (check candidates)
3. If good match: FINAL_ANSWER:phelps_code,confidence,reasoning

Step 2 - IF NO MATCH (confidence <70):
1. SEARCH_INVENTORY:keywords,language
2. CHECK_TAG:PIN (see existing tags)
3. ADD_NEW_PRAYER:new_code,confidence,reasoning
4. FINAL_ANSWER:new_code,confidence,reasoning`

	case "add-only":
		modeGuidance = `MODE: ADD-ONLY
🎯 GOAL: Skip matching, create new codes from inventory only.
⚠️  WORKS BEST: English, Arabic, Persian (limited other languages)

SIMPLE WORKFLOW:
1. SEARCH_INVENTORY:keywords,language
2. CHECK_TAG:PIN (see what tags exist)
3. ADD_NEW_PRAYER:new_code,confidence,reasoning
4. FINAL_ANSWER:new_code,confidence,reasoning

❌ DO NOT use SEARCH functions - go straight to inventory!`

	case "translit":
		modeGuidance = `MODE: TRANSLITERATION
🎯 GOAL: Match original Arabic/Persian text, then check/fix transliteration.

SIMPLE WORKFLOW:
1. SEARCH:keywords,opening_phrase,length_range (match the ORIGINAL text)
2. GET_FULL_TEXT:phelps_code (verify match)
3. CHECK_TRANSLIT_CONSISTENCY:phelps_code (check transliteration quality)
4. If transliteration needs fixing: CORRECT_TRANSLITERATION:phelps_code,confidence,corrected_text
5. FINAL_ANSWER:phelps_code,confidence,reasoning

💡 KEY: Match original text first, then check/fix transliteration quality`
	}

	// Get mode-specific function list and descriptions
	availableFunctions := generateConciseFunctionListForMode(operationMode)
	functionDescriptions := generateFunctionDescriptionsForMode(operationMode)

	header := fmt.Sprintf(`TASK: Match prayer text in %s to Phelps code. RESPOND ONLY WITH FUNCTION CALLS.

%s

Current reference language: %s (use SWITCH_REFERENCE_LANGUAGE if needed)

🚨 CRITICAL RULES:
1. RESPOND ONLY WITH FUNCTION CALLS - NO text, questions, or explanations
2. EVERY response MUST contain valid function calls
3. START IMMEDIATELY - don't ask what to do
4. Maximum 10 rounds - be efficient
5. If unsure, do more searches then FINAL_ANSWER

📋 AVAILABLE FUNCTIONS FOR THIS MODE:
%s

%s

⚡ EFFICIENCY TIPS:
- Combine multiple criteria in ONE search (keywords + opening + length)
- Use EXTEND_ROUNDS:reason if making progress but need more time
- Don't do separate searches - combine everything in one SEARCH call

SEARCH LANGUAGE: All searches use %s terms only
CONFIDENCE: Use >70 for match, UNKNOWN if <70

PHELPS CODE EXAMPLES (for context):
%s`, targetLanguage, modeGuidance, referenceLanguage, availableFunctions, functionDescriptions, referenceLanguage, phelpsContext)

	return header
}

// Interactive LLM conversation with function call support using Ollama API
func callLLMInteractive(db Database, currentReferenceLanguage string, prompt string, useGemini bool, textLength int, maxRoundsParam int, operationMode string) (LLMResponse, error) {
	// Store the current reference language for potential switching
	activeReferenceLanguage := currentReferenceLanguage
	maxRounds := maxRoundsParam // Maximum conversation rounds to prevent infinite loops
	originalMaxRounds := maxRounds
	roundsExtended := 0

	// Check if we should skip Gemini due to quota exceeded or too many arg errors
	if useGemini && atomic.LoadInt32(&geminiQuotaExceeded) == 1 {
		useGemini = false
		fmt.Printf("    ⚠️  Gemini quota exceeded - using only Ollama for this prayer\n")
		log.Printf("Gemini quota exceeded - using only Ollama")
	}
	if useGemini && atomic.LoadInt32(&geminiArgErrors) >= 2 {
		useGemini = false
		fmt.Printf("    ⚠️  Too many Gemini argument errors - using only Ollama for this prayer\n")
		log.Printf("Too many Gemini argument errors - using only Ollama")
	}

	fmt.Printf("    🤖 Starting interactive LLM conversation...\n")

	// Initialize conversation with system prompt
	messages := []OllamaMessage{
		{Role: "user", Content: prompt},
	}

	for round := 1; round <= maxRounds; round++ {
		fmt.Printf("    📝 Round %d: Calling LLM...\n", round)

		var rawResponse string
		var err error

		if useGemini {
			// Try Gemini first, fallback to Ollama
			geminiResponse, geminiErr := CallGemini(messages)
			if geminiErr == nil && strings.TrimSpace(geminiResponse) != "" {
				rawResponse = geminiResponse
			} else {
				// Check if this is a quota exceeded error
				if geminiErr != nil {
					errorStr := strings.ToLower(geminiErr.Error())
					if strings.Contains(errorStr, "quota") && (strings.Contains(errorStr, "exceeded") || strings.Contains(errorStr, "exhausted")) {
						atomic.StoreInt32(&geminiQuotaExceeded, 1)
						fmt.Printf("    ⚠️  Gemini quota exceeded - switching to Ollama for remaining requests\n")
						log.Printf("Gemini quota exceeded - continuing with Ollama only")
						useGemini = false // Disable Gemini for remaining rounds
					}
					log.Printf("2025/09/22 00:47:04 Gemini returned empty/invalid response (PhelpsCode empty), falling back to Ollama")
					truncatedResponse := truncateAndStore(geminiErr.Error(), "Gemini")
					log.Printf("2025/09/22 00:47:04 Gemini raw response: %q", truncatedResponse)
				}

				// Fallback to Ollama API
				rawResponse, err = CallOllamaWithMessages(messages, textLength)
				if err != nil {
					fmt.Printf("    ❌ Both LLM calls failed: Gemini: %v, Ollama: %v\n", geminiErr, err)
					return LLMResponse{}, fmt.Errorf("both LLM services failed: %v", err)
				}
			}
		} else {
			// Use Ollama API directly
			rawResponse, err = CallOllamaWithMessages(messages, textLength)
			if err != nil {
				fmt.Printf("    ❌ Ollama API call failed: %v\n", err)
				return LLMResponse{}, err
			}
		}

		// Show LLM response
		fmt.Printf("    💭 LLM Response:\n")
		lines := strings.Split(rawResponse, "\n")
		for _, line := range lines {
			if strings.TrimSpace(line) != "" {
				fmt.Printf("       %s\n", line)
			}
		}

		// Add LLM response to conversation
		messages = append(messages, OllamaMessage{Role: "assistant", Content: rawResponse})

		// Parse function calls (supports multiple formats)
		functionCalls, invalidCalls := extractAllFunctionCalls(rawResponse)

		// Check for FINAL_ANSWER function call
		var finalAnswerCall string
		var otherCalls []string
		for _, call := range functionCalls {
			if strings.HasPrefix(call, "FINAL_ANSWER:") {
				finalAnswerCall = call
			} else {
				otherCalls = append(otherCalls, call)
			}
		}

		// If we have a FINAL_ANSWER call, process it
		if finalAnswerCall != "" {
			results := processLLMFunctionCall(db, activeReferenceLanguage, finalAnswerCall)
			if len(results) > 0 && strings.HasPrefix(results[0], "FINAL ANSWER RECEIVED:") {
				// Parse the final answer from the result
				lines := strings.Split(results[0], "\n")
				var phelpsCode, reasoning string
				var confidence float64

				for _, line := range lines {
					if strings.HasPrefix(line, "Phelps: ") {
						phelpsCode = strings.TrimSpace(strings.TrimPrefix(line, "Phelps: "))
					} else if strings.HasPrefix(line, "Confidence: ") {
						confStr := strings.TrimSpace(strings.TrimPrefix(line, "Confidence: "))
						confStr = strings.TrimSuffix(confStr, "%")
						if conf, err := strconv.ParseFloat(confStr, 64); err == nil {
							// Convert percentage to decimal if needed (e.g., 90 -> 0.90)
							if conf > 1.0 {
								confidence = conf / 100.0
							} else {
								confidence = conf
							}
						}
					} else if strings.HasPrefix(line, "Reasoning: ") {
						reasoning = strings.TrimSpace(strings.TrimPrefix(line, "Reasoning: "))
					}
				}

				fmt.Printf("    ✅ Valid final answer received via tool call!\n")

				// Add a success note if we got a valid match
				if phelpsCode != "UNKNOWN" && confidence > 0.7 {
					addSessionNote(activeReferenceLanguage, "SUCCESS",
						fmt.Sprintf("Successfully matched prayer using interactive search"),
						phelpsCode, confidence)

					// Also add a pattern note about the successful strategy
					addSessionNote(activeReferenceLanguage, "PATTERN",
						fmt.Sprintf("Interactive search workflow successful for %s prayers", activeReferenceLanguage), "", 0.0)
				}

				return LLMResponse{
					PhelpsCode: phelpsCode,
					Confidence: confidence,
					Reasoning:  reasoning,
				}, nil
			}
		}

		// Parse the response for legacy final answer format (fallback)
		parsedResponse := parseLLMResponse(rawResponse)
		finalAnswer := validateFinalAnswer(rawResponse, parsedResponse, db, activeReferenceLanguage)

		if finalAnswer.IsValid && len(otherCalls) == 0 && len(invalidCalls) == 0 {
			fmt.Printf("    ✅ Valid final answer received!\n")

			// Add a success note if we got a valid match
			if finalAnswer.Response.PhelpsCode != "UNKNOWN" && finalAnswer.Response.Confidence > 0.7 {
				addSessionNote(activeReferenceLanguage, "SUCCESS",
					fmt.Sprintf("Successfully matched prayer using legacy format"),
					finalAnswer.Response.PhelpsCode, finalAnswer.Response.Confidence)

				// Add strategy note about legacy format working
				addSessionNote(activeReferenceLanguage, "STRATEGY",
					fmt.Sprintf("Legacy format worked well for %s prayers", activeReferenceLanguage), "", 0.0)
			}

			return finalAnswer.Response, nil
		}

		// Handle invalid function calls
		if len(invalidCalls) > 0 {
			roundsLeft := maxRounds - round
			fmt.Printf("    ⚠️  Found %d invalid function call(s) (%d rounds left):\n", len(invalidCalls), roundsLeft)
			systemMessage := fmt.Sprintf("ERROR - Invalid function calls detected (Round %d/%d, %d remaining", round, maxRounds, roundsLeft)
			if roundsExtended > 0 {
				systemMessage += fmt.Sprintf(", extended by %d", roundsExtended)
			}
			systemMessage += "):\n"

			for _, invalidCall := range invalidCalls {
				fmt.Printf("       ❌ %s\n", invalidCall.Error)
				systemMessage += fmt.Sprintf("ERROR: %s\n", invalidCall.Error)
			}

			systemMessage += "Valid function formats (USE COMBINED SEARCHES!):\n"
			systemMessage += generateFunctionHelp()
			systemMessage += "\nIMPORTANT: Use ONE combined SEARCH, not multiple separate searches!\n"
			if roundsLeft <= 2 && roundsExtended == 0 {
				systemMessage += "WARNING: FEW ROUNDS LEFT - USE FINAL_ANSWER SOON OR EXTEND_ROUNDS!\n"
			} else if roundsLeft <= 2 {
				systemMessage += "WARNING: FEW ROUNDS LEFT - USE FINAL_ANSWER SOON!\n"
			}
			systemMessage += "Please correct the function call format and try again.\n\n"

			messages = append(messages, OllamaMessage{Role: "user", Content: systemMessage})
			fmt.Printf("    🔄 Asking LLM to correct function call syntax (%d rounds left)...\n", roundsLeft)
			continue
		}

		if len(functionCalls) == 0 && !finalAnswer.IsValid {
			// No valid function calls and no valid final answer
			fmt.Printf("    ❌ No valid function calls or final answer found\n")
			roundsLeft := maxRounds - round
			systemMessage := "CRITICAL ERROR - YOU ARE NOT FOLLOWING INSTRUCTIONS!\n\n"
			systemMessage += "DO NOT GIVE EXPLANATIONS OR CONVERSATIONAL RESPONSES!\n"
			systemMessage += "YOU MUST RESPOND WITH FUNCTION CALLS ONLY!\n\n"
			systemMessage += fmt.Sprintf("CURRENT STATUS: Round %d/%d, %d remaining", round, maxRounds, roundsLeft)
			if roundsExtended > 0 {
				systemMessage += fmt.Sprintf(", extended by %d", roundsExtended)
			}
			systemMessage += " - OPTIMIZE YOUR STRATEGY!\n\n"
			systemMessage += "REQUIRED FORMAT:\n"
			systemMessage += "Use any of these functions:\n"
			systemMessage += generateFunctionHelp()
			systemMessage += "\n"
			if roundsLeft <= 2 && roundsExtended == 0 {
				systemMessage += "WARNING: FEW ROUNDS LEFT - USE FINAL_ANSWER SOON OR EXTEND_ROUNDS!\n"
			} else if roundsLeft <= 2 {
				systemMessage += "WARNING: FEW ROUNDS LEFT - USE FINAL_ANSWER SOON!\n"
			}
			systemMessage += "RESPOND NOW WITH VALID FUNCTION CALLS - NO OTHER TEXT ALLOWED!"

			messages = append(messages, OllamaMessage{Role: "user", Content: systemMessage})
			fmt.Printf("    🔄 Demanding structured response from LLM (%d rounds left)...\n", roundsLeft)
			continue
		}

		// Process valid function calls
		if len(functionCalls) > 0 {
			roundsLeft := maxRounds - round
			fmt.Printf("    🔍 Processing %d function call(s) (%d rounds left):\n", len(functionCalls), roundsLeft)
			systemMessage := fmt.Sprintf("Function results (Round %d/%d, %d remaining", round, maxRounds, roundsLeft)
			if roundsExtended > 0 {
				systemMessage += fmt.Sprintf(", extended by %d", roundsExtended)
			}
			systemMessage += "):\n"

			for _, functionCall := range functionCalls {
				fmt.Printf("       📞 %s\n", functionCall)
				results := processLLMFunctionCallWithMode(db, activeReferenceLanguage, functionCall, operationMode)

				// Check for reference language change or round extension
				for _, result := range results {
					if strings.HasPrefix(result, "REFERENCE_LANGUAGE_CHANGED:") {
						newRefLang := strings.TrimPrefix(result, "REFERENCE_LANGUAGE_CHANGED:")
						fmt.Printf("       🔄 Reference language changed: %s -> %s\n", activeReferenceLanguage, newRefLang)
						activeReferenceLanguage = newRefLang

						// Update the header for remaining conversation
						newHeader := prepareLLMHeaderWithMode(db, "", activeReferenceLanguage, operationMode)
						messages[0] = OllamaMessage{Role: "user", Content: newHeader + "\n\nPrayer text to analyze:\n" + strings.Split(messages[0].Content, "Prayer text to analyze:\n")[1]}

						systemMessage += fmt.Sprintf("REFERENCE_LANGUAGE_CHANGED: Now using %s as reference language.\n", newRefLang)
						continue
					}

					if strings.HasPrefix(result, "ROUNDS_EXTENDED:") {
						parts := strings.Split(result, ":")
						if len(parts) >= 3 {
							extensionAmount := 10 // Default extension
							reason := strings.Join(parts[2:], ":")

							// Apply limits based on current extensions
							if roundsExtended >= 30 {
								systemMessage += "EXTENSION DENIED: Maximum extensions reached (30). Use FINAL_ANSWER now.\n"
								continue
							} else if roundsExtended >= 20 {
								extensionAmount = 5 // Smaller extensions after 20
								systemMessage += "EXTENSION GRANTED: Reduced to 5 rounds due to previous extensions.\n"
							} else if roundsExtended >= 10 {
								extensionAmount = 7 // Smaller extensions after 10
								systemMessage += "EXTENSION GRANTED: Reduced to 7 rounds due to previous extensions.\n"
							}

							maxRounds += extensionAmount
							roundsExtended += extensionAmount

							fmt.Printf("       ⏰ Rounds extended by %d (total: %d, extended: %d)\n", extensionAmount, maxRounds, roundsExtended)
							fmt.Printf("       📝 Reason: %s\n", reason)

							systemMessage += fmt.Sprintf("ROUNDS_EXTENDED: Added %d more rounds (total now: %d). Reason: %s\n", extensionAmount, maxRounds, reason)

							// Progressive warnings
							if roundsExtended >= 25 {
								systemMessage += "FINAL WARNING: You have used most available extensions. FINAL_ANSWER required soon.\n"
							} else if roundsExtended >= 15 {
								systemMessage += "WARNING: Multiple extensions used. Focus on making a decision.\n"
							}
						}
						continue
					}

				}

				fmt.Printf("       📋 Results:\n")
				for _, result := range results {
					// Truncate long results for display
					displayResult := result
					if len(displayResult) > 120 {
						displayResult = displayResult[:120] + "..."
					}
					fmt.Printf("          %s\n", displayResult)
				}

				systemMessage += fmt.Sprintf("\n%s -> %s\n", functionCall, strings.Join(results, "\n"))
			}

			// Add strategic guidance based on rounds remaining
			if roundsLeft <= 1 {
				systemMessage += "\n\nCRITICAL: This is your LAST ROUND! You MUST use FINAL_ANSWER now!"
				// Get the FINAL_ANSWER function example dynamically
				for _, handler := range registeredFunctions {
					if handler.GetPattern() == "FINAL_ANSWER:" {
						systemMessage += "\n" + handler.GetUsageExample()
						break
					}
				}
			} else if roundsLeft <= 3 {
				systemMessage += "\n\nWARNING: Only " + fmt.Sprintf("%d", roundsLeft) + " rounds left! Consider using FINAL_ANSWER if you have a good match."
				systemMessage += "\nContinue analysis or provide FINAL_ANSWER:"
			} else {
				systemMessage += "\nContinue your analysis or provide your final answer:"
			}
			messages = append(messages, OllamaMessage{Role: "user", Content: systemMessage})
			fmt.Printf("    ⏳ Continuing to round %d...\n", round+1)
		}
	}

	// If we've reached max rounds without a final answer, return unknown
	fmt.Printf("    ⚠️  Maximum conversation rounds exceeded (started with %d, extended by %d)\n", originalMaxRounds, roundsExtended)
	addSessionNote(activeReferenceLanguage, "FAILURE",
		fmt.Sprintf("Interactive search exceeded maximum conversation rounds (%d original + %d extended)", originalMaxRounds, roundsExtended), "", 0.0)
	return LLMResponse{
		PhelpsCode: "UNKNOWN",
		Confidence: 0.0,
		Reasoning:  fmt.Sprintf("Interactive search exceeded maximum conversation rounds (%d total)", maxRounds),
	}, nil
}

// InvalidFunctionCall represents a malformed function call attempt
type InvalidFunctionCall struct {
	Original string
	Error    string
}

// FinalAnswerResult represents the validation result of a final answer
type FinalAnswerResult struct {
	IsValid  bool
	Response LLMResponse
	Error    string
}

// FunctionCallHandler interface for extensible function call system
type FunctionCallHandler interface {
	GetPattern() string                                                  // e.g., "SEARCH:" or "GET_STATS"
	IsStandalone() bool                                                  // true for GET_STATS, false for SEARCH:
	Validate(call string) bool                                           // validates if the call matches this handler
	GetKeywords() []string                                               // keywords for malformed detection
	GetJSONPattern() string                                              // JSON pattern for gpt-oss style calls
	Execute(db Database, referenceLanguage string, call string) []string // process the function call
	GetDescription() string                                              // description for LLM help text
	GetUsageExample() string                                             // usage example for LLM help text
	IsEnabledForMode(operationMode string) bool                          // check if function is enabled for the given mode
}

// PrefixFunction handles functions that require arguments (PREFIX:args)
type PrefixFunction struct {
	Prefix       string
	Keywords     []string
	JSONPattern  string
	EnabledModes []string // Empty slice means enabled for all modes
}

func (p PrefixFunction) GetPattern() string {
	return p.Prefix + ":"
}

func (p PrefixFunction) IsStandalone() bool {
	return false
}

func (p PrefixFunction) Validate(call string) bool {
	return strings.HasPrefix(call, p.Prefix)
}

func (p PrefixFunction) GetKeywords() []string {
	return p.Keywords
}

func (p PrefixFunction) GetJSONPattern() string {
	return p.JSONPattern
}

func (p PrefixFunction) Execute(db Database, referenceLanguage string, call string) []string {
	return []string{"Function not implemented"}
}

func (p PrefixFunction) GetDescription() string {
	return "Generic prefix function"
}

func (p PrefixFunction) GetUsageExample() string {
	return p.Prefix + ":example"
}

func (p PrefixFunction) IsEnabledForMode(operationMode string) bool {
	// If no modes specified, enabled for all modes
	if len(p.EnabledModes) == 0 {
		return true
	}
	// Check if the operation mode is in the enabled modes list
	for _, mode := range p.EnabledModes {
		if mode == operationMode {
			return true
		}
	}
	return false
}

// StandaloneFunction handles functions that don't require arguments
type StandaloneFunction struct {
	Name         string
	Keywords     []string
	JSONPattern  string
	EnabledModes []string // Empty slice means enabled for all modes
}

func (s StandaloneFunction) GetPattern() string {
	return s.Name
}

func (s StandaloneFunction) IsStandalone() bool {
	return true
}

func (s StandaloneFunction) Validate(call string) bool {
	return strings.TrimSpace(call) == s.Name
}

func (s StandaloneFunction) GetKeywords() []string {
	return s.Keywords
}

func (s StandaloneFunction) GetJSONPattern() string {
	return s.JSONPattern
}

func (s StandaloneFunction) Execute(db Database, referenceLanguage string, call string) []string {
	return []string{"Function not implemented"}
}

func (s StandaloneFunction) GetDescription() string {
	return "Generic standalone function"
}

func (s StandaloneFunction) GetUsageExample() string {
	return s.Name
}

func (s StandaloneFunction) IsEnabledForMode(operationMode string) bool {
	// If no modes specified, enabled for all modes
	if len(s.EnabledModes) == 0 {
		return true
	}
	// Check if the operation mode is in the enabled modes list
	for _, mode := range s.EnabledModes {
		if mode == operationMode {
			return true
		}
	}
	return false
}

// Specific function implementations
type SearchFunction struct{ PrefixFunction }

func (s SearchFunction) Execute(db Database, referenceLanguage string, call string) []string {
	searchStr := strings.TrimPrefix(call, "SEARCH:")
	results := searchPrayersUnified(db, referenceLanguage, searchStr)

	// Add automatic note if search was successful
	if len(results) > 0 && !strings.Contains(results[0], "No prayers found") {
		addSessionNote(referenceLanguage, "SUCCESS",
			fmt.Sprintf("SEARCH:%s found %d matches", searchStr, len(results)), "", 0.0)
	} else {
		addSessionNote(referenceLanguage, "FAILURE",
			fmt.Sprintf("SEARCH:%s returned no matches", searchStr), "", 0.0)
	}
	return results
}

func (s SearchFunction) GetDescription() string {
	return "SEARCH:keywords,opening,range,... (unified search with multiple criteria)"
}

func (s SearchFunction) GetUsageExample() string {
	return "SEARCH:lord,god,assist OR SEARCH:O Thou,100-200"
}

type ExtendRoundsFunction struct{ PrefixFunction }

func (e ExtendRoundsFunction) Execute(db Database, referenceLanguage string, call string) []string {
	reasonStr := strings.TrimSpace(strings.TrimPrefix(call, "EXTEND_ROUNDS:"))
	if reasonStr == "" {
		return []string{"Error: EXTEND_ROUNDS requires a reason (e.g., EXTEND_ROUNDS:Making good progress, need more searches to confirm match)"}
	}

	// Check if reason is valid (not just trying to avoid making a decision)
	reasonLower := strings.ToLower(reasonStr)
	if strings.Contains(reasonLower, "don't know") || strings.Contains(reasonLower, "unsure") ||
		strings.Contains(reasonLower, "confused") || strings.Contains(reasonLower, "need help") {
		return []string{"Error: EXTEND_ROUNDS denied. Provide a specific reason about progress made and what you need to verify."}
	}

	// Check for valid progress indicators
	hasValidReason := strings.Contains(reasonLower, "progress") || strings.Contains(reasonLower, "found") ||
		strings.Contains(reasonLower, "verify") || strings.Contains(reasonLower, "confirm") ||
		strings.Contains(reasonLower, "narrow") || strings.Contains(reasonLower, "refine") ||
		strings.Contains(reasonLower, "search") || strings.Contains(reasonLower, "check") ||
		strings.Contains(reasonLower, "analyze") || strings.Contains(reasonLower, "compare")

	if !hasValidReason {
		return []string{"Error: EXTEND_ROUNDS denied. You must explain what progress you've made or what specific verification you need."}
	}

	// Log session note about extension
	addSessionNote(referenceLanguage, "INFO", fmt.Sprintf("Extended rounds: %s", reasonStr), "", 0.0)

	// This would be handled by the calling function to actually extend rounds
	return []string{fmt.Sprintf("EXTEND_ROUNDS_APPROVED: %s", reasonStr)}
}

func (e ExtendRoundsFunction) GetDescription() string {
	return "EXTEND_ROUNDS:reason (adds 10 more rounds if making progress but need more time)"
}

func (e ExtendRoundsFunction) GetUsageExample() string {
	return "EXTEND_ROUNDS:Making good progress, need more searches to confirm match"
}

type GetStatsFunction struct{ StandaloneFunction }

func (g GetStatsFunction) Execute(db Database, referenceLanguage string, call string) []string {
	stats := getSessionNotesStats()
	var result strings.Builder
	result.WriteString("Session Statistics:\n")
	for key, value := range stats {
		result.WriteString(fmt.Sprintf("  %s: %d\n", key, value))
	}
	return []string{result.String()}
}

func (g GetStatsFunction) GetDescription() string {
	return "GET_STATS (shows session statistics and notes summary)"
}

func (g GetStatsFunction) GetUsageExample() string {
	return "GET_STATS"
}

func (g GetStatsFunction) IsEnabledInMode(mode string) bool {
	return true // Enabled in all modes
}

// Additional function implementations
type SearchKeywordsFunction struct{ PrefixFunction }

func (s SearchKeywordsFunction) Execute(db Database, referenceLanguage string, call string) []string {
	keywordStr := strings.TrimPrefix(call, "SEARCH_KEYWORDS:")
	cleanKeywords := sanitizeKeywords(keywordStr)
	return searchPrayersByKeywords(db, referenceLanguage, cleanKeywords, 15)
}

func (s SearchKeywordsFunction) GetDescription() string {
	return "SEARCH_KEYWORDS:word1,word2,word3 (search by keywords only)"
}
func (s SearchKeywordsFunction) GetUsageExample() string {
	return "SEARCH_KEYWORDS:lord,god,assistance"
}

type SearchLengthFunction struct{ PrefixFunction }

func (s SearchLengthFunction) Execute(db Database, referenceLanguage string, call string) []string {
	lengthStr := strings.TrimPrefix(call, "SEARCH_LENGTH:")
	parts := strings.Split(lengthStr, "-")
	if len(parts) != 2 {
		return []string{"Error: SEARCH_LENGTH requires format min-max (e.g., SEARCH_LENGTH:100-200)"}
	}
	minWords, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	maxWords, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil {
		return []string{"Error: SEARCH_LENGTH requires numeric values (e.g., SEARCH_LENGTH:100-200)"}
	}
	return searchPrayersByLength(db, referenceLanguage, minWords, maxWords, 15)
}

func (s SearchLengthFunction) GetDescription() string {
	return "SEARCH_LENGTH:min-max (search by word count range)"
}
func (s SearchLengthFunction) GetUsageExample() string {
	return "SEARCH_LENGTH:100-200"
}

type SearchOpeningFunction struct{ PrefixFunction }

func (s SearchOpeningFunction) Execute(db Database, referenceLanguage string, call string) []string {
	openingText := strings.TrimPrefix(call, "SEARCH_OPENING:")
	if strings.TrimSpace(openingText) == "" {
		return []string{"Error: SEARCH_OPENING requires text (e.g., SEARCH_OPENING:O Lord my God)"}
	}
	return searchPrayersByOpening(db, referenceLanguage, openingText, 15)
}

func (s SearchOpeningFunction) GetDescription() string {
	return "SEARCH_OPENING:text (search by opening phrase)"
}
func (s SearchOpeningFunction) GetUsageExample() string {
	return "SEARCH_OPENING:O Lord my God"
}

type GetFullTextFunction struct{ PrefixFunction }

func (g GetFullTextFunction) Execute(db Database, referenceLanguage string, call string) []string {
	phelpsCode := strings.TrimPrefix(call, "GET_FULL_TEXT:")
	return getFullTextByPhelps(db, referenceLanguage, phelpsCode)
}

func (g GetFullTextFunction) GetDescription() string {
	return "GET_FULL_TEXT:phelps_code (get complete prayer text)"
}
func (g GetFullTextFunction) GetUsageExample() string {
	return "GET_FULL_TEXT:AB00001"
}

func (g GetFullTextFunction) IsEnabledInMode(mode string) bool {
	return true // Enabled in all modes
}

type GetFocusTextFunction struct{ PrefixFunction }

func (g GetFocusTextFunction) Execute(db Database, referenceLanguage string, call string) []string {
	args := strings.TrimPrefix(call, "GET_FOCUS_TEXT:")
	return getFocusTextByPhelps(db, referenceLanguage, args)
}

func (g GetFocusTextFunction) GetDescription() string {
	return "GET_FOCUS_TEXT:keyword,phelps_code1,phelps_code2,... (get text around keyword, or use 'head'/'tail')"
}
func (g GetFocusTextFunction) GetUsageExample() string {
	return "GET_FOCUS_TEXT:lord,AB00001FIR,AB00002SEC"
}

type GetPartialTextFunction struct{ PrefixFunction }

func (g GetPartialTextFunction) Execute(db Database, referenceLanguage string, call string) []string {
	args := strings.TrimPrefix(call, "GET_PARTIAL_TEXT:")
	parts := strings.Split(args, ",")
	if len(parts) != 2 {
		return []string{"Error: GET_PARTIAL_TEXT requires format: phelps_code,range"}
	}
	return getPartialTextByPhelps(db, referenceLanguage, args)
}

func (g GetPartialTextFunction) GetDescription() string {
	return "GET_PARTIAL_TEXT:phelps_code,range (get excerpt from prayer)"
}
func (g GetPartialTextFunction) GetUsageExample() string {
	return "GET_PARTIAL_TEXT:AB00001,1,5"
}

func (g GetPartialTextFunction) IsEnabledInMode(mode string) bool {
	return true // Enabled in all modes
}

type AddNoteFunction struct{ PrefixFunction }

func (a AddNoteFunction) Execute(db Database, referenceLanguage string, call string) []string {
	args := strings.TrimPrefix(call, "ADD_NOTE:")
	parts := strings.SplitN(args, ",", 2)
	if len(parts) != 2 {
		return []string{"Error: ADD_NOTE requires format: type,content"}
	}
	noteType := strings.TrimSpace(parts[0])
	content := strings.TrimSpace(parts[1])
	addSessionNote(referenceLanguage, noteType, content, "", 0.0)
	return []string{fmt.Sprintf("Added %s note: %s", noteType, content)}
}

func (a AddNoteFunction) GetDescription() string {
	return "ADD_NOTE:type,content (add session note for learning)"
}
func (a AddNoteFunction) GetUsageExample() string {
	return "ADD_NOTE:PATTERN,This prayer mentions mercy and compassion"
}

type SearchNotesFunction struct{ PrefixFunction }

func (s SearchNotesFunction) Execute(db Database, referenceLanguage string, call string) []string {
	query := strings.TrimPrefix(call, "SEARCH_NOTES:")
	if strings.TrimSpace(query) == "" {
		return []string{"Error: SEARCH_NOTES requires a search query"}
	}
	notes := searchSessionNotes(query, "", "")
	var result []string
	for _, note := range notes {
		result = append(result, fmt.Sprintf("[%s] %s: %s", note.NoteType, note.Timestamp.Format("15:04"), note.Content))
	}
	return result
}

func (s SearchNotesFunction) GetDescription() string {
	return "SEARCH_NOTES:query (search session notes)"
}
func (s SearchNotesFunction) GetUsageExample() string {
	return "SEARCH_NOTES:unity"
}

func (s SearchNotesFunction) IsEnabledInMode(mode string) bool {
	return true // Enabled in all modes
}

type ClearNotesFunction struct{ PrefixFunction }

func (c ClearNotesFunction) Execute(db Database, referenceLanguage string, call string) []string {
	criteria := strings.TrimPrefix(call, "CLEAR_NOTES:")
	if strings.TrimSpace(criteria) == "" {
		return []string{"Error: CLEAR_NOTES requires criteria"}
	}
	removeSessionNotes(criteria, "", 0)
	return []string{fmt.Sprintf("Cleared notes matching: %s", criteria)}
}

func (c ClearNotesFunction) GetDescription() string {
	return "CLEAR_NOTES:criteria (clear session notes)"
}
func (c ClearNotesFunction) GetUsageExample() string {
	return "CLEAR_NOTES:older_than_1h"
}

type SwitchReferenceLanguageFunction struct{ PrefixFunction }

func (s SwitchReferenceLanguageFunction) Execute(db Database, referenceLanguage string, call string) []string {
	newRefLang := strings.TrimSpace(strings.TrimPrefix(call, "SWITCH_REFERENCE_LANGUAGE:"))
	hasReference := false
	count := 0
	for _, writing := range db.Writing {
		if writing.Language == newRefLang && writing.Phelps != "" && strings.TrimSpace(writing.Phelps) != "" {
			hasReference = true
			count++
		}
	}
	if !hasReference {
		return []string{fmt.Sprintf("Language '%s' has no prayers with Phelps codes. Use LIST_REFERENCE_LANGUAGES to see available options.", newRefLang)}
	}
	addSessionNote(referenceLanguage, "STRATEGY",
		fmt.Sprintf("Switched reference language from %s to %s (%d prayers available)", referenceLanguage, newRefLang, count), "", 0.0)
	return []string{fmt.Sprintf("REFERENCE_LANGUAGE_CHANGED:%s", newRefLang)}
}

func (s SwitchReferenceLanguageFunction) GetDescription() string {
	return "SWITCH_REFERENCE_LANGUAGE:language_code (change reference language)"
}
func (s SwitchReferenceLanguageFunction) GetUsageExample() string {
	return "SWITCH_REFERENCE_LANGUAGE:es"
}

func (s SwitchReferenceLanguageFunction) IsEnabledInMode(mode string) bool {
	return true // Enabled in all modes
}

type FinalAnswerFunction struct{ PrefixFunction }

func (f FinalAnswerFunction) Execute(db Database, referenceLanguage string, call string) []string {
	args := strings.TrimPrefix(call, "FINAL_ANSWER:")
	parts := strings.SplitN(args, ",", 3)
	if len(parts) != 3 {
		return []string{"Error: FINAL_ANSWER requires format: phelps_code,confidence,reasoning"}
	}
	phelpsCode := strings.TrimSpace(parts[0])
	confidenceStr := strings.TrimSpace(parts[1])
	reasoning := strings.TrimSpace(parts[2])

	confidence, err := strconv.ParseFloat(confidenceStr, 64)
	if err != nil {
		return []string{"Error: Confidence must be a number (0-100)"}
	}
	if confidence > 1 {
		confidence = confidence / 100.0
	}
	return processFinalAnswer(fmt.Sprintf("%s,%.0f,%s", phelpsCode, confidence*100, reasoning))
}

func (f FinalAnswerFunction) GetDescription() string {
	return "FINAL_ANSWER:phelps_code,confidence,reasoning (provide final match result)"
}
func (f FinalAnswerFunction) GetUsageExample() string {
	return "FINAL_ANSWER:AB00001FIR,85,Clear match based on opening phrase and themes"
}

type ListReferenceLanguagesFunction struct{ StandaloneFunction }

func (l ListReferenceLanguagesFunction) Execute(db Database, referenceLanguage string, call string) []string {
	return []string{listReferenceLanguages(db)}
}

func (l ListReferenceLanguagesFunction) GetDescription() string {
	return "LIST_REFERENCE_LANGUAGES (show available reference languages with statistics)"
}
func (l ListReferenceLanguagesFunction) GetUsageExample() string {
	return "LIST_REFERENCE_LANGUAGES"
}

func (l ListReferenceLanguagesFunction) IsEnabledInMode(mode string) bool {
	return true // Enabled in all modes
}

type SearchInventoryFunction struct{ PrefixFunction }

func (s SearchInventoryFunction) Execute(db Database, referenceLanguage string, call string) []string {
	args := strings.TrimPrefix(call, "SEARCH_INVENTORY:")
	parts := strings.Split(args, ",")
	if len(parts) < 2 {
		return []string{
			"Error: SEARCH_INVENTORY requires format: keywords,language",
			"",
			"SUPPORTED LANGUAGES (with good inventory coverage):",
			"- Eng (English) - best coverage",
			"- Ara (Arabic) - original texts",
			"- Per (Persian) - original texts",
			"- Trk (Turkish) - some coverage",
			"",
			"Example: SEARCH_INVENTORY:lord god mercy,Eng",
		}
	}

	keywords := strings.TrimSpace(parts[0])
	language := strings.TrimSpace(parts[1])

	// Map common language codes to inventory format and validate
	inventoryLang := language
	var isWellSupported bool
	switch strings.ToLower(language) {
	case "en", "eng":
		inventoryLang = "Eng"
		isWellSupported = true
	case "ar", "ara", "arabic":
		inventoryLang = "Ara"
		isWellSupported = true
	case "fa", "per", "persian":
		inventoryLang = "Per"
		isWellSupported = true
	case "tr", "trk", "turkish":
		inventoryLang = "Trk"
		isWellSupported = false
	default:
		return []string{
			fmt.Sprintf("Warning: Language '%s' may have very limited inventory coverage.", language),
			"",
			"BEST SUPPORTED LANGUAGES:",
			"- Eng (English) - comprehensive coverage",
			"- Ara (Arabic) - original Bahá'í texts",
			"- Per (Persian) - original Bahá'í texts",
			"",
			"Continue with inventory search anyway? Use exact format: SEARCH_INVENTORY:keywords,Eng",
		}
	}

	// Build search query for inventory table
	keywordList := strings.Split(keywords, " ")
	var conditions []string
	for _, keyword := range keywordList {
		keyword = strings.TrimSpace(keyword)
		if keyword != "" {
			escaped := strings.ReplaceAll(keyword, "'", "''")
			conditions = append(conditions, fmt.Sprintf("(Title LIKE '%%%s%%' OR `First line (original)` LIKE '%%%s%%' OR `First line (translated)` LIKE '%%%s%%' OR Abstracts LIKE '%%%s%%')", escaped, escaped, escaped, escaped))
		}
	}

	whereClause := strings.Join(conditions, " AND ")
	if whereClause == "" {
		whereClause = "1=1"
	}

	query := fmt.Sprintf("SELECT PIN, Title, `First line (original)`, `First line (translated)` FROM inventory WHERE Language = '%s' AND (%s) LIMIT 10", inventoryLang, whereClause)

	output, err := execDoltQuery(query)
	if err != nil {
		return []string{fmt.Sprintf("Error searching inventory: %v", err)}
	}

	lines := strings.Split(string(output), "\n")
	var results []string
	if isWellSupported {
		results = append(results, fmt.Sprintf("INVENTORY SEARCH: '%s' in language '%s' ✅", keywords, inventoryLang))
	} else {
		results = append(results, fmt.Sprintf("INVENTORY SEARCH: '%s' in language '%s' ⚠️ (limited coverage)", keywords, inventoryLang))
	}

	for i, line := range lines {
		if i < 3 || line == "" { // Skip header lines
			continue
		}
		if strings.Contains(line, "|") {
			fields := strings.Split(line, "|")
			if len(fields) >= 4 {
				pin := strings.TrimSpace(fields[0])
				title := strings.TrimSpace(fields[1])
				firstOriginal := strings.TrimSpace(fields[2])
				firstTranslated := strings.TrimSpace(fields[3])

				result := fmt.Sprintf("PIN: %s - %s", pin, title)
				if firstOriginal != "" && firstOriginal != "NULL" {
					result += fmt.Sprintf("\n  Original: %s", firstOriginal)
				}
				if firstTranslated != "" && firstTranslated != "NULL" {
					result += fmt.Sprintf("\n  Translated: %s", firstTranslated)
				}
				results = append(results, result)
			}
		}
	}

	if len(results) == 1 {
		results = append(results, "No matching documents found.")
	}

	return results
}

func (s SearchInventoryFunction) GetDescription() string {
	return "SEARCH_INVENTORY:keywords,language (search inventory table for documents by title/content)"
}

func (s SearchInventoryFunction) GetUsageExample() string {
	return "SEARCH_INVENTORY:lord god mercy,Eng"
}

type CheckTagFunction struct{ PrefixFunction }

func (c CheckTagFunction) Execute(db Database, referenceLanguage string, call string) []string {
	args := strings.TrimPrefix(call, "CHECK_TAG:")
	pin := strings.TrimSpace(args)

	if pin == "" {
		return []string{"Error: CHECK_TAG requires a PIN (e.g., AB00001)"}
	}

	// Escape PIN for SQL query
	escapedPIN := strings.ReplaceAll(pin, "'", "''")

	// Query for all Phelps codes starting with this PIN
	query := fmt.Sprintf("SELECT DISTINCT phelps FROM writings WHERE phelps LIKE '%s%%' AND phelps IS NOT NULL AND phelps != '' ORDER BY phelps", escapedPIN)

	output, err := execDoltQuery(query)
	if err != nil {
		return []string{fmt.Sprintf("Error checking tags for PIN %s: %v", pin, err)}
	}

	lines := strings.Split(string(output), "\n")
	var results []string
	results = append(results, fmt.Sprintf("EXISTING PHELPS CODES for PIN '%s':", pin))

	var foundCodes []string
	for i, line := range lines {
		if i < 3 || line == "" { // Skip header lines
			continue
		}
		if strings.Contains(line, "|") {
			fields := strings.Split(line, "|")
			if len(fields) >= 1 {
				phelpsCode := strings.TrimSpace(fields[0])
				if phelpsCode != "" && phelpsCode != "NULL" {
					foundCodes = append(foundCodes, phelpsCode)
				}
			}
		}
	}

	if len(foundCodes) == 0 {
		results = append(results, "No existing Phelps codes found for this PIN.")
		results = append(results, "This means:")
		results = append(results, "- If the prayer IS the entire document, use: "+pin)
		results = append(results, "- If the prayer is PART of the document, use: "+pin+"XXX (where XXX is a 3-letter tag)")
	} else {
		results = append(results, fmt.Sprintf("Found %d existing codes:", len(foundCodes)))
		for _, code := range foundCodes {
			if len(code) == len(pin) {
				results = append(results, "- "+code+" (entire document)")
			} else if len(code) > len(pin) {
				tag := code[len(pin):]
				results = append(results, "- "+code+" (tag: "+tag+")")
			} else {
				results = append(results, "- "+code)
			}
		}
		results = append(results, "For a new prayer from this document, suggest next available tag.")
	}

	return results
}

func (c CheckTagFunction) GetDescription() string {
	return "CHECK_TAG:pin (show existing Phelps codes and tags for a PIN)"
}

func (c CheckTagFunction) GetUsageExample() string {
	return "CHECK_TAG:AB00156"
}

func (c CheckTagFunction) IsEnabledInMode(mode string) bool {
	return mode == "match-add" || mode == "add-only" // Only enabled in add modes
}

type AddNewPrayerFunction struct{ PrefixFunction }

func (a AddNewPrayerFunction) Execute(db Database, referenceLanguage string, call string) []string {
	args := strings.TrimPrefix(call, "ADD_NEW_PRAYER:")
	parts := strings.SplitN(args, ",", 3)
	if len(parts) != 3 {
		return []string{"Error: ADD_NEW_PRAYER requires format: phelps_code,confidence,reasoning"}
	}

	phelpsCode := strings.TrimSpace(parts[0])
	confidenceStr := strings.TrimSpace(parts[1])
	reasoning := strings.TrimSpace(parts[2])

	// Validate confidence
	confidence, err := strconv.ParseFloat(confidenceStr, 64)
	if err != nil {
		return []string{"Error: Confidence must be a number (0-100)"}
	}
	if confidence < 0 || confidence > 100 {
		return []string{"Error: Confidence must be between 0 and 100"}
	}

	// Basic validation of Phelps code format
	if len(phelpsCode) < 7 {
		return []string{"Error: Phelps code too short - should be PIN (7 chars) or PIN+tag (10 chars)"}
	}

	// Extract PIN from Phelps code
	var pin string
	if len(phelpsCode) == 7 {
		pin = phelpsCode // Entire document
	} else if len(phelpsCode) == 10 {
		pin = phelpsCode[:7] // Document with 3-letter tag
	} else {
		return []string{"Error: Invalid Phelps code format - should be 7 chars (PIN) or 10 chars (PIN+tag)"}
	}

	// Check if PIN exists in inventory
	escapedPIN := strings.ReplaceAll(pin, "'", "''")
	checkQuery := fmt.Sprintf("SELECT COUNT(*) FROM inventory WHERE PIN = '%s'", escapedPIN)
	output, err := execDoltQuery(checkQuery)
	if err != nil {
		return []string{fmt.Sprintf("Error validating PIN %s: %v", pin, err)}
	}

	// Parse count result
	lines := strings.Split(string(output), "\n")
	var count int
	for i, line := range lines {
		if i < 3 || line == "" {
			continue
		}
		if strings.Contains(line, "|") {
			fields := strings.Split(line, "|")
			if len(fields) >= 1 {
				countStr := strings.TrimSpace(fields[0])
				if countStr != "NULL" && countStr != "" {
					count, _ = strconv.Atoi(countStr)
				}
			}
		}
	}

	if count == 0 {
		return []string{fmt.Sprintf("Error: PIN %s not found in inventory - cannot create Phelps code", pin)}
	}

	// Check if Phelps code already exists
	escapedPhelps := strings.ReplaceAll(phelpsCode, "'", "''")
	existsQuery := fmt.Sprintf("SELECT COUNT(*) FROM writings WHERE phelps = '%s'", escapedPhelps)
	output, err = execDoltQuery(existsQuery)
	if err != nil {
		return []string{fmt.Sprintf("Error checking existing Phelps code: %v", err)}
	}

	// Parse exists result
	lines = strings.Split(string(output), "\n")
	var existsCount int
	for i, line := range lines {
		if i < 3 || line == "" {
			continue
		}
		if strings.Contains(line, "|") {
			fields := strings.Split(line, "|")
			if len(fields) >= 1 {
				countStr := strings.TrimSpace(fields[0])
				if countStr != "NULL" && countStr != "" {
					existsCount, _ = strconv.Atoi(countStr)
				}
			}
		}
	}

	if existsCount > 0 {
		return []string{fmt.Sprintf("Error: Phelps code %s already exists - cannot create duplicate", phelpsCode)}
	}

	// Log the successful creation attempt
	results := []string{
		fmt.Sprintf("✅ NEW PHELPS CODE READY: %s", phelpsCode),
		fmt.Sprintf("   PIN: %s (validated in inventory)", pin),
		fmt.Sprintf("   Confidence: %.0f%%", confidence),
		fmt.Sprintf("   Reasoning: %s", reasoning),
		"",
		"This new Phelps code will be assigned when the prayer is processed.",
		"Use FINAL_ANSWER with this code to complete the assignment.",
	}

	return results
}

func (a AddNewPrayerFunction) GetDescription() string {
	return "ADD_NEW_PRAYER:phelps_code,confidence,reasoning (create new Phelps code from inventory PIN)"
}

func (a AddNewPrayerFunction) GetUsageExample() string {
	return "ADD_NEW_PRAYER:AB00001THI,85,Third prayer from Will and Testament document"
}

type CorrectTransliterationFunction struct{ PrefixFunction }

func (c CorrectTransliterationFunction) Execute(db Database, referenceLanguage string, call string) []string {
	// Check if Bahá'í transliteration standards have been accessed this session
	standardsNotes := searchSessionNotes("TRANSLIT_STANDARDS", "TRANSLIT_STANDARDS", referenceLanguage)
	if len(standardsNotes) == 0 {
		return []string{"Error: You must use CHECK_TRANSLIT_STANDARDS first to review Bahá'í transliteration guidelines before making corrections"}
	}

	args := strings.TrimPrefix(call, "CORRECT_TRANSLITERATION:")
	parts := strings.SplitN(args, ",", 3)
	if len(parts) < 2 {
		return []string{"Error: CORRECT_TRANSLITERATION requires: phelps_code,corrected_text[,confidence]"}
	}

	phelpsCode := strings.TrimSpace(parts[0])
	correctedText := strings.TrimSpace(parts[1])

	// Optional confidence parameter (default to 95.0)
	confidence := 95.0
	if len(parts) == 3 {
		confidenceStr := strings.TrimSpace(parts[2])
		if parsed, err := strconv.ParseFloat(confidenceStr, 64); err == nil {
			confidence = parsed
		}
	}

	if confidence < 0 || confidence > 100 {
		return []string{"Error: Confidence must be between 0 and 100"}
	}

	if len(correctedText) < 10 {
		return []string{"Error: Corrected text seems too short - provide substantial correction"}
	}

	// Determine target language based on original writing language
	// Need to find the original writing language to determine correct transliteration target
	targetLang := "ar-translit" // Default to Arabic transliteration

	// Check if we can find the original writing to determine language
	for _, writing := range db.Writing {
		if writing.Phelps == phelpsCode && (writing.Language == "ar" || writing.Language == "fa") {
			if writing.Language == "fa" {
				targetLang = "fa-translit"
			} else {
				targetLang = "ar-translit"
			}
			break
		}
	}

	// Fallback: check if corrected text contains Persian-style transliteration patterns
	if strings.Contains(strings.ToLower(correctedText), "khuda") ||
		strings.Contains(strings.ToLower(correctedText), "baha") ||
		strings.Contains(strings.ToLower(correctedText), "persian") {
		targetLang = "fa-translit"
	}

	// Store the correction for later application
	correction := TranslitCorrection{
		PhelpsCode:    phelpsCode,
		Confidence:    confidence / 100.0, // Convert to decimal
		CorrectedText: correctedText,
		Language:      targetLang,
	}

	results := []string{
		fmt.Sprintf("✅ TRANSLITERATION CORRECTION STORED: %s", phelpsCode),
		fmt.Sprintf("   Target: %s", targetLang),
		fmt.Sprintf("   Confidence: %.0f%%", confidence),
		fmt.Sprintf("   Corrected text preview: %s...", correctedText[:min(100, len(correctedText))]),
		"",
		"This transliteration correction will be applied when FINAL_ANSWER is processed.",
		"Use FINAL_ANSWER with this code to complete the correction.",
	}

	// Add transliteration standards reference
	results = append(results, "")
	results = append(results, "📚 TRANSLITERATION REFERENCE:")
	results = append(results, "Use CHECK_TRANSLIT_STANDARDS for detailed guidance on correct transliteration.")

	// Add session note to track that a correction was made
	addSessionNote(referenceLanguage, "SUCCESS", fmt.Sprintf("CORRECT_TRANSLITERATION:%s applied", phelpsCode), phelpsCode, confidence)

	translitCorrectionsMutex.Lock()
	pendingTranslitCorrections = append(pendingTranslitCorrections, correction)
	translitCorrectionsMutex.Unlock()

	return results
}

func (c CorrectTransliterationFunction) GetDescription() string {
	return "CORRECT_TRANSLITERATION:phelps_code,corrected_text (store corrected transliteration - requires CHECK_TRANSLIT_STANDARDS first)"
}

func (c CorrectTransliterationFunction) GetUsageExample() string {
	return "CORRECT_TRANSLITERATION:AB00001FIR,O Thou Who art the Lord of all names..."
}

func (c CorrectTransliterationFunction) IsEnabledInMode(mode string) bool {
	return mode == "translit" || mode == "match" || mode == "match-add" // Enabled when transliteration work is relevant
}

type CheckTranslitConsistencyFunction struct{ PrefixFunction }

func (c CheckTranslitConsistencyFunction) Execute(db Database, referenceLanguage string, call string) []string {
	args := strings.TrimPrefix(call, "CHECK_TRANSLIT_CONSISTENCY:")
	phelpsCode := strings.TrimSpace(args)

	if phelpsCode == "" {
		return []string{"Error: CHECK_TRANSLIT_CONSISTENCY requires a Phelps code"}
	}

	// Find all language versions of this prayer
	escapedPhelps := strings.ReplaceAll(phelpsCode, "'", "''")
	query := fmt.Sprintf("SELECT language, LEFT(text, 300), name FROM writings WHERE phelps = '%s' AND language IN ('ar', 'fa', 'ar-translit', 'fa-translit') ORDER BY language", escapedPhelps)

	output, err := execDoltQuery(query)
	if err != nil {
		return []string{fmt.Sprintf("Error checking transliteration consistency: %v", err)}
	}

	lines := strings.Split(string(output), "\n")
	var results []string
	results = append(results, fmt.Sprintf("TRANSLITERATION CONSISTENCY CHECK for %s:", phelpsCode))

	versions := make(map[string]string)
	names := make(map[string]string)

	for i, line := range lines {
		if i < 3 || line == "" {
			continue
		}
		if strings.Contains(line, "|") {
			fields := strings.Split(line, "|")
			if len(fields) >= 3 {
				language := strings.TrimSpace(fields[0])
				textPreview := strings.TrimSpace(fields[1])
				name := strings.TrimSpace(fields[2])

				if language != "" && language != "NULL" {
					versions[language] = textPreview
					names[language] = name
				}
			}
		}
	}

	if len(versions) == 0 {
		results = append(results, "No Arabic/Persian versions found for consistency check.")
		return results
	}

	// Report what versions exist
	for _, lang := range []string{"ar", "fa", "ar-translit", "fa-translit"} {
		if text, exists := versions[lang]; exists {
			status := "✅"
			if strings.HasSuffix(lang, "-translit") {
				// Check if original exists
				originalLang := strings.TrimSuffix(lang, "-translit")
				if _, hasOriginal := versions[originalLang]; !hasOriginal {
					status = "⚠️ (missing original)"
				}
			}
			results = append(results, fmt.Sprintf("- %s %s: %s", status, strings.ToUpper(lang), names[lang]))
			if text != "" && text != "NULL" {
				results = append(results, fmt.Sprintf("  Preview: %s...", text))
			}
		}
	}

	// Suggest actions
	results = append(results, "")
	if _, hasAr := versions["ar"]; hasAr {
		if _, hasArTranslit := versions["ar-translit"]; !hasArTranslit {
			results = append(results, "💡 Suggestion: Arabic original exists but no transliteration found")
		}
	}
	if _, hasFa := versions["fa"]; hasFa {
		if _, hasFaTranslit := versions["fa-translit"]; !hasFaTranslit {
			results = append(results, "💡 Suggestion: Persian original exists but no transliteration found")
		}
	}

	return results
}

func (c CheckTranslitConsistencyFunction) GetDescription() string {
	return "CHECK_TRANSLIT_CONSISTENCY:phelps_code (check Arabic/Persian vs transliteration versions)"
}

func (c CheckTranslitConsistencyFunction) GetUsageExample() string {
	return "CHECK_TRANSLIT_CONSISTENCY:AB00001FIR"
}

func (c CheckTranslitConsistencyFunction) IsEnabledInMode(mode string) bool {
	return mode == "translit" || mode == "match" || mode == "match-add" // Enabled when transliteration work is relevant
}

type FindOriginalVersionFunction struct{ PrefixFunction }

func (f FindOriginalVersionFunction) Execute(db Database, referenceLanguage string, call string) []string {
	args := strings.TrimPrefix(call, "FIND_ORIGINAL_VERSION:")
	phelpsCode := strings.TrimSpace(args)

	if phelpsCode == "" {
		return []string{"Error: FIND_ORIGINAL_VERSION requires a Phelps code"}
	}

	// Find corresponding original language versions (ar, fa)
	escapedPhelps := strings.ReplaceAll(phelpsCode, "'", "''")
	query := fmt.Sprintf("SELECT language, LEFT(text, 200), name FROM writings WHERE phelps = '%s' AND language IN ('ar', 'fa') LIMIT 5", escapedPhelps)

	output, err := execDoltQuery(query)
	if err != nil {
		return []string{fmt.Sprintf("Error finding original versions: %v", err)}
	}

	lines := strings.Split(string(output), "\n")
	var results []string
	results = append(results, fmt.Sprintf("ORIGINAL VERSIONS for %s:", phelpsCode))

	var foundVersions []string
	for i, line := range lines {
		if i < 3 || line == "" {
			continue
		}
		if strings.Contains(line, "|") {
			fields := strings.Split(line, "|")
			if len(fields) >= 3 {
				language := strings.TrimSpace(fields[0])
				textPreview := strings.TrimSpace(fields[1])
				name := strings.TrimSpace(fields[2])

				if language != "" && language != "NULL" {
					result := fmt.Sprintf("- %s: %s", strings.ToUpper(language), name)
					if textPreview != "" && textPreview != "NULL" {
						result += fmt.Sprintf("\n  Text: %s...", textPreview)
					}
					foundVersions = append(foundVersions, result)
				}
			}
		}
	}

	if len(foundVersions) == 0 {
		results = append(results, "No Arabic or Persian original versions found.")
		results = append(results, "This may be a standalone transliteration or the original is missing.")
	} else {
		results = append(results, foundVersions...)
	}

	return results
}

func (f FindOriginalVersionFunction) GetDescription() string {
	return "FIND_ORIGINAL_VERSION:phelps_code (find Arabic/Persian original for comparison)"
}

func (f FindOriginalVersionFunction) GetUsageExample() string {
	return "FIND_ORIGINAL_VERSION:AB00001FIR"
}

func (f FindOriginalVersionFunction) IsEnabledInMode(mode string) bool {
	return mode == "translit" // Primarily for transliteration mode
}

// Helper functions for easier function registration
func NewPrefixFunction(name string) PrefixFunction {
	return PrefixFunction{
		Prefix:      name + ":",
		Keywords:    []string{name},
		JSONPattern: name,
	}
}

func NewPrefixFunctionWithModes(name string, modes []string) PrefixFunction {
	return PrefixFunction{
		Prefix:       name + ":",
		Keywords:     []string{name},
		JSONPattern:  name,
		EnabledModes: modes,
	}
}

func NewStandaloneFunction(name string) StandaloneFunction {
	return StandaloneFunction{
		Name:        name,
		Keywords:    []string{name},
		JSONPattern: name,
	}
}

func NewStandaloneFunctionWithModes(name string, modes []string) StandaloneFunction {
	return StandaloneFunction{
		Name:         name,
		Keywords:     []string{name},
		EnabledModes: modes,
	}
}

// Registry of all supported function calls
var registeredFunctions = []FunctionCallHandler{
	// Matching functions - disabled in add-only mode
	SearchFunction{NewPrefixFunctionWithModes("SEARCH", []string{"match", "match-add", "translit"})},
	SearchKeywordsFunction{NewPrefixFunctionWithModes("SEARCH_KEYWORDS", []string{"match", "match-add", "translit"})},
	SearchLengthFunction{NewPrefixFunctionWithModes("SEARCH_LENGTH", []string{"match", "match-add", "translit"})},
	SearchOpeningFunction{NewPrefixFunctionWithModes("SEARCH_OPENING", []string{"match", "match-add", "translit"})},
	GetFullTextFunction{NewPrefixFunctionWithModes("GET_FULL_TEXT", []string{"match", "match-add", "translit"})},
	GetFocusTextFunction{NewPrefixFunctionWithModes("GET_FOCUS_TEXT", []string{"match", "match-add", "translit"})},
	GetPartialTextFunction{NewPrefixFunctionWithModes("GET_PARTIAL_TEXT", []string{"match", "match-add", "translit"})},

	// Universal functions - available in all modes
	AddNoteFunction{NewPrefixFunction("ADD_NOTE")},
	SearchNotesFunction{NewPrefixFunction("SEARCH_NOTES")},
	ClearNotesFunction{NewPrefixFunction("CLEAR_NOTES")},
	ExtendRoundsFunction{NewPrefixFunction("EXTEND_ROUNDS")},
	SwitchReferenceLanguageFunction{NewPrefixFunction("SWITCH_REFERENCE_LANGUAGE")},
	FinalAnswerFunction{NewPrefixFunction("FINAL_ANSWER")},
	GetStatsFunction{NewStandaloneFunction("GET_STATS")},
	ListReferenceLanguagesFunction{NewStandaloneFunction("LIST_REFERENCE_LANGUAGES")},

	// Inventory functions - available in match-add and add-only modes
	SearchInventoryFunction{NewPrefixFunctionWithModes("SEARCH_INVENTORY", []string{"match-add", "add-only"})},
	CheckTagFunction{NewPrefixFunctionWithModes("CHECK_TAG", []string{"match-add", "add-only"})},
	AddNewPrayerFunction{NewPrefixFunctionWithModes("ADD_NEW_PRAYER", []string{"match-add", "add-only"})},

	// Transliteration functions - available in all modes but priority in translit
	CorrectTransliterationFunction{NewPrefixFunction("CORRECT_TRANSLITERATION")},
	FindOriginalVersionFunction{NewPrefixFunction("FIND_ORIGINAL_VERSION")},
	CheckTranslitConsistencyFunction{NewPrefixFunction("CHECK_TRANSLIT_CONSISTENCY")},
	CheckTranslitStandardsFunction{NewStandaloneFunctionWithModes("CHECK_TRANSLIT_STANDARDS", []string{"translit", "match", "match-add"})},

	// Transliteration-specific version matching and correction functions
	MatchVersionFunction{NewPrefixFunctionWithModes("MATCH_VERSION", []string{"translit"})},
	CorrectVersionFunction{NewPrefixFunctionWithModes("CORRECT_VERSION", []string{"translit"})},
	SearchVersionFunction{NewPrefixFunctionWithModes("SEARCH_VERSION", []string{"translit"})},
	MatchConfirmedFunction{NewPrefixFunctionWithModes("MATCH_CONFIRMED", []string{"translit"})},
}

type CheckTranslitStandardsFunction struct{ StandaloneFunction }

func (c CheckTranslitStandardsFunction) Execute(db Database, referenceLanguage string, call string) []string {
	// Add session note to track that standards were accessed
	addSessionNote(referenceLanguage, "TRANSLIT_STANDARDS", "Bahá'í transliteration standards accessed", "", 100.0)

	// Return the complete embedded Bahá'í transliteration standards
	return getTransliterationStandards()
}

func (c CheckTranslitStandardsFunction) GetDescription() string {
	return "CHECK_TRANSLIT_STANDARDS (display official Bahá'í transliteration guidelines)"
}

func (c CheckTranslitStandardsFunction) GetUsageExample() string {
	return "CHECK_TRANSLIT_STANDARDS"
}

func (c CheckTranslitStandardsFunction) IsEnabledInMode(mode string) bool {
	return mode == "translit" || mode == "match" || mode == "match-add"
}

// MATCH_VERSION function - matches current prayer version to get Phelps code (translit mode only)
type MatchVersionFunction struct {
	PrefixFunction
}

func (m MatchVersionFunction) Execute(db Database, referenceLanguage string, functionCall string) []string {
	// Extract prayer text or description from the function call
	description := strings.TrimSpace(strings.TrimPrefix(functionCall, "MATCH_VERSION:"))

	if description == "" {
		return []string{"MATCH_VERSION requires description: MATCH_VERSION:prayer text or keywords"}
	}

	// Search for matching prayers using the description
	results := searchPrayersUnified(db, referenceLanguage, description)

	if len(results) == 0 || strings.Contains(results[0], "No prayers found") {
		return []string{fmt.Sprintf("MATCH_VERSION: No matches found for '%s' in %s inventory", description, referenceLanguage)}
	}

	// Format response for transliteration mode
	response := []string{}
	response = append(response, fmt.Sprintf("MATCH_VERSION results for '%s' in %s:", description, referenceLanguage))
	response = append(response, "")
	response = append(response, results...)
	response = append(response, "")
	response = append(response, "Next steps:")
	response = append(response, "1. Select the best matching Phelps code from above")
	response = append(response, "2. Use CORRECT_VERSION:PhelpsCode to check/fix the transliteration")
	response = append(response, "3. Use FINAL_ANSWER:PhelpsCode with your confidence when done")

	return response
}

// MATCH_CONFIRMED function - confirms match without terminating conversation (translit mode only)
type MatchConfirmedFunction struct {
	PrefixFunction
}

func (m MatchConfirmedFunction) Execute(db Database, referenceLanguage string, functionCall string) []string {
	// Extract Phelps code and confidence from the function call
	args := strings.TrimSpace(strings.TrimPrefix(functionCall, "MATCH_CONFIRMED:"))

	parts := strings.Split(args, ",")
	if len(parts) < 2 {
		return []string{"MATCH_CONFIRMED requires: MATCH_CONFIRMED:PhelpsCode,confidence"}
	}

	phelpsCode := strings.TrimSpace(parts[0])
	confidenceStr := strings.TrimSpace(parts[1])

	if phelpsCode == "" {
		return []string{"MATCH_CONFIRMED: Phelps code cannot be empty"}
	}

	confidence, err := strconv.ParseFloat(confidenceStr, 64)
	if err != nil {
		return []string{"MATCH_CONFIRMED: Invalid confidence value. Use number like 95.0"}
	}

	response := []string{}
	response = append(response, fmt.Sprintf("✅ MATCH CONFIRMED: %s (%.1f%% confidence)", phelpsCode, confidence))
	response = append(response, "")
	response = append(response, "Next steps:")
	response = append(response, "1. Review the current transliteration quality")
	response = append(response, "2. Use CORRECT_TRANSLITERATION:"+phelpsCode+",corrected_text if improvements needed")
	response = append(response, "3. Use FINAL_ANSWER:"+phelpsCode+" when satisfied")

	return response
}

// CORRECT_VERSION function - corrects transliteration of a matched prayer (translit mode only)
type CorrectVersionFunction struct {
	PrefixFunction
}

func (c CorrectVersionFunction) Execute(db Database, referenceLanguage string, functionCall string) []string {
	args := strings.TrimSpace(strings.TrimPrefix(functionCall, "CORRECT_VERSION:"))

	// Parse arguments: version_id,corrected_transliteration
	parts := strings.SplitN(args, ",", 2)
	if len(parts) < 1 || parts[0] == "" {
		return []string{"CORRECT_VERSION requires version ID: CORRECT_VERSION:version_id or CORRECT_VERSION:version_id,corrected_transliteration"}
	}

	versionID := strings.TrimSpace(parts[0])

	// If only version ID provided, show current status
	if len(parts) == 1 {
		// Find the writing by version ID
		var targetWriting *Writing
		for _, writing := range db.Writing {
			if writing.Version == versionID {
				targetWriting = &writing
				break
			}
		}

		if targetWriting == nil {
			return []string{fmt.Sprintf("CORRECT_VERSION: No writing found with version ID %s", versionID)}
		}

		response := []string{}
		response = append(response, fmt.Sprintf("CORRECT_VERSION status for version %s:", versionID))
		response = append(response, "")
		response = append(response, fmt.Sprintf("Language: %s", targetWriting.Language))
		response = append(response, fmt.Sprintf("Current text: %s", targetWriting.Text[:min(200, len(targetWriting.Text))]+"..."))
		response = append(response, "")
		response = append(response, "To submit correction: CORRECT_VERSION:"+versionID+",your_corrected_transliteration")

		return response
	}

	// Submit corrected transliteration
	correctedText := strings.TrimSpace(parts[1])
	if correctedText == "" {
		return []string{"CORRECT_VERSION: Corrected transliteration cannot be empty"}
	}

	// Store the correction
	err := createOrUpdateTransliteration(versionID, referenceLanguage+"-translit", correctedText, 95.0)
	if err != nil {
		return []string{fmt.Sprintf("CORRECT_VERSION: Failed to store correction: %v", err)}
	}

	return []string{
		fmt.Sprintf("✅ TRANSLITERATION CORRECTION STORED for version %s", versionID),
		fmt.Sprintf("Language: %s-translit", referenceLanguage),
		fmt.Sprintf("Corrected text: %s", correctedText[:min(100, len(correctedText))]+"..."),
		"",
		"Use FINAL_ANSWER:version_id to complete the process",
	}
}

// SEARCH_VERSION function - searches for prayers and returns version IDs (translit mode only)
type SearchVersionFunction struct {
	PrefixFunction
}

func (s SearchVersionFunction) Execute(db Database, referenceLanguage string, functionCall string) []string {
	// Extract search criteria from the function call
	searchStr := strings.TrimSpace(strings.TrimPrefix(functionCall, "SEARCH_VERSION:"))

	if searchStr == "" {
		return []string{"SEARCH_VERSION requires search criteria: SEARCH_VERSION:keywords or opening phrase"}
	}

	// Search for matching prayers with version information
	results := searchPrayersUnifiedWithVersions(db, referenceLanguage, searchStr, true)

	if len(results) == 0 || strings.Contains(results[0], "No prayers found") {
		return []string{fmt.Sprintf("SEARCH_VERSION: No matches found for '%s' in %s", searchStr, referenceLanguage)}
	}

	// Format response for transliteration mode
	response := []string{}
	response = append(response, fmt.Sprintf("SEARCH_VERSION results for '%s' in %s:", searchStr, referenceLanguage))
	response = append(response, "")
	response = append(response, results...)
	response = append(response, "")
	response = append(response, "Each result shows: PHELPS_CODE (Version: VERSION_ID, MATCH%) CONTEXT")
	response = append(response, "Use the Version ID to reference the specific prayer text")

	return response
}

// Helper function to determine if we should check transliteration
func shouldCheckTransliteration(language, operationMode string) bool {
	// In translit mode, check all Arabic/Persian variants
	if operationMode == "translit" {
		return language == "ar" || language == "fa" || language == "ar-translit" || language == "fa-translit"
	}
	// In all other modes, always check ar/fa originals for transliteration opportunities
	return language == "ar" || language == "fa"
}

// Add transliteration context to the prompt for all modes
func addTransliterationContext(db Database, writing Writing, originalPrompt string, operationMode string) string {
	if !shouldCheckTransliteration(writing.Language, operationMode) {
		return originalPrompt
	}

	translitGuidance := "\n\nTRANSLITERATION NOTE:\n"
	if operationMode == "translit" {
		if strings.HasSuffix(writing.Language, "-translit") {
			baseLanguage := strings.TrimSuffix(writing.Language, "-translit")
			translitGuidance += fmt.Sprintf("🔤 TRANSLIT MODE: This is %s transliteration (Version: %s)\n", baseLanguage, writing.Version)

			if writing.Phelps != "" && strings.TrimSpace(writing.Phelps) != "" {
				translitGuidance += fmt.Sprintf("Has Phelps code: %s\n", writing.Phelps)

				// Find and provide the original text for comparison
				var originalText string
				for _, w := range db.Writing {
					if w.Phelps == writing.Phelps && w.Language == baseLanguage {
						originalText = w.Text
						break
					}
				}

				if originalText != "" {
					translitGuidance += "\n📖 ORIGINAL " + strings.ToUpper(baseLanguage) + " TEXT:\n"
					if len(originalText) > 300 {
						translitGuidance += originalText[:300] + "...\n"
					} else {
						translitGuidance += originalText + "\n"
					}
					translitGuidance += "\n🔤 CURRENT TRANSLITERATION:\n"
					if len(writing.Text) > 300 {
						translitGuidance += writing.Text[:300] + "...\n"
					} else {
						translitGuidance += writing.Text + "\n"
					}
					translitGuidance += "\n"
				}

				translitGuidance += "1. Use CHECK_TRANSLIT_STANDARDS to review Bahá'í transliteration guidelines\n"
				translitGuidance += "2. Compare current transliteration with original using Bahá'í standards\n"
				translitGuidance += "3. Use CORRECT_TRANSLITERATION:" + writing.Phelps + ",corrected_text if improvements needed\n"
				translitGuidance += "4. Use FINAL_ANSWER:" + writing.Phelps + " when satisfied\n"
			} else {
				translitGuidance += "No Phelps code - match this transliteration to find original:\n"
				translitGuidance += "1. Use search functions to find matching original prayer\n"
				translitGuidance += "2. Use MATCH_CONFIRMED:PhelpsCode,confidence to confirm match\n"
				translitGuidance += "3. Use CHECK_TRANSLIT_STANDARDS to review Bahá'í guidelines\n"
				translitGuidance += "4. Use CORRECT_TRANSLITERATION:PhelpsCode,corrected_text if needed\n"
				translitGuidance += "5. Use FINAL_ANSWER:PhelpsCode when done\n"
			}
		} else if writing.Language == "ar" || writing.Language == "fa" {
			translitGuidance += fmt.Sprintf("🔤 TRANSLIT MODE: This is %s original - not typical for translit mode\n", writing.Language)
			translitGuidance += "Consider processing the corresponding transliteration instead.\n"
		}
	} else {
		// Non-translit modes
		if writing.Language == "ar" || writing.Language == "fa" {
			translitGuidance += "⚠️ This is an Arabic/Persian prayer. After matching, check if transliteration version exists and needs updating.\n"
			translitGuidance += "Use CHECK_TRANSLIT_CONSISTENCY after finding the Phelps code to verify transliteration quality.\n"
		}
	}

	return originalPrompt + translitGuidance
}

// Check and trigger transliteration after successful prayer matching for all modes
func checkAndTriggerTransliteration(db Database, writing Writing, phelpsCode string, verbose bool, reportFile *os.File, operationMode string) {
	// Always check transliteration for ar/fa prayers regardless of mode
	if !shouldCheckTransliteration(writing.Language, operationMode) {
		return
	}

	// Check if transliteration versions exist
	escapedPhelps := strings.ReplaceAll(phelpsCode, "'", "''")
	var query string
	var expectedTranslit string

	if writing.Language == "ar" {
		expectedTranslit = "ar-translit"
		query = fmt.Sprintf("SELECT COUNT(*) FROM writings WHERE phelps = '%s' AND language = 'ar-translit'", escapedPhelps)
	} else if writing.Language == "fa" {
		expectedTranslit = "fa-translit"
		query = fmt.Sprintf("SELECT COUNT(*) FROM writings WHERE phelps = '%s' AND language = 'fa-translit'", escapedPhelps)
	} else {
		return // Only check for ar/fa base languages
	}

	output, err := execDoltQuery(query)
	if err != nil {
		if verbose {
			log.Printf("Error checking for transliteration trigger for %s (%s): %v", phelpsCode, writing.Language, err)
		}
		return
	}

	// Parse result
	lines := strings.Split(string(output), "\n")
	var count int
	for i, line := range lines {
		if i < 3 || line == "" {
			continue
		}
		if strings.Contains(line, "|") {
			fields := strings.Split(line, "|")
			if len(fields) >= 1 {
				countStr := strings.TrimSpace(fields[0])
				if countStr != "NULL" && countStr != "" {
					count, _ = strconv.Atoi(countStr)
				}
			}
		}
	}

	if count == 0 {
		fmt.Fprintf(reportFile, "  📝 TRANSLITERATION OPPORTUNITY: No %s version found for %s\n", expectedTranslit, phelpsCode)
		if verbose {
			fmt.Printf("  📝 Consider creating %s transliteration for %s\n", expectedTranslit, phelpsCode)
		}
	} else if verbose {
		fmt.Printf("  ✅ %s transliteration exists for %s\n", expectedTranslit, phelpsCode)
	}
}

// RegisterFunction adds a new function to the registry (for extensions)
func RegisterFunction(handler FunctionCallHandler) {
	registeredFunctions = append(registeredFunctions, handler)
}

// generateFunctionHelp creates help text for all registered functions
// generateFunctionList creates a concise pipe-separated list of all registered functions
func generateFunctionHelp() string {
	return generateFunctionHelpForMode("")
}

func generateFunctionHelpForMode(operationMode string) string {
	var help strings.Builder
	for _, handler := range registeredFunctions {
		if operationMode == "" || handler.IsEnabledForMode(operationMode) {
			help.WriteString("- ")
			help.WriteString(handler.GetDescription())
			help.WriteString("\n  Example: ")
			help.WriteString(handler.GetUsageExample())
			help.WriteString("\n")
		}
	}
	return help.String()
}

// generateConciseFunctionList creates a concise pipe-separated list of all registered functions
func generateConciseFunctionList() string {
	return generateConciseFunctionListForMode("")
}

func generateConciseFunctionListForMode(operationMode string) string {
	var functions []string
	for _, handler := range registeredFunctions {
		if operationMode == "" || handler.IsEnabledForMode(operationMode) {
			// Extract the basic function signature from the description
			desc := handler.GetDescription()
			// Take everything before the first space or opening parenthesis for a clean signature
			if idx := strings.Index(desc, " "); idx != -1 {
				functions = append(functions, desc[:idx])
			} else if idx := strings.Index(desc, "("); idx != -1 {
				functions = append(functions, desc[:idx])
			} else {
				functions = append(functions, desc)
			}
		}
	}
	return strings.Join(functions, ", ")
}

func generateFunctionDescriptionsForMode(operationMode string) string {
	var descriptions []string

	for _, handler := range registeredFunctions {
		if operationMode == "" || handler.IsEnabledForMode(operationMode) {
			desc := handler.GetDescription()
			example := handler.GetUsageExample()
			descriptions = append(descriptions, fmt.Sprintf("- %s\n  Example: %s", desc, example))
		}
	}

	return strings.Join(descriptions, "\n")
}

func generateFunctionExamplesForMode(operationMode string) string {
	var examples []string

	// Generate examples based on available functions for this mode
	for _, handler := range registeredFunctions {
		if operationMode == "" || handler.IsEnabledForMode(operationMode) {
			pattern := handler.GetPattern()
			example := handler.GetUsageExample()

			// Only include key functions in examples to avoid clutter
			if isKeyFunctionForMode(pattern, operationMode) {
				examples = append(examples, example)
			}
		}
	}

	if len(examples) == 0 {
		examples = []string{
			"SEARCH:keywords,opening_phrase,length_range",
			"FINAL_ANSWER:phelps_code,confidence,reasoning",
		}
	}

	result := "📋 KEY FUNCTION EXAMPLES:\n"
	for _, example := range examples {
		result += "- " + example + "\n"
	}
	return result
}

func isKeyFunctionForMode(pattern string, operationMode string) bool {
	switch operationMode {
	case "match":
		return pattern == "SEARCH:" || pattern == "GET_FULL_TEXT:" || pattern == "FINAL_ANSWER:"
	case "match-add":
		return pattern == "SEARCH:" || pattern == "GET_FULL_TEXT:" || pattern == "SEARCH_INVENTORY:" || pattern == "ADD_NEW_PRAYER:" || pattern == "FINAL_ANSWER:"
	case "add-only":
		return pattern == "SEARCH_INVENTORY:" || pattern == "CHECK_TAG:" || pattern == "ADD_NEW_PRAYER:" || pattern == "FINAL_ANSWER:"
	case "translit":
		return pattern == "SEARCH:" || pattern == "GET_FULL_TEXT:" || pattern == "CHECK_TRANSLIT_CONSISTENCY:" || pattern == "CORRECT_TRANSLITERATION:" || pattern == "FINAL_ANSWER:"
	default:
		return pattern == "SEARCH:" || pattern == "FINAL_ANSWER:"
	}
}

func isValidFunctionCall(line string) bool {
	return isValidFunctionCallInMode(line, "")
}

func isValidFunctionCallInMode(line string, operationMode string) bool {
	for _, handler := range registeredFunctions {
		if handler.Validate(line) && (operationMode == "" || handler.IsEnabledForMode(operationMode)) {
			return true
		}
	}
	return false
}

func containsFunctionKeyword(line string) bool {
	return containsFunctionKeywordForMode(line, "")
}

func containsFunctionKeywordForMode(line string, operationMode string) bool {
	upperLine := strings.ToUpper(line)
	for _, handler := range registeredFunctions {
		if operationMode == "" || handler.IsEnabledForMode(operationMode) {
			for _, keyword := range handler.GetKeywords() {
				if strings.Contains(upperLine, keyword) {
					return true
				}
			}
		}
	}
	return false
}

func extractAllFunctionCalls(text string) ([]string, []InvalidFunctionCall) {
	var validCalls []string
	var invalidCalls []InvalidFunctionCall

	// First try to parse as tool_calls JSON format
	if toolCalls := parseToolCallsFormat(text); len(toolCalls) > 0 {
		return toolCalls, invalidCalls
	}

	lines := strings.Split(text, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Check for standard format using centralized validation
		if isValidFunctionCall(line) {
			validCalls = append(validCalls, line)
			continue
		}

		// Check for JSON function call format (like gpt-oss uses) - use registry system
		jsonHandled := false
		for _, handler := range registeredFunctions {
			jsonPattern := handler.GetJSONPattern()
			if jsonPattern != "" && strings.Contains(line, fmt.Sprintf(`"function":"%s"`, jsonPattern)) {
				if handler.IsStandalone() {
					validCalls = append(validCalls, handler.GetPattern())
					jsonHandled = true
					break
				} else if strings.Contains(line, `"arguments"`) {
					functionCall := parseJSONFunctionCall(line, jsonPattern)
					if functionCall != "" {
						// Special handling for SEARCH_KEYWORDS sanitization
						if strings.HasPrefix(functionCall, "SEARCH_KEYWORDS:") {
							keywordStr := strings.TrimPrefix(functionCall, "SEARCH_KEYWORDS:")
							cleanKeywords := sanitizeKeywords(keywordStr)
							functionCall = "SEARCH_KEYWORDS:" + strings.Join(cleanKeywords, ",")
						}
						validCalls = append(validCalls, functionCall)
						jsonHandled = true
						break
					} else {
						invalidCalls = append(invalidCalls, InvalidFunctionCall{
							Original: line,
							Error:    fmt.Sprintf("Invalid JSON function call format for %s. Use: %s", jsonPattern, handler.GetPattern()),
						})
						jsonHandled = true
						break
					}
				}
			}
		}

		if jsonHandled {
			continue
		}

		// Check for malformed function attempts using centralized validation
		if containsFunctionKeyword(line) {
			// Check if it's a malformed attempt (not in correct format)
			if !isValidFunctionCall(line) {
				invalidCalls = append(invalidCalls, InvalidFunctionCall{
					Original: line,
					Error:    fmt.Sprintf("Malformed function call: '%s'. Use correct format like SEARCH_KEYWORDS:word1,word2", line),
				})
			}
		}
	}

	return validCalls, invalidCalls
}

// ToolCall represents a single tool call in the JSON format
type ToolCall struct {
	Function struct {
		Name      string `json:"name"`
		Arguments struct {
			Arguments string `json:"arguments"`
			Name      string `json:"name"`
		} `json:"arguments"`
	} `json:"function"`
}

// ToolCallsResponse represents the tool_calls JSON format
type ToolCallsResponse struct {
	ToolCalls []ToolCall `json:"tool_calls"`
}

// Parse tool_calls JSON format from Ollama responses
func parseToolCallsFormat(text string) []string {
	var validCalls []string

	// Try to find tool_calls JSON in the response
	if !strings.Contains(text, "tool_calls") {
		return validCalls
	}

	// Extract JSON part containing tool_calls
	startIdx := strings.Index(text, `"tool_calls"`)
	if startIdx == -1 {
		return validCalls
	}

	// Find the start of the JSON object
	jsonStart := strings.LastIndex(text[:startIdx], "{")
	if jsonStart == -1 {
		jsonStart = 0
	}

	// Find the end of the JSON object
	braceCount := 0
	jsonEnd := len(text)
	for i := jsonStart; i < len(text); i++ {
		if text[i] == '{' {
			braceCount++
		} else if text[i] == '}' {
			braceCount--
			if braceCount == 0 {
				jsonEnd = i + 1
				break
			}
		}
	}

	jsonStr := text[jsonStart:jsonEnd]

	var toolCallsResp ToolCallsResponse
	if err := json.Unmarshal([]byte(jsonStr), &toolCallsResp); err != nil {
		return validCalls
	}

	// Convert tool calls to standard format using registered functions
	for _, toolCall := range toolCallsResp.ToolCalls {
		functionName := toolCall.Function.Name

		// Find matching handler in registered functions
		for _, handler := range registeredFunctions {
			if handler.GetJSONPattern() == functionName {
				if handler.IsStandalone() {
					// Standalone functions like GET_STATS
					validCalls = append(validCalls, functionName)
				} else {
					// Prefix functions with arguments
					if args := toolCall.Function.Arguments.Arguments; args != "" {
						validCalls = append(validCalls, fmt.Sprintf("%s:%s", functionName, args))
					}
				}
				break
			}
		}
	}

	return validCalls
}

// Parse JSON function call and convert to standard format
func parseJSONFunctionCall(line, functionName string) string {
	// Simple JSON parsing for function calls
	if strings.Contains(line, `"arguments":"`) {
		start := strings.Index(line, `"arguments":"`)
		if start == -1 {
			return ""
		}
		start += len(`"arguments":"`)
		end := strings.Index(line[start:], `"`)
		if end == -1 {
			return ""
		}
		args := line[start : start+end]
		return functionName + ":" + args
	}
	return ""
}

// Validate if the response contains a proper final answer
func validateFinalAnswer(text string, response LLMResponse, db Database, referenceLanguage string) FinalAnswerResult {
	lines := strings.Split(text, "\n")

	var phelpsLine, confidenceLine, reasoningLine string

	for _, line := range lines {
		line = strings.TrimSpace(line)
		lowerLine := strings.ToLower(line)

		if strings.HasPrefix(lowerLine, "phelps:") {
			phelpsLine = line
		} else if strings.HasPrefix(lowerLine, "confidence:") {
			confidenceLine = line
		} else if strings.HasPrefix(lowerLine, "reasoning:") {
			reasoningLine = line
		}
	}

	// Check if we have all required components
	if phelpsLine == "" || confidenceLine == "" || reasoningLine == "" {
		return FinalAnswerResult{
			IsValid: false,
			Error:   "Missing required final answer format. Need: Phelps: [CODE], Confidence: [0-100], Reasoning: [explanation]",
		}
	}

	// Parse the response
	phelpsCode := strings.TrimSpace(strings.TrimPrefix(phelpsLine, "Phelps:"))
	phelpsCode = strings.TrimSpace(strings.TrimPrefix(phelpsCode, "phelps:"))

	confidenceStr := strings.TrimSpace(strings.TrimPrefix(confidenceLine, "Confidence:"))
	confidenceStr = strings.TrimSpace(strings.TrimPrefix(confidenceStr, "confidence:"))
	confidenceStr = strings.TrimSuffix(confidenceStr, "%")

	reasoning := strings.TrimSpace(strings.TrimPrefix(reasoningLine, "Reasoning:"))
	reasoning = strings.TrimSpace(strings.TrimPrefix(reasoning, "reasoning:"))

	// Validate confidence is a number
	confidence, err := strconv.ParseFloat(confidenceStr, 64)
	if err != nil {
		return FinalAnswerResult{
			IsValid: false,
			Error:   fmt.Sprintf("Invalid confidence value '%s'. Must be a number 0-100", confidenceStr),
		}
	}

	// Convert percentage to decimal if needed
	if confidence > 1.0 {
		confidence = confidence / 100.0
	}

	// Validate Phelps code format (basic validation)
	if phelpsCode == "" {
		return FinalAnswerResult{
			IsValid: false,
			Error:   "Empty Phelps code. Use format like AB00001FIR or UNKNOWN",
		}
	}

	// Check if Phelps code exists in database (unless it's UNKNOWN)
	if phelpsCode != "UNKNOWN" && !phelpsCodeExists(db, referenceLanguage, phelpsCode) {
		return FinalAnswerResult{
			IsValid: false,
			Error:   fmt.Sprintf("ERROR: Phelps code '%s' does not exist in the database for language '%s'.\n\nADMONITION: You MUST only use Phelps codes that exist in the provided context. Do not invent codes from outside knowledge. If you cannot find a match among the available prayers, use UNKNOWN instead.\n\nPlease search the available prayers using the SEARCH function and select from the actual results.", phelpsCode, referenceLanguage),
		}
	}

	return FinalAnswerResult{
		IsValid: true,
		Response: LLMResponse{
			PhelpsCode: phelpsCode,
			Confidence: confidence,
			Reasoning:  reasoning,
		},
	}
}

// Legacy function for backward compatibility
func extractFunctionCalls(text string) []string {
	validCalls, _ := extractAllFunctionCalls(text)
	return validCalls
}

// Calculate missing prayers per language
func calculateMissingPrayersPerLanguage(db Database) map[string]int {
	missing := make(map[string]int)
	for _, writing := range db.Writing {
		if writing.Phelps == "" || strings.TrimSpace(writing.Phelps) == "" {
			missing[writing.Language]++
		}
	}
	return missing
}

// Find language with lowest non-zero missing prayers, prioritizing common languages
func findOptimalDefaultLanguage(db Database, noPriority bool) string {
	missing := calculateMissingPrayersPerLanguage(db)

	if len(missing) == 0 {
		return "en" // fallback to English
	}

	// Priority languages (more likely to have good LLM support)
	var priorityLangs map[string]bool
	if !noPriority {
		priorityLangs = map[string]bool{
			"en": true, "es": true, "fr": true, "de": true, "it": true,
			"pt": true, "ru": true, "ja": true, "zh": true, "ar": true,
			"fa": true, "tr": true, "hi": true, "ko": true,
		}
	} else {
		priorityLangs = make(map[string]bool) // Empty map - no priorities
	}

	minMissing := -1
	optimalLang := "en"

	// First pass: look for priority languages
	for lang, count := range missing {
		if count > 0 && priorityLangs[lang] {
			if minMissing == -1 || count < minMissing {
				minMissing = count
				optimalLang = lang
			}
		}
	}

	// If no priority language found with missing prayers, use any language
	if minMissing == -1 {
		for lang, count := range missing {
			if count > 0 && (minMissing == -1 || count < minMissing) {
				minMissing = count
				optimalLang = lang
			}
		}
	}

	return optimalLang
}

// Get all languages sorted by priority (missing prayers count + preference)
func getLanguagesPrioritized(db Database, noPriority bool) []string {
	missing := calculateMissingPrayersPerLanguage(db)

	// Priority languages (more likely to have good LLM support)
	var priorityLangs map[string]bool
	if !noPriority {
		priorityLangs = map[string]bool{
			"en": true, "es": true, "fr": true, "de": true, "it": true,
			"pt": true, "ru": true, "ja": true, "zh": true, "ar": true,
			"fa": true, "tr": true, "hi": true, "ko": true,
		}
	} else {
		priorityLangs = make(map[string]bool) // Empty map - no priorities
	}

	type langPriority struct {
		lang     string
		missing  int
		priority bool
	}

	var langs []langPriority
	for lang, count := range missing {
		if count > 0 {
			langs = append(langs, langPriority{lang, count, priorityLangs[lang]})
		}
	}

	// Sort by priority (priority languages first), then by missing count (ascending)
	sort.Slice(langs, func(i, j int) bool {
		if langs[i].priority != langs[j].priority {
			return langs[i].priority // priority languages first
		}
		return langs[i].missing < langs[j].missing // fewer missing first
	})

	result := make([]string, len(langs))
	for i, lang := range langs {
		result[i] = lang.lang
	}
	return result
}

// Process random prayers from all languages
func processRandomPrayers(db *Database, referenceLanguage string, useGemini bool, reportFile *os.File, maxPrayers int, verbose bool, maxRounds int) ([]Writing, error) {
	// Collect all unmatched prayers from all languages
	var allUnmatched []Writing
	for _, writing := range db.Writing {
		if writing.Phelps == "" || strings.TrimSpace(writing.Phelps) == "" {
			allUnmatched = append(allUnmatched, writing)
		}
	}

	if len(allUnmatched) == 0 {
		fmt.Printf("No unmatched prayers found across all languages!\n")
		return []Writing{}, nil
	}

	// Shuffle the prayers for randomness
	rand.Shuffle(len(allUnmatched), func(i, j int) {
		allUnmatched[i], allUnmatched[j] = allUnmatched[j], allUnmatched[i]
	})

	totalToProcess := len(allUnmatched)
	if maxPrayers > 0 && maxPrayers < totalToProcess {
		totalToProcess = maxPrayers
		allUnmatched = allUnmatched[:maxPrayers]
	}

	fmt.Printf("🎲 Lucky mode: Processing %d random prayers from all languages\n", totalToProcess)
	fmt.Fprintf(reportFile, "=== LUCKY MODE: Random Prayer Processing ===\n")
	fmt.Fprintf(reportFile, "Processing %d random prayers from all languages at %s\n\n", totalToProcess, time.Now().Format(time.RFC3339))

	// Process the shuffled prayers
	return processShuffledPrayers(db, allUnmatched, referenceLanguage, useGemini, reportFile, verbose, maxRounds)
}

// Process languages continuously in priority order with mode support
func processLanguagesContinuouslyWithMode(db *Database, referenceLanguage string, useGemini bool, reportFile *os.File, maxPrayers int, verbose, noPriority, legacyMode bool, maxRounds int, operationMode string) ([]Writing, error) {
	languages := getLanguagesPrioritized(*db, noPriority)

	if len(languages) == 0 {
		fmt.Printf("No languages with missing prayers found!\n")
		return []Writing{}, nil
	}

	if noPriority {
		fmt.Printf("🔄 Continue mode: Processing languages by missing count (smallest first)\n")
	} else {
		fmt.Printf("🔄 Continue mode: Processing languages in priority order\n")
	}
	fmt.Printf("Language queue: %v\n", languages[:min(5, len(languages))])
	if len(languages) > 5 {
		fmt.Printf("... and %d more\n", len(languages)-5)
	}

	fmt.Fprintf(reportFile, "=== CONTINUE MODE: Continuous Language Processing ===\n")
	if noPriority {
		fmt.Fprintf(reportFile, "Processing languages by missing count (smallest first) at %s\n", time.Now().Format(time.RFC3339))
	} else {
		fmt.Fprintf(reportFile, "Processing languages in priority order at %s\n", time.Now().Format(time.RFC3339))
	}
	fmt.Fprintf(reportFile, "Language queue: %v\n\n", languages)

	var allUnmatched []Writing
	totalProcessed := 0

	// Get missing counts for all languages
	missing := calculateMissingPrayersPerLanguage(*db)

	for i, lang := range languages {
		if maxPrayers > 0 && totalProcessed >= maxPrayers {
			fmt.Printf("Reached maximum prayer limit (%d). Stopping.\n", maxPrayers)
			break
		}

		remainingQuota := 0
		if maxPrayers > 0 {
			remainingQuota = maxPrayers - totalProcessed
		}

		fmt.Printf("\n--- Processing language %d/%d: %s (%d missing) ---\n", i+1, len(languages), lang, missing[lang])
		fmt.Fprintf(reportFile, "\n--- Language %d/%d: %s (%d missing) ---\n", i+1, len(languages), lang, missing[lang])

		unmatchedForLang, err := processPrayersForLanguageWithMode(db, lang, referenceLanguage, useGemini, reportFile, remainingQuota, verbose, legacyMode, maxRounds, operationMode)
		if err != nil {
			log.Printf("Error processing language %s: %v", lang, err)
			continue
		}

		allUnmatched = append(allUnmatched, unmatchedForLang...)

		// Count how many prayers were actually processed (not just unmatched)
		processed := missing[lang] - len(unmatchedForLang)
		totalProcessed += processed

		fmt.Printf("✅ Completed language %s: processed %d prayers\n", lang, processed)

		if atomic.LoadInt32(&stopRequested) == 1 {
			fmt.Printf("Stop requested. Processed %d languages so far.\n", i+1)
			break
		}
	}

	return allUnmatched, nil
}

// Helper function to process a list of shuffled prayers
func processShuffledPrayers(db *Database, prayers []Writing, referenceLanguage string, useGemini bool, reportFile *os.File, verbose bool, maxRounds int) ([]Writing, error) {
	return processShuffledPrayersWithMode(db, prayers, referenceLanguage, useGemini, reportFile, verbose, false, maxRounds, "match")
}

// Process random prayers from all languages with mode support
func processRandomPrayersWithMode(db *Database, referenceLanguage string, useGemini bool, reportFile *os.File, maxPrayers int, verbose, legacyMode bool, maxRounds int, operationMode string) ([]Writing, error) {
	// Collect all unmatched prayers from all languages
	var allUnmatched []Writing
	for _, writing := range db.Writing {
		if writing.Phelps == "" || strings.TrimSpace(writing.Phelps) == "" {
			allUnmatched = append(allUnmatched, writing)
		}
	}

	if len(allUnmatched) == 0 {
		fmt.Printf("No unmatched prayers found across all languages!\n")
		return []Writing{}, nil
	}

	// Shuffle the prayers for randomness
	rand.Shuffle(len(allUnmatched), func(i, j int) {
		allUnmatched[i], allUnmatched[j] = allUnmatched[j], allUnmatched[i]
	})

	totalToProcess := len(allUnmatched)
	if maxPrayers > 0 && maxPrayers < totalToProcess {
		totalToProcess = maxPrayers
		allUnmatched = allUnmatched[:maxPrayers]
	}

	fmt.Printf("🎲 Lucky mode: Processing %d random prayers from all languages\n", totalToProcess)
	fmt.Fprintf(reportFile, "=== LUCKY MODE: Random Prayer Processing ===\n")
	fmt.Fprintf(reportFile, "Processing %d random prayers from all languages at %s\n\n", totalToProcess, time.Now().Format(time.RFC3339))

	// Process the shuffled prayers
	return processShuffledPrayersWithMode(db, allUnmatched, referenceLanguage, useGemini, reportFile, verbose, legacyMode, maxRounds, operationMode)
}

func processShuffledPrayersWithMode(db *Database, prayers []Writing, referenceLanguage string, useGemini bool, reportFile *os.File, verbose bool, legacyMode bool, maxRounds int, operationMode string) ([]Writing, error) {
	var unmatchedPrayers []Writing

	for i, writing := range prayers {
		if atomic.LoadInt32(&stopRequested) == 1 {
			fmt.Printf("Graceful stop requested. Processed %d prayers so far.\n", i)
			fmt.Fprintf(reportFile, "Graceful stop requested at %s. Processed %d prayers.\n", time.Now().Format(time.RFC3339), i)
			break
		}

		fmt.Printf("\n📿 Processing prayer %d/%d: %s\n", i+1, len(prayers), writing.Name)
		fmt.Printf("   Language: %s | Version: %s\n", writing.Language, writing.Version)
		if len(writing.Text) > 150 {
			fmt.Printf("   Preview: %s...\n", writing.Text[:150])
		} else {
			fmt.Printf("   Text: %s\n", writing.Text)
		}

		// Use the appropriate header for this prayer's language
		languageSpecificHeader := prepareLLMHeaderWithMode(*db, writing.Language, referenceLanguage, operationMode)
		prompt := languageSpecificHeader + "\n\nPrayer text to analyze:\n" + writing.Text

		// Add transliteration context if needed
		prompt = addTransliterationContext(*db, writing, prompt, operationMode)

		fmt.Fprintf(reportFile, "Processing writing: %s (%s) (Version: %s)\n", writing.Name, writing.Language, writing.Version)

		fmt.Printf("   🧠 Analyzing with LLM...")

		var response LLMResponse
		var err error

		if legacyMode {
			// Use old prompt with all contexts
			oldPrompt := prepareLLMHeaderLegacy(*db, writing.Language, referenceLanguage) + "\n\nPrayer text to analyze:\n" + writing.Text
			response, err = callLLM(oldPrompt, useGemini, len(writing.Text))
		} else {
			// Use new interactive mode
			response, err = callLLMInteractive(*db, referenceLanguage, prompt, useGemini, len(writing.Text), maxRounds, "match")
		}
		if err != nil {
			fmt.Fprintf(reportFile, "  ERROR: LLM call failed: %v\n", err)
			if verbose {
				fmt.Printf(" ERROR: %v\n", err)
			} else {
				log.Printf("Error processing %s: %v", writing.Version, err)
			}

			// Create a fallback response for unknown matches
			response = LLMResponse{
				PhelpsCode: "UNKNOWN",
				Confidence: 0.0,
				Reasoning:  fmt.Sprintf("LLM service error: %v", err),
			}
		}

		// Always show the final decision clearly
		fmt.Printf("\n  🎯 FINAL DECISION:\n")
		fmt.Printf("     Prayer: %s (%s)\n", writing.Name, writing.Language)
		fmt.Printf("     Result: %s\n", response.PhelpsCode)
		fmt.Printf("     Confidence: %.1f%%\n", response.Confidence*100)
		fmt.Printf("     Reasoning: %s\n", response.Reasoning)

		if verbose {
			fmt.Printf("  ✓ Analysis complete!\n")
		}

		if response.PhelpsCode != "UNKNOWN" && response.Confidence >= 0.70 {
			if verbose {
				fmt.Printf("  MATCHED: %s -> database updated\n", response.PhelpsCode)
			}

			err := updateWritingPhelps(response.PhelpsCode, writing.Language, writing.Version)
			if err != nil {
				log.Printf("Error updating database: %v", err)
				fmt.Fprintf(reportFile, "  ERROR updating database: %v\n", err)
			} else {
				fmt.Fprintf(reportFile, "  MATCHED: %s (%.1f%% confidence) -> database updated\n", response.PhelpsCode, response.Confidence)
			}
		} else {
			unmatchedPrayers = append(unmatchedPrayers, writing)
			if verbose {
				if response.PhelpsCode == "UNKNOWN" {
					fmt.Printf("  UNMATCHED: %s\n", response.Reasoning)
				} else {
					fmt.Printf("  LOW CONFIDENCE: %s (%.1f%%) - added to unmatched list\n", response.PhelpsCode, response.Confidence)
				}
			}
			fmt.Fprintf(reportFile, "  UNMATCHED: %s (%.1f%% confidence) - %s\n", response.PhelpsCode, response.Confidence, response.Reasoning)
		}

		if verbose {
			fmt.Printf("  Waiting 1 second...\n")
		}
		time.Sleep(1 * time.Second)
	}

	return unmatchedPrayers, nil
}

// Interactive assignment for unmatched prayers
func interactiveAssignment(db *Database, unmatchedPrayers []Writing, reportFile *os.File) {
	if len(unmatchedPrayers) == 0 {
		return
	}

	fmt.Printf("\n=== Interactive Assignment for %d Unmatched Prayers ===\n", len(unmatchedPrayers))
	fmt.Fprintf(reportFile, "\n=== Interactive Assignment Session ===\n")
	fmt.Fprintf(reportFile, "Started at: %s\n", time.Now().Format(time.RFC3339))

	scanner := bufio.NewScanner(os.Stdin)

	// Get available Phelps codes for reference
	phelpsSet := make(map[string]bool)
	for _, writing := range db.Writing {
		if writing.Phelps != "" {
			phelpsSet[writing.Phelps] = true
		}
	}

	var phelpsList []string
	for phelps := range phelpsSet {
		phelpsList = append(phelpsList, phelps)
	}
	sort.Strings(phelpsList)

	assigned := 0
	skipped := 0

	for i, prayer := range unmatchedPrayers {
		fmt.Printf("\n--- Prayer %d of %d ---\n", i+1, len(unmatchedPrayers))
		fmt.Printf("Name: %s\n", prayer.Name)
		fmt.Printf("Language: %s\n", prayer.Language)
		fmt.Printf("Version: %s\n", prayer.Version)
		fmt.Printf("Text (first 200 chars): %s...\n", truncateString(prayer.Text, 200))

		fmt.Printf("\nAvailable Phelps codes: %s\n", strings.Join(phelpsList[:min(10, len(phelpsList))], ", "))
		if len(phelpsList) > 10 {
			fmt.Printf("... and %d more (type 'list' to see all)\n", len(phelpsList)-10)
		}

		for {
			fmt.Printf("\nEnter Phelps code (or 'skip', 'quit', 'list'): ")
			if !scanner.Scan() {
				break
			}

			input := strings.TrimSpace(scanner.Text())

			switch strings.ToLower(input) {
			case "quit", "q", "exit":
				fmt.Printf("Exiting interactive assignment. Assigned: %d, Skipped: %d\n", assigned, skipped)
				fmt.Fprintf(reportFile, "Interactive session ended early. Assigned: %d, Skipped: %d\n", assigned, skipped)
				return
			case "skip", "s", "":
				skipped++
				fmt.Fprintf(reportFile, "  SKIPPED: %s (Version: %s)\n", prayer.Name, prayer.Version)
				goto nextPrayer
			case "list", "l":
				fmt.Printf("All available Phelps codes:\n")
				for j, code := range phelpsList {
					fmt.Printf("  %s", code)
					if (j+1)%5 == 0 {
						fmt.Printf("\n")
					} else {
						fmt.Printf("  ")
					}
				}
				fmt.Printf("\n")
				continue
			default:
				// Validate Phelps code
				if phelpsSet[input] {
					// Update the prayer
					if err := updateWritingPhelps(input, prayer.Language, prayer.Version); err != nil {
						fmt.Printf("Error updating prayer: %v\n", err)
						fmt.Fprintf(reportFile, "  ERROR: Failed to update %s: %v\n", prayer.Version, err)
						continue
					}
					assigned++
					fmt.Printf("✓ Assigned %s to %s\n", input, prayer.Name)
					fmt.Fprintf(reportFile, "  ASSIGNED: %s -> %s (Version: %s)\n", input, prayer.Name, prayer.Version)
					goto nextPrayer
				} else {
					fmt.Printf("Invalid Phelps code. Please enter a valid code or 'list' to see options.\n")
					continue
				}
			}
		}
	nextPrayer:
	}

	fmt.Printf("\nInteractive assignment completed. Assigned: %d, Skipped: %d\n", assigned, skipped)
	fmt.Fprintf(reportFile, "Interactive assignment completed. Assigned: %d, Skipped: %d\n", assigned, skipped)

	// Commit interactive changes if any
	if assigned > 0 {
		commitMessage := fmt.Sprintf("Interactive prayer assignment: %d prayers assigned", assigned)
		if err := execDoltRun("add", "."); err != nil {
			fmt.Fprintf(reportFile, "  ERROR: Failed to stage interactive changes: %v\n", err)
		} else {
			if err := execDoltRun("commit", "-m", commitMessage); err != nil {
				fmt.Fprintf(reportFile, "  ERROR: Failed to commit interactive changes: %v\n", err)
			} else {
				fmt.Fprintf(reportFile, "  SUCCESS: Interactive changes committed: %s\n", commitMessage)
			}
		}
	}
}

// Utility functions
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Call LLM (Gemini first, then Ollama fallback)
func callLLM(prompt string, useGemini bool, textLength int) (LLMResponse, error) {
	return callLLMWithCaller(prompt, useGemini, textLength, DefaultLLMCaller{})
}

// callLLMWithCaller allows dependency injection for testing
func callLLMWithCaller(prompt string, useGemini bool, textLength int, caller LLMCaller) (LLMResponse, error) {
	var response string
	var geminiErr error
	var ollamaErr error
	var geminiResponse string
	var ollamaResponse string
	var triedGemini bool
	var triedOllama bool

	// Check if we should skip Gemini due to quota exceeded or too many arg errors
	if useGemini && atomic.LoadInt32(&geminiQuotaExceeded) == 1 {
		useGemini = false
		log.Printf("Gemini quota exceeded - using only Ollama")
	}
	if useGemini && atomic.LoadInt32(&geminiArgErrors) >= 2 {
		useGemini = false
		log.Printf("Too many Gemini argument errors - using only Ollama")
	}

	if useGemini {
		triedGemini = true
		// Convert prompt to message format for CallGemini
		messages := []OllamaMessage{{Role: "user", Content: prompt}}
		response, geminiErr = caller.CallGemini(messages)
		if geminiErr != nil {
			// Check if this is a quota exceeded error
			errorStr := strings.ToLower(geminiErr.Error())
			if strings.Contains(errorStr, "quota") && (strings.Contains(errorStr, "exceeded") || strings.Contains(errorStr, "exhausted")) {
				atomic.StoreInt32(&geminiQuotaExceeded, 1)
				log.Printf("Gemini quota exceeded - continuing with Ollama only")
			} else if strings.Contains(errorStr, "argument list too long") || strings.Contains(errorStr, "repeated argument list too long errors") {
				log.Printf("Gemini argument list error, falling back to Ollama: %v", geminiErr)
			} else {
				log.Printf("Gemini call failed with error, falling back to Ollama: %v", geminiErr)
			}
		} else {
			geminiResponse = response
			parsed := parseLLMResponse(response)
			// Check if Gemini response is valid
			if parsed.PhelpsCode != "" {
				log.Printf("Gemini returned valid response")
				return parsed, nil
			}
			log.Printf("Gemini returned empty/invalid response (PhelpsCode empty), falling back to Ollama")
			truncatedResponse := truncateAndStore(response, "Gemini")
			log.Printf("Gemini raw response: %q", truncatedResponse)
		}

		// Try Ollama as fallback
		triedOllama = true
		response, ollamaErr = caller.CallOllama(prompt, textLength)
		if ollamaErr != nil {
			// Both failed with errors
			return LLMResponse{}, fmt.Errorf("both LLM services failed - Gemini error: %v, Ollama error: %v", geminiErr, ollamaErr)
		}
		ollamaResponse = response
	} else {
		triedOllama = true
		response, ollamaErr = caller.CallOllama(prompt, textLength)
		ollamaResponse = response
	}

	if ollamaErr != nil {
		if triedGemini {
			return LLMResponse{}, fmt.Errorf("both LLM services failed - Gemini error: %v, Ollama error: %v", geminiErr, ollamaErr)
		} else {
			return LLMResponse{}, fmt.Errorf("Ollama failed: %v", ollamaErr)
		}
	}

	parsed := parseLLMResponse(response)

	// Validate final response
	if parsed.PhelpsCode == "" {
		var debugInfo strings.Builder
		debugInfo.WriteString("All LLM services returned empty or invalid responses.\n")
		if triedGemini {
			debugInfo.WriteString(fmt.Sprintf("Gemini attempted: %v\n", geminiErr == nil))
			if geminiErr != nil {
				debugInfo.WriteString(fmt.Sprintf("Gemini error: %v\n", geminiErr))
			} else {
				truncatedResponse := truncateAndStore(geminiResponse, "Gemini Debug")
				debugInfo.WriteString(fmt.Sprintf("Gemini raw response: %q\n", truncatedResponse))
			}
		}
		if triedOllama {
			debugInfo.WriteString(fmt.Sprintf("Ollama attempted: %v\n", ollamaErr == nil))
			if ollamaErr != nil {
				debugInfo.WriteString(fmt.Sprintf("Ollama error: %v\n", ollamaErr))
			} else {
				truncatedResponse := truncateAndStore(ollamaResponse, "Ollama Debug")
				debugInfo.WriteString(fmt.Sprintf("Ollama raw response: %q\n", truncatedResponse))
			}
		}
		debugInfo.WriteString(fmt.Sprintf("Prompt used: %q\n", prompt))

		return LLMResponse{
			PhelpsCode: "UNKNOWN",
			Confidence: 0.0,
			Reasoning:  fmt.Sprintf("LLM returned empty or invalid response. Debug info:\n%s", debugInfo.String()),
		}, nil
	}

	return parsed, nil
}

func MustInt(s string) int {
	i, err := strconv.Atoi(s)
	if err != nil {
		panic(err)
	}
	return i
}

func MustFloat(s string) float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		panic(err)
	}
	return f
}

// Schema Overview:
// languages: langcode (PK), inlang, name
// prayer_heuristics: id (PK), phelps_code, prayer_name, language_pattern, text_pattern, pattern_type, confidence_level, notes, created_date, validated, match_count
// prayer_match_candidates: id (PK), version_id, proposed_name, proposed_phelps, language, text_length, reference_length, length_ratio, confidence_score, validation_status, validation_notes, created_date
// writings: phelps, language, version (PK), name, type, notes, link, text, source, source_id
// Indexes: writings.lookup on (phelps, language)

type Language struct {
	LangCode string
	InLang   string
	Name     string
}

type Writing struct {
	Phelps   string
	Language string
	Version  string
	Name     string
	Type     string
	Notes    string
	Link     string
	Text     string
	Source   string
	SourceID string
}

func parseWriting(line string) (Writing, error) {
	r := csv.NewReader(strings.NewReader(line))
	r.FieldsPerRecord = 10
	rec, err := r.Read()
	if err != nil {
		return Writing{}, err
	}
	return Writing{
		Phelps:   rec[0],
		Language: rec[1],
		Version:  rec[2],
		Name:     rec[3],
		Type:     rec[4],
		Notes:    rec[5],
		Link:     rec[6],
		Text:     rec[7],
		Source:   rec[8],
		SourceID: rec[9],
	}, nil
}

func parseLanguage(line string) (Language, error) {
	r := csv.NewReader(strings.NewReader(line))
	r.FieldsPerRecord = 3
	rec, err := r.Read()
	if err != nil {
		return Language{}, err
	}
	return Language{
		LangCode: rec[0],
		InLang:   rec[1],
		Name:     rec[2],
	}, nil
}

type MatchAttempt struct {
	ID                    int
	VersionID             string
	TargetLanguage        string
	ReferenceLanguage     string
	AttemptTimestamp      string
	ResultType            string
	PhelpsCode            string
	ConfidenceScore       float64
	Reasoning             string
	LLMProvider           string
	LLMModel              string
	ProcessingTimeSeconds int
	FailureReason         string
}

type LanguagePairStats struct {
	ID                    int
	TargetLanguage        string
	ReferenceLanguage     string
	TotalAttempts         int
	SuccessfulMatches     int
	FailedAttempts        int
	LowConfidenceAttempts int
	UnknownAttempts       int
	SuccessRate           float64
	AvgConfidence         float64
	AvgProcessingTime     float64
	LastAttemptTimestamp  string
	CreatedTimestamp      string
	UpdatedTimestamp      string
}

type SkipListEntry struct {
	ID                          int
	VersionID                   string
	TargetLanguage              string
	SkipReason                  string
	SkipTimestamp               string
	AttemptedReferenceLanguages string
	Notes                       string
	SkipUntil                   string
}

type Database struct {
	Writing           []Writing
	Language          []Language
	MatchAttempts     []MatchAttempt
	LanguagePairStats []LanguagePairStats
	SkipList          []SkipListEntry
	Skipped           map[string]int
}

func GetDatabase() Database {
	// Shell out to Dolt database and read in the data to populate the in-memory database
	db := Database{
		Writing:           []Writing{},
		Language:          []Language{},
		MatchAttempts:     []MatchAttempt{},
		LanguagePairStats: []LanguagePairStats{},
		SkipList:          []SkipListEntry{},
		Skipped:           make(map[string]int),
	}

	// Helper to run a dolt query and return the resulting CSV output
	runQuery := func(table string, columns string) (string, error) {
		query := fmt.Sprintf("SELECT %s FROM %s", columns, table)
		out, err := execDoltQueryCSV(query)
		if err != nil {
			return "", fmt.Errorf("dolt query for %s failed: %w", table, err)
		}
		return string(out), nil
	}

	// Load Writing data
	if csvOut, err := runQuery("writings", "phelps,language,version,name,type,notes,link,text,source,source_id"); err != nil {
		log.Fatalf("Failed to load writing data: %v", err)
	} else {
		r := csv.NewReader(strings.NewReader(csvOut))
		r.FieldsPerRecord = 10
		r.LazyQuotes = true
		records, err := r.ReadAll()
		if err != nil {
			log.Fatalf("Failed to parse writing CSV: %v", err)
		}
		if len(records) > 0 {
			records = records[1:] // skip header
		}
		for _, rec := range records {
			w := Writing{
				Phelps:   rec[0],
				Language: rec[1],
				Version:  rec[2],
				Name:     rec[3],
				Type:     rec[4],
				Notes:    rec[5],
				Link:     rec[6],
				Text:     rec[7],
				Source:   rec[8],
				SourceID: rec[9],
			}
			db.Writing = append(db.Writing, w)
		}
	}

	// Load Language data
	if csvOut, err := runQuery("languages", "langcode,inlang,name"); err != nil {
		log.Fatalf("Failed to load language data: %v", err)
	} else {
		r := csv.NewReader(strings.NewReader(csvOut))
		r.FieldsPerRecord = 3
		records, err := r.ReadAll()
		if err != nil {
			log.Fatalf("Failed to parse language CSV: %v", err)
		}
		if len(records) > 0 {
			records = records[1:]
		}
		for _, rec := range records {
			l := Language{
				LangCode: rec[0],
				InLang:   rec[1],
				Name:     rec[2],
			}
			db.Language = append(db.Language, l)
		}
	}

	// Load MatchAttempts data
	if csvOut, err := runQuery("match_attempts", "id,version_id,target_language,reference_language,attempt_timestamp,result_type,phelps_code,confidence_score,reasoning,llm_provider,llm_model,processing_time_seconds,failure_reason"); err != nil {
		log.Printf("Warning: Failed to load match attempts data: %v", err)
	} else {
		r := csv.NewReader(strings.NewReader(csvOut))
		r.FieldsPerRecord = 13
		records, err := r.ReadAll()
		if err != nil {
			log.Printf("Warning: Failed to parse match attempts CSV: %v", err)
		} else {
			if len(records) > 0 {
				records = records[1:]
			}
			for _, rec := range records {
				ma := MatchAttempt{
					ID:                    MustInt(rec[0]),
					VersionID:             rec[1],
					TargetLanguage:        rec[2],
					ReferenceLanguage:     rec[3],
					AttemptTimestamp:      rec[4],
					ResultType:            rec[5],
					PhelpsCode:            rec[6],
					ConfidenceScore:       MustFloat(rec[7]),
					Reasoning:             rec[8],
					LLMProvider:           rec[9],
					LLMModel:              rec[10],
					ProcessingTimeSeconds: MustInt(rec[11]),
					FailureReason:         rec[12],
				}
				db.MatchAttempts = append(db.MatchAttempts, ma)
			}
		}
	}

	// Load LanguagePairStats data
	if csvOut, err := runQuery("language_pair_stats", "id,target_language,reference_language,total_attempts,successful_matches,failed_attempts,low_confidence_attempts,unknown_attempts,success_rate,avg_confidence,avg_processing_time,last_attempt_timestamp,created_timestamp,updated_timestamp"); err != nil {
		log.Printf("Warning: Failed to load language pair stats data: %v", err)
	} else {
		r := csv.NewReader(strings.NewReader(csvOut))
		r.FieldsPerRecord = 14
		records, err := r.ReadAll()
		if err != nil {
			log.Printf("Warning: Failed to parse language pair stats CSV: %v", err)
		} else {
			if len(records) > 0 {
				records = records[1:]
			}
			for _, rec := range records {
				lps := LanguagePairStats{
					ID:                    MustInt(rec[0]),
					TargetLanguage:        rec[1],
					ReferenceLanguage:     rec[2],
					TotalAttempts:         MustInt(rec[3]),
					SuccessfulMatches:     MustInt(rec[4]),
					FailedAttempts:        MustInt(rec[5]),
					LowConfidenceAttempts: MustInt(rec[6]),
					UnknownAttempts:       MustInt(rec[7]),
					SuccessRate:           MustFloat(rec[8]),
					AvgConfidence:         MustFloat(rec[9]),
					AvgProcessingTime:     MustFloat(rec[10]),
					LastAttemptTimestamp:  rec[11],
					CreatedTimestamp:      rec[12],
					UpdatedTimestamp:      rec[13],
				}
				db.LanguagePairStats = append(db.LanguagePairStats, lps)
			}
		}
	}

	// Load SkipList data
	if csvOut, err := runQuery("skip_list", "id,version_id,target_language,skip_reason,skip_timestamp,attempted_reference_languages,notes,skip_until"); err != nil {
		log.Printf("Warning: Failed to load skip list data: %v", err)
	} else {
		r := csv.NewReader(strings.NewReader(csvOut))
		r.FieldsPerRecord = 8
		records, err := r.ReadAll()
		if err != nil {
			log.Printf("Warning: Failed to parse skip list CSV: %v", err)
		} else {
			if len(records) > 0 {
				records = records[1:]
			}
			for _, rec := range records {
				sle := SkipListEntry{
					ID:                          MustInt(rec[0]),
					VersionID:                   rec[1],
					TargetLanguage:              rec[2],
					SkipReason:                  rec[3],
					SkipTimestamp:               rec[4],
					AttemptedReferenceLanguages: rec[5],
					Notes:                       rec[6],
					SkipUntil:                   rec[7],
				}
				db.SkipList = append(db.SkipList, sle)
			}
		}
	}

	return db
}

// Insert match attempt record
func insertMatchAttempt(attempt MatchAttempt) error {
	// Escape strings for SQL injection prevention
	escapedVersionID := strings.ReplaceAll(attempt.VersionID, "'", "''")
	escapedTargetLang := strings.ReplaceAll(attempt.TargetLanguage, "'", "''")
	escapedRefLang := strings.ReplaceAll(attempt.ReferenceLanguage, "'", "''")
	escapedResultType := strings.ReplaceAll(attempt.ResultType, "'", "''")
	escapedPhelps := strings.ReplaceAll(attempt.PhelpsCode, "'", "''")
	escapedReasoning := strings.ReplaceAll(attempt.Reasoning, "'", "''")
	escapedProvider := strings.ReplaceAll(attempt.LLMProvider, "'", "''")
	escapedModel := strings.ReplaceAll(attempt.LLMModel, "'", "''")
	escapedFailureReason := strings.ReplaceAll(attempt.FailureReason, "'", "''")

	// Shell out to Dolt to insert the record
	query := fmt.Sprintf(`INSERT INTO match_attempts
		(version_id, target_language, reference_language, result_type, phelps_code,
		 confidence_score, reasoning, llm_provider, llm_model, processing_time_seconds, failure_reason)
		VALUES ('%s', '%s', '%s', '%s', '%s', %.3f, '%s', '%s', '%s', %d, '%s')`,
		escapedVersionID, escapedTargetLang, escapedRefLang, escapedResultType, escapedPhelps,
		attempt.ConfidenceScore, escapedReasoning, escapedProvider, escapedModel,
		attempt.ProcessingTimeSeconds, escapedFailureReason)

	if _, err := execDoltQuery(query); err != nil {
		return fmt.Errorf("failed to insert match attempt: %w", err)
	}

	return nil
}

// Check if a prayer should be skipped based on skip list and failed attempts
func shouldSkipPrayer(db Database, versionID, targetLanguage string) (bool, string) {
	// Check skip list first
	for _, skip := range db.SkipList {
		if skip.VersionID == versionID && skip.TargetLanguage == targetLanguage {
			// Check if skip is still valid (skip_until)
			if skip.SkipUntil != "" {
				skipUntil, err := time.Parse("2006-01-02 15:04:05", skip.SkipUntil)
				if err == nil && time.Now().Before(skipUntil) {
					return true, fmt.Sprintf("Skipped until %s: %s", skip.SkipUntil, skip.SkipReason)
				}
			} else {
				return true, fmt.Sprintf("Permanently skipped: %s", skip.SkipReason)
			}
		}
	}

	// Check for repeated failures across multiple reference languages
	failureCount := 0
	attemptedRefLangs := make(map[string]bool)

	for _, attempt := range db.MatchAttempts {
		if attempt.VersionID == versionID && attempt.TargetLanguage == targetLanguage {
			if attempt.ResultType == "failure" || attempt.ResultType == "unknown" {
				failureCount++
				attemptedRefLangs[attempt.ReferenceLanguage] = true
			}
		}
	}

	// If we've failed with 3 or more different reference languages, suggest skipping
	if failureCount >= 3 && len(attemptedRefLangs) >= 3 {
		return false, fmt.Sprintf("Warning: %d failures with %d reference languages", failureCount, len(attemptedRefLangs))
	}

	return false, ""
}

// Find optimal reference language for a target language, avoiding same-language matching
func findOptimalReferenceLanguage(db Database, targetLanguage string, availableLanguages []string) string {
	// First, exclude same language
	if targetLanguage == "en" {
		// If target is English, prefer other major languages
		for _, refLang := range []string{"es", "fr", "de", "it", "pt", "ar", "fa"} {
			if contains(availableLanguages, refLang) {
				return refLang
			}
		}
	}

	// Look for language pair stats to find best performing reference language
	bestRefLang := ""
	bestSuccessRate := -1.0
	bestAttempts := 0

	for _, stats := range db.LanguagePairStats {
		if stats.TargetLanguage == targetLanguage &&
			stats.ReferenceLanguage != targetLanguage &&
			contains(availableLanguages, stats.ReferenceLanguage) &&
			stats.TotalAttempts >= 3 {

			if stats.SuccessRate > bestSuccessRate ||
				(stats.SuccessRate == bestSuccessRate && stats.TotalAttempts > bestAttempts) {
				bestSuccessRate = stats.SuccessRate
				bestAttempts = stats.TotalAttempts
				bestRefLang = stats.ReferenceLanguage
			}
		}
	}

	if bestRefLang != "" {
		return bestRefLang
	}

	// Fallback to priority languages, avoiding same language
	priorityLangs := []string{"en", "es", "fr", "de", "it", "pt", "ru", "ja", "zh", "ar", "fa", "tr", "hi", "ko"}
	for _, refLang := range priorityLangs {
		if refLang != targetLanguage && contains(availableLanguages, refLang) {
			return refLang
		}
	}

	// Last resort: use any available language except target language
	for _, refLang := range availableLanguages {
		if refLang != targetLanguage {
			return refLang
		}
	}

	// If all else fails and we have no choice, use the target language but log a warning
	log.Printf("Warning: No alternative reference language found for target language %s, using same language", targetLanguage)
	return targetLanguage
}

// Helper function to check if a slice contains a string
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// Update language pair statistics based on match attempt
func updateLanguagePairStats(attempt MatchAttempt) error {
	// First, check if stats already exist for this language pair
	query := fmt.Sprintf(`SELECT id FROM language_pair_stats
		WHERE target_language = '%s' AND reference_language = '%s'`,
		strings.ReplaceAll(attempt.TargetLanguage, "'", "''"),
		strings.ReplaceAll(attempt.ReferenceLanguage, "'", "''"))

	output, err := execDoltQueryCSV(query)
	if err != nil {
		return fmt.Errorf("failed to check existing language pair stats: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	statsExist := len(lines) > 1 // Header + at least one row

	if statsExist {
		// Update existing stats
		updateQuery := fmt.Sprintf(`UPDATE language_pair_stats
			SET total_attempts = total_attempts + 1,
				successful_matches = successful_matches + %s,
				failed_attempts = failed_attempts + %s,
				low_confidence_attempts = low_confidence_attempts + %s,
				unknown_attempts = unknown_attempts + %s,
				success_rate = CASE WHEN (total_attempts + 1) > 0
					THEN (successful_matches + %s) / (total_attempts + 1)
					ELSE 0 END,
				avg_confidence = (avg_confidence * total_attempts + %.3f) / (total_attempts + 1),
				avg_processing_time = (avg_processing_time * total_attempts + %.2f) / (total_attempts + 1),
				last_attempt_timestamp = CURRENT_TIMESTAMP,
				updated_timestamp = CURRENT_TIMESTAMP
			WHERE target_language = '%s' AND reference_language = '%s'`,
			map[string]string{"success": "1", "failure": "0", "low_confidence": "0", "unknown": "0"}[attempt.ResultType],
			map[string]string{"success": "0", "failure": "1", "low_confidence": "0", "unknown": "0"}[attempt.ResultType],
			map[string]string{"success": "0", "failure": "0", "low_confidence": "1", "unknown": "0"}[attempt.ResultType],
			map[string]string{"success": "0", "failure": "0", "low_confidence": "0", "unknown": "1"}[attempt.ResultType],
			map[string]string{"success": "1", "failure": "0", "low_confidence": "0", "unknown": "0"}[attempt.ResultType],
			attempt.ConfidenceScore,
			float64(attempt.ProcessingTimeSeconds),
			strings.ReplaceAll(attempt.TargetLanguage, "'", "''"),
			strings.ReplaceAll(attempt.ReferenceLanguage, "'", "''"))

		if _, err := execDoltQuery(updateQuery); err != nil {
			err = fmt.Errorf("failed to update language pair stats: %w", err)
		}
	} else {
		// Insert new stats
		insertQuery := fmt.Sprintf(`INSERT INTO language_pair_stats
			(target_language, reference_language, total_attempts, successful_matches,
			 failed_attempts, low_confidence_attempts, unknown_attempts, success_rate,
			 avg_confidence, avg_processing_time, last_attempt_timestamp)
			VALUES ('%s', '%s', 1, %s, %s, %s, %s, %.3f, %.3f, %.2f, CURRENT_TIMESTAMP)`,
			strings.ReplaceAll(attempt.TargetLanguage, "'", "''"),
			strings.ReplaceAll(attempt.ReferenceLanguage, "'", "''"),
			map[string]string{"success": "1", "failure": "0", "low_confidence": "0", "unknown": "0"}[attempt.ResultType],
			map[string]string{"success": "0", "failure": "1", "low_confidence": "0", "unknown": "0"}[attempt.ResultType],
			map[string]string{"success": "0", "failure": "0", "low_confidence": "1", "unknown": "0"}[attempt.ResultType],
			map[string]string{"success": "0", "failure": "0", "low_confidence": "0", "unknown": "1"}[attempt.ResultType],
			func() float64 {
				if attempt.ResultType == "success" {
					return 1.0
				} else {
					return 0.0
				}
			}(),
			attempt.ConfidenceScore,
			float64(attempt.ProcessingTimeSeconds))

		if _, err := execDoltQuery(insertQuery); err != nil {
			err = fmt.Errorf("failed to update language pair stats: %w", err)
		}
	}

	if err != nil {
		return err
	}

	return nil
}

// Check if a prayer should be added to skip list after repeated failures
func checkAndAddToSkipList(db Database, versionID, targetLanguage string) error {
	// Count failures and attempted reference languages for this prayer
	failureCount := 0
	attemptedRefLangs := make(map[string]bool)
	var lastFailureTime time.Time

	for _, attempt := range db.MatchAttempts {
		if attempt.VersionID == versionID && attempt.TargetLanguage == targetLanguage {
			if attempt.ResultType == "failure" || attempt.ResultType == "unknown" {
				failureCount++
				attemptedRefLangs[attempt.ReferenceLanguage] = true

				// Parse timestamp
				if timestamp, err := time.Parse("2006-01-02 15:04:05", attempt.AttemptTimestamp); err == nil {
					if timestamp.After(lastFailureTime) {
						lastFailureTime = timestamp
					}
				}
			}
		}
	}

	// If we have 3+ failures with 3+ different reference languages, add to skip list
	if failureCount >= 3 && len(attemptedRefLangs) >= 3 {
		// Check if already in skip list
		for _, skip := range db.SkipList {
			if skip.VersionID == versionID && skip.TargetLanguage == targetLanguage {
				return nil // Already in skip list
			}
		}

		// Build attempted languages string
		var attemptedLangsList []string
		for lang := range attemptedRefLangs {
			attemptedLangsList = append(attemptedLangsList, lang)
		}
		attemptedLangsStr := strings.Join(attemptedLangsList, ",")

		// Add to skip list
		query := fmt.Sprintf(`INSERT INTO skip_list
			(version_id, target_language, skip_reason, attempted_reference_languages, notes)
			VALUES ('%s', '%s', 'repeated_failures', '%s', 'Auto-added after %d failures with %d reference languages')`,
			strings.ReplaceAll(versionID, "'", "''"),
			strings.ReplaceAll(targetLanguage, "'", "''"),
			strings.ReplaceAll(attemptedLangsStr, "'", "''"),
			failureCount, len(attemptedRefLangs))

		if _, err := execDoltQuery(query); err != nil {
			return fmt.Errorf("failed to add to skip list: %w", err)
		}

		log.Printf("Added prayer %s (%s) to skip list after %d failures with %d reference languages",
			versionID, targetLanguage, failureCount, len(attemptedRefLangs))
	}

	return nil
}

// List available reference languages with statistics
func listReferenceLanguages(db Database) string {
	// Count prayers with Phelps codes by language
	langCounts := make(map[string]int)
	for _, writing := range db.Writing {
		if writing.Phelps != "" && strings.TrimSpace(writing.Phelps) != "" {
			langCounts[writing.Language]++
		}
	}

	if len(langCounts) == 0 {
		return "No reference languages available (no prayers have Phelps codes)."
	}

	// Get success rates from language pair stats
	successRates := make(map[string]float64)
	attemptCounts := make(map[string]int)

	for _, stats := range db.LanguagePairStats {
		if stats.TotalAttempts > 0 {
			key := stats.ReferenceLanguage
			if existingRate, exists := successRates[key]; !exists || stats.SuccessRate > existingRate {
				successRates[key] = stats.SuccessRate
				attemptCounts[key] = stats.TotalAttempts
			}
		}
	}

	// Sort languages by prayer count (descending)
	type langStat struct {
		lang        string
		count       int
		successRate float64
		attempts    int
	}

	var langs []langStat
	for lang, count := range langCounts {
		langs = append(langs, langStat{
			lang:        lang,
			count:       count,
			successRate: successRates[lang],
			attempts:    attemptCounts[lang],
		})
	}

	// Sort by count descending, then by success rate descending
	sort.Slice(langs, func(i, j int) bool {
		if langs[i].count != langs[j].count {
			return langs[i].count > langs[j].count
		}
		return langs[i].successRate > langs[j].successRate
	})

	result := fmt.Sprintf("Available reference languages (%d total):\n", len(langs))

	for i, lang := range langs {
		if i >= 15 { // Show top 15
			result += fmt.Sprintf("... and %d more languages\n", len(langs)-15)
			break
		}

		statsStr := ""
		if lang.attempts > 0 {
			statsStr = fmt.Sprintf(" | Success Rate: %.1f%% (%d attempts)", lang.successRate*100, lang.attempts)
		}

		result += fmt.Sprintf("- %s: %d prayers with Phelps codes%s\n", lang.lang, lang.count, statsStr)
	}

	result += "\nRecommended languages: " + strings.Join([]string{"en", "es", "fr", "de", "ar", "fa"}, ", ")
	result += "\nUse switch_reference_language(language=\"CODE\") to change reference language."

	return result
}

func updateWritingPhelps(phelps, language, version string) error {
	// Escape strings for SQL injection prevention
	escapedPhelps := strings.ReplaceAll(phelps, "'", "''")
	escapedLanguage := strings.ReplaceAll(language, "'", "''")
	escapedVersion := strings.ReplaceAll(version, "'", "''")

	query := fmt.Sprintf(`UPDATE writings SET phelps = '%s' WHERE language = '%s' AND version = '%s'`,
		escapedPhelps, escapedLanguage, escapedVersion)

	if _, err := execDoltQuery(query); err != nil {
		return fmt.Errorf("failed to update database: %v", err)
	}

	return nil
}

func createOrUpdateTransliteration(identifier, targetLang, correctedText string, confidence float64) error {
	// Generate a new UUID for the version
	newVersion := fmt.Sprintf("%s", time.Now().Format("20060102-150405"))

	// Escape strings for SQL injection prevention
	escapedIdentifier := strings.ReplaceAll(identifier, "'", "''")
	escapedLang := strings.ReplaceAll(targetLang, "'", "''")
	escapedText := strings.ReplaceAll(correctedText, "'", "''")
	escapedVersion := strings.ReplaceAll(newVersion, "'", "''")

	// Check if identifier is a version ID or Phelps code
	var checkQuery string
	if strings.Contains(identifier, "-") && len(identifier) > 20 {
		// Looks like a version ID (UUID format)
		checkQuery = fmt.Sprintf("SELECT version FROM writings WHERE version = '%s' AND language = '%s' LIMIT 1", escapedIdentifier, escapedLang)
	} else {
		// Looks like a Phelps code
		checkQuery = fmt.Sprintf("SELECT version FROM writings WHERE phelps = '%s' AND language = '%s' LIMIT 1", escapedIdentifier, escapedLang)
	}

	output, err := execDoltQuery(checkQuery)

	var query string
	if err != nil || len(strings.TrimSpace(string(output))) == 0 {
		// Create new transliteration entry
		if strings.Contains(identifier, "-") && len(identifier) > 20 {
			// Using version ID - need to find the Phelps code from original
			var originalPhelps string
			origQuery := fmt.Sprintf("SELECT phelps FROM writings WHERE version = '%s' LIMIT 1", escapedIdentifier)
			if origOutput, origErr := execDoltQuery(origQuery); origErr == nil && len(strings.TrimSpace(string(origOutput))) > 0 {
				originalPhelps = strings.TrimSpace(string(origOutput))
			}
			query = fmt.Sprintf(`INSERT INTO writings (phelps, language, version, text, name, type, notes, link, source, source_id)
				VALUES ('%s', '%s', '%s', '%s', 'Transliteration (Corrected)', 'prayer', 'AI-corrected transliteration', '', '', '')`,
				strings.ReplaceAll(originalPhelps, "'", "''"), escapedLang, escapedVersion, escapedText)
		} else {
			// Using Phelps code directly
			query = fmt.Sprintf(`INSERT INTO writings (phelps, language, version, text, name, type, notes, link, source, source_id)
				VALUES ('%s', '%s', '%s', '%s', 'Transliteration (Corrected)', 'prayer', 'AI-corrected transliteration', '', '', '')`,
				escapedIdentifier, escapedLang, escapedVersion, escapedText)
		}
	} else {
		// Update existing transliteration
		if strings.Contains(identifier, "-") && len(identifier) > 20 {
			query = fmt.Sprintf(`UPDATE writings SET text = '%s', version = '%s' WHERE version = '%s' AND language = '%s'`,
				escapedText, escapedVersion, escapedIdentifier, escapedLang)
		} else {
			query = fmt.Sprintf(`UPDATE writings SET text = '%s', version = '%s' WHERE phelps = '%s' AND language = '%s'`,
				escapedText, escapedVersion, escapedIdentifier, escapedLang)
		}
	}

	if _, err := execDoltQuery(query); err != nil {
		return fmt.Errorf("failed to create/update transliteration: %v", err)
	}

	return nil
}

// Process prayers for a specific language
func processPrayersForLanguageWithMode(db *Database, targetLanguage, referenceLanguage string, useGemini bool, reportFile *os.File, maxPrayers int, verbose bool, legacyMode bool, maxRounds int, operationMode string) ([]Writing, error) {
	// In translit mode, use the base language for reference
	if operationMode == "translit" {
		originalRef := referenceLanguage
		if strings.HasSuffix(targetLanguage, "-translit") {
			baseLanguage := strings.TrimSuffix(targetLanguage, "-translit")
			referenceLanguage = baseLanguage
			if referenceLanguage != originalRef {
				fmt.Printf("🔤 TRANSLIT mode: Using %s as reference language for %s transliterations\n", referenceLanguage, targetLanguage)
				fmt.Fprintf(reportFile, "TRANSLIT mode: Using %s as reference language for %s transliterations\n", referenceLanguage, targetLanguage)
			}
		}
	} else if referenceLanguage == targetLanguage {
		// Get available reference languages from database
		availableLanguages := make(map[string]bool)
		for _, writing := range db.Writing {
			if writing.Phelps != "" && strings.TrimSpace(writing.Phelps) != "" {
				availableLanguages[writing.Language] = true
			}
		}

		var availableLangs []string
		for lang := range availableLanguages {
			availableLangs = append(availableLangs, lang)
		}

		originalRef := referenceLanguage
		referenceLanguage = findOptimalReferenceLanguage(*db, targetLanguage, availableLangs)

		if referenceLanguage != originalRef {
			fmt.Printf("🔄 Auto-selected reference language: %s -> %s (avoiding same-language matching)\n", originalRef, referenceLanguage)
			fmt.Fprintf(reportFile, "Auto-selected reference language: %s -> %s (avoiding same-language matching)\n", originalRef, referenceLanguage)
		}
	}

	header := prepareLLMHeaderWithMode(*db, targetLanguage, referenceLanguage, operationMode)
	processed := 0
	totalEligible := 0
	var unmatchedPrayers []Writing

	// Count eligible prayers
	for _, writing := range db.Writing {
		if operationMode == "translit" {
			// In translit mode, process transliteration versions
			if writing.Language == targetLanguage && strings.HasSuffix(writing.Language, "-translit") {
				totalEligible++
			}
		} else {
			// In other modes, process prayers with empty Phelps codes
			if writing.Language == targetLanguage && (writing.Phelps == "" || strings.TrimSpace(writing.Phelps) == "") {
				totalEligible++
			}
		}
	}

	if totalEligible == 0 {
		if operationMode == "translit" {
			fmt.Printf("No transliteration prayers found for language: %s\n", targetLanguage)
			fmt.Fprintf(reportFile, "No transliteration prayers found for language: %s\n", targetLanguage)
		} else {
			fmt.Printf("No unmatched prayers found for language: %s\n", targetLanguage)
			fmt.Fprintf(reportFile, "No unmatched prayers found for language: %s\n", targetLanguage)
		}
		return []Writing{}, nil
	}

	maxToShow := totalEligible
	if maxPrayers > 0 && maxPrayers < totalEligible {
		maxToShow = maxPrayers
		fmt.Printf("Found %d eligible prayers to process in language %s\n", totalEligible, targetLanguage)
		fmt.Printf("Will process first %d prayers (limited by -max flag)\n", maxToShow)
	} else {
		fmt.Printf("Found %d prayers to process in language %s\n", totalEligible, targetLanguage)
	}

	for _, writing := range db.Writing {
		var shouldProcess bool
		if operationMode == "translit" {
			// In translit mode, process transliteration versions
			shouldProcess = writing.Language == targetLanguage && strings.HasSuffix(writing.Language, "-translit")
		} else {
			// In other modes, process prayers without Phelps codes
			shouldProcess = writing.Language == targetLanguage && (writing.Phelps == "" || strings.TrimSpace(writing.Phelps) == "")
		}

		if !shouldProcess {
			continue
		}

		if atomic.LoadInt32(&stopRequested) == 1 {
			fmt.Printf("Graceful stop requested. Processed %d prayers so far.\n", processed)
			fmt.Fprintf(reportFile, "Graceful stop requested at %s. Processed %d prayers.\n", time.Now().Format(time.RFC3339), processed)
			break
		}

		if maxPrayers > 0 && processed >= maxPrayers {
			fmt.Printf("Reached maximum prayer limit (%d). Stopping.\n", maxPrayers)
			fmt.Fprintf(reportFile, "Reached maximum prayer limit (%d) at %s.\n", maxPrayers, time.Now().Format(time.RFC3339))
			break
		}

		// Check if we should skip this prayer
		shouldSkip, reason := shouldSkipPrayer(*db, writing.Version, targetLanguage)
		if shouldSkip {
			fmt.Printf("⏭️  Skipping prayer: %s - %s\n", writing.Name, reason)
			fmt.Fprintf(reportFile, "SKIPPED: %s - %s\n", writing.Name, reason)
			continue
		}

		processed++

		maxToProcess := totalEligible
		if maxPrayers > 0 && maxPrayers < totalEligible {
			maxToProcess = maxPrayers
		}

		fmt.Printf("\n📿 Processing prayer %d/%d: %s\n", processed, maxToProcess, writing.Name)
		fmt.Printf("   Language: %s | Version: %s\n", writing.Language, writing.Version)
		if len(writing.Text) > 150 {
			fmt.Printf("   Preview: %s...\n", writing.Text[:150])
		} else {
			fmt.Printf("   Text: %s\n", writing.Text)
		}

		prompt := header + "\n\nPrayer text to analyze:\n" + writing.Text

		// Add transliteration context if needed
		prompt = addTransliterationContext(*db, writing, prompt, operationMode)

		fmt.Fprintf(reportFile, "Processing writing: %s (Version: %s)\n", writing.Name, writing.Version)

		fmt.Printf("   🧠 Analyzing with LLM...")

		startTime := time.Now()
		var response LLMResponse
		var err error

		if legacyMode {
			// Use old prompt with all contexts
			oldPrompt := prepareLLMHeaderLegacy(*db, writing.Language, referenceLanguage) + "\n\nPrayer text to analyze:\n" + writing.Text
			response, err = callLLM(oldPrompt, useGemini, len(writing.Text))
		} else {
			// Use new interactive mode
			response, err = callLLMInteractive(*db, referenceLanguage, prompt, useGemini, len(writing.Text), maxRounds, operationMode)
		}
		if err != nil {
			fmt.Fprintf(reportFile, "  ERROR: LLM call failed: %v\n", err)
			if verbose {
				fmt.Printf(" ERROR: %v\n", err)
			} else {
				log.Printf("Error processing %s: %v", writing.Version, err)
			}

			// Create a fallback response for unknown matches
			response = LLMResponse{
				PhelpsCode: "UNKNOWN",
				Confidence: 0.0,
				Reasoning:  fmt.Sprintf("LLM service error: %v", err),
			}
		}

		// Always show the final decision clearly
		fmt.Printf("\n  🎯 FINAL DECISION:\n")
		fmt.Printf("     Prayer: %s (%s)\n", writing.Name, writing.Language)
		fmt.Printf("     Result: %s\n", response.PhelpsCode)
		fmt.Printf("     Confidence: %.1f%%\n", response.Confidence*100)
		fmt.Printf("     Reasoning: %s\n", response.Reasoning)

		// Record the match attempt
		processingTime := int(time.Since(startTime).Seconds())
		resultType := "unknown"
		failureReason := ""

		if response.PhelpsCode != "UNKNOWN" && response.Confidence >= 0.70 {
			resultType = "success"
		} else if response.PhelpsCode != "UNKNOWN" && response.Confidence < 0.70 {
			resultType = "low_confidence"
		} else if response.PhelpsCode == "UNKNOWN" {
			resultType = "failure"
			if err != nil {
				failureReason = err.Error()
			} else {
				failureReason = "LLM returned UNKNOWN"
			}
		}

		llmProvider := "ollama"
		if useGemini && atomic.LoadInt32(&geminiQuotaExceeded) == 0 {
			llmProvider = "gemini"
		}

		attempt := MatchAttempt{
			VersionID:             writing.Version,
			TargetLanguage:        targetLanguage,
			ReferenceLanguage:     referenceLanguage,
			ResultType:            resultType,
			PhelpsCode:            response.PhelpsCode,
			ConfidenceScore:       response.Confidence,
			Reasoning:             response.Reasoning,
			LLMProvider:           llmProvider,
			LLMModel:              OllamaModel,
			ProcessingTimeSeconds: processingTime,
			FailureReason:         failureReason,
		}

		// Insert attempt record
		if err := insertMatchAttempt(attempt); err != nil {
			log.Printf("Warning: Failed to record match attempt: %v", err)
		}

		// Update language pair statistics
		if err := updateLanguagePairStats(attempt); err != nil {
			log.Printf("Warning: Failed to update language pair stats: %v", err)
		}

		// Check if we should add this prayer to skip list after repeated failures
		if resultType == "failure" || resultType == "unknown" {
			if err := checkAndAddToSkipList(*db, writing.Version, targetLanguage); err != nil {
				log.Printf("Warning: Failed to check skip list: %v", err)
			}
		}

		if verbose {
			fmt.Printf("  ✓ Analysis complete!\n")
		}

		if response.PhelpsCode != "UNKNOWN" && response.Confidence >= 0.70 {
			fmt.Printf("  ✅ MATCHED: %s -> database updated\n", response.PhelpsCode)

			err := updateWritingPhelps(response.PhelpsCode, writing.Language, writing.Version)
			if err != nil {
				log.Printf("Error updating database: %v", err)
				fmt.Fprintf(reportFile, "  ERROR updating database: %v\n", err)
				fmt.Printf("  ❌ Database update failed: %v\n", err)
			} else {
				fmt.Fprintf(reportFile, "  MATCHED: %s (%.1f%% confidence) -> database updated\n", response.PhelpsCode, response.Confidence*100)
			}

			// Apply any pending transliteration corrections
			translitCorrectionsMutex.Lock()
			var appliedCorrections []TranslitCorrection
			for _, correction := range pendingTranslitCorrections {
				if correction.PhelpsCode == response.PhelpsCode {
					err := createOrUpdateTransliteration(correction.PhelpsCode, correction.Language, correction.CorrectedText, correction.Confidence)
					if err != nil {
						log.Printf("Error applying transliteration correction: %v", err)
						fmt.Fprintf(reportFile, "  ERROR applying transliteration correction: %v\n", err)
						fmt.Printf("  ⚠️  Failed to apply transliteration correction: %v\n", err)
					} else {
						fmt.Printf("  ✅ Applied transliteration correction for %s (%s)\n", correction.PhelpsCode, correction.Language)
						fmt.Fprintf(reportFile, "  TRANSLITERATION CORRECTED: %s (%s) -> database updated\n", correction.PhelpsCode, correction.Language)
						appliedCorrections = append(appliedCorrections, correction)
					}
				}
			}

			// Remove applied corrections from pending list
			var remainingCorrections []TranslitCorrection
			for _, correction := range pendingTranslitCorrections {
				found := false
				for _, applied := range appliedCorrections {
					if applied.PhelpsCode == correction.PhelpsCode && applied.Language == correction.Language {
						found = true
						break
					}
				}
				if !found {
					remainingCorrections = append(remainingCorrections, correction)
				}
			}
			pendingTranslitCorrections = remainingCorrections
			translitCorrectionsMutex.Unlock()
		} else {
			unmatchedPrayers = append(unmatchedPrayers, writing)
			if response.PhelpsCode == "UNKNOWN" {
				fmt.Printf("  ❓ UNMATCHED: %s\n", response.Reasoning)
			} else {
				fmt.Printf("  ⚠️  LOW CONFIDENCE: %s (%.1f%%) - added to unmatched list\n", response.PhelpsCode, response.Confidence*100)
			}
			fmt.Fprintf(reportFile, "  UNMATCHED: %s (%.1f%% confidence) - %s\n", response.PhelpsCode, response.Confidence*100, response.Reasoning)
		}

		fmt.Printf("  ⏳ Waiting 1 second before next prayer...\n")
		fmt.Printf("  %s\n", strings.Repeat("-", 80))
		time.Sleep(1 * time.Second)
	}

	return unmatchedPrayers, nil
}

func processPrayersForLanguage(db *Database, targetLanguage, referenceLanguage string, useGemini bool, reportFile *os.File, maxPrayers int, verbose bool, maxRounds int) ([]Writing, error) {
	header := prepareLLMHeader(*db, targetLanguage, referenceLanguage)
	processed := 0
	matched := 0
	candidates := 0
	var unmatchedPrayers []Writing

	fmt.Fprintf(reportFile, "\n=== Processing prayers for language: %s (reference: %s) ===\n", targetLanguage, referenceLanguage)
	fmt.Fprintf(reportFile, "Started at: %s\n", time.Now().Format(time.RFC3339))
	if maxPrayers > 0 {
		fmt.Fprintf(reportFile, "Max prayers to process: %d\n", maxPrayers)
	}
	fmt.Fprintf(reportFile, "Verbose mode: %t\n\n", verbose)

	// Count total eligible prayers first
	totalEligible := 0
	for _, writing := range db.Writing {
		if writing.Phelps == "" && writing.Language == targetLanguage && strings.TrimSpace(writing.Text) != "" {
			totalEligible++
		}
	}

	if verbose {
		fmt.Printf("Found %d eligible prayers to process in language %s\n", totalEligible, targetLanguage)
		if maxPrayers > 0 && maxPrayers < totalEligible {
			fmt.Printf("Will process first %d prayers (limited by -max flag)\n", maxPrayers)
		}
	}

	for _, writing := range db.Writing {
		// Check for stop signal
		if atomic.LoadInt32(&stopRequested) > 0 {
			fmt.Printf("\nGraceful stop requested. Processed %d prayers so far.\n", processed)
			fmt.Fprintf(reportFile, "\nGraceful stop requested at %s. Processed %d prayers.\n", time.Now().Format(time.RFC3339), processed)
			break
		}

		// Skip if already has Phelps code or not the target language
		if writing.Phelps != "" || writing.Language != targetLanguage {
			continue
		}

		// Skip if no text to analyze
		if strings.TrimSpace(writing.Text) == "" {
			continue
		}

		// Check max prayers limit
		if maxPrayers > 0 && processed >= maxPrayers {
			fmt.Printf("Reached maximum prayer limit (%d). Stopping.\n", maxPrayers)
			fmt.Fprintf(reportFile, "Reached maximum prayer limit (%d) at %s.\n", maxPrayers, time.Now().Format(time.RFC3339))
			break
		}

		processed++

		if verbose {
			maxToProcess := totalEligible
			if maxPrayers > 0 && maxPrayers < totalEligible {
				maxToProcess = maxPrayers
			}
			fmt.Printf("Processing %d/%d: %s (Version: %s)\n", processed, maxToProcess, writing.Name, writing.Version)
			if len(writing.Text) > 100 {
				fmt.Printf("  Text preview: %s...\n", writing.Text[:100])
			}
		}

		prompt := header + "\n\nPrayer text to analyze:\n" + writing.Text

		fmt.Fprintf(reportFile, "Processing writing: %s (Version: %s)\n", writing.Name, writing.Version)

		fmt.Printf("   🧠 Analyzing with LLM...")

		var response LLMResponse
		var err error

		// Use new interactive mode by default
		response, err = callLLMInteractive(*db, referenceLanguage, prompt, useGemini, len(writing.Text), maxRounds, "match")
		if err != nil {
			fmt.Fprintf(reportFile, "  ERROR: LLM call failed: %v\n", err)
			if verbose {
				fmt.Printf(" ERROR: %v\n", err)
			} else {
				log.Printf("Error processing %s: %v", writing.Version, err)
			}

			// Create a fallback response for unknown matches
			response = LLMResponse{
				PhelpsCode: "UNKNOWN",
				Confidence: 0.0,
				Reasoning:  fmt.Sprintf("LLM service error: %v", err),
			}
		}

		// Always show the final decision clearly
		fmt.Printf("\n  🎯 FINAL DECISION:\n")
		fmt.Printf("     Prayer: %s (%s)\n", writing.Name, writing.Language)
		fmt.Printf("     Result: %s\n", response.PhelpsCode)
		fmt.Printf("     Confidence: %.1f%%\n", response.Confidence*100)
		fmt.Printf("     Reasoning: %s\n", response.Reasoning)

		if verbose {
			fmt.Printf("  ✓ Analysis complete!\n")
		}

		if response.PhelpsCode == "UNKNOWN" || response.Confidence < 0.7 {
			// Low confidence or unknown - add to unmatched for interactive assignment
			unmatchedPrayers = append(unmatchedPrayers, writing)
			if response.PhelpsCode == "UNKNOWN" {
				fmt.Fprintf(reportFile, "  UNMATCHED: Will prompt for interactive assignment\n")
			} else {
				candidates++
				fmt.Fprintf(reportFile, "  LOW CONFIDENCE (%.1f%%): Will prompt for interactive assignment\n", response.Confidence*100)
				if verbose {
					fmt.Printf("  LOW CONFIDENCE: %s (%.1f%%) - added to unmatched list\n", response.PhelpsCode, response.Confidence*100)
				}
			}
		} else {
			// High confidence - update writings table directly
			if err := updateWritingPhelps(response.PhelpsCode, writing.Language, writing.Version); err != nil {
				fmt.Fprintf(reportFile, "  ERROR: Failed to update writing: %v\n", err)
				if verbose {
					fmt.Printf("  ERROR updating database: %v\n", err)
				} else {
					log.Printf("Error updating writing %s: %v", writing.Version, err)
				}
			} else {
				matched++
				fmt.Fprintf(reportFile, "  MATCHED: Updated writings table with Phelps code %s\n", response.PhelpsCode)
				if verbose {
					fmt.Printf("  MATCHED: %s -> database updated\n", response.PhelpsCode)
				}

				// Record successful match and check for transliteration opportunities
				checkAndTriggerTransliteration(*db, writing, response.PhelpsCode, verbose, reportFile, "match")
			}
		}

		// Small delay to avoid overwhelming the LLM service
		if verbose {
			fmt.Printf("  Waiting 1 second...\n\n")
		}
		time.Sleep(1 * time.Second)
	}

	fmt.Fprintf(reportFile, "\nSummary for %s:\n", targetLanguage)
	fmt.Fprintf(reportFile, "  Processed: %d prayers\n", processed)
	fmt.Fprintf(reportFile, "  High confidence matches: %d\n", matched)
	fmt.Fprintf(reportFile, "  Low confidence candidates: %d\n", candidates)
	fmt.Fprintf(reportFile, "  Unmatched (for interactive): %d\n", len(unmatchedPrayers))
	fmt.Fprintf(reportFile, "Completed at: %s\n", time.Now().Format(time.RFC3339))

	// Commit changes to Dolt if any matches or candidates were processed
	if matched > 0 || candidates > 0 {
		commitMessage := fmt.Sprintf("LLM prayer matching for %s: %d matches, %d candidates", targetLanguage, matched, candidates)
		cmd := exec.Command("dolt", "add", ".")
		if output, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(reportFile, "  ERROR: Failed to stage changes: %v: %s\n", err, string(output))
			log.Printf("Error staging changes: %v", err)
		} else {
			cmd = exec.Command("dolt", "commit", "-m", commitMessage)
			if output, err := cmd.CombinedOutput(); err != nil {
				fmt.Fprintf(reportFile, "  ERROR: Failed to commit changes: %v: %s\n", err, string(output))
				log.Printf("Error committing changes: %v", err)
			} else {
				fmt.Fprintf(reportFile, "  SUCCESS: Changes committed to Dolt with message: %s\n", commitMessage)
				log.Printf("Changes committed: %s", commitMessage)
			}
		}
	}

	return unmatchedPrayers, nil
}

// Check if Ollama is available and model exists
func checkOllama(model string) error {
	// Check if Ollama API is available
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Try to get the list of available models via API
	httpReq, err := http.NewRequestWithContext(ctx, "GET", ollamaAPIURL+"/api/tags", nil)
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}

	client := &http.Client{}
	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("ollama API not available at %s: %v", ollamaAPIURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ollama API returned status %d: %s", resp.StatusCode, string(body))
	}

	// Read and parse response to check if model exists
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	var tagsResponse OllamaTagsResponse
	if err := json.Unmarshal(responseBody, &tagsResponse); err != nil {
		return fmt.Errorf("failed to parse models response: %w", err)
	}

	// Check if the model exists in the list
	modelFound := false
	var availableModels []string
	for _, m := range tagsResponse.Models {
		availableModels = append(availableModels, m.Name)
		// Check for exact match or with :latest tag
		if m.Name == model || m.Name == model+":latest" || strings.TrimSuffix(m.Name, ":latest") == model {
			modelFound = true
			break
		}
	}

	if !modelFound {
		return fmt.Errorf("model '%s' not found in Ollama API. Available models: %v\nTry: ollama pull %s", model, availableModels, model)
	}

	return nil
}

func main() {
	var targetLanguage = flag.String("language", "", "Target language code(s) to process - single (es) or multiple (es,fr,de) or auto-detect optimal")
	var referenceLanguage = flag.String("reference", "en", "Reference language for Phelps codes (default: en)")
	var operationMode = flag.String("mode", "match-add", "Operation mode: match (existing codes only), match-add (try match then add new), add-only (skip matching, add new codes), translit (transliteration matching/correction)")
	var useGemini = flag.Bool("gemini", true, "Use Gemini CLI (default: true, falls back to Ollama)")
	var ollamaModel = flag.String("model", "gpt-oss", "Ollama model to use (default: gpt-oss)")
	var reportPath = flag.String("report", "prayer_matching_report.txt", "Path for the report file")
	var interactive = flag.Bool("interactive", true, "Enable interactive assignment for unmatched prayers")
	var maxPrayers = flag.Int("max", 0, "Maximum number of prayers to process (0 = unlimited)")
	var maxRounds = flag.Int("max-rounds", 10, "Maximum conversation rounds in interactive mode (default: 10)")
	var cleanupInvalidCodes = flag.Bool("cleanup-invalid-codes", false, "Clean up invalid Phelps codes from database (reset to empty)")
	var verbose = flag.Bool("verbose", false, "Enable verbose output")
	var showHelp = flag.Bool("help", false, "Show help message")
	var showRaw = flag.Bool("show-raw", false, "Show full raw responses at the end")
	var lucky = flag.Bool("lucky", false, "Random prayer mode: process random prayers from all languages")
	var continueMode = flag.Bool("continue", false, "Auto-continue mode: process languages in priority order")
	var noPriority = flag.Bool("no-priority", false, "Disable priority language system: process smallest languages first")
	var testPrompt = flag.Bool("test-prompt", false, "Show the LLM prompt that would be generated and exit")
	var testFunctions = flag.Bool("test-functions", false, "Test the extensible function system and exit")
	var legacyMode = flag.Bool("legacy", false, "Use legacy mode with full prayer contexts in prompt (not interactive)")
	var clearNotesAge = flag.String("clear-notes", "", "Clear session notes older than duration (e.g., 30m, 2h)")

	flag.Parse()

	// Clear old session notes if requested
	if *clearNotesAge != "" {
		if duration, err := time.ParseDuration(*clearNotesAge); err == nil {
			removed := clearOldSessionNotes(duration)
			if removed > 0 {
				fmt.Printf("Cleared %d session notes older than %s\n", removed, *clearNotesAge)
			}
		} else {
			fmt.Printf("Invalid duration format: %s. Use formats like '30m', '2h', '24h'\n", *clearNotesAge)
			return
		}
	}

	if *showHelp {
		fmt.Printf("Bahá'í Prayers LLM Language Matcher\n")
		fmt.Printf("====================================\n\n")
		fmt.Printf("This tool uses Large Language Models (LLMs) to match prayers in different languages\n")
		fmt.Printf("to their corresponding Phelps codes in the Bahá'í writings database.\n\n")
		fmt.Printf("Usage: %s [options]\n\n", os.Args[0])
		fmt.Printf("Options:\n")
		flag.PrintDefaults()
		fmt.Printf("Examples:\n")
		fmt.Printf("  %s                                 # Auto-select optimal language (match-add mode)\n", os.Args[0])
		fmt.Printf("  %s -language=es -max=10            # Process first 10 Spanish prayers\n", os.Args[0])
		fmt.Printf("  %s -language=fr -verbose           # Process French with detailed output\n", os.Args[0])
		fmt.Printf("  %s -language=es,fr,de -max=50      # Process multiple languages (50 total)\n", os.Args[0])
		fmt.Printf("  %s -language=de -interactive=false # Process German without interactive mode\n", os.Args[0])
		fmt.Printf("  %s -language=es -mode=match-add    # Try matching, add new codes if no match\n", os.Args[0])
		fmt.Printf("  %s -language=en -mode=add-only     # Skip matching, add new codes from inventory\n", os.Args[0])
		fmt.Printf("  %s -mode=add-only                  # Defaults to English for inventory-based adding\n", os.Args[0])
		fmt.Printf("  %s -language=ar -mode=translit     # Fix Arabic transliteration (match ar, correct ar-translit)\n", os.Args[0])
		fmt.Printf("  %s -language=fa -mode=translit     # Fix Persian transliteration (match fa, correct fa-translit)\n", os.Args[0])
		fmt.Printf("  %s -mode=translit                  # Fix all transliterations (defaults to ar,fa)\n", os.Args[0])
		fmt.Printf("  %s -language=es -gemini=false      # Process Spanish prayers using only Ollama\n", os.Args[0])
		fmt.Printf("  %s -lucky -max=20                  # Process 20 random prayers from all languages\n", os.Args[0])
		fmt.Printf("  %s -continue -max=50               # Auto-process languages in priority order\n", os.Args[0])
		fmt.Printf("  %s -continue -no-priority          # Process smallest languages first\n", os.Args[0])
		fmt.Printf("  %s -test-prompt -language=es       # Show LLM prompt for Spanish prayers\n", os.Args[0])
		fmt.Printf("  %s -legacy -language=es            # Use legacy mode with full contexts\n", os.Args[0])
		fmt.Printf("  %s -clear-notes=2h                 # Clear session notes older than 2 hours\n", os.Args[0])
		fmt.Printf("  %s -help                           # Show this help message\n", os.Args[0])
		fmt.Printf("\nTroubleshooting:\n")
		fmt.Printf("  If Ollama fails, ensure it's installed and the model is available:\n")
		fmt.Printf("    ollama list                      # Check available models\n")
		fmt.Printf("    ollama pull %s                 # Pull required model\n", *ollamaModel)
		fmt.Printf("  If Gemini fails, install Gemini CLI or use -gemini=false\n")
		fmt.Printf("  For languages with minimal missing prayers, consider using -language=es or -language=fr\n")
		fmt.Printf("  Use -max=N to limit processing and -verbose for detailed output\n")
		fmt.Printf("  Use -mode=match-add to create new codes when no existing match found\n")
		fmt.Printf("  Use -mode=add-only for adding missing codes (best: English, Arabic, Persian)\n")
		fmt.Printf("  ADD-ONLY mode: Works best with -language=en/ar/fa (inventory has limited other languages)\n")
		fmt.Printf("  MULTI-LANGUAGE: Use comma-separated list (es,fr,de) to process multiple languages\n")
		fmt.Printf("  TRANSLIT mode: Supports ar/fa base languages, auto-converts to ar-translit/fa-translit\n")
		fmt.Printf("  Send SIGINT (Ctrl+C) for graceful stop after current prayer (press twice to force quit)\n")
		return
	}

	// Validate operation mode
	validModes := map[string]bool{"match": true, "match-add": true, "add-only": true, "translit": true}
	if !validModes[*operationMode] {
		log.Fatalf("Invalid operation mode: %s. Valid modes: match, match-add, add-only, translit", *operationMode)
	}

	// Special validation for add-only mode
	if *operationMode == "add-only" {
		if *targetLanguage == "" {
			// Default to English for add-only mode
			*targetLanguage = "en"
			fmt.Printf("🔤 ADD-ONLY mode: Defaulting to English language\n")
		}

		// Warn about non-supported languages
		supportedLangs := map[string]string{
			"en": "English", "eng": "English",
			"ar": "Arabic", "ara": "Arabic", "arabic": "Arabic",
			"fa": "Persian", "per": "Persian", "persian": "Persian",
		}

		langName, isSupported := supportedLangs[strings.ToLower(*targetLanguage)]
		if !isSupported {
			fmt.Printf("⚠️  WARNING: ADD-ONLY mode works best with English, Arabic, or Persian.\n")
			fmt.Printf("   You specified: %s\n", *targetLanguage)
			fmt.Printf("   The inventory table has limited coverage for other languages.\n")
			fmt.Printf("   You may find very few or no matching documents.\n\n")
			fmt.Printf("   Continue anyway? (y/N): ")

			var response string
			fmt.Scanln(&response)
			if strings.ToLower(response) != "y" && strings.ToLower(response) != "yes" {
				fmt.Printf("Exiting. Use -language=en, -language=ar, or -language=fa for best results.\n")
				return
			}
			fmt.Printf("Continuing with language: %s\n\n", *targetLanguage)
		} else {
			fmt.Printf("🔤 ADD-ONLY mode: Processing %s prayers from inventory\n", langName)
		}
	}

	// Special validation and processing for translit mode
	if *operationMode == "translit" {
		if *targetLanguage == "" {
			// Default to both Arabic and Persian transliterations
			*targetLanguage = "ar-translit,fa-translit"
			fmt.Printf("🔤 TRANSLIT mode: Defaulting to Arabic and Persian transliterations\n")
		}

		// Parse and process languages for translit mode
		languages := strings.Split(*targetLanguage, ",")
		var processLanguages []string

		for _, lang := range languages {
			lang = strings.TrimSpace(lang)
			if lang == "ar" || lang == "arabic" {
				processLanguages = append(processLanguages, "ar-translit")
				fmt.Printf("🔤 TRANSLIT mode: Processing ar-translit to check/correct transliterations\n")
			} else if lang == "fa" || lang == "persian" || lang == "per" {
				processLanguages = append(processLanguages, "fa-translit")
				fmt.Printf("🔤 TRANSLIT mode: Processing fa-translit to check/correct transliterations\n")
			} else if strings.HasSuffix(lang, "-translit") {
				processLanguages = append(processLanguages, lang)
				fmt.Printf("🔤 TRANSLIT mode: Processing %s to check/correct transliterations\n", lang)
			} else {
				fmt.Printf("⚠️  WARNING: TRANSLIT mode works only with transliteration languages.\n")
				fmt.Printf("   You specified: %s (not supported)\n", lang)
				fmt.Printf("   Supported: ar-translit, fa-translit\n")
				fmt.Printf("   Continue anyway? (y/N): ")

				var response string
				fmt.Scanln(&response)
				if strings.ToLower(response) != "y" && strings.ToLower(response) != "yes" {
					fmt.Printf("Exiting. Use -language=ar-translit,fa-translit for translit mode.\n")
					return
				}
			}
		}

		// Update targetLanguage with processed languages
		*targetLanguage = strings.Join(processLanguages, ",")
		fmt.Printf("🔤 TRANSLIT mode: Final language list: %s\n", *targetLanguage)
		fmt.Printf("🔤 TRANSLIT mode: Will process transliterations directly with version IDs\n")
	}

	// Set up signal handling for graceful stop and force quit
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for range sigChan {
			count := atomic.AddInt32(&interruptCount, 1)
			if count == 1 {
				fmt.Printf("\nReceived stop signal. Will stop after current prayer...\n")
				fmt.Printf("Press Ctrl+C again to force quit immediately.\n")
				atomic.StoreInt32(&stopRequested, 1)
			} else {
				fmt.Printf("\nForce quit requested. Exiting immediately...\n")
				os.Exit(1)
			}
		}
	}()

	// Initialize random seed for lucky mode
	rand.Seed(time.Now().UnixNano())

	// Set the Ollama model
	OllamaModel = *ollamaModel

	// Initialize Gemini CLI settings early if we're using Gemini
	if *useGemini {
		if err := ensureGeminiSettings(); err != nil {
			log.Printf("Warning: failed to configure Gemini settings: %v", err)
		}
	}

	// Check Ollama availability
	log.Printf("Checking Ollama availability for model %s...", *ollamaModel)
	if err := checkOllama(*ollamaModel); err != nil {
		log.Printf("Ollama check failed: %v", err)
		if !*useGemini {
			log.Fatalf("Ollama is required when Gemini is disabled")
		}
		log.Printf("Will attempt to use Gemini CLI only")
	} else {
		log.Printf("Ollama is ready with model %s", *ollamaModel)
	}

	// Open report file
	reportFile, err := os.Create(*reportPath)
	if err != nil {
		log.Fatalf("Failed to create report file: %v", err)
	}
	defer reportFile.Close()

	fmt.Fprintf(reportFile, "Prayer Matching Report\n")
	fmt.Fprintf(reportFile, "=====================\n")
	fmt.Fprintf(reportFile, "Started: %s\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(reportFile, "Target Language: %s\n", *targetLanguage)
	fmt.Fprintf(reportFile, "Reference Language: %s\n", *referenceLanguage)
	fmt.Fprintf(reportFile, "Operation Mode: %s\n", *operationMode)
	fmt.Fprintf(reportFile, "Lucky Mode: %t\n", *lucky)
	fmt.Fprintf(reportFile, "Continue Mode: %t\n", *continueMode)
	fmt.Fprintf(reportFile, "No Priority Languages: %t\n", *noPriority)
	fmt.Fprintf(reportFile, "Using Gemini: %t\n", *useGemini)
	fmt.Fprintf(reportFile, "Gemini Quota Exceeded: %t\n", atomic.LoadInt32(&geminiQuotaExceeded) == 1)
	fmt.Fprintf(reportFile, "Ollama Model: %s\n", *ollamaModel)
	fmt.Fprintf(reportFile, "Interactive Mode: %t\n", *interactive)
	fmt.Fprintf(reportFile, "Max Prayers: %d\n", *maxPrayers)
	fmt.Fprintf(reportFile, "Verbose Mode: %t\n", *verbose)
	fmt.Fprintf(reportFile, "\n")

	db := GetDatabase()
	log.Println("Database loaded")
	fmt.Fprintf(reportFile, "Database loaded successfully\n")

	// Test prompt mode - show the prompt and exit
	if *testPrompt {
		targetLang := *targetLanguage
		if targetLang == "" {
			targetLang = "es" // Default to Spanish for testing
		}

		fmt.Printf("=== TESTING LLM PROMPT GENERATION ===\n\n")
		fmt.Printf("Target Language: %s\n", targetLang)
		fmt.Printf("Reference Language: %s\n", *referenceLanguage)
		fmt.Printf("Mode: %s\n", map[bool]string{true: "Legacy (all contexts)", false: "Interactive (function calls)"}[*legacyMode])

		fmt.Printf("\n%s\n", strings.Repeat("=", 80))
		fmt.Printf("GENERATED PROMPT:\n")
		fmt.Printf("%s\n\n", strings.Repeat("=", 80))

		// Generate the appropriate header/prompt
		var header string
		if *legacyMode {
			header = prepareLLMHeaderLegacy(db, targetLang, *referenceLanguage)
		} else {
			header = prepareLLMHeaderWithMode(db, targetLang, *referenceLanguage, *operationMode)
		}
		fmt.Print(header)

		fmt.Printf("\n\nPrayer text to analyze:\n")
		fmt.Printf("[This is where the %s prayer text would go]\n", targetLang)

		fmt.Printf("\n%s\n", strings.Repeat("=", 80))
		fmt.Printf("END OF PROMPT\n")
		fmt.Printf("%s\n", strings.Repeat("=", 80))

		// Show some statistics
		phelpsContext := buildPhelpsContext(db, *referenceLanguage)
		fmt.Printf("\nSTATISTICS:\n")
		fmt.Printf("- Total Phelps codes with context: %d\n", len(phelpsContext))
		fmt.Printf("- Mode: %s\n", map[bool]string{true: "All contexts included in prompt", false: "Interactive search available"}[*legacyMode])

		if !*legacyMode {
			fmt.Printf("\nAVAILABLE SEARCH FUNCTIONS:\n")
			fmt.Printf("- SEARCH:keywords,opening,range,... (ALWAYS COMBINE MULTIPLE CRITERIA)\n")
			fmt.Printf("- GET_FULL_TEXT:phelps_code\n")
			fmt.Printf("- GET_FOCUS_TEXT:keyword,phelps_code1,phelps_code2,... (special: 'head'/'tail')\n")
			fmt.Printf("- GET_PARTIAL_TEXT:phelps_code,range\n")
			fmt.Printf("- FINAL_ANSWER:phelps_code,confidence,reasoning\n")
			fmt.Printf("- GET_STATS\n")
			fmt.Printf("\nSEARCH Examples (ALWAYS COMBINE!):\n")
			fmt.Printf("- SEARCH:lord,god,mercy,100-200 (keywords + length - PREFERRED)\n")
			fmt.Printf("- SEARCH:lord,god,O Lord my God,100-200 (keywords + opening + length - BEST)\n")
			fmt.Printf("- SEARCH:mercy,compassion,O Thou Compassionate,150-300 (full combination)\n")
			fmt.Printf("\nAVOID: Multiple separate searches - always use ONE combined search!\n")
		} else {
			// Show a few examples of the context for legacy mode
			fmt.Printf("\nSAMPLE CONTEXTS (first 3):\n")
			count := 0
			for phelps, context := range phelpsContext {
				if count >= 3 {
					break
				}
				fmt.Printf("\n%d. %s:\n   %s\n", count+1, phelps, context)
				count++
			}

			if len(phelpsContext) > 3 {
				fmt.Printf("\n... and %d more contexts\n", len(phelpsContext)-3)
			}
		}
		return
	}

	// Test the extensible function system if requested
	if *testFunctions {
		fmt.Println("Testing extensible function system...")
		fmt.Println("Generated function help:")
		fmt.Println(generateFunctionHelp())
		return
	}

	// Validate mode flags
	modeCount := 0
	if *lucky {
		modeCount++
	}
	if *continueMode {
		modeCount++
	}
	if *targetLanguage != "" {
		modeCount++
	}

	if modeCount > 1 {
		log.Fatalf("Error: Cannot use multiple modes simultaneously. Choose one: -language=X[,Y,Z], -lucky, or -continue")
	}

	// Auto-select optimal language if no mode specified
	if !*lucky && !*continueMode && *targetLanguage == "" {
		*targetLanguage = findOptimalDefaultLanguage(db, *noPriority)
		fmt.Printf("Auto-selected target language: %s\n", *targetLanguage)
		fmt.Fprintf(reportFile, "Auto-selected target language: %s\n", *targetLanguage)

		// Show missing counts for context
		missing := calculateMissingPrayersPerLanguage(db)
		fmt.Printf("Missing prayers by language:\n")
		fmt.Fprintf(reportFile, "Missing prayers by language:\n")

		// Sort languages by missing count for display
		type langCount struct {
			lang  string
			count int
		}
		var langCounts []langCount
		for lang, count := range missing {
			if count > 0 {
				langCounts = append(langCounts, langCount{lang, count})
			}
		}
		sort.Slice(langCounts, func(i, j int) bool {
			return langCounts[i].count < langCounts[j].count
		})

		for i, lc := range langCounts {
			marker := ""
			if lc.lang == *targetLanguage {
				marker = " <- SELECTED"
			}
			fmt.Printf("  %s: %d%s\n", lc.lang, lc.count, marker)
			fmt.Fprintf(reportFile, "  %s: %d%s\n", lc.lang, lc.count, marker)
			if i >= 9 { // Show top 10
				remaining := len(langCounts) - 10
				if remaining > 0 {
					fmt.Printf("  ... and %d more languages\n", remaining)
					fmt.Fprintf(reportFile, "  ... and %d more languages\n", remaining)
				}
				break
			}
		}
		fmt.Println()
		fmt.Fprintf(reportFile, "\n")
	}

	// Handle multiple languages if specified
	var targetLanguages []string
	if *targetLanguage != "" {
		targetLanguages = strings.Split(*targetLanguage, ",")
		for i, lang := range targetLanguages {
			targetLanguages[i] = strings.TrimSpace(lang)
		}

		if len(targetLanguages) > 1 {
			fmt.Printf("🌐 Processing multiple languages: %v\n", targetLanguages)
			fmt.Fprintf(reportFile, "Processing multiple languages: %v\n", targetLanguages)
		}
	} else {
		targetLanguages = []string{*targetLanguage}
	}

	// Show database size
	log.Println("Database size:",
		len(db.Writing), "/", db.Skipped["writing"],
		len(db.Language), "/", db.Skipped["language"],
		len(db.MatchAttempts), "match attempts",
		len(db.LanguagePairStats), "language pairs",
		len(db.SkipList), "skip entries",
	)

	fmt.Fprintf(reportFile, "Database size: %d writings, %d languages, %d match attempts, %d language pairs, %d skip entries\n",
		len(db.Writing), len(db.Language), len(db.MatchAttempts), len(db.LanguagePairStats), len(db.SkipList))

	// Process prayers based on selected mode
	var unmatchedPrayers []Writing
	// Handle cleanup operation
	if *cleanupInvalidCodes {
		fmt.Printf("🧹 Cleaning up invalid Phelps codes from database...\n")
		cleanedCount, err := cleanupInvalidPhelpsCode()
		if err != nil {
			log.Fatalf("Error during cleanup: %v", err)
		}
		fmt.Printf("✅ Cleanup completed: %d invalid codes reset to empty\n", cleanedCount)
		return
	}

	var processErr error

	if *lucky {
		unmatchedPrayers, processErr = processRandomPrayersWithMode(&db, *referenceLanguage, *useGemini, reportFile, *maxPrayers, *verbose, *legacyMode, *maxRounds, *operationMode)
	} else if *continueMode {
		unmatchedPrayers, processErr = processLanguagesContinuouslyWithMode(&db, *referenceLanguage, *useGemini, reportFile, *maxPrayers, *verbose, *noPriority, *legacyMode, *maxRounds, *operationMode)
	} else if len(targetLanguages) > 1 {
		// Multiple language mode
		var allUnmatched []Writing
		totalProcessed := 0
		prayersPerLanguage := *maxPrayers
		if *maxPrayers > 0 {
			prayersPerLanguage = *maxPrayers / len(targetLanguages)
			if prayersPerLanguage == 0 {
				prayersPerLanguage = 1
			}
		}

		for i, lang := range targetLanguages {
			if atomic.LoadInt32(&stopRequested) == 1 {
				fmt.Printf("\n🛑 Stop requested. Processed %d languages so far.\n", i)
				break
			}

			remainingQuota := prayersPerLanguage
			if *maxPrayers > 0 && i == len(targetLanguages)-1 {
				// Give remaining quota to last language
				remainingQuota = *maxPrayers - totalProcessed
				if remainingQuota <= 0 {
					break
				}
			}

			fmt.Printf("\n📚 Processing language %d/%d: %s\n", i+1, len(targetLanguages), lang)
			fmt.Fprintf(reportFile, "\n=== Language %d/%d: %s ===\n", i+1, len(targetLanguages), lang)

			languageUnmatched, err := processPrayersForLanguageWithMode(&db, lang, *referenceLanguage, *useGemini, reportFile, remainingQuota, *verbose, *legacyMode, *maxRounds, *operationMode)
			if err != nil {
				fmt.Printf("⚠️ Error processing language %s: %v\n", lang, err)
				continue
			}

			allUnmatched = append(allUnmatched, languageUnmatched...)
			totalProcessed += (remainingQuota - len(languageUnmatched))
		}

		unmatchedPrayers = allUnmatched
		fmt.Printf("🎯 Multi-language processing completed: %d languages, %d unmatched prayers\n", len(targetLanguages), len(allUnmatched))
	} else {
		// Single language mode (traditional)
		unmatchedPrayers, processErr = processPrayersForLanguageWithMode(&db, targetLanguages[0], *referenceLanguage, *useGemini, reportFile, *maxPrayers, *verbose, *legacyMode, *maxRounds, *operationMode)
	}

	if processErr != nil {
		log.Fatalf("Error processing prayers: %v", processErr)
	}

	// Interactive assignment for unmatched prayers
	if *interactive && len(unmatchedPrayers) > 0 {
		interactiveAssignment(&db, unmatchedPrayers, reportFile)
	} else if len(unmatchedPrayers) > 0 {
		fmt.Printf("Found %d unmatched prayers. Run with -interactive=true to assign them manually.\n", len(unmatchedPrayers))
		fmt.Fprintf(reportFile, "Found %d unmatched prayers (interactive mode disabled)\n", len(unmatchedPrayers))
	}

	// Final status report
	fmt.Fprintf(reportFile, "\n=== FINAL STATUS ===\n")
	fmt.Fprintf(reportFile, "Completed: %s\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(reportFile, "Operation Mode: %s\n", *operationMode)

	// Report transliteration statistics if relevant
	if *operationMode == "translit" || (len(targetLanguages) > 0 && shouldCheckTransliteration(targetLanguages[0], *operationMode)) {
		fmt.Fprintf(reportFile, "\n=== TRANSLITERATION STATUS ===\n")

		// Count transliteration opportunities
		translitQuery := "SELECT COUNT(*) as ar_missing FROM writings w1 WHERE w1.language = 'ar' AND w1.phelps IS NOT NULL AND w1.phelps != '' AND NOT EXISTS (SELECT 1 FROM writings w2 WHERE w2.phelps = w1.phelps AND w2.language = 'ar-translit')"
		cmd := exec.Command("dolt", "sql", "-q", translitQuery)
		if output, err := cmd.CombinedOutput(); err == nil {
			lines := strings.Split(string(output), "\n")
			for i, line := range lines {
				if i >= 3 && strings.Contains(line, "|") {
					fields := strings.Split(line, "|")
					if len(fields) >= 1 {
						count := strings.TrimSpace(fields[0])
						if count != "" && count != "NULL" {
							fmt.Fprintf(reportFile, "Arabic prayers missing transliteration: %s\n", count)
						}
					}
					break
				}
			}
		}

		translitQuery = "SELECT COUNT(*) as fa_missing FROM writings w1 WHERE w1.language = 'fa' AND w1.phelps IS NOT NULL AND w1.phelps != '' AND NOT EXISTS (SELECT 1 FROM writings w2 WHERE w2.phelps = w1.phelps AND w2.language = 'fa-translit')"
		cmd = exec.Command("dolt", "sql", "-q", translitQuery)
		if output, err := cmd.CombinedOutput(); err == nil {
			lines := strings.Split(string(output), "\n")
			for i, line := range lines {
				if i >= 3 && strings.Contains(line, "|") {
					fields := strings.Split(line, "|")
					if len(fields) >= 1 {
						count := strings.TrimSpace(fields[0])
						if count != "" && count != "NULL" {
							fmt.Fprintf(reportFile, "Persian prayers missing transliteration: %s\n", count)
						}
					}
					break
				}
			}
		}
	}
	if atomic.LoadInt32(&geminiQuotaExceeded) == 1 {
		fmt.Fprintf(reportFile, "Gemini quota was exceeded during processing - continued with Ollama only\n")
		log.Printf("Prayer matching completed with Gemini quota exceeded - used Ollama fallback. Report written to: %s", *reportPath)
	} else {
		log.Printf("Prayer matching completed. Report written to: %s", *reportPath)
	}

	// Show raw responses if requested
	if *showRaw {
		showStoredRawResponses()
	}

	// Show final session notes statistics
	stats := getSessionNotesStats()
	if stats["total"] > 0 {
		fmt.Printf("\n📝 Session Notes Summary:\n")
		fmt.Printf("Total notes created: %d\n", stats["total"])
		for noteType := range map[string]bool{"SUCCESS": true, "FAILURE": true, "PATTERN": true, "STRATEGY": true, "TIP": true} {
			key := "type_" + strings.ToLower(noteType)
			if count, exists := stats[key]; exists && count > 0 {
				fmt.Printf("  %s: %d\n", noteType, count)
			}
		}
		fmt.Printf("These notes helped the LLM learn patterns during this session.\n")
	}
}
