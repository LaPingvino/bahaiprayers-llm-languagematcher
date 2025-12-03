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
	"path/filepath"
	"regexp"
	"sort"
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
var useSmartFallback bool
var useStatusCheck bool
var useRetryBatches bool
var useCsvProcessing bool
var logFile string
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
	Phelps         string  `json:"phelps"`
	TargetVersion  string  `json:"target_version"`
	MatchType      string  `json:"match_type"` // EXISTING, NEW_TRANSLATION, or SKIP
	Confidence     float64 `json:"confidence"`
	TranslatedText string  `json:"translated_text,omitempty"`
	Reasoning      string  `json:"reasoning"`
}

// BatchMatchResponse is the complete response from Claude
type BatchMatchResponse struct {
	Matches []MatchResult `json:"matches"`
	Summary string        `json:"summary"`
}

// Backend represents an available processing backend
type Backend struct {
	Name      string
	Available bool
	Command   string
	Flag      string
	Priority  int
}

// ProcessingStatus represents the current database status
type ProcessingStatus struct {
	TotalPrayers     int
	TotalLanguages   int
	MatchedPrayers   int
	UnmatchedPrayers int
	CompletionRate   int
	UnprocessedLangs int
	TranslitLangs    int
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

func execDoltQueryCSV(query string) ([][]string, error) {
	cmd := execDoltCommand("sql", "-r", "csv", "-q", query)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("dolt CSV query failed: %w: %s", err, string(output))
	}

	// Parse CSV output properly using csv.Reader
	reader := csv.NewReader(strings.NewReader(string(output)))
	reader.LazyQuotes = true
	reader.TrimLeadingSpace = true

	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("failed to parse CSV output: %w", err)
	}

	return records, nil
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
	records, err := execDoltQueryCSV("SELECT phelps,language,version,name,type,notes,link,text,source,source_id,is_verified FROM writings")
	if err != nil {
		return Database{}, fmt.Errorf("failed to load writings: %w", err)
	}

	if len(records) > 0 {
		records = records[1:] // skip header
	}

	for _, rec := range records {
		// Ensure we have enough fields
		if len(rec) < 11 {
			log.Printf("Warning: skipping record with insufficient fields: %v", rec)
			continue
		}
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
	langRecords, err := execDoltQueryCSV("SELECT langcode,inlang,name FROM languages")
	if err != nil {
		return Database{}, fmt.Errorf("failed to load languages: %w", err)
	}

	if len(langRecords) > 0 {
		langRecords = langRecords[1:]
	}

	for _, rec := range langRecords {
		// Ensure we have enough fields
		if len(rec) < 3 {
			log.Printf("Warning: skipping language record with insufficient fields: %v", rec)
			continue
		}
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

	// Call gemini CLI with prompt via stdin
	cmd := exec.Command("gemini")
	cmd.Stdin = strings.NewReader(prompt)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gemini CLI failed: %w: %s", err, string(output))
	}

	log.Printf("Gemini CLI success")
	return string(output), nil
}

// CallGptOss calls ollama for local processing
func CallGptOss(prompt string) (string, error) {
	log.Printf("Calling ollama (local fallback)...")

	// Use gpt-oss model and pass prompt via stdin to avoid argument length limits
	cmd := exec.Command("ollama", "run", "gpt-oss")
	cmd.Stdin = strings.NewReader(prompt)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ollama CLI failed: %w: %s", err, string(output))
	}

	log.Printf("ollama success (local processing)")
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

// getAttemptedPrayersFromReviews returns a set of prayer IDs that have already been attempted
func getAttemptedPrayersFromReviews(language string) map[string]bool {
	attempted := make(map[string]bool)

	// Look for review files for this language
	pattern := fmt.Sprintf("review_*_%s_*.txt", language)
	files, err := filepath.Glob(pattern)
	if err != nil {
		return attempted
	}

	for _, file := range files {
		// Skip summary files
		if strings.Contains(file, "review_summary_") {
			continue
		}

		// Parse review file to extract prayer IDs
		prayerIDs := extractPrayerIDsFromReviewFile(file)
		for _, id := range prayerIDs {
			attempted[id] = true
		}
	}

	return attempted
}

// extractPrayerIDsFromReviewFile extracts target prayer IDs from a review file
func extractPrayerIDsFromReviewFile(filename string) []string {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil
	}

	content := string(data)
	var prayerIDs []string

	lines := strings.Split(content, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		// Look for entries like "[1/366] Target Prayer: 00b7402e-76ec-4a03-a8c4-1c3db8615a3d"
		if strings.Contains(line, "] Target Prayer: ") {
			parts := strings.Split(line, "] Target Prayer: ")
			if len(parts) == 2 {
				prayerIDs = append(prayerIDs, parts[1])
			}
		}
	}

	return prayerIDs
}

// BuildTargetPrayers extracts all prayers in target language, filtering out already-attempted ones
func BuildTargetPrayers(db Database, language string) []TargetPrayer {
	// Get already-attempted prayer IDs from review files
	attemptedPrayers := getAttemptedPrayersFromReviews(language)

	var prayers []TargetPrayer
	for _, w := range db.Writings {
		if w.Language == language && w.Text != "" {
			// Skip prayers that have already been attempted (unless they're matched)
			if attemptedPrayers[w.Version] && w.Phelps == "" {
				continue // Skip unmatched prayers that were already attempted
			}

			prayers = append(prayers, TargetPrayer{
				Version: w.Version,
				Name:    w.Name,
				Text:    w.Text,
				Link:    w.Link,
				Source:  w.Source,
			})
		}
	}

	// Count total prayers for this language
	totalForLang := 0
	for _, w := range db.Writings {
		if w.Language == language {
			totalForLang++
		}
	}

	log.Printf("📋 %s: %d total prayers, %d already attempted, %d to process",
		language, totalForLang, len(attemptedPrayers), len(prayers))

	return prayers
}

// BuildTargetPrayersWithHeuristics creates a heuristically-sorted prayer list with 5% mistake correction sampling
func BuildTargetPrayersWithHeuristics(db Database, language string) []TargetPrayer {
	// Get already-attempted prayer IDs from review files
	attemptedPrayers := getAttemptedPrayersFromReviews(language)

	var unmatchedPrayers []TargetPrayer
	var matchedPrayers []TargetPrayer

	// Separate unmatched and matched prayers
	for _, w := range db.Writings {
		if w.Language == language && w.Text != "" {
			if w.Phelps != "" {
				// This is a matched prayer - add to matched collection
				matchedPrayers = append(matchedPrayers, TargetPrayer{
					Version: w.Version,
					Name:    w.Name,
					Text:    w.Text,
					Link:    w.Link,
					Source:  w.Source,
				})
			} else {
				// This is an unmatched prayer
				// Skip prayers that have already been attempted
				if attemptedPrayers[w.Version] {
					continue // Skip unmatched prayers that were already attempted
				}

				unmatchedPrayers = append(unmatchedPrayers, TargetPrayer{
					Version: w.Version,
					Name:    w.Name,
					Text:    w.Text,
					Link:    w.Link,
					Source:  w.Source,
				})
			}
		}
	}

	// Clean up duplicate Phelps IDs first (keep only best matches)
	if err := cleanupDuplicatePhelpsIDs(db, language); err != nil {
		log.Printf("⚠️  Duplicate cleanup failed for %s: %v", language, err)
	}

	// Apply heuristic sorting to unmatched prayers
	sortPrayersByLikelihood(unmatchedPrayers)

	// Add adaptive % sample of matched prayers for mistake correction
	mistakeCorrectionSample := calculateMistakeCorrectionSample(matchedPrayers, len(unmatchedPrayers))

	// Combine unmatched prayers with mistake correction sample
	finalPrayers := append(unmatchedPrayers, mistakeCorrectionSample...)

	log.Printf("📋 %s Heuristic: %d unmatched + %d mistake-correction = %d total prayers",
		language, len(unmatchedPrayers), len(mistakeCorrectionSample), len(finalPrayers))

	return finalPrayers
}

// sortPrayersByLikelihood sorts prayers by likelihood of successful matching
func sortPrayersByLikelihood(prayers []TargetPrayer) {
	sort.Slice(prayers, func(i, j int) bool {
		// Priority 1: Shorter texts first (easier to match)
		if len(prayers[i].Text) != len(prayers[j].Text) {
			return len(prayers[i].Text) < len(prayers[j].Text)
		}

		// Priority 2: Common prayer patterns first
		scoreI := getPrayerLikelihoodScore(prayers[i])
		scoreJ := getPrayerLikelihoodScore(prayers[j])
		if scoreI != scoreJ {
			return scoreI > scoreJ // Higher score = more likely to match
		}

		// Priority 3: Alphabetical by name for consistency
		return prayers[i].Name < prayers[j].Name
	})
}

// getPrayerLikelihoodScore assigns likelihood scores based on prayer characteristics
func getPrayerLikelihoodScore(prayer TargetPrayer) int {
	score := 0
	name := strings.ToLower(prayer.Name)
	text := strings.ToLower(prayer.Text)

	// High-likelihood patterns (well-known prayers)
	commonPatterns := []string{
		"alláh-u-abhá", "allah-u-abha", "allahu abha",
		"remover of difficulties", "difficulty",
		"tablet of ahmad", "ahmad",
		"fire tablet", "fire",
		"hidden words", "hidden",
		"kitab-i-aqdas", "aqdas",
		"prayers and meditations", "meditation",
		"short obligatory prayer", "obligatory",
		"long healing prayer", "healing",
		"baha'u'llah", "bahaullah", "bahá'u'lláh",
		"abdul-baha", "abdu'l-baha", "'abdu'l-bahá",
	}

	for _, pattern := range commonPatterns {
		if strings.Contains(name, pattern) || strings.Contains(text, pattern) {
			score += 10
		}
	}

	// Medium-likelihood patterns
	if strings.Contains(name, "prayer") || strings.Contains(name, "tablet") {
		score += 5
	}

	// Short texts are generally easier to match
	if len(prayer.Text) < 200 {
		score += 3
	} else if len(prayer.Text) < 500 {
		score += 1
	}

	return score
}

// cleanupDuplicatePhelpsIDs finds duplicate Phelps IDs and keeps only the best match, clearing others
func cleanupDuplicatePhelpsIDs(db Database, language string) error {
	log.Printf("🔍 Cleaning up duplicate Phelps IDs for %s...", language)

	// Get all matched prayers for this language
	var matchedPrayers []Writing
	for _, w := range db.Writings {
		if w.Language == language && w.Phelps != "" && w.Text != "" {
			matchedPrayers = append(matchedPrayers, w)
		}
	}

	if len(matchedPrayers) == 0 {
		return nil
	}

	// Group prayers by Phelps ID
	phelpsGroups := make(map[string][]Writing)
	for _, prayer := range matchedPrayers {
		phelpsGroups[prayer.Phelps] = append(phelpsGroups[prayer.Phelps], prayer)
	}

	duplicatesFound := 0
	duplicatesFixed := 0

	// Process each group with duplicates
	for phelps, group := range phelpsGroups {
		if len(group) <= 1 {
			continue // No duplicates in this group
		}

		duplicatesFound += len(group) - 1
		log.Printf("   🔍 Found %d duplicates for Phelps ID %s", len(group), phelps)

		// Find the best match using similarity scoring
		bestMatch := findBestMatchInGroup(db, group, phelps)
		if bestMatch == nil {
			log.Printf("   ⚠️  Could not determine best match for Phelps %s", phelps)
			continue
		}

		// Clear Phelps codes from all except the best match
		for _, prayer := range group {
			if prayer.Version != bestMatch.Version {
				// Clear the Phelps code for inferior matches
				err := clearPhelpsCode(prayer.Version)
				if err != nil {
					log.Printf("   ❌ Failed to clear Phelps for %s: %v", prayer.Version, err)
				} else {
					log.Printf("   🧹 Cleared Phelps %s from %s (keeping %s)", phelps, prayer.Version, bestMatch.Version)
					duplicatesFixed++
				}
			}
		}
	}

	if duplicatesFound > 0 {
		log.Printf("✅ Duplicate cleanup complete: %d duplicates found, %d fixed", duplicatesFound, duplicatesFixed)
	} else {
		log.Printf("✅ No duplicate Phelps IDs found for %s", language)
	}

	return nil
}

// findBestMatchInGroup uses LLM to determine which prayer in a duplicate group is the best match
func findBestMatchInGroup(db Database, duplicates []Writing, phelps string) *Writing {
	// Get the English reference for this Phelps ID
	var englishRef *Writing
	for _, w := range db.Writings {
		if w.Language == "en" && w.Phelps == phelps && w.Text != "" {
			englishRef = &w
			break
		}
	}

	if englishRef == nil {
		// If no English reference, keep the first one arbitrarily
		log.Printf("   ⚠️  No English reference found for Phelps %s, keeping first duplicate", phelps)
		return &duplicates[0]
	}

	if len(duplicates) == 1 {
		return &duplicates[0]
	}

	// Use LLM to determine the best match
	bestMatch, err := llmResolveDuplicateMatch(*englishRef, duplicates)
	if err != nil {
		log.Printf("   ⚠️  LLM resolution failed for Phelps %s: %v, keeping first duplicate", phelps, err)
		return &duplicates[0]
	}

	return bestMatch
}

// llmResolveDuplicateMatch asks LLM to determine which duplicate is the best match
func llmResolveDuplicateMatch(englishRef Writing, duplicates []Writing) (*Writing, error) {
	language := duplicates[0].Language

	// Build prompt for LLM duplicate resolution
	var prompt strings.Builder
	prompt.WriteString("You are an expert in Bahá'í prayers tasked with resolving duplicate matches.\n\n")
	prompt.WriteString("TASK: Multiple prayers in the same language have been matched to the same English reference. ")
	prompt.WriteString("Determine which ONE is the correct match and should keep the Phelps code.\n\n")

	prompt.WriteString("# ENGLISH REFERENCE PRAYER\n")
	prompt.WriteString(fmt.Sprintf("**Phelps Code**: %s\n", englishRef.Phelps))
	prompt.WriteString(fmt.Sprintf("**Name**: %s\n", englishRef.Name))
	prompt.WriteString(fmt.Sprintf("**Text**: %s\n\n", englishRef.Text))

	prompt.WriteString(fmt.Sprintf("# DUPLICATE %s PRAYERS\n", strings.ToUpper(language)))
	for i, dup := range duplicates {
		prompt.WriteString(fmt.Sprintf("## Option %d\n", i+1))
		prompt.WriteString(fmt.Sprintf("**Version**: %s\n", dup.Version))
		prompt.WriteString(fmt.Sprintf("**Name**: %s\n", dup.Name))
		prompt.WriteString(fmt.Sprintf("**Text**: %s\n\n", dup.Text))
	}

	prompt.WriteString("# INSTRUCTIONS\n")
	prompt.WriteString("1. Compare each option's semantic meaning, completeness, and accuracy against the English reference\n")
	prompt.WriteString("2. Consider translation quality, completeness, and contextual appropriateness\n")
	prompt.WriteString("3. Choose the ONE best match\n")
	prompt.WriteString("4. Respond with ONLY the option number (1, 2, 3, etc.) of the best match\n")
	prompt.WriteString("5. If options are equivalent, choose 1\n\n")
	prompt.WriteString("RESPONSE: ")

	// Call LLM with backend fallback
	response, err := callLLMWithFallback(prompt.String())
	if err != nil {
		return nil, fmt.Errorf("LLM call failed: %w", err)
	}

	// Parse the response to get the option number
	response = strings.TrimSpace(response)
	optionNum := 1 // default fallback

	// Extract number from response
	for _, char := range response {
		if char >= '1' && char <= '9' {
			if int(char-'0') <= len(duplicates) {
				optionNum = int(char - '0')
				break
			}
		}
	}

	log.Printf("   🤖 LLM selected option %d out of %d duplicates for Phelps %s",
		optionNum, len(duplicates), englishRef.Phelps)

	return &duplicates[optionNum-1], nil
}

// callLLMWithBackendFallback calls LLM with backend fallback system, respecting command line flags
func callLLMWithBackendFallback(prompt string, context string, stopOnRateLimit bool) (string, error) {
	// Build backends list based on command line flags in priority order
	var backends []struct {
		name string
		call func(string) (string, error)
	}

	// Priority order: Claude CLI -> Gemini CLI -> ollama (same as ultra_compressed_matcher)
	if useCLI {
		backends = append(backends, struct {
			name string
			call func(string) (string, error)
		}{"Claude CLI", func(p string) (string, error) { return CallClaudeCLI(p) }})
	}

	if useGemini {
		backends = append(backends, struct {
			name string
			call func(string) (string, error)
		}{"Gemini CLI", func(p string) (string, error) { return CallGeminiCLI(p) }})
	}

	if useGptOss {
		backends = append(backends, struct {
			name string
			call func(string) (string, error)
		}{"ollama", func(p string) (string, error) { return CallGptOss(p) }})
	}

	// If no CLI flags are set, use Claude API (but only for non-ultra processing)
	if !useCLI && !useGemini && !useGptOss && claudeAPIKey != "" {
		backends = append(backends, struct {
			name string
			call func(string) (string, error)
		}{"Claude API", func(p string) (string, error) { return CallClaudeAPI(p, 8000) }})
	}

	// Fallback: if no backends specified or available, use full fallback chain
	if len(backends) == 0 {
		backends = []struct {
			name string
			call func(string) (string, error)
		}{
			{"Claude CLI", func(p string) (string, error) { return CallClaudeCLI(p) }},
			{"Gemini CLI", func(p string) (string, error) { return CallGeminiCLI(p) }},
			{"ollama", func(p string) (string, error) { return CallGptOss(p) }},
		}
	}

	var lastErr error
	for _, backend := range backends {
		if context != "" {
			log.Printf("🔄 Trying %s for %s...", backend.name, context)
		} else {
			log.Printf("🔄 Trying %s...", backend.name)
		}

		response, err := backend.call(prompt)
		if err == nil {
			if context != "" {
				log.Printf("✅ Success with %s for %s", backend.name, context)
			} else {
				log.Printf("✅ Success with %s", backend.name)
			}
			return response, nil
		}

		if context != "" {
			log.Printf("❌ %s failed for %s: %v", backend.name, context, err)
		} else {
			log.Printf("❌ Failed with %s: %v", backend.name, err)
		}
		lastErr = err

		// Check if we should stop on rate limits
		if stopOnRateLimit && (strings.Contains(err.Error(), "limit reached") || strings.Contains(err.Error(), "rate limit")) {
			break
		}
	}

	return "", fmt.Errorf("all backends failed, last error: %w", lastErr)
}

// callLLMWithFallback calls LLM with backend fallback for duplicate resolution
func callLLMWithFallback(prompt string) (string, error) {
	return callLLMWithBackendFallback(prompt, "duplicate resolution", false)
}

// clearPhelpsCode removes the Phelps code from a specific prayer version
func clearPhelpsCode(version string) error {
	query := fmt.Sprintf("UPDATE writings SET phelps = '' WHERE version = '%s'", version)
	_, err := execDoltQuery(query)
	return err
}

// calculateMistakeCorrectionSample selects prayers for re-evaluation, prioritizing error indicators
func calculateMistakeCorrectionSample(matchedPrayers []TargetPrayer, unmatchedCount int) []TargetPrayer {
	if len(matchedPrayers) == 0 {
		return []TargetPrayer{}
	}

	// Calculate adaptive percentage based on completion rate - INCREASED for error correction
	totalPrayers := len(matchedPrayers) + unmatchedCount
	completionRate := float64(len(matchedPrayers)) / float64(totalPrayers)

	var percentage int
	if completionRate > 0.95 {
		percentage = 40 // 40% for nearly complete languages (aggressive error correction)
	} else if completionRate > 0.90 {
		percentage = 35 // 35% for very high completion languages
	} else if completionRate > 0.80 {
		percentage = 25 // 25% for high completion languages
	} else if completionRate > 0.50 {
		percentage = 15 // 15% for medium completion languages
	} else {
		percentage = 10 // 10% default for lower completion languages
	}

	// Calculate sample size: percentage of MATCHED prayers (error correction focus), minimum 5, maximum 100
	sampleSize := max(5, min(100, len(matchedPrayers)*percentage/100))

	// Collect ALL error indicators
	var errorPrayers []TargetPrayer
	errorReasons := make(map[string][]string) // Track why each prayer was flagged

	// 1. Find duplicate Phelps IDs (highest priority error)
	duplicatePhelps := findDuplicatePhelpsIDs(matchedPrayers)
	for _, prayer := range duplicatePhelps {
		errorPrayers = append(errorPrayers, prayer)
		errorReasons[prayer.Version] = append(errorReasons[prayer.Version], "duplicate_phelps_id")
	}

	// 2. Find prayers with significant length mismatches
	lengthMismatches := findLengthMismatches(matchedPrayers)
	for _, prayer := range lengthMismatches {
		if !contains(errorPrayers, prayer) {
			errorPrayers = append(errorPrayers, prayer)
		}
		errorReasons[prayer.Version] = append(errorReasons[prayer.Version], "length_mismatch")
	}

	// 3. Find prayers potentially confused with similar prayers (e.g., short/middle/long obligatory)
	similarConfusion := findSimilarPrayerConfusion(matchedPrayers)
	for _, prayer := range similarConfusion {
		if !contains(errorPrayers, prayer) {
			errorPrayers = append(errorPrayers, prayer)
		}
		errorReasons[prayer.Version] = append(errorReasons[prayer.Version], "similar_prayer_confusion")
	}

	// 4. Find prayers with missing English reference
	missingEnglish := findMissingEnglishReference(matchedPrayers)
	for _, prayer := range missingEnglish {
		if !contains(errorPrayers, prayer) {
			errorPrayers = append(errorPrayers, prayer)
		}
		errorReasons[prayer.Version] = append(errorReasons[prayer.Version], "missing_english_reference")
	}

	// Log error statistics
	log.Printf("   🔍 Error Detection Results:")
	log.Printf("      - Duplicate Phelps IDs: %d", len(duplicatePhelps))
	log.Printf("      - Length mismatches: %d", len(lengthMismatches))
	log.Printf("      - Similar prayer confusion: %d", len(similarConfusion))
	log.Printf("      - Missing English reference: %d", len(missingEnglish))
	log.Printf("      - Total error candidates: %d", len(errorPrayers))

	// If we have more error candidates than sample size, prioritize by severity
	if len(errorPrayers) > sampleSize {
		errorPrayers = prioritizeErrorsBySerity(errorPrayers, errorReasons, sampleSize)
	}

	// Fill remaining sample with random selection if needed
	var sample []TargetPrayer
	sample = append(sample, errorPrayers...)

	if len(sample) < sampleSize {
		remaining := sampleSize - len(sample)
		nonErrors := filterOutErrors(matchedPrayers, errorPrayers)

		for i := 0; i < remaining && i < len(nonErrors); i++ {
			// Use modulo to pseudo-randomly select prayers across the collection
			index := (i*7 + 3) % len(nonErrors)
			sample = append(sample, nonErrors[index])
		}
	}

	log.Printf("   📊 Error Correction: %d%% sample rate (completion: %.1f%%), %d total prayers selected (%d errors + %d random)",
		percentage, completionRate*100, len(sample), len(errorPrayers), len(sample)-len(errorPrayers))

	return sample
}

// findDuplicatePhelpsIDs identifies prayers that share the same Phelps ID (indicating matching mistakes)
// NOTE: TMP codes are EXCLUDED - duplicate TMP codes are valid (same prayer in multiple languages)
func findDuplicatePhelpsIDs(prayers []TargetPrayer) []TargetPrayer {
	// First, we need to get the Phelps IDs for these prayers from the database
	// Since TargetPrayer doesn't have Phelps field, we need to query the database
	db, err := GetDatabase()
	if err != nil {
		log.Printf("⚠️  Failed to load database for duplicate detection: %v", err)
		return []TargetPrayer{}
	}

	// Create a map of Version -> Phelps for matched prayers
	versionToPhelps := make(map[string]string)
	for _, w := range db.Writings {
		if w.Phelps != "" {
			versionToPhelps[w.Version] = w.Phelps
		}
	}

	// Count occurrences of each Phelps ID among our prayer set
	// SKIP TMP codes - they are allowed to have duplicates
	phelpsCount := make(map[string]int)
	phelpsToVersions := make(map[string][]string)

	for _, prayer := range prayers {
		if phelps, exists := versionToPhelps[prayer.Version]; exists && phelps != "" {
			// Skip TMP codes - they're not errors when duplicated
			if isTMPCode(phelps) {
				continue
			}
			phelpsCount[phelps]++
			phelpsToVersions[phelps] = append(phelpsToVersions[phelps], prayer.Version)
		}
	}

	// Collect prayers with duplicate Phelps IDs
	var duplicates []TargetPrayer
	duplicateVersions := make(map[string]bool)

	for phelps, count := range phelpsCount {
		if count > 1 {
			// This Phelps ID has multiple prayers - all are potential mistakes
			for _, version := range phelpsToVersions[phelps] {
				duplicateVersions[version] = true
			}
		}
	}

	// Return the actual prayer objects that have duplicate Phelps IDs
	for _, prayer := range prayers {
		if duplicateVersions[prayer.Version] {
			duplicates = append(duplicates, prayer)
		}
	}

	return duplicates
}

// filterOutDuplicates removes duplicate prayers from the main list
func filterOutDuplicates(allPrayers []TargetPrayer, duplicates []TargetPrayer) []TargetPrayer {
	duplicateSet := make(map[string]bool)
	for _, dup := range duplicates {
		duplicateSet[dup.Version] = true
	}

	var filtered []TargetPrayer
	for _, prayer := range allPrayers {
		if !duplicateSet[prayer.Version] {
			filtered = append(filtered, prayer)
		}
	}

	return filtered
}

// filterOutErrors removes error prayers from the main list
func filterOutErrors(allPrayers []TargetPrayer, errors []TargetPrayer) []TargetPrayer {
	errorSet := make(map[string]bool)
	for _, err := range errors {
		errorSet[err.Version] = true
	}

	var filtered []TargetPrayer
	for _, prayer := range allPrayers {
		if !errorSet[prayer.Version] {
			filtered = append(filtered, prayer)
		}
	}

	return filtered
}

// contains checks if a prayer is already in the list
func contains(prayers []TargetPrayer, target TargetPrayer) bool {
	for _, p := range prayers {
		if p.Version == target.Version {
			return true
		}
	}
	return false
}

// findLengthMismatches identifies prayers with significant length differences from their English reference
func findLengthMismatches(matchedPrayers []TargetPrayer) []TargetPrayer {
	db, err := GetDatabase()
	if err != nil {
		log.Printf("⚠️  Failed to load database for length mismatch detection: %v", err)
		return []TargetPrayer{}
	}

	// Create map of Version -> Phelps for matched prayers
	versionToPhelps := make(map[string]string)
	for _, w := range db.Writings {
		if w.Phelps != "" {
			versionToPhelps[w.Version] = w.Phelps
		}
	}

	// Create map of Phelps -> English text length
	phelpsToLength := make(map[string]int)
	for _, w := range db.Writings {
		if w.Language == "en" && w.Phelps != "" {
			phelpsToLength[w.Phelps] = len(w.Text)
		}
	}

	var mismatches []TargetPrayer
	for _, prayer := range matchedPrayers {
		phelps := versionToPhelps[prayer.Version]
		if phelps == "" {
			continue
		}

		englishLength := phelpsToLength[phelps]
		if englishLength == 0 {
			continue // No English reference found
		}

		targetLength := len(prayer.Text)

		// Calculate length ratio - flag if more than 2x or less than 0.5x
		ratio := float64(targetLength) / float64(englishLength)

		// More aggressive thresholds for error detection
		if ratio > 2.5 || ratio < 0.4 {
			mismatches = append(mismatches, prayer)
		}
	}

	return mismatches
}

// findSimilarPrayerConfusion identifies prayers that might be confused with similar prayers
func findSimilarPrayerConfusion(matchedPrayers []TargetPrayer) []TargetPrayer {
	db, err := GetDatabase()
	if err != nil {
		log.Printf("⚠️  Failed to load database for similar prayer detection: %v", err)
		return []TargetPrayer{}
	}

	// Create map of Version -> Phelps
	versionToPhelps := make(map[string]string)
	for _, w := range db.Writings {
		if w.Phelps != "" {
			versionToPhelps[w.Version] = w.Phelps
		}
	}

	// Define groups of similar prayers that are often confused
	similarGroups := map[string][]string{
		"obligatory": {
			"short obligatory", "medium obligatory", "long obligatory",
			"short", "medium", "long", "obligatory prayer",
		},
		"healing": {
			"long healing", "short healing", "healing prayer", "healing",
		},
		"forgiveness": {
			"forgiveness", "remover of difficulties", "difficulty",
		},
		"bahaullah": {
			"bahá'u'lláh", "bahaullah", "baha'u'llah",
		},
		"abdulbaha": {
			"abdul-baha", "abdu'l-baha", "'abdu'l-bahá",
		},
	}

	var confusion []TargetPrayer
	for _, prayer := range matchedPrayers {
		phelps := versionToPhelps[prayer.Version]
		if phelps == "" {
			continue
		}

		// Get English reference name for this Phelps
		var englishName string
		for _, w := range db.Writings {
			if w.Language == "en" && w.Phelps == phelps {
				englishName = strings.ToLower(w.Name)
				break
			}
		}

		if englishName == "" {
			continue
		}

		// Check if this prayer belongs to a confusable group
		prayerName := strings.ToLower(prayer.Name)
		for groupName, keywords := range similarGroups {
			englishMatches := 0
			prayerMatches := 0

			for _, keyword := range keywords {
				if strings.Contains(englishName, keyword) {
					englishMatches++
				}
				if strings.Contains(prayerName, keyword) {
					prayerMatches++
				}
			}

			// If prayer name matches group keywords but English doesn't (or vice versa),
			// it might be a confusion case
			if (prayerMatches > 0 && englishMatches == 0) || (englishMatches > 0 && prayerMatches == 0) {
				confusion = append(confusion, prayer)
				log.Printf("      🔀 Potential confusion in group '%s': %s (%s) -> %s",
					groupName, prayer.Name, prayer.Version, englishName)
				break
			}
		}
	}

	return confusion
}

// findMissingEnglishReference identifies prayers with Phelps codes that don't exist in English
// NOTE: TMP codes are EXCLUDED - they're expected to not have English references
func findMissingEnglishReference(matchedPrayers []TargetPrayer) []TargetPrayer {
	db, err := GetDatabase()
	if err != nil {
		log.Printf("⚠️  Failed to load database for missing English reference detection: %v", err)
		return []TargetPrayer{}
	}

	// Create map of Version -> Phelps
	versionToPhelps := make(map[string]string)
	for _, w := range db.Writings {
		if w.Phelps != "" {
			versionToPhelps[w.Version] = w.Phelps
		}
	}

	// Create set of valid English Phelps codes (including TMP codes in English)
	validPhelps := make(map[string]bool)
	for _, w := range db.Writings {
		if w.Language == "en" && w.Phelps != "" {
			validPhelps[w.Phelps] = true
		}
	}

	var missing []TargetPrayer
	for _, prayer := range matchedPrayers {
		phelps := versionToPhelps[prayer.Version]
		if phelps == "" {
			continue
		}

		// Skip TMP codes - they're allowed to not have English references
		// (that's the whole point - they link to ar/fa instead)
		if isTMPCode(phelps) {
			continue
		}

		// Only flag real Phelps codes that are missing
		if !validPhelps[phelps] {
			missing = append(missing, prayer)
			log.Printf("      ❌ Invalid Phelps code: %s has Phelps %s (no English prayer found)",
				prayer.Version, phelps)
		}
	}

	return missing
}

// prioritizeErrorsBySerity sorts errors by severity and returns top N
func prioritizeErrorsBySerity(errors []TargetPrayer, reasons map[string][]string, limit int) []TargetPrayer {
	type errorScore struct {
		prayer TargetPrayer
		score  int
	}

	var scored []errorScore
	for _, prayer := range errors {
		score := 0
		errorTypes := reasons[prayer.Version]

		for _, errType := range errorTypes {
			switch errType {
			case "duplicate_phelps_id":
				score += 10 // Highest priority
			case "missing_english_reference":
				score += 8
			case "similar_prayer_confusion":
				score += 6
			case "length_mismatch":
				score += 4
			}
		}

		scored = append(scored, errorScore{prayer, score})
	}

	// Sort by score descending
	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})

	// Return top N
	var result []TargetPrayer
	for i := 0; i < len(scored) && i < limit; i++ {
		result = append(result, scored[i].prayer)
	}

	return result
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

// --- JSON Extraction Utility ---

// ExtractJSONFromResponse robustly extracts JSON from LLM responses that may be wrapped in markdown code blocks
func ExtractJSONFromResponse(response string) (string, error) {
	response = strings.TrimSpace(response)

	// First try to find JSON within markdown code blocks
	if jsonStart := strings.Index(response, "```json"); jsonStart != -1 {
		jsonStart += 7 // Skip past "```json"
		// Skip whitespace
		for jsonStart < len(response) && (response[jsonStart] == '\n' || response[jsonStart] == '\r' || response[jsonStart] == ' ' || response[jsonStart] == '\t') {
			jsonStart++
		}
		if jsonEnd := strings.Index(response[jsonStart:], "```"); jsonEnd != -1 {
			jsonStr := strings.TrimSpace(response[jsonStart : jsonStart+jsonEnd])
			if jsonStr != "" {
				// Validate JSON
				var temp interface{}
				if err := json.Unmarshal([]byte(jsonStr), &temp); err == nil {
					return jsonStr, nil
				}
			}
		}
	}

	// Try to find JSON object or array within the response
	var startChar, endChar rune
	var startIdx, endIdx int = -1, -1

	// Look for JSON object {...} or array [...]
	for i, r := range response {
		if r == '{' || r == '[' {
			startChar = r
			if r == '{' {
				endChar = '}'
			} else {
				endChar = ']'
			}
			startIdx = i
			break
		}
	}

	if startIdx == -1 {
		return "", fmt.Errorf("no JSON object or array found in response")
	}

	// Find the matching closing brace/bracket
	depth := 1
	inString := false
	escapeNext := false

	for i := startIdx + 1; i < len(response); i++ {
		r := rune(response[i])

		if escapeNext {
			escapeNext = false
			continue
		}

		if r == '\\' {
			escapeNext = true
			continue
		}

		if r == '"' && !escapeNext {
			inString = !inString
			continue
		}

		if inString {
			continue
		}

		if r == startChar {
			depth++
		} else if r == endChar {
			depth--
			if depth == 0 {
				endIdx = i
				break
			}
		}
	}

	if endIdx == -1 {
		return "", fmt.Errorf("no matching closing brace/bracket found")
	}

	jsonStr := response[startIdx : endIdx+1]

	// Validate that it's proper JSON
	var temp interface{}
	if err := json.Unmarshal([]byte(jsonStr), &temp); err != nil {
		return "", fmt.Errorf("extracted text is not valid JSON: %w", err)
	}

	return jsonStr, nil
}

// RepairJSONWithLLM uses an LLM to fix malformed JSON responses
func RepairJSONWithLLM(brokenResponse string) (string, error) {
	prompt := `You are a JSON repair specialist. I have a broken JSON response from an LLM that needs to be fixed. The response should be in CompressedBatchResponse format with these exact fields:

{
  "matches": [
    {
      "phelps": "BH00009SER",
      "target_version": "0098495e-44fa-4869-ae06-9e4f261c4d7e",
      "match_type": "LIKELY",
      "confidence": 99.17,
      "match_reasons": [
        "strong_key_terms_overlap",
        "strong_signature_overlap",
        "similar_word_count",
        "matching_boolean_flags"
      ]
    }
  ],
  "exact_matches": 0,
  "likely_matches": 72,
  "ambiguous_count": 5,
  "new_translations": 3,
  "summary": "Processed 80 prayers: 0 exact, 72 likely, 5 need review, 3 new translations needed"
}

IMPORTANT:
- confidence can be a float (like 99.17) or integer
- match_type values: EXACT, LIKELY, AMBIGUOUS, NEW_TRANSLATION
- match_reasons are arrays of strings describing why the match was made
- Fix any missing closing braces/brackets, trailing commas, or structural issues
- Preserve all existing data, just fix the JSON structure

Please fix the JSON below and return ONLY the corrected JSON with no explanation:

` + brokenResponse

	// Use common backend fallback for JSON repair
	response, err := callLLMWithBackendFallback(prompt, "JSON repair", false)
	if err != nil {
		return "", fmt.Errorf("JSON repair failed with all backends: %w", err)
	}

	// Try to extract and validate the repaired JSON
	repairedJSON, err := ExtractJSONFromResponse(response)
	if err != nil {
		return "", fmt.Errorf("JSON extraction failed after repair: %w", err)
	}

	// Validate it can be parsed as CompressedBatchResponse
	var testResult CompressedBatchResponse
	if err := json.Unmarshal([]byte(repairedJSON), &testResult); err != nil {
		return "", fmt.Errorf("JSON validation failed after repair: %w", err)
	}

	log.Printf("✅ JSON successfully repaired")
	return repairedJSON, nil
}

// --- Main ---

func main() {
	targetLanguage := flag.String("language", "", "Target language code (e.g., es, pt, fr)")
	reportPath := flag.String("report", "matching_report.txt", "Path for the report file")
	dryRun := flag.Bool("dry-run", false, "Don't update database, just show what would happen")
	useCLIFlag := flag.Bool("cli", false, "Use claude CLI instead of API (works with Claude Pro)")
	useGeminiFlag := flag.Bool("gemini", false, "Use Gemini CLI as fallback when Claude hits rate limits")
	useGptOssFlag := flag.Bool("gpt-oss", false, "Use ollama as local fallback (no rate limits)")
	useCompressedFlag := flag.Bool("compressed", false, "Use compressed fingerprint matching (90% fewer API calls)")
	useUltraCompressedFlag := flag.Bool("ultra", false, "Use ultra-compressed multi-language batching (97% fewer API calls)")
	useSmartFallbackFlag := flag.Bool("smart-fallback", false, "Use smart backend fallback (Claude→Gemini→ollama)")
	useStatusCheckFlag := flag.Bool("status", false, "Check database status and processing recommendations")
	useRetryBatchesFlag := flag.Bool("retry", false, "Retry processing saved batch files")
	csvFileFlag := flag.String("csv", "", "Process issues from CSV file (specify filename or leave empty for writings_issues.csv)")
	resolveAmbiguousFlag := flag.Bool("resolve-ambiguous", false, "Phase 2: Resolve ambiguous matches using full-text matching")
	skipProcessedFlag := flag.Bool("skip-processed", true, "Skip languages with existing review files (disable with -skip-processed=false)")
	reverseFlag := flag.Bool("reverse", false, "Process languages from smallest to largest (start with rare/small languages)")
	heuristicFlag := flag.Bool("heuristic", false, "Use heuristic pre-sorting to prioritize likely matches first")
	initTMPCodesFlag := flag.Bool("init-tmp", false, "Initialize TMP codes for unmatched en/ar/fa prayers")
	useTMPFallbackFlag := flag.Bool("use-tmp-fallback", false, "Enable three-tier matching: en -> ar -> fa -> new TMP")
	flag.Parse()

	// Check if no arguments were provided - show interactive menu
	if len(os.Args) == 1 {
		if err := ShowMainMenu(); err != nil {
			log.Fatalf("Interactive menu failed: %v", err)
		}
		return
	}

	useCLI = *useCLIFlag
	useGemini = *useGeminiFlag
	useGptOss = *useGptOssFlag
	useCompressed = *useCompressedFlag
	useUltraCompressed = *useUltraCompressedFlag
	useSmartFallback = *useSmartFallbackFlag
	useStatusCheck = *useStatusCheckFlag
	useRetryBatches = *useRetryBatchesFlag
	useCsvProcessing = (*csvFileFlag != "")

	// Set the CSV file if specified
	if useCsvProcessing && *csvFileFlag != "" {
		// Store the filename in a way we can access it later
		os.Setenv("CSV_FILENAME", *csvFileFlag)
	}
	resolveAmbiguous := *resolveAmbiguousFlag
	skipProcessed := *skipProcessedFlag
	reverse := *reverseFlag
	heuristic := *heuristicFlag
	initTMPCodes := *initTMPCodesFlag
	useTMPFallback := *useTMPFallbackFlag

	// Route to TMP code initialization if requested
	if initTMPCodes {
		log.Println("🏷️  Initializing TMP codes for unmatched en/ar/fa prayers...")
		if err := AssignTMPCodes(); err != nil {
			log.Fatalf("TMP code initialization failed: %v", err)
		}
		log.Println("✅ TMP code initialization completed successfully!")
		return
	}

	// Skip API key check for status-only commands and CSV processing
	if !useStatusCheck && !useRetryBatches && !useSmartFallback && !resolveAmbiguous && !useCsvProcessing {
		// Get API key from environment (only required if not using CLI)
		if !useCLI && !useGemini && !useGptOss {
			claudeAPIKey = os.Getenv("CLAUDE_API_KEY")
			if claudeAPIKey == "" {
				log.Fatal("CLAUDE_API_KEY environment variable must be set (or use -cli/-gemini/-gpt-oss flag)")
			}
		}
	}

	// For CSV processing, ensure at least one backend is available
	if useCsvProcessing {
		if !useCLI && !useGemini && !useGptOss {
			// Default to gemini if no backend specified for CSV processing
			useGemini = true
		}
	}

	// Check Gemini requirements
	if useGemini {
		if _, err := exec.LookPath("gemini"); err != nil {
			log.Fatal("gemini CLI not found. Please install it first and authenticate: gemini auth login")
		}
	}

	// Check ollama requirements
	if useGptOss {
		if _, err := exec.LookPath("ollama"); err != nil {
			log.Fatal("ollama CLI not found. Please install it first: https://ollama.com/")
		}
	}

	// Route to smart fallback if requested
	if useSmartFallback {
		if *targetLanguage != "" {
			log.Println("Note: -smart-fallback processes ALL languages, ignoring -language flag")
		}
		log.Printf("Starting SMART FALLBACK processing (Claude→Gemini→ollama)")
		if err := SmartFallbackProcessing(); err != nil {
			log.Fatalf("Smart fallback processing failed: %v", err)
		}
		log.Println("Smart fallback processing completed successfully!")
		return
	}

	// Route to status check if requested
	if useStatusCheck {
		if err := StatusCheckCommand(); err != nil {
			log.Fatalf("Status check failed: %v", err)
		}
		return
	}

	// Route to retry batches if requested
	if useRetryBatches {
		if err := RetryBatchesCommand(); err != nil {
			log.Fatalf("❌ Retry batches failed: %v", err)
		}
		return
	}

	if useCsvProcessing {
		if err := processCsvIssues(); err != nil {
			log.Fatalf("❌ CSV processing failed: %v", err)
		}
		return
	}

	// Route to resolve ambiguous if requested
	if resolveAmbiguous {
		if *targetLanguage == "" {
			log.Fatal("Error: -language flag is required for -resolve-ambiguous (e.g., -language=fa)")
		}
		if err := ResolveAmbiguousMatches(*targetLanguage); err != nil {
			log.Fatalf("Resolve ambiguous matches failed: %v", err)
		}
		return
	}

	// Route to ultra-compressed matching if requested (processes ALL languages)
	if useUltraCompressed {
		if *targetLanguage != "" {
			log.Println("Note: -ultra flag processes ALL languages, ignoring -language flag")
		}
		if useGptOss {
			log.Printf("Starting ULTRA-COMPRESSED multi-language batch matching with ollama (local)")
		} else if useGemini {
			log.Printf("Starting ULTRA-COMPRESSED multi-language batch matching with Gemini")
		} else {
			log.Printf("Starting ULTRA-COMPRESSED multi-language batch matching")
		}
		if err := UltraCompressedBulkMatchingWithSkip(skipProcessed, reverse, heuristic); err != nil {
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
		if useTMPFallback {
			log.Printf("Starting COMPRESSED matching with TMP FALLBACK for language: %s", *targetLanguage)
			if err := CompressedLanguageMatchingWithTMPFallback(*targetLanguage); err != nil {
				log.Fatalf("Compressed TMP fallback matching failed: %v", err)
			}
			log.Println("Compressed TMP fallback matching completed successfully!")
			return
		}

		if useGptOss {
			log.Printf("Starting COMPRESSED matching for language: %s (using ollama local)", *targetLanguage)
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

		// Call LLM with backend fallback
		log.Printf("Calling LLM for chunk %d/%d...", chunkIdx+1, totalChunks)
		response, err := callLLMWithBackendFallback(prompt, fmt.Sprintf("chunk %d/%d", chunkIdx+1, totalChunks), true)
		if err != nil {
			log.Fatalf("LLM call failed on chunk %d: %v", chunkIdx+1, err)
		}

		fmt.Fprintf(reportFile, "Claude response received (%d chars)\n", len(response))

		// Parse response using robust JSON extraction
		jsonStr, err := ExtractJSONFromResponse(response)
		if err != nil {
			log.Printf("Failed to extract JSON from chunk %d response: %v\nResponse: %s", chunkIdx+1, err, response)
			fmt.Fprintf(reportFile, "ERROR extracting JSON from chunk %d: %v\n", chunkIdx+1, err)
			continue
		}

		var chunkResults BatchMatchResponse
		if err := json.Unmarshal([]byte(jsonStr), &chunkResults); err != nil {
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

// --- Smart Fallback Processing ---

func GetAvailableBackends() []Backend {
	backends := []Backend{}

	// Check Claude CLI
	if _, err := exec.LookPath("claude"); err == nil {
		backends = append(backends, Backend{
			Name:      "Claude CLI",
			Available: true,
			Command:   "claude",
			Flag:      "-cli",
			Priority:  1,
		})
	}

	// Check Gemini CLI
	if _, err := exec.LookPath("gemini"); err == nil {
		backends = append(backends, Backend{
			Name:      "Gemini CLI",
			Available: true,
			Command:   "gemini",
			Flag:      "-gemini",
			Priority:  2,
		})
	}

	// Check ollama
	if _, err := exec.LookPath("ollama"); err == nil {
		backends = append(backends, Backend{
			Name:      "ollama",
			Available: true,
			Command:   "ollama",
			Flag:      "-gpt-oss",
			Priority:  3,
		})
	}

	return backends
}

func SmartFallbackProcessing() error {
	logFile = fmt.Sprintf("smart_fallback_%s.log", time.Now().Format("20060102_150405"))

	log.Printf("🧠 Smart Fallback Prayer Matching")
	log.Printf("=================================")
	log.Printf("Starting intelligent multi-backend processing")

	backends := GetAvailableBackends()
	if len(backends) == 0 {
		return fmt.Errorf("no backends available - install claude, gemini, or ollama")
	}

	log.Printf("🔍 Available backends:")
	for _, backend := range backends {
		log.Printf("  ✅ %s: Available", backend.Name)
	}

	success := false
	finalBackend := ""

	for _, backend := range backends {
		log.Printf("🔄 Attempting with %s...", backend.Name)

		// Set the appropriate backend flags
		switch backend.Name {
		case "Claude CLI":
			useCLI = true
			useGemini = false
			useGptOss = false
		case "Gemini CLI":
			useCLI = false
			useGemini = true
			useGptOss = false
		case "ollama":
			useCLI = false
			useGemini = false
			useGptOss = true
		}

		// Try ultra-compressed processing
		err := UltraCompressedBulkMatching()
		if err == nil {
			log.Printf("✅ SUCCESS with %s!", backend.Name)
			success = true
			finalBackend = backend.Name
			break
		} else {
			log.Printf("❌ FAILED with %s: %v", backend.Name, err)

			// Try processing saved batches if they exist
			if err := ProcessSavedBatches(backend); err != nil {
				log.Printf("  Also failed to process saved batches: %v", err)
			}

			time.Sleep(5 * time.Second) // Wait before trying next backend
		}
	}

	log.Printf("🏁 SMART FALLBACK PROCESSING COMPLETED")

	if success {
		log.Printf("🎉 SUCCESS with %s!", finalBackend)
		log.Printf("✅ Prayer database processing completed")
		return StatusCheckCommand()
	} else {
		log.Printf("❌ ALL BACKENDS FAILED")
		return fmt.Errorf("all backends failed - check logs for details")
	}
}

func ProcessSavedBatches(backend Backend) error {
	// Check for saved batch files
	remainingFiles, _ := filepath.Glob("remaining_batches_*.json")
	pendingFiles, _ := filepath.Glob("pending_batch_*.json")

	if len(remainingFiles) == 0 && len(pendingFiles) == 0 {
		log.Printf("  📭 No saved batches found")
		return nil
	}

	processed := 0
	failed := 0

	// Process pending batches
	for _, file := range pendingFiles {
		// Extract language from filename
		lang := strings.TrimPrefix(file, "pending_batch_")
		lang = strings.Split(lang, "_")[0]

		log.Printf("  🔄 Processing %s with %s...", lang, backend.Name)

		err := CompressedLanguageMatching(lang)
		if err == nil {
			log.Printf("  ✅ %s completed", lang)
			os.Rename(file, file+".processed")
			processed++
		} else {
			log.Printf("  ❌ %s failed", lang)
			failed++
		}

		time.Sleep(3 * time.Second)
	}

	log.Printf("  📊 Processed: %d, Failed: %d", processed, failed)
	return nil
}

// --- Status Check Command ---

func StatusCheckCommand() error {
	log.Printf("🔍 Prayer Matching Database Status Check")
	log.Printf("========================================")

	status, err := GetProcessingStatus()
	if err != nil {
		return fmt.Errorf("failed to get status: %v", err)
	}

	log.Printf("📊 Overall Statistics:")
	log.Printf("----------------------")
	log.Printf("  Total prayers: %d", status.TotalPrayers)
	log.Printf("  Total languages: %d", status.TotalLanguages)
	log.Printf("  Matched prayers: %d", status.MatchedPrayers)
	log.Printf("  Unmatched prayers: %d", status.UnmatchedPrayers)
	log.Printf("  Overall completion: %d%%", status.CompletionRate)

	// Get English reference status
	englishStatus, err := GetEnglishStatus()
	if err == nil {
		log.Printf("🎯 English Reference Status:")
		log.Printf("-----------------------------")
		log.Printf("  English prayers with Phelps codes: %d", englishStatus.MatchedPrayers)
		log.Printf("  English reference completion: %d%%", englishStatus.CompletionRate)
	}

	// Get top languages by prayer count
	if err := ShowTopLanguages(); err != nil {
		log.Printf("Warning: Could not show top languages: %v", err)
	}

	// Show unprocessed languages
	if err := ShowUnprocessedLanguages(); err != nil {
		log.Printf("Warning: Could not show unprocessed languages: %v", err)
	}

	// Show processing recommendations
	ShowProcessingRecommendations(status)

	log.Printf("🔄 Last Updated: %s", time.Now().Format(time.RFC3339))
	return nil
}

func GetProcessingStatus() (*ProcessingStatus, error) {
	status := &ProcessingStatus{}

	// Get total prayers
	if result, err := execDoltQueryCSV("SELECT COUNT(*) FROM writings"); err == nil && len(result) > 1 {
		fmt.Sscanf(result[1][0], "%d", &status.TotalPrayers)
	}

	// Get total languages
	if result, err := execDoltQueryCSV("SELECT COUNT(DISTINCT language) FROM writings"); err == nil && len(result) > 1 {
		fmt.Sscanf(result[1][0], "%d", &status.TotalLanguages)
	}

	// Get matched prayers
	if result, err := execDoltQueryCSV("SELECT COUNT(*) FROM writings WHERE phelps IS NOT NULL"); err == nil && len(result) > 1 {
		fmt.Sscanf(result[1][0], "%d", &status.MatchedPrayers)
	}

	// Get unmatched prayers
	if result, err := execDoltQueryCSV("SELECT COUNT(*) FROM writings WHERE phelps IS NULL"); err == nil && len(result) > 1 {
		fmt.Sscanf(result[1][0], "%d", &status.UnmatchedPrayers)
	}

	// Calculate completion rate
	if status.TotalPrayers > 0 {
		status.CompletionRate = (status.MatchedPrayers * 100) / status.TotalPrayers
	}

	// Get unprocessed languages count
	if result, err := execDoltQueryCSV("SELECT COUNT(DISTINCT language) FROM writings WHERE language != 'en' AND phelps IS NULL AND language NOT LIKE '%-translit'"); err == nil && len(result) > 1 {
		fmt.Sscanf(result[1][0], "%d", &status.UnprocessedLangs)
	}

	return status, nil
}

func GetEnglishStatus() (*ProcessingStatus, error) {
	status := &ProcessingStatus{}

	if result, err := execDoltQueryCSV("SELECT COUNT(*) FROM writings WHERE language = 'en'"); err == nil && len(result) > 1 {
		fmt.Sscanf(result[1][0], "%d", &status.TotalPrayers)
	}

	if result, err := execDoltQueryCSV("SELECT COUNT(*) FROM writings WHERE language = 'en' AND phelps IS NOT NULL"); err == nil && len(result) > 1 {
		fmt.Sscanf(result[1][0], "%d", &status.MatchedPrayers)
	}

	if status.TotalPrayers > 0 {
		status.CompletionRate = (status.MatchedPrayers * 100) / status.TotalPrayers
	}

	return status, nil
}

func ShowTopLanguages() error {
	log.Printf("📈 Top 20 Languages by Prayer Count:")
	log.Printf("------------------------------------")

	result, err := execDoltQueryCSV(`
		SELECT
			language,
			COUNT(*) as total_prayers,
			COUNT(phelps) as matched_prayers,
			ROUND(COUNT(phelps) * 100.0 / COUNT(*), 1) as match_percent
		FROM writings
		GROUP BY language
		HAVING COUNT(*) > 0
		ORDER BY total_prayers DESC
		LIMIT 20
	`)

	if err != nil {
		return err
	}

	for i := 1; i < len(result); i++ {
		lang := result[i][0]
		total := result[i][1]
		matched := result[i][2]
		percent := result[i][3]

		status := "🔄 PARTIAL"
		if matched == total {
			status = "✅ COMPLETE"
		} else if matched == "0" {
			status = "❌ UNPROCESSED"
		}

		log.Printf("  %s: %s total, %s matched (%s%%) %s", lang, total, matched, percent, status)
	}

	return nil
}

func ShowUnprocessedLanguages() error {
	log.Printf("🚨 Unprocessed Languages (need matching):")
	log.Printf("-----------------------------------------")

	result, err := execDoltQueryCSV(`
		SELECT language, COUNT(*) as prayer_count
		FROM writings
		WHERE language != 'en' AND phelps IS NULL AND language NOT LIKE '%-translit'
		GROUP BY language
		HAVING COUNT(*) > 0
		ORDER BY prayer_count DESC
	`)

	if err != nil {
		return err
	}

	if len(result) <= 1 {
		log.Printf("🎉 ALL LANGUAGES HAVE BEEN PROCESSED!")
		log.Printf("The prayer matching database is complete!")
		return nil
	}

	for i := 1; i < len(result); i++ {
		lang := result[i][0]
		count := result[i][1]
		log.Printf("  %s: %s prayers", lang, count)
	}

	log.Printf("Total unprocessed languages: %d", len(result)-1)
	return nil
}

func ShowProcessingRecommendations(status *ProcessingStatus) {
	log.Printf("🔧 Processing Recommendations:")
	log.Printf("------------------------------")

	if status.UnprocessedLangs == 0 {
		log.Printf("✅ Database is fully matched - no action needed!")
	} else if status.UnprocessedLangs <= 5 {
		log.Printf("🎯 Few languages remaining - process individually:")
		log.Printf("   ./prayer-matcher -language=XX -compressed -cli")
	} else if status.UnprocessedLangs <= 20 {
		log.Printf("⚡ Moderate number remaining - consider batch processing:")
		log.Printf("   ./prayer-matcher -ultra -cli")
	} else {
		log.Printf("🚀 Many languages remaining - run ultra-compressed processing:")
		log.Printf("   ./prayer-matcher -ultra -cli")
		estimatedCalls := (status.UnprocessedLangs + 4) / 5
		estimatedTime := status.UnprocessedLangs / 3
		log.Printf("   Estimated API calls needed: %d", estimatedCalls)
		log.Printf("   Estimated time: %d minutes", estimatedTime)
	}
}

// --- Retry Batches Command ---

func RetryBatchesCommand() error {
	log.Printf("🔄 Retry Saved Batches")
	log.Printf("=====================")

	// Check for saved batch files
	remainingFiles, _ := filepath.Glob("remaining_batches_*.json")
	pendingFiles, _ := filepath.Glob("pending_batch_*.json")
	failedResponseFiles, _ := filepath.Glob("failed_response_*.txt")

	if len(remainingFiles) == 0 && len(pendingFiles) == 0 && len(failedResponseFiles) == 0 {
		log.Printf("📭 No saved batch files or failed responses found")
		log.Printf("Saved batches are created when processing is interrupted by rate limits.")
		log.Printf("Run one of these first:")
		log.Printf("  ./prayer-matcher -ultra -cli")
		log.Printf("  ./prayer-matcher -smart-fallback")
		return nil
	}

	log.Printf("📁 Found saved files:")
	if len(remainingFiles) > 0 {
		log.Printf("  Remaining batches: %d", len(remainingFiles))
	}
	if len(pendingFiles) > 0 {
		log.Printf("  Pending batches: %d", len(pendingFiles))
	}
	if len(failedResponseFiles) > 0 {
		log.Printf("  Failed responses: %d", len(failedResponseFiles))
	}

	backends := GetAvailableBackends()
	if len(backends) == 0 {
		return fmt.Errorf("no backends available - install claude, gemini, or ollama")
	}

	log.Printf("🔧 Available backends:")
	for _, backend := range backends {
		log.Printf("  %s", backend.Name)
	}

	totalProcessed := 0
	totalFailed := 0

	// First, try to repair failed responses using LLM before processing
	if len(failedResponseFiles) > 0 {
		log.Printf("🔧 Attempting to repair failed responses with LLM...")
		if err := RepairAllFailedResponses(failedResponseFiles); err != nil {
			log.Printf("⚠️ LLM repair encountered issues: %v", err)
		}
	}

	// Then, try to reprocess failed responses with improved JSON extraction
	if len(failedResponseFiles) > 0 {
		log.Printf("🔧 Reprocessing failed responses with improved JSON parsing...")
		processed, failed := ProcessFailedResponses(failedResponseFiles)
		totalProcessed += processed
		totalFailed += failed
	}

	// Process pending batch files
	if len(pendingFiles) > 0 {
		log.Printf("🔄 Processing pending batches...")
		processed, failed := ProcessPendingBatches(pendingFiles, backends)
		totalProcessed += processed
		totalFailed += failed
	}

	// Process remaining batch files
	if len(remainingFiles) > 0 {
		log.Printf("🔄 Processing remaining batches...")
		processed, failed := ProcessRemainingBatches(remainingFiles, backends)
		totalProcessed += processed
		totalFailed += failed
	}

	// Final summary
	log.Printf("🏁 Retry completed!")
	log.Printf("  Languages successful: %d", totalProcessed)
	log.Printf("  Languages failed: %d", totalFailed)

	if totalProcessed > 0 {
		log.Printf("✅ Successfully processed %d languages", totalProcessed)
		StatusCheckCommand()
	}

	if totalFailed > 0 {
		log.Printf("⚠️ %d languages failed with all backends", totalFailed)
		log.Printf("These may need manual review or different approaches")
	}

	return nil
}

// ProcessFailedResponses attempts to reparse saved failed response files using the improved JSON extraction
func ProcessFailedResponses(files []string) (int, int) {
	processed := 0
	failed := 0

	for _, file := range files {
		log.Printf("📄 Reprocessing: %s", file)

		// Extract language from filename (e.g., failed_response_fa_1764287969.txt)
		lang := strings.TrimPrefix(file, "failed_response_")
		lang = strings.Split(lang, "_")[0]

		// Read the failed response
		responseData, err := os.ReadFile(file)
		if err != nil {
			log.Printf("❌ Could not read %s: %v", file, err)
			failed++
			continue
		}

		response := string(responseData)
		log.Printf("   Language: %s, Response size: %d bytes", lang, len(response))

		// Try to extract JSON using our improved function
		jsonStr, err := ExtractJSONFromResponse(response)
		if err != nil {
			log.Printf("❌ JSON extraction failed: %v", err)
			log.Printf("🔧 Attempting LLM-based JSON repair...")

			// Try to repair the JSON using an LLM
			jsonStr, err = RepairJSONWithLLM(response)
			if err != nil {
				log.Printf("❌ JSON repair also failed: %v", err)
				continue
			}
			log.Printf("✅ JSON successfully repaired!")
		}

		log.Printf("✅ JSON extracted successfully! Size: %d bytes", len(jsonStr))

		// Determine which type of response this is and process accordingly
		if strings.Contains(jsonStr, "\"matches\"") {
			// This looks like a compressed or ultra-compressed response
			if err := processCompressedResponse(lang, jsonStr); err != nil {
				log.Printf("❌ Failed to process compressed response: %v", err)
				log.Printf("🔧 Attempting LLM-based JSON repair for parsing failure...")

				// Try to repair the JSON using an LLM
				repairedJSON, repairErr := RepairJSONWithLLM(response)
				if repairErr != nil {
					log.Printf("❌ JSON repair also failed: %v", repairErr)
					failed++
					continue
				}
				log.Printf("✅ JSON successfully repaired for parsing!")

				// Try processing the repaired JSON
				if err := processCompressedResponse(lang, repairedJSON); err != nil {
					log.Printf("❌ Even repaired JSON failed to process: %v", err)
					failed++
					continue
				}
				log.Printf("✅ Repaired JSON processed successfully!")
			}
		} else {
			log.Printf("⚠️ Unknown response format, skipping")
			failed++
			continue
		}

		// If successful, delete the failed response file
		if err := os.Remove(file); err != nil {
			log.Printf("⚠️ Could not delete processed file %s: %v", file, err)
		} else {
			log.Printf("🗑️ Deleted processed file: %s", file)
		}

		processed++
		log.Printf("✅ Successfully reprocessed %s", lang)
	}

	return processed, failed
}

// generateReviewFiles creates detailed output files for matches needing human review
func generateReviewFiles(language string, matches []CompressedMatchResult) error {
	timestamp := time.Now().Format("20060102_150405")

	// Filter matches by confidence and type
	var ambiguous []CompressedMatchResult
	var lowConfidence []CompressedMatchResult
	var highConfidence []CompressedMatchResult
	var unmatched []CompressedMatchResult

	for _, match := range matches {
		switch {
		case match.MatchType == "AMBIGUOUS":
			ambiguous = append(ambiguous, match)
		case match.Confidence < 70:
			lowConfidence = append(lowConfidence, match)
		case match.Confidence >= 70:
			highConfidence = append(highConfidence, match)
		case match.MatchType == "NEW_TRANSLATION":
			unmatched = append(unmatched, match)
		}
	}

	// Generate ambiguous matches file
	if len(ambiguous) > 0 {
		filename := fmt.Sprintf("review_ambiguous_%s_%s.txt", language, timestamp)
		if err := writeReviewFile(filename, "AMBIGUOUS MATCHES", language, ambiguous); err != nil {
			return err
		}
		log.Printf("📝 Created ambiguous matches file: %s (%d items)", filename, len(ambiguous))
	}

	// Generate low confidence matches file
	if len(lowConfidence) > 0 {
		filename := fmt.Sprintf("review_low_confidence_%s_%s.txt", language, timestamp)
		if err := writeReviewFile(filename, "LOW CONFIDENCE MATCHES", language, lowConfidence); err != nil {
			return err
		}
		log.Printf("📝 Created low confidence file: %s (%d items)", filename, len(lowConfidence))
	}

	// Generate summary file
	summaryFile := fmt.Sprintf("review_summary_%s_%s.txt", language, timestamp)
	if err := writeSummaryFile(summaryFile, language, len(highConfidence), len(lowConfidence), len(ambiguous), len(unmatched)); err != nil {
		return err
	}
	log.Printf("📝 Created summary file: %s", summaryFile)

	return nil
}

// writeReviewFile creates a detailed review file for human inspection
func writeReviewFile(filename, title, language string, matches []CompressedMatchResult) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	fmt.Fprintf(file, "%s - %s\n", title, strings.ToUpper(language))
	fmt.Fprintf(file, "Generated: %s\n", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(file, "Total items: %d\n\n", len(matches))
	fmt.Fprintf(file, "INSTRUCTIONS:\n")
	fmt.Fprintf(file, "- Review each match below\n")
	fmt.Fprintf(file, "- Confidence ranges: 0.0 (no match) to 1.0 (perfect match)\n")
	fmt.Fprintf(file, "- For ambiguous matches, check the suggested reasons\n")
	fmt.Fprintf(file, "- Verify Phelps codes against English reference\n\n")
	fmt.Fprintf(file, "═══════════════════════════════════════════════════════════════\n\n")

	for i, match := range matches {
		fmt.Fprintf(file, "[%d/%d] Target Prayer: %s\n", i+1, len(matches), match.TargetVersion)
		fmt.Fprintf(file, "Suggested Phelps: %s\n", match.EnglishPhelps)
		fmt.Fprintf(file, "Match Type: %s\n", match.MatchType)
		fmt.Fprintf(file, "Confidence: %d%%\n", match.Confidence)

		if len(match.MatchReasons) > 0 {
			fmt.Fprintf(file, "Reasons: %s\n", strings.Join(match.MatchReasons, ", "))
		}

		if match.AmbiguityReason != "" {
			fmt.Fprintf(file, "Ambiguity: %s\n", match.AmbiguityReason)
		}

		fmt.Fprintf(file, "Action needed: [ ] APPROVE [ ] REJECT [ ] MODIFY\n")
		fmt.Fprintf(file, "Notes: ______________________________________\n")
		fmt.Fprintf(file, "───────────────────────────────────────────────────────────────\n\n")
	}

	return nil
}

// writeSummaryFile creates an overview of the matching results
func writeSummaryFile(filename, language string, high, low, ambiguous, unmatched int) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	total := high + low + ambiguous + unmatched

	fmt.Fprintf(file, "PRAYER MATCHING SUMMARY - %s\n", strings.ToUpper(language))
	fmt.Fprintf(file, "Generated: %s\n\n", time.Now().Format("2006-01-02 15:04:05"))

	fmt.Fprintf(file, "PROCESSING RESULTS:\n")
	fmt.Fprintf(file, "─────────────────\n")
	fmt.Fprintf(file, "Total prayers processed: %d\n", total)
	fmt.Fprintf(file, "High confidence matches: %d (%.1f%%)\n", high, float64(high)/float64(total)*100)
	fmt.Fprintf(file, "Low confidence matches:  %d (%.1f%%)\n", low, float64(low)/float64(total)*100)
	fmt.Fprintf(file, "Ambiguous matches:       %d (%.1f%%)\n", ambiguous, float64(ambiguous)/float64(total)*100)
	fmt.Fprintf(file, "New translations:        %d (%.1f%%)\n", unmatched, float64(unmatched)/float64(total)*100)
	fmt.Fprintf(file, "\n")

	fmt.Fprintf(file, "ACTION ITEMS:\n")
	fmt.Fprintf(file, "────────────\n")
	if low > 0 {
		fmt.Fprintf(file, "□ Review low confidence matches in: review_low_confidence_%s_*.txt\n", language)
	}
	if ambiguous > 0 {
		fmt.Fprintf(file, "□ Resolve ambiguous matches in: review_ambiguous_%s_*.txt\n", language)
	}
	if unmatched > 0 {
		fmt.Fprintf(file, "□ %d prayers identified as new translations (no English equivalent)\n", unmatched)
	}
	if high > 0 {
		fmt.Fprintf(file, "□ %d high confidence matches applied automatically\n", high)
	}

	fmt.Fprintf(file, "\nNext steps: Review files, make corrections, then run database updates.\n")

	return nil
}

// processCompressedResponse handles parsing and applying compressed match results
func processCompressedResponse(language, jsonStr string) error {
	// Try parsing as CompressedBatchResponse first
	var compressedResults CompressedBatchResponse
	if err := json.Unmarshal([]byte(jsonStr), &compressedResults); err == nil {
		return applyCompressedMatches(language, compressedResults)
	}

	// Try parsing as UltraBatchResponse
	var ultraResults UltraBatchResponse
	if err := json.Unmarshal([]byte(jsonStr), &ultraResults); err == nil {
		return applyUltraMatches(ultraResults)
	}

	// Try parsing as regular BatchMatchResponse
	var regularResults BatchMatchResponse
	if err := json.Unmarshal([]byte(jsonStr), &regularResults); err == nil {
		return applyRegularMatches(language, regularResults)
	}

	return fmt.Errorf("could not parse response as any known format")
}

// RepairAllFailedResponses proactively repairs all failed response files using LLM
func RepairAllFailedResponses(failedFiles []string) error {
	log.Printf("🔧 Attempting to repair %d failed response files...", len(failedFiles))

	repaired := 0
	for _, file := range failedFiles {
		log.Printf("📄 Repairing: %s", file)

		// Read the failed response
		responseData, err := os.ReadFile(file)
		if err != nil {
			log.Printf("❌ Could not read %s: %v", file, err)
			continue
		}

		response := string(responseData)
		log.Printf("   Response size: %d bytes", len(response))

		// Try LLM-based repair
		repairedJSON, err := RepairJSONWithLLM(response)
		if err != nil {
			log.Printf("❌ LLM repair failed for %s: %v", file, err)
			continue
		}

		// Create repaired filename
		repairedFile := strings.Replace(file, "failed_response_", "repaired_response_", 1)

		// Write the repaired JSON
		if err := os.WriteFile(repairedFile, []byte(repairedJSON), 0644); err != nil {
			log.Printf("❌ Could not write repaired file %s: %v", repairedFile, err)
			continue
		}

		// Replace the original failed file with the repaired one
		if err := os.Rename(repairedFile, file); err != nil {
			log.Printf("❌ Could not replace original file %s: %v", file, err)
			continue
		}

		log.Printf("✅ Successfully repaired: %s", file)
		repaired++
	}

	log.Printf("🎯 Repair summary: %d/%d files successfully repaired", repaired, len(failedFiles))
	return nil
}

// validateMatchLanguages checks that all matches belong to the expected language
// and filters out any that don't, returning the validated list and count of invalid matches
func validateMatchLanguages(expectedLang string, matches []CompressedMatchResult) ([]CompressedMatchResult, int) {
	if len(matches) == 0 {
		return matches, 0
	}

	// Get database to check actual languages
	db, err := GetDatabase()
	if err != nil {
		log.Printf("⚠️ Failed to load database for validation, skipping language check: %v", err)
		return matches, 0
	}

	// Build map of version -> actual language
	versionToLang := make(map[string]string)
	for _, w := range db.Writings {
		versionToLang[w.Version] = w.Language
	}

	// Filter matches
	var validMatches []CompressedMatchResult
	invalidCount := 0

	for _, match := range matches {
		actualLang, exists := versionToLang[match.TargetVersion]
		if !exists {
			log.Printf("⚠️ Warning: UUID %s not found in database", match.TargetVersion)
			invalidCount++
			continue
		}

		if actualLang != expectedLang {
			log.Printf("⚠️ Language mismatch: UUID %s is %s, expected %s (Phelps: %s)",
				match.TargetVersion, actualLang, expectedLang, match.EnglishPhelps)
			invalidCount++
			continue
		}

		// Add language to match for consistency
		match.TargetLanguage = actualLang
		validMatches = append(validMatches, match)
	}

	return validMatches, invalidCount
}

// Helper functions for applying different types of matches
func applyCompressedMatches(language string, results CompressedBatchResponse) error {
	log.Printf("🔄 Applying %d compressed matches for %s", len(results.Matches), language)

	// Validate and filter matches to ensure they belong to the correct language
	validatedMatches, invalidCount := validateMatchLanguages(language, results.Matches)
	if invalidCount > 0 {
		log.Printf("⚠️ Filtered out %d matches with incorrect language (expected %s)", invalidCount, language)
	}

	// Generate review files for matches needing human review
	if err := generateReviewFiles(language, validatedMatches); err != nil {
		log.Printf("⚠️ Failed to generate review files: %v", err)
	}

	// Apply only high confidence matches automatically
	applied := 0
	for _, match := range validatedMatches {
		if match.EnglishPhelps != "" && match.TargetVersion != "" &&
			match.MatchType != "AMBIGUOUS" && match.Confidence >= 70 {
			// Update database with the match
			query := fmt.Sprintf(`UPDATE writings SET phelps = '%s' WHERE version = '%s' AND language = '%s' AND phelps IS NULL`,
				match.EnglishPhelps, match.TargetVersion, language)

			cmd := execDoltCommand("sql", "-q", query)
			if output, err := cmd.CombinedOutput(); err != nil {
				log.Printf("⚠️ Failed to update %s: %v: %s", match.TargetVersion, err, string(output))
			} else {
				applied++
			}
		}
	}

	log.Printf("✅ Applied %d high confidence matches automatically", applied)
	log.Printf("📝 Review files generated for manual verification")

	return nil
}

func applyUltraMatches(results UltraBatchResponse) error {
	log.Printf("🔄 Applying %d ultra-compressed matches", len(results.Matches))

	for _, match := range results.Matches {
		if match.EnglishPhelps != "" && match.TargetVersion != "" {
			// Extract language from the match context or determine it another way
			// For now, we'll need to look up the language based on the version
			query := fmt.Sprintf(`UPDATE writings SET phelps = '%s' WHERE version = '%s' AND phelps IS NULL`,
				match.EnglishPhelps, match.TargetVersion)

			cmd := execDoltCommand("sql", "-q", query)
			if output, err := cmd.CombinedOutput(); err != nil {
				log.Printf("⚠️ Failed to update %s: %v: %s", match.TargetVersion, err, string(output))
			}
		}
	}

	return nil
}

func applyRegularMatches(language string, results BatchMatchResponse) error {
	log.Printf("🔄 Applying %d regular matches for %s", len(results.Matches), language)

	for _, match := range results.Matches {
		if match.Phelps != "" && match.TargetVersion != "" {
			query := fmt.Sprintf(`UPDATE writings SET phelps = '%s' WHERE version = '%s' AND language = '%s' AND phelps IS NULL`,
				match.Phelps, match.TargetVersion, language)

			cmd := execDoltCommand("sql", "-q", query)
			if output, err := cmd.CombinedOutput(); err != nil {
				log.Printf("⚠️ Failed to update %s: %v: %s", match.TargetVersion, err, string(output))
			}
		}
	}

	return nil
}

func ProcessPendingBatches(files []string, backends []Backend) (int, int) {
	processed := 0
	failed := 0

	for _, file := range files {
		log.Printf("📄 Processing: %s", file)

		// Extract language from filename
		lang := strings.TrimPrefix(file, "pending_batch_")
		lang = strings.Split(lang, "_")[0]

		if !isValidLanguageCode(lang) {
			log.Printf("  ⚠️ Invalid language code: %s", lang)
			continue
		}

		log.Printf("  Language: %s", lang)

		if ProcessLanguageWithFallback(lang, backends) {
			processed++
			os.Rename(file, file+".processed")
			log.Printf("  📁 Moved to %s.processed", file)
		} else {
			failed++
		}
	}

	return processed, failed
}

func ProcessRemainingBatches(files []string, backends []Backend) (int, int) {
	processed := 0
	failed := 0

	for _, file := range files {
		log.Printf("📄 Processing: %s", file)

		languages, err := ExtractLanguagesFromBatch(file)
		if err != nil {
			log.Printf("  ⚠️ Could not extract languages from %s: %v", file, err)
			continue
		}

		if len(languages) == 0 {
			log.Printf("  ⚠️ No valid languages found in %s", file)
			continue
		}

		log.Printf("  Languages: %v", languages[:min(5, len(languages))])

		for _, lang := range languages {
			if isValidLanguageCode(lang) {
				log.Printf("  Processing: %s", lang)

				if ProcessLanguageWithFallback(lang, backends) {
					processed++
				} else {
					failed++
				}
			}
		}

		os.Rename(file, file+".processed")
		log.Printf("  📁 Moved to %s.processed", file)
	}

	return processed, failed
}

func ProcessLanguageWithFallback(lang string, backends []Backend) bool {
	for _, backend := range backends {
		log.Printf("    🔄 Trying %s...", backend.Name)

		// Set backend flags
		switch backend.Name {
		case "Claude CLI":
			useCLI = true
			useGemini = false
			useGptOss = false
		case "Gemini CLI":
			useCLI = false
			useGemini = true
			useGptOss = false
		case "ollama":
			useCLI = false
			useGemini = false
			useGptOss = true
		}

		if err := CompressedLanguageMatching(lang); err == nil {
			log.Printf("    ✅ Success with %s", backend.Name)
			return true
		} else {
			log.Printf("    ❌ Failed with %s", backend.Name)
		}

		time.Sleep(2 * time.Second)
	}

	log.Printf("    💔 All backends failed for %s", lang)
	return false
}

func ExtractLanguagesFromBatch(filename string) ([]string, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	// Simple regex to find language codes in JSON
	re := regexp.MustCompile(`"([a-z]{2,3}(?:-[a-z]+)?)"`)
	matches := re.FindAllStringSubmatch(string(data), -1)

	languageSet := make(map[string]bool)
	var languages []string

	for _, match := range matches {
		if len(match) > 1 {
			lang := match[1]
			if isValidLanguageCode(lang) && !languageSet[lang] {
				languageSet[lang] = true
				languages = append(languages, lang)

				// Safety limit
				if len(languages) >= 20 {
					break
				}
			}
		}
	}

	return languages, nil
}

func isValidLanguageCode(lang string) bool {
	// Basic validation for language codes
	if len(lang) < 2 || len(lang) > 10 {
		return false
	}

	// Check if it matches language pattern
	matched, _ := regexp.MatchString(`^[a-z]{2,3}(-[a-z]+)?$`, lang)
	if !matched {
		return false
	}

	// Exclude common non-language strings
	excludes := []string{"language", "phelps", "version", "type", "status", "created", "summary"}
	for _, exclude := range excludes {
		if lang == exclude {
			return false
		}
	}

	return true
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ResolveAmbiguousMatches implements Phase 2 full-text matching for ambiguous results
func ResolveAmbiguousMatches(language string) error {
	log.Printf("Starting Phase 2 ambiguous resolution for language: %s", language)

	// Find review files for this language
	reviewFiles, err := filepath.Glob(fmt.Sprintf("review_*_%s_*.txt", language))
	if err != nil {
		return fmt.Errorf("failed to find review files: %v", err)
	}

	if len(reviewFiles) == 0 {
		return fmt.Errorf("no review files found for language %s. Run compressed matching first to generate review files", language)
	}

	log.Printf("Found %d review files for %s. Phase 2 resolution is a placeholder - implement full-text matching here", len(reviewFiles), language)
	return nil
}

// --- CSV Issue Processing Functions ---

type IssueRecord struct {
	Version string
	Issue   string
}

func processCsvIssues() error {
	fmt.Println("\n📋 Process CSV Issue List")
	fmt.Println("═════════════════════════")

	// Default CSV file
	csvFile := "writings_issues.csv"

	// Check if a custom CSV file was specified via environment (set during flag parsing)
	if envFile := os.Getenv("CSV_FILENAME"); envFile != "" {
		csvFile = envFile
	}

	issues, err := loadIssuesFromCSV(csvFile)
	if err != nil {
		return fmt.Errorf("failed to load CSV: %w", err)
	}

	fmt.Printf("Loaded %d issues from %s\n\n", len(issues), csvFile)

	// Categorize issues
	missingPhelps := []IssueRecord{}
	duplicates := []IssueRecord{}
	other := []IssueRecord{}

	for _, issue := range issues {
		if issue.Issue == "Missing phelps code" {
			missingPhelps = append(missingPhelps, issue)
		} else if strings.Contains(issue.Issue, "Duplicate language") {
			duplicates = append(duplicates, issue)
		} else {
			other = append(other, issue)
		}
	}

	fmt.Printf("Issue breakdown:\n")
	fmt.Printf("  - Missing phelps codes: %d\n", len(missingPhelps))
	fmt.Printf("  - Duplicate languages: %d\n", len(duplicates))
	fmt.Printf("  - Other issues: %d\n", len(other))
	fmt.Println()

	// Auto-process missing phelps codes first
	if len(missingPhelps) > 0 {
		fmt.Printf("🔧 Processing %d missing phelps codes...\n", len(missingPhelps))
		if err := fixMissingPhelpsFromCSV(missingPhelps); err != nil {
			return fmt.Errorf("failed to fix missing phelps codes: %w", err)
		}
	}

	// Then process duplicates
	if len(duplicates) > 0 {
		fmt.Printf("🔧 Processing %d duplicate language issues...\n", len(duplicates))
		if err := fixDuplicatesFromCSV(duplicates); err != nil {
			return fmt.Errorf("failed to fix duplicates: %w", err)
		}
	}

	if len(other) > 0 {
		fmt.Printf("⚠️ Found %d other issues that need manual review\n", len(other))
		for _, issue := range other {
			fmt.Printf("  - %s: %s\n", issue.Version, issue.Issue)
		}
	}

	return nil
}

func loadIssuesFromCSV(filename string) ([]IssueRecord, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("cannot open file: %w", err)
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.LazyQuotes = true
	reader.TrimLeadingSpace = true

	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("failed to parse CSV: %w", err)
	}

	if len(records) < 2 {
		return nil, fmt.Errorf("CSV file has no data rows")
	}

	var issues []IssueRecord
	for i, record := range records[1:] { // Skip header
		if len(record) < 2 {
			fmt.Printf("Warning: skipping malformed row %d\n", i+2)
			continue
		}
		issues = append(issues, IssueRecord{
			Version: record[0],
			Issue:   record[1],
		})
	}

	return issues, nil
}

func fixMissingPhelpsFromCSV(issues []IssueRecord) error {
	if len(issues) == 0 {
		return nil
	}

	// Load database to get language information for each version
	db, err := GetDatabase()
	if err != nil {
		return fmt.Errorf("failed to load database: %w", err)
	}

	// Group versions by language
	langToVersions := make(map[string][]string)
	for _, issue := range issues {
		for _, writing := range db.Writings {
			if writing.Version == issue.Version {
				langToVersions[writing.Language] = append(langToVersions[writing.Language], issue.Version)
				break
			}
		}
	}

	fmt.Printf("Found issues in %d languages\n", len(langToVersions))

	// Process each language
	for language, versions := range langToVersions {
		fmt.Printf("\n📝 Processing %s (%d prayers)...\n", language, len(versions))

		if err := processLanguageVersions(db, language, versions); err != nil {
			fmt.Printf("Error processing %s: %v\n", language, err)
			continue
		}
	}

	fmt.Println("\n✅ Missing phelps code fixing completed")
	return nil
}

func fixDuplicatesFromCSV(issues []IssueRecord) error {
	if len(issues) == 0 {
		return nil
	}

	// Load database
	db, err := GetDatabase()
	if err != nil {
		return fmt.Errorf("failed to load database: %w", err)
	}

	// Group by language for targeted cleanup
	processedLanguages := make(map[string]bool)

	for _, issue := range issues {
		// Extract language from duplicate issue description
		parts := strings.Fields(issue.Issue)
		if len(parts) < 6 {
			fmt.Printf("Warning: cannot parse duplicate issue: %s\n", issue.Issue)
			continue
		}

		language := parts[2]
		if processedLanguages[language] {
			continue // Already processed this language
		}
		processedLanguages[language] = true

		fmt.Printf("🔍 Resolving duplicates for language %s\n", language)

		if err := cleanupDuplicatePhelpsIDs(db, language); err != nil {
			fmt.Printf("Error cleaning up %s: %v\n", language, err)
		}
	}

	fmt.Println("\n✅ Duplicate language fixing completed")
	return nil
}

func processLanguageVersions(db Database, language string, versions []string) error {
	// Create target prayers from the specific versions we need to process
	var targetPrayers []TargetPrayer
	for _, writing := range db.Writings {
		if writing.Language == language {
			for _, version := range versions {
				if writing.Version == version {
					targetPrayers = append(targetPrayers, TargetPrayer{
						Version: writing.Version,
						Name:    writing.Name,
						Text:    writing.Text,
						Link:    writing.Link,
						Source:  writing.Source,
					})
					break
				}
			}
		}
	}

	if len(targetPrayers) == 0 {
		return fmt.Errorf("no prayers found for specified versions")
	}

	// Use existing matching logic
	englishRefs := BuildEnglishReference(db)

	// Process in chunks
	chunkSize := 20
	for i := 0; i < len(targetPrayers); i += chunkSize {
		end := i + chunkSize
		if end > len(targetPrayers) {
			end = len(targetPrayers)
		}

		chunk := targetPrayers[i:end]
		fmt.Printf("  Processing chunk %d-%d of %d...\n", i+1, end, len(targetPrayers))

		if err := processCSVChunk(chunk, englishRefs, language); err != nil {
			return fmt.Errorf("failed to process chunk: %w", err)
		}
	}

	return nil
}

func processCSVChunk(prayers []TargetPrayer, englishRefs []EnglishReference, language string) error {
	chunkInfo := fmt.Sprintf("Processing %d prayers for %s from CSV issues", len(prayers), language)
	prompt := CreateMatchingPrompt(englishRefs, prayers, language, chunkInfo)

	response, err := callLLMWithBackendFallback(prompt, "CSV issue fixing", true)
	if err != nil {
		return fmt.Errorf("LLM call failed: %w", err)
	}

	// Extract JSON response and parse it
	jsonResponse, err := ExtractJSONFromResponse(response)
	if err != nil {
		return fmt.Errorf("failed to extract JSON: %w", err)
	}

	// Parse the JSON response into BatchMatchResponse
	var batchResponse BatchMatchResponse
	if err := json.Unmarshal([]byte(jsonResponse), &batchResponse); err != nil {
		return fmt.Errorf("failed to parse JSON response: %w", err)
	}

	// Apply matches directly to database
	return applyCsvMatches(batchResponse, language)
}

func applyCsvMatches(results BatchMatchResponse, language string) error {
	fmt.Printf("🔄 Applying %d matches for %s from CSV processing\n", len(results.Matches), language)

	for _, match := range results.Matches {
		if match.Phelps != "" && match.TargetVersion != "" {
			query := fmt.Sprintf(`UPDATE writings SET phelps = '%s' WHERE version = '%s' AND language = '%s' AND phelps IS NULL`,
				match.Phelps, match.TargetVersion, language)

			cmd := execDoltCommand("sql", "-q", query)
			if output, err := cmd.CombinedOutput(); err != nil {
				fmt.Printf("⚠️ Failed to update %s: %v: %s\n", match.TargetVersion, err, string(output))
			} else {
				fmt.Printf("✅ Updated %s -> %s\n", match.TargetVersion, match.Phelps)
			}
		}
	}

	return nil
}
