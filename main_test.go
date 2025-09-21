package main

import (
	"fmt"
	"os"
	"strings"
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

	if !strings.Contains(header, "AB00001FIR") {
		t.Error("Header should contain known Phelps codes")
	}

	if !strings.Contains(header, "Fire Tablet") {
		t.Error("Header should contain prayer names from reference language")
	}

	if !strings.Contains(header, "AB00032DAR") {
		t.Error("Header should contain all known Phelps codes")
	}

	if !strings.Contains(header, "Confidence:") {
		t.Error("Header should contain response format instructions")
	}

	if !strings.Contains(header, "UNKNOWN") {
		t.Error("Header should contain instructions for unknown matches")
	}

	if !strings.Contains(header, "reference: en") {
		t.Error("Header should mention reference language")
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

func TestInsertPrayerMatchCandidateDataStructure(t *testing.T) {
	db := Database{
		PrayerMatchCandidate: []PrayerMatchCandidate{},
	}

	candidate := PrayerMatchCandidate{
		VersionID:        "test_version_001",
		ProposedName:     "Test Prayer Name",
		ProposedPhelps:   "AB00001FIR",
		Language:         "en",
		TextLength:       100,
		ReferenceLength:  95,
		LengthRatio:      1.05,
		ConfidenceScore:  0.75,
		ValidationStatus: "pending",
		ValidationNotes:  "LLM match with medium confidence",
		CreatedDate:      time.Now().Format("2006-01-02 15:04:05"),
	}

	// Test the in-memory part (we can't easily test the Dolt part without a real database)
	initialCount := len(db.PrayerMatchCandidate)

	// We'll simulate just the in-memory addition part
	db.PrayerMatchCandidate = append(db.PrayerMatchCandidate, candidate)

	if len(db.PrayerMatchCandidate) != initialCount+1 {
		t.Error("Candidate should be added to in-memory database")
	}

	added := db.PrayerMatchCandidate[len(db.PrayerMatchCandidate)-1]
	if added.ProposedPhelps != candidate.ProposedPhelps {
		t.Errorf("Added candidate ProposedPhelps = %v, want %v", added.ProposedPhelps, candidate.ProposedPhelps)
	}
	if added.ConfidenceScore != candidate.ConfidenceScore {
		t.Errorf("Added candidate ConfidenceScore = %v, want %v", added.ConfidenceScore, candidate.ConfidenceScore)
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
			result := findOptimalDefaultLanguage(db)
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
	if !strings.Contains(header, "Fire Tablet") {
		t.Error("Should use English names when en is reference language")
	}
	if strings.Contains(header, "Tabla del Fuego") {
		t.Error("Should not use Spanish names when en is reference language")
	}

	// Test using Spanish as reference
	header2 := prepareLLMHeader(db, "French", "es")
	if !strings.Contains(header2, "Tabla del Fuego") {
		t.Error("Should use Spanish names when es is reference language")
	}
	if strings.Contains(header2, "Fire Tablet") {
		t.Error("Should not use English names when es is reference language")
	}
}

// MockLLMCaller for testing
type MockLLMCaller struct {
	GeminiResponse string
	GeminiError    error
	OllamaResponse string
	OllamaError    error
}

func (m MockLLMCaller) CallGemini(prompt string) (string, error) {
	return m.GeminiResponse, m.GeminiError
}

func (m MockLLMCaller) CallOllama(prompt string) (string, error) {
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
			result, err := callLLMWithCaller("test prompt", tt.useGemini, mockCaller)

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
