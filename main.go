package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// --- Global variables for LLM configuration ---
var OllamaModel string = "gpt-oss"                  // Default Ollama model
var ollamaAPIURL = "http://localhost:11434"         // Ollama API endpoint
var claudeAPIKey *string                           // Claude API Key from flag or env var
var claudeAPIURL = "https://api.anthropic.com/v1/messages" // Claude API endpoint (or configurable)

// --- Data Structures ---
type Writing struct {
	Phelps     string
	Language   string
	Version    string
	Name       string
	Type       string
	Notes      string
	Link       string
	Text       string
	Source     string
	SourceID   string
	IsVerified bool // New field for verification status
}

type Language struct {
	LangCode string
	InLang   string
	Name     string
}

type Inventory struct {
	PIN                    string
	Title                  string
	WordCount              string
	Language               string
	FirstLineOriginal      string
	FirstLineTranslated    string
	Manuscripts            string
	Publications           string
	Translations           string
	MusicalInterpretations string
	Notes                  string
	Abstracts              string
	Subjects               string
}

type Database struct {
	Writings  []Writing
	Languages []Language
	Inventory []Inventory
}

// LLMResponse represents the parsed response from an LLM for matching
type LLMResponse struct {
	PhelpsCode string
	Confidence float64
	Reasoning  string
	Action     string // NEW_TRANSLATION, MATCH_EXISTING, PROBLEM
}

// --- Dolt Helper Functions ---

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

// Helper function to execute parameterized dolt query with safe parameter substitution
func execDoltQueryParam(query string, params ...interface{}) ([]byte, error) {
	finalQuery := query
	for _, param := range params {
		placeholder := "?"
		var replacement string

		switch v := param.(type) {
		case string:
			replacement = fmt.Sprintf("'%s'", strings.ReplaceAll(v, "'", "''"))
		case int, int64, float64:
			replacement = fmt.Sprintf("%v", v)
		case bool: // Handle bool type for is_verified
			replacement = fmt.Sprintf("%t", v)
		default:
			replacement = fmt.Sprintf("'%v'", v)
		}

		if idx := strings.Index(finalQuery, placeholder); idx != -1 {
			finalQuery = finalQuery[:idx] + replacement + finalQuery[idx+len(placeholder):]
		}
	}
	return execDoltQuery(finalQuery)
}

// MustBool safely parses a boolean string, logging a warning on error
func MustBool(s string) bool {
	b, err := strconv.ParseBool(s)
	if err != nil {
		log.Printf("Warning: Failed to parse bool '%s', defaulting to false: %v", s, err)
		return false // Default to false if parsing fails
	}
	return b
}

// GetDatabase loads necessary data from Dolt (writings, languages, inventory)
func GetDatabase() (Database, error) {
	db := Database{
		Writings:  []Writing{},
		Languages: []Language{},
		Inventory: []Inventory{},
	}

	runQuery := func(table string, columns string) (string, error) {
		query := fmt.Sprintf("SELECT %s FROM %s", columns, table)
		out, err := execDoltQueryCSV(query)
		if err != nil {
			return "", fmt.Errorf("dolt query for %s failed: %w", table, err)
		}
		return string(out), nil
	}

	// Load Writing data
	if csvOut, err := runQuery("writings", "phelps,language,version,name,type,notes,link,text,source,source_id,is_verified"); err != nil {
		return Database{}, fmt.Errorf("failed to load writing data: %w", err)
	} else {
		r := csv.NewReader(strings.NewReader(csvOut))
		r.FieldsPerRecord = 11
		r.LazyQuotes = true
		records, err := r.ReadAll()
		if err != nil {
			return Database{}, fmt.Errorf("failed to parse writing CSV: %v", err)
		}
		if len(records) > 0 {
			records = records[1:] // skip header
		}
		for _, rec := range records {
			w := Writing{
				Phelps:     rec[0],
				Language:   rec[1],
				Version:    rec[2],
				Name:       rec[3],
				Type:       rec[4],
				Notes:      rec[5],
				Link:       rec[6],
				Text:       rec[7],
				Source:     rec[8],
				SourceID:   rec[9],
				IsVerified: MustBool(rec[10]),
			}
			db.Writings = append(db.Writings, w)
		}
	}

	// Load Language data
	if csvOut, err := runQuery("languages", "langcode,inlang,name"); err != nil {
		return Database{}, fmt.Errorf("failed to load language data: %w", err)
	} else {
		r := csv.NewReader(strings.NewReader(csvOut))
		r.FieldsPerRecord = 3
		records, err := r.ReadAll()
		if err != nil {
			return Database{}, fmt.Errorf("failed to parse language CSV: %v", err)
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
			db.Languages = append(db.Languages, l)
		}
	}

	// Load Inventory data
	if csvOut, err := runQuery("inventory", "PIN,Title,`Word count`,Language,`First line (original)`,`First line (translated)`,Manuscripts,Publications,Translations,`Musical interpretations`,Notes,Abstracts,Subjects"); err != nil {
		log.Printf("Warning: Failed to load inventory data: %v", err) // Not fatal for this script
	} else {
		r := csv.NewReader(strings.NewReader(csvOut))
		r.FieldsPerRecord = 13
		r.LazyQuotes = true
		records, err := r.ReadAll()
		if err != nil {
			log.Printf("Warning: Failed to parse inventory CSV: %v", err)
		} else {
			if len(records) > 0 {
				records = records[1:] // skip header
			}
			for _, rec := range records {
				inv := Inventory{
					PIN:                    rec[0],
					Title:                  rec[1],
					WordCount:              rec[2],
					Language:               rec[3],
					FirstLineOriginal:      rec[4],
					FirstLineTranslated:    rec[5],
					Manuscripts:            rec[6],
					Publications:           rec[7],
					Translations:           rec[8],
					MusicalInterpretations: rec[9],
					Notes:                  rec[10],
					Abstracts:              rec[11],
					Subjects:               rec[12],
				}
				db.Inventory = append(db.Inventory, inv)
			}
		}
	}

	return db, nil
}

// --- LLM Interaction Functions ---

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

// Call Ollama API
func CallOllama(prompt string, timeout time.Duration) (string, error) {
	log.Printf("Starting Ollama API with model %s (timeout: %v)...", OllamaModel, timeout.Round(time.Second))

	request := OllamaChatRequest{
		Model:    OllamaModel,
		Messages: []OllamaMessage{{Role: "user", Content: prompt}},
		Stream:   false,
	}

	requestBody, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", ollamaAPIURL+"/api/chat", bytes.NewBuffer(requestBody))
	if err != nil {
		return "", fmt.Errorf("failed to create HTTP request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
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

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	var chatResponse OllamaChatResponse
	if err := json.Unmarshal(responseBody, &chatResponse); err != nil {
		return "", fmt.Errorf("failed to parse Ollama response: %w", err)
	}

	return filterCodeBlock(strings.TrimSpace(chatResponse.Message.Content)), nil
}

// Claude API request/response structures (simplified for stub)
type ClaudeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ClaudeRequest struct {
	Model     string          `json:"model"`
	Messages  []ClaudeMessage `json:"messages"`
	MaxTokens int             `json:"max_tokens"`
}

type ClaudeResponse struct {
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
}

// CallClaude Stub function
func CallClaude(prompt string, timeout time.Duration) (string, error) {
	if claudeAPIKey == nil || *claudeAPIKey == "" {
		return "", fmt.Errorf("Claude API Key not provided. Use -claude-api-key flag or set CLAUDE_API_KEY environment variable.")
	}

	log.Printf("Starting Claude API call (timeout: %v)...", timeout.Round(time.Second))

	request := ClaudeRequest{
		Model:    "claude-3-opus-20240229", // Example Claude model
		Messages: []ClaudeMessage{{Role: "user", Content: prompt}},
		MaxTokens: 4000, // Reasonable default
	}

	requestBody, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("failed to marshal Claude request: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", claudeAPIURL, bytes.NewBuffer(requestBody))
	if err != nil {
		return "", fmt.Errorf("failed to create Claude HTTP request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", *claudeAPIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01") // Required for Claude

	client := &http.Client{}
	resp, err := client.Do(httpReq)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("claude API timed out after %v", timeout.Round(time.Second))
		}
		return "", fmt.Errorf("claude API request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("claude API returned status %d: %s", resp.StatusCode, string(body))
	}

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read Claude response: %w", err)
	}

	var claudeResponse ClaudeResponse
	if err := json.Unmarshal(responseBody, &claudeResponse); err != nil {
		return "", fmt.Errorf("failed to parse Claude response: %w", err)
	}

	if len(claudeResponse.Content) > 0 {
		return filterCodeBlock(strings.TrimSpace(claudeResponse.Content[0].Text)), nil
	}

	return "", fmt.Errorf("claude API returned empty content")
}

// CallLLM is an orchestrator to choose between Ollama and Claude
func CallLLM(prompt string, useClaude bool, timeout time.Duration) (string, error) {
	if useClaude {
		return CallClaude(prompt, timeout)
	}
	return CallOllama(prompt, timeout)
}


// --- General Helper Functions ---

// filterCodeBlock removes markdown code block fences (```json) from the response
func filterCodeBlock(response string) string {
	if strings.HasPrefix(response, "```json") {
		response = strings.TrimPrefix(response, "```json")
	} else if strings.HasPrefix(response, "```") {
		response = strings.TrimPrefix(response, "```")
	}
	if strings.HasSuffix(response, "```") {
		response = strings.TrimSuffix(response, "```")
	}
	return strings.TrimSpace(response)
}

// Helper for phelps code generation, if needed for new translations
func generateCodeSuffix(text string) string {
	// Simple keyword extraction for suffix
	textLower := strings.ToLower(text)
	if strings.Contains(textLower, "prayer") {
		return "PRA"
	} else if strings.Contains(textLower, "love") {
		return "LOV"
	} else if strings.Contains(textLower, "mercy") {
		return "MER"
	} else if strings.Contains(textLower, "unity") {
		return "UNI"
	} else if strings.Contains(textLower, "god") {
		return "GOD"
	}
	return "NEW" // Default suffix
}

// Helper to check if a Phelps code already exists in the database
func phelpsCodeExistsInDb(db *Database, phelpsCode string) bool {
	for _, w := range db.Writings {
		if w.Phelps == phelpsCode {
			return true
		}
	}
	return false
}

// --- Main Logic for Translate Stage ---
func translatePrayersMain() {
	targetLanguage := flag.String("language", "", "Target language code for translation (e.g., pt, es)")
	maxPrayers := flag.Int("max", 0, "Maximum number of English prayers to translate (0 = unlimited)")
	ollamaModel := flag.String("model", "gpt-oss", "Ollama model to use for translation (default: gpt-oss)")
	reportPath := flag.String("report", "translation_report.txt", "Path for the translation report file")
	verbose := flag.Bool("verbose", false, "Enable verbose output")
	useClaude := flag.Bool("use-claude", false, "Use Claude API for LLM calls")
	claudeAPIKey = flag.String("claude-api-key", os.Getenv("CLAUDE_API_KEY"), "Claude API Key (or set CLAUDE_API_KEY env var)") // Claude API Key flag
	flag.Parse()

	if *targetLanguage == "" {
		log.Fatal("Error: -language flag is required (e.g., -language=pt)")
	}
	OllamaModel = *ollamaModel

	log.Printf("Starting translation for language: %s using model: %s", *targetLanguage, OllamaModel)

	db, err := GetDatabase()
	if err != nil {
		log.Fatalf("Failed to load database: %v", err)
	}
	log.Println("Database loaded successfully.")

	// Open report file
	reportFile, err := os.Create(*reportPath)
	if err != nil {
		log.Fatalf("Failed to create report file: %v", err)
	}
	defer reportFile.Close()

	fmt.Fprintf(reportFile, "Prayer Translation Report\n")
	fmt.Fprintf(reportFile, "=========================\n")
	fmt.Fprintf(reportFile, "Started: %s\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(reportFile, "Target Language: %s\n", *targetLanguage)
	fmt.Fprintf(reportFile, "LLM Model: %s (Claude: %t)\n", OllamaModel, *useClaude)
	fmt.Fprintf(reportFile, "Max Prayers to Process: %d\n", *maxPrayers)
	fmt.Fprintf(reportFile, "Verbose Mode: %t\n\n", *verbose)

	processedCount := 0
	translatedCount := 0
	skippedCount := 0

	// Get English prayers with Phelps codes
	englishPrayers := make(map[string]Writing)
	for _, w := range db.Writings {
		if w.Language == "en" && w.Phelps != "" && w.Text != "" {
			englishPrayers[w.Phelps] = w
		}
	}

	// Identify already translated prayers in the target language
	existingTranslations := make(map[string]bool) // Map of phelpsCode -> true if translated
	for _, w := range db.Writings {
		if w.Language == *targetLanguage && w.Phelps != "" && w.IsVerified {
			existingTranslations[w.Phelps] = true
		}
	}
	
	// Identify draft translations that might need updating
	existingDrafts := make(map[string]Writing) // Map of phelpsCode -> Writing
	for _, w := range db.Writings {
		if w.Language == *targetLanguage && w.Phelps != "" && w.Source == "LLM_DRAFT_TRANSLATION" {
			existingDrafts[w.Phelps] = w
		}
	}


	fmt.Fprintf(reportFile, "Found %d English prayers with Phelps codes.\n", len(englishPrayers))
	fmt.Fprintf(reportFile, "Found %d existing translations in %s.\n", len(existingTranslations), *targetLanguage)
	fmt.Fprintf(reportFile, "Found %d existing draft translations in %s.\n", len(existingDrafts), *targetLanguage)
	fmt.Printf("Found %d English prayers with Phelps codes.\n", len(englishPrayers))
	fmt.Printf("Found %d existing translations in %s.\n", len(existingTranslations), *targetLanguage)
	fmt.Printf("Found %d existing draft translations in %s.\n", len(existingDrafts), *targetLanguage)


	for phelps, enPrayer := range englishPrayers {
		if *maxPrayers > 0 && processedCount >= *maxPrayers {
			log.Printf("Max prayers limit (%d) reached. Stopping.", *maxPrayers)
			break
		}

		// Skip if a verified translation already exists
		if existingTranslations[phelps] {
			if *verbose {
				fmt.Printf("Skipping %s (English) -> %s: Verified translation already exists.\n", phelps, *targetLanguage)
			}
			skippedCount++
			continue
		}

		processedCount++
		fmt.Printf("\n--- Processing %d/%d: Translating %s (English) to %s ---\n", processedCount, len(englishPrayers), phelps, *targetLanguage)
		fmt.Fprintf(reportFile, "\n--- Processing %d/%d: Translating %s (English) to %s ---\n", processedCount, len(englishPrayers), phelps, *targetLanguage)

		// Prepare prompt for LLM
		prompt := fmt.Sprintf("Translate the following English Baháʼí prayer into %s:\n\n%s", *targetLanguage, enPrayer.Text)

		if *verbose {
			fmt.Printf("  Prompting LLM for translation...\n")
		}

		translatedText, err := CallLLM(prompt, *useClaude, 5*time.Minute) // 5 minutes timeout for translation
		if err != nil {
			log.Printf("Error translating %s to %s: %v", phelps, *targetLanguage, err)
			fmt.Fprintf(reportFile, "  ERROR translating %s: %v\n", phelps, err)
			skippedCount++
			continue
		}

		if strings.TrimSpace(translatedText) == "" {
			log.Printf("LLM returned empty translation for %s to %s", phelps, *targetLanguage)
			fmt.Fprintf(reportFile, "  WARNING: Empty translation returned for %s\n", phelps)
			skippedCount++
			continue
		}

		// Update or insert the translated prayer
		err = updateOrInsertTranslation(&db, phelps, *targetLanguage, translatedText)
		if err != nil {
			log.Printf("Error saving translated prayer %s (%s): %v", phelps, *targetLanguage, err)
			fmt.Fprintf(reportFile, "  ERROR saving translated prayer %s: %v\n", phelps, err)
			skippedCount++
		} else {
			translatedCount++
			fmt.Printf("  ✅ Translated %s to %s. Saved as draft.\n", phelps, *targetLanguage)
			fmt.Fprintf(reportFile, "  Translated %s to %s. Saved as draft.\n", phelps, *targetLanguage)
		}
		time.Sleep(500 * time.Millisecond) // Small delay to avoid overwhelming Ollama/Claude
	}

	fmt.Printf("\n--- Translation Process Complete ---\n")
	fmt.Printf("Total English prayers processed: %d\n", processedCount)
	fmt.Printf("Newly translated/updated drafts: %d\n", translatedCount)
	fmt.Printf("Skipped (verified/errors): %d\n", skippedCount)

	fmt.Fprintf(reportFile, "\n--- Translation Process Complete ---\n")
	fmt.Fprintf(reportFile, "Total English prayers processed: %d\n", processedCount)
	fmt.Fprintf(reportFile, "Newly translated/updated drafts: %d\n", translatedCount)
	fmt.Fprintf(reportFile, "Skipped (verified/errors): %d\n", skippedCount)
	fmt.Fprintf(reportFile, "Completed: %s\n", time.Now().Format(time.RFC3339))

	// Commit changes to Dolt
	if translatedCount > 0 {
		commitMessage := fmt.Sprintf("Translate prayers: %d new/updated drafts for %s", translatedCount, *targetLanguage)
		cmd := execDoltCommand("add", ".")
		if output, err := cmd.CombinedOutput(); err != nil {
			log.Printf("ERROR: Failed to stage changes for Dolt commit: %v: %s", err, string(output))
			fmt.Fprintf(reportFile, "  ERROR: Failed to stage changes for Dolt commit: %v: %s\n", err, string(output))
		} else {
			cmd = execDoltCommand("commit", "-m", commitMessage)
			if output, err := cmd.CombinedOutput(); err != nil {
				log.Printf("ERROR: Failed to commit changes to Dolt: %v: %s", err, string(output))
				fmt.Fprintf(reportFile, "  ERROR: Failed to commit changes to Dolt: %v: %s\n", err, string(output))
			} else {
				log.Printf("SUCCESS: Changes committed to Dolt: %s", commitMessage)
				fmt.Fprintf(reportFile, "  SUCCESS: Changes committed to Dolt: %s\n", commitMessage)
			}
		}
	}
}

// --- Main Logic for Match Stage ---
func matchTranslatedPrayersMain() {
	targetLanguage := flag.String("language", "", "Target language code for matching (e.g., pt, es)")
	maxPrayers := flag.Int("max", 0, "Maximum number of draft prayers to process (0 = unlimited)")
	ollamaModel := flag.String("model", "gpt-oss", "Ollama model to use for matching (default: gpt-oss)")
	reportPath := flag.String("report", "matching_report.txt", "Path for the matching report file")
	verbose := flag.Bool("verbose", false, "Enable verbose output")
	useClaude := flag.Bool("use-claude", false, "Use Claude API for LLM calls")
	claudeAPIKey = flag.String("claude-api-key", os.Getenv("CLAUDE_API_KEY"), "Claude API Key (or set CLAUDE_API_KEY env var)") // Claude API Key flag
	flag.Parse()

	if *targetLanguage == "" {
		log.Fatal("Error: -language flag is required (e.g., -language=pt)")
	}
	OllamaModel = *ollamaModel

	log.Printf("Starting matching for language: %s using model: %s", *targetLanguage, OllamaModel)

	db, err := GetDatabase()
	if err != nil {
		log.Fatalf("Failed to load database: %v", err)
	}
	log.Println("Database loaded successfully.")

	// Open report file
	reportFile, err := os.Create(*reportPath)
	if err != nil {
		log.Fatalf("Failed to create report file: %v", err)
	}
	defer reportFile.Close()

	fmt.Fprintf(reportFile, "Prayer Matching Report (from Draft Translations)\n")
	fmt.Fprintf(reportFile, "===============================================\n")
	fmt.Fprintf(reportFile, "Started: %s\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(reportFile, "Target Language: %s\n", *targetLanguage)
	fmt.Fprintf(reportFile, "LLM Model: %s (Claude: %t)\n", OllamaModel, *useClaude)
	fmt.Fprintf(reportFile, "Max Draft Prayers to Process: %d\n", *maxPrayers)
	fmt.Fprintf(reportFile, "Verbose Mode: %t\n\n", *verbose)

	processedCount := 0
	matchedCount := 0
	newlyAssignedCount := 0
	problemCount := 0

	// Get English prayers with Phelps codes (for reference)
	englishReferencePrayers := make(map[string]Writing)
	for _, w := range db.Writings {
		if w.Language == "en" && w.Phelps != "" && w.Text != "" {
			englishReferencePrayers[w.Phelps] = w
		}
	}

	// Get existing *verified* translations in the target language (for matching against)
	existingVerifiedTranslations := make(map[string]Writing) // phelpsCode -> Writing
	for _, w := range db.Writings {
		if w.Language == *targetLanguage && w.Phelps != "" && w.IsVerified {
			existingVerifiedTranslations[w.Phelps] = w
		}
	}

	// Get draft translations to be processed
	var draftTranslations []Writing
	for _, w := range db.Writings {
		if w.Language == *targetLanguage && w.Source == "LLM_DRAFT_TRANSLATION" && !w.IsVerified {
			draftTranslations = append(draftTranslations, w)
		}
	}

	log.Printf("Found %d English reference prayers.", len(englishReferencePrayers))
	log.Printf("Found %d existing VERIFIED translations in %s.", len(existingVerifiedTranslations), *targetLanguage)
	log.Printf("Found %d DRAFT translations to process in %s.", len(draftTranslations), *targetLanguage)

	fmt.Fprintf(reportFile, "Found %d English reference prayers.\n", len(englishReferencePrayers))
	fmt.Fprintf(reportFile, "Found %d existing VERIFIED translations in %s.\n", len(existingVerifiedTranslations), *targetLanguage)
	fmt.Fprintf(reportFile, "Found %d DRAFT translations to process in %s.\n", len(draftTranslations), *targetLanguage)


	for i, draftPrayer := range draftTranslations {
		if *maxPrayers > 0 && processedCount >= *maxPrayers {
			log.Printf("Max draft prayers limit (%d) reached. Stopping.", *maxPrayers)
			break
		}

		processedCount++
		fmt.Printf("\n--- Processing draft %d/%d: %s (Phelps: %s) ---\n", i+1, len(draftTranslations), draftPrayer.Version, draftPrayer.Phelps)
		fmt.Fprintf(reportFile, "\n--- Processing draft %d/%d: %s (Phelps: %s) ---\n", i+1, len(draftTranslations), draftPrayer.Version, draftPrayer.Phelps)

		// Get the English original for this draft
		enOriginal, hasEnglishOriginal := englishReferencePrayers[draftPrayer.Phelps]

		// Build prompt for matching/verification
		var prompt strings.Builder
		prompt.WriteString(fmt.Sprintf("You are an expert Baháʼí scholar and prayer matcher. Your task is to analyze a [Target Language] prayer text, an English original, and existing canonical translations. Based on this, you will determine if the [Target Language] prayer is a match to an existing known prayer, or if it is a new valid translation.\n\n"))
		prompt.WriteString(fmt.Sprintf("Target Language: %s\n\n", *targetLanguage))

		if hasEnglishOriginal {
			prompt.WriteString(fmt.Sprintf("English Original (Phelps: %s):\n%s\n\n", enOriginal.Phelps, enOriginal.Text))
		} else {
			prompt.WriteString(fmt.Sprintf("English Original: (Phelps: %s) - NOT FOUND IN REFERENCE. This draft might be problematic or the English original is missing.\n\n", draftPrayer.Phelps))
		}

		prompt.WriteString(fmt.Sprintf("Draft %s Translation (Version: %s):\n%s\n\n", *targetLanguage, draftPrayer.Version, draftPrayer.Text))
		
		prompt.WriteString("Here are existing *verified* translations in " + *targetLanguage + " for reference (compare with these first):")
		// Include relevant verified translations in the prompt for context
		// For now, let's just include a few general verified translations,
		// or ones that share similar keywords with the draft or English original.
		// This is where "intelligent context pruning" from the previous discussion would be implemented.
		// For simplification, let's just add a placeholder for now.
		prompt.WriteString("[Curated list of relevant existing VERIFIED translations in " + *targetLanguage + " (Phelps: Text)]\n\n")


		prompt.WriteString("Your response should be in the following JSON format. Please provide a clear action and reasoning:\n")
		prompt.WriteString(`{
  "phelps_code": "<Phelps Code or NEW_TRANSLATION or PROBLEM>",
  "confidence": <0-100 float>,
  "reasoning": "<Your detailed explanation>",
  "action": "<MATCH_EXISTING, NEW_TRANSLATION, or PROBLEM>"
}
`)
		
		prompt.WriteString("Instructions:\n")
		prompt.WriteString("1. If the Draft Translation is identical or very similar to an existing VERIFIED translation in " + *targetLanguage + " (based on content and Phelps code match), set 'action' to 'MATCH_EXISTING' and provide the Phelps code and high confidence.\n")
		prompt.WriteString("2. If the Draft Translation is a valid translation of the English original but does NOT match any existing VERIFIED translation, set 'action' to 'NEW_TRANSLATION'. Assign a Phelps code based on the English original's PIN and generate a 3-letter suffix (e.g., AB00001GOD, AB00001MER). Provide high confidence.\n")
		prompt.WriteString("3. If the Draft Translation is poor quality, irrelevant, or if the English original is missing and cannot be validated, set 'action' to 'PROBLEM'. Provide reasoning and low confidence. In this case, 'phelps_code' can be the original draft's phelps code or empty.\n")
		prompt.WriteString("4. Ensure 'phelps_code' is present for MATCH_EXISTING and NEW_TRANSLATION actions. For NEW_TRANSLATION, generate a unique 10-character code (7-char PIN + 3-char TAG).\n\n")

		if *verbose {
			fmt.Printf("  Prompting LLM for matching/verification...\n")
		}

		var llmResponseRaw string
		var callErr error

		if *useClaude {
			llmResponseRaw, callErr = CallClaude(prompt.String(), 2*time.Minute) // 2 minutes timeout for matching
		} else {
			llmResponseRaw, callErr = CallOllama(prompt.String(), 2*time.Minute)
		}

		if callErr != nil {
			log.Printf("Error during LLM matching for %s (%s): %v", draftPrayer.Version, *targetLanguage, callErr)
			fmt.Fprintf(reportFile, "  ERROR LLM Matching for %s: %v\n", draftPrayer.Version, callErr)
			problemCount++
			continue
		}

		var llmResponse LLMResponse
		if err := json.Unmarshal([]byte(llmResponseRaw), &llmResponse); err != nil {
			log.Printf("Error parsing LLM response for %s (%s): %v. Raw: %s", draftPrayer.Version, *targetLanguage, err, llmResponseRaw)
			fmt.Fprintf(reportFile, "  ERROR Parsing LLM Response for %s: %v. Raw: %s\n", draftPrayer.Version, err, llmResponseRaw)
			problemCount++
			continue
		}

		fmt.Printf("  LLM Decision: %s (Phelps: %s, Conf: %.1f%%)\n", llmResponse.Action, llmResponse.PhelpsCode, llmResponse.Confidence)
		fmt.Fprintf(reportFile, "  LLM Decision: %s (Phelps: %s, Conf: %.1f%%)\n", llmResponse.Action, llmResponse.PhelpsCode, llmResponse.Confidence)

		// Process LLM's action
		switch llmResponse.Action {
		case "MATCH_EXISTING":
			if llmResponse.PhelpsCode == "" {
				log.Printf("ERROR: LLM returned MATCH_EXISTING but no phelps_code for %s", draftPrayer.Version)
				fmt.Fprintf(reportFile, "  ERROR: LLM returned MATCH_EXISTING but no phelps_code for %s\n", draftPrayer.Version)
				problemCount++
				continue
			}
			// Update the draft prayer to reflect it's now a confirmed match
			query := `UPDATE writings SET phelps = ?, source = ?, notes = ?, is_verified = ?
			              WHERE version = ?`
			
			if _, err := execDoltQueryParam(query,
				llmResponse.PhelpsCode,
				"LLM_VERIFIED_MATCH",
				llmResponse.Reasoning,
				tru e, // Verified
				draftPrayer.Version); err != nil {
				log.Printf("Error updating writings table for MATCH_EXISTING (%s): %v", draftPrayer.Version, err)
				fmt.Fprintf(reportFile, "  ERROR DB Update for MATCH_EXISTING: %v\n", err)
				problemCount++
			} else {
				matchedCount++
				fmt.Printf("  ✅ Updated draft %s: Matched to existing %s.\n", draftPrayer.Version, llmResponse.PhelpsCode)
				fmt.Fprintf(reportFile, "  Updated draft %s: Matched to existing %s.\n", draftPrayer.Version, llmResponse.PhelpsCode)
			}

		case "NEW_TRANSLATION":
			// The LLM has confirmed this is a new, valid translation of the English prayer identified by draftPrayer.Phelps.
			// Therefore, the Phelps code should be the same as the English original.
			finalPhelpsCode := draftPrayer.Phelps
			
			// Update the draft prayer
			query := `UPDATE writings SET phelps = ?, source = ?, notes = ?, is_verified = ?
			                      WHERE version = ?`
			
			if _, err := execDoltQueryParam(query,
				finalPhelpsCode,
				"LLM_TRANSLATED_UNVERIFIED",
				llmResponse.Reasoning,
				false, // Still unverified, requires human review
				draftPrayer.Version); err != nil {
				log.Printf("Error updating writings table for NEW_TRANSLATION (%s): %v", draftPrayer.Version, err)
				fmt.Fprintf(reportFile, "  ERROR DB Update for NEW_TRANSLATION: %v\n", err)
				problemCount++
			} else {
				newlyAssignedCount++
				fmt.Printf("  ✅ Updated draft %s: Confirmed as new translation. Assigned Phelps: %s.\n", draftPrayer.Version, finalPhelpsCode)
				fmt.Fprintf(reportFile, "  Updated draft %s: Confirmed as new translation. Assigned Phelps: %s.\n", draftPrayer.Version, finalPhelpsCode)
			}

		case "PROBLEM":
			problemCount++
			// Mark the draft as problematic for review
			query := `UPDATE writings SET source = ?, notes = ?, is_verified = ?
			                      WHERE version = ?`
			
			if _, err := execDoltQueryParam(query,
				"LLM_PROBLEM_DRAFT",
				llmResponse.Reasoning,
				false, // Still unverified
				draftPrayer.Version); err != nil {
				log.Printf("Error updating writings table for PROBLEM (%s): %v", draftPrayer.Version, err)
				fmt.Fprintf(reportFile, "  ERROR DB Update for PROBLEM: %v\n", err)
			} else {
				fmt.Printf("  ⚠️  Marked draft %s as problematic: %s.\n", draftPrayer.Version, llmResponse.Reasoning)
				fmt.Fprintf(reportFile, "  Marked draft %s as problematic: %s.\n", draftPrayer.Version, llmResponse.Reasoning)
			}

		default:
			problemCount++
			log.Printf("LLM returned unregonized action for %s (%s): %s", draftPrayer.Version, *targetLanguage, llmResponse.Action)
			fmt.Fprintf(reportFile, "  ERROR LLM returned unregonized action for %s: %s\n", draftPrayer.Version, llmResponse.Action)
		}
		time.Sleep(500 * time.Millisecond) // Small delay to avoid overwhelming Ollama
	}

	fmt.Printf("\n--- Matching Process Complete ---\n")
	fmt.Printf("Total draft prayers processed: %d\n", processedCount)
	fmt.Printf("Matched to existing prayers: %d\n", matchedCount)
	fmt.Printf("Confirmed as new translations: %d\n", newlyAssignedCount)
	fmt.Printf("Problematic drafts: %d\n", problemCount)

	fmt.Fprintf(reportFile, "\n--- Matching Process Complete ---\n")
	fmt.Fprintf(reportFile, "Total draft prayers processed: %d\n", processedCount)
	fmt.Fprintf(reportFile, "Matched to existing prayers: %d\n", matchedCount)
	fmt.Fprintf(reportFile, "Confirmed as new translations: %d\n", newlyAssignedCount)
	fmt.Fprintf(reportFile, "Problematic drafts: %d\n", problemCount)
	fmt.Fprintf(reportFile, "Completed: %s\n", time.Now().Format(time.RFC3339))

	// Commit changes to Dolt
	if processedCount > 0 { // Commit if anything was processed
		commitMessage := fmt.Sprintf("Match translated prayers: %d drafts processed for %s (matched: %d, new: %d, problems: %d)",
			processedCount, *targetLanguage, matchedCount, newlyAssignedCount, problemCount)
		cmd := execDoltCommand("add", ".")
		if output, err := cmd.CombinedOutput(); err != nil {
			log.Printf("ERROR: Failed to stage changes for Dolt commit: %v: %s", err, string(output))
			fmt.Fprintf(reportFile, "  ERROR: Failed to stage changes for Dolt commit: %v: %s\n", err, string(output))
		} else {
			cmd = execDoltCommand("commit", "-m", commitMessage)
			if output, err := cmd.CombinedOutput(); err != nil {
				log.Printf("ERROR: Failed to commit changes to Dolt: %v: %s", err, string(output))
				fmt.Fprintf(reportFile, "  ERROR: Failed to commit changes to Dolt: %v: %s\n", err, string(output))
			} else {
				log.Printf("SUCCESS: Changes committed to Dolt: %s", commitMessage)
				fmt.Fprintf(reportFile, "  SUCCESS: Changes committed to Dolt: %s\n", commitMessage)
			}
		}
	}
}

func main() {
	var stage = flag.String("stage", "", "Stage to run: 'translate' or 'match'")
	flag.Parse()

	if *stage == "" {
		log.Fatal("Error: -stage flag is required (e.g., -stage=translate or -stage=match)")
	}

	sswitch *stage {
	case "translate":
		translatePrayersMain()
	case "match":
		matchTranslatedPrayersMain()
	default:
		log.Fatalf("Error: Invalid stage '%s'. Must be 'translate' or 'match'.", *stage)
	}
}
