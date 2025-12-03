package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
)

// TMP Code System - Temporary Phelps codes for unmatched prayers
// These codes allow matching against unidentified source prayers in en/ar/fa
// TMP codes are always overwritable - they get replaced when a real match is found

const (
	TMP_CODE_PREFIX = "TMP"
)

// AssignTMPCodes assigns temporary Phelps codes to unmatched prayers in en, ar, fa
func AssignTMPCodes() error {
	log.Println("🏷️  Assigning TMP codes to unmatched source prayers (en, ar, fa)...")

	languages := []string{"en", "ar", "fa"}
	totalAssigned := 0

	// Get the starting number ONCE for all languages to ensure global uniqueness
	nextNum, err := getNextTMPNumber()
	if err != nil {
		return fmt.Errorf("failed to get starting TMP number: %w", err)
	}

	for _, lang := range languages {
		count, newNextNum, err := assignTMPCodesForLanguageWithStart(lang, nextNum)
		if err != nil {
			log.Printf("❌ Failed to assign TMP codes for %s: %v", lang, err)
			continue
		}
		totalAssigned += count
		nextNum = newNextNum // Update for next language
		log.Printf("   ✅ %s: %d TMP codes assigned", lang, count)
	}

	log.Printf("✅ Total TMP codes assigned: %d", totalAssigned)
	return nil
}

// getNextTMPNumber finds the next available TMP code number
func getNextTMPNumber() (int, error) {
	query := `
		SELECT phelps
		FROM writings
		WHERE phelps LIKE 'TMP%'
		ORDER BY phelps DESC
		LIMIT 1
	`

	records, err := execDoltQueryCSV(query)
	if err != nil {
		return 1, fmt.Errorf("failed to query TMP codes: %w", err)
	}

	if len(records) <= 1 {
		return 1, nil // No TMP codes yet, start at 1
	}

	// Extract number from TMP code (format: TMP00001, TMP00002, etc.)
	lastCode := strings.Trim(records[1][0], `"`)
	numberPart := strings.TrimPrefix(lastCode, TMP_CODE_PREFIX)

	lastNum, err := strconv.Atoi(numberPart)
	if err != nil {
		// If we can't parse, start from 1
		return 1, nil
	}

	return lastNum + 1, nil
}

// assignTMPCodesForLanguageWithStart assigns TMP codes starting from a given number
func assignTMPCodesForLanguageWithStart(language string, startNum int) (int, int, error) {
	// Get all unmatched prayers for this language
	query := fmt.Sprintf(`
		SELECT version, name, text
		FROM writings
		WHERE language = '%s'
		  AND (phelps IS NULL OR phelps = '')
		  AND text != ''
		ORDER BY version
	`, language)

	records, err := execDoltQueryCSV(query)
	if err != nil {
		return 0, startNum, fmt.Errorf("failed to query unmatched prayers: %w", err)
	}

	if len(records) <= 1 {
		return 0, startNum, nil // No unmatched prayers (or only header)
	}

	nextNum := startNum
	assigned := 0

	for i := 1; i < len(records); i++ {
		if len(records[i]) < 3 {
			continue
		}

		version := strings.Trim(records[i][0], `"`)

		// Generate sequential TMP code
		tmpCode := fmt.Sprintf("%s%05d", TMP_CODE_PREFIX, nextNum)
		nextNum++

		// Assign the TMP code
		updateQuery := fmt.Sprintf(`
			UPDATE writings
			SET phelps = '%s'
			WHERE version = '%s' AND language = '%s'
		`,
			tmpCode,
			strings.ReplaceAll(version, "'", "''"),
			language,
		)

		if _, err := execDoltQuery(updateQuery); err != nil {
			log.Printf("⚠️  Failed to assign TMP code to %s: %v", version, err)
			continue
		}

		assigned++
	}

	// Return count and the next available number
	return assigned, nextNum, nil
}

// isTMPCode checks if a Phelps code is a temporary code
func isTMPCode(phelps string) bool {
	return strings.HasPrefix(phelps, TMP_CODE_PREFIX)
}

// BuildReferencesWithTMP builds reference collections including TMP-coded prayers
// Returns: English refs, Arabic refs, Persian refs
func BuildReferencesWithTMP(db Database) ([]EnglishReference, []EnglishReference, []EnglishReference) {
	var englishRefs []EnglishReference
	var arabicRefs []EnglishReference
	var persianRefs []EnglishReference

	// Build English references (including TMP codes)
	for _, w := range db.Writings {
		if w.Language == "en" && w.Phelps != "" && w.Text != "" {
			englishRefs = append(englishRefs, EnglishReference{
				Phelps:   w.Phelps,
				Name:     w.Name,
				Text:     w.Text,
				Category: w.Type,
			})
		}
	}

	// Build Arabic references (including TMP codes)
	for _, w := range db.Writings {
		if w.Language == "ar" && w.Phelps != "" && w.Text != "" {
			arabicRefs = append(arabicRefs, EnglishReference{
				Phelps:   w.Phelps,
				Name:     w.Name,
				Text:     w.Text,
				Category: w.Type,
			})
		}
	}

	// Build Persian references (including TMP codes)
	for _, w := range db.Writings {
		if w.Language == "fa" && w.Phelps != "" && w.Text != "" {
			persianRefs = append(persianRefs, EnglishReference{
				Phelps:   w.Phelps,
				Name:     w.Name,
				Text:     w.Text,
				Category: w.Type,
			})
		}
	}

	return englishRefs, arabicRefs, persianRefs
}

// CreateFallbackMatchingPrompt builds a prompt that tries en -> ar -> fa -> new TMP
func CreateFallbackMatchingPrompt(
	englishRefs, arabicRefs, persianRefs []PrayerFingerprint,
	targetFingerprints []PrayerFingerprint,
	targetLang string,
) string {
	var prompt strings.Builder

	prompt.WriteString("You are an expert in Bahá'í prayer matching with MULTI-TIER FALLBACK system.\n\n")

	prompt.WriteString("# THREE-TIER REFERENCE SYSTEM\n")
	prompt.WriteString("This system uses a fallback hierarchy:\n")
	prompt.WriteString("1. **PRIMARY**: English references (standard Phelps codes)\n")
	prompt.WriteString("2. **SECONDARY**: Arabic references (may have TMP codes)\n")
	prompt.WriteString("3. **TERTIARY**: Persian references (may have TMP codes)\n")
	prompt.WriteString("4. **FALLBACK**: Generate new TMP code if no match found\n\n")

	prompt.WriteString("# TMP CODE SYSTEM\n")
	prompt.WriteString("- **TMP codes** (format: TMP00001, TMP00002, etc.) are temporary identifiers\n")
	prompt.WriteString("- TMP codes are ALWAYS OVERWRITABLE - prefer real Phelps codes\n")
	prompt.WriteString("- If target prayer matches a TMP-coded prayer, use that TMP code\n")
	prompt.WriteString("- If no match anywhere, assign match_type: NEW_TMP_CODE\n")
	prompt.WriteString("- Matching the same TMP code means prayers are translations of each other\n\n")

	prompt.WriteString(fmt.Sprintf("# TARGET LANGUAGE: %s\n\n", strings.ToUpper(targetLang)))

	prompt.WriteString("# ENGLISH REFERENCES (Primary - Standard Phelps Codes)\n")
	prompt.WriteString("```json\n")
	engJSON, _ := json.MarshalIndent(englishRefs, "", "  ")
	prompt.WriteString(string(engJSON))
	prompt.WriteString("\n```\n\n")

	if len(arabicRefs) > 0 {
		prompt.WriteString("# ARABIC REFERENCES (Secondary - May include TMP codes)\n")
		prompt.WriteString("```json\n")
		arJSON, _ := json.MarshalIndent(arabicRefs, "", "  ")
		prompt.WriteString(string(arJSON))
		prompt.WriteString("\n```\n\n")
	}

	if len(persianRefs) > 0 {
		prompt.WriteString("# PERSIAN REFERENCES (Tertiary - May include TMP codes)\n")
		prompt.WriteString("```json\n")
		faJSON, _ := json.MarshalIndent(persianRefs, "", "  ")
		prompt.WriteString(string(faJSON))
		prompt.WriteString("\n```\n\n")
	}

	prompt.WriteString(fmt.Sprintf("# TARGET %s FINGERPRINTS\n", strings.ToUpper(targetLang)))
	prompt.WriteString("```json\n")
	targetJSON, _ := json.MarshalIndent(targetFingerprints, "", "  ")
	prompt.WriteString(string(targetJSON))
	prompt.WriteString("\n```\n\n")

	prompt.WriteString("# MATCHING STRATEGY (Fallback Order)\n")
	prompt.WriteString("1. **Try English first**: Match against English references\n")
	prompt.WriteString("2. **Fall back to Arabic**: If no English match, try Arabic references\n")
	prompt.WriteString("3. **Fall back to Persian**: If no Arabic match, try Persian references\n")
	prompt.WriteString("4. **Generate TMP code**: If no match found in any tier, set match_type: NEW_TMP_CODE\n\n")

	prompt.WriteString("# MATCHING CRITERIA (Same as before)\n")
	prompt.WriteString("- **EXACT**: text_hash match (confidence: 100%)\n")
	prompt.WriteString("- **LIKELY**: Advanced features match (confidence: 80-95%)\n")
	prompt.WriteString("- **AMBIGUOUS**: Multiple candidates (confidence: 60-75%)\n")
	prompt.WriteString("- **NEW_TMP_CODE**: No match in any tier (confidence: N/A)\n\n")

	prompt.WriteString("# OUTPUT FORMAT\n")
	prompt.WriteString("```json\n")
	prompt.WriteString(`{
  "matches": [
    {
      "phelps": "BH00123",
      "target_version": "uuid-123",
      "match_type": "EXACT",
      "confidence": 100,
      "match_reasons": ["text_hash_match"],
      "reference_tier": "english"
    },
    {
      "phelps": "TMP00042",
      "target_version": "uuid-456",
      "match_type": "LIKELY",
      "confidence": 85,
      "match_reasons": ["longest_words_match", "structure_hash_match"],
      "reference_tier": "arabic"
    },
    {
      "phelps": "",
      "target_version": "uuid-789",
      "match_type": "NEW_TMP_CODE",
      "confidence": 0,
      "match_reasons": ["no_match_any_tier"],
      "reference_tier": "none"
    }
  ],
  "exact_matches": 45,
  "likely_matches": 67,
  "ambiguous_count": 12,
  "new_tmp_codes": 8,
  "summary": "Processed 132 prayers: 45 exact, 67 likely, 12 ambiguous, 8 need new TMP codes"
}`)
	prompt.WriteString("\n```\n\n")

	prompt.WriteString("# CRITICAL RULES\n")
	prompt.WriteString("1. **Prefer real Phelps over TMP codes** - Real codes (e.g., BH00123) > TMP codes\n")
	prompt.WriteString("2. **TMP codes are overwritable** - If you find a better match, use it\n")
	prompt.WriteString("3. **Include reference_tier** - Specify which tier matched: 'english', 'arabic', 'persian', or 'none'\n")
	prompt.WriteString("4. **NEW_TMP_CODE for orphans** - Prayers with no match get match_type: NEW_TMP_CODE\n")
	prompt.WriteString("5. **TMP codes link translations** - Same TMP code = same prayer in different languages\n")
	prompt.WriteString("6. **Return ONLY JSON** - No explanations, no code\n\n")

	return prompt.String()
}

// GenerateNewTMPCode creates a new TMP code for unmatched target prayers
func GenerateNewTMPCode() (string, error) {
	nextNum, err := getNextTMPNumber()
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%s%05d", TMP_CODE_PREFIX, nextNum), nil
}

// ApplyTMPMatches processes results and generates TMP codes for unmatched prayers
func ApplyTMPMatches(language string, results CompressedBatchResponse) error {
	exactCount := 0
	likelyCount := 0
	tmpCount := 0
	newTmpCount := 0

	for _, match := range results.Matches {
		switch match.MatchType {
		case "EXACT":
			if match.Confidence >= 95 {
				// Apply the match (could be real Phelps or TMP code)
				query := fmt.Sprintf("UPDATE writings SET phelps = '%s' WHERE version = '%s' AND language = '%s'",
					strings.ReplaceAll(match.EnglishPhelps, "'", "''"),
					strings.ReplaceAll(match.TargetVersion, "'", "''"),
					language)

				if _, err := execDoltQuery(query); err != nil {
					log.Printf("ERROR updating %s: %v", match.TargetVersion, err)
					continue
				}

				if isTMPCode(match.EnglishPhelps) {
					tmpCount++
				} else {
					exactCount++
				}
			}

		case "LIKELY":
			if match.Confidence >= 80 {
				query := fmt.Sprintf("UPDATE writings SET phelps = '%s' WHERE version = '%s' AND language = '%s'",
					strings.ReplaceAll(match.EnglishPhelps, "'", "''"),
					strings.ReplaceAll(match.TargetVersion, "'", "''"),
					language)

				if _, err := execDoltQuery(query); err != nil {
					log.Printf("ERROR updating %s: %v", match.TargetVersion, err)
					continue
				}

				if isTMPCode(match.EnglishPhelps) {
					tmpCount++
				} else {
					likelyCount++
				}
			}

		case "NEW_TMP_CODE":
			// Generate and assign a new TMP code for this prayer
			newTmpCode, err := GenerateNewTMPCode()
			if err != nil {
				log.Printf("ERROR generating TMP code: %v", err)
				continue
			}

			query := fmt.Sprintf("UPDATE writings SET phelps = '%s' WHERE version = '%s' AND language = '%s'",
				newTmpCode,
				strings.ReplaceAll(match.TargetVersion, "'", "''"),
				language)

			if _, err := execDoltQuery(query); err != nil {
				log.Printf("ERROR assigning TMP code to %s: %v", match.TargetVersion, err)
				continue
			}

			newTmpCount++
			log.Printf("   🆕 Generated new TMP code: %s for %s", newTmpCode, match.TargetVersion)
		}
	}

	log.Printf("✅ %s: %d exact, %d likely, %d TMP codes used, %d new TMP codes generated",
		language, exactCount, likelyCount, tmpCount, newTmpCount)

	return nil
}
