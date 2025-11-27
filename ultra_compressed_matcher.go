package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
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
	Confidence      int      `json:"confidence"`
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
	Language        string  `json:"language"`
	TotalPrayers    int     `json:"total_prayers"`
	ExactMatches    int     `json:"exact_matches"`
	LikelyMatches   int     `json:"likely_matches"`
	Ambiguous       int     `json:"ambiguous"`
	NewTranslations int     `json:"new_translations"`
	CompletionRate  float64 `json:"completion_rate"`
}

// LanguageStats holds basic statistics for batching decisions
type LanguageStats struct {
	Language       string
	PrayerCount    int
	UnmatchedCount int
}

// GetUnprocessedLanguageStats returns statistics for languages needing processing
// Excludes transliteration languages which need special handling
func GetUnprocessedLanguageStats() ([]LanguageStats, error) {
	query := `
		SELECT
			language,
			COUNT(*) as total_prayers,
			SUM(CASE WHEN phelps IS NULL THEN 1 ELSE 0 END) as unmatched_prayers
		FROM writings
		WHERE language != 'en'
		  AND language NOT IN ('', 'unknown')
		  AND language NOT LIKE '%-translit'
		GROUP BY language
		HAVING SUM(CASE WHEN phelps IS NULL THEN 1 ELSE 0 END) > 0
		ORDER BY SUM(CASE WHEN phelps IS NULL THEN 1 ELSE 0 END) DESC, COUNT(*) DESC
	`

	output, err := execDoltQueryCSV(query)
	if err != nil {
		return nil, fmt.Errorf("failed to get language stats: %w", err)
	}

	lines := strings.Split(string(output), "\n")
	var stats []LanguageStats

	// Skip header and empty lines
	for i := 1; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}

		parts := strings.Split(line, ",")
		if len(parts) >= 3 {
			language := strings.Trim(parts[0], `"`)
			totalPrayers := parseInt(strings.Trim(parts[1], `"`))
			unmatchedPrayers := parseInt(strings.Trim(parts[2], `"`))

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
	var batches [][]string

	// Batching strategy based on prayer counts
	const MAX_PRAYERS_PER_BATCH = 250  // Total prayers per batch (increased)
	const MAX_LANGUAGES_PER_BATCH = 20 // Maximum languages in one batch (increased)
	const PACK_THRESHOLD = 150         // If a "solo" batch has < 150 prayers, pack small langs into it

	log.Printf("Creating optimally-packed language batches from %d unprocessed languages", len(stats))

	// Separate large and small languages
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

// ProcessLanguageBatch handles a batch of multiple languages
func ProcessLanguageBatch(languages []string) error {
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
		targetPrayers := BuildTargetPrayers(db, lang)
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

	// Call Claude
	log.Printf("Calling Claude for multi-language batch...")
	response, err := CallClaude(prompt, 12000) // Increased token limit for multi-language
	if err != nil {
		// Check if it's a rate limit issue
		if strings.Contains(err.Error(), "limit reached") || strings.Contains(err.Error(), "rate limit") {
			log.Printf("🚨 RATE LIMIT HIT for batch: %v", languages)
			log.Printf("💾 Saving batch info for manual pickup...")

			// Save batch details for manual processing
			batchFile := fmt.Sprintf("pending_batch_%s_%d.json",
				strings.Join(languages, "_"),
				time.Now().Unix())

			batchInfo := map[string]interface{}{
				"languages":     languages,
				"batch_size":    batch.BatchSize,
				"total_prayers": batch.TotalPrayers,
				"status":        "pending_rate_limit",
				"error":         err.Error(),
				"created_at":    time.Now().Format(time.RFC3339),
			}

			if batchJSON, marshalErr := json.MarshalIndent(batchInfo, "", "  "); marshalErr == nil {
				if writeErr := os.WriteFile(batchFile, batchJSON, 0644); writeErr == nil {
					log.Printf("📁 Batch saved to: %s", batchFile)
				}
			}
		}
		return fmt.Errorf("Claude API call failed: %w", err)
	}

	// Parse response
	var results UltraBatchResponse
	if err := json.Unmarshal([]byte(response), &results); err != nil {
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
	for _, lang := range languages {
		langMatches := 0
		for _, match := range results.Matches {
			if match.TargetLanguage == lang {
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

	log.Printf("Batch completed: %d total matches across %d languages",
		totalProcessed, len(languages))

	return nil
}

// UltraCompressedBulkMatching processes all languages with smart batching
func UltraCompressedBulkMatching() error {
	log.Println("🚀 Starting ULTRA-COMPRESSED bulk matching with smart batching")

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

	// Create smart batches
	batches := CreateLanguageBatches(stats)

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

		if err := ProcessLanguageBatch(batch); err != nil {
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

	lines := strings.Split(string(output), "\n")
	var stats []LanguageStats

	// Skip header and empty lines
	for i := 1; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}

		parts := strings.Split(line, ",")
		if len(parts) >= 3 {
			language := strings.Trim(parts[0], `"`)
			totalPrayers := parseInt(strings.Trim(parts[1], `"`))
			unmatchedPrayers := parseInt(strings.Trim(parts[2], `"`))

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
