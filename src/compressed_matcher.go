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

// Chinese theological characters for enhanced matching
var chineseTheologicalChars = map[rune]bool{
	'神': true, '主': true, '天': true, '圣': true, '灵': true, // God, Lord, Heaven, Holy, Spirit
	'祈': true, '祷': true, '求': true, '赐': true, '恩': true, // Pray, Prayer, Ask, Grant, Grace
	'慈': true, '悲': true, '爱': true, '光': true, '荣': true, // Mercy, Compassion, Love, Light, Glory
	'巴': true, '哈': true, // Bahá (specific to Bahá'í prayers)
}

// Traditional to Simplified Chinese character mappings for cross-script matching
var traditionalToSimplified = map[rune]rune{
	'經': '经', '學': '学', '國': '国', '門': '门', '來': '来', '時': '时', '無': '无', '長': '长',
	'會': '会', '現': '现', '開': '开', '關': '关', '東': '东', '車': '车', '見': '见', '說': '说',
	'語': '语', '電': '电', '話': '话', '網': '网', '間': '间', '問': '问', '題': '题', '業': '业',
	'個': '个', '們': '们', '對': '对', '應': '应', '還': '还', '這': '这', '樣': '样', '點': '点',
	'實': '实', '種': '种', '類': '类', '處': '处', '進': '进', '運': '运', '動': '动', '過': '过',
	'發': '发', '場': '场', '機': '机', '聖': '圣', '靈': '灵', '書': '书', '義': '义', '愛': '爱',
	'賜': '赐', '禱': '祷', '讚': '赞', '頌': '颂', '榮': '荣', '耀': '耀', '僕': '仆', '團': '团',
	'轉': '转', '顯': '显', '徵': '征', '啟': '启', '潛': '潜', '於': '于',
}

// PrayerFingerprint contains compressed semantic information about a prayer
type PrayerFingerprint struct {
	Phelps   string `json:"phelps,omitempty"`  // English reference only
	Version  string `json:"version,omitempty"` // Target language only
	Language string `json:"language"`

	// PRIMARY MATCHING (Language-agnostic)
	TextHash      string `json:"text_hash"`      // MD5 of normalized text - MOST RELIABLE for cross-language
	WordCount     int    `json:"word_count"`     // Prayer length indicator
	CharCount     int    `json:"char_count"`     // Text size indicator
	StructureHash string `json:"structure_hash"` // Hash of prayer structure (paragraphs, verses)

	// SECONDARY MATCHING (Language-aware)
	OpeningPhrase  string   `json:"opening"`   // First 8-12 words (null for non-Latin scripts)
	ClosingPhrase  string   `json:"closing"`   // Last 6-8 words (null for non-Latin scripts)
	KeyTerms       []string `json:"key_terms"` // Theological terms (null for cross-language matching)
	SignatureWords []string `json:"signature"` // Most distinctive words (null for cross-language)

	// ADVANCED MATCHING (Cross-language/Cross-script)
	LongestWords     []string          `json:"longest_words"`     // 3-5 longest words (often proper nouns/theological terms)
	RareCharacters   []string          `json:"rare_characters"`   // Uncommon kanji/hanzi/characters for CJK languages
	RecurringPhrases []RecurringPhrase `json:"recurring_phrases"` // Repeated expressions with frequency
	UniqueSequences  []string          `json:"unique_sequences"`  // Distinctive 3-4 word sequences

	// STRUCTURAL MARKERS (Language-agnostic)
	HasInvocation   bool   `json:"has_invocation"`   // Contains invocation pattern
	HasBlessings    bool   `json:"has_blessings"`    // Contains blessing pattern
	HasSupplication bool   `json:"has_supplication"` // Contains petition pattern
	PrayerType      string `json:"type"`             // devotional, healing, protection, etc.
	ParagraphCount  int    `json:"paragraph_count"`  // Number of text blocks
	VersePattern    string `json:"verse_pattern"`    // "single", "multiple", "responsive"

	// PHONETIC HINTS (Cross-language support)
	PhoneticTerms []string `json:"phonetic_terms,omitempty"` // Transliterated key terms for cross-script matching

	// EDGE CASE HANDLING
	FullText       string `json:"full_text,omitempty"` // Complete prayer for short texts or low-resource languages
	IsShortPrayer  bool   `json:"is_short_prayer"`     // Less than 50 words
	IsRareLanguage bool   `json:"is_rare_language"`    // Language has <30 total prayers

	// Debug info
	Name string `json:"name,omitempty"`
}

// RecurringPhrase represents a phrase that appears multiple times in a prayer
type RecurringPhrase struct {
	Phrase    string `json:"phrase"`    // The repeated text
	Count     int    `json:"count"`     // How many times it appears
	Positions []int  `json:"positions"` // Character positions where it occurs
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
	TargetLanguage  string   `json:"target_language,omitempty"` // Language code for validation
	MatchType       string   `json:"match_type"`                // EXACT, LIKELY, AMBIGUOUS, NEW_TRANSLATION
	Confidence      float64  `json:"confidence"`
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
	// For Chinese, apply variant normalization before general normalization
	processedText := text
	if isChinese(language) {
		processedText = normalizeChineseVariants(text)
	}

	normalized := normalizeText(processedText)

	// Use Chinese-aware processing for Chinese languages
	var words []string
	var wordCount int
	var charCount int

	if isChinese(language) {
		wordCount = getChineseWordCount(text, language)
		charCount = len([]rune(normalized))
		// For Chinese, we'll handle word extraction differently
		words = []string{} // Empty for Chinese processing
	} else {
		words = strings.Fields(normalized)
		wordCount = len(words)
		charCount = len(normalized)
	}

	fingerprint := PrayerFingerprint{
		Phelps:    phelps,
		Version:   version,
		Language:  language,
		Name:      name,
		WordCount: wordCount,
		CharCount: charCount,
		TextHash:  textHash(normalized),
	}

	// Edge case detection
	fingerprint.IsShortPrayer = len(words) < 50
	fingerprint.IsRareLanguage = isRareLanguage(language)

	// Extract opening phrase (Chinese-aware)
	if isChinese(language) {
		fingerprint.OpeningPhrase = extractChineseOpeningPhrase(normalized)
		fingerprint.ClosingPhrase = extractChineseClosingPhrase(normalized)
	} else {
		fingerprint.OpeningPhrase = extractOpeningPhrase(words)
		fingerprint.ClosingPhrase = extractClosingPhrase(words)
	}

	// Find key theological terms
	fingerprint.KeyTerms = findKeyTerms(normalized)

	// Advanced matching features (Chinese-aware)
	if isChinese(language) {
		fingerprint.LongestWords = extractChineseLongestSequences(normalized)
		fingerprint.RareCharacters = extractChineseRareCharacters(text, language)
		fingerprint.RecurringPhrases = findChineseRecurringPhrases(normalized)
		fingerprint.UniqueSequences = extractChineseSignatureSequences(normalized)
	} else {
		fingerprint.LongestWords = extractLongestWords(words)
		fingerprint.RareCharacters = extractRareCharacters(text, language)
		fingerprint.RecurringPhrases = findRecurringPhrases(normalized)
		fingerprint.UniqueSequences = extractUniqueSequences(words)
	}

	// Detect structural markers (Chinese-aware)
	if isChinese(language) {
		fingerprint.HasInvocation = hasChineseInvocation(normalized)
		fingerprint.HasBlessings = hasChineseBlessings(normalized)
		fingerprint.HasSupplication = hasChineseSupplication(normalized)
	} else {
		fingerprint.HasInvocation = hasInvocation(normalized)
		fingerprint.HasBlessings = hasBlessings(normalized)
		fingerprint.HasSupplication = hasSupplication(normalized)
	}
	fingerprint.ParagraphCount = countParagraphs(text)
	fingerprint.VersePattern = detectVersePattern(text)

	// Generate structure hash
	fingerprint.StructureHash = generateStructureHash(text)

	// Determine prayer type (Chinese-aware)
	if isChinese(language) {
		fingerprint.PrayerType = determineChinesePrayerType(normalized)
	} else {
		fingerprint.PrayerType = determinePrayerType(normalized)
	}

	// Extract signature words (most distinctive terms)
	fingerprint.SignatureWords = extractSignatureWords(words, fingerprint.KeyTerms)

	// Add phonetic terms for cross-language matching
	fingerprint.PhoneticTerms = extractPhoneticTerms(fingerprint.KeyTerms, language)

	// Include full text for edge cases
	if fingerprint.IsShortPrayer || fingerprint.IsRareLanguage {
		fingerprint.FullText = text
	}

	return fingerprint
}

// isRareLanguage checks if a language has very few total prayers
func isRareLanguage(language string) bool {
	// This could be enhanced to query the actual database
	// For now, use heuristics based on known low-resource languages
	rareLangs := map[string]bool{
		"bla": true, "chr": true, "chn": true, "gil": true, "gwi": true,
		"hur": true, "ik": true, "kiw": true, "lkt": true, "meu": true,
		"mic": true, "moh": true, "nv": true, "oj": true, "tl": true,
		"wam": true, "ch": true, "fj": true, "mh": true, "bi": true,
	}
	return rareLangs[language]
}

// extractLongestWords finds the longest words which are often distinctive
func extractLongestWords(words []string) []string {
	type wordLen struct {
		word string
		len  int
	}

	var candidates []wordLen
	for _, word := range words {
		clean := strings.Trim(word, ".,!?;:\"'")
		if len(clean) > 4 { // Only consider substantial words
			candidates = append(candidates, wordLen{clean, len(clean)})
		}
	}

	// Sort by length descending
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].len > candidates[j].len
	})

	// Return top 5 longest unique words
	seen := make(map[string]bool)
	var result []string
	for _, wl := range candidates {
		if !seen[wl.word] && len(result) < 5 {
			result = append(result, wl.word)
			seen[wl.word] = true
		}
	}

	return result
}

// extractRareCharacters finds uncommon characters for CJK languages
func extractRareCharacters(text, language string) []string {
	if language != "zh-Hans" && language != "zh-Hant" && language != "ja" && language != "ko" {
		return nil // Only applicable for CJK languages
	}

	// Common characters that appear in many texts (less distinctive)
	commonChars := map[rune]bool{
		'的': true, '了': true, '在': true, '是': true, '我': true, '有': true, '和': true,
		'を': true, 'に': true, 'は': true, 'が': true, 'の': true, 'で': true, 'と': true,
		'이': true, '가': true, '을': true, '는': true, '에': true, '와': true, '의': true,
	}

	charCount := make(map[rune]int)
	for _, r := range text {
		if !commonChars[r] && (r >= 0x4E00 && r <= 0x9FFF) || // CJK Unified Ideographs
			(r >= 0x3040 && r <= 0x309F) || // Hiragana
			(r >= 0x30A0 && r <= 0x30FF) || // Katakana
			(r >= 0xAC00 && r <= 0xD7AF) { // Hangul
			charCount[r]++
		}
	}

	// Sort by frequency (rarer chars first)
	type charFreq struct {
		char rune
		freq int
	}
	var chars []charFreq
	for char, freq := range charCount {
		chars = append(chars, charFreq{char, freq})
	}
	sort.Slice(chars, func(i, j int) bool {
		return chars[i].freq < chars[j].freq
	})

	// Return up to 10 rarest characters
	var result []string
	for i, cf := range chars {
		if i >= 10 {
			break
		}
		result = append(result, string(cf.char))
	}

	return result
}

// findRecurringPhrases identifies repeated expressions with their frequency
func findRecurringPhrases(text string) []RecurringPhrase {
	words := strings.Fields(text)
	phrases := make(map[string][]int) // phrase -> positions

	// Look for 2-4 word phrases that repeat
	for phraseLen := 2; phraseLen <= 4; phraseLen++ {
		for i := 0; i <= len(words)-phraseLen; i++ {
			phrase := strings.Join(words[i:i+phraseLen], " ")
			if len(phrase) > 10 { // Only meaningful phrases
				pos := strings.Index(text, phrase)
				phrases[phrase] = append(phrases[phrase], pos)
			}
		}
	}

	var result []RecurringPhrase
	for phrase, positions := range phrases {
		if len(positions) >= 2 { // Must repeat at least twice
			result = append(result, RecurringPhrase{
				Phrase:    phrase,
				Count:     len(positions),
				Positions: positions,
			})
		}
	}

	// Sort by frequency (most frequent first)
	sort.Slice(result, func(i, j int) bool {
		return result[i].Count > result[j].Count
	})

	// Return top 5
	if len(result) > 5 {
		result = result[:5]
	}

	return result
}

// extractUniqueSequences finds distinctive 3-4 word combinations
func extractUniqueSequences(words []string) []string {
	sequences := make(map[string]bool)

	// Extract 3-word sequences
	for i := 0; i <= len(words)-3; i++ {
		seq := strings.Join(words[i:i+3], " ")
		if len(seq) > 15 && !isCommonSequence(seq) {
			sequences[seq] = true
		}
	}

	// Extract 4-word sequences
	for i := 0; i <= len(words)-4; i++ {
		seq := strings.Join(words[i:i+4], " ")
		if len(seq) > 20 && !isCommonSequence(seq) {
			sequences[seq] = true
		}
	}

	var result []string
	for seq := range sequences {
		result = append(result, seq)
	}

	sort.Strings(result)

	// Return top 10
	if len(result) > 10 {
		result = result[:10]
	}

	return result
}

// isCommonSequence checks if a sequence is too common to be distinctive
func isCommonSequence(seq string) bool {
	commonSeqs := []string{
		"in the name", "of the lord", "praise be to", "glory be to",
		"o my god", "blessed is he", "there is no", "god but god",
	}

	seqLower := strings.ToLower(seq)
	for _, common := range commonSeqs {
		if strings.Contains(seqLower, common) {
			return true
		}
	}

	return false
}

// countParagraphs counts text blocks/paragraphs
func countParagraphs(text string) int {
	return len(strings.Split(strings.TrimSpace(text), "\n\n"))
}

// detectVersePattern identifies prayer structure
func detectVersePattern(text string) string {
	lines := strings.Split(text, "\n")
	nonEmptyLines := 0
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			nonEmptyLines++
		}
	}

	if nonEmptyLines <= 2 {
		return "single"
	} else if nonEmptyLines <= 6 {
		return "multiple"
	} else {
		return "responsive"
	}
}

// generateStructureHash creates a hash based on prayer structure
func generateStructureHash(text string) string {
	// Create a structural fingerprint based on text organization
	lines := strings.Split(text, "\n")
	structure := fmt.Sprintf("%d_%d_%d",
		len(lines),                       // Line count
		len(strings.Split(text, "\n\n")), // Paragraph count
		len(strings.Fields(text)),        // Word count
	)

	hash := md5.Sum([]byte(structure))
	return hex.EncodeToString(hash[:])[:8]
}

// extractPhoneticTerms creates transliterated versions for cross-language matching
func extractPhoneticTerms(keyTerms []string, language string) []string {
	// This is a simplified version - could be enhanced with proper transliteration
	var phonetic []string

	for _, term := range keyTerms {
		// Basic transliteration mappings for common theological terms
		switch strings.ToLower(term) {
		case "allah", "alláh":
			phonetic = append(phonetic, "god", "dieu", "dios", "gott")
		case "bahá", "baha":
			phonetic = append(phonetic, "baha", "glory", "splendor")
		case "abhá", "abha":
			phonetic = append(phonetic, "abha", "most-glorious")
		default:
			// Keep original for now
			phonetic = append(phonetic, term)
		}
	}

	return phonetic
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

// normalizeChineseVariants converts Traditional Chinese to Simplified for consistent matching
func normalizeChineseVariants(text string) string {
	if len(text) == 0 {
		return text
	}

	runes := []rune(text)
	for i, r := range runes {
		if simplified, exists := traditionalToSimplified[r]; exists {
			runes[i] = simplified
		}
	}
	return string(runes)
}

// normalizeText creates MD5 hash of normalized text for exact matching
func textHash(text string) string {
	// For Chinese, normalize Traditional/Simplified variants first
	normalized := text
	if strings.ContainsAny(text, "經學國門來時無長會現開關東車見說語電話網間問題業個們對應還這樣點") {
		normalized = normalizeChineseVariants(text)
	}

	// Remove all punctuation and spacing for hash
	clean := regexp.MustCompile(`[^\p{L}\p{N}]`).ReplaceAllString(normalized, "")
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

	prompt.WriteString("You are an expert in Bahá'í prayer matching using semantic fingerprints for cross-language prayer identification.\n\n")

	prompt.WriteString("# TASK\n")
	prompt.WriteString(fmt.Sprintf("Match %s prayer fingerprints to English reference fingerprints.\n", targetLang))
	prompt.WriteString("Each fingerprint contains compressed semantic information instead of full text.\n")
	prompt.WriteString("⚠️  CROSS-LANGUAGE LIMITATION: Semantic comparison (key_terms, opening/closing phrases) may be unreliable across different scripts/languages.\n\n")

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

	prompt.WriteString("# MATCHING STRATEGY (Priority Order)\n")
	prompt.WriteString("1. **PRIMARY: text_hash matches** - Most reliable across all languages (confidence: 100%)\n")
	prompt.WriteString("2. **SECONDARY: advanced features** - longest_words, rare_characters, recurring_phrases, unique_sequences\n")
	prompt.WriteString("3. **STRUCTURAL patterns** - word_count (±10%), paragraph_count, structure_hash, verse_pattern\n")
	prompt.WriteString("4. **SEMANTIC similarity** - Only for same-script languages (key_terms, opening/closing phrases)\n")
	prompt.WriteString("5. **FALLBACK: NEW_TRANSLATION** - When no reliable match exists\n\n")
	prompt.WriteString("⚠️  **Cross-Language/Cross-Script Guidance:**\n")
	prompt.WriteString("- **Chinese (zh-Hans/zh-Hant)**: Character-based analysis - prioritize rare_characters (theological chars: 神,主,天,圣,灵,祈,祷,恩,慈,巴,哈), longest_words (character sequences), recurring_phrases (repeated character patterns)\n")
	prompt.WriteString("- **Chinese Traditional/Simplified**: Text normalized to Simplified for cross-script matching - same prayer in both scripts should have similar text_hash\n")
	prompt.WriteString("- **CJK languages (Japanese/Korean)**: Use rare_characters, longest_words, structure_hash\n")
	prompt.WriteString("- **Arabic script**: Use longest_words, phonetic_terms, recurring_phrases\n")
	prompt.WriteString("- **Short prayers (is_short_prayer=true)**: Use full_text if available\n")
	prompt.WriteString("- **Rare languages (is_rare_language=true)**: Use full_text and all available features\n")
	prompt.WriteString("- **Chinese word_count**: Estimated from character count (~1.7 chars per word)\n")
	prompt.WriteString("- **Chinese opening/closing**: Character-based phrases, not word-based\n")
	prompt.WriteString("- **Same rare_characters or longest_words = strong indicator of same prayer**\n")
	prompt.WriteString("- **Similar recurring_phrases = likely same prayer with different translation**\n\n")

	// Add Chinese-specific examples if target language is Chinese
	if isChinese(targetLang) {
		prompt.WriteString("# CHINESE MATCHING EXAMPLES\n")
		prompt.WriteString("**Example 1: Strong Match**\n")
		prompt.WriteString("- English rare_chars: ['G', 'l', 'o', 'r', 'y'] vs Chinese rare_chars: ['荣', '耀', '神', '圣', '主']\n")
		prompt.WriteString("- English longest_words: ['glorified', 'almighty', 'protection'] vs Chinese longest_words: ['全能的主', '荣耀归于', '保护我们']\n")
		prompt.WriteString("- Match confidence: HIGH (90%+) - theological terms align\n\n")
		prompt.WriteString("**Example 2: Character-based Analysis**\n")
		prompt.WriteString("- Chinese opening: '仁慈的主啊！祢的仆人' (Merciful Lord! Your servants)\n")
		prompt.WriteString("- Chinese closing: '祢是慷慨者，博爱者。' (You are Generous, All-Loving)\n")
		prompt.WriteString("- Recurring phrases: ['祢是', '我们祈求', '上帝啊'] indicate prayer structure\n")
		prompt.WriteString("- Match on structural + theological character patterns, not word-for-word\n\n")
		prompt.WriteString("**Example 3: Traditional/Simplified Cross-Script**\n")
		prompt.WriteString("- Traditional: '仁慈的主啊！祢的僕人在此團聚' vs Simplified: '仁慈的主啊！祢的仆人在此团聚'\n")
		prompt.WriteString("- After normalization, both have similar text_hash and rare_characters\n")
		prompt.WriteString("- Match confidence: VERY HIGH (95%+) - same prayer, different script\n\n")
	}

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

	prompt.WriteString("# STRICT RULES\n")
	prompt.WriteString("1. **text_hash match = EXACT (confidence: 100)** - Always prioritize this\n")
	prompt.WriteString("2. **Advanced feature matches:**\n")
	prompt.WriteString("   - Same longest_words (2+ matches) = LIKELY (confidence: 85-95)\n")
	prompt.WriteString("   - Same rare_characters (3+ matches) = LIKELY (confidence: 85-95)\n")
	prompt.WriteString("   - Same recurring_phrases (1+ matches) = LIKELY (confidence: 80-90)\n")
	prompt.WriteString("3. **Structural similarity + word_count (±10%) = LIKELY (confidence: 75-85)**\n")
	prompt.WriteString("4. **Multiple candidates with similar features = AMBIGUOUS (confidence: 60-75)**\n")
	prompt.WriteString("5. **Cross-script languages: Prioritize longest_words, rare_characters over semantic features**\n")
	prompt.WriteString("6. **Short prayers: Use full_text for direct comparison if available**\n")
	prompt.WriteString("7. **Rare languages: Weight all available features more heavily**\n")
	prompt.WriteString("8. **Always include specific match_reasons** (e.g., 'longest_words_match', 'rare_characters_match')\n")
	prompt.WriteString("9. **Never explain your process - just return the JSON**\n")
	prompt.WriteString("10. **Never generate code - only JSON results**\n\n")

	prompt.WriteString("🚨 **CRITICAL: Return ONLY the JSON response. No explanations, no code, no additional text.**\n\n")

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

// CompressedLanguageMatchingWithTMPFallback performs matching with en -> ar -> fa -> TMP fallback
func CompressedLanguageMatchingWithTMPFallback(targetLang string) error {
	log.Printf("Starting compressed matching with TMP fallback for language: %s", targetLang)

	// Load database
	db, err := GetDatabase()
	if err != nil {
		return fmt.Errorf("failed to load database: %w", err)
	}

	// Get three-tier references (English, Arabic, Persian)
	englishRefs, arabicRefs, persianRefs := BuildReferencesWithTMP(db)

	// Get target language prayers
	targetPrayers := BuildTargetPrayers(db, targetLang)

	log.Printf("Loaded %d English, %d Arabic, %d Persian refs, %d %s prayers",
		len(englishRefs), len(arabicRefs), len(persianRefs), len(targetPrayers), targetLang)

	// Create fingerprints for all three tiers
	var englishFingerprints []PrayerFingerprint
	for _, ref := range englishRefs {
		fp := CreatePrayerFingerprint(ref.Phelps, "", "en", ref.Name, ref.Text)
		englishFingerprints = append(englishFingerprints, fp)
	}

	var arabicFingerprints []PrayerFingerprint
	for _, ref := range arabicRefs {
		fp := CreatePrayerFingerprint(ref.Phelps, "", "ar", ref.Name, ref.Text)
		arabicFingerprints = append(arabicFingerprints, fp)
	}

	var persianFingerprints []PrayerFingerprint
	for _, ref := range persianRefs {
		fp := CreatePrayerFingerprint(ref.Phelps, "", "fa", ref.Name, ref.Text)
		persianFingerprints = append(persianFingerprints, fp)
	}

	var targetFingerprints []PrayerFingerprint
	for _, prayer := range targetPrayers {
		fp := CreatePrayerFingerprint("", prayer.Version, targetLang, prayer.Name, prayer.Text)
		targetFingerprints = append(targetFingerprints, fp)
	}

	log.Printf("Created fingerprints: %d en + %d ar + %d fa + %d target",
		len(englishFingerprints), len(arabicFingerprints), len(persianFingerprints), len(targetFingerprints))

	// Create three-tier fallback prompt
	prompt := CreateFallbackMatchingPrompt(
		englishFingerprints,
		arabicFingerprints,
		persianFingerprints,
		targetFingerprints,
		targetLang,
	)

	log.Printf("Calling LLM for three-tier fallback matching...")
	response, err := callLLMWithBackendFallback(prompt, "TMP fallback matching", true)
	if err != nil {
		return fmt.Errorf("LLM call failed for TMP fallback matching: %w", err)
	}

	// Parse response using robust JSON extraction
	jsonStr, err := ExtractJSONFromResponse(response)
	if err != nil {
		failedResponseFile := fmt.Sprintf("failed_response_%s_%d.txt", targetLang, time.Now().Unix())
		if writeErr := os.WriteFile(failedResponseFile, []byte(response), 0644); writeErr == nil {
			log.Printf("❌ JSON extraction failed, saved response to: %s", failedResponseFile)
		}
		return fmt.Errorf("failed to extract JSON from response (saved to %s): %w", failedResponseFile, err)
	}

	var results CompressedBatchResponse
	if err := json.Unmarshal([]byte(jsonStr), &results); err != nil {
		failedResponseFile := fmt.Sprintf("failed_response_%s_%d.txt", targetLang, time.Now().Unix())
		if writeErr := os.WriteFile(failedResponseFile, []byte(response), 0644); writeErr == nil {
			log.Printf("❌ Parse failed, saved response to: %s", failedResponseFile)
		}
		return fmt.Errorf("failed to parse response (saved to %s): %w", failedResponseFile, err)
	}

	// Process results using TMP-aware processing
	if err := ApplyTMPMatches(targetLang, results); err != nil {
		return fmt.Errorf("failed to apply TMP matches: %w", err)
	}

	log.Printf("✅ TMP fallback matching completed for %s", targetLang)
	return nil
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

	log.Printf("Calling LLM for compressed bulk matching...")
	response, err := callLLMWithBackendFallback(prompt, "compressed bulk matching", true)
	if err != nil {
		return fmt.Errorf("LLM call failed for compressed bulk matching: %w", err)
	}

	// Parse response using robust JSON extraction
	jsonStr, err := ExtractJSONFromResponse(response)
	if err != nil {
		// Save failed response for later analysis
		failedResponseFile := fmt.Sprintf("failed_response_%s_%d.txt",
			targetLang,
			time.Now().Unix())
		if writeErr := os.WriteFile(failedResponseFile, []byte(response), 0644); writeErr == nil {
			log.Printf("❌ JSON extraction failed, saved response to: %s", failedResponseFile)
		}
		return fmt.Errorf("failed to extract JSON from response (saved to %s): %w", failedResponseFile, err)
	}

	var results CompressedBatchResponse
	if err := json.Unmarshal([]byte(jsonStr), &results); err != nil {
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

// isChinese checks if the language is Chinese (Traditional or Simplified)
func isChinese(language string) bool {
	return language == "zh-Hans" || language == "zh-Hant"
}

// getChineseWordCount estimates word count for Chinese text
func getChineseWordCount(text, language string) int {
	if !isChinese(language) {
		return len(strings.Fields(normalizeText(text)))
	}

	// Count meaningful Chinese characters (excluding punctuation)
	count := 0
	for _, r := range text {
		if unicode.Is(unicode.Han, r) {
			count++
		}
	}

	// Approximate word count: ~1.7 characters per semantic unit
	return int(float64(count) / 1.7)
}

// extractChineseOpeningPhrase extracts meaningful opening for Chinese text
func extractChineseOpeningPhrase(text string) string {
	runes := []rune(text)
	if len(runes) == 0 {
		return ""
	}

	// Extract first 15-20 characters as "opening phrase"
	maxChars := 20
	if len(runes) < maxChars {
		maxChars = len(runes)
	}

	// Look for natural stopping points (punctuation)
	for i := 8; i < maxChars; i++ {
		if runes[i] == '。' || runes[i] == '！' || runes[i] == '，' {
			return string(runes[:i+1])
		}
	}

	return string(runes[:maxChars])
}

// extractChineseClosingPhrase extracts meaningful closing for Chinese text
func extractChineseClosingPhrase(text string) string {
	runes := []rune(text)
	if len(runes) == 0 {
		return ""
	}

	// Extract last 15 characters as "closing phrase"
	startIdx := len(runes) - 15
	if startIdx < 0 {
		startIdx = 0
	}

	return string(runes[startIdx:])
}

// extractChineseRareCharacters finds distinctive Chinese characters
func extractChineseRareCharacters(text, language string) []string {
	if !isChinese(language) {
		return extractRareCharacters(text, language)
	}

	// Count all Han characters
	charCount := make(map[rune]int)
	for _, r := range text {
		if unicode.Is(unicode.Han, r) {
			charCount[r]++
		}
	}

	// Score characters by rarity and theological significance
	type charScore struct {
		char  rune
		score float64
	}

	var scored []charScore
	for char, count := range charCount {
		score := 1.0 / float64(count) // Rarity score
		if chineseTheologicalChars[char] {
			score *= 3.0 // Boost theological significance
		}
		scored = append(scored, charScore{char, score})
	}

	// Sort by score (highest first)
	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})

	// Return top 15 distinctive characters
	var result []string
	for i, cs := range scored {
		if i >= 15 {
			break
		}
		result = append(result, string(cs.char))
	}

	return result
}

// extractChineseLongestSequences finds distinctive character sequences
func extractChineseLongestSequences(text string) []string {
	runes := []rune(text)
	sequences := make(map[string]int)

	// Extract 3-6 character sequences that don't contain punctuation
	for length := 3; length <= 6; length++ {
		for i := 0; i <= len(runes)-length; i++ {
			seq := string(runes[i : i+length])

			// Skip sequences with punctuation
			if regexp.MustCompile(`[\p{P}\p{S}]`).MatchString(seq) {
				continue
			}

			// Only consider sequences with Han characters
			hasHan := false
			for _, r := range seq {
				if unicode.Is(unicode.Han, r) {
					hasHan = true
					break
				}
			}

			if hasHan {
				sequences[seq] = length // Score by length
			}
		}
	}

	// Sort by length (longest first)
	type seqScore struct {
		seq   string
		score int
	}

	var scored []seqScore
	for seq, score := range sequences {
		scored = append(scored, seqScore{seq, score})
	}

	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})

	// Return top 5 longest sequences
	var result []string
	for i, ss := range scored {
		if i >= 5 {
			break
		}
		result = append(result, ss.seq)
	}

	return result
}

// extractChineseSignatureSequences finds recurring Chinese patterns
func extractChineseSignatureSequences(text string) []string {
	runes := []rune(text)
	sequences := make(map[string]int)

	// Extract 2-4 character sequences
	for length := 2; length <= 4; length++ {
		for i := 0; i <= len(runes)-length; i++ {
			seq := string(runes[i : i+length])

			// Skip if contains punctuation
			if regexp.MustCompile(`[\p{P}\p{S}]`).MatchString(seq) {
				continue
			}

			sequences[seq]++
		}
	}

	// Return sequences that appear multiple times
	var result []string
	for seq, count := range sequences {
		if count >= 2 && len([]rune(seq)) >= 3 {
			result = append(result, seq)
		}
	}

	return result
}

// findChineseRecurringPhrases identifies repeated Chinese expressions
func findChineseRecurringPhrases(text string) []RecurringPhrase {
	runes := []rune(text)
	phrases := make(map[string][]int)

	// Look for 3-8 character phrases that repeat
	for phraseLen := 3; phraseLen <= 8; phraseLen++ {
		for i := 0; i <= len(runes)-phraseLen; i++ {
			phrase := string(runes[i : i+phraseLen])

			// Skip phrases with punctuation
			if regexp.MustCompile(`[\p{P}\p{S}]`).MatchString(phrase) {
				continue
			}

			phrases[phrase] = append(phrases[phrase], i)
		}
	}

	// Filter for phrases that appear at least twice
	var result []RecurringPhrase
	for phrase, positions := range phrases {
		if len(positions) >= 2 {
			result = append(result, RecurringPhrase{
				Phrase:    phrase,
				Count:     len(positions),
				Positions: positions,
			})
		}
	}

	// Sort by frequency
	sort.Slice(result, func(i, j int) bool {
		return result[i].Count > result[j].Count
	})

	// Return top 5 most frequent
	if len(result) > 5 {
		result = result[:5]
	}

	return result
}

// determineChinesePrayerType classifies Chinese prayer types
func determineChinesePrayerType(text string) string {
	if strings.Contains(text, "祈祷") || strings.Contains(text, "祈求") {
		return "supplication"
	}
	if strings.Contains(text, "赞美") || strings.Contains(text, "颂扬") {
		return "praise"
	}
	if strings.Contains(text, "保护") || strings.Contains(text, "庇护") {
		return "protection"
	}
	if strings.Contains(text, "治疗") || strings.Contains(text, "医治") {
		return "healing"
	}
	if strings.Contains(text, "儿童") || strings.Contains(text, "孩子") {
		return "children"
	}

	return "general"
}

// hasChineseInvocation detects Chinese prayer invocations
func hasChineseInvocation(text string) bool {
	invocationPatterns := []string{
		"仁慈的主", "上帝啊", "我的主", "全能的主", "慈悲的主",
		"主啊", "神啊", "天父", "至高的神",
	}

	for _, pattern := range invocationPatterns {
		if strings.Contains(text, pattern) {
			return true
		}
	}

	return false
}

// hasChineseBlessings detects Chinese blessing patterns
func hasChineseBlessings(text string) bool {
	blessingPatterns := []string{
		"赐福", "福佑", "恩赐", "赐予", "降福",
		"祝福", "保佑", "恩典", "慈爱",
	}

	for _, pattern := range blessingPatterns {
		if strings.Contains(text, pattern) {
			return true
		}
	}

	return false
}

// hasChineseSupplication detects Chinese supplication patterns
func hasChineseSupplication(text string) bool {
	supplicationPatterns := []string{
		"恳求", "祈求", "恳请", "求", "请求",
		"乞求", "祷告", "祈祷", "恳望",
	}

	for _, pattern := range supplicationPatterns {
		if strings.Contains(text, pattern) {
			return true
		}
	}

	return false
}
