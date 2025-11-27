package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

// PrayerFingerprint contains compressed semantic information about a prayer
type PrayerFingerprint struct {
	Phelps   string `json:"phelps,omitempty"`  // English reference only
	Version  string `json:"version,omitempty"` // Target language only
	Language string `json:"language"`

	// Semantic markers
	OpeningPhrase string   `json:"opening"`   // First 8-12 words
	ClosingPhrase string   `json:"closing"`   // Last 6-8 words
	KeyTerms      []string `json:"key_terms"` // Important theological terms
	WordCount     int      `json:"word_count"`
	CharCount     int      `json:"char_count"`

	// Structural markers
	HasInvocation   bool   `json:"has_invocation"`   // Contains "O God", "O Lord", etc.
	HasBlessings    bool   `json:"has_blessings"`    // Contains blessing language
	HasSupplication bool   `json:"has_supplication"` // Contains requests/petitions
	PrayerType      string `json:"type"`             // devotional, healing, protection, etc.

	// Quick match hints
	TextHash       string   `json:"text_hash"` // MD5 of normalized text
	SignatureWords []string `json:"signature"` // Most distinctive 3-5 words

	// Debug info
	Name string `json:"name,omitempty"`
}

// CompressedMatchRequest represents bulk fingerprint matching
type CompressedMatchRequest struct {
	EnglishFingerprints []PrayerFingerprint `json:"english_refs"`
	TargetFingerprints  []PrayerFingerprint `json:"target_prayers"`
	TargetLanguage      string              `json:"target_language"`
	ProcessingMode      string              `json:"mode"` // "bulk_match", "ambiguous_only"
}

// CompressedMatchResult represents the result of fingerprint matching
type CompressedMatchResult struct {
	EnglishPhelps   string   `json:"phelps"`
	TargetVersion   string   `json:"target_version"`
	MatchType       string   `json:"match_type"` // EXACT, LIKELY, AMBIGUOUS, NEW_TRANSLATION
	Confidence      int      `json:"confidence"`
	MatchReasons    []string `json:"match_reasons"`
	AmbiguityReason string   `json:"ambiguity_reason,omitempty"`
}

// CompressedBatchResponse is the LLM response for bulk matching
type CompressedBatchResponse struct {
	Matches         []CompressedMatchResult `json:"matches"`
	ExactMatches    int                     `json:"exact_matches"`
	LikelyMatches   int                     `json:"likely_matches"`
	AmbiguousCount  int                     `json:"ambiguous_count"`
	NewTranslations int                     `json:"new_translations"`
	Summary         string                  `json:"summary"`
}

// Theological terms that are important for matching across languages
var theologicalTerms = []string{
	"god", "lord", "bahá", "glory", "kingdom", "mercy", "grace", "praise",
	"prayer", "supplication", "blessing", "protection", "guidance", "forgiveness",
	"light", "divine", "holy", "sacred", "eternal", "almighty", "creator",
	"servants", "faithful", "believers", "covenant", "manifestation", "revelation",
	"tablet", "kitab", "aqdas", "abhá", "yá", "alláh", "dios", "señor",
	"allah", "rabb", "ilahi", "dieu", "seigneur", "gott", "herr", "iddio",
	"signore", "deus", "senhor", "heer", "gud", "jumala",
}

// Prayer type indicators
var prayerTypeKeywords = map[string][]string{
	"devotional":  {"praise", "glory", "exalted", "magnified", "worship"},
	"healing":     {"heal", "healing", "health", "cure", "remedy", "physician"},
	"protection":  {"protect", "protection", "shield", "guard", "safety", "refuge"},
	"guidance":    {"guide", "guidance", "path", "way", "direction", "wisdom"},
	"forgiveness": {"forgive", "forgiveness", "mercy", "pardon", "sin"},
	"unity":       {"unity", "union", "oneness", "together", "brotherhood"},
	"assistance":  {"assist", "help", "aid", "support", "strength", "enable"},
	"gratitude":   {"thank", "grateful", "gratitude", "thankful", "appreciation"},
}

// CreatePrayerFingerprint generates a compressed semantic fingerprint of a prayer
func CreatePrayerFingerprint(phelps, version, language, name, text string) PrayerFingerprint {
	normalized := normalizeText(text)
	words := strings.Fields(normalized)

	fingerprint := PrayerFingerprint{
		Phelps:    phelps,
		Version:   version,
		Language:  language,
		Name:      name,
		WordCount: len(words),
		CharCount: len(normalized),
		TextHash:  textHash(normalized),
	}

	// Extract opening phrase (first 8-12 words, or until punctuation)
	fingerprint.OpeningPhrase = extractOpeningPhrase(words)

	// Extract closing phrase (last 6-8 words)
	fingerprint.ClosingPhrase = extractClosingPhrase(words)

	// Find key theological terms
	fingerprint.KeyTerms = findKeyTerms(normalized)

	// Detect structural markers
	fingerprint.HasInvocation = hasInvocation(normalized)
	fingerprint.HasBlessings = hasBlessings(normalized)
	fingerprint.HasSupplication = hasSupplication(normalized)

	// Determine prayer type
	fingerprint.PrayerType = determinePrayerType(normalized)

	// Extract signature words (most distinctive terms)
	fingerprint.SignatureWords = extractSignatureWords(words, fingerprint.KeyTerms)

	return fingerprint
}

// normalizeText cleans and normalizes text for comparison
func normalizeText(text string) string {
	// Remove HTML tags, extra whitespace, and normalize unicode
	text = regexp.MustCompile(`<[^>]*>`).ReplaceAllString(text, "")
	text = regexp.MustCompile(`\s+`).ReplaceAllString(text, " ")
	text = strings.ToLower(strings.TrimSpace(text))

	// Remove common punctuation but keep structure
	text = regexp.MustCompile(`[""''`+"`]").ReplaceAllString(text, "'")
	text = regexp.MustCompile(`[-–—]`).ReplaceAllString(text, "-")

	return text
}

// textHash creates MD5 hash of normalized text for exact matching
func textHash(text string) string {
	// Remove all punctuation and spacing for hash
	clean := regexp.MustCompile(`[^\p{L}\p{N}]`).ReplaceAllString(text, "")
	hash := md5.Sum([]byte(clean))
	return hex.EncodeToString(hash[:])[:12] // First 12 chars
}

// extractOpeningPhrase gets the meaningful opening words
func extractOpeningPhrase(words []string) string {
	if len(words) == 0 {
		return ""
	}

	maxWords := 12
	if len(words) < maxWords {
		maxWords = len(words)
	}

	// Look for natural stopping points
	for i := 4; i < maxWords; i++ {
		word := words[i]
		if strings.HasSuffix(word, ".") || strings.HasSuffix(word, "!") ||
			strings.HasSuffix(word, ",") && i > 6 {
			return strings.Join(words[:i+1], " ")
		}
	}

	return strings.Join(words[:maxWords], " ")
}

// extractClosingPhrase gets the meaningful closing words
func extractClosingPhrase(words []string) string {
	if len(words) == 0 {
		return ""
	}

	maxWords := 8
	startIdx := len(words) - maxWords
	if startIdx < 0 {
		startIdx = 0
	}

	return strings.Join(words[startIdx:], " ")
}

// findKeyTerms identifies important theological and distinctive terms
func findKeyTerms(text string) []string {
	found := make(map[string]bool)
	words := strings.Fields(text)

	// Find theological terms
	for _, word := range words {
		clean := strings.Trim(word, ".,!?;:")
		for _, term := range theologicalTerms {
			if strings.Contains(clean, term) ||
				(len(clean) > 3 && strings.Contains(term, clean)) {
				found[term] = true
			}
		}
	}

	// Add distinctive capitalized words (likely proper nouns)
	for _, word := range words {
		if len(word) > 3 && unicode.IsUpper(rune(word[0])) {
			clean := strings.ToLower(strings.Trim(word, ".,!?;:"))
			if !isCommonWord(clean) {
				found[clean] = true
			}
		}
	}

	// Convert to sorted slice
	var terms []string
	for term := range found {
		terms = append(terms, term)
	}
	sort.Strings(terms)

	// Limit to most important terms
	if len(terms) > 15 {
		return terms[:15]
	}
	return terms
}

// hasInvocation checks for invocational language
func hasInvocation(text string) bool {
	invocations := []string{
		"o god", "o lord", "oh god", "oh lord", "o my god",
		"yá alláh", "yá rabb", "oh señor", "ô dieu", "o herr",
		"o iddio", "ó deus", "o heer", "allāhumma",
	}

	for _, inv := range invocations {
		if strings.Contains(text, inv) {
			return true
		}
	}
	return false
}

// hasBlessings checks for blessing language
func hasBlessings(text string) bool {
	blessings := []string{
		"bless", "blessed", "blessing", "bendice", "bendición",
		"bénir", "béni", "segne", "gesegnet", "benedici", "benedetto",
		"abençoe", "abençoado", "zegen", "gezegend",
	}

	for _, blessing := range blessings {
		if strings.Contains(text, blessing) {
			return true
		}
	}
	return false
}

// hasSupplication checks for petitionary language
func hasSupplication(text string) bool {
	supplications := []string{
		"grant", "give", "bestow", "aid", "help", "assist", "enable",
		"concede", "otorga", "da", "ayuda", "accorde", "donne", "hilf",
		"gib", "concedi", "dai", "concede", "dá", "ajuda", "verleen", "geef",
	}

	for _, sup := range supplications {
		if strings.Contains(text, sup) {
			return true
		}
	}
	return false
}

// determinePrayerType categorizes the prayer based on content
func determinePrayerType(text string) string {
	maxScore := 0
	bestType := "general"

	for prayerType, keywords := range prayerTypeKeywords {
		score := 0
		for _, keyword := range keywords {
			if strings.Contains(text, keyword) {
				score++
			}
		}
		if score > maxScore {
			maxScore = score
			bestType = prayerType
		}
	}

	return bestType
}

// extractSignatureWords finds the most distinctive words
func extractSignatureWords(words []string, keyTerms []string) []string {
	// Combine key terms with distinctive words from the text
	wordCount := make(map[string]int)
	for _, word := range words {
		clean := strings.ToLower(strings.Trim(word, ".,!?;:"))
		if len(clean) > 3 && !isCommonWord(clean) {
			wordCount[clean]++
		}
	}

	// Add key terms
	signature := make(map[string]bool)
	for _, term := range keyTerms {
		signature[term] = true
	}

	// Add most frequent distinctive words
	type wordFreq struct {
		word string
		freq int
	}
	var frequencies []wordFreq
	for word, freq := range wordCount {
		if freq >= 2 { // Appears at least twice
			frequencies = append(frequencies, wordFreq{word, freq})
		}
	}

	// Sort by frequency
	sort.Slice(frequencies, func(i, j int) bool {
		return frequencies[i].freq > frequencies[j].freq
	})

	// Add top frequent words
	for i := 0; i < len(frequencies) && i < 5; i++ {
		signature[frequencies[i].word] = true
	}

	// Convert to sorted slice
	var result []string
	for word := range signature {
		result = append(result, word)
	}
	sort.Strings(result)

	// Limit to 8 signature words
	if len(result) > 8 {
		return result[:8]
	}
	return result
}

// isCommonWord checks if a word is too common to be distinctive
func isCommonWord(word string) bool {
	common := []string{
		"the", "and", "or", "but", "in", "on", "at", "to", "for", "of", "with",
		"by", "from", "up", "about", "into", "through", "during", "before",
		"after", "above", "below", "between", "among", "this", "that", "these",
		"those", "his", "her", "its", "our", "your", "their", "he", "she", "it",
		"we", "you", "they", "him", "them", "us", "me", "my", "mine", "yours",
		"ours", "theirs", "who", "what", "where", "when", "why", "how", "which",
		"all", "any", "some", "many", "much", "more", "most", "other", "another",
		"such", "very", "just", "even", "also", "only", "first", "last", "long",
		"great", "little", "own", "good", "new", "old", "right", "big", "small",
		"large", "next", "early", "young", "important", "few", "public", "same",
	}

	for _, c := range common {
		if word == c {
			return true
		}
	}
	return false
}

// CreateCompressedMatchingPrompt builds an efficient bulk matching prompt
func CreateCompressedMatchingPrompt(englishFingerprints, targetFingerprints []PrayerFingerprint, targetLang, mode string) string {
	var prompt strings.Builder

	prompt.WriteString("You are an expert in Bahá'í prayer matching using semantic fingerprints.\n\n")

	prompt.WriteString("# TASK\n")
	prompt.WriteString(fmt.Sprintf("Match %s prayer fingerprints to English reference fingerprints.\n", targetLang))
	prompt.WriteString("Each fingerprint contains compressed semantic information instead of full text.\n\n")

	if mode == "bulk_match" {
		prompt.WriteString("# MODE: BULK MATCHING\n")
		prompt.WriteString("Process ALL fingerprints efficiently. Mark ambiguous cases for detailed review.\n\n")
	} else {
		prompt.WriteString("# MODE: AMBIGUOUS RESOLUTION\n")
		prompt.WriteString("Focus on resolving previously flagged ambiguous matches.\n\n")
	}

	prompt.WriteString("# ENGLISH REFERENCE FINGERPRINTS\n")
	prompt.WriteString("```json\n")
	englishJSON, _ := json.MarshalIndent(englishFingerprints, "", "  ")
	prompt.WriteString(string(englishJSON))
	prompt.WriteString("\n```\n\n")

	prompt.WriteString(fmt.Sprintf("# TARGET %s FINGERPRINTS\n", strings.ToUpper(targetLang)))
	prompt.WriteString("```json\n")
	targetJSON, _ := json.MarshalIndent(targetFingerprints, "", "  ")
	prompt.WriteString(string(targetJSON))
	prompt.WriteString("\n```\n\n")

	prompt.WriteString("# MATCHING STRATEGY\n")
	prompt.WriteString("1. EXACT: text_hash matches (identical prayers)\n")
	prompt.WriteString("2. LIKELY: opening/closing phrases + key_terms + word_count align well\n")
	prompt.WriteString("3. AMBIGUOUS: multiple possible matches or unclear correspondence\n")
	prompt.WriteString("4. NEW_TRANSLATION: no reasonable match exists in target language\n\n")

	prompt.WriteString("# OUTPUT FORMAT\n")
	prompt.WriteString("```json\n")
	prompt.WriteString(`{
  "matches": [
    {
      "phelps": "AB00001",
      "target_version": "uuid-123",
      "match_type": "EXACT|LIKELY|AMBIGUOUS|NEW_TRANSLATION",
      "confidence": 95,
      "match_reasons": ["text_hash_match", "opening_phrase_similar", "key_terms_overlap"],
      "ambiguity_reason": "multiple candidates with similar signatures"
    }
  ],
  "exact_matches": 45,
  "likely_matches": 67,
  "ambiguous_count": 12,
  "new_translations": 31,
  "summary": "Processed 155 prayers: 45 exact, 67 likely, 12 need review, 31 new translations needed"
}`)
	prompt.WriteString("\n```\n\n")

	prompt.WriteString("# RULES\n")
	prompt.WriteString("- Prioritize exact text_hash matches (confidence: 100)\n")
	prompt.WriteString("- Strong opening_phrase similarity + key_terms overlap = LIKELY (confidence: 80-95)\n")
	prompt.WriteString("- Mark as AMBIGUOUS if multiple good candidates exist (confidence: 50-79)\n")
	prompt.WriteString("- NEW_TRANSLATION only if no reasonable target candidate exists\n")
	prompt.WriteString("- Include specific match_reasons for each decision\n")
	prompt.WriteString("- Be efficient: process all fingerprints in one response\n\n")

	prompt.WriteString("Begin bulk matching. Return only the JSON response.\n")

	return prompt.String()
}

// ProcessCompressedResults handles the compressed matching results
func ProcessCompressedResults(results CompressedBatchResponse, targetLang string) (int, int, int, error) {
	exactCount := 0
	likelyCount := 0
	ambiguousCount := 0

	log.Printf("Processing compressed results for %s:", targetLang)
	log.Printf("  - Exact matches: %d", results.ExactMatches)
	log.Printf("  - Likely matches: %d", results.LikelyMatches)
	log.Printf("  - Ambiguous cases: %d", results.AmbiguousCount)
	log.Printf("  - New translations: %d", results.NewTranslations)

	for _, match := range results.Matches {
		switch match.MatchType {
		case "EXACT":
			// High confidence updates
			if match.Confidence >= 95 {
				query := fmt.Sprintf("UPDATE writings SET phelps = '%s' WHERE version = '%s' AND language = '%s'",
					strings.ReplaceAll(match.EnglishPhelps, "'", "''"),
					strings.ReplaceAll(match.TargetVersion, "'", "''"),
					targetLang)

				if _, err := execDoltQuery(query); err != nil {
					log.Printf("ERROR updating %s: %v", match.TargetVersion, err)
					continue
				}
				exactCount++
			}

		case "LIKELY":
			// Medium confidence updates
			if match.Confidence >= 80 {
				query := fmt.Sprintf("UPDATE writings SET phelps = '%s' WHERE version = '%s' AND language = '%s'",
					strings.ReplaceAll(match.EnglishPhelps, "'", "''"),
					strings.ReplaceAll(match.TargetVersion, "'", "''"),
					targetLang)

				if _, err := execDoltQuery(query); err != nil {
					log.Printf("ERROR updating %s: %v", match.TargetVersion, err)
					continue
				}
				likelyCount++
			}

		case "AMBIGUOUS":
			// Log for manual review or detailed processing
			log.Printf("AMBIGUOUS: %s -> %s (reason: %s)",
				match.EnglishPhelps, match.TargetVersion, match.AmbiguityReason)
			ambiguousCount++

		case "NEW_TRANSLATION":
			// Could create placeholder entries or log for translation
			log.Printf("NEW_TRANSLATION needed: %s", match.EnglishPhelps)
		}
	}

	return exactCount, likelyCount, ambiguousCount, nil
}

// CompressedLanguageMatching performs efficient bulk matching for a language
func CompressedLanguageMatching(targetLang string) error {
	log.Printf("Starting compressed matching for language: %s", targetLang)

	// Load database
	db, err := GetDatabase()
	if err != nil {
		return fmt.Errorf("failed to load database: %w", err)
	}

	// Get English references
	englishRefs := BuildEnglishReference(db)

	// Get target language prayers
	targetPrayers := BuildTargetPrayers(db, targetLang)

	log.Printf("Loaded %d English refs, %d %s prayers",
		len(englishRefs), len(targetPrayers), targetLang)

	// Create fingerprints
	var englishFingerprints []PrayerFingerprint
	for _, ref := range englishRefs {
		fp := CreatePrayerFingerprint(ref.Phelps, "", "en", ref.Name, ref.Text)
		englishFingerprints = append(englishFingerprints, fp)
	}

	var targetFingerprints []PrayerFingerprint
	for _, prayer := range targetPrayers {
		fp := CreatePrayerFingerprint("", prayer.Version, targetLang, prayer.Name, prayer.Text)
		targetFingerprints = append(targetFingerprints, fp)
	}

	log.Printf("Created %d English + %d target fingerprints",
		len(englishFingerprints), len(targetFingerprints))

	// Create bulk matching prompt
	prompt := CreateCompressedMatchingPrompt(englishFingerprints, targetFingerprints, targetLang, "bulk_match")

	log.Printf("Calling Claude for compressed bulk matching...")
	response, err := CallClaude(prompt, 8000)
	if err != nil {
		return fmt.Errorf("Claude API call failed: %w", err)
	}

	// Parse response
	var results CompressedBatchResponse
	if err := json.Unmarshal([]byte(response), &results); err != nil {
		// Save failed response for later analysis
		failedResponseFile := fmt.Sprintf("failed_response_%s_%d.txt",
			targetLang,
			time.Now().Unix())
		if writeErr := os.WriteFile(failedResponseFile, []byte(response), 0644); writeErr == nil {
			log.Printf("❌ Parse failed, saved response to: %s", failedResponseFile)
		}
		return fmt.Errorf("failed to parse response (saved to %s): %w", failedResponseFile, err)
	}

	// Process results
	exactCount, likelyCount, ambiguousCount, err := ProcessCompressedResults(results, targetLang)
	if err != nil {
		return fmt.Errorf("failed to process results: %w", err)
	}

	log.Printf("Compressed matching completed for %s:", targetLang)
	log.Printf("  - Exact matches processed: %d", exactCount)
	log.Printf("  - Likely matches processed: %d", likelyCount)
	log.Printf("  - Ambiguous cases logged: %d", ambiguousCount)

	return nil
}
