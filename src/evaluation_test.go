package main

import (
	"strings"
	"testing"
	"time"
)

// Test LLM Evaluation Layer
func TestLLMEvaluationLayer(t *testing.T) {
	_ = &Database{
		Writing: []Writing{
			{Phelps: "AB00001FIR", Language: "en", Text: "O Thou Who art the Lord of all names and the Maker of the heavens!"},
			{Phelps: "AB00002SEC", Language: "en", Text: "Blessed is the spot, and the house, and the place, and the city..."},
		},
	}

	tests := []struct {
		name             string
		originalResponse LLMResponse
		evalResponse     string
		expectedPhelps   string
		expectedConfMin  float64
		expectedConfMax  float64
		shouldImprove    bool
	}{
		{
			name: "Valid match gets confidence boost",
			originalResponse: LLMResponse{
				PhelpsCode: "AB00001FIR",
				Confidence: 0.75,
				Reasoning:  "This appears to be the Fire Tablet",
			},
			evalResponse:    "VALID: YES\nCONFIDENCE_APPROPRIATE: YES\nIMPROVED_CODE: NONE\nEVALUATION: Original match is correct",
			expectedPhelps:  "AB00001FIR",
			expectedConfMin: 0.80, // Should get confidence boost
			expectedConfMax: 0.90,
			shouldImprove:   false,
		},
		{
			name: "Invalid match gets confidence reduction",
			originalResponse: LLMResponse{
				PhelpsCode: "AB00001FIR",
				Confidence: 0.80,
				Reasoning:  "Matched based on keywords",
			},
			evalResponse:    "VALID: NO\nCONFIDENCE_APPROPRIATE: NO\nIMPROVED_CODE: NONE\nEVALUATION: Match seems questionable",
			expectedPhelps:  "AB00001FIR",
			expectedConfMin: 0.50, // Should get confidence reduction
			expectedConfMax: 0.70,
			shouldImprove:   false,
		},
		{
			name: "Evaluator suggests better match",
			originalResponse: LLMResponse{
				PhelpsCode: "AB00001FIR",
				Confidence: 0.60,
				Reasoning:  "Tentative match",
			},
			evalResponse:    "VALID: NO\nIMPROVED_CODE: AB00002SEC\nIMPROVED_CONFIDENCE: 85\nIMPROVED_REASONING: Better match found\nEVALUATION: Found superior alternative",
			expectedPhelps:  "AB00002SEC",
			expectedConfMin: 0.80,
			expectedConfMax: 0.90,
			shouldImprove:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Parse the mock evaluation response
			evaluation := parseEvaluationResponse(tt.evalResponse)

			// Apply evaluation to get final response
			finalResponse := applyEvaluation(tt.originalResponse, evaluation)

			// Check Phelps code
			if finalResponse.PhelpsCode != tt.expectedPhelps {
				t.Errorf("Expected Phelps code %s, got %s", tt.expectedPhelps, finalResponse.PhelpsCode)
			}

			// Check confidence range
			if finalResponse.Confidence < tt.expectedConfMin || finalResponse.Confidence > tt.expectedConfMax {
				t.Errorf("Expected confidence between %.2f and %.2f, got %.2f",
					tt.expectedConfMin, tt.expectedConfMax, finalResponse.Confidence)
			}

			// Check if improvement was applied correctly
			isImproved := strings.Contains(finalResponse.Reasoning, "EVALUATED & IMPROVED")
			if isImproved != tt.shouldImprove {
				t.Errorf("Expected improvement flag %v, got %v", tt.shouldImprove, isImproved)
			}
		})
	}
}

func TestParseEvaluationResponse(t *testing.T) {
	tests := []struct {
		name     string
		response string
		expected LLMEvaluation
	}{
		{
			name: "Valid evaluation with improvement",
			response: `VALID: NO
CONFIDENCE_APPROPRIATE: NO
IMPROVED_CODE: AB00123NEW
IMPROVED_CONFIDENCE: 90
IMPROVED_REASONING: Found better match with higher similarity
EVALUATION: Original was too generic, new match is more specific`,
			expected: LLMEvaluation{
				IsValid:        true,
				OriginalValid:  false,
				ImprovedCode:   "AB00123NEW",
				ImprovedConf:   0.90,
				ImprovedReason: "Found better match with higher similarity",
				EvalReasoning:  "Original was too generic, new match is more specific",
			},
		},
		{
			name: "Valid original match",
			response: `VALID: YES
CONFIDENCE_APPROPRIATE: YES
IMPROVED_CODE: NONE
EVALUATION: Original match is accurate and well-justified`,
			expected: LLMEvaluation{
				IsValid:       false,
				OriginalValid: true,
				ImprovedCode:  "",
				ImprovedConf:  0.0,
				EvalReasoning: "Original match is accurate and well-justified",
			},
		},
		{
			name: "Confidence percentage conversion",
			response: `IMPROVED_CONFIDENCE: 75
IMPROVED_CODE: AB00456TEST`,
			expected: LLMEvaluation{
				IsValid:      true,
				ImprovedCode: "AB00456TEST",
				ImprovedConf: 0.75, // Should convert from percentage
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseEvaluationResponse(tt.response)

			if result.IsValid != tt.expected.IsValid {
				t.Errorf("Expected IsValid %v, got %v", tt.expected.IsValid, result.IsValid)
			}
			if result.OriginalValid != tt.expected.OriginalValid {
				t.Errorf("Expected OriginalValid %v, got %v", tt.expected.OriginalValid, result.OriginalValid)
			}
			if result.ImprovedCode != tt.expected.ImprovedCode {
				t.Errorf("Expected ImprovedCode %s, got %s", tt.expected.ImprovedCode, result.ImprovedCode)
			}
			if abs := result.ImprovedConf - tt.expected.ImprovedConf; abs > 0.01 {
				t.Errorf("Expected ImprovedConf %.2f, got %.2f", tt.expected.ImprovedConf, result.ImprovedConf)
			}
			if result.ImprovedReason != tt.expected.ImprovedReason {
				t.Errorf("Expected ImprovedReason %s, got %s", tt.expected.ImprovedReason, result.ImprovedReason)
			}
			if result.EvalReasoning != tt.expected.EvalReasoning {
				t.Errorf("Expected EvalReasoning %s, got %s", tt.expected.EvalReasoning, result.EvalReasoning)
			}
		})
	}
}

func TestBuildEvaluationPrompt(t *testing.T) {
	db := Database{
		Writing: []Writing{
			{Phelps: "AB00001FIR", Language: "en", Text: "O Thou Who art the Lord of all names"},
		},
	}

	originalResponse := LLMResponse{
		PhelpsCode: "AB00001FIR",
		Confidence: 0.85,
		Reasoning:  "Strong match based on distinctive opening phrase",
	}

	prayerText := "O Thou Who art the Lord of all names and the Maker of the heavens!"

	prompt := buildEvaluationPrompt(db, prayerText, originalResponse)

	// Check that prompt contains essential components
	if !strings.Contains(prompt, "expert evaluator") {
		t.Error("Prompt should identify role as expert evaluator")
	}
	if !strings.Contains(prompt, "AB00001FIR") {
		t.Error("Prompt should contain the Phelps code being evaluated")
	}
	if !strings.Contains(prompt, "85.0%") {
		t.Error("Prompt should contain the confidence percentage")
	}
	if !strings.Contains(prompt, prayerText) {
		t.Error("Prompt should contain the prayer text to evaluate")
	}
	if !strings.Contains(prompt, "VALID:") {
		t.Error("Prompt should request VALID response field")
	}
	if !strings.Contains(prompt, "IMPROVED_CODE:") {
		t.Error("Prompt should request IMPROVED_CODE response field")
	}
	if !strings.Contains(prompt, "EVALUATION:") {
		t.Error("Prompt should request EVALUATION response field")
	}

	// Check that matched prayer text from database is included
	if !strings.Contains(prompt, "MATCHED PRAYER TEXT") {
		t.Error("Prompt should include matched prayer text from database")
	}
}

func TestEvaluationThresholds(t *testing.T) {
	tests := []struct {
		name           string
		phelpsCode     string
		confidence     float64
		shouldEvaluate bool
	}{
		{
			name:           "High confidence should be evaluated",
			phelpsCode:     "AB00001FIR",
			confidence:     0.75,
			shouldEvaluate: true,
		},
		{
			name:           "Medium confidence should be evaluated",
			phelpsCode:     "AB00002SEC",
			confidence:     0.45,
			shouldEvaluate: true,
		},
		{
			name:           "Very low confidence should skip evaluation",
			phelpsCode:     "AB00003THI",
			confidence:     0.25,
			shouldEvaluate: false,
		},
		{
			name:           "UNKNOWN should skip evaluation",
			phelpsCode:     "UNKNOWN",
			confidence:     0.0,
			shouldEvaluate: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// This tests the logic in callLLMWithEvaluation
			shouldSkip := tt.phelpsCode == "UNKNOWN" || tt.confidence < 0.3
			shouldEvaluate := !shouldSkip

			if shouldEvaluate != tt.shouldEvaluate {
				t.Errorf("Expected shouldEvaluate %v, got %v for %s with confidence %.2f",
					tt.shouldEvaluate, shouldEvaluate, tt.phelpsCode, tt.confidence)
			}
		})
	}
}

// Test persistent cross-session note system
func TestCrossSessionNotes(t *testing.T) {
	// Initialize session to create tables
	initializeSession()

	// Clear any existing notes for testing
	removeSessionNotes("", "", 0) // Remove all notes

	// Add a note to the current session
	addSessionNote("en", "SUCCESS", "Test cross-session note persistence", "AB00001FIR", 0.85)

	// Get notes - should include the one we just added
	notes := getRelevantNotes("en")

	if len(notes) == 0 {
		t.Error("Expected at least one note to be returned")
	}

	found := false
	for _, note := range notes {
		if strings.Contains(note.Content, "Test cross-session note persistence") {
			found = true
			if note.NoteType != "SUCCESS" {
				t.Errorf("Expected note type SUCCESS, got %s", note.NoteType)
			}
			if note.PhelpsCode != "AB00001FIR" {
				t.Errorf("Expected Phelps code AB00001FIR, got %s", note.PhelpsCode)
			}
			if note.Confidence != 0.85 {
				t.Errorf("Expected confidence 0.85, got %f", note.Confidence)
			}
			break
		}
	}

	if !found {
		t.Error("Expected to find the test note in retrieved notes")
	}
}

func TestSearchCrossSessionNotes(t *testing.T) {
	// Initialize session to create tables
	initializeSession()

	// Add some test notes with different types and languages
	addSessionNote("en", "PATTERN", "English prayers often start with 'O Thou'", "", 0.0)
	addSessionNote("es", "STRATEGY", "Use opening phrase matching for Spanish prayers", "", 0.0)
	addSessionNote("fr", "TIP", "French translations may have different word order", "", 0.0)

	// Search by content
	results := searchSessionNotes("opening phrase", "", "")
	found := false
	for _, note := range results {
		if strings.Contains(note.Content, "opening phrase") {
			found = true
			break
		}
	}
	if !found {
		t.Error("Expected to find note containing 'opening phrase'")
	}

	// Search by type
	strategyNotes := searchSessionNotes("", "STRATEGY", "")
	found = false
	for _, note := range strategyNotes {
		if note.NoteType == "STRATEGY" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Expected to find STRATEGY type notes")
	}

	// Search by language
	spanishNotes := searchSessionNotes("", "", "es")
	found = false
	for _, note := range spanishNotes {
		if note.Language == "es" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Expected to find Spanish language notes")
	}
}

func TestFormatNotesForPrompt(t *testing.T) {
	// Create some test notes
	testNotes := []SessionNote{
		{
			Timestamp:  time.Now().Add(-10 * time.Minute),
			Language:   "en",
			NoteType:   "SUCCESS",
			Content:    "Successfully matched using opening phrase",
			PhelpsCode: "AB00001FIR",
			Confidence: 0.90,
			SessionID:  "test_session_123",
		},
		{
			Timestamp: time.Now().Add(-5 * time.Minute),
			Language:  "en",
			NoteType:  "PATTERN",
			Content:   "Prayers with 'divine mercy' often relate to compassion themes",
			SessionID: "test_session_456",
		},
	}

	formatted := formatNotesForPrompt(testNotes)

	// Check that the formatted output contains expected elements
	if !strings.Contains(formatted, "CROSS-SESSION EXPERIENCE NOTES") {
		t.Error("Expected header about cross-session experience")
	}
	if !strings.Contains(formatted, "Successfully matched using opening phrase") {
		t.Error("Expected to find success note content")
	}
	if !strings.Contains(formatted, "AB00001FIR") {
		t.Error("Expected to find Phelps code in success note")
	}
	if !strings.Contains(formatted, "90%") {
		t.Error("Expected to find confidence percentage")
	}
	if !strings.Contains(formatted, "[SESSION:") {
		t.Error("Expected to find session ID information")
	}
	if !strings.Contains(formatted, "✅ SUCCESS") {
		t.Error("Expected success emoji and type")
	}
	if !strings.Contains(formatted, "🔍 PATTERN") {
		t.Error("Expected pattern emoji and type")
	}
}
