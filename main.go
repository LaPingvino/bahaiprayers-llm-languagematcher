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

// --- Configuration ---
var claudeAPIKey string
var useCLI bool
var useGemini bool
var useGptOss bool
var useCompressed bool
var useUltraCompressed bool
var claudeModel = "claude-sonnet-4-20250514" // Latest Sonnet 4

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
	IsVerified bool
}

type Language struct {
	LangCode string
	InLang   string
	Name     string
}

type Database struct {
	Writings  []Writing
	Languages []Language
}

// EnglishReference represents a prayer from the English reference collection
type EnglishReference struct {
	Phelps   string
	Name     string
	Text     string
	Category string
}

// TargetPrayer represents a prayer in the target language to be matched
type TargetPrayer struct {
	Version string
	Name    string
	Text    string
	Link    string
	Source  string
}

// MatchResult represents the LLM's matching decision
type MatchResult struct {
	Phelps         string `json:"phelps"`
	TargetVersion  string `json:"target_version"`
	MatchType      string `json:"match_type"` // EXISTING, NEW_TRANSLATION, or SKIP
	Confidence     int    `json:"confidence"`
	TranslatedText string `json:"translated_text,omitempty"`
	Reasoning      string `json:"reasoning"`
}

// BatchMatchResponse is the complete response from Claude
type BatchMatchResponse struct {
	Matches []MatchResult `json:"matches"`
	Summary string        `json:"summary"`
}

// --- Dolt Helper Functions ---

func execDoltCommand(args ...string) *exec.Cmd {
	cmd := exec.Command("dolt", args...)
	cmd.Dir = "bahaiwritings"
	cmd.Env = append(os.Environ(), "DOLT_PAGER=cat")
	return cmd
}

func execDoltQuery(query string) ([]byte, error) {
	cmd := execDoltCommand("sql", "-q", query)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("dolt query failed: %w: %s", err, string(output))
	}
	return output, nil
}

func execDoltQueryCSV(query string) ([]byte, error) {
	cmd := execDoltCommand("sql", "-r", "csv", "-q", query)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("dolt CSV query failed: %w: %s", err, string(output))
	}
	return output, nil
}

func parseBool(s string) bool {
	b, err := strconv.ParseBool(s)
	if err != nil {
		return false
	}
	return b
}

// GetDatabase loads the writings and languages from Dolt
func GetDatabase() (Database, error) {
	db := Database{
		Writings:  []Writing{},
		Languages: []Language{},
	}

	// Load writings
	csvOut, err := execDoltQueryCSV("SELECT phelps,language,version,name,type,notes,link,text,source,source_id,is_verified FROM writings")
	if err != nil {
		return Database{}, fmt.Errorf("failed to load writings: %w", err)
	}

	r := csv.NewReader(strings.NewReader(string(csvOut)))
	r.FieldsPerRecord = 11
	r.LazyQuotes = true
	records, err := r.ReadAll()
	if err != nil {
		return Database{}, fmt.Errorf("failed to parse writings CSV: %v", err)
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
			IsVerified: parseBool(rec[10]),
		}
		db.Writings = append(db.Writings, w)
	}

	// Load languages
	csvOut, err = execDoltQueryCSV("SELECT langcode,inlang,name FROM languages")
	if err != nil {
		return Database{}, fmt.Errorf("failed to load languages: %w", err)
	}

	r = csv.NewReader(strings.NewReader(string(csvOut)))
	r.FieldsPerRecord = 3
	records, err = r.ReadAll()
	if err != nil {
		return Database{}, fmt.Errorf("failed to parse languages CSV: %v", err)
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

	return db, nil
}

// --- Claude API Integration ---

type ClaudeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ClaudeRequest struct {
	Model     string          `json:"model"`
	Messages  []ClaudeMessage `json:"messages"`
	MaxTokens int             `json:"max_tokens"`
}

type ClaudeContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ClaudeResponse struct {
	Content []ClaudeContentBlock `json:"content"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// CallClaudeCLI uses the claude CLI tool (works with Claude Pro subscription)
func CallClaudeCLI(prompt string) (string, error) {
	log.Printf("Calling Claude via CLI (model: %s)...", claudeModel)

	// Write prompt to temp file
	tmpFile, err := os.CreateTemp("", "claude_prompt_*.txt")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(prompt); err != nil {
		return "", fmt.Errorf("failed to write prompt: %w", err)
	}
	tmpFile.Close()

	// Call claude CLI with file input
	// Use bash -c to handle the file redirection properly
	cmd := exec.Command("bash", "-c", fmt.Sprintf("claude --model %s --print < %s", claudeModel, tmpFile.Name()))
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("claude CLI failed: %w: %s", err, string(output))
	}

	log.Printf("Claude CLI success")
	return strings.TrimSpace(string(output)), nil
}

// CallClaudeAPI uses the direct API (requires API key)
func CallClaudeAPI(prompt string, maxTokens int) (string, error) {
	if claudeAPIKey == "" {
		return "", fmt.Errorf("CLAUDE_API_KEY environment variable not set")
	}

	log.Printf("Calling Claude API (model: %s, max tokens: %d)...", claudeModel, maxTokens)

	request := ClaudeRequest{
		Model:     claudeModel,
		Messages:  []ClaudeMessage{{Role: "user", Content: prompt}},
		MaxTokens: maxTokens,
	}

	requestBody, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", bytes.NewBuffer(requestBody))
	if err != nil {
		return "", fmt.Errorf("failed to create HTTP request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", claudeAPIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	client := &http.Client{}
	resp, err := client.Do(httpReq)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("claude API timed out after 5 minutes")
		}
		return "", fmt.Errorf("claude API request failed: %w", err)
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("claude API returned status %d: %s", resp.StatusCode, string(responseBody))
	}

	var claudeResponse ClaudeResponse
	if err := json.Unmarshal(responseBody, &claudeResponse); err != nil {
		return "", fmt.Errorf("failed to parse Claude response: %w", err)
	}

	if len(claudeResponse.Content) == 0 {
		return "", fmt.Errorf("claude API returned empty content")
	}

	log.Printf("Claude API success (input: %d tokens, output: %d tokens)",
		claudeResponse.Usage.InputTokens, claudeResponse.Usage.OutputTokens)

	return strings.TrimSpace(claudeResponse.Content[0].Text), nil
}

// CallClaude is the unified interface that picks CLI or API
// CallGeminiCLI calls Gemini using the CLI
func CallGeminiCLI(prompt string) (string, error) {
	log.Printf("Calling Gemini via CLI...")

	// Write prompt to temp file
	tmpFile, err := os.CreateTemp("", "gemini_prompt_*.txt")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(prompt); err != nil {
		tmpFile.Close()
		return "", fmt.Errorf("failed to write prompt: %w", err)
	}
	tmpFile.Close()

	// Call gemini CLI
	cmd := exec.Command("gemini", "-f", tmpFile.Name())
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gemini CLI failed: %w: %s", err, string(output))
	}

	log.Printf("Gemini CLI success")
	return string(output), nil
}

// CallGptOss calls gpt-oss for local processing
func CallGptOss(prompt string) (string, error) {
	log.Printf("Calling gpt-oss (local fallback)...")

	// Write prompt to temp file
	tmpFile, err := os.CreateTemp("", "gptoss_prompt_*.txt")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(prompt); err != nil {
		tmpFile.Close()
		return "", fmt.Errorf("failed to write prompt: %w", err)
	}
	tmpFile.Close()

	// Call gpt-oss CLI
	cmd := exec.Command("gpt-oss", "-f", tmpFile.Name())
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gpt-oss CLI failed: %w: %s", err, string(output))
	}

	log.Printf("gpt-oss success (local processing)")
	return string(output), nil
}

// CallClaude routes to CLI, API, or fallback based on configuration
func CallClaude(prompt string, maxTokens int) (string, error) {
	if useGptOss {
		return CallGptOss(prompt)
	}
	if useGemini {
		return CallGeminiCLI(prompt)
	}
	if useCLI {
		return CallClaudeCLI(prompt)
	}
	return CallClaudeAPI(prompt, maxTokens)
}

// --- Matching Logic ---

// BuildEnglishReference extracts all English prayers with Phelps codes
func BuildEnglishReference(db Database) []EnglishReference {
	var refs []EnglishReference
	for _, w := range db.Writings {
		if w.Language == "en" && w.Phelps != "" && w.Text != "" {
			refs = append(refs, EnglishReference{
				Phelps:   w.Phelps,
				Name:     w.Name,
				Text:     w.Text,
				Category: w.Type,
			})
		}
	}
	return refs
}

// BuildTargetPrayers extracts all prayers in target language
func BuildTargetPrayers(db Database, language string) []TargetPrayer {
	var prayers []TargetPrayer
	for _, w := range db.Writings {
		if w.Language == language && w.Text != "" {
			prayers = append(prayers, TargetPrayer{
				Version: w.Version,
				Name:    w.Name,
				Text:    w.Text,
				Link:    w.Link,
				Source:  w.Source,
			})
		}
	}
	return prayers
}

// ChunkSize defines how many English prayers to process per request
const ChunkSize = 30

// CreateMatchingPrompt builds the structured prompt for Claude
func CreateMatchingPrompt(englishRefs []EnglishReference, targetPrayers []TargetPrayer, targetLang string, chunkInfo string) string {
	var prompt strings.Builder

	prompt.WriteString("You are an expert in Bahá'í prayers tasked with matching prayers across languages.\n\n")
	if chunkInfo != "" {
		prompt.WriteString(fmt.Sprintf("# BATCH INFO\n%s\n\n", chunkInfo))
	}
	prompt.WriteString("# YOUR TASK\n")
	prompt.WriteString(fmt.Sprintf("Match %s prayers to English reference prayers using Phelps codes. For each English prayer:\n", targetLang))
	prompt.WriteString("1. Find the matching prayer in the target language collection (if it exists)\n")
	prompt.WriteString("2. If no match exists, provide a NEW_TRANSLATION\n")
	prompt.WriteString("3. Assign the same Phelps code to maintain cross-language linking\n\n")

	prompt.WriteString("# ENGLISH REFERENCE COLLECTION (with Phelps codes)\n")
	prompt.WriteString("```json\n")
	englishJSON, _ := json.MarshalIndent(englishRefs, "", "  ")
	prompt.WriteString(string(englishJSON))
	prompt.WriteString("\n```\n\n")

	prompt.WriteString(fmt.Sprintf("# TARGET LANGUAGE COLLECTION (%s)\n", targetLang))
	prompt.WriteString("```json\n")
	targetJSON, _ := json.MarshalIndent(targetPrayers, "", "  ")
	prompt.WriteString(string(targetJSON))
	prompt.WriteString("\n```\n\n")

	prompt.WriteString("# OUTPUT FORMAT\n")
	prompt.WriteString("Respond with JSON in this exact format:\n")
	prompt.WriteString("```json\n")
	prompt.WriteString(`{
  "matches": [
    {
      "phelps": "AB00001FIR",
      "target_version": "es_prayer_001",
      "match_type": "EXISTING",
      "confidence": 95,
      "reasoning": "Opening phrase and content match perfectly"
    },
    {
      "phelps": "AB00002SEC",
      "target_version": "",
      "match_type": "NEW_TRANSLATION",
      "confidence": 100,
      "translated_text": "[Your translation here]",
      "reasoning": "No existing translation found, created new one"
    }
  ],
  "summary": "Matched X existing prayers, created Y new translations"
}
`)
	prompt.WriteString("```\n\n")

	prompt.WriteString("# RULES\n")
	prompt.WriteString("- match_type must be: EXISTING (found match), NEW_TRANSLATION (translate), or SKIP (cannot match/translate)\n")
	prompt.WriteString("- For EXISTING: provide target_version\n")
	prompt.WriteString("- For NEW_TRANSLATION: provide translated_text\n")
	prompt.WriteString("- confidence: 0-100 (how sure you are of the match/translation)\n")
	prompt.WriteString("- Always include reasoning\n")
	prompt.WriteString("- Preserve theological terminology and style\n\n")

	prompt.WriteString("Begin matching now. Return only the JSON response.\n")

	return prompt.String()
}

// ProcessMatchResults applies the matching results to the database
func ProcessMatchResults(db *Database, results BatchMatchResponse, targetLang string, reportFile *os.File) error {
	matched := 0
	translated := 0
	skipped := 0

	for _, match := range results.Matches {
		fmt.Fprintf(reportFile, "\n--- Processing: %s ---\n", match.Phelps)
		fmt.Fprintf(reportFile, "  Type: %s\n", match.MatchType)
		fmt.Fprintf(reportFile, "  Confidence: %d%%\n", match.Confidence)
		fmt.Fprintf(reportFile, "  Reasoning: %s\n", match.Reasoning)

		switch match.MatchType {
		case "EXISTING":
			// Update existing prayer with Phelps code
			query := fmt.Sprintf("UPDATE writings SET phelps = '%s' WHERE version = '%s' AND language = '%s'",
				strings.ReplaceAll(match.Phelps, "'", "''"),
				strings.ReplaceAll(match.TargetVersion, "'", "''"),
				targetLang)

			if _, err := execDoltQuery(query); err != nil {
				fmt.Fprintf(reportFile, "  ERROR: Failed to update: %v\n", err)
				continue
			}
			matched++
			fmt.Fprintf(reportFile, "  ✅ Updated %s -> %s\n", match.TargetVersion, match.Phelps)

		case "NEW_TRANSLATION":
			// Insert new translation
			version := fmt.Sprintf("%s_llm_%s", targetLang, match.Phelps)
			query := fmt.Sprintf(`INSERT INTO writings (phelps, language, version, name, text, source, is_verified)
				VALUES ('%s', '%s', '%s', 'LLM Translation', '%s', 'LLM_TRANSLATION', false)`,
				strings.ReplaceAll(match.Phelps, "'", "''"),
				targetLang,
				strings.ReplaceAll(version, "'", "''"),
				strings.ReplaceAll(match.TranslatedText, "'", "''"))

			if _, err := execDoltQuery(query); err != nil {
				fmt.Fprintf(reportFile, "  ERROR: Failed to insert: %v\n", err)
				continue
			}
			translated++
			fmt.Fprintf(reportFile, "  ✅ Created new translation for %s\n", match.Phelps)

		case "SKIP":
			skipped++
			fmt.Fprintf(reportFile, "  ⏭️  Skipped\n")
		}
	}

	fmt.Fprintf(reportFile, "\n=== SUMMARY ===\n")
	fmt.Fprintf(reportFile, "Matched existing: %d\n", matched)
	fmt.Fprintf(reportFile, "New translations: %d\n", translated)
	fmt.Fprintf(reportFile, "Skipped: %d\n", skipped)

	log.Printf("Summary: %d matched, %d translated, %d skipped", matched, translated, skipped)

	return nil
}

// --- Main ---

func main() {
	targetLanguage := flag.String("language", "", "Target language code (e.g., es, pt, fr)")
	reportPath := flag.String("report", "matching_report.txt", "Path for the report file")
	dryRun := flag.Bool("dry-run", false, "Don't update database, just show what would happen")
	useCLIFlag := flag.Bool("cli", false, "Use claude CLI instead of API (works with Claude Pro)")
	useGeminiFlag := flag.Bool("gemini", false, "Use Gemini CLI as fallback when Claude hits rate limits")
	useGptOssFlag := flag.Bool("gpt-oss", false, "Use gpt-oss as local fallback (no rate limits)")
	useCompressedFlag := flag.Bool("compressed", false, "Use compressed fingerprint matching (90% fewer API calls)")
	useUltraCompressedFlag := flag.Bool("ultra", false, "Use ultra-compressed multi-language batching (97% fewer API calls)")
	flag.Parse()

	useCLI = *useCLIFlag
	useGemini = *useGeminiFlag
	useGptOss = *useGptOssFlag
	useCompressed = *useCompressedFlag
	useUltraCompressed = *useUltraCompressedFlag

	// Get API key from environment (only required if not using CLI)
	if !useCLI && !useGemini && !useGptOss {
		claudeAPIKey = os.Getenv("CLAUDE_API_KEY")
		if claudeAPIKey == "" {
			log.Fatal("CLAUDE_API_KEY environment variable must be set (or use -cli/-gemini/-gpt-oss flag)")
		}
	}

	// Check Gemini requirements
	if useGemini {
		if os.Getenv("GEMINI_API_KEY") == "" {
			log.Fatal("GEMINI_API_KEY environment variable must be set when using -gemini flag")
		}
	}

	// Check gpt-oss requirements
	if useGptOss {
		if _, err := exec.LookPath("gpt-oss"); err != nil {
			log.Fatal("gpt-oss CLI not found. Please install it first: https://github.com/your-repo/gpt-oss")
		}
	}

	// Route to ultra-compressed matching if requested (processes ALL languages)
	if useUltraCompressed {
		if *targetLanguage != "" {
			log.Println("Note: -ultra flag processes ALL languages, ignoring -language flag")
		}
		if useGptOss {
			log.Printf("Starting ULTRA-COMPRESSED multi-language batch matching with gpt-oss (local)")
		} else if useGemini {
			log.Printf("Starting ULTRA-COMPRESSED multi-language batch matching with Gemini")
		} else {
			log.Printf("Starting ULTRA-COMPRESSED multi-language batch matching")
		}
		if err := UltraCompressedBulkMatching(); err != nil {
			log.Fatalf("Ultra-compressed matching failed: %v", err)
		}
		log.Println("Ultra-compressed matching completed successfully!")
		return
	}

	if *targetLanguage == "" {
		log.Fatal("Error: -language flag is required (e.g., -language=es)")
	}

	// Route to compressed matching if requested
	if useCompressed {
		if useGptOss {
			log.Printf("Starting COMPRESSED matching for language: %s (using gpt-oss local)", *targetLanguage)
		} else if useGemini {
			log.Printf("Starting COMPRESSED matching for language: %s (using Gemini)", *targetLanguage)
		} else {
			log.Printf("Starting COMPRESSED matching for language: %s", *targetLanguage)
		}
		if err := CompressedLanguageMatching(*targetLanguage); err != nil {
			log.Fatalf("Compressed matching failed: %v", err)
		}
		log.Println("Compressed matching completed successfully!")
		return
	}

	log.Printf("Starting structured matching for language: %s", *targetLanguage)

	// Load database
	db, err := GetDatabase()
	if err != nil {
		log.Fatalf("Failed to load database: %v", err)
	}
	log.Printf("Database loaded: %d writings, %d languages", len(db.Writings), len(db.Languages))

	// Build reference collections
	englishRefs := BuildEnglishReference(db)
	targetPrayers := BuildTargetPrayers(db, *targetLanguage)
	log.Printf("English reference: %d prayers", len(englishRefs))
	log.Printf("Target %s prayers: %d prayers", *targetLanguage, len(targetPrayers))

	if len(englishRefs) == 0 {
		log.Fatal("No English reference prayers found")
	}

	// Create report file
	reportFile, err := os.Create(*reportPath)
	if err != nil {
		log.Fatalf("Failed to create report file: %v", err)
	}
	defer reportFile.Close()

	fmt.Fprintf(reportFile, "Structured Prayer Matching Report\n")
	fmt.Fprintf(reportFile, "==================================\n")
	fmt.Fprintf(reportFile, "Started: %s\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(reportFile, "Target Language: %s\n", *targetLanguage)
	fmt.Fprintf(reportFile, "English References: %d\n", len(englishRefs))
	fmt.Fprintf(reportFile, "Target Prayers: %d\n", len(targetPrayers))
	fmt.Fprintf(reportFile, "Dry Run: %v\n\n", *dryRun)

	// Process in chunks
	totalChunks := (len(englishRefs) + ChunkSize - 1) / ChunkSize
	log.Printf("Processing in %d chunks of ~%d prayers each", totalChunks, ChunkSize)
	fmt.Fprintf(reportFile, "Processing in %d chunks\n\n", totalChunks)

	var allMatches []MatchResult

	for chunkIdx := 0; chunkIdx < totalChunks; chunkIdx++ {
		start := chunkIdx * ChunkSize
		end := start + ChunkSize
		if end > len(englishRefs) {
			end = len(englishRefs)
		}

		englishChunk := englishRefs[start:end]
		chunkInfo := fmt.Sprintf("Processing chunk %d/%d (English prayers %d-%d)", chunkIdx+1, totalChunks, start+1, end)

		log.Printf("Chunk %d/%d: Processing %d English prayers", chunkIdx+1, totalChunks, len(englishChunk))
		fmt.Fprintf(reportFile, "\n=== CHUNK %d/%d ===\n", chunkIdx+1, totalChunks)

		// Create prompt for this chunk
		prompt := CreateMatchingPrompt(englishChunk, targetPrayers, *targetLanguage, chunkInfo)

		// Call Claude
		log.Printf("Calling Claude for chunk %d/%d...", chunkIdx+1, totalChunks)
		response, err := CallClaude(prompt, 8000)
		if err != nil {
			log.Fatalf("Claude API failed on chunk %d: %v", chunkIdx+1, err)
		}

		fmt.Fprintf(reportFile, "Claude response received (%d chars)\n", len(response))

		// Parse response
		response = strings.TrimPrefix(response, "```json")
		response = strings.TrimPrefix(response, "```")
		response = strings.TrimSuffix(response, "```")
		response = strings.TrimSpace(response)

		var chunkResults BatchMatchResponse
		if err := json.Unmarshal([]byte(response), &chunkResults); err != nil {
			log.Printf("Failed to parse chunk %d response: %v\nResponse: %s", chunkIdx+1, err, response)
			fmt.Fprintf(reportFile, "ERROR parsing chunk %d: %v\n", chunkIdx+1, err)
			continue
		}

		log.Printf("Chunk %d: Parsed %d matches", chunkIdx+1, len(chunkResults.Matches))
		fmt.Fprintf(reportFile, "Parsed %d matches from chunk %d\n", len(chunkResults.Matches), chunkIdx+1)

		allMatches = append(allMatches, chunkResults.Matches...)

		// Small delay between chunks to be respectful
		if chunkIdx < totalChunks-1 {
			time.Sleep(2 * time.Second)
		}
	}

	log.Printf("Total matches collected: %d", len(allMatches))
	fmt.Fprintf(reportFile, "\n=== PROCESSING ALL RESULTS ===\n")
	fmt.Fprintf(reportFile, "Total matches: %d\n\n", len(allMatches))

	// Combine all matches
	combinedResults := BatchMatchResponse{
		Matches: allMatches,
		Summary: fmt.Sprintf("Processed %d chunks, %d total matches", totalChunks, len(allMatches)),
	}

	if !*dryRun {
		// Process and apply results
		if err := ProcessMatchResults(&db, combinedResults, *targetLanguage, reportFile); err != nil {
			log.Fatalf("Failed to process results: %v", err)
		}

		// Commit to Dolt
		commitMsg := fmt.Sprintf("Structured matching for %s: %s", *targetLanguage, combinedResults.Summary)
		cmd := execDoltCommand("add", ".")
		if output, err := cmd.CombinedOutput(); err != nil {
			log.Printf("WARNING: Failed to stage changes: %v: %s", err, string(output))
		} else {
			cmd = execDoltCommand("commit", "-m", commitMsg)
			if output, err := cmd.CombinedOutput(); err != nil {
				log.Printf("WARNING: Failed to commit: %v: %s", err, string(output))
			} else {
				log.Printf("✅ Committed to Dolt: %s", commitMsg)
				fmt.Fprintf(reportFile, "\n✅ Committed to Dolt: %s\n", commitMsg)
			}
		}
	} else {
		fmt.Fprintf(reportFile, "\n⚠️  DRY RUN - No changes made to database\n")
		log.Println("Dry run completed - no changes made")
	}

	fmt.Fprintf(reportFile, "\nCompleted: %s\n", time.Now().Format(time.RFC3339))
	log.Printf("Report saved to: %s", *reportPath)
}
