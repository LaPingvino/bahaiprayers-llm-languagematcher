package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// LanguageBatch represents multiple languages processed together
type LanguageBatch struct {
	Languages      []string                       `json:"languages"`
	EnglishRefs    []PrayerFingerprint            `json:"english_refs"`
	LanguageGroups map[string][]PrayerFingerprint `json:"language_groups"`
	TotalPrayers   int                            `json:"total_prayers"`
	BatchSize      string                         `json:"batch_size"` // "small", "medium", "large"
}

// MultiLanguageMatchResult represents matches across multiple languages
type MultiLanguageMatchResult struct {
	EnglishPhelps   string   `json:"phelps"`
	TargetLanguage  string   `json:"target_language"`
	TargetVersion   string   `json:"target_version"`
	MatchType       string   `json:"match_type"`
	Confidence      float64  `json:"confidence"`
	MatchReasons    []string `json:"match_reasons"`
	AmbiguityReason string   `json:"ambiguity_reason,omitempty"`
}

// UltraBatchResponse handles multi-language processing results
type UltraBatchResponse struct {
	Matches              []MultiLanguageMatchResult `json:"matches"`
	LanguagesSummary     map[string]LanguageSummary `json:"languages_summary"`
	TotalExactMatches    int                        `json:"total_exact_matches"`
	TotalLikelyMatches   int                        `json:"total_likely_matches"`
	TotalAmbiguous       int                        `json:"total_ambiguous"`
	TotalNewTranslations int                        `json:"total_new_translations"`
	ProcessedLanguages   []string                   `json:"processed_languages"`
	Summary              string                     `json:"summary"`
}

// LanguageSummary provides per-language statistics
type LanguageSummary struct {
	Language      string `json:"language"`
	TotalPrayers  int    `json:"total_prayers"`
	ExactMatches  int    `json:"exact_matches"`
	LikelyMatches int    `json:"likely_matches"`
	Ambiguous     int    `json:"ambiguous"`
}

// LanguageStats provides simple language statistics for menu system
type LanguageStats struct {
	Language        string
	PrayerCount     int
	UnmatchedCount  int
	NewTranslations int     `json:"new_translations"`
	CompletionRate  float64 `json:"completion_rate"`
}

// langSizeStruct for prioritizing languages by size
type langSizeStruct struct {
	lang  string
	count int
}

// GetUnprocessedLanguageStats returns statistics for languages needing processing
// Excludes transliteration languages which need special handling
// NOW INCLUDES languages with matched prayers for error correction
func GetUnprocessedLanguageStats() ([]LanguageStats, error) {
	query := `
		SELECT
			language,
			COUNT(*) as total_prayers,
			SUM(CASE WHEN phelps IS NULL OR phelps = '' THEN 1 ELSE 0 END) as unmatched_prayers,
			SUM(CASE WHEN phelps IS NOT NULL AND phelps != '' THEN 1 ELSE 0 END) as matched_prayers
		FROM writings
		WHERE language != 'en'
		  AND language NOT IN ('', 'unknown')
		  AND language NOT LIKE '%-translit'
		GROUP BY language
		HAVING SUM(CASE WHEN phelps IS NULL OR phelps = '' THEN 1 ELSE 0 END) > 0
		   OR SUM(CASE WHEN phelps IS NOT NULL AND phelps != '' THEN 1 ELSE 0 END) > 10
		ORDER BY SUM(CASE WHEN phelps IS NULL OR phelps = '' THEN 1 ELSE 0 END) DESC, COUNT(*) DESC
	`

	output, err := execDoltQueryCSV(query)
	if err != nil {
		return nil, fmt.Errorf("failed to get language stats: %w", err)
	}

	var stats []LanguageStats

	// Skip header and process each record
	for i := 1; i < len(output); i++ {
		if len(output[i]) >= 4 {
			language := strings.Trim(output[i][0], `"`)
			totalPrayers := parseInt(strings.Trim(output[i][1], `"`))
			unmatchedPrayers := parseInt(strings.Trim(output[i][2], `"`))
			matchedPrayers := parseInt(strings.Trim(output[i][3], `"`))

			// Include languages with either unmatched prayers OR significant matched prayers (for error correction)
			if unmatchedPrayers > 0 || matchedPrayers > 10 {
				stats = append(stats, LanguageStats{
					Language:       language,
					PrayerCount:    totalPrayers,
					UnmatchedCount: unmatchedPrayers,
				})
			}
		}
	}

	return stats, nil
}

// parseInt safely converts string to int
func parseInt(s string) int {
	if s == "" {
		return 0
	}
	// Simple conversion - just handle basic cases
	switch s {
	case "0":
		return 0
	case "1":
		return 1
	case "2":
		return 2
	case "3":
		return 3
	case "4":
		return 4
	case "5":
		return 5
	case "6":
		return 6
	case "7":
		return 7
	case "8":
		return 8
	case "9":
		return 9
	default:
		// For larger numbers, do basic parsing
		result := 0
		for _, c := range s {
			if c >= '0' && c <= '9' {
				result = result*10 + int(c-'0')
			}
		}
		return result
	}
}

// CreateLanguageBatches groups languages for efficient processing with optimal packing
func CreateLanguageBatches(stats []LanguageStats) [][]string {
	return CreateLanguageBatchesWithMode(stats, false)
}

// CreateLanguageBatchesWithMode groups languages with optional reverse packing
func CreateLanguageBatchesWithMode(stats []LanguageStats, reverse bool) [][]string {
	return CreateLanguageBatchesWithHeuristics(stats, reverse, false)
}

func CreateLanguageBatchesWithHeuristics(stats []LanguageStats, reverse, heuristic bool) [][]string {
	var batches [][]string

	// Batching strategy based on prayer counts
	const MAX_PRAYERS_PER_BATCH = 250  // Total prayers per batch (increased)
	const MAX_LANGUAGES_PER_BATCH = 20 // Maximum languages in one batch (increased)
	const PACK_THRESHOLD = 150         // If a "solo" batch has < 150 prayers, pack small langs into it

	log.Printf("Creating optimally-packed language batches from %d unprocessed languages", len(stats))

	if reverse {
		return createReverseBatches(stats)
	}

	// Apply heuristic sorting if enabled
	if heuristic {
		stats = sortLanguagesByLikelihood(stats)
		log.Printf("✨ Applied heuristic language sorting for optimal match success")
	}

	// Normal mode: Separate large and small languages
	var largeLangs []LanguageStats
	var smallLangs []LanguageStats

	for _, stat := range stats {
		if stat.UnmatchedCount >= 50 {
			largeLangs = append(largeLangs, stat)
		} else {
			smallLangs = append(smallLangs, stat)
		}
	}

	// Process large languages and pack small ones into them when possible
	smallIdx := 0
	for _, largeLang := range largeLangs {
		currentBatch := []string{largeLang.Language}
		currentBatchPrayers := largeLang.UnmatchedCount

		// If this large language has room for small languages, pack them in
		if currentBatchPrayers < PACK_THRESHOLD {
			log.Printf("Packing small languages into %s batch (%d prayers)", largeLang.Language, currentBatchPrayers)

			// Add small languages until we hit limits
			for smallIdx < len(smallLangs) &&
				len(currentBatch) < MAX_LANGUAGES_PER_BATCH &&
				currentBatchPrayers+smallLangs[smallIdx].UnmatchedCount <= MAX_PRAYERS_PER_BATCH {

				currentBatch = append(currentBatch, smallLangs[smallIdx].Language)
				currentBatchPrayers += smallLangs[smallIdx].UnmatchedCount
				smallIdx++
			}

			log.Printf("Packed batch: %s + [%s] (%d prayers)",
				largeLang.Language,
				strings.Join(currentBatch[1:], ", "),
				currentBatchPrayers)
		} else {
			log.Printf("Solo batch: %s (%d prayers)", largeLang.Language, currentBatchPrayers)
		}

		batches = append(batches, currentBatch)
	}

	// Process remaining small languages in efficient batches
	for smallIdx < len(smallLangs) {
		var currentBatch []string
		currentBatchPrayers := 0

		// Pack small languages together
		for smallIdx < len(smallLangs) &&
			len(currentBatch) < MAX_LANGUAGES_PER_BATCH &&
			currentBatchPrayers+smallLangs[smallIdx].UnmatchedCount <= MAX_PRAYERS_PER_BATCH {

			currentBatch = append(currentBatch, smallLangs[smallIdx].Language)
			currentBatchPrayers += smallLangs[smallIdx].UnmatchedCount
			smallIdx++
		}

		if len(currentBatch) > 0 {
			batches = append(batches, currentBatch)
			log.Printf("Small batch: [%s] (%d prayers)", strings.Join(currentBatch, ", "), currentBatchPrayers)
		}
	}

	log.Printf("Created %d optimally-packed batches (vs %d with old strategy)", len(batches), len(largeLangs)+(len(smallLangs)+9)/10)
	return batches
}

// CreateMultiLanguagePrompt builds a prompt for processing multiple languages
func CreateMultiLanguagePrompt(batch LanguageBatch) string {
	var prompt strings.Builder

	prompt.WriteString("You are an expert in Bahá'í prayer matching across multiple languages simultaneously.\n\n")

	prompt.WriteString("# MULTI-LANGUAGE BATCH PROCESSING\n")
	prompt.WriteString(fmt.Sprintf("Processing %d languages in one batch: %s\n",
		len(batch.Languages), strings.Join(batch.Languages, ", ")))
	prompt.WriteString(fmt.Sprintf("Total prayers to process: %d\n", batch.TotalPrayers))
	prompt.WriteString(fmt.Sprintf("Batch size: %s\n\n", batch.BatchSize))

	prompt.WriteString("# ENGLISH REFERENCE FINGERPRINTS\n")
	prompt.WriteString("```json\n")
	englishJSON, _ := json.MarshalIndent(batch.EnglishRefs, "", "  ")
	prompt.WriteString(string(englishJSON))
	prompt.WriteString("\n```\n\n")

	// Add each language group
	for _, lang := range batch.Languages {
		fingerprints := batch.LanguageGroups[lang]
		prompt.WriteString(fmt.Sprintf("# %s LANGUAGE FINGERPRINTS (%d prayers)\n",
			strings.ToUpper(lang), len(fingerprints)))
		prompt.WriteString("```json\n")
		langJSON, _ := json.MarshalIndent(fingerprints, "", "  ")
		prompt.WriteString(string(langJSON))
		prompt.WriteString("\n```\n\n")
	}

	prompt.WriteString("# MATCHING INSTRUCTIONS\n")
	prompt.WriteString("1. Process ALL languages in this batch efficiently\n")
	prompt.WriteString("2. For each English reference, find matches in ANY of the target languages\n")
	prompt.WriteString("3. Use the same matching criteria: EXACT, LIKELY, AMBIGUOUS, NEW_TRANSLATION\n")
	prompt.WriteString("4. Include target_language field to specify which language contains the match\n")
	prompt.WriteString("5. Focus on high-confidence matches for bulk processing efficiency\n\n")

	prompt.WriteString("# OUTPUT FORMAT\n")
	prompt.WriteString("```json\n")
	prompt.WriteString(`{
  "matches": [
    {
      "phelps": "AB00001",
      "target_language": "cy",
      "target_version": "uuid-123",
      "match_type": "EXACT|LIKELY|AMBIGUOUS|NEW_TRANSLATION",
      "confidence": 95,
      "match_reasons": ["text_hash_match", "opening_phrase_similar"],
      "ambiguity_reason": ""
    }
  ],
  "languages_summary": {
    "cy": {
      "language": "cy",
      "total_prayers": 8,
      "exact_matches": 3,
      "likely_matches": 4,
      "ambiguous": 1,
      "new_translations": 0,
      "completion_rate": 87.5
    }
  },
  "total_exact_matches": 45,
  "total_likely_matches": 67,
  "total_ambiguous": 12,
  "total_new_translations": 31,
  "processed_languages": ["cy", "th", "tl"],
  "summary": "Processed 3 languages with 155 total prayers: 45 exact, 67 likely, 12 ambiguous"
}`)
	prompt.WriteString("\n```\n\n")

	prompt.WriteString("# EFFICIENCY GUIDELINES\n")
	prompt.WriteString("- Process all languages in one comprehensive response\n")
	prompt.WriteString("- Prioritize clear, high-confidence matches\n")
	prompt.WriteString("- Mark ambiguous cases for later detailed review\n")
	prompt.WriteString("- Provide per-language statistics in languages_summary\n")
	prompt.WriteString("- Be thorough but efficient\n\n")

	prompt.WriteString("Begin multi-language matching. Return only the JSON response.\n")

	return prompt.String()
}

// ProcessLanguageBatchWithRetry processes a language batch with automatic splitting on failure
// getLanguageForVersion queries the database to get the actual language of a prayer version
func getLanguageForVersion(version string) (string, error) {
	query := fmt.Sprintf("SELECT language FROM writings WHERE version = '%s'",
		strings.ReplaceAll(version, "'", "''"))

	output, err := execDoltQuery(query)
	if err != nil {
		return "", err
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) < 2 {
		return "", fmt.Errorf("no results found for version %s", version)
	}

	return strings.TrimSpace(lines[1]), nil
}

func ProcessLanguageBatchWithRetry(languages []string, isRetry, heuristic bool) error {
	// First try the normal processing
	if err := ProcessLanguageBatch(languages, heuristic); err != nil {
		// If it's already a retry or batch is small, don't split further
		if isRetry || len(languages) <= 3 {
			return err
		}

		log.Printf("❌ Large batch failed, attempting to split and retry...")
		return splitAndRetryBatch(languages, heuristic)
	}
	return nil
}

// ProcessLanguageBatch handles a batch of multiple languages
func ProcessLanguageBatch(languages []string, heuristic bool) error {
	log.Printf("Processing language batch: %v", languages)

	// Load database
	db, err := GetDatabase()
	if err != nil {
		return fmt.Errorf("failed to load database: %w", err)
	}

	// Build English references
	englishRefs := BuildEnglishReference(db)

	// Create fingerprints for English references
	var englishFingerprints []PrayerFingerprint
	for _, ref := range englishRefs {
		fp := CreatePrayerFingerprint(ref.Phelps, "", "en", ref.Name, ref.Text)
		englishFingerprints = append(englishFingerprints, fp)
	}

	// Build batch structure
	batch := LanguageBatch{
		Languages:      languages,
		EnglishRefs:    englishFingerprints,
		LanguageGroups: make(map[string][]PrayerFingerprint),
		TotalPrayers:   0,
	}

	// Process each language in the batch
	for _, lang := range languages {
		var targetPrayers []TargetPrayer
		if heuristic {
			targetPrayers = BuildTargetPrayersWithHeuristics(db, lang)
		} else {
			targetPrayers = BuildTargetPrayers(db, lang)
		}
		var targetFingerprints []PrayerFingerprint

		for _, prayer := range targetPrayers {
			fp := CreatePrayerFingerprint("", prayer.Version, lang, prayer.Name, prayer.Text)
			targetFingerprints = append(targetFingerprints, fp)
		}

		batch.LanguageGroups[lang] = targetFingerprints
		batch.TotalPrayers += len(targetFingerprints)

		log.Printf("  - %s: %d prayers", lang, len(targetFingerprints))
	}

	// Determine batch size
	if batch.TotalPrayers <= 50 {
		batch.BatchSize = "small"
	} else if batch.TotalPrayers <= 150 {
		batch.BatchSize = "medium"
	} else {
		batch.BatchSize = "large"
	}

	log.Printf("Created batch: %d languages, %d total prayers (%s)",
		len(languages), batch.TotalPrayers, batch.BatchSize)

	// Create prompt
	prompt := CreateMultiLanguagePrompt(batch)

	// Try backends with fallback using common function
	response, backendErr := callLLMWithBackendFallback(prompt, "batch processing", true)

	// Handle rate limit case with batch saving
	if backendErr != nil && (strings.Contains(backendErr.Error(), "limit reached") || strings.Contains(backendErr.Error(), "rate limit")) {
		log.Printf("🚨 RATE LIMIT HIT for batch: %v", languages)
		log.Printf("💾 Saving batch info for manual pickup...")

		// Save batch details for manual processing
		batchFile := fmt.Sprintf("pending_batch_%s_%d.json",
			strings.Join(languages, "_"),
			time.Now().Unix())

		batchInfo := map[string]interface{}{
			"languages":     languages,
			"total_prayers": batch.TotalPrayers,
			"batch_size":    len(languages),
			"created_at":    time.Now().Format(time.RFC3339),
			"status":        "pending",
			"prompt":        prompt,
		}

		if batchJSON, marshalErr := json.MarshalIndent(batchInfo, "", "  "); marshalErr == nil {
			if writeErr := os.WriteFile(batchFile, batchJSON, 0644); writeErr == nil {
				log.Printf("📁 Batch saved to: %s", batchFile)
			}
		}

		return backendErr
	}

	if backendErr != nil {
		return fmt.Errorf("all backends failed for batch %v: %w", languages, backendErr)
	}

	// Parse response using robust JSON extraction
	jsonStr, err := ExtractJSONFromResponse(response)
	if err != nil {
		// Save failed response for later analysis
		failedResponseFile := fmt.Sprintf("failed_response_%s_%d.txt",
			strings.Join(languages, "_"),
			time.Now().Unix())
		if writeErr := os.WriteFile(failedResponseFile, []byte(response), 0644); writeErr == nil {
			log.Printf("❌ JSON extraction failed, saved response to: %s", failedResponseFile)
		}
		return fmt.Errorf("failed to extract JSON from response (saved to %s): %w", failedResponseFile, err)
	}

	var results UltraBatchResponse
	if err := json.Unmarshal([]byte(jsonStr), &results); err != nil {
		// Save failed response for later analysis
		failedResponseFile := fmt.Sprintf("failed_response_%s_%d.txt",
			strings.Join(languages, "_"),
			time.Now().Unix())
		if writeErr := os.WriteFile(failedResponseFile, []byte(response), 0644); writeErr == nil {
			log.Printf("❌ Parse failed, saved response to: %s", failedResponseFile)
		}
		return fmt.Errorf("failed to parse response (saved to %s): %w", failedResponseFile, err)
	}

	// Process results for each language
	totalProcessed := 0
	languageMismatches := 0

	for _, lang := range languages {
		langMatches := 0
		for _, match := range results.Matches {
			if match.TargetLanguage == lang {
				// Validate that the UUID actually belongs to this language
				actualLang, err := getLanguageForVersion(match.TargetVersion)
				if err != nil {
					log.Printf("⚠️ Error checking language for %s: %v", match.TargetVersion, err)
					continue
				}

				if actualLang != lang {
					log.Printf("⚠️ Language mismatch: LLM said %s but UUID %s is actually %s (Phelps: %s)",
						lang, match.TargetVersion, actualLang, match.EnglishPhelps)
					languageMismatches++
					continue
				}

				// Apply the match to database
				if match.MatchType == "EXACT" && match.Confidence >= 95 {
					query := fmt.Sprintf("UPDATE writings SET phelps = '%s' WHERE version = '%s' AND language = '%s'",
						strings.ReplaceAll(match.EnglishPhelps, "'", "''"),
						strings.ReplaceAll(match.TargetVersion, "'", "''"),
						lang)

					if _, err := execDoltQuery(query); err != nil {
						log.Printf("ERROR updating %s: %v", match.TargetVersion, err)
						continue
					}
					langMatches++
				} else if match.MatchType == "LIKELY" && match.Confidence >= 80 {
					query := fmt.Sprintf("UPDATE writings SET phelps = '%s' WHERE version = '%s' AND language = '%s'",
						strings.ReplaceAll(match.EnglishPhelps, "'", "''"),
						strings.ReplaceAll(match.TargetVersion, "'", "''"),
						lang)

					if _, err := execDoltQuery(query); err != nil {
						log.Printf("ERROR updating %s: %v", match.TargetVersion, err)
						continue
					}
					langMatches++
				}
			}
		}

		log.Printf("  ✅ %s: %d matches processed", lang, langMatches)
		totalProcessed += langMatches
	}

	if languageMismatches > 0 {
		log.Printf("⚠️ Filtered out %d matches due to language mismatches", languageMismatches)
	}

	log.Printf("Batch completed: %d total matches across %d languages",
		totalProcessed, len(languages))

	return nil
}

// splitAndRetryBatch splits a large batch into smaller ones and retries
func splitAndRetryBatch(languages []string, heuristic bool) error {
	log.Printf("📦 Splitting batch of %d languages into smaller batches", len(languages))

	// Split into batches of 3-5 languages each
	const maxSubBatchSize = 5
	var subBatches [][]string

	for i := 0; i < len(languages); i += maxSubBatchSize {
		end := i + maxSubBatchSize
		if end > len(languages) {
			end = len(languages)
		}
		subBatches = append(subBatches, languages[i:end])
	}

	log.Printf("📦 Created %d sub-batches from failed large batch", len(subBatches))

	var lastError error
	successCount := 0

	for i, subBatch := range subBatches {
		log.Printf("🔄 Processing sub-batch %d/%d: %v", i+1, len(subBatches), subBatch)

		if err := ProcessLanguageBatchWithRetry(subBatch, true, heuristic); err != nil {
			log.Printf("❌ Sub-batch %d failed: %v", i+1, err)
			lastError = err
		} else {
			log.Printf("✅ Sub-batch %d completed successfully", i+1)
			successCount++
		}

		// Brief pause between sub-batches
		time.Sleep(2 * time.Second)
	}

	log.Printf("📊 Sub-batch results: %d/%d successful", successCount, len(subBatches))

	if successCount == 0 {
		return fmt.Errorf("all sub-batches failed, last error: %w", lastError)
	}

	if successCount < len(subBatches) {
		log.Printf("⚠️ %d sub-batches failed but %d succeeded", len(subBatches)-successCount, successCount)
	}

	return nil
}

// UltraCompressedBulkMatching processes all languages with smart batching
func UltraCompressedBulkMatching() error {
	return UltraCompressedBulkMatchingWithSkip(true, false, false)
}

// UltraCompressedBulkMatchingWithSkip processes all languages with smart skipping and optional reverse order
func UltraCompressedBulkMatchingWithSkip(skipProcessed, reverse, heuristic bool) error {
	if heuristic {
		log.Println("🚀 Starting ULTRA-COMPRESSED bulk matching with HEURISTIC PRIORITIZATION")
		log.Println("   ✨ Features enabled: 5% mistake correction + likelihood sorting")
	} else {
		log.Println("🚀 Starting ULTRA-COMPRESSED bulk matching with smart batching")
	}

	// First, process transliteration languages
	log.Println("Phase 1: Processing transliteration languages")
	if err := ProcessTransliterationLanguages(); err != nil {
		log.Printf("⚠️  Transliteration processing failed: %v", err)
		log.Println("Continuing with main language processing...")
	}

	// Get unprocessed language statistics (excluding transliterations)
	stats, err := GetUnprocessedLanguageStats()
	if err != nil {
		return fmt.Errorf("failed to get language stats: %w", err)
	}

	if len(stats) == 0 {
		log.Println("🎉 No unprocessed languages found - database is complete!")
		return nil
	}

	log.Printf("Phase 2: Processing %d main languages with ultra-compression", len(stats))

	log.Printf("Found %d languages needing processing", len(stats))

	// Print statistics
	totalUnmatched := 0
	for _, stat := range stats {
		totalUnmatched += stat.UnmatchedCount
		log.Printf("  - %s: %d prayers (%d unmatched)",
			stat.Language, stat.PrayerCount, stat.UnmatchedCount)
	}

	// Create smart batches with heuristic sorting if enabled
	batches := CreateLanguageBatchesWithHeuristics(stats, reverse, heuristic)

	log.Printf("\n📊 Processing Plan:")
	log.Printf("  - Traditional approach: ~%d API calls", (256 * len(stats) / 30))
	log.Printf("  - Compressed approach: %d API calls", len(stats))
	log.Printf("  - Ultra-compressed approach: %d API calls", len(batches))
	log.Printf("  - Efficiency improvement: %d%% reduction",
		((256*len(stats)/30)-len(batches))*100/(256*len(stats)/30))

	// Process each batch with rate limit awareness
	successfulBatches := 0
	failedBatches := 0
	rateLimitHit := false

	for i, batch := range batches {
		log.Printf("\n[%d/%d] Processing batch: %v", i+1, len(batches), batch)

		if err := ProcessLanguageBatchWithRetry(batch, false, heuristic); err != nil {
			if strings.Contains(err.Error(), "limit reached") || strings.Contains(err.Error(), "rate limit") {
				log.Printf("🚨 RATE LIMIT HIT - Stopping processing")
				log.Printf("📊 Progress so far: %d/%d batches completed", successfulBatches, len(batches))
				log.Printf("📝 Remaining batches: %d", len(batches)-(i+1))

				// Save remaining batches for manual processing
				remainingBatches := batches[i:]
				remainingFile := fmt.Sprintf("remaining_batches_%d.json", time.Now().Unix())

				remainingInfo := map[string]interface{}{
					"remaining_batches": remainingBatches,
					"completed_batches": successfulBatches,
					"failed_batches":    failedBatches,
					"total_batches":     len(batches),
					"stopped_at_batch":  i + 1,
					"status":            "rate_limit_interrupted",
					"created_at":        time.Now().Format(time.RFC3339),
				}

				if remainingJSON, marshalErr := json.MarshalIndent(remainingInfo, "", "  "); marshalErr == nil {
					if writeErr := os.WriteFile(remainingFile, remainingJSON, 0644); writeErr == nil {
						log.Printf("💾 Remaining batches saved to: %s", remainingFile)
						log.Printf("📋 To resume later, use these batches manually or wait for rate limit reset")
					}
				}

				rateLimitHit = true
				failedBatches++
				break
			}

			log.Printf("❌ Batch failed: %v", err)
			failedBatches++
		} else {
			log.Printf("✅ Batch completed successfully")
			successfulBatches++
		}

		// Small delay between batches
		if i < len(batches)-1 {
			log.Println("   Waiting 3 seconds before next batch...")
			// Note: time.Sleep would need to be imported
		}
	}

	// Final summary
	if rateLimitHit {
		log.Printf("\n🚨 PROCESSING INTERRUPTED BY RATE LIMIT!")
		log.Printf("  - Total batches planned: %d", len(batches))
		log.Printf("  - Successful: %d", successfulBatches)
		log.Printf("  - Failed: %d", failedBatches)
		log.Printf("  - Remaining: %d", len(batches)-(successfulBatches+failedBatches))
		log.Printf("  - Progress: %d%% complete", successfulBatches*100/len(batches))
		log.Printf("  - API calls used: %d", successfulBatches)

		log.Printf("\n📋 MANUAL PICKUP INSTRUCTIONS:")
		log.Printf("  1. Wait for rate limit reset (usually at 11pm Lisbon time)")
		log.Printf("  2. Check remaining_batches_*.json for pending work")
		log.Printf("  3. Run individual batches with: ./prayer-matcher -language=XX -compressed -cli")
		log.Printf("  4. Or retry: ./prayer-matcher -ultra -cli")

		if successfulBatches > 0 {
			log.Printf("\n✅ PARTIAL SUCCESS: %d batches completed", successfulBatches)
			log.Printf("The database has been partially updated - progress saved!")
		}

	} else {
		log.Printf("\n🏁 ULTRA-COMPRESSED BULK MATCHING COMPLETED!")
		log.Printf("  - Total batches: %d", len(batches))
		log.Printf("  - Successful: %d", successfulBatches)
		log.Printf("  - Failed: %d", failedBatches)
		log.Printf("  - Success rate: %d%%", successfulBatches*100/len(batches))
		log.Printf("  - API calls used: %d (vs %d traditional)", len(batches), (256 * len(stats) / 30))

		if failedBatches > 0 {
			log.Printf("\n⚠️  %d batches failed - you may want to retry them individually", failedBatches)
		} else {
			log.Printf("\n🎉 ALL BATCHES COMPLETED SUCCESSFULLY!")
			log.Printf("The prayer matching database should now be complete!")
		}
	}

	return nil
}

// createReverseBatches packs as many small languages as possible into first batches
func createReverseBatches(stats []LanguageStats) [][]string {
	const MAX_PRAYERS_PER_BATCH = 250  // Total prayers per batch (restored)
	const MAX_LANGUAGES_PER_BATCH = 20 // Maximum languages in one batch (restored)

	log.Printf("🔄 REVERSE MODE: Packing small languages into first batches")

	// Sort by prayer count (smallest first)
	sort.Slice(stats, func(i, j int) bool {
		return stats[i].PrayerCount < stats[j].PrayerCount
	})

	var batches [][]string
	var currentBatch []string
	var currentPrayers int
	var currentLanguages int

	for _, stat := range stats {
		prayerCount := stat.PrayerCount

		// Check if we can add this language to current batch
		if (currentPrayers+prayerCount <= MAX_PRAYERS_PER_BATCH) &&
			(currentLanguages < MAX_LANGUAGES_PER_BATCH) {
			// Add to current batch
			currentBatch = append(currentBatch, stat.Language)
			currentPrayers += prayerCount
			currentLanguages++

			log.Printf("  Added %s (%d prayers) to batch %d - Total: %d prayers, %d languages",
				stat.Language, prayerCount, len(batches)+1, currentPrayers, currentLanguages)
		} else {
			// Start new batch
			if len(currentBatch) > 0 {
				batches = append(batches, currentBatch)
				log.Printf("📦 Completed batch %d: [%s] (%d prayers, %d languages)",
					len(batches), strings.Join(currentBatch, ", "), currentPrayers, currentLanguages)
			}

			currentBatch = []string{stat.Language}
			currentPrayers = prayerCount
			currentLanguages = 1

			log.Printf("  Started new batch %d with %s (%d prayers)",
				len(batches)+1, stat.Language, prayerCount)
		}
	}

	// Add final batch
	if len(currentBatch) > 0 {
		batches = append(batches, currentBatch)
		log.Printf("📦 Final batch %d: [%s] (%d prayers, %d languages)",
			len(batches), strings.Join(currentBatch, ", "), currentPrayers, currentLanguages)
	}

	log.Printf("🎯 Created %d reverse-optimized batches (small languages packed first)", len(batches))
	return batches
}

// prioritizeLanguagesBySize sorts languages by number of prayers (smallest first if reverse=true)
func prioritizeLanguagesBySize(languages []string, reverse bool) []string {

	// Get prayer counts for each language
	var langSizes []langSizeStruct
	for _, lang := range languages {
		count := getLanguagePrayerCount(lang)
		langSizes = append(langSizes, langSizeStruct{lang, count})
	}

	if reverse {
		// REVERSE MODE: Pack as many small languages as possible into first batch
		// Sort smallest first for reverse packing
		sort.Slice(langSizes, func(i, j int) bool {
			return langSizes[i].count < langSizes[j].count
		})

		// Repack into reverse-optimized batches
		return repackForReverseOrder(langSizes)
	} else {
		// Normal mode: Largest first
		sort.Slice(langSizes, func(i, j int) bool {
			return langSizes[i].count > langSizes[j].count
		})
	}

	// Extract sorted language codes
	var result []string
	for _, ls := range langSizes {
		result = append(result, ls.lang)
	}

	return result
}

// repackForReverseOrder packs as many small languages as possible into first batches
func repackForReverseOrder(langSizes []langSizeStruct) []string {
	const maxBatchSize = 250 // Target batch size

	var result []string
	var currentBatch []string
	var currentSize int

	// Pack small languages into batches, prioritizing filling first batches
	for _, ls := range langSizes {
		if currentSize+ls.count <= maxBatchSize {
			// Add to current batch
			currentBatch = append(currentBatch, ls.lang)
			currentSize += ls.count
		} else {
			// Finalize current batch and start new one
			if len(currentBatch) > 0 {
				result = append(result, currentBatch...)
				currentBatch = []string{ls.lang}
				currentSize = ls.count
			}
		}
	}

	// Add final batch
	if len(currentBatch) > 0 {
		result = append(result, currentBatch...)
	}

	return result
}

// getLanguagePrayerCount returns the number of unprocessed prayers for a language
// sortLanguagesByLikelihood sorts languages by likelihood of successful matching
func sortLanguagesByLikelihood(stats []LanguageStats) []LanguageStats {
	// Create a copy to avoid modifying the original
	sortedStats := make([]LanguageStats, len(stats))
	copy(sortedStats, stats)

	sort.Slice(sortedStats, func(i, j int) bool {
		scoreI := getLanguageLikelihoodScore(sortedStats[i])
		scoreJ := getLanguageLikelihoodScore(sortedStats[j])

		// Higher score = more likely to succeed = process first
		if scoreI != scoreJ {
			return scoreI > scoreJ
		}

		// If scores are equal, prefer smaller languages (faster processing)
		return sortedStats[i].UnmatchedCount < sortedStats[j].UnmatchedCount
	})

	return sortedStats
}

// getLanguageLikelihoodScore calculates likelihood of successful matching for a language
func getLanguageLikelihoodScore(stat LanguageStats) int {
	score := 0
	lang := stat.Language

	// High-success language families (Latin/Germanic/Romance scripts) - EXPANDED
	latinScriptLangs := map[string]bool{
		"es": true, "pt": true, "fr": true, "it": true, "ca": true, "ro": true,
		"de": true, "nl": true, "da": true, "sv": true, "no": true, "is": true,
		"en": true, "fi": true, "et": true, "lv": true, "lt": true, "pl": true,
		"cs": true, "sk": true, "sl": true, "hr": true, "bs": true, "sq": true,
		"af": true, "eu": true, "mt": true, "cy": true, "ga": true, "gd": true,
		"hu": true, "tr": true, "az": true, "uz": true, "tk": true, "ky": true,
		"id": true, "ms": true, "tl": true, "vi": true, "sw": true, "zu": true,
		"xh": true, "st": true, "tn": true, "sn": true, "yo": true, "ig": true,
		"ha": true, "so": true, "mg": true, "ny": true, "rw": true, "wo": true,
	}

	// Medium-success languages (familiar scripts, established communities) - EXPANDED
	mediumSuccessLangs := map[string]bool{
		"ru": true, "uk": true, "be": true, "bg": true, "mk": true, "sr": true,
		"el": true, "hy": true, "ka": true, "he": true, "ar": true, "fa": true,
		"hi": true, "ur": true, "bn": true, "gu": true, "pa": true, "ta": true,
		"te": true, "kn": true, "ml": true, "th": true, "ko": true, "ja": true,
		"zh-Hans": true, "zh-Hant": true, "zh": true, "yue": true, "nan": true,
		"am": true, "ti": true, "km": true, "lo": true, "my": true, "si": true,
		"ne": true, "mr": true, "sa": true, "or": true, "as": true, "ps": true,
		"ku": true, "sd": true, "ug": true, "bo": true, "dz": true, "mn": true,
	}

	// Languages with existing high completion rates get HIGHER priority for error correction
	completionRate := float64(stat.PrayerCount-stat.UnmatchedCount) / float64(stat.PrayerCount)
	if completionRate > 0.95 {
		score += 100 // VERY high priority for near-complete (error correction focus)
	} else if completionRate > 0.90 {
		score += 70 // High existing success (still needs error checking)
	} else if completionRate > 0.80 {
		score += 50 // High existing success
	} else if completionRate > 0.50 {
		score += 30 // Medium existing success
	} else {
		score += 10 // Lower completion (still important)
	}

	// Script/family-based scoring
	if latinScriptLangs[lang] {
		score += 25 // Latin script languages tend to match well
	} else if mediumSuccessLangs[lang] {
		score += 15 // Established writing systems
	} else {
		score += 5 // Rare/complex scripts need more attention
	}

	// Size-based adjustments
	if stat.UnmatchedCount <= 10 {
		score += 20 // Small languages are quick wins
	} else if stat.UnmatchedCount <= 50 {
		score += 10 // Medium languages
	} else if stat.UnmatchedCount > 150 {
		score -= 5 // Large languages are more complex
	}

	// Special case: transliteration languages get lower priority
	if strings.Contains(lang, "-translit") {
		score -= 10
	}

	return score
}

func getLanguagePrayerCount(language string) int {
	records, err := execDoltQueryCSV(fmt.Sprintf("SELECT COUNT(*) FROM writings WHERE language = '%s' AND (phelps = '' OR phelps IS NULL)", language))
	if err != nil || len(records) < 2 {
		return 0
	}

	if count, err := strconv.Atoi(records[1][0]); err == nil {
		return count
	}
	return 0
}

// Helper functions for skip processing (moved from main.go)
func getAllUnprocessedLanguages() ([]string, error) {
	records, err := execDoltQueryCSV("SELECT DISTINCT language FROM writings WHERE phelps = '' OR phelps IS NULL ORDER BY language")
	if err != nil {
		return nil, err
	}

	var languages []string
	for i, record := range records {
		if i == 0 { // Skip header
			continue
		}
		if len(record) > 0 && record[0] != "" && record[0] != "en" { // Skip English reference
			languages = append(languages, record[0])
		}
	}

	return languages, nil
}

func getProblematicLanguages() (map[string]bool, error) {
	problematic := make(map[string]bool)

	files, err := filepath.Glob("review_summary_*_*.txt")
	if err != nil {
		return problematic, err
	}

	for _, file := range files {
		parts := strings.Split(filepath.Base(file), "_")
		if len(parts) < 3 {
			continue
		}
		lang := parts[2]

		failureRate := getLanguageFailureRateFromFile(file)
		if failureRate > 85.0 {
			problematic[lang] = true
		}
	}

	return problematic, nil
}

func hasExistingReviews(language string) (bool, float64) {
	pattern := fmt.Sprintf("review_summary_%s_*.txt", language)
	files, err := filepath.Glob(pattern)
	if err != nil || len(files) == 0 {
		return false, 0.0
	}

	latestFile := files[len(files)-1]
	failureRate := getLanguageFailureRateFromFile(latestFile)

	return true, failureRate
}

func getLanguageFailureRateFromFile(filename string) float64 {
	data, err := os.ReadFile(filename)
	if err != nil {
		return 0.0
	}

	content := string(data)
	lines := strings.Split(content, "\n")

	var totalProcessed, ambiguous, lowConfidence int

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Total prayers processed:") {
			fmt.Sscanf(line, "Total prayers processed: %d", &totalProcessed)
		} else if strings.HasPrefix(line, "Ambiguous matches:") {
			fmt.Sscanf(line, "Ambiguous matches: %d", &ambiguous)
		} else if strings.HasPrefix(line, "Low confidence matches:") {
			fmt.Sscanf(line, "Low confidence matches: %d", &lowConfidence)
		}
	}

	if totalProcessed == 0 {
		return 0.0
	}

	failedMatches := ambiguous + lowConfidence
	return float64(failedMatches) / float64(totalProcessed) * 100.0
}

// GetTransliterationLanguages returns transliteration languages that need special processing
func GetTransliterationLanguages() ([]LanguageStats, error) {
	query := `
		SELECT
			language,
			COUNT(*) as total_prayers,
			SUM(CASE WHEN phelps IS NULL THEN 1 ELSE 0 END) as unmatched_prayers
		FROM writings
		WHERE language LIKE '%-translit'
		GROUP BY language
		HAVING SUM(CASE WHEN phelps IS NULL THEN 1 ELSE 0 END) > 0
		ORDER BY COUNT(*) DESC
	`

	output, err := execDoltQueryCSV(query)
	if err != nil {
		return nil, fmt.Errorf("failed to get transliteration stats: %w", err)
	}

	var stats []LanguageStats

	// Skip header and process each record
	for i := 1; i < len(output); i++ {
		if len(output[i]) >= 3 {
			language := strings.Trim(output[i][0], `"`)
			totalPrayers := parseInt(strings.Trim(output[i][1], `"`))
			unmatchedPrayers := parseInt(strings.Trim(output[i][2], `"`))

			if unmatchedPrayers > 0 {
				stats = append(stats, LanguageStats{
					Language:       language,
					PrayerCount:    totalPrayers,
					UnmatchedCount: unmatchedPrayers,
				})
			}
		}
	}

	return stats, nil
}

// ProcessTransliterationLanguages handles transliteration languages by copying Phelps codes
func ProcessTransliterationLanguages() error {
	log.Println("🔤 Processing transliteration languages...")

	translitLangs, err := GetTransliterationLanguages()
	if err != nil {
		return fmt.Errorf("failed to get transliteration languages: %w", err)
	}

	if len(translitLangs) == 0 {
		log.Println("✅ No transliteration languages need processing")
		return nil
	}

	for _, lang := range translitLangs {
		log.Printf("Processing transliteration language: %s (%d prayers)", lang.Language, lang.PrayerCount)

		// Determine base language
		var baseLanguage string
		if strings.HasPrefix(lang.Language, "ar-") {
			baseLanguage = "ar"
		} else if strings.HasPrefix(lang.Language, "fa-") {
			baseLanguage = "fa"
		} else {
			log.Printf("⚠️  Unknown transliteration language: %s", lang.Language)
			continue
		}

		// Copy Phelps codes from base language to transliteration language
		// This assumes that transliteration texts correspond 1:1 with base language texts
		query := fmt.Sprintf(`
			UPDATE writings AS translit
			INNER JOIN writings AS base ON (
				base.language = '%s'
				AND base.phelps IS NOT NULL
				AND translit.name = base.name
			)
			SET translit.phelps = base.phelps
			WHERE translit.language = '%s' AND translit.phelps IS NULL
		`, baseLanguage, lang.Language)

		result, err := execDoltQuery(query)
		if err != nil {
			log.Printf("❌ Failed to update %s: %v", lang.Language, err)
			continue
		}

		log.Printf("✅ Updated %s using %s base language", lang.Language, baseLanguage)
		log.Printf("   Query result: %s", strings.TrimSpace(string(result)))
	}

	return nil
}
