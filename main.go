package main

import (
	"bufio"
	"encoding/csv"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

var OllamaModel string = "gpt-oss"
var stopRequested int32 // Atomic flag for graceful stop

// Helper function to shell out to Ollama
func CallOllama(prompt string) (string, error) {
	cmd := exec.Command("ollama", "run", OllamaModel)
	cmd.Stdin = strings.NewReader(prompt)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("error running ollama with model %s: %w\nOutput: %s", OllamaModel, err, string(output))
	}
	return string(output), nil
}

// Helper function to shell out to Gemini CLI
func CallGemini(prompt string) (string, error) {
	cmd := exec.Command("gemini", prompt)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("error running gemini: %w", err)
	}
	return string(output), nil
}

// LLMResponse represents the parsed response from an LLM
type LLMResponse struct {
	PhelpsCode string
	Confidence float64
	Reasoning  string
}

// LLMCaller interface allows dependency injection for testing
type LLMCaller interface {
	CallGemini(prompt string) (string, error)
	CallOllama(prompt string) (string, error)
}

// DefaultLLMCaller implements LLMCaller using the actual CLI tools
type DefaultLLMCaller struct{}

func (d DefaultLLMCaller) CallGemini(prompt string) (string, error) {
	return CallGemini(prompt)
}

func (d DefaultLLMCaller) CallOllama(prompt string) (string, error) {
	return CallOllama(prompt)
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

// Prepare header for LLM calls with all known Phelps codes
func prepareLLMHeader(db Database, targetLanguage, referenceLanguage string) string {
	if targetLanguage == "" {
		targetLanguage = "English"
	}
	if referenceLanguage == "" {
		referenceLanguage = "English"
	}

	// Get all known Phelps codes with their names from the reference language
	phelpsData := make(map[string]string) // phelps -> name
	for _, writing := range db.Writing {
		if writing.Phelps != "" && writing.Language == referenceLanguage {
			if existing, exists := phelpsData[writing.Phelps]; !exists || len(writing.Name) > len(existing) {
				phelpsData[writing.Phelps] = writing.Name
			}
		}
	}

	// If no reference language data, fall back to any language
	if len(phelpsData) == 0 {
		for _, writing := range db.Writing {
			if writing.Phelps != "" {
				if existing, exists := phelpsData[writing.Phelps]; !exists || len(writing.Name) > len(existing) {
					phelpsData[writing.Phelps] = writing.Name
				}
			}
		}
	}

	var phelpsInfo []string
	for phelps, name := range phelpsData {
		if name != "" {
			phelpsInfo = append(phelpsInfo, fmt.Sprintf("%s (%s)", phelps, name))
		} else {
			phelpsInfo = append(phelpsInfo, phelps)
		}
	}
	sort.Strings(phelpsInfo)

	header := fmt.Sprintf(`You are an expert in Bahá'í writings and prayers. Your task is to match a prayer text in %s to known Phelps codes.

Known Phelps codes with their names (reference: %s):
%s

Instructions:
1. Analyze the provided prayer text in %s
2. Match it to the most appropriate Phelps code from the list above
3. Provide a confidence score (0-100%%)
4. Give your reasoning

Response format:
Phelps: [CODE]
Confidence: [PERCENTAGE]
Reasoning: [Your explanation]

If you cannot find a match with reasonable confidence (>70%%), respond with:
Phelps: UNKNOWN
Confidence: 0
Reasoning: [Explanation of why no match was found]

`, targetLanguage, referenceLanguage, strings.Join(phelpsInfo, ", "), targetLanguage)

	return header
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
func findOptimalDefaultLanguage(db Database) string {
	missing := calculateMissingPrayersPerLanguage(db)

	if len(missing) == 0 {
		return "en" // fallback to English
	}

	// Priority languages (more likely to have good LLM support)
	priorityLangs := map[string]bool{
		"en": true, "es": true, "fr": true, "de": true, "it": true,
		"pt": true, "ru": true, "ja": true, "zh": true, "ar": true,
		"fa": true, "tr": true, "hi": true, "ko": true,
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
		cmd := exec.Command("dolt", "add", ".")
		if output, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(reportFile, "  ERROR: Failed to stage interactive changes: %v: %s\n", err, string(output))
		} else {
			cmd = exec.Command("dolt", "commit", "-m", commitMessage)
			if output, err := cmd.CombinedOutput(); err != nil {
				fmt.Fprintf(reportFile, "  ERROR: Failed to commit interactive changes: %v: %s\n", err, string(output))
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
func callLLM(prompt string, useGemini bool) (LLMResponse, error) {
	return callLLMWithCaller(prompt, useGemini, DefaultLLMCaller{})
}

// callLLMWithCaller allows dependency injection for testing
func callLLMWithCaller(prompt string, useGemini bool, caller LLMCaller) (LLMResponse, error) {
	var response string
	var geminiErr error
	var ollamaErr error
	var geminiResponse string
	var ollamaResponse string
	var triedGemini bool
	var triedOllama bool

	if useGemini {
		triedGemini = true
		response, geminiErr = caller.CallGemini(prompt)
		if geminiErr != nil {
			log.Printf("Gemini call failed with error, falling back to Ollama: %v", geminiErr)
		} else {
			geminiResponse = response
			parsed := parseLLMResponse(response)
			// Check if Gemini response is valid
			if parsed.PhelpsCode != "" {
				log.Printf("Gemini returned valid response")
				return parsed, nil
			}
			log.Printf("Gemini returned empty/invalid response (PhelpsCode empty), falling back to Ollama")
			log.Printf("Gemini raw response: %q", response)
		}

		// Try Ollama as fallback
		triedOllama = true
		response, ollamaErr = caller.CallOllama(prompt)
		if ollamaErr != nil {
			// Both failed with errors
			return LLMResponse{}, fmt.Errorf("both LLM services failed - Gemini error: %v, Ollama error: %v", geminiErr, ollamaErr)
		}
		ollamaResponse = response
	} else {
		triedOllama = true
		response, ollamaErr = caller.CallOllama(prompt)
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
				debugInfo.WriteString(fmt.Sprintf("Gemini raw response: %q\n", geminiResponse))
			}
		}
		if triedOllama {
			debugInfo.WriteString(fmt.Sprintf("Ollama attempted: %v\n", ollamaErr == nil))
			if ollamaErr != nil {
				debugInfo.WriteString(fmt.Sprintf("Ollama error: %v\n", ollamaErr))
			} else {
				debugInfo.WriteString(fmt.Sprintf("Ollama raw response: %q\n", ollamaResponse))
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

type PrayerHeuristic struct {
	ID              int
	PhelpsCode      string
	PrayerName      string
	LanguagePattern string
	TextPattern     string
	PatternType     string
	ConfidenceLevel string
	Notes           string
	CreatedDate     string
	Validated       bool
	MatchCount      int
}

func parsePrayerHeuristic(line string) (PrayerHeuristic, error) {
	r := csv.NewReader(strings.NewReader(line))
	r.FieldsPerRecord = 11
	rec, err := r.Read()
	if err != nil {
		return PrayerHeuristic{}, err
	}
	return PrayerHeuristic{
		ID:              MustInt(rec[0]),
		PhelpsCode:      rec[1],
		PrayerName:      rec[2],
		LanguagePattern: rec[3],
		TextPattern:     rec[4],
		PatternType:     rec[5],
		ConfidenceLevel: rec[6],
		Notes:           rec[7],
		CreatedDate:     rec[8],
		Validated:       rec[9] == "true",
		MatchCount:      MustInt(rec[10]),
	}, nil
}

type PrayerMatchCandidate struct {
	ID               int
	VersionID        string
	ProposedName     string
	ProposedPhelps   string
	Language         string
	TextLength       int
	ReferenceLength  int
	LengthRatio      float64
	ConfidenceScore  float64
	ValidationStatus string
	ValidationNotes  string
	CreatedDate      string
}

func parsePrayerMatchCandidate(line string) (PrayerMatchCandidate, error) {
	r := csv.NewReader(strings.NewReader(line))
	r.FieldsPerRecord = 12
	rec, err := r.Read()
	if err != nil {
		return PrayerMatchCandidate{}, err
	}
	return PrayerMatchCandidate{
		ID:               MustInt(rec[0]),
		VersionID:        rec[1],
		ProposedName:     rec[2],
		ProposedPhelps:   rec[3],
		Language:         rec[4],
		TextLength:       MustInt(rec[5]),
		ReferenceLength:  MustInt(rec[6]),
		LengthRatio:      MustFloat(rec[7]),
		ConfidenceScore:  MustFloat(rec[8]),
		ValidationStatus: rec[9],
		ValidationNotes:  rec[10],
		CreatedDate:      rec[11],
	}, nil
}

type Database struct {
	Writing              []Writing
	Language             []Language
	PrayerHeuristic      []PrayerHeuristic
	PrayerMatchCandidate []PrayerMatchCandidate
	Skipped              map[string]int
}

func GetDatabase() Database {
	// Shell out to Dolt database and read in the data to populate the in-memory database
	db := Database{
		Writing:              []Writing{},
		Language:             []Language{},
		PrayerHeuristic:      []PrayerHeuristic{},
		PrayerMatchCandidate: []PrayerMatchCandidate{},
		Skipped:              make(map[string]int),
	}

	// Helper to run a dolt query and return the resulting CSV output
	runQuery := func(table string, columns string) (string, error) {
		cmd := exec.Command("dolt", "sql", "-q", fmt.Sprintf("SELECT %s FROM %s", columns, table), "-r", "csv")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("dolt query for %s failed: %w: %s", table, err, string(out))
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

	// Load PrayerHeuristic data
	if csvOut, err := runQuery("prayer_heuristics", "id,phelps_code,prayer_name,language_pattern,text_pattern,pattern_type,confidence_level,notes,created_date,validated,match_count"); err != nil {
		log.Fatalf("Failed to load prayer heuristic data: %v", err)
	} else {
		r := csv.NewReader(strings.NewReader(csvOut))
		r.FieldsPerRecord = 11
		records, err := r.ReadAll()
		if err != nil {
			log.Fatalf("Failed to parse prayer heuristic CSV: %v", err)
		}
		if len(records) > 0 {
			records = records[1:]
		}
		for _, rec := range records {
			ph := PrayerHeuristic{
				ID:              MustInt(rec[0]),
				PhelpsCode:      rec[1],
				PrayerName:      rec[2],
				LanguagePattern: rec[3],
				TextPattern:     rec[4],
				PatternType:     rec[5],
				ConfidenceLevel: rec[6],
				Notes:           rec[7],
				CreatedDate:     rec[8],
				Validated:       rec[9] == "true",
				MatchCount:      MustInt(rec[10]),
			}
			db.PrayerHeuristic = append(db.PrayerHeuristic, ph)
		}
	}

	// Load PrayerMatchCandidate data
	if csvOut, err := runQuery("prayer_match_candidates", "id,version_id,proposed_name,proposed_phelps,language,text_length,reference_length,length_ratio,confidence_score,validation_status,validation_notes,created_date"); err != nil {
		log.Fatalf("Failed to load prayer match candidate data: %v", err)
	} else {
		r := csv.NewReader(strings.NewReader(csvOut))
		r.FieldsPerRecord = 12
		records, err := r.ReadAll()
		if err != nil {
			log.Fatalf("Failed to parse prayer match candidate CSV: %v", err)
		}
		if len(records) > 0 {
			records = records[1:]
		}
		for _, rec := range records {
			pmc := PrayerMatchCandidate{
				ID:               MustInt(rec[0]),
				VersionID:        rec[1],
				ProposedName:     rec[2],
				ProposedPhelps:   rec[3],
				Language:         rec[4],
				TextLength:       MustInt(rec[5]),
				ReferenceLength:  MustInt(rec[6]),
				LengthRatio:      MustFloat(rec[7]),
				ConfidenceScore:  MustFloat(rec[8]),
				ValidationStatus: rec[9],
				ValidationNotes:  rec[10],
				CreatedDate:      rec[11],
			}
			db.PrayerMatchCandidate = append(db.PrayerMatchCandidate, pmc)
		}
	}

	return db
}

// Insert or update prayer match candidate
func insertPrayerMatchCandidate(db *Database, candidate PrayerMatchCandidate) error {
	// Add to in-memory database
	db.PrayerMatchCandidate = append(db.PrayerMatchCandidate, candidate)

	// Escape strings for SQL injection prevention
	escapedVersionID := strings.ReplaceAll(candidate.VersionID, "'", "''")
	escapedName := strings.ReplaceAll(candidate.ProposedName, "'", "''")
	escapedPhelps := strings.ReplaceAll(candidate.ProposedPhelps, "'", "''")
	escapedLanguage := strings.ReplaceAll(candidate.Language, "'", "''")
	escapedStatus := strings.ReplaceAll(candidate.ValidationStatus, "'", "''")
	escapedNotes := strings.ReplaceAll(candidate.ValidationNotes, "'", "''")
	escapedDate := strings.ReplaceAll(candidate.CreatedDate, "'", "''")

	// Shell out to Dolt to insert the record
	query := fmt.Sprintf(`INSERT INTO prayer_match_candidates
		(version_id, proposed_name, proposed_phelps, language, text_length, reference_length,
		 length_ratio, confidence_score, validation_status, validation_notes, created_date)
		VALUES ('%s', '%s', '%s', '%s', %d, %d, %.2f, %.2f, '%s', '%s', '%s')`,
		escapedVersionID, escapedName, escapedPhelps, escapedLanguage,
		candidate.TextLength, candidate.ReferenceLength, candidate.LengthRatio,
		candidate.ConfidenceScore, escapedStatus, escapedNotes, escapedDate)

	cmd := exec.Command("dolt", "sql", "-q", query)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to insert prayer match candidate: %w: %s", err, string(output))
	}

	return nil
}

// Update writing with Phelps code
func updateWritingPhelps(phelps, language, version string) error {
	// Escape strings for SQL injection prevention
	escapedPhelps := strings.ReplaceAll(phelps, "'", "''")
	escapedLanguage := strings.ReplaceAll(language, "'", "''")
	escapedVersion := strings.ReplaceAll(version, "'", "''")

	query := fmt.Sprintf(`UPDATE writings SET phelps = '%s' WHERE language = '%s' AND version = '%s'`,
		escapedPhelps, escapedLanguage, escapedVersion)

	cmd := exec.Command("dolt", "sql", "-q", query)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to update writing: %w: %s", err, string(output))
	}

	return nil
}

// Process prayers for a specific language
func processPrayersForLanguage(db *Database, targetLanguage, referenceLanguage string, useGemini bool, reportFile *os.File, maxPrayers int, verbose bool) ([]Writing, error) {
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

		if verbose {
			fmt.Printf("  Calling LLM...")
		}

		response, err := callLLM(prompt, useGemini)
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
		} else if verbose {
			fmt.Printf(" Done!\n")
		}

		fmt.Fprintf(reportFile, "  LLM Response - Phelps: %s, Confidence: %.1f%%, Reasoning: %s\n",
			response.PhelpsCode, response.Confidence*100, response.Reasoning)

		if verbose {
			fmt.Printf("  Result: %s (%.1f%% confidence)\n", response.PhelpsCode, response.Confidence*100)
		}

		if response.PhelpsCode == "UNKNOWN" || response.Confidence < 0.7 {
			// Low confidence or unknown - add to unmatched for interactive assignment
			if response.PhelpsCode == "UNKNOWN" {
				unmatchedPrayers = append(unmatchedPrayers, writing)
				fmt.Fprintf(reportFile, "  UNMATCHED: Will prompt for interactive assignment\n")
			} else {
				// Low confidence - add to candidates table
				candidate := PrayerMatchCandidate{
					VersionID:        writing.Version,
					ProposedName:     writing.Name,
					ProposedPhelps:   response.PhelpsCode,
					Language:         writing.Language,
					TextLength:       len(writing.Text),
					ReferenceLength:  0, // Would need to calculate from reference text
					LengthRatio:      1.0,
					ConfidenceScore:  response.Confidence,
					ValidationStatus: "pending",
					ValidationNotes:  response.Reasoning,
					CreatedDate:      time.Now().Format("2006-01-02 15:04:05"),
				}

				if err := insertPrayerMatchCandidate(db, candidate); err != nil {
					fmt.Fprintf(reportFile, "  ERROR: Failed to insert candidate: %v\n", err)
					if verbose {
						fmt.Printf("  ERROR inserting candidate: %v\n", err)
					} else {
						log.Printf("Error inserting candidate for %s: %v", writing.Version, err)
					}
				} else {
					candidates++
					fmt.Fprintf(reportFile, "  CANDIDATE: Added to prayer_match_candidates table\n")
					if verbose {
						fmt.Printf("  Added to candidates table\n")
					}
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
	// Check if ollama is installed
	if _, err := exec.LookPath("ollama"); err != nil {
		return fmt.Errorf("ollama not found in PATH: %v", err)
	}

	// Check if model is available
	cmd := exec.Command("ollama", "list")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to list ollama models: %v", err)
	}

	if !strings.Contains(string(output), model) {
		return fmt.Errorf("model '%s' not found. Available models:\n%s\nTry: ollama pull %s", model, string(output), model)
	}

	return nil
}

func main() {
	var targetLanguage = flag.String("language", "", "Target language code to process (default: auto-detect optimal)")
	var referenceLanguage = flag.String("reference", "en", "Reference language for Phelps codes (default: en)")
	var useGemini = flag.Bool("gemini", true, "Use Gemini CLI (default: true, falls back to Ollama)")
	var ollamaModel = flag.String("model", "gpt-oss", "Ollama model to use (default: gpt-oss)")
	var reportPath = flag.String("report", "prayer_matching_report.txt", "Path for the report file")
	var interactive = flag.Bool("interactive", true, "Enable interactive assignment for unmatched prayers")
	var maxPrayers = flag.Int("max", 0, "Maximum number of prayers to process (0 = unlimited)")
	var verbose = flag.Bool("verbose", false, "Enable verbose output")
	var showHelp = flag.Bool("help", false, "Show help message")

	flag.Parse()

	if *showHelp {
		fmt.Printf("Bahá'í Prayers LLM Language Matcher\n")
		fmt.Printf("====================================\n\n")
		fmt.Printf("This tool uses Large Language Models (LLMs) to match prayers in different languages\n")
		fmt.Printf("to their corresponding Phelps codes in the Bahá'í writings database.\n\n")
		fmt.Printf("Usage: %s [options]\n\n", os.Args[0])
		fmt.Printf("Options:\n")
		flag.PrintDefaults()
		fmt.Printf("Examples:\n")
		fmt.Printf("  %s                                 # Auto-select optimal language\n", os.Args[0])
		fmt.Printf("  %s -language=es -max=10            # Process first 10 Spanish prayers\n", os.Args[0])
		fmt.Printf("  %s -language=fr -verbose           # Process French with detailed output\n", os.Args[0])
		fmt.Printf("  %s -language=de -interactive=false # Process German without interactive mode\n", os.Args[0])
		fmt.Printf("  %s -language=es -gemini=false      # Process Spanish prayers using only Ollama\n", os.Args[0])
		fmt.Printf("  %s -help                           # Show this help message\n", os.Args[0])
		fmt.Printf("\nTroubleshooting:\n")
		fmt.Printf("  If Ollama fails, ensure it's installed and the model is available:\n")
		fmt.Printf("    ollama list                      # Check available models\n")
		fmt.Printf("    ollama pull %s                 # Pull required model\n", *ollamaModel)
		fmt.Printf("  If Gemini fails, install Gemini CLI or use -gemini=false\n")
		fmt.Printf("  For languages with minimal missing prayers, consider using -language=es or -language=fr\n")
		fmt.Printf("  Use -max=N to limit processing and -verbose for detailed output\n")
		fmt.Printf("  Send SIGINT (Ctrl+C) for graceful stop after current prayer\n")
		return
	}

	// Set up signal handling for graceful stop
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Printf("\nReceived stop signal. Will stop after current prayer...\n")
		atomic.StoreInt32(&stopRequested, 1)
	}()

	// Set the Ollama model
	OllamaModel = *ollamaModel

	// Check Ollama availability
	if err := checkOllama(*ollamaModel); err != nil {
		log.Printf("Ollama check failed: %v", err)
		if !*useGemini {
			log.Fatalf("Ollama is required when Gemini is disabled")
		}
		log.Printf("Will attempt to use Gemini CLI only")
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
	fmt.Fprintf(reportFile, "Using Gemini: %t\n", *useGemini)
	fmt.Fprintf(reportFile, "Ollama Model: %s\n", *ollamaModel)
	fmt.Fprintf(reportFile, "Interactive Mode: %t\n", *interactive)
	fmt.Fprintf(reportFile, "Max Prayers: %d\n", *maxPrayers)
	fmt.Fprintf(reportFile, "Verbose Mode: %t\n", *verbose)
	fmt.Fprintf(reportFile, "\n")

	db := GetDatabase()
	log.Println("Database loaded")
	fmt.Fprintf(reportFile, "Database loaded successfully\n")

	// Auto-select optimal language if not specified
	if *targetLanguage == "" {
		*targetLanguage = findOptimalDefaultLanguage(db)
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

	// Show database size
	log.Println("Database size:",
		len(db.Writing), "/", db.Skipped["writing"],
		len(db.Language), "/", db.Skipped["language"],
		len(db.PrayerHeuristic), "/", db.Skipped["prayer_heuristic"],
		len(db.PrayerMatchCandidate), "/", db.Skipped["prayer_match_candidate"],
	)

	fmt.Fprintf(reportFile, "Database size: %d writings, %d languages, %d heuristics, %d candidates\n",
		len(db.Writing), len(db.Language), len(db.PrayerHeuristic), len(db.PrayerMatchCandidate))

	// Process prayers for the specified language
	unmatchedPrayers, err := processPrayersForLanguage(&db, *targetLanguage, *referenceLanguage, *useGemini, reportFile, *maxPrayers, *verbose)
	if err != nil {
		log.Fatalf("Error processing prayers: %v", err)
	}

	// Interactive assignment for unmatched prayers
	if *interactive && len(unmatchedPrayers) > 0 {
		interactiveAssignment(&db, unmatchedPrayers, reportFile)
	} else if len(unmatchedPrayers) > 0 {
		fmt.Printf("Found %d unmatched prayers. Run with -interactive=true to assign them manually.\n", len(unmatchedPrayers))
		fmt.Fprintf(reportFile, "Found %d unmatched prayers (interactive mode disabled)\n", len(unmatchedPrayers))
	}

	log.Printf("Prayer matching completed. Report written to: %s", *reportPath)
}
