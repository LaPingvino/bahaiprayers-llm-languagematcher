//go:build generate
// +build generate

package main

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type Prayer struct {
	UUID       string
	Language   string
	Text       string
	Name       string
	Candidates []Candidate
}

type Candidate struct {
	Phelps     string
	Confidence float64
	MatchType  string
	FirstLine  string
	LastLine   string
	Name       string
}

func main() {
	// Read the consolidated review file
	prayers, err := parseConsolidatedReport("consolidated_review_20251129_003240.txt")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing report: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Parsed %d prayers\n", len(prayers))

	// Filter out Persian (FA) prayers - special case to handle with English later
	filteredPrayers := make([]Prayer, 0, len(prayers))
	skippedPersian := 0
	for _, p := range prayers {
		if strings.ToUpper(p.Language) == "FA" {
			skippedPersian++
			continue
		}
		filteredPrayers = append(filteredPrayers, p)
	}
	prayers = filteredPrayers
	fmt.Printf("Filtered out %d Persian prayers (special case)\n", skippedPersian)
	fmt.Printf("Processing %d prayers from other languages\n", len(prayers))

	// Generate SQL updates
	sqlFile, err := os.Create("phelps_code_updates.sql")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating SQL file: %v\n", err)
		os.Exit(1)
	}
	defer sqlFile.Close()

	w := bufio.NewWriter(sqlFile)
	defer w.Flush()

	// Write header
	fmt.Fprintf(w, "-- Phelps Code Assignment Updates\n")
	fmt.Fprintf(w, "-- Generated: %s\n", "2025-11-28")
	fmt.Fprintf(w, "-- Total prayers: %d\n\n", len(prayers))

	updated := 0
	skipped := 0

	for _, prayer := range prayers {
		if prayer.UUID == "" || len(prayer.Candidates) == 0 {
			skipped++
			continue
		}

		// Select best candidate
		bestCandidate := selectBestCandidate(prayer)
		if bestCandidate == nil {
			skipped++
			continue
		}

		// Generate comment with context
		fmt.Fprintf(w, "-- Language: %s\n", strings.ToUpper(prayer.Language))
		if prayer.Name != "" {
			fmt.Fprintf(w, "-- Prayer name: %s\n", prayer.Name)
		}
		if prayer.Text != "" {
			firstLine := getFirstLine(prayer.Text)
			if firstLine != "" {
				fmt.Fprintf(w, "-- Starts with: %s\n", truncate(firstLine, 80))
			}
		}
		if bestCandidate.Name != "" {
			fmt.Fprintf(w, "-- English: %s\n", bestCandidate.Name)
		}
		if bestCandidate.FirstLine != "" {
			fmt.Fprintf(w, "-- EN starts: %s\n", truncate(bestCandidate.FirstLine, 80))
		}
		fmt.Fprintf(w, "-- Confidence: %.0f%%, Type: %s, Candidates: %d\n",
			bestCandidate.Confidence, bestCandidate.MatchType, len(prayer.Candidates))

		// Generate UPDATE statement
		fmt.Fprintf(w, "UPDATE writings SET phelps = '%s' WHERE version = '%s';\n\n",
			bestCandidate.Phelps, prayer.UUID)

		updated++
	}

	// Write summary
	fmt.Fprintf(w, "-- Summary:\n")
	fmt.Fprintf(w, "-- Total prayers: %d\n", len(prayers))
	fmt.Fprintf(w, "-- Updates generated: %d\n", updated)
	fmt.Fprintf(w, "-- Skipped: %d\n", skipped)

	fmt.Printf("\nSQL file generated: phelps_code_updates.sql\n")
	fmt.Printf("Updates generated: %d\n", updated)
	fmt.Printf("Skipped: %d\n", skipped)
}

func parseConsolidatedReport(filename string) ([]Prayer, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var prayers []Prayer
	var currentPrayer *Prayer
	var currentLanguage string

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024) // 10MB buffer for large lines

	inFullText := false
	inCandidates := false
	var textLines []string
	candidateNum := 0

	for scanner.Scan() {
		line := scanner.Text()

		// Language section
		if strings.HasPrefix(line, "LANGUAGE: ") {
			currentLanguage = strings.TrimSpace(strings.TrimPrefix(line, "LANGUAGE:"))
			continue
		}

		// New prayer entry
		if strings.HasPrefix(line, "[") && strings.Contains(line, "] Prayer UUID:") {
			// Save previous prayer
			if currentPrayer != nil && currentPrayer.UUID != "" {
				prayers = append(prayers, *currentPrayer)
			}

			// Start new prayer
			currentPrayer = &Prayer{
				Language:   currentLanguage,
				Candidates: []Candidate{},
			}
			textLines = []string{}
			inFullText = false
			inCandidates = false

			// Extract UUID
			parts := strings.Split(line, "Prayer UUID:")
			if len(parts) >= 2 {
				currentPrayer.UUID = strings.TrimSpace(parts[1])
			}
			continue
		}

		if currentPrayer == nil {
			continue
		}

		// Prayer name
		if strings.HasPrefix(line, "Name: ") && !inCandidates {
			currentPrayer.Name = strings.TrimSpace(strings.TrimPrefix(line, "Name:"))
			continue
		}

		// Full text section
		if strings.Contains(line, "FULL TEXT (") {
			inFullText = true
			textLines = []string{}
			continue
		}

		// Candidate section starts
		if strings.HasPrefix(line, "CANDIDATE PHELPS CODES") {
			if len(textLines) > 0 {
				currentPrayer.Text = strings.Join(textLines, "\n")
			}
			inFullText = false
			inCandidates = true
			candidateNum = 0
			continue
		}

		// Individual candidate
		if inCandidates && strings.HasPrefix(strings.TrimSpace(line), "[") && strings.Contains(line, "]") {
			// Parse candidate: [1] BH12345 (Confidence: 85%, Type: AMBIGUOUS)
			// Also handle format: (Confidence: %!d(float64=60)%, Type: AMBIGUOUS)
			candidateNum++

			re := regexp.MustCompile(`\[(\d+)\]\s+([A-Z0-9]+)\s+\(Confidence:\s+([^,]+),\s+Type:\s+([^)]+)\)`)
			matches := re.FindStringSubmatch(line)

			if len(matches) >= 5 {
				confStr := strings.TrimSpace(matches[3])
				// Handle %!d(float64=60)% format
				if strings.Contains(confStr, "float64=") {
					confStr = strings.TrimPrefix(confStr, "%!d(float64=")
					confStr = strings.TrimSuffix(confStr, ")%")
				} else {
					confStr = strings.TrimSuffix(confStr, "%")
				}
				conf, _ := strconv.ParseFloat(confStr, 64)
				candidate := Candidate{
					Phelps:     strings.TrimSpace(matches[2]),
					Confidence: conf,
					MatchType:  strings.TrimSpace(matches[4]),
				}
				currentPrayer.Candidates = append(currentPrayer.Candidates, candidate)
			}
			continue
		}

		// Candidate details (Name, First, Last)
		if inCandidates && len(currentPrayer.Candidates) > 0 {
			lastIdx := len(currentPrayer.Candidates) - 1

			if strings.HasPrefix(strings.TrimSpace(line), "Name: ") {
				currentPrayer.Candidates[lastIdx].Name = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "Name:"))
			} else if strings.HasPrefix(strings.TrimSpace(line), "First: ") {
				currentPrayer.Candidates[lastIdx].FirstLine = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "First:"))
			} else if strings.HasPrefix(strings.TrimSpace(line), "Last: ") {
				currentPrayer.Candidates[lastIdx].LastLine = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "Last:"))
			}
			continue
		}

		// Selected/Notes section - end of prayer
		if strings.HasPrefix(line, "Selected:") {
			inCandidates = false
			continue
		}

		// Collect full text lines
		if inFullText && line != "" && !strings.HasPrefix(line, "CANDIDATE") {
			textLines = append(textLines, line)
		}
	}

	// Don't forget last prayer
	if currentPrayer != nil && currentPrayer.UUID != "" {
		prayers = append(prayers, *currentPrayer)
	}

	return prayers, scanner.Err()
}

func selectBestCandidate(prayer Prayer) *Candidate {
	if len(prayer.Candidates) == 0 {
		return nil
	}

	// Sort candidates by confidence (highest first)
	candidates := make([]Candidate, len(prayer.Candidates))
	copy(candidates, prayer.Candidates)

	sort.Slice(candidates, func(i, j int) bool {
		// Prefer higher confidence
		if candidates[i].Confidence != candidates[j].Confidence {
			return candidates[i].Confidence > candidates[j].Confidence
		}
		// Prefer non-ambiguous if same confidence
		if candidates[i].MatchType != candidates[j].MatchType {
			if candidates[i].MatchType == "SEMANTIC_MATCH" {
				return true
			}
			if candidates[j].MatchType == "SEMANTIC_MATCH" {
				return false
			}
		}
		return false
	})

	// Return highest confidence candidate with at least 40% confidence
	if candidates[0].Confidence >= 40 {
		return &candidates[0]
	}

	return nil
}

func getFirstLine(text string) string {
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			return trimmed
		}
	}
	return ""
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
