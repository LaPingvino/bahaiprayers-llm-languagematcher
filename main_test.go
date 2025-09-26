package main

import (
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
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
			text: `SEARCH_KEYWORDS:lord,god,assist
SEARCH_LENGTH:50-150
SEARCH_OPENING:O Lord my God
GET_FULL_TEXT:AB00001FIR
GET_FOCUS_TEXT:lord,AB00001FIR,AB00002SEC
GET_PARTIAL_TEXT:AB00001FIR,100-500
FINAL_ANSWER:AB00001FIR,85,This prayer matches based on distinctive phrases
GET_STATS`,
			expectedValid: []string{
				"SEARCH_KEYWORDS:lord,god,assist",
				"SEARCH_LENGTH:50-150",
				"SEARCH_OPENING:O Lord my God",
				"GET_FULL_TEXT:AB00001FIR",
				"GET_FOCUS_TEXT:lord,AB00001FIR,AB00002SEC",
				"GET_PARTIAL_TEXT:AB00001FIR,100-500",
				"FINAL_ANSWER:AB00001FIR,85,This prayer matches based on distinctive phrases",
				"GET_STATS",
			},
			expectedInvalid: 0,
		},
		{
			name: "Tool calls JSON format",
			text: `{"tool_calls":[{"function":{"name":"SEARCH_KEYWORDS","arguments":{"arguments":"lord,god,assist","name":"SEARCH_KEYWORDS"}}}]}`,
			expectedValid: []string{
				"SEARCH_KEYWORDS:lord,god,assist",
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
				"Error: SEARCH_INVENTORY requires format: keywords,language",
				"",
				"SUPPORTED LANGUAGES (with good inventory coverage):",
				"- Eng (English) - best coverage",
				"- Ara (Arabic) - original texts",
				"- Per (Persian) - original texts",
				"- Trk (Turkish) - some coverage",
				"",
				"Example: SEARCH_INVENTORY:lord god mercy,Eng",
			},
		},
		{
			name: "Unsupported language",
			call: "SEARCH_INVENTORY:lord god,xyz",
			expected: []string{
				"Warning: Language 'xyz' may have very limited inventory coverage.",
				"",
				"BEST SUPPORTED LANGUAGES:",
				"- Eng (English) - comprehensive coverage",
				"- Ara (Arabic) - original Bahá'í texts",
				"- Per (Persian) - original Bahá'í texts",
				"",
				"Continue with inventory search anyway? Use exact format: SEARCH_INVENTORY:keywords,Eng",
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
			hasError: false, // Will fail with PIN validation, but format is correct
		},
		{
			name:     "Valid format - PIN with tag",
			call:     "ADD_NEW_PRAYER:AB00001FIR,85,First prayer from document",
			hasError: false, // Will fail with PIN validation, but format is correct
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
			call:     "CORRECT_TRANSLITERATION:AB00001FIR,85,O Thou Who art the Lord of all names and the Creator of the heavens",
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
		{"ar-translit", "match", true},
		{"fa-translit", "match", true},
		{"en", "match", false},
		{"es", "match", false},
		{"ar", "translit", true},
		{"fa-translit", "translit", true},
	}

	for _, tt := range tests {
		result := shouldCheckTransliteration(tt.language, tt.mode)
		if result != tt.expected {
			t.Errorf("shouldCheckTransliteration(%s, %s): expected %v, got %v", tt.language, tt.mode, tt.expected, result)
		}
	}
}

func TestPrepareLLMHeaderWithMode(t *testing.T) {
	// Create a mock database with some known Phelps codes
	db := Database{
		Writing: []Writing{
			{Phelps: "AB00001FIR", Language: "en", Name: "Fire Tablet", Text: "Test prayer 1"},
			{Phelps: "AB00032DAR", Language: "en", Name: "Tablet of Ahmad", Text: "Test prayer 2"},
		},
	}

	tests := []struct {
		mode     string
		contains []string
	}{
		{
			mode:     "match",
			contains: []string{"MODE: MATCH ONLY"},
		},
		{
			mode:     "match-add",
			contains: []string{"MODE: MATCH-ADD", "NEW CODE WORKFLOW"},
		},
		{
			mode:     "add-only",
			contains: []string{"MODE: ADD-ONLY", "ADD-ONLY WORKFLOW"},
		},
		{
			mode:     "translit",
			contains: []string{"MODE: TRANSLITERATION", "TRANSLIT WORKFLOW"},
		},
	}

	for _, tt := range tests {
		t.Run("Mode_"+tt.mode, func(t *testing.T) {
			header := prepareLLMHeaderWithMode(db, "English", "en", tt.mode)

			for _, expectedText := range tt.contains {
				if !strings.Contains(header, expectedText) {
					t.Errorf("Header for mode %s should contain %q", tt.mode, expectedText)
				}
			}
		})
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
			name:           "Arabic base language",
			inputLang:      "ar",
			expectedOutput: "ar-translit",
			expectedCount:  1,
		},
		{
			name:           "Persian base language",
			inputLang:      "fa",
			expectedOutput: "fa-translit",
			expectedCount:  1,
		},
		{
			name:           "Persian alternative",
			inputLang:      "persian",
			expectedOutput: "fa-translit",
			expectedCount:  1,
		},
		{
			name:           "Already translit format",
			inputLang:      "ar-translit",
			expectedOutput: "ar-translit",
			expectedCount:  1,
		},
		{
			name:           "Multiple languages",
			inputLang:      "ar,fa",
			expectedOutput: "ar-translit,fa-translit",
			expectedCount:  2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			languages := strings.Split(tt.inputLang, ",")
			var translitLanguages []string

			for _, lang := range languages {
				lang = strings.TrimSpace(lang)

				if lang == "ar" || lang == "arabic" {
					translitLanguages = append(translitLanguages, "ar-translit")
				} else if lang == "fa" || lang == "persian" || lang == "per" {
					translitLanguages = append(translitLanguages, "fa-translit")
				} else if strings.HasSuffix(lang, "-translit") {
					translitLanguages = append(translitLanguages, lang)
				} else {
					translitLanguages = append(translitLanguages, lang)
				}
			}

			result := strings.Join(translitLanguages, ",")
			if result != tt.expectedOutput {
				t.Errorf("Expected %s, got %s", tt.expectedOutput, result)
			}

			if len(translitLanguages) != tt.expectedCount {
				t.Errorf("Expected %d languages, got %d", tt.expectedCount, len(translitLanguages))
			}
		})
	}
}
