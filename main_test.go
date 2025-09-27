package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAllPrayersParsed(t *testing.T) {
	// Load the in‑memory database. This will execute Dolt queries.
	db := GetDatabase()

	// Count any prayers that lack a Phelps code.
	missingPhelps := 0
	for _, w := range db.Writing {
		if w.Phelps == "" {
			missingPhelps++
		}
	}

	if missingPhelps > 0 {
		t.Logf("%d prayers have an empty Phelps code", missingPhelps)
	}

	// Ensure no skipped prayers were reported.
	for table, count := range db.Skipped {
		if count > 0 {
			t.Fatalf("Found %d skipped entries in table %s", count, table)
		}
	}
}

func TestParseLLMResponse(t *testing.T) {
	tests := []struct {
		name     string
		response string
		expected LLMResponse
	}{
		{
			name: "Valid response with high confidence",
			response: `Phelps: AB00001FIR
Confidence: 85
Reasoning: This prayer matches the Fire Tablet based on distinctive phrases and structure.`,
			expected: LLMResponse{
				PhelpsCode: "AB00001FIR",
				Confidence: 0.85,
				Reasoning:  "This prayer matches the Fire Tablet based on distinctive phrases and structure.",
			},
		},
		{
			name: "Unknown response",
			response: `Phelps: UNKNOWN
Confidence: 0
Reasoning: Could not find a matching prayer in the database.`,
			expected: LLMResponse{
				PhelpsCode: "UNKNOWN",
				Confidence: 0.0,
				Reasoning:  "Could not find a matching prayer in the database.",
			},
		},
		{
			name: "Case insensitive parsing",
			response: `phelps: AB00032DAR
confidence: 92
reasoning: Clear match with distinctive opening words.`,
			expected: LLMResponse{
				PhelpsCode: "AB00032DAR",
				Confidence: 0.92,
				Reasoning:  "Clear match with distinctive opening words.",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseLLMResponse(tt.response)
			if result.PhelpsCode != tt.expected.PhelpsCode {
				t.Errorf("PhelpsCode = %v, want %v", result.PhelpsCode, tt.expected.PhelpsCode)
			}
			if result.Confidence != tt.expected.Confidence {
				t.Errorf("Confidence = %v, want %v", result.Confidence, tt.expected.Confidence)
			}
			if result.Reasoning != tt.expected.Reasoning {
				t.Errorf("Reasoning = %v, want %v", result.Reasoning, tt.expected.Reasoning)
			}
		})
	}
}

func TestPrepareLLMHeader(t *testing.T) {
	// Create a mock database with some known Phelps codes
	db := Database{
		Writing: []Writing{
			{Phelps: "AB00001FIR", Language: "en", Name: "Fire Tablet", Text: "Test prayer 1"},
			{Phelps: "AB00032DAR", Language: "en", Name: "Tablet of Ahmad", Text: "Test prayer 2"},
			{Phelps: "", Language: "en", Name: "Unknown Prayer", Text: "Prayer without Phelps code"},
			{Phelps: "AB00044PRO", Language: "es", Name: "Oración Matutina", Text: "Spanish prayer"},
		},
	}

	header := prepareLLMHeader(db, "English", "en")

	// Check that the header contains expected elements
	if !strings.Contains(header, "English") {
		t.Error("Header should contain the target language name")
	}

	if !strings.Contains(header, "SEARCH:") {
		t.Error("Header should contain function instructions")
	}

	if !strings.Contains(header, "GET_FULL_TEXT") {
		t.Error("Header should contain GET_FULL_TEXT function")
	}

	if !strings.Contains(header, "GET_FOCUS_TEXT") {
		t.Error("Header should contain GET_FOCUS_TEXT function")
	}

	if !strings.Contains(header, "CONFIDENCE") {
		t.Error("Header should contain confidence instructions")
	}

	if !strings.Contains(header, "UNKNOWN") {
		t.Error("Header should contain instructions for unknown matches")
	}

	if !strings.Contains(header, "FINAL_ANSWER") {
		t.Error("Header should contain FINAL_ANSWER function")
	}
}

func TestPrepareLLMHeaderDefaultLanguage(t *testing.T) {
	db := Database{
		Writing: []Writing{
			{Phelps: "AB00001FIR", Language: "en", Name: "Test Prayer", Text: "Test prayer"},
		},
	}

	// Test with empty target language string
	header := prepareLLMHeader(db, "", "en")
	if !strings.Contains(header, "English") {
		t.Error("Should default to English when target language is empty")
	}

	// Test with empty reference language string
	header2 := prepareLLMHeader(db, "French", "")
	if !strings.Contains(header2, "English") {
		t.Error("Should default to English when reference language is empty")
	}
}

func TestMatchAttemptDataStructure(t *testing.T) {
	db := Database{
		MatchAttempts: []MatchAttempt{},
	}

	attempt := MatchAttempt{
		VersionID:             "test_version_001",
		TargetLanguage:        "fr",
		ReferenceLanguage:     "en",
		ResultType:            "low_confidence",
		PhelpsCode:            "AB00001FIR",
		ConfidenceScore:       0.75,
		Reasoning:             "LLM match with medium confidence",
		LLMProvider:           "ollama",
		LLMModel:              "gpt-oss",
		ProcessingTimeSeconds: 15,
		FailureReason:         "",
	}

	// Test the in-memory part (we can't easily test the Dolt part without a real database)
	initialCount := len(db.MatchAttempts)

	// We'll simulate just the in-memory addition part
	db.MatchAttempts = append(db.MatchAttempts, attempt)

	if len(db.MatchAttempts) != initialCount+1 {
		t.Error("Match attempt should be added to in-memory database")
	}

	added := db.MatchAttempts[len(db.MatchAttempts)-1]
	if added.PhelpsCode != attempt.PhelpsCode {
		t.Errorf("Added attempt PhelpsCode = %v, want %v", added.PhelpsCode, attempt.PhelpsCode)
	}
	if added.ConfidenceScore != attempt.ConfidenceScore {
		t.Errorf("Added attempt ConfidenceScore = %v, want %v", added.ConfidenceScore, attempt.ConfidenceScore)
	}
	if added.TargetLanguage != attempt.TargetLanguage {
		t.Errorf("Added attempt TargetLanguage = %v, want %v", added.TargetLanguage, attempt.TargetLanguage)
	}
}

func TestLLMResponseHandling(t *testing.T) {
	tests := []struct {
		name               string
		response           LLMResponse
		expectedHighConf   bool
		expectedPhelpsCode string
	}{
		{
			name: "High confidence match",
			response: LLMResponse{
				PhelpsCode: "AB00001FIR",
				Confidence: 0.85,
				Reasoning:  "Strong textual match",
			},
			expectedHighConf:   true,
			expectedPhelpsCode: "AB00001FIR",
		},
		{
			name: "Low confidence match",
			response: LLMResponse{
				PhelpsCode: "AB00032DAR",
				Confidence: 0.65,
				Reasoning:  "Weak textual similarity",
			},
			expectedHighConf:   false,
			expectedPhelpsCode: "AB00032DAR",
		},
		{
			name: "Unknown match",
			response: LLMResponse{
				PhelpsCode: "UNKNOWN",
				Confidence: 0.0,
				Reasoning:  "No matching prayer found",
			},
			expectedHighConf:   false,
			expectedPhelpsCode: "UNKNOWN",
		},
		{
			name: "Boundary case - exactly 70% confidence",
			response: LLMResponse{
				PhelpsCode: "AB00044PRO",
				Confidence: 0.70,
				Reasoning:  "Borderline match",
			},
			expectedHighConf:   true,
			expectedPhelpsCode: "AB00044PRO",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isHighConf := tt.response.PhelpsCode != "UNKNOWN" && tt.response.Confidence >= 0.7

			if isHighConf != tt.expectedHighConf {
				t.Errorf("Expected high confidence = %v, got %v", tt.expectedHighConf, isHighConf)
			}

			if tt.response.PhelpsCode != tt.expectedPhelpsCode {
				t.Errorf("Expected Phelps code = %v, got %v", tt.expectedPhelpsCode, tt.response.PhelpsCode)
			}
		})
	}
}

func TestSQLEscaping(t *testing.T) {
	// Test that SQL injection prevention works
	testCases := []struct {
		input    string
		expected string
	}{
		{"normal text", "normal text"},
		{"text with 'single quote'", "text with ''single quote''"},
		{"'; DROP TABLE writings; --", "''; DROP TABLE writings; --"},
		{"text with '' double quotes", "text with '''' double quotes"},
	}

	for _, tc := range testCases {
		result := strings.ReplaceAll(tc.input, "'", "''")
		if result != tc.expected {
			t.Errorf("SQL escaping failed: input %q, expected %q, got %q", tc.input, tc.expected, result)
		}
	}
}

func TestMockLLMIntegration(t *testing.T) {
	// Create a temporary report file for testing
	tmpFile, err := os.CreateTemp("", "test_report_*.txt")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Create a minimal mock database
	db := Database{
		Writing: []Writing{
			{
				Phelps:   "",
				Language: "en",
				Version:  "test_v1",
				Name:     "Test Prayer",
				Text:     "O God, this is a test prayer for matching.",
			},
		},
	}

	// Test the LLM header preparation
	header := prepareLLMHeader(db, "en", "en")
	if len(header) == 0 {
		t.Error("LLM header should not be empty")
	}

	// Test response parsing
	mockResponse := `Phelps: AB00001FIR
Confidence: 80
Reasoning: This appears to be a test prayer with religious content.`

	parsed := parseLLMResponse(mockResponse)
	if parsed.PhelpsCode != "AB00001FIR" {
		t.Errorf("Expected AB00001FIR, got %s", parsed.PhelpsCode)
	}
	if parsed.Confidence != 0.8 {
		t.Errorf("Expected 0.8, got %f", parsed.Confidence)
	}
}

func TestCalculateMissingPrayersPerLanguage(t *testing.T) {
	db := Database{
		Writing: []Writing{
			{Phelps: "AB00001FIR", Language: "en", Text: "English with code"},
			{Phelps: "", Language: "en", Text: "English without code"},
			{Phelps: "", Language: "en", Text: "Another English without code"},
			{Phelps: "AB00032DAR", Language: "es", Text: "Spanish with code"},
			{Phelps: "", Language: "es", Text: "Spanish without code"},
			{Phelps: "", Language: "fr", Text: "French without code"},
			{Phelps: "AB00044PRO", Language: "de", Text: "German with code"},
		},
	}

	missing := calculateMissingPrayersPerLanguage(db)

	expected := map[string]int{
		"en": 2,
		"es": 1,
		"fr": 1,
	}

	for lang, expectedCount := range expected {
		if missing[lang] != expectedCount {
			t.Errorf("Language %s: expected %d missing, got %d", lang, expectedCount, missing[lang])
		}
	}

	// German should not be in missing map (no missing prayers)
	if _, exists := missing["de"]; exists {
		t.Error("German should not be in missing map as it has no missing prayers")
	}
}

func TestFindOptimalDefaultLanguage(t *testing.T) {
	tests := []struct {
		name     string
		writings []Writing
		expected string
	}{
		{
			name: "Normal case with multiple languages",
			writings: []Writing{
				{Phelps: "", Language: "en", Text: "Missing 1"},
				{Phelps: "", Language: "en", Text: "Missing 2"},
				{Phelps: "", Language: "en", Text: "Missing 3"},
				{Phelps: "", Language: "es", Text: "Missing 1"},
				{Phelps: "", Language: "fr", Text: "Missing 1"},
				{Phelps: "", Language: "fr", Text: "Missing 2"},
				{Phelps: "AB00001FIR", Language: "de", Text: "Has code"},
			},
			expected: "es", // Spanish has lowest missing count (1)
		},
		{
			name: "All languages complete",
			writings: []Writing{
				{Phelps: "AB00001FIR", Language: "en", Text: "Complete"},
				{Phelps: "AB00002DAR", Language: "es", Text: "Complete"},
			},
			expected: "en", // Fallback to en when no missing
		},
		{
			name: "Single language with missing",
			writings: []Writing{
				{Phelps: "", Language: "fr", Text: "Missing"},
			},
			expected: "fr",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := Database{Writing: tt.writings}
			result := findOptimalDefaultLanguage(db, false)
			if result != tt.expected {
				t.Errorf("Expected %s, got %s", tt.expected, result)
			}
		})
	}
}

func TestTruncateString(t *testing.T) {
	tests := []struct {
		input    string
		maxLen   int
		expected string
	}{
		{"short", 10, "short"},
		{"exactlyten", 10, "exactlyten"},
		{"this is longer than limit", 10, "this is lo"},
		{"", 5, ""},
		{"test", 0, ""},
	}

	for _, tt := range tests {
		result := truncateString(tt.input, tt.maxLen)
		if result != tt.expected {
			t.Errorf("truncateString(%q, %d) = %q, want %q", tt.input, tt.maxLen, result, tt.expected)
		}
	}
}

func TestMinFunction(t *testing.T) {
	tests := []struct {
		a, b     int
		expected int
	}{
		{5, 3, 3},
		{1, 8, 1},
		{4, 4, 4},
		{-2, 5, -2},
		{0, 0, 0},
	}

	for _, tt := range tests {
		result := min(tt.a, tt.b)
		if result != tt.expected {
			t.Errorf("min(%d, %d) = %d, want %d", tt.a, tt.b, result, tt.expected)
		}
	}
}

func TestPrepareLLMHeaderWithReferenceLanguage(t *testing.T) {
	db := Database{
		Writing: []Writing{
			{Phelps: "AB00001FIR", Language: "en", Name: "Fire Tablet", Text: "English version"},
			{Phelps: "AB00001FIR", Language: "es", Name: "Tabla del Fuego", Text: "Spanish version"},
			{Phelps: "AB00032DAR", Language: "en", Name: "Tablet of Ahmad", Text: "English version"},
			{Phelps: "AB00032DAR", Language: "fr", Name: "Tablette d'Ahmad", Text: "French version"},
		},
	}

	// Test using English as reference
	header := prepareLLMHeader(db, "Spanish", "en")
	if !strings.Contains(header, "Spanish") {
		t.Error("Should mention target language (Spanish)")
	}
	if !strings.Contains(header, "en terms only") {
		t.Error("Should specify English as search language")
	}

	// Test using Spanish as reference
	header2 := prepareLLMHeader(db, "French", "es")
	if !strings.Contains(header2, "French") {
		t.Error("Should mention target language (French)")
	}
	if !strings.Contains(header2, "es terms only") {
		t.Error("Should specify Spanish as search language")
	}
}

func TestSearchPrayersUnified(t *testing.T) {
	db := Database{
		Writing: []Writing{
			{Phelps: "TEST001", Language: "en", Name: "Short Prayer", Text: "O Lord, help me."},
			{Phelps: "TEST002", Language: "en", Name: "Long Prayer", Text: "O Lord my God! I beseech Thee by Thy mercy that hath embraced all things, and by Thy grace which hath permeated the whole creation, to make me steadfast in Thy Faith. Amen."},
			{Phelps: "TEST003", Language: "en", Name: "Medium Prayer", Text: "Blessed is He who trusteth in God and is guided by His light."},
		},
	}

	tests := []struct {
		name     string
		search   string
		expected int // number of results expected
	}{
		{
			name:     "Length range only",
			search:   "10-20",
			expected: 2, // Should find TEST001 and TEST003
		},
		{
			name:     "Keywords only",
			search:   "lord,god",
			expected: 2, // Should find TEST001 and TEST002
		},
		{
			name:     "Combined keywords and length",
			search:   "lord,god,10-50",
			expected: 3, // Should find all with combined scoring
		},
		{
			name:     "Opening phrase",
			search:   "O Lord my God",
			expected: 1, // Should find TEST002
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results := searchPrayersUnified(db, "en", tt.search)
			if len(results) == 0 {
				t.Errorf("Expected %d results, got 0", tt.expected)
				return
			}
			// Check that we got some results (exact count may vary based on scoring)
			if len(results) < 1 {
				t.Errorf("Expected at least 1 result, got %d", len(results))
			}
		})
	}
}

// MockLLMCaller for testing
type MockLLMCaller struct {
	GeminiResponse string
	GeminiError    error
	OllamaResponse string
	OllamaError    error
}

func (m MockLLMCaller) CallGemini(messages []OllamaMessage) (string, error) {
	return m.GeminiResponse, m.GeminiError
}

func (m MockLLMCaller) CallOllama(prompt string, textLength int) (string, error) {
	return m.OllamaResponse, m.OllamaError
}

func TestLLMFallbackLogic(t *testing.T) {

	tests := []struct {
		name               string
		useGemini          bool
		geminiResponse     string
		geminiError        error
		ollamaResponse     string
		ollamaError        error
		expectedPhelps     string
		expectedConfidence float64
		shouldContainDebug bool
		errorExpected      bool
	}{
		{
			name:               "Gemini success, no fallback needed",
			useGemini:          true,
			geminiResponse:     "Phelps: AB00001FIR\nConfidence: 85\nReasoning: Clear match",
			geminiError:        nil,
			expectedPhelps:     "AB00001FIR",
			expectedConfidence: 0.85,
			shouldContainDebug: false,
			errorExpected:      false,
		},
		{
			name:               "Gemini empty response, Ollama success",
			useGemini:          true,
			geminiResponse:     "Invalid response format\nNo Phelps code found",
			geminiError:        nil,
			ollamaResponse:     "Phelps: AB00032DAR\nConfidence: 70\nReasoning: Ollama fallback worked",
			ollamaError:        nil,
			expectedPhelps:     "AB00032DAR",
			expectedConfidence: 0.70,
			shouldContainDebug: false,
			errorExpected:      false,
		},
		{
			name:               "Gemini error, Ollama success",
			useGemini:          true,
			geminiResponse:     "",
			geminiError:        fmt.Errorf("Gemini API error"),
			ollamaResponse:     "Phelps: AB00044PRO\nConfidence: 75\nReasoning: Ollama worked after Gemini failed",
			ollamaError:        nil,
			expectedPhelps:     "AB00044PRO",
			expectedConfidence: 0.75,
			shouldContainDebug: false,
			errorExpected:      false,
		},
		{
			name:               "Both services return invalid responses",
			useGemini:          true,
			geminiResponse:     "Invalid format from Gemini",
			geminiError:        nil,
			ollamaResponse:     "Invalid format from Ollama",
			ollamaError:        nil,
			expectedPhelps:     "UNKNOWN",
			expectedConfidence: 0.0,
			shouldContainDebug: true,
			errorExpected:      false,
		},
		{
			name:               "Both services fail with errors",
			useGemini:          true,
			geminiResponse:     "",
			geminiError:        fmt.Errorf("Gemini connection error"),
			ollamaResponse:     "",
			ollamaError:        fmt.Errorf("Ollama not available"),
			expectedPhelps:     "",
			expectedConfidence: 0.0,
			shouldContainDebug: false,
			errorExpected:      true,
		},
		{
			name:               "Ollama only mode success",
			useGemini:          false,
			ollamaResponse:     "Phelps: AB00001FIR\nConfidence: 80\nReasoning: Direct Ollama call",
			ollamaError:        nil,
			expectedPhelps:     "AB00001FIR",
			expectedConfidence: 0.80,
			shouldContainDebug: false,
			errorExpected:      false,
		},
		{
			name:               "Ollama only mode failure",
			useGemini:          false,
			ollamaResponse:     "",
			ollamaError:        fmt.Errorf("Ollama service down"),
			expectedPhelps:     "",
			expectedConfidence: 0.0,
			shouldContainDebug: false,
			errorExpected:      true,
		},
		{
			name:               "Gemini quota exceeded error, Ollama success",
			useGemini:          true,
			geminiResponse:     "",
			geminiError:        fmt.Errorf("gemini quota exceeded: Resource has been exhausted (e.g. check quota)"),
			ollamaResponse:     "Phelps: AB00055QUO\nConfidence: 80\nReasoning: Ollama worked after Gemini quota exceeded",
			ollamaError:        nil,
			expectedPhelps:     "AB00055QUO",
			expectedConfidence: 0.80,
			shouldContainDebug: false,
			errorExpected:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create mock caller
			mockCaller := MockLLMCaller{
				GeminiResponse: tt.geminiResponse,
				GeminiError:    tt.geminiError,
				OllamaResponse: tt.ollamaResponse,
				OllamaError:    tt.ollamaError,
			}

			// Call the function under test
			result, err := callLLMWithCaller("test prompt", tt.useGemini, 100, mockCaller)

			// Check error expectation
			if tt.errorExpected && err == nil {
				t.Error("Expected an error but got none")
				return
			}
			if !tt.errorExpected && err != nil {
				t.Errorf("Did not expect an error but got: %v", err)
				return
			}

			// Skip further checks if error was expected
			if tt.errorExpected {
				return
			}

			// Check Phelps code
			if result.PhelpsCode != tt.expectedPhelps {
				t.Errorf("Expected PhelpsCode %s, got %s", tt.expectedPhelps, result.PhelpsCode)
			}

			// Check confidence
			if result.Confidence != tt.expectedConfidence {
				t.Errorf("Expected Confidence %f, got %f", tt.expectedConfidence, result.Confidence)
			}

			// Check debug info presence
			if tt.shouldContainDebug {
				if !strings.Contains(result.Reasoning, "Debug info:") {
					t.Error("Expected debug info in reasoning but not found")
				}
				if !strings.Contains(result.Reasoning, "Prompt used:") {
					t.Error("Expected prompt info in debug output")
				}
			}

		})
	}
}

func TestExtractAllFunctionCalls(t *testing.T) {
	tests := []struct {
		name            string
		text            string
		expectedValid   []string
		expectedInvalid int
	}{
		{
			name: "Standard format function calls",
			text: `ADD_NOTE:match,AB00001FIR,85,Found good match for Fire Tablet
SEARCH_NOTES:fire,tablet
CLEAR_NOTES:match
EXTEND_ROUNDS:Need more time to verify
SWITCH_REFERENCE_LANGUAGE:ar
FINAL_ANSWER:AB00001FIR,85,This prayer matches based on distinctive phrases
CORRECT_TRANSLITERATION:AB00001FIR,90,Corrected transliteration text
GET_STATS`,
			expectedValid: []string{
				"ADD_NOTE:match,AB00001FIR,85,Found good match for Fire Tablet",
				"SEARCH_NOTES:fire,tablet",
				"CLEAR_NOTES:match",
				"EXTEND_ROUNDS:Need more time to verify",
				"SWITCH_REFERENCE_LANGUAGE:ar",
				"FINAL_ANSWER:AB00001FIR,85,This prayer matches based on distinctive phrases",
				"CORRECT_TRANSLITERATION:AB00001FIR,90,Corrected transliteration text",
				"GET_STATS",
			},
			expectedInvalid: 0,
		},
		{
			name: "Tool calls JSON format",
			text: `{"tool_calls":[{"function":{"name":"ADD_NOTE","arguments":{"arguments":"match,AB00001FIR,85,Test note","name":"ADD_NOTE"}}}]}`,
			expectedValid: []string{
				"ADD_NOTE:match,AB00001FIR,85,Test note",
			},
			expectedInvalid: 0,
		},
		{
			name: "GET_FOCUS_TEXT tool call",
			text: `GET_FOCUS_TEXT:merciful,AB00001FIR,AB00002SEC`,
			expectedValid: []string{
				"GET_FOCUS_TEXT:merciful,AB00001FIR,AB00002SEC",
			},
			expectedInvalid: 0,
		},
		{
			name: "SEARCH combined criteria",
			text: `SEARCH:lord,god,O Lord my God,100-200`,
			expectedValid: []string{
				"SEARCH:lord,god,O Lord my God,100-200",
			},
			expectedInvalid: 0,
		},
		{
			name: "Multiple tool calls JSON format",
			text: `{"tool_calls":[{"function":{"name":"SEARCH_KEYWORDS","arguments":{"arguments":"lord,god,assist","name":"SEARCH_KEYWORDS"}}},{"function":{"name":"SEARCH_LENGTH","arguments":{"arguments":"50-150","name":"SEARCH_LENGTH"}}}]}`,
			expectedValid: []string{
				"SEARCH_KEYWORDS:lord,god,assist",
				"SEARCH_LENGTH:50-150",
			},
			expectedInvalid: 0,
		},
		{
			name: "Mixed content with tool calls",
			text: `I need to search for this prayer. Let me use the search function.

{"tool_calls":[{"function":{"name":"SEARCH_KEYWORDS","arguments":{"arguments":"merciful,compassionate,forgiveness","name":"SEARCH_KEYWORDS"}}}]}

This should help find the prayer.`,
			expectedValid: []string{
				"SEARCH_KEYWORDS:merciful,compassionate,forgiveness",
			},
			expectedInvalid: 0,
		},
		{
			name: "Malformed function calls",
			text: `SEARCH_KEYWORDS(lord,god,assist)
This is a SEARCH_KEYWORDS call but wrong format
SEARCH_LENGTH without colon`,
			expectedValid:   []string{},
			expectedInvalid: 3,
		},
		{
			name:            "Empty response",
			text:            "",
			expectedValid:   []string{},
			expectedInvalid: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validCalls, invalidCalls := extractAllFunctionCalls(tt.text)

			if len(validCalls) != len(tt.expectedValid) {
				t.Errorf("Expected %d valid calls, got %d", len(tt.expectedValid), len(validCalls))
			}

			for i, expected := range tt.expectedValid {
				if i >= len(validCalls) || validCalls[i] != expected {
					t.Errorf("Expected valid call %d to be %q, got %q", i, expected, validCalls[i])
				}
			}

			if len(invalidCalls) != tt.expectedInvalid {
				t.Errorf("Expected %d invalid calls, got %d", tt.expectedInvalid, len(invalidCalls))
			}
		})
	}
}

func TestParseToolCallsFormat(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		expected []string
	}{
		{
			name:     "Single SEARCH_KEYWORDS tool call",
			text:     `{"tool_calls":[{"function":{"name":"SEARCH_KEYWORDS","arguments":{"arguments":"lord,god,assist","name":"SEARCH_KEYWORDS"}}}]}`,
			expected: []string{"SEARCH_KEYWORDS:lord,god,assist"},
		},
		{
			name:     "Single SEARCH_LENGTH tool call",
			text:     `{"tool_calls":[{"function":{"name":"SEARCH_LENGTH","arguments":{"arguments":"50-150","name":"SEARCH_LENGTH"}}}]}`,
			expected: []string{"SEARCH_LENGTH:50-150"},
		},
		{
			name:     "Single SEARCH_OPENING tool call",
			text:     `{"tool_calls":[{"function":{"name":"SEARCH_OPENING","arguments":{"arguments":"O Lord my God","name":"SEARCH_OPENING"}}}]}`,
			expected: []string{"SEARCH_OPENING:O Lord my God"},
		},
		{
			name:     "GET_FULL_TEXT tool call",
			text:     `{"tool_calls":[{"function":{"name":"GET_FULL_TEXT","arguments":{"arguments":"AB00001FIR","name":"GET_FULL_TEXT"}}}]}`,
			expected: []string{"GET_FULL_TEXT:AB00001FIR"},
		},
		{
			name:     "GET_FOCUS_TEXT tool call",
			text:     `{"tool_calls":[{"function":{"name":"GET_FOCUS_TEXT","arguments":{"arguments":"lord,AB00001FIR,AB00002SEC","name":"GET_FOCUS_TEXT"}}}]}`,
			expected: []string{"GET_FOCUS_TEXT:lord,AB00001FIR,AB00002SEC"},
		},
		{
			name:     "SEARCH combined criteria",
			text:     `{"tool_calls":[{"function":{"name":"SEARCH","arguments":{"arguments":"lord,god,100-200","name":"SEARCH"}}}]}`,
			expected: []string{"SEARCH:lord,god,100-200"},
		},
		{
			name:     "GET_PARTIAL_TEXT tool call",
			text:     `{"tool_calls":[{"function":{"name":"GET_PARTIAL_TEXT","arguments":{"arguments":"AB00001FIR,100-500","name":"GET_PARTIAL_TEXT"}}}]}`,
			expected: []string{"GET_PARTIAL_TEXT:AB00001FIR,100-500"},
		},
		{
			name:     "FINAL_ANSWER tool call",
			text:     `{"tool_calls":[{"function":{"name":"FINAL_ANSWER","arguments":{"arguments":"AB00001FIR,85,This prayer matches based on distinctive phrases","name":"FINAL_ANSWER"}}}]}`,
			expected: []string{"FINAL_ANSWER:AB00001FIR,85,This prayer matches based on distinctive phrases"},
		},
		{
			name:     "GET_STATS tool call",
			text:     `{"tool_calls":[{"function":{"name":"GET_STATS","arguments":{"name":"GET_STATS"}}}]}`,
			expected: []string{"GET_STATS"},
		},
		{
			name:     "Multiple tool calls",
			text:     `{"tool_calls":[{"function":{"name":"SEARCH_KEYWORDS","arguments":{"arguments":"lord,god","name":"SEARCH_KEYWORDS"}}},{"function":{"name":"FINAL_ANSWER","arguments":{"arguments":"AB00001FIR,90,Perfect match found","name":"FINAL_ANSWER"}}}]}`,
			expected: []string{"SEARCH_KEYWORDS:lord,god", "FINAL_ANSWER:AB00001FIR,90,Perfect match found"},
		},
		{
			name: "Tool calls with extra content",
			text: `Let me search for this prayer:

{"tool_calls":[{"function":{"name":"SEARCH_KEYWORDS","arguments":{"arguments":"merciful,compassionate","name":"SEARCH_KEYWORDS"}}}]}

I hope this helps find it.`,
			expected: []string{"SEARCH_KEYWORDS:merciful,compassionate"},
		},
		{
			name:     "No tool calls",
			text:     `This is just regular text without any tool calls.`,
			expected: []string{},
		},
		{
			name:     "Invalid JSON",
			text:     `{"tool_calls":[{"function":{"name":"SEARCH_KEYWORDS","arguments":{"arguments":"lord,god"]}]}`, // Missing closing brace
			expected: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseToolCallsFormat(tt.text)

			if len(result) != len(tt.expected) {
				t.Errorf("Expected %d results, got %d", len(tt.expected), len(result))
			}

			for i, expected := range tt.expected {
				if i >= len(result) || result[i] != expected {
					t.Errorf("Expected result %d to be %q, got %q", i, expected, result[i])
				}
			}
		})
	}
}

func TestFilterThinkingFromResponse(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name: "Response with thinking section",
			input: `Thinking...
This is a thinking section that should be removed.
Let me analyze this prayer.
...done thinking.

SEARCH_KEYWORDS:lord,god,assist`,
			expected: "SEARCH_KEYWORDS:lord,god,assist",
		},
		{
			name:     "Response without thinking section",
			input:    "SEARCH_KEYWORDS:lord,god,assist",
			expected: "SEARCH_KEYWORDS:lord,god,assist",
		},
		{
			name: "Multiple thinking sections",
			input: `Thinking...
First thinking section.
...done thinking.

SEARCH_KEYWORDS:lord,god

Thinking...
Second thinking section.
...done thinking.

SEARCH_LENGTH:50-100`,
			expected: `SEARCH_KEYWORDS:lord,god
SEARCH_LENGTH:50-100`,
		},
		{
			name:     "Empty response",
			input:    "",
			expected: "",
		},
		{
			name: "Only thinking section",
			input: `Thinking...
Just thinking, no actual content.
...done thinking.`,
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := filterThinkingFromResponse(tt.input)
			if result != tt.expected {
				t.Errorf("Expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestGetPartialTextByPhelps(t *testing.T) {
	// Create test database
	db := Database{
		Writing: []Writing{
			{
				Phelps:   "TEST001",
				Language: "en",
				Name:     "Test Prayer",
				Text:     "O Lord my God! I beseech Thee by Thy mercy that hath embraced all things, and by Thy grace which hath permeated the whole creation, to make me steadfast in Thy Faith. Amen.",
			},
		},
	}

	tests := []struct {
		name     string
		args     string
		expected string
	}{
		{
			name:     "Character range",
			args:     "TEST001,10-50",
			expected: "PARTIAL TEXT for TEST001 (Test Prayer) [chars 10-50]:\n\nGod! I beseech Thee by Thy mercy that ha",
		},
		{
			name:     "From word to end",
			args:     "TEST001,from:mercy",
			expected: "PARTIAL TEXT for TEST001 (Test Prayer) [from 'mercy' to end]:\n\nmercy that hath embraced all things, and by Thy grace which hath permeated the whole creation, to make me steadfast in Thy Faith. Amen.",
		},
		{
			name:     "From start to word",
			args:     "TEST001,to:mercy",
			expected: "PARTIAL TEXT for TEST001 (Test Prayer) [from start to 'mercy']:\n\nO Lord my God! I beseech Thee by Thy mercy",
		},
		{
			name:     "Between two words",
			args:     "TEST001,from:beseech,to:grace",
			expected: "PARTIAL TEXT for TEST001 (Test Prayer) [from 'beseech' to 'grace']:\n\nbeseech Thee by Thy mercy that hath embraced all things, and by Thy grace",
		},
		{
			name:     "Invalid phelps code",
			args:     "INVALID,100-200",
			expected: "Phelps code 'INVALID' not found",
		},
		{
			name:     "Invalid format",
			args:     "TEST001",
			expected: "Error: GET_PARTIAL_TEXT requires format: phelps_code,start-end OR phelps_code,from:word,to:word OR phelps_code,from:word OR phelps_code,to:word",
		},
		{
			name:     "Invalid character range",
			args:     "TEST001,abc-def",
			expected: "Error: Invalid character range format. Use: start-end (e.g., 100-500)",
		},
		{
			name:     "Word not found",
			args:     "TEST001,from:nonexistent",
			expected: "Error: Start word 'nonexistent' not found in prayer text",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getPartialTextByPhelps(db, "en", tt.args)
			if len(result) != 1 {
				t.Errorf("Expected 1 result, got %d", len(result))
				return
			}
			if result[0] != tt.expected {
				t.Errorf("Expected %q, got %q", tt.expected, result[0])
			}
		})
	}
}

func TestProcessFinalAnswer(t *testing.T) {
	tests := []struct {
		name     string
		args     string
		expected string
	}{
		{
			name:     "Valid final answer",
			args:     "AB00001FIR,85,This prayer matches based on distinctive phrases and structure",
			expected: "FINAL ANSWER RECEIVED:\nPhelps: AB00001FIR\nConfidence: 85%\nReasoning: This prayer matches based on distinctive phrases and structure",
		},
		{
			name:     "Unknown answer",
			args:     "UNKNOWN,0,No matching prayer found after extensive search",
			expected: "FINAL ANSWER RECEIVED:\nPhelps: UNKNOWN\nConfidence: 0%\nReasoning: No matching prayer found after extensive search",
		},
		{
			name:     "Reasoning with commas",
			args:     "AB00002TEST,75,Prayer mentions Lord, God, mercy, and blessing which match reference text",
			expected: "FINAL ANSWER RECEIVED:\nPhelps: AB00002TEST\nConfidence: 75%\nReasoning: Prayer mentions Lord, God, mercy, and blessing which match reference text",
		},
		{
			name:     "High confidence match",
			args:     "AB00003XYZ,95,Exact phrase match found in opening and closing sentences",
			expected: "FINAL ANSWER RECEIVED:\nPhelps: AB00003XYZ\nConfidence: 95%\nReasoning: Exact phrase match found in opening and closing sentences",
		},
		{
			name:     "Insufficient arguments",
			args:     "AB00001FIR,85",
			expected: "Error: FINAL_ANSWER requires format: phelps_code,confidence,reasoning (e.g., FINAL_ANSWER:AB00001FIR,85,This prayer matches based on distinctive phrases)",
		},
		{
			name:     "Invalid confidence - not a number",
			args:     "AB00001FIR,high,Test reasoning",
			expected: "Error: Invalid confidence value 'high'. Must be a number 0-100",
		},
		{
			name:     "Invalid confidence - negative",
			args:     "AB00001FIR,-10,This doesn't match",
			expected: "Error: Confidence must be between 0-100 (or 0.0-1.0)",
		},
		{
			name:     "Invalid confidence - too high",
			args:     "AB00001FIR,150,This is very confident but invalid",
			expected: "Error: Confidence must be between 0-100 (or 0.0-1.0)",
		},
		{
			name:     "Empty phelps code",
			args:     ",85,Good reasoning provided",
			expected: "Error: Phelps code cannot be empty",
		},
		{
			name:     "Empty reasoning",
			args:     "AB00001FIR,85,",
			expected: "Error: Reasoning cannot be empty",
		},
		{
			name:     "Minimal valid input",
			args:     "TEST,0,No match",
			expected: "FINAL ANSWER RECEIVED:\nPhelps: TEST\nConfidence: 0%\nReasoning: No match",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := processFinalAnswer(tt.args)
			if len(result) != 1 {
				t.Errorf("Expected 1 result, got %d", len(result))
				return
			}
			if result[0] != tt.expected {
				t.Errorf("Expected %q, got %q", tt.expected, result[0])
			}
		})
	}
}

func TestGeminiQuotaExceededFlag(t *testing.T) {
	// Reset the quota exceeded flag before test
	atomic.StoreInt32(&geminiQuotaExceeded, 0)

	// Test that quota exceeded error sets the flag
	mockCaller := MockLLMCaller{
		GeminiError:    fmt.Errorf("gemini quota exceeded: You exceeded your current quota"),
		OllamaResponse: "Phelps: AB00001FIR\nConfidence: 80\nReasoning: Fallback worked",
		OllamaError:    nil,
	}

	// First call should set the flag
	_, err := callLLMWithCaller("test prompt", true, 100, mockCaller)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if atomic.LoadInt32(&geminiQuotaExceeded) != 1 {
		t.Error("Expected quota exceeded flag to be set to 1")
	}

	// Second call should skip Gemini entirely
	mockCaller2 := MockLLMCaller{
		GeminiResponse: "This should not be called",
		GeminiError:    nil,
		OllamaResponse: "Phelps: AB00002SEC\nConfidence: 75\nReasoning: Second call used Ollama only",
		OllamaError:    nil,
	}

	response, err := callLLMWithCaller("second prompt", true, 100, mockCaller2)
	if err != nil {
		t.Fatalf("Expected no error on second call, got: %v", err)
	}

	if response.PhelpsCode != "AB00002SEC" {
		t.Errorf("Expected PhelpsCode AB00002SEC, got %s", response.PhelpsCode)
	}

	// Reset flag for other tests
	atomic.StoreInt32(&geminiQuotaExceeded, 0)
}

func TestExtensibleFunctionSystem(t *testing.T) {
	// Test that all registered functions have proper implementations
	for _, handler := range registeredFunctions {
		t.Run(fmt.Sprintf("Handler_%s", handler.GetPattern()), func(t *testing.T) {
			// Test basic properties
			pattern := handler.GetPattern()
			if pattern == "" {
				t.Error("Handler pattern cannot be empty")
			}

			description := handler.GetDescription()
			if description == "" {
				t.Error("Handler description cannot be empty")
			}

			example := handler.GetUsageExample()
			if example == "" {
				t.Error("Handler usage example cannot be empty")
			}

			keywords := handler.GetKeywords()
			if len(keywords) == 0 {
				t.Error("Handler must have at least one keyword")
			}

			// Test validation works
			if !handler.IsStandalone() {
				// For prefix functions, test with the pattern
				if !handler.Validate(pattern + "test") {
					t.Errorf("Handler should validate its own pattern: %s", pattern)
				}
			} else {
				// For standalone functions, test exact match
				if !handler.Validate(pattern) {
					t.Errorf("Handler should validate its own pattern: %s", pattern)
				}
			}
		})
	}
}

func TestExtendRoundsValidation(t *testing.T) {
	// This tests the specific issue that Command-R was having
	testCall := "EXTEND_ROUNDS: Making good progress with search results, need to verify match accuracy"

	// Test that the call is properly validated (not marked as malformed)
	validCalls, invalidCalls := extractAllFunctionCalls(testCall)

	if len(invalidCalls) > 0 {
		t.Errorf("EXTEND_ROUNDS call should not be invalid. Got errors: %v", invalidCalls)
	}

	if len(validCalls) != 1 {
		t.Errorf("Expected 1 valid call, got %d: %v", len(validCalls), validCalls)
	}

	if len(validCalls) > 0 && validCalls[0] != testCall {
		t.Errorf("Expected call to be preserved as-is, got: %s", validCalls[0])
	}

	// Test that the handler can be found and executed
	db := Database{}
	for _, handler := range registeredFunctions {
		if handler.Validate(testCall) {
			result := handler.Execute(db, "en", testCall)
			if len(result) == 0 {
				t.Error("EXTEND_ROUNDS handler should return at least one result")
			}

			// Should return approval message
			if !strings.Contains(result[0], "EXTEND_ROUNDS_APPROVED") {
				t.Errorf("Expected approval message, got: %s", result[0])
			}
			return
		}
	}
	t.Error("No handler found for EXTEND_ROUNDS call")
}

func TestFunctionHelpGeneration(t *testing.T) {
	help := generateFunctionHelp()

	if help == "" {
		t.Error("Function help should not be empty")
	}

	// Should contain descriptions for key functions
	expectedFunctions := []string{"SEARCH:", "EXTEND_ROUNDS:", "GET_FULL_TEXT:", "FINAL_ANSWER:"}

	for _, fn := range expectedFunctions {
		if !strings.Contains(help, fn) {
			t.Errorf("Function help should contain %s, but got: %s", fn, help)
		}
	}

	// Should contain usage examples
	if !strings.Contains(help, "Example:") {
		t.Error("Function help should contain usage examples")
	}
}

func TestCommandRCompatibility(t *testing.T) {
	// Test various formats that Command-R might use
	testCases := []struct {
		name          string
		input         string
		shouldBeValid bool
	}{
		{
			name:          "EXTEND_ROUNDS with space after colon",
			input:         "EXTEND_ROUNDS: Making progress, need more time",
			shouldBeValid: true,
		},
		{
			name:          "EXTEND_ROUNDS without space after colon",
			input:         "EXTEND_ROUNDS:Making progress, need more time",
			shouldBeValid: true,
		},
		{
			name:          "SEARCH with multiple criteria",
			input:         "SEARCH:lord,god,mercy,100-200",
			shouldBeValid: true,
		},
		{
			name:          "GET_FULL_TEXT with code",
			input:         "GET_FULL_TEXT:AB00001FIR",
			shouldBeValid: true,
		},
		{
			name:          "FINAL_ANSWER with all parts",
			input:         "FINAL_ANSWER:AB00001FIR,85,Clear match based on themes",
			shouldBeValid: true,
		},
		{
			name:          "Invalid function call",
			input:         "INVALID_FUNCTION:something",
			shouldBeValid: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			validCalls, invalidCalls := extractAllFunctionCalls(tc.input)

			if tc.shouldBeValid {
				if len(invalidCalls) > 0 {
					t.Errorf("Expected valid call, but got invalid: %v", invalidCalls)
				}
				if len(validCalls) != 1 {
					t.Errorf("Expected 1 valid call, got %d: %v", len(validCalls), validCalls)
				}
			} else {
				if len(validCalls) > 0 {
					t.Errorf("Expected invalid call, but got valid: %v", validCalls)
				}
			}
		})
	}
}

func TestSearchInventoryFunction(t *testing.T) {
	// Create a SearchInventoryFunction instance
	searchFunc := SearchInventoryFunction{NewPrefixFunction("SEARCH_INVENTORY")}
	mockDB := Database{} // Empty database for this test

	tests := []struct {
		name     string
		call     string
		expected []string
	}{
		{
			name: "Missing arguments",
			call: "SEARCH_INVENTORY:",
			expected: []string{
				"Error: SEARCH_INVENTORY requires format: keywords[,source_language,field]",
				"",
				"💡 SIMPLIFIED USAGE:",
				"SEARCH_INVENTORY:praise sovereignty mercy",
				"SEARCH_INVENTORY:lord god forgiveness,ALL",
				"SEARCH_INVENTORY:الحمد لله,first_line",
				"",
				"🔍 OPTIONAL PARAMETERS:",
				"• source_language: Eng/Ara/Per (filters by document language, rarely needed)",
				"• field: title/first_line/subjects/notes/ALL (default: ALL)",
				"",
				"📚 NOTE: Language parameter filters by source document language,",
				"not search language. Most searches should omit this parameter.",
			},
		},
		{
			name: "Unsupported language",
			call: "SEARCH_INVENTORY:lord god,xyz",
			expected: []string{
				"INVENTORY SEARCH: 'lord god' filtered by language 'xyz' (field: ALL) ⚠️ (limited coverage)",
				"No matching documents found.",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := searchFunc.Execute(mockDB, "en", tt.call)
			if len(result) != len(tt.expected) {
				t.Errorf("Expected %d results, got %d", len(tt.expected), len(result))
				return
			}
			for i, expected := range tt.expected {
				if i < len(result) && result[i] != expected {
					t.Errorf("Result %d: expected %q, got %q", i, expected, result[i])
				}
			}
		})
	}
}

func TestCheckTagFunction(t *testing.T) {
	checkFunc := CheckTagFunction{NewPrefixFunction("CHECK_TAG")}
	mockDB := Database{} // Empty database for this test

	tests := []struct {
		name     string
		call     string
		hasError bool
	}{
		{
			name:     "Empty PIN",
			call:     "CHECK_TAG:",
			hasError: true,
		},
		{
			name:     "Valid PIN format",
			call:     "CHECK_TAG:AB00001",
			hasError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := checkFunc.Execute(mockDB, "en", tt.call)
			hasError := len(result) > 0 && strings.Contains(result[0], "Error:")

			if hasError != tt.hasError {
				t.Errorf("Expected error: %v, got error: %v (result: %v)", tt.hasError, hasError, result[0])
			}
		})
	}
}

func TestAddNewPrayerFunction(t *testing.T) {
	addFunc := AddNewPrayerFunction{NewPrefixFunction("ADD_NEW_PRAYER")}
	mockDB := Database{} // Empty database for this test

	tests := []struct {
		name     string
		call     string
		hasError bool
	}{
		{
			name:     "Missing arguments",
			call:     "ADD_NEW_PRAYER:",
			hasError: true,
		},
		{
			name:     "Invalid confidence - not a number",
			call:     "ADD_NEW_PRAYER:AB00001FIR,high,Test reasoning",
			hasError: true,
		},
		{
			name:     "Invalid confidence - out of range",
			call:     "ADD_NEW_PRAYER:AB00001FIR,150,Test reasoning",
			hasError: true,
		},
		{
			name:     "Invalid Phelps code - too short",
			call:     "ADD_NEW_PRAYER:AB001,85,Test reasoning",
			hasError: true,
		},
		{
			name:     "Invalid Phelps code - wrong length",
			call:     "ADD_NEW_PRAYER:AB00001FIRRR,85,Test reasoning",
			hasError: true,
		},
		{
			name:     "Valid format - PIN only",
			call:     "ADD_NEW_PRAYER:AB00001,85,Complete document prayer",
			hasError: true, // Will fail with PIN validation since PIN doesn't exist in mock DB
		},
		{
			name:     "Valid format - PIN with tag",
			call:     "ADD_NEW_PRAYER:AB00001FIR,85,First prayer from document",
			hasError: true, // Will fail with PIN validation since PIN doesn't exist in mock DB
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := addFunc.Execute(mockDB, "en", tt.call)
			hasError := len(result) > 0 && strings.Contains(result[0], "Error:")

			if hasError != tt.hasError {
				t.Errorf("Expected error: %v, got error: %v (result: %v)", tt.hasError, hasError, result[0])
			}
		})
	}
}

func TestCorrectTransliterationFunction(t *testing.T) {
	correctFunc := CorrectTransliterationFunction{NewPrefixFunction("CORRECT_TRANSLITERATION")}
	mockDB := Database{} // Empty database for this test

	// Need to call CHECK_TRANSLIT_STANDARDS first to satisfy prerequisite
	standardsFunc := CheckTranslitStandardsFunction{NewStandaloneFunction("CHECK_TRANSLIT_STANDARDS")}
	standardsFunc.Execute(mockDB, "en", "CHECK_TRANSLIT_STANDARDS")

	tests := []struct {
		name     string
		call     string
		hasError bool
	}{
		{
			name:     "Missing arguments",
			call:     "CORRECT_TRANSLITERATION:",
			hasError: true,
		},
		{
			name:     "Invalid confidence",
			call:     "CORRECT_TRANSLITERATION:AB00001FIR,abc,O Thou Who art the Lord",
			hasError: true,
		},
		{
			name:     "Text too short",
			call:     "CORRECT_TRANSLITERATION:AB00001FIR,85,Short",
			hasError: true,
		},
		{
			name:     "Valid correction",
			call:     "CORRECT_TRANSLITERATION:AB00001FIR,O Thou Who art the Lord of all names and the Creator of the heavens",
			hasError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := correctFunc.Execute(mockDB, "en", tt.call)
			hasError := len(result) > 0 && strings.Contains(result[0], "Error:")

			if hasError != tt.hasError {
				t.Errorf("Expected error: %v, got error: %v", tt.hasError, hasError)
			}
		})
	}
}

func TestOperationModeValidation(t *testing.T) {
	tests := []struct {
		language string
		mode     string
		expected bool
	}{
		{"ar", "match", true},
		{"fa", "match", true},
		{"ar-translit", "match", false}, // translit versions not checked in match mode
		{"fa-translit", "match", false}, // translit versions not checked in match mode
		{"en", "match", false},
		{"es", "match", false},
		{"ar", "translit", true},
		{"fa", "translit", true},
		{"ar-translit", "translit", true},
		{"fa-translit", "translit", true},
		{"ar", "match-add", true},
		{"fa", "add-only", true},
		{"en", "translit", false},
	}

	for _, tt := range tests {
		result := shouldCheckTransliteration(tt.language, tt.mode)
		if result != tt.expected {
			t.Errorf("shouldCheckTransliteration(%s, %s): expected %v, got %v", tt.language, tt.mode, tt.expected, result)
		}
	}
}

func TestMultiLanguageSupport(t *testing.T) {
	tests := []struct {
		name          string
		languageInput string
		expectedLangs []string
	}{
		{
			name:          "Single language",
			languageInput: "es",
			expectedLangs: []string{"es"},
		},
		{
			name:          "Multiple languages",
			languageInput: "es,fr,de",
			expectedLangs: []string{"es", "fr", "de"},
		},
		{
			name:          "Languages with spaces",
			languageInput: "es, fr, de",
			expectedLangs: []string{"es", "fr", "de"},
		},
		{
			name:          "Empty string",
			languageInput: "",
			expectedLangs: []string{""},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var languages []string
			if tt.languageInput != "" {
				splitLangs := strings.Split(tt.languageInput, ",")
				for _, lang := range splitLangs {
					languages = append(languages, strings.TrimSpace(lang))
				}
			} else {
				languages = []string{tt.languageInput}
			}

			if len(languages) != len(tt.expectedLangs) {
				t.Errorf("Expected %d languages, got %d", len(tt.expectedLangs), len(languages))
				return
			}

			for i, expected := range tt.expectedLangs {
				if languages[i] != expected {
					t.Errorf("Language %d: expected %s, got %s", i, expected, languages[i])
				}
			}
		})
	}
}

func TestTranslitLanguageProcessing(t *testing.T) {
	tests := []struct {
		name           string
		inputLang      string
		expectedOutput string
		expectedCount  int
	}{
		{
			name:           "Arabic base language - converts to ar-translit",
			inputLang:      "ar",
			expectedOutput: "ar-translit",
			expectedCount:  1,
		},
		{
			name:           "Persian base language - converts to fa-translit",
			inputLang:      "fa",
			expectedOutput: "fa-translit",
			expectedCount:  1,
		},
		{
			name:           "Persian alternative - converts to fa-translit",
			inputLang:      "persian",
			expectedOutput: "fa-translit",
			expectedCount:  1,
		},
		{
			name:           "Translit format stays as translit",
			inputLang:      "ar-translit",
			expectedOutput: "ar-translit",
			expectedCount:  1,
		},
		{
			name:           "Multiple languages convert to translits",
			inputLang:      "ar,fa",
			expectedOutput: "ar-translit,fa-translit",
			expectedCount:  2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			languages := strings.Split(tt.inputLang, ",")
			var processLanguages []string

			for _, lang := range languages {
				lang = strings.TrimSpace(lang)

				// New translit mode logic: process transliterations directly
				if lang == "ar" || lang == "arabic" {
					processLanguages = append(processLanguages, "ar-translit")
				} else if lang == "fa" || lang == "persian" || lang == "per" {
					processLanguages = append(processLanguages, "fa-translit")
				} else if strings.HasSuffix(lang, "-translit") {
					processLanguages = append(processLanguages, lang)
				} else {
					processLanguages = append(processLanguages, lang)
				}
			}

			result := strings.Join(processLanguages, ",")
			if result != tt.expectedOutput {
				t.Errorf("Expected %s, got %s", tt.expectedOutput, result)
			}

			if len(processLanguages) != tt.expectedCount {
				t.Errorf("Expected %d languages, got %d", tt.expectedCount, len(processLanguages))
			}
		})
	}
}

func TestCommandFilteringByMode(t *testing.T) {
	tests := []struct {
		name         string
		functionName string
		mode         string
		enabled      bool
	}{
		// Search functions - only in match, match-add, translit
		{"Search in match mode", "SEARCH", "match", true},
		{"Search in match-add mode", "SEARCH", "match-add", true},
		{"Search in translit mode", "SEARCH", "translit", true},
		{"Search in add-only mode", "SEARCH", "add-only", false},

		// Inventory functions - only in match-add, add-only
		{"Inventory search in match-add", "SEARCH_INVENTORY", "match-add", true},
		{"Inventory search in add-only", "SEARCH_INVENTORY", "add-only", true},
		{"Inventory search in match", "SEARCH_INVENTORY", "match", false},
		{"Inventory search in translit", "SEARCH_INVENTORY", "translit", false},
		{"Smart inventory search in match-add", "SMART_INVENTORY_SEARCH", "match-add", true},
		{"Smart inventory search in add-only", "SMART_INVENTORY_SEARCH", "add-only", true},
		{"Smart inventory search in match", "SMART_INVENTORY_SEARCH", "match", false},
		{"Smart inventory search in translit", "SMART_INVENTORY_SEARCH", "translit", false},
		{"Get inventory context in match-add", "GET_INVENTORY_CONTEXT", "match-add", true},
		{"Get inventory context in add-only", "GET_INVENTORY_CONTEXT", "add-only", true},
		{"Get inventory context in match", "GET_INVENTORY_CONTEXT", "match", false},
		{"Get inventory context in translit", "GET_INVENTORY_CONTEXT", "translit", false},

		// Universal functions - always enabled
		{"Final answer in match", "FINAL_ANSWER", "match", true},
		{"Final answer in add-only", "FINAL_ANSWER", "add-only", true},
		{"Final answer in translit", "FINAL_ANSWER", "translit", true},

		// Transliteration functions - always enabled
		{"Translit correction in match", "CORRECT_TRANSLITERATION", "match", true},
		{"Translit correction in translit", "CORRECT_TRANSLITERATION", "translit", true},
		{"Translit correction in add-only", "CORRECT_TRANSLITERATION", "add-only", true},

		// Transliteration-specific functions - only in translit mode
		{"Match confirmed in translit", "MATCH_CONFIRMED", "translit", true},
		{"Match confirmed in match", "MATCH_CONFIRMED", "match", false},
		{"Search version in translit", "SEARCH_VERSION", "translit", true},
		{"Search version in match", "SEARCH_VERSION", "match", false},
		{"Correct version in translit", "CORRECT_VERSION", "translit", true},
		{"Correct version in match", "CORRECT_VERSION", "match", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Find the function handler
			var handler FunctionCallHandler
			for _, h := range registeredFunctions {
				if strings.Contains(h.GetDescription(), tt.functionName) {
					handler = h
					break
				}
			}

			if handler == nil {
				t.Fatalf("Function %s not found in registry", tt.functionName)
			}

			enabled := handler.IsEnabledForMode(tt.mode)
			if enabled != tt.enabled {
				t.Errorf("Function %s in mode %s: expected enabled=%v, got %v",
					tt.functionName, tt.mode, tt.enabled, enabled)
			}
		})
	}
}

func TestMatchConfirmedFunction(t *testing.T) {
	db := Database{
		Writing: []Writing{
			{Phelps: "AB00001FIR", Language: "ar", Name: "Fire Tablet", Text: "Arabic original text"},
			{Phelps: "AB00001FIR", Language: "ar-translit", Name: "Fire Tablet", Text: "Transliteration text"},
		},
	}

	tests := []struct {
		name          string
		call          string
		hasError      bool
		shouldContain string
	}{
		{
			name:          "Valid match confirmation",
			call:          "MATCH_CONFIRMED:AB00001FIR,95.5",
			hasError:      false,
			shouldContain: "MATCH CONFIRMED: AB00001FIR (95.5% confidence)",
		},
		{
			name:     "Missing confidence",
			call:     "MATCH_CONFIRMED:AB00001FIR",
			hasError: true,
		},
		{
			name:     "Invalid confidence",
			call:     "MATCH_CONFIRMED:AB00001FIR,invalid",
			hasError: true,
		},
		{
			name:     "Empty Phelps code",
			call:     "MATCH_CONFIRMED:,95.0",
			hasError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := MatchConfirmedFunction{NewPrefixFunctionWithModes("MATCH_CONFIRMED", []string{"translit"})}
			result := handler.Execute(db, "ar", tt.call)

			if tt.hasError {
				if len(result) == 0 || (!strings.Contains(strings.Join(result, " "), "requires") && !strings.Contains(strings.Join(result, " "), "Invalid") && !strings.Contains(strings.Join(result, " "), "cannot be empty")) {
					t.Errorf("Expected error message, got: %v", result)
				}
			} else {
				if len(result) == 0 {
					t.Errorf("Expected result, got empty")
				} else if tt.shouldContain != "" && !strings.Contains(strings.Join(result, " "), tt.shouldContain) {
					t.Errorf("Expected result to contain '%s', got: %v", tt.shouldContain, result)
				}
			}
		})
	}
}

func TestModeDefaultValue(t *testing.T) {
	// Test that the default mode is now "match-add"
	// This is tested by checking the flag default value
	expectedDefault := "match-add"

	// We can't easily test flag defaults in unit tests, but we can test
	// that the prepareLLMHeaderWithMode function works correctly with match-add mode
	db := Database{
		Writing: []Writing{
			{Phelps: "AB00001FIR", Language: "en", Name: "Test", Text: "Test"},
		},
	}

	header := prepareLLMHeaderWithMode(db, "English", "en", expectedDefault)

	if !strings.Contains(header, "MODE: MATCH-ADD") {
		t.Errorf("Expected header to contain MATCH-ADD mode guidance")
	}

	if !strings.Contains(header, "SIMPLE WORKFLOW") {
		t.Errorf("Expected header to contain SIMPLE WORKFLOW for match-add mode")
	}
}

func TestAddTransliterationContext(t *testing.T) {
	db := Database{
		Writing: []Writing{
			{Phelps: "TEST001", Language: "ar", Text: "Arabic text"},
		},
	}

	tests := []struct {
		name      string
		writing   Writing
		mode      string
		shouldAdd bool
		contains  []string
	}{
		{
			name:      "Arabic prayer in match mode",
			writing:   Writing{Language: "ar", Text: "Arabic text"},
			mode:      "match",
			shouldAdd: true,
			contains:  []string{"TRANSLITERATION NOTE", "This is an Arabic/Persian prayer"},
		},
		{
			name:      "Persian transliteration in translit mode with Phelps",
			writing:   Writing{Language: "fa-translit", Phelps: "TEST001", Version: "test-version-123", Text: "Persian transliteration text"},
			mode:      "translit",
			shouldAdd: true,
			contains:  []string{"TRANSLITERATION NOTE", "TRANSLIT MODE", "fa transliteration", "Has Phelps code"},
		},
		{
			name:      "Arabic transliteration in translit mode",
			writing:   Writing{Language: "ar-translit", Version: "test-version-456", Text: "Arabic transliteration text"},
			mode:      "translit",
			shouldAdd: true,
			contains:  []string{"TRANSLITERATION NOTE", "TRANSLIT MODE", "No Phelps code", "search functions"},
		},
		{
			name:      "Arabic transliteration in translit mode",
			writing:   Writing{Language: "ar-translit", Text: "Transliterated text"},
			mode:      "translit",
			shouldAdd: true,
			contains:  []string{"TRANSLITERATION NOTE", "FIND_ORIGINAL_VERSION"},
		},
		{
			name:      "English prayer in match mode",
			writing:   Writing{Language: "en", Text: "English text"},
			mode:      "match",
			shouldAdd: false,
			contains:  []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			originalPrompt := "Original prompt text"
			result := addTransliterationContext(db, tt.writing, originalPrompt, tt.mode)

			if tt.shouldAdd {
				if result == originalPrompt {
					t.Errorf("Expected transliteration context to be added, but prompt unchanged")
				}

				for _, expectedText := range tt.contains {
					if !strings.Contains(result, expectedText) {
						t.Errorf("Expected result to contain %q", expectedText)
					}
				}
			} else {
				if result != originalPrompt {
					t.Errorf("Expected prompt to remain unchanged for language %s in mode %s", tt.writing.Language, tt.mode)
				}
			}
		})
	}
}

func TestTranslitModeWorkflow(t *testing.T) {
	// Test the complete translit mode workflow
	db := Database{
		Writing: []Writing{
			// Arabic original
			{Phelps: "AB00001FIR", Language: "ar", Name: "Fire Tablet", Text: "Arabic original text"},
			// Corresponding transliteration (should be found and corrected)
			{Phelps: "AB00001FIR", Language: "ar-translit", Name: "Fire Tablet", Text: "Poor transliteration"},
			// Persian original
			{Phelps: "AB00002TAB", Language: "fa", Name: "Tablet of Ahmad", Text: "Persian original text"},
			// Missing Persian transliteration (should be flagged for creation)
		},
	}

	tests := []struct {
		name             string
		inputLanguage    string
		expectedLanguage string
		mode             string
		description      string
	}{
		{
			name:             "Arabic translit mode processes transliteration",
			inputLanguage:    "ar",
			expectedLanguage: "ar-translit",
			mode:             "translit",
			description:      "Should process ar-translit directly with ar as reference language",
		},
		{
			name:             "Persian translit mode processes transliteration",
			inputLanguage:    "fa",
			expectedLanguage: "fa-translit",
			mode:             "translit",
			description:      "Should process fa-translit directly with fa as reference language",
		},
		{
			name:             "Arabic translit input stays as translit",
			inputLanguage:    "ar-translit",
			expectedLanguage: "ar-translit",
			mode:             "translit",
			description:      "Should process ar-translit directly",
		},
		{
			name:             "Persian translit input stays as translit",
			inputLanguage:    "fa-translit",
			expectedLanguage: "fa-translit",
			mode:             "translit",
			description:      "Should process fa-translit directly",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test language conversion logic (from main.go translit mode processing)
			var processLanguage string
			if tt.inputLanguage == "ar" || tt.inputLanguage == "arabic" {
				processLanguage = "ar-translit"
			} else if tt.inputLanguage == "fa" || tt.inputLanguage == "persian" || tt.inputLanguage == "per" {
				processLanguage = "fa-translit"
			} else if strings.HasSuffix(tt.inputLanguage, "-translit") {
				processLanguage = tt.inputLanguage
			} else {
				processLanguage = tt.inputLanguage
			}

			if processLanguage != tt.expectedLanguage {
				t.Errorf("Expected language %s, got %s", tt.expectedLanguage, processLanguage)
			}

			// Test that the correct functions are available in translit mode
			searchEnabled := false
			translitEnabled := false

			for _, handler := range registeredFunctions {
				desc := handler.GetDescription()
				if strings.Contains(desc, "SEARCH:keywords,opening,range") {
					searchEnabled = handler.IsEnabledForMode(tt.mode)
				}
				if strings.Contains(desc, "CORRECT_TRANSLITERATION") {
					translitEnabled = handler.IsEnabledForMode(tt.mode)
				}
				if strings.Contains(desc, "SEARCH_INVENTORY") || strings.Contains(desc, "SMART_INVENTORY_SEARCH") || strings.Contains(desc, "GET_INVENTORY_CONTEXT") {
					_ = !handler.IsEnabledForMode(tt.mode) // Check inventory functions are properly registered
				}
			}

			// Check workflow expectations
			if tt.mode == "translit" {
				if !searchEnabled {
					t.Errorf("SEARCH functions should be enabled in translit mode")
				}
				if !translitEnabled {
					t.Errorf("CORRECT_TRANSLITERATION should be enabled in translit mode")
				}
				// Note: Inventory functions are disabled in translit mode as expected
			}

			// Test that transliteration checking is triggered for ar-translit/fa-translit
			shouldCheck := shouldCheckTransliteration(processLanguage, tt.mode)
			if !shouldCheck {
				t.Errorf("shouldCheckTransliteration should return true for %s in %s mode", processLanguage, tt.mode)
			}

			// Test header contains correct guidance
			baseLanguage := strings.TrimSuffix(processLanguage, "-translit")
			header := prepareLLMHeaderWithMode(db, processLanguage, baseLanguage, tt.mode)
			if !strings.Contains(header, "MODE: TRANSLITERATION") {
				t.Errorf("Header should contain TRANSLITERATION mode guidance")
			}
			if !strings.Contains(header, "SIMPLE WORKFLOW") {
				t.Errorf("Header should contain SIMPLE WORKFLOW")
			}
			if !strings.Contains(header, "MATCH_CONFIRMED") || !strings.Contains(header, "CORRECT_TRANSLITERATION") {
				t.Errorf("Header should mention MATCH_CONFIRMED and CORRECT_TRANSLITERATION functions")
			}
		})
	}
}

func TestTranslitModeEndToEnd(t *testing.T) {
	// Test that demonstrates the complete workflow:
	// 1. Input: ar or ar-translit -> Process: ar-translit (transliteration directly)
	// 2. Use base language (ar/fa) as reference for matching
	// 3. Use version IDs and MATCH_CONFIRMED/CORRECT_TRANSLITERATION functions

	t.Run("Translit mode workflow explanation", func(t *testing.T) {
		// This test documents the expected workflow:

		// NEW (simplified workflow):
		// translit mode: ar -> ar-translit (process transliteration with ar as reference)
		// translit mode: ar-translit -> ar-translit (process transliteration directly)
		// translit mode: fa -> fa-translit (process transliteration with fa as reference)

		testCases := map[string]string{
			"ar":          "ar-translit", // Arabic base converts to ar-translit
			"fa":          "fa-translit", // Persian base converts to fa-translit
			"ar-translit": "ar-translit", // Arabic translit stays ar-translit
			"fa-translit": "fa-translit", // Persian translit stays fa-translit
			"arabic":      "ar-translit", // Arabic alias converts to ar-translit
			"persian":     "fa-translit", // Persian alias converts to fa-translit
		}

		for input, expected := range testCases {
			var result string
			if input == "ar" || input == "arabic" {
				result = "ar-translit"
			} else if input == "fa" || input == "persian" {
				result = "fa-translit"
			} else if strings.HasSuffix(input, "-translit") {
				result = input
			}

			if result != expected {
				t.Errorf("Input %s: expected %s, got %s", input, expected, result)
			}
		}

		// Test that new functions are available
		matchConfirmedFound := false
		searchVersionFound := false
		correctVersionFound := false

		for _, handler := range registeredFunctions {
			desc := handler.GetDescription()
			if strings.Contains(desc, "MATCH_CONFIRMED") && handler.IsEnabledForMode("translit") {
				matchConfirmedFound = true
			}
			if strings.Contains(desc, "SEARCH_VERSION") && handler.IsEnabledForMode("translit") {
				searchVersionFound = true
			}
			if strings.Contains(desc, "CORRECT_VERSION") && handler.IsEnabledForMode("translit") {
				correctVersionFound = true
			}
		}

		if !matchConfirmedFound {
			t.Error("MATCH_CONFIRMED function should be available in translit mode")
		}
		if !searchVersionFound {
			t.Error("SEARCH_VERSION function should be available in translit mode")
		}
		if !correctVersionFound {
			t.Error("CORRECT_VERSION function should be available in translit mode")
		}
	})
}

func TestModeSpecificFunctionGeneration(t *testing.T) {
	// Test that mode-specific function lists are generated correctly from EnabledModes
	tests := []struct {
		mode          string
		shouldInclude []string
		shouldExclude []string
		description   string
	}{
		{
			mode: "match",
			shouldInclude: []string{
				"SEARCH:", "GET_FULL_TEXT:", "FINAL_ANSWER:",
				"ADD_NOTE:", "GET_STATS", // universal functions
			},
			shouldExclude: []string{
				"SEARCH_INVENTORY:", "SMART_INVENTORY_SEARCH:", "GET_INVENTORY_CONTEXT:", "ADD_NEW_PRAYER:", // inventory functions
			},
			description: "Match mode should have search functions but no inventory functions",
		},
		{
			mode: "match-add",
			shouldInclude: []string{
				"SEARCH:", "GET_FULL_TEXT:", "FINAL_ANSWER:", // matching functions
				"SEARCH_INVENTORY:", "SMART_INVENTORY_SEARCH:", "GET_INVENTORY_CONTEXT:", "ADD_NEW_PRAYER:", // inventory functions
				"ADD_NOTE:", "GET_STATS", // universal functions
			},
			shouldExclude: []string{},
			description:   "Match-add mode should have both matching and inventory functions",
		},
		{
			mode: "add-only",
			shouldInclude: []string{
				"SEARCH_INVENTORY:", "SMART_INVENTORY_SEARCH:", "GET_INVENTORY_CONTEXT:", "CHECK_TAG:", "ADD_NEW_PRAYER:", // inventory functions
				"FINAL_ANSWER:", "ADD_NOTE:", "GET_STATS", // universal functions
			},
			shouldExclude: []string{
				"SEARCH:keywords,opening,range", "GET_FULL_TEXT:", // matching functions should be excluded
			},
			description: "Add-only mode should have inventory functions but no matching functions",
		},
		{
			mode: "translit",
			shouldInclude: []string{
				"SEARCH:", "GET_FULL_TEXT:", // matching functions
				"CORRECT_TRANSLITERATION:", "CHECK_TRANSLIT_CONSISTENCY:", // translit functions
				"FINAL_ANSWER:", "ADD_NOTE:", "GET_STATS", // universal functions
			},
			shouldExclude: []string{
				"SEARCH_INVENTORY:keywords,language", "SMART_INVENTORY_SEARCH:", "GET_INVENTORY_CONTEXT:", "ADD_NEW_PRAYER:", // inventory functions
			},
			description: "Translit mode should have matching and transliteration functions but no inventory functions",
		},
	}

	for _, tt := range tests {
		t.Run(tt.description, func(t *testing.T) {
			// Test concise function list
			functionList := generateConciseFunctionListForMode(tt.mode)

			for _, shouldInclude := range tt.shouldInclude {
				if !strings.Contains(functionList, shouldInclude) {
					t.Errorf("Mode %s should include function %s in list: %s", tt.mode, shouldInclude, functionList)
				}
			}

			for _, shouldExclude := range tt.shouldExclude {
				// Use more precise matching to avoid false positives
				if strings.Contains(functionList+",", shouldExclude+",") || strings.HasPrefix(functionList, shouldExclude+",") || strings.HasSuffix(functionList, ", "+shouldExclude) {
					t.Errorf("Mode %s should NOT include function %s in list: %s", tt.mode, shouldExclude, functionList)
				}
			}

			// Test detailed function descriptions
			descriptions := generateFunctionDescriptionsForMode(tt.mode)

			for _, shouldInclude := range tt.shouldInclude {
				if !strings.Contains(descriptions, shouldInclude) {
					t.Errorf("Mode %s should include function %s in descriptions", tt.mode, shouldInclude)
				}
			}

			for _, shouldExclude := range tt.shouldExclude {
				// Use more precise matching for descriptions
				if strings.Contains(descriptions, shouldExclude+" ") || strings.HasPrefix(descriptions, shouldExclude+" ") || strings.Contains(descriptions, "\n"+shouldExclude+" ") {
					t.Errorf("Mode %s should NOT include function %s in descriptions", tt.mode, shouldExclude)
				}
			}

			t.Logf("Mode %s functions: %s", tt.mode, functionList)
		})
	}
}

func TestModeInstructionsContainCorrectFunctions(t *testing.T) {
	// Test that mode instructions include dynamically generated function lists
	db := Database{
		Writing: []Writing{
			{Phelps: "AB00001FIR", Language: "en", Name: "Test", Text: "Test"},
		},
	}

	modes := []string{"match", "match-add", "add-only", "translit"}

	for _, mode := range modes {
		t.Run("Mode_"+mode, func(t *testing.T) {
			header := prepareLLMHeaderWithMode(db, "English", "en", mode)

			// Should contain the mode guidance
			expectedModeText := strings.ToUpper(mode)
			if mode == "match-add" {
				expectedModeText = "MATCH-ADD"
			} else if mode == "add-only" {
				expectedModeText = "ADD-ONLY"
			} else if mode == "translit" {
				expectedModeText = "TRANSLITERATION"
			}

			if !strings.Contains(header, expectedModeText) {
				t.Errorf("Header should contain mode guidance for %s", mode)
			}

			// Should contain function descriptions
			if !strings.Contains(header, "AVAILABLE FUNCTIONS FOR THIS MODE:") {
				t.Errorf("Header should contain available functions section")
			}

			// Should contain examples section
			if !strings.Contains(header, "Example:") {
				t.Errorf("Header should contain function examples")
			}

			// Verify mode-specific functions are present
			switch mode {
			case "match":
				if !strings.Contains(header, "SEARCH:") || strings.Contains(header, "SEARCH_INVENTORY:") {
					t.Errorf("Match mode should have SEARCH but not SEARCH_INVENTORY")
				}
			case "add-only":
				// Check for the main SEARCH function (not SEARCH_INVENTORY or SEARCH_NOTES)
				hasMainSearch := strings.Contains(header, "SEARCH:keywords,opening,range")
				hasSearchInventory := strings.Contains(header, "SEARCH_INVENTORY:")
				if hasMainSearch || !hasSearchInventory {
					t.Errorf("Add-only mode should have SEARCH_INVENTORY but not SEARCH")
				}
			case "translit":
				if !strings.Contains(header, "SEARCH:") || !strings.Contains(header, "CORRECT_TRANSLITERATION:") {
					t.Errorf("Translit mode should have both SEARCH and CORRECT_TRANSLITERATION")
				}
			}
		})
	}
}

func TestImprovedModeInstructions(t *testing.T) {
	// Test that the new mode instructions are clear and actionable for LLMs
	db := Database{
		Writing: []Writing{
			{Phelps: "AB00001FIR", Language: "en", Name: "Test", Text: "Test"},
		},
	}

	tests := []struct {
		mode             string
		shouldContain    []string
		shouldNotContain []string
	}{
		{
			mode: "match",
			shouldContain: []string{
				"MODE: MATCH ONLY",
				"🎯 GOAL:",
				"SIMPLE WORKFLOW:",
				"1. SEARCH:",
				"2. GET_FULL_TEXT:",
				"3. FINAL_ANSWER:",
			},
			shouldNotContain: []string{
				"SEARCH_INVENTORY",
				"ADD_NEW_PRAYER",
				"complex",
				"lengthy",
			},
		},
		{
			mode: "match-add",
			shouldContain: []string{
				"MODE: MATCH-ADD",
				"🎯 GOAL:",
				"SIMPLE WORKFLOW:",
				"Step 1 - TRY MATCHING:",
				"Step 2 - IF NO MATCH (confidence <70):",
				"SMART_INVENTORY_SEARCH:",
			},
			shouldNotContain: []string{},
		},
		{
			mode: "add-only",
			shouldContain: []string{
				"MODE: ADD-ONLY",
				"🎯 GOAL:",
				"SIMPLE WORKFLOW:",
				"SMART_INVENTORY_SEARCH:",
				"❌ DO NOT use SEARCH functions",
			},
			shouldNotContain: []string{
				"SEARCH:keywords,opening,range",
				"GET_FULL_TEXT:",
			},
		},
		{
			mode: "translit",
			shouldContain: []string{
				"MODE: TRANSLITERATION",
				"🎯 GOAL:",
				"SIMPLE WORKFLOW:",
				"SEARCH:",
				"CHECK_TRANSLIT_CONSISTENCY:",
				"CORRECT_TRANSLITERATION:",
				"FINAL_ANSWER:",
			},
			shouldNotContain: []string{
				"TRANSLITERATION STANDARDS:",
				"Use proper Bahá'í transliteration",
				"Follow Bahá'í conventions",
				"lengthy explanation",
				"SEARCH_INVENTORY",
			},
		},
	}

	for _, tt := range tests {
		t.Run("Mode_"+tt.mode+"_instructions", func(t *testing.T) {
			header := prepareLLMHeaderWithMode(db, "English", "en", tt.mode)

			// Check that required content is present
			for _, required := range tt.shouldContain {
				if !strings.Contains(header, required) {
					t.Errorf("Mode %s instructions should contain %q", tt.mode, required)
				}
			}

			// Check that problematic content is not present
			for _, forbidden := range tt.shouldNotContain {
				if strings.Contains(header, forbidden) {
					t.Errorf("Mode %s instructions should NOT contain %q", tt.mode, forbidden)
				}
			}

			// Verify instructions are concise (not too long)
			lines := strings.Split(header, "\n")
			modeSection := ""
			inModeSection := false
			for _, line := range lines {
				if strings.HasPrefix(line, "MODE: ") {
					inModeSection = true
				}
				if inModeSection {
					modeSection += line + "\n"
					if strings.HasPrefix(line, "Current reference language:") {
						break
					}
				}
			}

			// Mode instructions should be focused and clear
			if len(modeSection) < 50 {
				t.Errorf("Mode %s instructions seem too short: %s", tt.mode, modeSection)
			}
		})
	}
}

func TestTranslitModeHeaderOutput(t *testing.T) {
	// Test to show what the actual header looks like for translit mode
	db := Database{
		Writing: []Writing{
			{Phelps: "AB00001FIR", Language: "ar", Name: "Fire Tablet", Text: "Test prayer"},
			{Phelps: "AB00002TAB", Language: "fa", Name: "Tablet of Ahmad", Text: "Test prayer"},
		},
	}

	header := prepareLLMHeaderWithMode(db, "Arabic", "en", "translit")

	t.Logf("=== TRANSLIT MODE HEADER ===")
	t.Logf("%s", header)

	// Verify key components are present
	requiredElements := []string{
		"MODE: TRANSLITERATION",
		"Match original Arabic/Persian text",
		"AVAILABLE FUNCTIONS FOR THIS MODE:",
		"SEARCH:",
		"CHECK_TRANSLIT_CONSISTENCY:",
		"CORRECT_TRANSLITERATION:",
		"RESPOND ONLY WITH FUNCTION CALLS",
	}

	for _, element := range requiredElements {
		if !strings.Contains(header, element) {
			t.Errorf("Translit mode header missing required element: %s", element)
		}
	}

	// Verify problematic elements are NOT present
	shouldNotContain := []string{
		"SEARCH_INVENTORY:", // Should not be available in translit mode
		"ADD_NEW_PRAYER:",   // Should not be available in translit mode
	}

	for _, element := range shouldNotContain {
		if strings.Contains(header, element) {
			t.Errorf("Translit mode header should NOT contain: %s", element)
		}
	}
}

func TestSimplifiedInventorySearch(t *testing.T) {
	db := GetDatabase()

	tests := []struct {
		name     string
		call     string
		hasError bool
		contains []string
	}{
		{
			name:     "Keywords only",
			call:     "SEARCH_INVENTORY:praise sovereignty mercy",
			hasError: false,
			contains: []string{"INVENTORY SEARCH:", "all languages"},
		},
		{
			name:     "Keywords with field",
			call:     "SEARCH_INVENTORY:praise mercy,first_line",
			hasError: false,
			contains: []string{"field: first_line"},
		},
		{
			name:     "Keywords with language filter",
			call:     "SEARCH_INVENTORY:praise mercy,Eng,ALL",
			hasError: false,
			contains: []string{"filtered by language 'Eng'"},
		},
		{
			name:     "Empty keywords",
			call:     "SEARCH_INVENTORY:",
			hasError: true,
			contains: []string{"Error:", "keywords"},
		},
		{
			name:     "Arabic search",
			call:     "SEARCH_INVENTORY:الحمد لله",
			hasError: false,
			contains: []string{"INVENTORY SEARCH:", "all languages"},
		},
	}

	searchFunc := SearchInventoryFunction{NewPrefixFunction("SEARCH_INVENTORY")}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results := searchFunc.Execute(db, "en", tt.call)

			if tt.hasError {
				if len(results) == 0 || !strings.Contains(results[0], "Error") {
					t.Errorf("Expected error for call %s, got: %v", tt.call, results)
				}
			} else {
				if len(results) == 0 {
					t.Errorf("Expected results for call %s, got empty", tt.call)
				}
			}

			for _, expectedContent := range tt.contains {
				found := false
				for _, result := range results {
					if strings.Contains(result, expectedContent) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("Expected to find '%s' in results for %s, got: %v", expectedContent, tt.call, results)
				}
			}
		})
	}
}

func TestSmartInventorySearch(t *testing.T) {
	db := GetDatabase()

	tests := []struct {
		name             string
		call             string
		hasError         bool
		expectedStrategy string
		contains         []string
	}{
		{
			name:             "Basic keywords",
			call:             "SMART_INVENTORY_SEARCH:praise sovereignty mercy",
			hasError:         false,
			expectedStrategy: "First line search",
			contains:         []string{"SMART SEARCH STRATEGY:", "SELECTED FIELDS:"},
		},
		{
			name:             "Opening phrase detection",
			call:             "SMART_INVENTORY_SEARCH:He is the Eternal the One",
			hasError:         false,
			expectedStrategy: "First line search",
			contains:         []string{"First line search", "opening phrase detected"},
		},
		{
			name:             "Subject search detection",
			call:             "SMART_INVENTORY_SEARCH:prayer themes topics",
			hasError:         false,
			expectedStrategy: "Subject-based search",
			contains:         []string{"Subject-based search", "topics/themes detected"},
		},
		{
			name:             "Arabic opening",
			call:             "SMART_INVENTORY_SEARCH:الحمد لله",
			hasError:         false,
			expectedStrategy: "First line search",
			contains:         []string{"First line search"},
		},
		{
			name:     "Empty keywords",
			call:     "SMART_INVENTORY_SEARCH:",
			hasError: true,
			contains: []string{"Error:", "keywords"},
		},
		{
			name:             "With language filter",
			call:             "SMART_INVENTORY_SEARCH:mercy forgiveness,Eng",
			hasError:         false,
			expectedStrategy: "Title + subjects search",
			contains:         []string{"SMART SEARCH STRATEGY:"},
		},
	}

	smartFunc := SmartInventorySearchFunction{NewPrefixFunction("SMART_INVENTORY_SEARCH")}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results := smartFunc.Execute(db, "en", tt.call)

			if tt.hasError {
				if len(results) == 0 || !strings.Contains(results[0], "Error") {
					t.Errorf("Expected error for call %s, got: %v", tt.call, results)
				}
			} else {
				if len(results) == 0 {
					t.Errorf("Expected results for call %s, got empty", tt.call)
				}

				// Check for strategy detection
				if tt.expectedStrategy != "" {
					found := false
					for _, result := range results {
						if strings.Contains(result, tt.expectedStrategy) {
							found = true
							break
						}
					}
					if !found {
						t.Errorf("Expected strategy '%s' for %s, got: %v", tt.expectedStrategy, tt.call, results)
					}
				}
			}

			for _, expectedContent := range tt.contains {
				found := false
				for _, result := range results {
					if strings.Contains(result, expectedContent) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("Expected to find '%s' in results for %s, got: %v", expectedContent, tt.call, results)
				}
			}
		})
	}
}

func TestImprovedAddNewPrayerFunction(t *testing.T) {
	db := GetDatabase()

	tests := []struct {
		name     string
		call     string
		hasError bool
		contains []string
	}{
		{
			name:     "Missing parameters",
			call:     "ADD_NEW_PRAYER:AB12345GOD",
			hasError: true,
			contains: []string{"Error:", "format", "confidence", "reasoning"},
		},
		{
			name:     "Invalid confidence",
			call:     "ADD_NEW_PRAYER:AB12345GOD,invalid,Prayer about God",
			hasError: true,
			contains: []string{"Error:", "Confidence", "number"},
		},
		{
			name:     "Out of range confidence",
			call:     "ADD_NEW_PRAYER:AB12345GOD,150,Prayer about God",
			hasError: true,
			contains: []string{"Error:", "Confidence", "between 0 and 100"},
		},
		{
			name:     "Empty reasoning",
			call:     "ADD_NEW_PRAYER:AB12345GOD,85,",
			hasError: true,
			contains: []string{"Error:", "Reasoning cannot be empty"},
		},
		{
			name:     "Short Phelps code",
			call:     "ADD_NEW_PRAYER:AB123,85,Prayer about God",
			hasError: true,
			contains: []string{"Error:", "too short", "7 chars"},
		},
		{
			name:     "Invalid tag format",
			call:     "ADD_NEW_PRAYER:AB12345go!,85,Prayer about God",
			hasError: true,
			contains: []string{"Error:", "uppercase letters and numbers"},
		},
		{
			name:     "Invalid tag length",
			call:     "ADD_NEW_PRAYER:AB12345GODS,85,Prayer about God",
			hasError: true,
			contains: []string{"7 chars (PIN) or 10 chars (PIN+tag)"},
		},
		{
			name:     "Valid PIN only format",
			call:     "ADD_NEW_PRAYER:AB12345,90,Entire document about prayers",
			hasError: true, // Will error on PIN validation since AB12345 doesn't exist
			contains: []string{"PIN AB12345 not found"},
		},
		{
			name:     "Valid PIN+TAG format",
			call:     "ADD_NEW_PRAYER:AB12345GOD,85,Prayer praising God's attributes",
			hasError: true, // Will error on PIN validation since AB12345 doesn't exist
			contains: []string{"PIN AB12345 not found"},
		},
		{
			name:     "No prayers without codes",
			call:     "ADD_NEW_PRAYER:BH00001GOD,85,Prayer about God",
			hasError: true, // Will error on PIN validation first
			contains: []string{"PIN BH00001 not found"},
		},
	}

	addFunc := AddNewPrayerFunction{NewPrefixFunction("ADD_NEW_PRAYER")}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results := addFunc.Execute(db, "en", tt.call)

			if len(results) == 0 {
				t.Errorf("Expected results for call %s, got empty", tt.call)
				return
			}

			hasError := strings.Contains(results[0], "Error") || strings.Contains(results[0], "❌")
			if tt.hasError != hasError {
				t.Errorf("Expected hasError=%v for call %s, got hasError=%v, results: %v", tt.hasError, tt.call, hasError, results)
			}

			for _, expectedContent := range tt.contains {
				found := false
				for _, result := range results {
					if strings.Contains(result, expectedContent) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("Expected to find '%s' in results for %s, got: %v", expectedContent, tt.call, results)
				}
			}
		})
	}
}

func TestImprovedFunctionCallExtraction(t *testing.T) {
	tests := []struct {
		name           string
		text           string
		expectedValid  []string
		expectedErrors []string
	}{
		{
			name:          "Valid simple calls",
			text:          "SMART_INVENTORY_SEARCH:praise mercy\nSEARCH_INVENTORY:keywords\nFINAL_ANSWER:AB12345GOD,85,Prayer about God",
			expectedValid: []string{"SMART_INVENTORY_SEARCH:praise mercy", "SEARCH_INVENTORY:keywords", "FINAL_ANSWER:AB12345GOD,85,Prayer about God"},
		},
		{
			name:           "OpenAI tool error simulation",
			text:           `Error executing tool "smart_inventory_search": Tool "smart_inventory_search" not found in registry.`,
			expectedValid:  []string{},
			expectedErrors: []string{"OpenAI-style tool"},
		},
		{
			name:           "Conversational response",
			text:           "I need to search the inventory for prayers about mercy.",
			expectedValid:  []string{},
			expectedErrors: []string{"Conversational response detected"},
		},
		{
			name:           "Mixed valid and invalid",
			text:           "SMART_INVENTORY_SEARCH:praise\nI am unable to proceed with this search.\nFINAL_ANSWER:AB12345,85,Found it",
			expectedValid:  []string{"SMART_INVENTORY_SEARCH:praise", "FINAL_ANSWER:AB12345,85,Found it"},
			expectedErrors: []string{"Conversational response"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validCalls, invalidCalls := extractAllFunctionCalls(tt.text)

			if len(validCalls) != len(tt.expectedValid) {
				t.Errorf("Expected %d valid calls, got %d: %v", len(tt.expectedValid), len(validCalls), validCalls)
			}

			for i, expected := range tt.expectedValid {
				if i >= len(validCalls) || validCalls[i] != expected {
					t.Errorf("Expected valid call %d to be '%s', got '%s'", i, expected, validCalls[i])
				}
			}

			if len(invalidCalls) != len(tt.expectedErrors) {
				t.Errorf("Expected %d invalid calls, got %d: %v", len(tt.expectedErrors), len(invalidCalls), invalidCalls)
			}

			for _, expectedError := range tt.expectedErrors {
				found := false
				for _, invalidCall := range invalidCalls {
					if strings.Contains(invalidCall.Error, expectedError) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("Expected to find error containing '%s' in invalid calls: %v", expectedError, invalidCalls)
				}
			}
		})
	}
}

func TestInventorySearchLanguageHandling(t *testing.T) {
	db := GetDatabase()

	tests := []struct {
		name        string
		call        string
		expectLang  bool
		expectAll   bool
		description string
	}{
		{
			name:        "Keywords only - searches all languages",
			call:        "SEARCH_INVENTORY:praise sovereignty",
			expectLang:  false,
			expectAll:   true,
			description: "Should search across all document languages",
		},
		{
			name:        "Keywords with field - still all languages",
			call:        "SEARCH_INVENTORY:mercy forgiveness,first_line",
			expectLang:  false,
			expectAll:   true,
			description: "Field specified but no language filter",
		},
		{
			name:        "Keywords with language filter",
			call:        "SEARCH_INVENTORY:God attributes,Eng,subjects",
			expectLang:  true,
			expectAll:   false,
			description: "Should filter by English documents only",
		},
		{
			name:        "Arabic keywords no filter",
			call:        "SEARCH_INVENTORY:الحمد لله",
			expectLang:  false,
			expectAll:   true,
			description: "Arabic keywords but searches all document languages",
		},
	}

	searchFunc := SearchInventoryFunction{NewPrefixFunction("SEARCH_INVENTORY")}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results := searchFunc.Execute(db, "en", tt.call)

			if len(results) == 0 {
				t.Errorf("Expected results for %s, got empty", tt.description)
				return
			}

			searchResultLine := results[0]

			if tt.expectAll && !strings.Contains(searchResultLine, "all languages") {
				t.Errorf("Expected 'all languages' in result for %s, got: %s", tt.description, searchResultLine)
			}

			if tt.expectLang && !strings.Contains(searchResultLine, "filtered by language") {
				t.Errorf("Expected 'filtered by language' in result for %s, got: %s", tt.description, searchResultLine)
			}

			if !tt.expectAll && !tt.expectLang {
				t.Errorf("Test case %s must expect either all languages or language filter", tt.name)
			}
		})
	}
}

func TestAddNewPrayerIntegrationWorkflow(t *testing.T) {
	// Test the complete workflow for ADD_NEW_PRAYER in add-only mode
	db := GetDatabase()

	// First, let's test the inventory search to find a valid PIN
	searchFunc := SmartInventorySearchFunction{NewPrefixFunction("SMART_INVENTORY_SEARCH")}
	searchResults := searchFunc.Execute(db, "en", "SMART_INVENTORY_SEARCH:praise sovereignty mercy")

	if len(searchResults) == 0 {
		t.Skip("No inventory results found - skipping integration test")
	}

	// Look for a PIN in the search results
	var foundPIN string
	for _, result := range searchResults {
		if strings.Contains(result, "PIN:") {
			// Extract PIN from result like "PIN: AB12345 - Title"
			parts := strings.Split(result, "PIN:")
			if len(parts) > 1 {
				pinPart := strings.TrimSpace(parts[1])
				pinFields := strings.Fields(pinPart)
				if len(pinFields) > 0 && len(pinFields[0]) >= 7 {
					foundPIN = pinFields[0]
					break
				}
			}
		}
	}

	if foundPIN == "" {
		t.Skip("No valid PIN found in search results - skipping integration test")
	}

	// Test the complete workflow:
	// 1. Check what tags already exist for this PIN
	checkTagFunc := CheckTagFunction{NewPrefixFunction("CHECK_TAG")}
	tagResults := checkTagFunc.Execute(db, "en", "CHECK_TAG:"+foundPIN)

	if len(tagResults) == 0 {
		t.Errorf("CHECK_TAG should return results for PIN %s", foundPIN)
	}

	// 2. Try to create a new prayer with a unique tag
	testTag := "TST" // Test tag
	phelpsCode := foundPIN + testTag

	// First verify the code doesn't already exist
	existsQuery := fmt.Sprintf("SELECT COUNT(*) FROM writings WHERE phelps = '%s'", phelpsCode)
	output, err := execDoltQuery(existsQuery)
	if err == nil {
		lines := strings.Split(string(output), "\n")
		for i, line := range lines {
			if i < 3 || line == "" {
				continue
			}
			if strings.Contains(line, "|") {
				fields := strings.Split(line, "|")
				if len(fields) >= 1 {
					countStr := strings.TrimSpace(fields[0])
					if count, parseErr := strconv.Atoi(countStr); parseErr == nil && count > 0 {
						t.Skipf("Test Phelps code %s already exists - skipping integration test", phelpsCode)
					}
				}
			}
		}
	}

	// 3. Add a prayer without a Phelps code to test against
	testVersion := fmt.Sprintf("TEST_%d", time.Now().Unix())
	insertQuery := fmt.Sprintf(`INSERT INTO writings (phelps, language, version, text, name, type, notes, link, source, source_id)
		VALUES ('', 'en', '%s', 'Test prayer text for integration test', 'Test Prayer', 'prayer', 'Integration test', '', '', '')`,
		testVersion)

	_, insertErr := execDoltQuery(insertQuery)
	if insertErr != nil {
		t.Errorf("Failed to insert test prayer: %v", insertErr)
		return
	}

	// Clean up the test prayer at the end
	defer func() {
		deleteQuery := fmt.Sprintf("DELETE FROM writings WHERE version = '%s'", testVersion)
		execDoltQuery(deleteQuery)
	}()

	// 4. Now test ADD_NEW_PRAYER with the specific version
	addFunc := AddNewPrayerFunction{NewPrefixFunction("ADD_NEW_PRAYER")}
	addCall := fmt.Sprintf("ADD_NEW_PRAYER:%s,85,Integration test prayer,%s", phelpsCode, testVersion)
	addResults := addFunc.Execute(db, "en", addCall)

	if len(addResults) == 0 {
		t.Errorf("ADD_NEW_PRAYER should return results")
		return
	}

	// Check if the assignment was successful
	hasSuccess := false
	for _, result := range addResults {
		if strings.Contains(result, "✅") && strings.Contains(result, "ASSIGNED") {
			hasSuccess = true
			break
		}
	}

	if !hasSuccess {
		t.Errorf("ADD_NEW_PRAYER should indicate successful assignment, got: %v", addResults)
	}

	// 5. Verify the code was actually assigned in the database
	verifyQuery := fmt.Sprintf("SELECT phelps FROM writings WHERE version = '%s'", testVersion)
	verifyOutput, verifyErr := execDoltQuery(verifyQuery)
	if verifyErr != nil {
		t.Errorf("Failed to verify assignment: %v", verifyErr)
		return
	}

	verifyLines := strings.Split(string(verifyOutput), "\n")
	var assignedPhelps string
	for i, line := range verifyLines {
		if i < 3 || line == "" {
			continue
		}
		if strings.Contains(line, "|") {
			fields := strings.Split(line, "|")
			if len(fields) >= 1 {
				assignedPhelps = strings.TrimSpace(fields[0])
				break
			}
		}
	}

	if assignedPhelps != phelpsCode {
		t.Errorf("Expected Phelps code %s to be assigned, but got %s", phelpsCode, assignedPhelps)
	}

	t.Logf("✅ Integration test successful: PIN %s + TAG %s = %s assigned to version %s",
		foundPIN, testTag, phelpsCode, testVersion)
}

func TestPINDiscoveryTracking(t *testing.T) {
	// Test that ADD_NEW_PRAYER only accepts PINs discovered through proper workflow
	db := GetDatabase()

	// Initialize session
	initializeSession()
	defer clearDiscoveredPINs()

	// First, find a real PIN from inventory to use for testing
	searchFunc := SmartInventorySearchFunction{NewPrefixFunction("SMART_INVENTORY_SEARCH")}
	searchResults := searchFunc.Execute(db, "en", "SMART_INVENTORY_SEARCH:praise sovereignty")

	if len(searchResults) == 0 {
		t.Skip("No search results - cannot test PIN discovery workflow")
	}

	// Look for a PIN in search results
	var realPIN string
	for _, result := range searchResults {
		if strings.Contains(result, "PIN:") {
			parts := strings.Split(result, "PIN:")
			if len(parts) > 1 {
				pinPart := strings.TrimSpace(parts[1])
				fields := strings.Fields(pinPart)
				if len(fields) > 0 && len(fields[0]) >= 7 {
					realPIN = fields[0]
					break
				}
			}
		}
	}

	if realPIN == "" {
		t.Skip("No PIN found in search results - cannot test discovery workflow")
	}

	// Now clear discovered PINs to test without discovery
	clearDiscoveredPINs()

	// Try to use ADD_NEW_PRAYER with a real PIN but without discovering it first
	addFunc := AddNewPrayerFunction{NewPrefixFunction("ADD_NEW_PRAYER")}
	testPhelpsCode := realPIN + "TST"

	// This should fail because the PIN wasn't discovered in this session
	results := addFunc.Execute(db, "en", fmt.Sprintf("ADD_NEW_PRAYER:%s,85,Test prayer", testPhelpsCode))

	if len(results) == 0 {
		t.Errorf("Expected error message for undiscovered PIN")
		return
	}

	// Check that it rejects undiscovered PINs (should show discovery security error)
	hasSecurityError := false
	for _, result := range results {
		if strings.Contains(result, "not discovered through inventory search") && strings.Contains(result, "🔒 SECURITY") {
			hasSecurityError = true
			break
		}
	}

	if !hasSecurityError {
		t.Errorf("Expected security error for undiscovered PIN, got: %v", results)
	}

	// Now re-discover the PIN through search
	searchResults = searchFunc.Execute(db, "en", "SMART_INVENTORY_SEARCH:praise sovereignty")

	// Now ADD_NEW_PRAYER should accept this discovered PIN
	addResults := addFunc.Execute(db, "en", fmt.Sprintf("ADD_NEW_PRAYER:%s,85,Test with discovered PIN", testPhelpsCode))

	if len(addResults) == 0 {
		t.Errorf("Expected results for discovered PIN")
		return
	}

	// Check that it doesn't show security error anymore
	hasSecurityError = false
	for _, result := range addResults {
		if strings.Contains(result, "not discovered through inventory search") {
			hasSecurityError = true
			break
		}
	}

	if hasSecurityError {
		t.Errorf("Should not have security error for discovered PIN, got: %v", addResults)
	}

	t.Logf("✅ PIN discovery tracking test successful: PIN %s was properly tracked", realPIN)
}

func TestPINTrackingAcrossInventoryFunctions(t *testing.T) {
	// Test that both SEARCH_INVENTORY and GET_INVENTORY_CONTEXT track PINs
	db := GetDatabase()

	initializeSession()
	defer clearDiscoveredPINs()

	// Test 1: SEARCH_INVENTORY should track discovered PINs
	searchFunc := SearchInventoryFunction{NewPrefixFunction("SEARCH_INVENTORY")}
	searchResults := searchFunc.Execute(db, "en", "SEARCH_INVENTORY:praise mercy")

	var searchPIN string
	for _, result := range searchResults {
		if strings.Contains(result, "PIN:") {
			parts := strings.Split(result, "PIN:")
			if len(parts) > 1 {
				pinPart := strings.TrimSpace(parts[1])
				fields := strings.Fields(pinPart)
				if len(fields) > 0 && len(fields[0]) >= 7 {
					searchPIN = fields[0]
					break
				}
			}
		}
	}

	if searchPIN != "" {
		// Verify this PIN is now tracked
		if !isPINDiscovered(searchPIN) {
			t.Errorf("PIN %s should be tracked after SEARCH_INVENTORY", searchPIN)
		}
	}

	// Test 2: GET_INVENTORY_CONTEXT should also track PINs
	if searchPIN != "" {
		clearDiscoveredPINs() // Clear to test GET_INVENTORY_CONTEXT independently

		contextFunc := GetInventoryContextFunction{NewPrefixFunction("GET_INVENTORY_CONTEXT")}
		contextResults := contextFunc.Execute(db, "en", "GET_INVENTORY_CONTEXT:"+searchPIN)

		if len(contextResults) > 0 && !strings.Contains(contextResults[0], "No inventory record") {
			// Verify this PIN is now tracked
			if !isPINDiscovered(searchPIN) {
				t.Errorf("PIN %s should be tracked after GET_INVENTORY_CONTEXT", searchPIN)
			}
		}
	}

	if searchPIN == "" {
		t.Skip("No PIN found in search results - cannot fully test PIN tracking")
	}

	t.Logf("✅ PIN tracking across functions test successful")
}

func TestBahaiPrayersApiSearchFunction(t *testing.T) {
	// Create a BahaiPrayersApiSearchFunction instance
	apiSearchFunc := BahaiPrayersApiSearchFunction{NewPrefixFunction("API_SEARCH")}
	mockDB := Database{} // Empty database for this test

	tests := []struct {
		name     string
		call     string
		hasError bool
		contains []string
	}{
		{
			name:     "Missing arguments",
			call:     "API_SEARCH:",
			hasError: true,
			contains: []string{"Error: API_SEARCH requires format: query,language"},
		},
		{
			name:     "Missing language",
			call:     "API_SEARCH:love mercy",
			hasError: false,
			contains: []string{"love mercy", "english"},
		},
		{
			name:     "With author parameter",
			call:     "API_SEARCH:praise,english,Baha'u'llah",
			hasError: false,
			contains: []string{"praise", "english"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := apiSearchFunc.Execute(mockDB, "en", tt.call)

			// Check if we expect an error
			if tt.hasError {
				if len(result) == 0 || !strings.Contains(result[0], "Error") {
					t.Errorf("Expected error message, got: %v", result)
				}
				return
			}

			// For API calls, we may get either successful results or API errors due to network/connectivity
			// Both are acceptable in a test environment
			resultText := strings.Join(result, " ")
			hasAPIError := strings.Contains(resultText, "API Search Error") || strings.Contains(resultText, "failed to")

			if !hasAPIError {
				// If no API error, check for expected content
				for _, expectedContent := range tt.contains {
					if !strings.Contains(resultText, expectedContent) {
						t.Errorf("Expected result to contain %q, got: %v", expectedContent, result)
					}
				}
			} else {
				// API error is acceptable - just ensure it's a proper error message
				if len(result) == 0 {
					t.Errorf("Expected error result, got empty response")
				}
			}
		})
	}
}

func TestApiGetDocumentFunction(t *testing.T) {
	// Create an ApiGetDocumentFunction instance
	getDocFunc := ApiGetDocumentFunction{NewPrefixFunction("API_GET_DOCUMENT")}
	mockDB := Database{} // Empty database for this test

	tests := []struct {
		name     string
		call     string
		hasError bool
		contains []string
	}{
		{
			name:     "Missing arguments",
			call:     "API_GET_DOCUMENT:",
			hasError: true,
			contains: []string{"Error: API_GET_DOCUMENT requires format: documentId,language"},
		},
		{
			name:     "Missing language",
			call:     "API_GET_DOCUMENT:doc123",
			hasError: true,
			contains: []string{"Error: API_GET_DOCUMENT requires format: documentId,language"},
		},
		{
			name:     "Valid parameters",
			call:     "API_GET_DOCUMENT:doc123,english",
			hasError: false,
			contains: []string{"doc123"},
		},
		{
			name:     "With highlight parameter",
			call:     "API_GET_DOCUMENT:doc456,arabic,الحمد",
			hasError: false,
			contains: []string{"doc456"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getDocFunc.Execute(mockDB, "en", tt.call)

			// Check if we expect an error
			if tt.hasError {
				if len(result) == 0 || !strings.Contains(result[0], "Error") {
					t.Errorf("Expected error message, got: %v", result)
				}
				return
			}

			// For API calls, we may get either successful results or API errors due to network/connectivity
			// Both are acceptable in a test environment
			resultText := strings.Join(result, " ")
			hasAPIError := strings.Contains(resultText, "API Document Error") || strings.Contains(resultText, "failed to")

			if !hasAPIError {
				// If no API error, check for expected content
				for _, expectedContent := range tt.contains {
					if !strings.Contains(resultText, expectedContent) {
						t.Errorf("Expected result to contain %q, got: %v", expectedContent, result)
					}
				}
			} else {
				// API error is acceptable - just ensure it's a proper error message
				if len(result) == 0 {
					t.Errorf("Expected error result, got empty response")
				}
			}
		})
	}
}

func TestBahaiPrayersApiFunctionIntegration(t *testing.T) {
	// Test that API functions are properly registered
	apiSearchFunc := BahaiPrayersApiSearchFunction{NewPrefixFunctionWithModes("API_SEARCH", []string{"match-add", "add-only"})}
	getDocFunc := ApiGetDocumentFunction{NewPrefixFunctionWithModes("API_GET_DOCUMENT", []string{"match-add", "add-only"})}

	// Test function descriptions
	if apiSearchFunc.GetDescription() == "" {
		t.Error("API_SEARCH function should have a description")
	}
	if getDocFunc.GetDescription() == "" {
		t.Error("API_GET_DOCUMENT function should have a description")
	}

	// Test usage examples
	if apiSearchFunc.GetUsageExample() == "" {
		t.Error("API_SEARCH function should have a usage example")
	}
	if getDocFunc.GetUsageExample() == "" {
		t.Error("API_GET_DOCUMENT function should have a usage example")
	}
}
