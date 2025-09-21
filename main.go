package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var OllamaModel string = "gpt-oss"
var stopRequested int32                     // Atomic flag for graceful stop
var interruptCount int32                    // Count of interrupt signals received
var geminiQuotaExceeded int32               // Atomic flag for quota exceeded
var ollamaAPIURL = "http://localhost:11434" // Ollama API endpoint

// Raw response storage for large responses
var storedRawResponses []string
var storedRawResponsesMutex sync.Mutex

// Helper function to truncate large responses and store them
func truncateAndStore(response string, source string) string {
	const maxDisplayLength = 500
	if len(response) > maxDisplayLength {
		storedRawResponsesMutex.Lock()
		storedRawResponses = append(storedRawResponses, fmt.Sprintf("=== %s Raw Response ===\n%s\n", source, response))
		storedRawResponsesMutex.Unlock()

		return fmt.Sprintf("%s... [TRUNCATED - %d more characters. Use -show-raw to see full responses at end]",
			response[:maxDisplayLength], len(response)-maxDisplayLength)
	}
	return response
}

// Function to display all stored raw responses
func showStoredRawResponses() {
	storedRawResponsesMutex.Lock()
	defer storedRawResponsesMutex.Unlock()

	if len(storedRawResponses) == 0 {
		return
	}

	fmt.Printf("\n%s\n", strings.Repeat("=", 80))
	fmt.Printf("FULL RAW RESPONSES (truncated during execution)\n")
	fmt.Printf("%s\n\n", strings.Repeat("=", 80))

	for i, response := range storedRawResponses {
		fmt.Printf("Response %d:\n%s\n", i+1, response)
		if i < len(storedRawResponses)-1 {
			fmt.Printf("%s\n", strings.Repeat("-", 40))
		}
	}

	fmt.Printf("%s\n", strings.Repeat("=", 80))
}

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

type OllamaToolCall struct {
	Function struct {
		Name      string `json:"name"`
		Arguments struct {
			Arguments string `json:"arguments"`
			Function  string `json:"function"`
			Args      string `json:"args"`
			Call      string `json:"call"`
		} `json:"arguments"`
	} `json:"function"`
}

type OllamaMessageWithTools struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []OllamaToolCall `json:"tool_calls"`
}

type OllamaChatResponseWithTools struct {
	Message OllamaMessageWithTools `json:"message"`
	Done    bool                   `json:"done"`
}

type OllamaModelInfo struct {
	Name       string    `json:"name"`
	ModifiedAt time.Time `json:"modified_at"`
	Size       int64     `json:"size"`
}

type OllamaTagsResponse struct {
	Models []OllamaModelInfo `json:"models"`
}

// Helper function to call Ollama API with dynamic timeout
func CallOllama(prompt string, textLength int) (string, error) {
	return CallOllamaWithMessages([]OllamaMessage{
		{Role: "user", Content: prompt},
	}, textLength)
}

// Call Ollama API with conversation messages
func CallOllamaWithMessages(messages []OllamaMessage, textLength int) (string, error) {
	// Calculate dynamic timeout based on text length
	baseTimeout := 30 * time.Minute
	extraTime := time.Duration(textLength/1000) * 1 * time.Minute
	timeout := baseTimeout + extraTime

	if timeout < 30*time.Minute {
		timeout = 30 * time.Minute
	}
	if timeout > 90*time.Minute {
		timeout = 90 * time.Minute
	}

	log.Printf("Starting Ollama API with model %s (timeout: %v for %d chars)...", OllamaModel, timeout.Round(time.Second), textLength)

	// Prepare the request
	request := OllamaChatRequest{
		Model:    OllamaModel,
		Messages: messages,
		Stream:   false,
	}

	requestBody, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	// Create HTTP request
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", ollamaAPIURL+"/api/chat", bytes.NewBuffer(requestBody))
	if err != nil {
		return "", fmt.Errorf("failed to create HTTP request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")

	// Make the request
	client := &http.Client{}
	startTime := time.Now()

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

	// Read and parse response
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	// Try to parse as response with tool calls first
	var chatResponseWithTools OllamaChatResponseWithTools
	if err := json.Unmarshal(responseBody, &chatResponseWithTools); err == nil && len(chatResponseWithTools.Message.ToolCalls) > 0 {
		// Handle tool calls format
		var toolCallStrings []string
		for _, toolCall := range chatResponseWithTools.Message.ToolCalls {
			// Try to get arguments from different field names
			args := toolCall.Function.Arguments.Arguments
			if args == "" {
				args = toolCall.Function.Arguments.Args
			}

			switch toolCall.Function.Name {
			case "SEARCH_KEYWORDS":
				if args != "" {
					toolCallStrings = append(toolCallStrings, fmt.Sprintf("SEARCH_KEYWORDS:%s", args))
				}
			case "SEARCH_LENGTH":
				if args != "" {
					toolCallStrings = append(toolCallStrings, fmt.Sprintf("SEARCH_LENGTH:%s", args))
				}
			case "SEARCH_OPENING":
				if args != "" {
					toolCallStrings = append(toolCallStrings, fmt.Sprintf("SEARCH_OPENING:%s", args))
				}
			case "GET_FULL_TEXT":
				if args != "" {
					toolCallStrings = append(toolCallStrings, fmt.Sprintf("GET_FULL_TEXT:%s", args))
				}
			case "GET_PARTIAL_TEXT":
				if args != "" {
					toolCallStrings = append(toolCallStrings, fmt.Sprintf("GET_PARTIAL_TEXT:%s", args))
				}
			case "FINAL_ANSWER":
				if args != "" {
					toolCallStrings = append(toolCallStrings, fmt.Sprintf("FINAL_ANSWER:%s", args))
				}
			case "GET_STATS":
				toolCallStrings = append(toolCallStrings, "GET_STATS")
			}
		}

		// Combine content and tool calls
		content := strings.TrimSpace(chatResponseWithTools.Message.Content)
		if len(toolCallStrings) > 0 {
			if content != "" {
				content += "\n" + strings.Join(toolCallStrings, "\n")
			} else {
				content = strings.Join(toolCallStrings, "\n")
			}
		}

		elapsed := time.Since(startTime)
		log.Printf("Ollama API completed successfully in %v", elapsed.Round(time.Second))
		return content, nil
	}

	// Fall back to standard response format
	var chatResponse OllamaChatResponse
	if err := json.Unmarshal(responseBody, &chatResponse); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	// Filter out thinking sections and extract the actual response
	content := filterThinkingFromResponse(chatResponse.Message.Content)

	elapsed := time.Since(startTime)
	log.Printf("Ollama API completed successfully in %v", elapsed.Round(time.Second))

	return content, nil
}

// Helper function to filter out thinking sections from model responses
func filterThinkingFromResponse(content string) string {
	lines := strings.Split(content, "\n")
	var filteredLines []string
	skipMode := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Skip thinking sections
		if strings.HasPrefix(trimmed, "Thinking...") {
			skipMode = true
			continue
		}
		if strings.HasPrefix(trimmed, "...done thinking") {
			skipMode = false
			continue
		}

		// If we're in skip mode, continue skipping
		if skipMode {
			continue
		}

		// Keep non-thinking lines
		if trimmed != "" {
			filteredLines = append(filteredLines, line)
		}
	}

	return strings.Join(filteredLines, "\n")
}

// Helper function to shell out to Gemini CLI
func CallGemini(prompt string) (string, error) {
	// Check if quota exceeded flag is already set
	if atomic.LoadInt32(&geminiQuotaExceeded) == 1 {
		return "", fmt.Errorf("gemini quota previously exceeded, skipping")
	}

	cmd := exec.Command("gemini", prompt)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Check if this is a quota exceeded error
		errorStr := strings.ToLower(err.Error() + string(output))
		if strings.Contains(errorStr, "quota") && (strings.Contains(errorStr, "exceeded") || strings.Contains(errorStr, "exhausted")) ||
			strings.Contains(errorStr, "429") ||
			strings.Contains(errorStr, "resource_exhausted") {
			atomic.StoreInt32(&geminiQuotaExceeded, 1)
			log.Printf("Gemini quota exceeded - disabling Gemini for remainder of session")
			return "", fmt.Errorf("gemini quota exceeded: %w", err)
		}
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
	CallOllama(prompt string, textLength int) (string, error)
}

// DefaultLLMCaller implements LLMCaller using the actual CLI tools
type DefaultLLMCaller struct{}

func (d DefaultLLMCaller) CallGemini(prompt string) (string, error) {
	return CallGemini(prompt)
}

func (d DefaultLLMCaller) CallOllama(prompt string, textLength int) (string, error) {
	return CallOllama(prompt, textLength)
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

// Extract distinctive words from text (most unique/uncommon words)
func extractDistinctiveWords(text string, n int) []string {
	// Common words to exclude (stop words)
	stopWords := map[string]bool{
		"the": true, "a": true, "an": true, "and": true, "or": true, "but": true,
		"in": true, "on": true, "at": true, "to": true, "for": true, "of": true,
		"with": true, "by": true, "from": true, "up": true, "about": true, "into": true,
		"through": true, "during": true, "before": true, "after": true, "above": true,
		"below": true, "between": true, "among": true, "is": true, "are": true, "was": true,
		"were": true, "be": true, "been": true, "being": true, "have": true, "has": true,
		"had": true, "do": true, "does": true, "did": true, "will": true, "would": true,
		"could": true, "should": true, "may": true, "might": true, "must": true, "can": true,
		"this": true, "that": true, "these": true, "those": true, "i": true, "me": true,
		"my": true, "myself": true, "we": true, "our": true, "ours": true, "ourselves": true,
		"you": true, "your": true, "yours": true, "yourself": true, "yourselves": true,
		"he": true, "him": true, "his": true, "himself": true, "she": true, "her": true,
		"hers": true, "herself": true, "it": true, "its": true, "itself": true, "they": true,
		"them": true, "their": true, "theirs": true, "themselves": true, "what": true,
		"which": true, "who": true, "whom": true, "whose": true, "where": true, "when": true,
		"why": true, "how": true, "all": true, "any": true, "both": true, "each": true,
		"few": true, "more": true, "most": true, "other": true, "some": true, "such": true,
		"no": true, "nor": true, "not": true, "only": true, "own": true, "same": true,
		"so": true, "than": true, "too": true, "very": true, "just": true, "now": true,
		"o": true, "oh": true, "thy": true, "thee": true, "thou": true, "thine": true,
	}

	// Clean and split text into words
	text = strings.ToLower(text)
	// Remove punctuation and split
	words := strings.FieldsFunc(text, func(c rune) bool {
		return !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9'))
	})

	// Count word frequencies
	wordCount := make(map[string]int)
	for _, word := range words {
		if len(word) > 2 && !stopWords[word] {
			wordCount[word]++
		}
	}

	// Sort words by frequency (less frequent = more distinctive)
	type wordFreq struct {
		word string
		freq int
	}
	var sortedWords []wordFreq
	for word, freq := range wordCount {
		sortedWords = append(sortedWords, wordFreq{word, freq})
	}
	sort.Slice(sortedWords, func(i, j int) bool {
		if sortedWords[i].freq == sortedWords[j].freq {
			return sortedWords[i].word < sortedWords[j].word // Alphabetical for ties
		}
		return sortedWords[i].freq < sortedWords[j].freq // Less frequent first
	})

	// Return top n distinctive words
	result := make([]string, 0, n)
	for i := 0; i < len(sortedWords) && i < n; i++ {
		result = append(result, sortedWords[i].word)
	}
	return result
}

// Get first meaningful line from text (skip empty lines)
func getFirstLine(text string) string {
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" && len(line) > 10 {
			if len(line) > 100 {
				return line[:100] + "..."
			}
			return line
		}
	}
	if len(text) > 100 {
		return text[:100] + "..."
	}
	return text
}

// Build rich context for each Phelps code
func buildPhelpsContext(db Database, referenceLanguage string) map[string]string {
	phelpsContext := make(map[string]string)

	// Group writings by Phelps code
	phelpsWritings := make(map[string][]Writing)
	for _, writing := range db.Writing {
		if writing.Phelps != "" && writing.Language == referenceLanguage && writing.Text != "" {
			phelpsWritings[writing.Phelps] = append(phelpsWritings[writing.Phelps], writing)
		}
	}

	// If no reference language data, use any language
	if len(phelpsWritings) == 0 {
		for _, writing := range db.Writing {
			if writing.Phelps != "" && writing.Text != "" {
				phelpsWritings[writing.Phelps] = append(phelpsWritings[writing.Phelps], writing)
			}
		}
	}

	// Build context for each Phelps code
	for phelps, writings := range phelpsWritings {
		if len(writings) == 0 {
			continue
		}

		// Use the longest/most complete writing for context
		bestWriting := writings[0]
		for _, writing := range writings {
			if len(writing.Text) > len(bestWriting.Text) {
				bestWriting = writing
			}
		}

		name := bestWriting.Name
		if name == "" {
			name = "Untitled"
		}

		firstLine := getFirstLine(bestWriting.Text)
		distinctiveWords := extractDistinctiveWords(bestWriting.Text, 5)
		wordCount := len(strings.Fields(bestWriting.Text))
		charCount := len(bestWriting.Text)

		context := fmt.Sprintf("%s (%s) [%d words, %d chars] - Opening: \"%s\" - Key words: %s",
			phelps, name, wordCount, charCount, firstLine, strings.Join(distinctiveWords, ", "))

		phelpsContext[phelps] = context
	}

	return phelpsContext
}

// Search prayers by keywords
func searchPrayersByKeywords(db Database, referenceLanguage string, keywords []string, limit int) []string {
	phelpsContext := buildPhelpsContext(db, referenceLanguage)

	type phelpsScore struct {
		phelps  string
		context string
		score   int
	}

	var results []phelpsScore

	for phelps, context := range phelpsContext {
		score := 0
		contextLower := strings.ToLower(context)

		for _, keyword := range keywords {
			keywordLower := strings.ToLower(strings.TrimSpace(keyword))
			if strings.Contains(contextLower, keywordLower) {
				score++
			}
		}

		if score > 0 {
			results = append(results, phelpsScore{phelps, context, score})
		}
	}

	// Sort by score (highest first)
	sort.Slice(results, func(i, j int) bool {
		return results[i].score > results[j].score
	})

	// Return top results up to limit
	var output []string
	for i, result := range results {
		if limit > 0 && i >= limit {
			break
		}
		output = append(output, fmt.Sprintf("MATCH_%d: %s", result.score, result.context))
	}

	if len(output) == 0 {
		return []string{"No prayers found matching those keywords."}
	}

	return output
}

// Search prayers by length range
func searchPrayersByLength(db Database, referenceLanguage string, minWords, maxWords int, limit int) []string {
	phelpsContext := buildPhelpsContext(db, referenceLanguage)

	var results []string
	count := 0

	for _, context := range phelpsContext {
		// Extract word count from context string like "[60 words, 347 chars]"
		if strings.Contains(context, "words") {
			parts := strings.Split(context, "[")
			if len(parts) > 1 {
				wordPart := strings.Split(parts[1], " words")[0]
				if wordCount, err := strconv.Atoi(wordPart); err == nil {
					if wordCount >= minWords && wordCount <= maxWords {
						results = append(results, context)
						count++
						if limit > 0 && count >= limit {
							break
						}
					}
				}
			}
		}
	}

	if len(results) == 0 {
		return []string{fmt.Sprintf("No prayers found with %d-%d words.", minWords, maxWords)}
	}

	return results
}

// Search prayers by opening text similarity
func searchPrayersByOpening(db Database, referenceLanguage string, openingText string, limit int) []string {
	phelpsContext := buildPhelpsContext(db, referenceLanguage)
	openingLower := strings.ToLower(openingText)

	type phelpsMatch struct {
		context    string
		similarity int
	}

	var matches []phelpsMatch

	for _, context := range phelpsContext {
		// Extract opening text from context
		if strings.Contains(context, "Opening: \"") {
			parts := strings.Split(context, "Opening: \"")
			if len(parts) > 1 {
				opening := strings.Split(parts[1], "\"")[0]
				openingCtxLower := strings.ToLower(opening)

				// Simple similarity score based on common words
				openingWords := strings.Fields(openingLower)
				ctxWords := strings.Fields(openingCtxLower)

				commonWords := 0
				for _, word := range openingWords {
					for _, ctxWord := range ctxWords {
						if word == ctxWord && len(word) > 2 {
							commonWords++
							break
						}
					}
				}

				if commonWords > 0 {
					matches = append(matches, phelpsMatch{context, commonWords})
				}
			}
		}
	}

	// Sort by similarity (highest first)
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].similarity > matches[j].similarity
	})

	var results []string
	for i, match := range matches {
		if limit > 0 && i >= limit {
			break
		}
		results = append(results, fmt.Sprintf("SIMILARITY_%d: %s", match.similarity, match.context))
	}

	if len(results) == 0 {
		return []string{"No prayers found with similar opening text."}
	}

	return results
}

// Process LLM function calls
func processLLMFunctionCall(db Database, referenceLanguage string, functionCall string) []string {
	functionCall = strings.TrimSpace(functionCall)

	if strings.HasPrefix(functionCall, "SEARCH_KEYWORDS:") {
		keywordStr := strings.TrimPrefix(functionCall, "SEARCH_KEYWORDS:")
		keywords := strings.Split(keywordStr, ",")
		return searchPrayersByKeywords(db, referenceLanguage, keywords, 10)
	}

	if strings.HasPrefix(functionCall, "SEARCH_LENGTH:") {
		lengthStr := strings.TrimPrefix(functionCall, "SEARCH_LENGTH:")
		parts := strings.Split(lengthStr, "-")
		if len(parts) == 2 {
			if minWords, err1 := strconv.Atoi(strings.TrimSpace(parts[0])); err1 == nil {
				if maxWords, err2 := strconv.Atoi(strings.TrimSpace(parts[1])); err2 == nil {
					return searchPrayersByLength(db, referenceLanguage, minWords, maxWords, 10)
				}
			}
		}
		return []string{"Invalid length format. Use: SEARCH_LENGTH:min-max (e.g., SEARCH_LENGTH:50-100)"}
	}

	if strings.HasPrefix(functionCall, "SEARCH_OPENING:") {
		openingText := strings.TrimPrefix(functionCall, "SEARCH_OPENING:")
		return searchPrayersByOpening(db, referenceLanguage, openingText, 10)
	}

	if strings.HasPrefix(functionCall, "GET_FULL_TEXT:") {
		phelpsCode := strings.TrimSpace(strings.TrimPrefix(functionCall, "GET_FULL_TEXT:"))
		return getFullTextByPhelps(db, referenceLanguage, phelpsCode)
	}

	if strings.HasPrefix(functionCall, "GET_PARTIAL_TEXT:") {
		args := strings.TrimSpace(strings.TrimPrefix(functionCall, "GET_PARTIAL_TEXT:"))
		return getPartialTextByPhelps(db, referenceLanguage, args)
	}

	if strings.HasPrefix(functionCall, "FINAL_ANSWER:") {
		args := strings.TrimSpace(strings.TrimPrefix(functionCall, "FINAL_ANSWER:"))
		return processFinalAnswer(args)
	}

	if functionCall == "GET_STATS" {
		phelpsContext := buildPhelpsContext(db, referenceLanguage)
		return []string{fmt.Sprintf("Database contains %d prayers with context for matching.", len(phelpsContext))}
	}

	return []string{"Unknown function. Available functions: SEARCH_KEYWORDS:word1,word2,word3 | SEARCH_LENGTH:min-max | SEARCH_OPENING:text | GET_FULL_TEXT:phelps_code | GET_PARTIAL_TEXT:phelps_code,range | FINAL_ANSWER:phelps_code,confidence,reasoning | GET_STATS"}
}

// Get full text of a prayer by Phelps code
func getFullTextByPhelps(db Database, referenceLanguage string, phelpsCode string) []string {
	phelpsCode = strings.TrimSpace(phelpsCode)
	if phelpsCode == "" {
		return []string{"Error: No Phelps code provided. Use: GET_FULL_TEXT:AB00001FIR"}
	}

	// Find the prayer with the specified Phelps code in the reference language
	for _, writing := range db.Writing {
		if writing.Language == referenceLanguage && writing.Phelps == phelpsCode {
			if writing.Text == "" {
				return []string{fmt.Sprintf("Found Phelps code %s but text is empty.", phelpsCode)}
			}

			// Return the full text with some metadata
			result := fmt.Sprintf("FULL TEXT for %s (%s):\n\n%s", phelpsCode, writing.Name, writing.Text)
			return []string{result}
		}
	}

	// If not found, provide helpful suggestions
	var availablePhelps []string
	for _, writing := range db.Writing {
		if writing.Language == referenceLanguage && writing.Phelps != "" {
			availablePhelps = append(availablePhelps, writing.Phelps)
			if len(availablePhelps) >= 10 { // Limit suggestions to first 10
				break
			}
		}
	}

	if len(availablePhelps) > 0 {
		return []string{fmt.Sprintf("Phelps code '%s' not found. Available codes: %s", phelpsCode, strings.Join(availablePhelps[:min(5, len(availablePhelps))], ", "))}
	}

	return []string{fmt.Sprintf("Phelps code '%s' not found and no reference prayers available.", phelpsCode)}
}

// Get partial text of a prayer by Phelps code with various paging options
func getPartialTextByPhelps(db Database, referenceLanguage string, args string) []string {
	parts := strings.Split(args, ",")
	if len(parts) < 2 {
		return []string{"Error: GET_PARTIAL_TEXT requires format: phelps_code,start-end OR phelps_code,from:word,to:word OR phelps_code,from:word OR phelps_code,to:word"}
	}

	phelpsCode := strings.TrimSpace(parts[0])
	if phelpsCode == "" {
		return []string{"Error: No Phelps code provided"}
	}

	// Find the prayer with the specified Phelps code
	var prayerText string
	var prayerName string
	for _, writing := range db.Writing {
		if writing.Language == referenceLanguage && writing.Phelps == phelpsCode {
			prayerText = writing.Text
			prayerName = writing.Name
			break
		}
	}

	if prayerText == "" {
		return []string{fmt.Sprintf("Phelps code '%s' not found", phelpsCode)}
	}

	// Parse the range/search parameters
	rangeParam := strings.TrimSpace(parts[1])

	// Handle character range format: start-end
	if strings.Contains(rangeParam, "-") && !strings.Contains(rangeParam, ":") {
		rangeParts := strings.Split(rangeParam, "-")
		if len(rangeParts) == 2 {
			start, err1 := strconv.Atoi(strings.TrimSpace(rangeParts[0]))
			end, err2 := strconv.Atoi(strings.TrimSpace(rangeParts[1]))

			if err1 != nil || err2 != nil {
				return []string{"Error: Invalid character range format. Use: start-end (e.g., 100-500)"}
			}

			if start < 0 {
				start = 0
			}
			if end > len(prayerText) {
				end = len(prayerText)
			}
			if start >= end {
				return []string{"Error: Start position must be less than end position"}
			}

			excerpt := prayerText[start:end]
			return []string{fmt.Sprintf("PARTIAL TEXT for %s (%s) [chars %d-%d]:\n\n%s", phelpsCode, prayerName, start, end, excerpt)}
		}
	}

	// Handle search term format: from:word, to:word, from:word,to:word
	var fromWord, toWord string

	// Parse additional parameters
	for i := 1; i < len(parts); i++ {
		param := strings.TrimSpace(parts[i])
		if strings.HasPrefix(param, "from:") {
			fromWord = strings.TrimSpace(strings.TrimPrefix(param, "from:"))
		} else if strings.HasPrefix(param, "to:") {
			toWord = strings.TrimSpace(strings.TrimPrefix(param, "to:"))
		}
	}

	// Apply search term filtering
	startIdx := 0
	endIdx := len(prayerText)

	if fromWord != "" {
		idx := strings.Index(strings.ToLower(prayerText), strings.ToLower(fromWord))
		if idx != -1 {
			startIdx = idx
		} else {
			return []string{fmt.Sprintf("Error: Start word '%s' not found in prayer text", fromWord)}
		}
	}

	if toWord != "" {
		searchText := prayerText[startIdx:]
		idx := strings.Index(strings.ToLower(searchText), strings.ToLower(toWord))
		if idx != -1 {
			endIdx = startIdx + idx + len(toWord)
		} else {
			return []string{fmt.Sprintf("Error: End word '%s' not found in prayer text", toWord)}
		}
	}

	if startIdx >= endIdx {
		return []string{"Error: Start position is after end position"}
	}

	excerpt := prayerText[startIdx:endIdx]

	var rangeDesc string
	if fromWord != "" && toWord != "" {
		rangeDesc = fmt.Sprintf("from '%s' to '%s'", fromWord, toWord)
	} else if fromWord != "" {
		rangeDesc = fmt.Sprintf("from '%s' to end", fromWord)
	} else if toWord != "" {
		rangeDesc = fmt.Sprintf("from start to '%s'", toWord)
	}

	return []string{fmt.Sprintf("PARTIAL TEXT for %s (%s) [%s]:\n\n%s", phelpsCode, prayerName, rangeDesc, excerpt)}
}

// Process final answer from tool call
func processFinalAnswer(args string) []string {
	parts := strings.Split(args, ",")
	if len(parts) < 3 {
		return []string{"Error: FINAL_ANSWER requires format: phelps_code,confidence,reasoning (e.g., FINAL_ANSWER:AB00001FIR,85,This prayer matches based on distinctive phrases)"}
	}

	phelpsCode := strings.TrimSpace(parts[0])
	confidenceStr := strings.TrimSpace(parts[1])
	reasoning := strings.TrimSpace(strings.Join(parts[2:], ","))

	confidence, err := strconv.Atoi(confidenceStr)
	if err != nil {
		return []string{fmt.Sprintf("Error: Invalid confidence value '%s'. Must be 0-100", confidenceStr)}
	}

	if confidence < 0 || confidence > 100 {
		return []string{"Error: Confidence must be between 0-100"}
	}

	if phelpsCode == "" {
		return []string{"Error: Phelps code cannot be empty"}
	}

	if reasoning == "" {
		return []string{"Error: Reasoning cannot be empty"}
	}

	return []string{fmt.Sprintf("FINAL ANSWER RECEIVED:\nPhelps: %s\nConfidence: %d\nReasoning: %s", phelpsCode, confidence, reasoning)}
}

// Prepare legacy header with all prayer contexts (old method)
func prepareLLMHeaderLegacy(db Database, targetLanguage, referenceLanguage string) string {
	if targetLanguage == "" {
		targetLanguage = "English"
	}
	if referenceLanguage == "" {
		referenceLanguage = "English"
	}

	// Build rich context for all Phelps codes
	phelpsContext := buildPhelpsContext(db, referenceLanguage)

	var phelpsInfo []string
	for _, context := range phelpsContext {
		phelpsInfo = append(phelpsInfo, context)
	}
	sort.Strings(phelpsInfo)

	header := fmt.Sprintf(`You are an expert in Bahá'í writings and prayers. Your task is to match a prayer text in %s to known Phelps codes.

Known Phelps codes with detailed context (reference: %s):

%s

Instructions:
1. Analyze the provided prayer text in %s
2. Compare it with the context provided above (opening lines, key words, length, etc.)
3. Match it to the most appropriate Phelps code based on content similarity
4. Provide a confidence score (0-100%%)
5. Give detailed reasoning explaining the match

Response format:
Phelps: [CODE]
Confidence: [PERCENTAGE]
Reasoning: [Your detailed explanation comparing the text with the matched prayer's characteristics]

If you cannot find a match with reasonable confidence (>70%%), respond with:
Phelps: UNKNOWN
Confidence: 0
Reasoning: [Explanation of why no match was found, mentioning what you looked for]

`, targetLanguage, referenceLanguage, strings.Join(phelpsInfo, "\n"), targetLanguage)

	return header
}

// Prepare interactive header for LLM calls
func prepareLLMHeader(db Database, targetLanguage, referenceLanguage string) string {
	if targetLanguage == "" {
		targetLanguage = "English"
	}
	if referenceLanguage == "" {
		referenceLanguage = "English"
	}

	phelpsContext := buildPhelpsContext(db, referenceLanguage)

	header := fmt.Sprintf(`You are an expert in Bahá'í writings and prayers. Your task is to match a prayer text in %s to known Phelps codes.

Instead of receiving all prayer contexts at once, you can interactively search the prayer database using these functions:

AVAILABLE FUNCTIONS:
- SEARCH_KEYWORDS:word1,word2,word3  (Search by keywords in %s, e.g., SEARCH_KEYWORDS:lord,god,assist)
- SEARCH_LENGTH:min-max              (Search by word count, e.g., SEARCH_LENGTH:50-150)
- SEARCH_OPENING:text                (Search by similar opening text in %s, e.g., SEARCH_OPENING:O Lord my God)
- GET_FULL_TEXT:phelps_code          (Get complete prayer text, e.g., GET_FULL_TEXT:AB00001FIR)
- GET_PARTIAL_TEXT:phelps_code,range (Get part of prayer, e.g., GET_PARTIAL_TEXT:AB00001FIR,100-500 or GET_PARTIAL_TEXT:AB00001FIR,from:Lord,to:Amen)
- FINAL_ANSWER:phelps_code,confidence,reasoning (Submit final match, e.g., FINAL_ANSWER:AB00001FIR,85,This prayer matches based on distinctive phrases)
- GET_STATS                          (Get database statistics)

WORKFLOW:
1. Analyze the provided prayer text in %s
2. Use functions to search for similar prayers (translate key concepts to %s for searching)
3. Narrow down candidates with additional searches
4. When you find a likely match, use FINAL_ANSWER function to submit your result

IMPORTANT: All search terms must be in %s since that's the language of the reference database.

FUNCTION CALL FORMAT:
To use a function, include it on its own line in your response. For example:
SEARCH_KEYWORDS:assist,firm,steadfast

FINAL RESPONSE FORMAT:
When ready to give your final answer, use the FINAL_ANSWER function:
FINAL_ANSWER:AB00001FIR,85,This prayer matches based on distinctive phrases and structure

If you cannot find a match with reasonable confidence (>70%), use:
FINAL_ANSWER:UNKNOWN,0,Explanation of why no match was found

Database size: %d prayers available for searching (reference: %s)

`, targetLanguage, referenceLanguage, referenceLanguage, targetLanguage, referenceLanguage, referenceLanguage, len(phelpsContext), referenceLanguage)

	return header
}

// Interactive LLM conversation with function call support using Ollama API
func callLLMInteractive(db Database, referenceLanguage string, prompt string, useGemini bool, textLength int) (LLMResponse, error) {
	maxRounds := 5 // Maximum conversation rounds to prevent infinite loops

	// Check if we should skip Gemini due to quota exceeded
	if useGemini && atomic.LoadInt32(&geminiQuotaExceeded) == 1 {
		useGemini = false
		fmt.Printf("    ⚠️  Gemini quota exceeded - using only Ollama for this prayer\n")
		log.Printf("Gemini quota exceeded - using only Ollama")
	}

	fmt.Printf("    🤖 Starting interactive LLM conversation...\n")

	// Initialize conversation with system prompt
	messages := []OllamaMessage{
		{Role: "user", Content: prompt},
	}

	for round := 1; round <= maxRounds; round++ {
		fmt.Printf("    📝 Round %d: Calling LLM...\n", round)

		var rawResponse string
		var err error

		if useGemini {
			// Try Gemini first, fallback to Ollama
			response, geminiErr := callLLM(messages[len(messages)-1].Content, useGemini, textLength)
			if geminiErr == nil {
				rawResponse = response.Reasoning
				if rawResponse == "" {
					rawResponse = fmt.Sprintf("Phelps: %s\nConfidence: %.1f\nReasoning: %s",
						response.PhelpsCode, response.Confidence*100, response.Reasoning)
				}
			} else {
				// Check if this is a quota exceeded error
				errorStr := strings.ToLower(geminiErr.Error())
				if strings.Contains(errorStr, "quota") && (strings.Contains(errorStr, "exceeded") || strings.Contains(errorStr, "exhausted")) {
					atomic.StoreInt32(&geminiQuotaExceeded, 1)
					fmt.Printf("    ⚠️  Gemini quota exceeded - switching to Ollama for remaining requests\n")
					log.Printf("Gemini quota exceeded - continuing with Ollama only")
					useGemini = false // Disable Gemini for remaining rounds
				}

				// Fallback to Ollama API
				rawResponse, err = CallOllamaWithMessages(messages, textLength)
				if err != nil {
					fmt.Printf("    ❌ Both LLM calls failed: Gemini: %v, Ollama: %v\n", geminiErr, err)
					return LLMResponse{}, fmt.Errorf("both LLM services failed: %v", err)
				}
			}
		} else {
			// Use Ollama API directly
			rawResponse, err = CallOllamaWithMessages(messages, textLength)
			if err != nil {
				fmt.Printf("    ❌ Ollama API call failed: %v\n", err)
				return LLMResponse{}, err
			}
		}

		// Show LLM response
		fmt.Printf("    💭 LLM Response:\n")
		lines := strings.Split(rawResponse, "\n")
		for _, line := range lines {
			if strings.TrimSpace(line) != "" {
				fmt.Printf("       %s\n", line)
			}
		}

		// Add LLM response to conversation
		messages = append(messages, OllamaMessage{Role: "assistant", Content: rawResponse})

		// Parse function calls (supports multiple formats)
		functionCalls, invalidCalls := extractAllFunctionCalls(rawResponse)

		// Check for FINAL_ANSWER function call
		var finalAnswerCall string
		var otherCalls []string
		for _, call := range functionCalls {
			if strings.HasPrefix(call, "FINAL_ANSWER:") {
				finalAnswerCall = call
			} else {
				otherCalls = append(otherCalls, call)
			}
		}

		// If we have a FINAL_ANSWER call, process it
		if finalAnswerCall != "" {
			results := processLLMFunctionCall(db, referenceLanguage, finalAnswerCall)
			if len(results) > 0 && strings.HasPrefix(results[0], "FINAL ANSWER RECEIVED:") {
				// Parse the final answer from the result
				lines := strings.Split(results[0], "\n")
				var phelpsCode, reasoning string
				var confidence float64

				for _, line := range lines {
					if strings.HasPrefix(line, "Phelps: ") {
						phelpsCode = strings.TrimSpace(strings.TrimPrefix(line, "Phelps: "))
					} else if strings.HasPrefix(line, "Confidence: ") {
						confStr := strings.TrimSpace(strings.TrimPrefix(line, "Confidence: "))
						if conf, err := strconv.ParseFloat(confStr, 64); err == nil {
							confidence = conf
						}
					} else if strings.HasPrefix(line, "Reasoning: ") {
						reasoning = strings.TrimSpace(strings.TrimPrefix(line, "Reasoning: "))
					}
				}

				fmt.Printf("    ✅ Valid final answer received via tool call!\n")
				return LLMResponse{
					PhelpsCode: phelpsCode,
					Confidence: confidence,
					Reasoning:  reasoning,
				}, nil
			}
		}

		// Parse the response for legacy final answer format (fallback)
		parsedResponse := parseLLMResponse(rawResponse)
		finalAnswer := validateFinalAnswer(rawResponse, parsedResponse)

		if finalAnswer.IsValid && len(otherCalls) == 0 && len(invalidCalls) == 0 {
			fmt.Printf("    ✅ Valid final answer received!\n")
			return finalAnswer.Response, nil
		}

		// Handle invalid function calls
		if len(invalidCalls) > 0 {
			fmt.Printf("    ⚠️  Found %d invalid function call(s):\n", len(invalidCalls))
			systemMessage := "ERROR - Invalid function calls detected:\n"

			for _, invalidCall := range invalidCalls {
				fmt.Printf("       ❌ %s\n", invalidCall.Error)
				systemMessage += fmt.Sprintf("ERROR: %s\n", invalidCall.Error)
			}

			systemMessage += "\nValid function formats:\n"
			systemMessage += "- SEARCH_KEYWORDS:word1,word2,word3\n"
			systemMessage += "- SEARCH_LENGTH:min-max\n"
			systemMessage += "- SEARCH_OPENING:text\n"
			systemMessage += "- GET_FULL_TEXT:phelps_code\n"
			systemMessage += "- GET_PARTIAL_TEXT:phelps_code,range\n"
			systemMessage += "- FINAL_ANSWER:phelps_code,confidence,reasoning\n"
			systemMessage += "- GET_STATS\n"
			systemMessage += "\nPlease use the correct format and try again:"

			messages = append(messages, OllamaMessage{Role: "user", Content: systemMessage})
			fmt.Printf("    🔄 Asking LLM to correct function call syntax...\n")
			continue
		}

		if len(functionCalls) == 0 && !finalAnswer.IsValid {
			// No valid function calls and no valid final answer
			fmt.Printf("    ❌ No valid function calls or final answer found\n")
			systemMessage := "ERROR - Expected either:\n"
			systemMessage += "1. Valid function calls (SEARCH_KEYWORDS:word1,word2 etc), OR\n"
			systemMessage += "2. Final answer using FINAL_ANSWER:phelps_code,confidence,reasoning\n"
			systemMessage += "\nPlease provide either function calls to search, or use FINAL_ANSWER to submit your result:"

			messages = append(messages, OllamaMessage{Role: "user", Content: systemMessage})
			fmt.Printf("    🔄 Asking LLM to provide valid response...\n")
			continue
		}

		// Process valid function calls
		if len(functionCalls) > 0 {
			fmt.Printf("    🔍 Processing %d function call(s):\n", len(functionCalls))
			systemMessage := "Function results:\n"

			for _, functionCall := range functionCalls {
				fmt.Printf("       📞 %s\n", functionCall)
				results := processLLMFunctionCall(db, referenceLanguage, functionCall)

				fmt.Printf("       📋 Results:\n")
				for _, result := range results {
					// Truncate long results for display
					displayResult := result
					if len(displayResult) > 120 {
						displayResult = displayResult[:120] + "..."
					}
					fmt.Printf("          %s\n", displayResult)
				}

				systemMessage += fmt.Sprintf("\n%s -> %s\n", functionCall, strings.Join(results, "\n"))
			}

			systemMessage += "\nPlease continue your analysis or provide your final answer:"
			messages = append(messages, OllamaMessage{Role: "user", Content: systemMessage})
			fmt.Printf("    ⏳ Continuing to round %d...\n", round+1)
		}
	}

	// If we've reached max rounds without a final answer, return unknown
	fmt.Printf("    ⚠️  Maximum conversation rounds exceeded\n")
	return LLMResponse{
		PhelpsCode: "UNKNOWN",
		Confidence: 0.0,
		Reasoning:  "Interactive search exceeded maximum conversation rounds",
	}, nil
}

// InvalidFunctionCall represents a malformed function call attempt
type InvalidFunctionCall struct {
	Original string
	Error    string
}

// FinalAnswerResult represents the validation result of a final answer
type FinalAnswerResult struct {
	IsValid  bool
	Response LLMResponse
	Error    string
}

// Extract function calls from LLM response text (supports multiple formats)
func extractAllFunctionCalls(text string) ([]string, []InvalidFunctionCall) {
	var validCalls []string
	var invalidCalls []InvalidFunctionCall

	// First try to parse as tool_calls JSON format
	if toolCalls := parseToolCallsFormat(text); len(toolCalls) > 0 {
		return toolCalls, invalidCalls
	}

	lines := strings.Split(text, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Check for standard format
		if strings.HasPrefix(line, "SEARCH_KEYWORDS:") ||
			strings.HasPrefix(line, "SEARCH_LENGTH:") ||
			strings.HasPrefix(line, "SEARCH_OPENING:") ||
			strings.HasPrefix(line, "GET_FULL_TEXT:") ||
			strings.HasPrefix(line, "GET_PARTIAL_TEXT:") ||
			strings.HasPrefix(line, "FINAL_ANSWER:") ||
			line == "GET_STATS" {
			validCalls = append(validCalls, line)
			continue
		}

		// Check for JSON function call format (like gpt-oss uses)
		if strings.Contains(line, `"function":"SEARCH_KEYWORDS"`) && strings.Contains(line, `"arguments"`) {
			// Parse JSON function call
			functionCall := parseJSONFunctionCall(line, "SEARCH_KEYWORDS")
			if functionCall != "" {
				validCalls = append(validCalls, functionCall)
			} else {
				invalidCalls = append(invalidCalls, InvalidFunctionCall{
					Original: line,
					Error:    "Invalid JSON function call format for SEARCH_KEYWORDS. Use: SEARCH_KEYWORDS:word1,word2,word3",
				})
			}
			continue
		}

		// Check for other JSON function formats
		if strings.Contains(line, `"function":"SEARCH_LENGTH"`) && strings.Contains(line, `"arguments"`) {
			functionCall := parseJSONFunctionCall(line, "SEARCH_LENGTH")
			if functionCall != "" {
				validCalls = append(validCalls, functionCall)
			} else {
				invalidCalls = append(invalidCalls, InvalidFunctionCall{
					Original: line,
					Error:    "Invalid JSON function call format for SEARCH_LENGTH. Use: SEARCH_LENGTH:min-max",
				})
			}
			continue
		}

		if strings.Contains(line, `"function":"SEARCH_OPENING"`) && strings.Contains(line, `"arguments"`) {
			functionCall := parseJSONFunctionCall(line, "SEARCH_OPENING")
			if functionCall != "" {
				validCalls = append(validCalls, functionCall)
			} else {
				invalidCalls = append(invalidCalls, InvalidFunctionCall{
					Original: line,
					Error:    "Invalid JSON function call format for SEARCH_OPENING. Use: SEARCH_OPENING:text",
				})
			}
			continue
		}

		if strings.Contains(line, `"function":"GET_FULL_TEXT"`) && strings.Contains(line, `"arguments"`) {
			functionCall := parseJSONFunctionCall(line, "GET_FULL_TEXT")
			if functionCall != "" {
				validCalls = append(validCalls, functionCall)
			} else {
				invalidCalls = append(invalidCalls, InvalidFunctionCall{
					Original: line,
					Error:    "Invalid JSON function call format for GET_FULL_TEXT. Use: GET_FULL_TEXT:phelps_code",
				})
			}
			continue
		}

		if strings.Contains(line, `"function":"GET_PARTIAL_TEXT"`) && strings.Contains(line, `"arguments"`) {
			functionCall := parseJSONFunctionCall(line, "GET_PARTIAL_TEXT")
			if functionCall != "" {
				validCalls = append(validCalls, functionCall)
			} else {
				invalidCalls = append(invalidCalls, InvalidFunctionCall{
					Original: line,
					Error:    "Invalid JSON function call format for GET_PARTIAL_TEXT. Use: GET_PARTIAL_TEXT:phelps_code,range",
				})
			}
			continue
		}

		if strings.Contains(line, `"function":"FINAL_ANSWER"`) && strings.Contains(line, `"arguments"`) {
			functionCall := parseJSONFunctionCall(line, "FINAL_ANSWER")
			if functionCall != "" {
				validCalls = append(validCalls, functionCall)
			} else {
				invalidCalls = append(invalidCalls, InvalidFunctionCall{
					Original: line,
					Error:    "Invalid JSON function call format for FINAL_ANSWER. Use: FINAL_ANSWER:phelps_code,confidence,reasoning",
				})
			}
			continue
		}

		if strings.Contains(line, `"function":"GET_STATS"`) {
			validCalls = append(validCalls, "GET_STATS")
			continue
		}

		// Check for malformed function attempts
		upperLine := strings.ToUpper(line)
		if strings.Contains(upperLine, "SEARCH_KEYWORDS") ||
			strings.Contains(upperLine, "SEARCH_LENGTH") ||
			strings.Contains(upperLine, "SEARCH_OPENING") ||
			strings.Contains(upperLine, "GET_FULL_TEXT") ||
			strings.Contains(upperLine, "GET_PARTIAL_TEXT") ||
			strings.Contains(upperLine, "FINAL_ANSWER") ||
			strings.Contains(upperLine, "GET_STATS") {

			// Check if it's a malformed attempt (not in correct format)
			if !strings.HasPrefix(line, "SEARCH_KEYWORDS:") &&
				!strings.HasPrefix(line, "SEARCH_LENGTH:") &&
				!strings.HasPrefix(line, "SEARCH_OPENING:") &&
				!strings.HasPrefix(line, "GET_FULL_TEXT:") &&
				!strings.HasPrefix(line, "GET_PARTIAL_TEXT:") &&
				!strings.HasPrefix(line, "FINAL_ANSWER:") &&
				line != "GET_STATS" {

				invalidCalls = append(invalidCalls, InvalidFunctionCall{
					Original: line,
					Error:    fmt.Sprintf("Malformed function call: '%s'. Use correct format like SEARCH_KEYWORDS:word1,word2", line),
				})
			}
		}
	}

	return validCalls, invalidCalls
}

// ToolCall represents a single tool call in the JSON format
type ToolCall struct {
	Function struct {
		Name      string `json:"name"`
		Arguments struct {
			Arguments string `json:"arguments"`
			Name      string `json:"name"`
		} `json:"arguments"`
	} `json:"function"`
}

// ToolCallsResponse represents the tool_calls JSON format
type ToolCallsResponse struct {
	ToolCalls []ToolCall `json:"tool_calls"`
}

// Parse tool_calls JSON format from Ollama responses
func parseToolCallsFormat(text string) []string {
	var validCalls []string

	// Try to find tool_calls JSON in the response
	if !strings.Contains(text, "tool_calls") {
		return validCalls
	}

	// Extract JSON part containing tool_calls
	startIdx := strings.Index(text, `"tool_calls"`)
	if startIdx == -1 {
		return validCalls
	}

	// Find the start of the JSON object
	jsonStart := strings.LastIndex(text[:startIdx], "{")
	if jsonStart == -1 {
		jsonStart = 0
	}

	// Find the end of the JSON object
	braceCount := 0
	jsonEnd := len(text)
	for i := jsonStart; i < len(text); i++ {
		if text[i] == '{' {
			braceCount++
		} else if text[i] == '}' {
			braceCount--
			if braceCount == 0 {
				jsonEnd = i + 1
				break
			}
		}
	}

	jsonStr := text[jsonStart:jsonEnd]

	var toolCallsResp ToolCallsResponse
	if err := json.Unmarshal([]byte(jsonStr), &toolCallsResp); err != nil {
		return validCalls
	}

	// Convert tool calls to standard format
	for _, toolCall := range toolCallsResp.ToolCalls {
		switch toolCall.Function.Name {
		case "SEARCH_KEYWORDS":
			if args := toolCall.Function.Arguments.Arguments; args != "" {
				validCalls = append(validCalls, fmt.Sprintf("SEARCH_KEYWORDS:%s", args))
			}
		case "SEARCH_LENGTH":
			if args := toolCall.Function.Arguments.Arguments; args != "" {
				validCalls = append(validCalls, fmt.Sprintf("SEARCH_LENGTH:%s", args))
			}
		case "SEARCH_OPENING":
			if args := toolCall.Function.Arguments.Arguments; args != "" {
				validCalls = append(validCalls, fmt.Sprintf("SEARCH_OPENING:%s", args))
			}
		case "GET_FULL_TEXT":
			if args := toolCall.Function.Arguments.Arguments; args != "" {
				validCalls = append(validCalls, fmt.Sprintf("GET_FULL_TEXT:%s", args))
			}
		case "GET_PARTIAL_TEXT":
			if args := toolCall.Function.Arguments.Arguments; args != "" {
				validCalls = append(validCalls, fmt.Sprintf("GET_PARTIAL_TEXT:%s", args))
			}
		case "FINAL_ANSWER":
			if args := toolCall.Function.Arguments.Arguments; args != "" {
				validCalls = append(validCalls, fmt.Sprintf("FINAL_ANSWER:%s", args))
			}
		case "GET_STATS":
			validCalls = append(validCalls, "GET_STATS")
		}
	}

	return validCalls
}

// Parse JSON function call and convert to standard format
func parseJSONFunctionCall(line, functionName string) string {
	// Simple JSON parsing for function calls
	if strings.Contains(line, `"arguments":"`) {
		start := strings.Index(line, `"arguments":"`)
		if start == -1 {
			return ""
		}
		start += len(`"arguments":"`)
		end := strings.Index(line[start:], `"`)
		if end == -1 {
			return ""
		}
		args := line[start : start+end]
		return functionName + ":" + args
	}
	return ""
}

// Validate if the response contains a proper final answer
func validateFinalAnswer(text string, response LLMResponse) FinalAnswerResult {
	lines := strings.Split(text, "\n")

	var phelpsLine, confidenceLine, reasoningLine string

	for _, line := range lines {
		line = strings.TrimSpace(line)
		lowerLine := strings.ToLower(line)

		if strings.HasPrefix(lowerLine, "phelps:") {
			phelpsLine = line
		} else if strings.HasPrefix(lowerLine, "confidence:") {
			confidenceLine = line
		} else if strings.HasPrefix(lowerLine, "reasoning:") {
			reasoningLine = line
		}
	}

	// Check if we have all required components
	if phelpsLine == "" || confidenceLine == "" || reasoningLine == "" {
		return FinalAnswerResult{
			IsValid: false,
			Error:   "Missing required final answer format. Need: Phelps: [CODE], Confidence: [0-100], Reasoning: [explanation]",
		}
	}

	// Parse the response
	phelpsCode := strings.TrimSpace(strings.TrimPrefix(phelpsLine, "Phelps:"))
	phelpsCode = strings.TrimSpace(strings.TrimPrefix(phelpsCode, "phelps:"))

	confidenceStr := strings.TrimSpace(strings.TrimPrefix(confidenceLine, "Confidence:"))
	confidenceStr = strings.TrimSpace(strings.TrimPrefix(confidenceStr, "confidence:"))
	confidenceStr = strings.TrimSuffix(confidenceStr, "%")

	reasoning := strings.TrimSpace(strings.TrimPrefix(reasoningLine, "Reasoning:"))
	reasoning = strings.TrimSpace(strings.TrimPrefix(reasoning, "reasoning:"))

	// Validate confidence is a number
	confidence, err := strconv.ParseFloat(confidenceStr, 64)
	if err != nil {
		return FinalAnswerResult{
			IsValid: false,
			Error:   fmt.Sprintf("Invalid confidence value '%s'. Must be a number 0-100", confidenceStr),
		}
	}

	// Convert percentage to decimal if needed
	if confidence > 1.0 {
		confidence = confidence / 100.0
	}

	// Validate Phelps code format (basic validation)
	if phelpsCode == "" {
		return FinalAnswerResult{
			IsValid: false,
			Error:   "Empty Phelps code. Use format like AB00001FIR or UNKNOWN",
		}
	}

	return FinalAnswerResult{
		IsValid: true,
		Response: LLMResponse{
			PhelpsCode: phelpsCode,
			Confidence: confidence,
			Reasoning:  reasoning,
		},
	}
}

// Legacy function for backward compatibility
func extractFunctionCalls(text string) []string {
	validCalls, _ := extractAllFunctionCalls(text)
	return validCalls
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
func findOptimalDefaultLanguage(db Database, noPriority bool) string {
	missing := calculateMissingPrayersPerLanguage(db)

	if len(missing) == 0 {
		return "en" // fallback to English
	}

	// Priority languages (more likely to have good LLM support)
	var priorityLangs map[string]bool
	if !noPriority {
		priorityLangs = map[string]bool{
			"en": true, "es": true, "fr": true, "de": true, "it": true,
			"pt": true, "ru": true, "ja": true, "zh": true, "ar": true,
			"fa": true, "tr": true, "hi": true, "ko": true,
		}
	} else {
		priorityLangs = make(map[string]bool) // Empty map - no priorities
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

// Get all languages sorted by priority (missing prayers count + preference)
func getLanguagesPrioritized(db Database, noPriority bool) []string {
	missing := calculateMissingPrayersPerLanguage(db)

	// Priority languages (more likely to have good LLM support)
	var priorityLangs map[string]bool
	if !noPriority {
		priorityLangs = map[string]bool{
			"en": true, "es": true, "fr": true, "de": true, "it": true,
			"pt": true, "ru": true, "ja": true, "zh": true, "ar": true,
			"fa": true, "tr": true, "hi": true, "ko": true,
		}
	} else {
		priorityLangs = make(map[string]bool) // Empty map - no priorities
	}

	type langPriority struct {
		lang     string
		missing  int
		priority bool
	}

	var langs []langPriority
	for lang, count := range missing {
		if count > 0 {
			langs = append(langs, langPriority{lang, count, priorityLangs[lang]})
		}
	}

	// Sort by priority (priority languages first), then by missing count (ascending)
	sort.Slice(langs, func(i, j int) bool {
		if langs[i].priority != langs[j].priority {
			return langs[i].priority // priority languages first
		}
		return langs[i].missing < langs[j].missing // fewer missing first
	})

	result := make([]string, len(langs))
	for i, lang := range langs {
		result[i] = lang.lang
	}
	return result
}

// Process random prayers from all languages
func processRandomPrayers(db *Database, referenceLanguage string, useGemini bool, reportFile *os.File, maxPrayers int, verbose bool) ([]Writing, error) {
	// Collect all unmatched prayers from all languages
	var allUnmatched []Writing
	for _, writing := range db.Writing {
		if writing.Phelps == "" || strings.TrimSpace(writing.Phelps) == "" {
			allUnmatched = append(allUnmatched, writing)
		}
	}

	if len(allUnmatched) == 0 {
		fmt.Printf("No unmatched prayers found across all languages!\n")
		return []Writing{}, nil
	}

	// Shuffle the prayers for randomness
	rand.Shuffle(len(allUnmatched), func(i, j int) {
		allUnmatched[i], allUnmatched[j] = allUnmatched[j], allUnmatched[i]
	})

	totalToProcess := len(allUnmatched)
	if maxPrayers > 0 && maxPrayers < totalToProcess {
		totalToProcess = maxPrayers
		allUnmatched = allUnmatched[:maxPrayers]
	}

	fmt.Printf("🎲 Lucky mode: Processing %d random prayers from all languages\n", totalToProcess)
	fmt.Fprintf(reportFile, "=== LUCKY MODE: Random Prayer Processing ===\n")
	fmt.Fprintf(reportFile, "Processing %d random prayers from all languages at %s\n\n", totalToProcess, time.Now().Format(time.RFC3339))

	// Process the shuffled prayers
	return processShuffledPrayers(db, allUnmatched, referenceLanguage, useGemini, reportFile, verbose)
}

// Process languages continuously in priority order with mode support
func processLanguagesContinuouslyWithMode(db *Database, referenceLanguage string, useGemini bool, reportFile *os.File, maxPrayers int, verbose bool, noPriority bool, legacyMode bool) ([]Writing, error) {
	languages := getLanguagesPrioritized(*db, noPriority)

	if len(languages) == 0 {
		fmt.Printf("No languages with missing prayers found!\n")
		return []Writing{}, nil
	}

	if noPriority {
		fmt.Printf("🔄 Continue mode: Processing languages by missing count (smallest first)\n")
	} else {
		fmt.Printf("🔄 Continue mode: Processing languages in priority order\n")
	}
	fmt.Printf("Language queue: %v\n", languages[:min(5, len(languages))])
	if len(languages) > 5 {
		fmt.Printf("... and %d more\n", len(languages)-5)
	}

	fmt.Fprintf(reportFile, "=== CONTINUE MODE: Continuous Language Processing ===\n")
	if noPriority {
		fmt.Fprintf(reportFile, "Processing languages by missing count (smallest first) at %s\n", time.Now().Format(time.RFC3339))
	} else {
		fmt.Fprintf(reportFile, "Processing languages in priority order at %s\n", time.Now().Format(time.RFC3339))
	}
	fmt.Fprintf(reportFile, "Language queue: %v\n\n", languages)

	var allUnmatched []Writing
	totalProcessed := 0

	for i, lang := range languages {
		if maxPrayers > 0 && totalProcessed >= maxPrayers {
			fmt.Printf("Reached maximum prayer limit (%d). Stopping.\n", maxPrayers)
			break
		}

		remainingQuota := 0
		if maxPrayers > 0 {
			remainingQuota = maxPrayers - totalProcessed
		}

		fmt.Printf("\n--- Processing language %d/%d: %s ---\n", i+1, len(languages), lang)
		fmt.Fprintf(reportFile, "\n--- Language %d/%d: %s ---\n", i+1, len(languages), lang)

		unmatchedForLang, err := processPrayersForLanguageWithMode(db, lang, referenceLanguage, useGemini, reportFile, remainingQuota, verbose, legacyMode)
		if err != nil {
			log.Printf("Error processing language %s: %v", lang, err)
			continue
		}

		allUnmatched = append(allUnmatched, unmatchedForLang...)

		// Count how many prayers were actually processed (not just unmatched)
		missing := calculateMissingPrayersPerLanguage(*db)
		processed := missing[lang] - len(unmatchedForLang) // approximation
		totalProcessed += processed

		if atomic.LoadInt32(&stopRequested) == 1 {
			fmt.Printf("Stop requested. Processed %d languages so far.\n", i+1)
			break
		}
	}

	return allUnmatched, nil
}

// Process languages continuously in priority order
func processLanguagesContinuously(db *Database, referenceLanguage string, useGemini bool, reportFile *os.File, maxPrayers int, verbose bool, noPriority bool) ([]Writing, error) {
	languages := getLanguagesPrioritized(*db, noPriority)

	if len(languages) == 0 {
		fmt.Printf("No languages with missing prayers found!\n")
		return []Writing{}, nil
	}

	if noPriority {
		fmt.Printf("🔄 Continue mode: Processing languages by missing count (smallest first)\n")
	} else {
		fmt.Printf("🔄 Continue mode: Processing languages in priority order\n")
	}
	fmt.Printf("Language queue: %v\n", languages[:min(5, len(languages))])
	if len(languages) > 5 {
		fmt.Printf("... and %d more\n", len(languages)-5)
	}

	fmt.Fprintf(reportFile, "=== CONTINUE MODE: Continuous Language Processing ===\n")
	if noPriority {
		fmt.Fprintf(reportFile, "Processing languages by missing count (smallest first) at %s\n", time.Now().Format(time.RFC3339))
	} else {
		fmt.Fprintf(reportFile, "Processing languages in priority order at %s\n", time.Now().Format(time.RFC3339))
	}
	fmt.Fprintf(reportFile, "Language queue: %v\n\n", languages)

	var allUnmatched []Writing
	totalProcessed := 0

	for i, lang := range languages {
		if maxPrayers > 0 && totalProcessed >= maxPrayers {
			fmt.Printf("Reached maximum prayer limit (%d). Stopping.\n", maxPrayers)
			break
		}

		remainingQuota := 0
		if maxPrayers > 0 {
			remainingQuota = maxPrayers - totalProcessed
		}

		fmt.Printf("\n--- Processing language %d/%d: %s ---\n", i+1, len(languages), lang)
		fmt.Fprintf(reportFile, "\n--- Language %d/%d: %s ---\n", i+1, len(languages), lang)

		unmatchedForLang, err := processPrayersForLanguage(db, lang, referenceLanguage, useGemini, reportFile, remainingQuota, verbose)
		if err != nil {
			log.Printf("Error processing language %s: %v", lang, err)
			continue
		}

		allUnmatched = append(allUnmatched, unmatchedForLang...)

		// Count how many prayers were actually processed (not just unmatched)
		missing := calculateMissingPrayersPerLanguage(*db)
		processed := missing[lang] - len(unmatchedForLang) // approximation
		totalProcessed += processed

		if atomic.LoadInt32(&stopRequested) == 1 {
			fmt.Printf("Stop requested. Processed %d languages so far.\n", i+1)
			break
		}
	}

	return allUnmatched, nil
}

// Helper function to process a list of shuffled prayers
func processShuffledPrayers(db *Database, prayers []Writing, referenceLanguage string, useGemini bool, reportFile *os.File, verbose bool) ([]Writing, error) {
	return processShuffledPrayersWithMode(db, prayers, referenceLanguage, useGemini, reportFile, verbose, false)
}

// Process random prayers from all languages with mode support
func processRandomPrayersWithMode(db *Database, referenceLanguage string, useGemini bool, reportFile *os.File, maxPrayers int, verbose bool, legacyMode bool) ([]Writing, error) {
	// Collect all unmatched prayers from all languages
	var allUnmatched []Writing
	for _, writing := range db.Writing {
		if writing.Phelps == "" || strings.TrimSpace(writing.Phelps) == "" {
			allUnmatched = append(allUnmatched, writing)
		}
	}

	if len(allUnmatched) == 0 {
		fmt.Printf("No unmatched prayers found across all languages!\n")
		return []Writing{}, nil
	}

	// Shuffle the prayers for randomness
	rand.Shuffle(len(allUnmatched), func(i, j int) {
		allUnmatched[i], allUnmatched[j] = allUnmatched[j], allUnmatched[i]
	})

	totalToProcess := len(allUnmatched)
	if maxPrayers > 0 && maxPrayers < totalToProcess {
		totalToProcess = maxPrayers
		allUnmatched = allUnmatched[:maxPrayers]
	}

	fmt.Printf("🎲 Lucky mode: Processing %d random prayers from all languages\n", totalToProcess)
	fmt.Fprintf(reportFile, "=== LUCKY MODE: Random Prayer Processing ===\n")
	fmt.Fprintf(reportFile, "Processing %d random prayers from all languages at %s\n\n", totalToProcess, time.Now().Format(time.RFC3339))

	// Process the shuffled prayers
	return processShuffledPrayersWithMode(db, allUnmatched, referenceLanguage, useGemini, reportFile, verbose, legacyMode)
}

func processShuffledPrayersWithMode(db *Database, prayers []Writing, referenceLanguage string, useGemini bool, reportFile *os.File, verbose bool, legacyMode bool) ([]Writing, error) {
	var unmatchedPrayers []Writing

	for i, writing := range prayers {
		if atomic.LoadInt32(&stopRequested) == 1 {
			fmt.Printf("Graceful stop requested. Processed %d prayers so far.\n", i)
			fmt.Fprintf(reportFile, "Graceful stop requested at %s. Processed %d prayers.\n", time.Now().Format(time.RFC3339), i)
			break
		}

		fmt.Printf("\n📿 Processing prayer %d/%d: %s\n", i+1, len(prayers), writing.Name)
		fmt.Printf("   Language: %s | Version: %s\n", writing.Language, writing.Version)
		if len(writing.Text) > 150 {
			fmt.Printf("   Preview: %s...\n", writing.Text[:150])
		} else {
			fmt.Printf("   Text: %s\n", writing.Text)
		}

		// Use the appropriate header for this prayer's language
		languageSpecificHeader := prepareLLMHeader(*db, writing.Language, referenceLanguage)
		prompt := languageSpecificHeader + "\n\nPrayer text to analyze:\n" + writing.Text

		fmt.Fprintf(reportFile, "Processing writing: %s (%s) (Version: %s)\n", writing.Name, writing.Language, writing.Version)

		fmt.Printf("   🧠 Analyzing with LLM...")

		var response LLMResponse
		var err error

		if legacyMode {
			// Use old prompt with all contexts
			oldPrompt := prepareLLMHeaderLegacy(*db, writing.Language, referenceLanguage) + "\n\nPrayer text to analyze:\n" + writing.Text
			response, err = callLLM(oldPrompt, useGemini, len(writing.Text))
		} else {
			// Use new interactive mode
			response, err = callLLMInteractive(*db, referenceLanguage, prompt, useGemini, len(writing.Text))
		}
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
		}

		// Always show the final decision clearly
		fmt.Printf("\n  🎯 FINAL DECISION:\n")
		fmt.Printf("     Prayer: %s (%s)\n", writing.Name, writing.Language)
		fmt.Printf("     Result: %s\n", response.PhelpsCode)
		fmt.Printf("     Confidence: %.1f%%\n", response.Confidence*100)
		fmt.Printf("     Reasoning: %s\n", response.Reasoning)

		if verbose {
			fmt.Printf("  ✓ Analysis complete!\n")
		}

		if response.PhelpsCode != "UNKNOWN" && response.Confidence >= 70.0 {
			if verbose {
				fmt.Printf("  MATCHED: %s -> database updated\n", response.PhelpsCode)
			}

			err := updateWritingPhelps(response.PhelpsCode, writing.Language, writing.Version)
			if err != nil {
				log.Printf("Error updating database: %v", err)
				fmt.Fprintf(reportFile, "  ERROR updating database: %v\n", err)
			} else {
				fmt.Fprintf(reportFile, "  MATCHED: %s (%.1f%% confidence) -> database updated\n", response.PhelpsCode, response.Confidence)
			}
		} else {
			unmatchedPrayers = append(unmatchedPrayers, writing)
			if verbose {
				if response.PhelpsCode == "UNKNOWN" {
					fmt.Printf("  UNMATCHED: %s\n", response.Reasoning)
				} else {
					fmt.Printf("  LOW CONFIDENCE: %s (%.1f%%) - added to unmatched list\n", response.PhelpsCode, response.Confidence)
				}
			}
			fmt.Fprintf(reportFile, "  UNMATCHED: %s (%.1f%% confidence) - %s\n", response.PhelpsCode, response.Confidence, response.Reasoning)
		}

		if verbose {
			fmt.Printf("  Waiting 1 second...\n")
		}
		time.Sleep(1 * time.Second)
	}

	return unmatchedPrayers, nil
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
func callLLM(prompt string, useGemini bool, textLength int) (LLMResponse, error) {
	return callLLMWithCaller(prompt, useGemini, textLength, DefaultLLMCaller{})
}

// callLLMWithCaller allows dependency injection for testing
func callLLMWithCaller(prompt string, useGemini bool, textLength int, caller LLMCaller) (LLMResponse, error) {
	var response string
	var geminiErr error
	var ollamaErr error
	var geminiResponse string
	var ollamaResponse string
	var triedGemini bool
	var triedOllama bool

	// Check if we should skip Gemini due to quota exceeded
	if useGemini && atomic.LoadInt32(&geminiQuotaExceeded) == 1 {
		useGemini = false
		log.Printf("Gemini quota exceeded - using only Ollama")
	}

	if useGemini {
		triedGemini = true
		response, geminiErr = caller.CallGemini(prompt)
		if geminiErr != nil {
			// Check if this is a quota exceeded error
			errorStr := strings.ToLower(geminiErr.Error())
			if strings.Contains(errorStr, "quota") && (strings.Contains(errorStr, "exceeded") || strings.Contains(errorStr, "exhausted")) {
				atomic.StoreInt32(&geminiQuotaExceeded, 1)
				log.Printf("Gemini quota exceeded - continuing with Ollama only")
			} else {
				log.Printf("Gemini call failed with error, falling back to Ollama: %v", geminiErr)
			}
		} else {
			geminiResponse = response
			parsed := parseLLMResponse(response)
			// Check if Gemini response is valid
			if parsed.PhelpsCode != "" {
				log.Printf("Gemini returned valid response")
				return parsed, nil
			}
			log.Printf("Gemini returned empty/invalid response (PhelpsCode empty), falling back to Ollama")
			truncatedResponse := truncateAndStore(response, "Gemini")
			log.Printf("Gemini raw response: %q", truncatedResponse)
		}

		// Try Ollama as fallback
		triedOllama = true
		response, ollamaErr = caller.CallOllama(prompt, textLength)
		if ollamaErr != nil {
			// Both failed with errors
			return LLMResponse{}, fmt.Errorf("both LLM services failed - Gemini error: %v, Ollama error: %v", geminiErr, ollamaErr)
		}
		ollamaResponse = response
	} else {
		triedOllama = true
		response, ollamaErr = caller.CallOllama(prompt, textLength)
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
				truncatedResponse := truncateAndStore(geminiResponse, "Gemini Debug")
				debugInfo.WriteString(fmt.Sprintf("Gemini raw response: %q\n", truncatedResponse))
			}
		}
		if triedOllama {
			debugInfo.WriteString(fmt.Sprintf("Ollama attempted: %v\n", ollamaErr == nil))
			if ollamaErr != nil {
				debugInfo.WriteString(fmt.Sprintf("Ollama error: %v\n", ollamaErr))
			} else {
				truncatedResponse := truncateAndStore(ollamaResponse, "Ollama Debug")
				debugInfo.WriteString(fmt.Sprintf("Ollama raw response: %q\n", truncatedResponse))
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
func processPrayersForLanguageWithMode(db *Database, targetLanguage, referenceLanguage string, useGemini bool, reportFile *os.File, maxPrayers int, verbose bool, legacyMode bool) ([]Writing, error) {
	header := prepareLLMHeader(*db, targetLanguage, referenceLanguage)
	processed := 0
	totalEligible := 0
	var unmatchedPrayers []Writing

	// Count eligible prayers
	for _, writing := range db.Writing {
		if writing.Language == targetLanguage && (writing.Phelps == "" || strings.TrimSpace(writing.Phelps) == "") {
			totalEligible++
		}
	}

	if totalEligible == 0 {
		fmt.Printf("No unmatched prayers found for language: %s\n", targetLanguage)
		fmt.Fprintf(reportFile, "No unmatched prayers found for language: %s\n", targetLanguage)
		return []Writing{}, nil
	}

	maxToShow := totalEligible
	if maxPrayers > 0 && maxPrayers < totalEligible {
		maxToShow = maxPrayers
		fmt.Printf("Found %d eligible prayers to process in language %s\n", totalEligible, targetLanguage)
		fmt.Printf("Will process first %d prayers (limited by -max flag)\n", maxToShow)
	} else {
		fmt.Printf("Found %d prayers to process in language %s\n", totalEligible, targetLanguage)
	}

	for _, writing := range db.Writing {
		if writing.Language != targetLanguage || (writing.Phelps != "" && strings.TrimSpace(writing.Phelps) != "") {
			continue
		}

		if atomic.LoadInt32(&stopRequested) == 1 {
			fmt.Printf("Graceful stop requested. Processed %d prayers so far.\n", processed)
			fmt.Fprintf(reportFile, "Graceful stop requested at %s. Processed %d prayers.\n", time.Now().Format(time.RFC3339), processed)
			break
		}

		if maxPrayers > 0 && processed >= maxPrayers {
			fmt.Printf("Reached maximum prayer limit (%d). Stopping.\n", maxPrayers)
			fmt.Fprintf(reportFile, "Reached maximum prayer limit (%d) at %s.\n", maxPrayers, time.Now().Format(time.RFC3339))
			break
		}

		processed++

		maxToProcess := totalEligible
		if maxPrayers > 0 && maxPrayers < totalEligible {
			maxToProcess = maxPrayers
		}

		fmt.Printf("\n📿 Processing prayer %d/%d: %s\n", processed, maxToProcess, writing.Name)
		fmt.Printf("   Language: %s | Version: %s\n", writing.Language, writing.Version)
		if len(writing.Text) > 150 {
			fmt.Printf("   Preview: %s...\n", writing.Text[:150])
		} else {
			fmt.Printf("   Text: %s\n", writing.Text)
		}

		prompt := header + "\n\nPrayer text to analyze:\n" + writing.Text

		fmt.Fprintf(reportFile, "Processing writing: %s (Version: %s)\n", writing.Name, writing.Version)

		fmt.Printf("   🧠 Analyzing with LLM...")

		var response LLMResponse
		var err error

		if legacyMode {
			// Use old prompt with all contexts
			oldPrompt := prepareLLMHeaderLegacy(*db, writing.Language, referenceLanguage) + "\n\nPrayer text to analyze:\n" + writing.Text
			response, err = callLLM(oldPrompt, useGemini, len(writing.Text))
		} else {
			// Use new interactive mode
			response, err = callLLMInteractive(*db, referenceLanguage, prompt, useGemini, len(writing.Text))
		}
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
		}

		// Always show the final decision clearly
		fmt.Printf("\n  🎯 FINAL DECISION:\n")
		fmt.Printf("     Prayer: %s (%s)\n", writing.Name, writing.Language)
		fmt.Printf("     Result: %s\n", response.PhelpsCode)
		fmt.Printf("     Confidence: %.1f%%\n", response.Confidence*100)
		fmt.Printf("     Reasoning: %s\n", response.Reasoning)

		if verbose {
			fmt.Printf("  ✓ Analysis complete!\n")
		}

		if response.PhelpsCode != "UNKNOWN" && response.Confidence >= 0.70 {
			fmt.Printf("  ✅ MATCHED: %s -> database updated\n", response.PhelpsCode)

			err := updateWritingPhelps(response.PhelpsCode, writing.Language, writing.Version)
			if err != nil {
				log.Printf("Error updating database: %v", err)
				fmt.Fprintf(reportFile, "  ERROR updating database: %v\n", err)
				fmt.Printf("  ❌ Database update failed: %v\n", err)
			} else {
				fmt.Fprintf(reportFile, "  MATCHED: %s (%.1f%% confidence) -> database updated\n", response.PhelpsCode, response.Confidence*100)
			}
		} else {
			unmatchedPrayers = append(unmatchedPrayers, writing)
			if response.PhelpsCode == "UNKNOWN" {
				fmt.Printf("  ❓ UNMATCHED: %s\n", response.Reasoning)
			} else {
				fmt.Printf("  ⚠️  LOW CONFIDENCE: %s (%.1f%%) - added to unmatched list\n", response.PhelpsCode, response.Confidence*100)
			}
			fmt.Fprintf(reportFile, "  UNMATCHED: %s (%.1f%% confidence) - %s\n", response.PhelpsCode, response.Confidence*100, response.Reasoning)
		}

		fmt.Printf("  ⏳ Waiting 1 second before next prayer...\n")
		fmt.Printf("  %s\n", strings.Repeat("-", 80))
		time.Sleep(1 * time.Second)
	}

	return unmatchedPrayers, nil
}

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

		fmt.Printf("   🧠 Analyzing with LLM...")

		var response LLMResponse
		var err error

		// Use new interactive mode by default
		response, err = callLLMInteractive(*db, referenceLanguage, prompt, useGemini, len(writing.Text))
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
		}

		// Always show the final decision clearly
		fmt.Printf("\n  🎯 FINAL DECISION:\n")
		fmt.Printf("     Prayer: %s (%s)\n", writing.Name, writing.Language)
		fmt.Printf("     Result: %s\n", response.PhelpsCode)
		fmt.Printf("     Confidence: %.1f%%\n", response.Confidence*100)
		fmt.Printf("     Reasoning: %s\n", response.Reasoning)

		if verbose {
			fmt.Printf("  ✓ Analysis complete!\n")
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
	// Check if Ollama API is available
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Try to get the list of available models via API
	httpReq, err := http.NewRequestWithContext(ctx, "GET", ollamaAPIURL+"/api/tags", nil)
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}

	client := &http.Client{}
	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("ollama API not available at %s: %v", ollamaAPIURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ollama API returned status %d: %s", resp.StatusCode, string(body))
	}

	// Read and parse response to check if model exists
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	var tagsResponse OllamaTagsResponse
	if err := json.Unmarshal(responseBody, &tagsResponse); err != nil {
		return fmt.Errorf("failed to parse models response: %w", err)
	}

	// Check if the model exists in the list
	modelFound := false
	var availableModels []string
	for _, m := range tagsResponse.Models {
		availableModels = append(availableModels, m.Name)
		// Check for exact match or with :latest tag
		if m.Name == model || m.Name == model+":latest" || strings.TrimSuffix(m.Name, ":latest") == model {
			modelFound = true
			break
		}
	}

	if !modelFound {
		return fmt.Errorf("model '%s' not found in Ollama API. Available models: %v\nTry: ollama pull %s", model, availableModels, model)
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
	var showRaw = flag.Bool("show-raw", false, "Show full raw responses at the end")
	var lucky = flag.Bool("lucky", false, "Random prayer mode: process random prayers from all languages")
	var continueMode = flag.Bool("continue", false, "Auto-continue mode: process languages in priority order")
	var noPriority = flag.Bool("no-priority", false, "Disable priority language system: process smallest languages first")
	var testPrompt = flag.Bool("test-prompt", false, "Show the LLM prompt that would be generated and exit")
	var legacyMode = flag.Bool("legacy", false, "Use legacy mode with full prayer contexts in prompt (not interactive)")

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
		fmt.Printf("  %s -lucky -max=20                  # Process 20 random prayers from all languages\n", os.Args[0])
		fmt.Printf("  %s -continue -max=50               # Auto-process languages in priority order\n", os.Args[0])
		fmt.Printf("  %s -continue -no-priority          # Process smallest languages first\n", os.Args[0])
		fmt.Printf("  %s -test-prompt -language=es       # Show LLM prompt for Spanish prayers\n", os.Args[0])
		fmt.Printf("  %s -legacy -language=es            # Use legacy mode with full contexts\n", os.Args[0])
		fmt.Printf("  %s -help                           # Show this help message\n", os.Args[0])
		fmt.Printf("\nTroubleshooting:\n")
		fmt.Printf("  If Ollama fails, ensure it's installed and the model is available:\n")
		fmt.Printf("    ollama list                      # Check available models\n")
		fmt.Printf("    ollama pull %s                 # Pull required model\n", *ollamaModel)
		fmt.Printf("  If Gemini fails, install Gemini CLI or use -gemini=false\n")
		fmt.Printf("  For languages with minimal missing prayers, consider using -language=es or -language=fr\n")
		fmt.Printf("  Use -max=N to limit processing and -verbose for detailed output\n")
		fmt.Printf("  Send SIGINT (Ctrl+C) for graceful stop after current prayer (press twice to force quit)\n")
		return
	}

	// Set up signal handling for graceful stop and force quit
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for range sigChan {
			count := atomic.AddInt32(&interruptCount, 1)
			if count == 1 {
				fmt.Printf("\nReceived stop signal. Will stop after current prayer...\n")
				fmt.Printf("Press Ctrl+C again to force quit immediately.\n")
				atomic.StoreInt32(&stopRequested, 1)
			} else {
				fmt.Printf("\nForce quit requested. Exiting immediately...\n")
				os.Exit(1)
			}
		}
	}()

	// Initialize random seed for lucky mode
	rand.Seed(time.Now().UnixNano())

	// Set the Ollama model
	OllamaModel = *ollamaModel

	// Check Ollama availability
	log.Printf("Checking Ollama availability for model %s...", *ollamaModel)
	if err := checkOllama(*ollamaModel); err != nil {
		log.Printf("Ollama check failed: %v", err)
		if !*useGemini {
			log.Fatalf("Ollama is required when Gemini is disabled")
		}
		log.Printf("Will attempt to use Gemini CLI only")
	} else {
		log.Printf("Ollama is ready with model %s", *ollamaModel)
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
	fmt.Fprintf(reportFile, "Lucky Mode: %t\n", *lucky)
	fmt.Fprintf(reportFile, "Continue Mode: %t\n", *continueMode)
	fmt.Fprintf(reportFile, "No Priority Languages: %t\n", *noPriority)
	fmt.Fprintf(reportFile, "Using Gemini: %t\n", *useGemini)
	fmt.Fprintf(reportFile, "Gemini Quota Exceeded: %t\n", atomic.LoadInt32(&geminiQuotaExceeded) == 1)
	fmt.Fprintf(reportFile, "Ollama Model: %s\n", *ollamaModel)
	fmt.Fprintf(reportFile, "Interactive Mode: %t\n", *interactive)
	fmt.Fprintf(reportFile, "Max Prayers: %d\n", *maxPrayers)
	fmt.Fprintf(reportFile, "Verbose Mode: %t\n", *verbose)
	fmt.Fprintf(reportFile, "\n")

	db := GetDatabase()
	log.Println("Database loaded")
	fmt.Fprintf(reportFile, "Database loaded successfully\n")

	// Test prompt mode - show the prompt and exit
	if *testPrompt {
		targetLang := *targetLanguage
		if targetLang == "" {
			targetLang = "es" // Default to Spanish for testing
		}

		fmt.Printf("=== TESTING LLM PROMPT GENERATION ===\n\n")
		fmt.Printf("Target Language: %s\n", targetLang)
		fmt.Printf("Reference Language: %s\n", *referenceLanguage)
		fmt.Printf("Mode: %s\n", map[bool]string{true: "Legacy (all contexts)", false: "Interactive (function calls)"}[*legacyMode])

		fmt.Printf("\n%s\n", strings.Repeat("=", 80))
		fmt.Printf("GENERATED PROMPT:\n")
		fmt.Printf("%s\n\n", strings.Repeat("=", 80))

		// Generate the appropriate header/prompt
		var header string
		if *legacyMode {
			header = prepareLLMHeaderLegacy(db, targetLang, *referenceLanguage)
		} else {
			header = prepareLLMHeader(db, targetLang, *referenceLanguage)
		}
		fmt.Print(header)

		fmt.Printf("\n\nPrayer text to analyze:\n")
		fmt.Printf("[This is where the %s prayer text would go]\n", targetLang)

		fmt.Printf("\n%s\n", strings.Repeat("=", 80))
		fmt.Printf("END OF PROMPT\n")
		fmt.Printf("%s\n", strings.Repeat("=", 80))

		// Show some statistics
		phelpsContext := buildPhelpsContext(db, *referenceLanguage)
		fmt.Printf("\nSTATISTICS:\n")
		fmt.Printf("- Total Phelps codes with context: %d\n", len(phelpsContext))
		fmt.Printf("- Mode: %s\n", map[bool]string{true: "All contexts included in prompt", false: "Interactive search available"}[*legacyMode])

		if !*legacyMode {
			fmt.Printf("\nAVAILABLE SEARCH FUNCTIONS:\n")
			fmt.Printf("- SEARCH_KEYWORDS:word1,word2,word3\n")
			fmt.Printf("- SEARCH_LENGTH:min-max\n")
			fmt.Printf("- SEARCH_OPENING:text\n")
			fmt.Printf("- GET_FULL_TEXT:phelps_code\n")
			fmt.Printf("- GET_PARTIAL_TEXT:phelps_code,range\n")
			fmt.Printf("- FINAL_ANSWER:phelps_code,confidence,reasoning\n")
			fmt.Printf("- GET_STATS\n")
		} else {
			// Show a few examples of the context for legacy mode
			fmt.Printf("\nSAMPLE CONTEXTS (first 3):\n")
			count := 0
			for phelps, context := range phelpsContext {
				if count >= 3 {
					break
				}
				fmt.Printf("\n%d. %s:\n   %s\n", count+1, phelps, context)
				count++
			}

			if len(phelpsContext) > 3 {
				fmt.Printf("\n... and %d more contexts\n", len(phelpsContext)-3)
			}
		}

		return
	}

	// Validate mode flags
	modeCount := 0
	if *lucky {
		modeCount++
	}
	if *continueMode {
		modeCount++
	}
	if *targetLanguage != "" {
		modeCount++
	}

	if modeCount > 1 {
		log.Fatalf("Error: Cannot use multiple modes simultaneously. Choose one: -language=X, -lucky, or -continue")
	}

	// Auto-select optimal language if no mode specified
	if !*lucky && !*continueMode && *targetLanguage == "" {
		*targetLanguage = findOptimalDefaultLanguage(db, *noPriority)
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

	// Process prayers based on selected mode
	var unmatchedPrayers []Writing
	var processErr error

	if *lucky {
		unmatchedPrayers, processErr = processRandomPrayersWithMode(&db, *referenceLanguage, *useGemini, reportFile, *maxPrayers, *verbose, *legacyMode)
	} else if *continueMode {
		unmatchedPrayers, processErr = processLanguagesContinuouslyWithMode(&db, *referenceLanguage, *useGemini, reportFile, *maxPrayers, *verbose, *noPriority, *legacyMode)
	} else {
		// Single language mode (traditional)
		unmatchedPrayers, processErr = processPrayersForLanguageWithMode(&db, *targetLanguage, *referenceLanguage, *useGemini, reportFile, *maxPrayers, *verbose, *legacyMode)
	}

	if processErr != nil {
		log.Fatalf("Error processing prayers: %v", processErr)
	}

	// Interactive assignment for unmatched prayers
	if *interactive && len(unmatchedPrayers) > 0 {
		interactiveAssignment(&db, unmatchedPrayers, reportFile)
	} else if len(unmatchedPrayers) > 0 {
		fmt.Printf("Found %d unmatched prayers. Run with -interactive=true to assign them manually.\n", len(unmatchedPrayers))
		fmt.Fprintf(reportFile, "Found %d unmatched prayers (interactive mode disabled)\n", len(unmatchedPrayers))
	}

	// Final status report
	fmt.Fprintf(reportFile, "\n=== FINAL STATUS ===\n")
	fmt.Fprintf(reportFile, "Completed: %s\n", time.Now().Format(time.RFC3339))
	if atomic.LoadInt32(&geminiQuotaExceeded) == 1 {
		fmt.Fprintf(reportFile, "Gemini quota was exceeded during processing - continued with Ollama only\n")
		log.Printf("Prayer matching completed with Gemini quota exceeded - used Ollama fallback. Report written to: %s", *reportPath)
	} else {
		log.Printf("Prayer matching completed. Report written to: %s", *reportPath)
	}

	// Show raw responses if requested
	if *showRaw {
		showStoredRawResponses()
	}
}
