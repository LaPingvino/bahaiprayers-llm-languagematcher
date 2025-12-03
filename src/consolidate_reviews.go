//go:build consolidate
// +build consolidate

package main

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type ReviewEntry struct {
	Language        string
	TargetPrayerID  string
	SuggestedPhelps string
	MatchType       string
	Confidence      string
	Reasons         string
	Ambiguity       string
	SourceFile      string
}

type PrayerText struct {
	Text     string
	Language string
	Name     string
	Phelps   string
}

type PrayerWithCandidates struct {
	UUID       string
	Language   string
	Prayer     *PrayerText
	Candidates []Candidate
}

type Candidate struct {
	Phelps     string
	Confidence string
	MatchType  string
	EnglishRef *PrayerText
}

var (
	prayerCache = make(map[string]*PrayerText)
	phelpsCache = make(map[string]*PrayerText)
)

func main() {
	// Parse all review files
	entries, err := parseAllReviewFiles()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing review files: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Found %d total entries\n", len(entries))

	// Filter out entries where language+Phelps combination already exists
	fmt.Println("Checking which language+Phelps combinations already exist...")
	filtered, err := filterEntriesWithoutPhelps(entries)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error filtering entries: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("After filtering out existing combinations: %d entries\n", len(filtered))

	// Group by language+UUID and collect all candidate Phelps codes
	grouped := groupByCandidates(filtered)
	fmt.Printf("Grouped into %d unique prayers\n", len(grouped))

	// Sort by language
	sort.Slice(grouped, func(i, j int) bool {
		if grouped[i].Language != grouped[j].Language {
			return grouped[i].Language < grouped[j].Language
		}
		return grouped[i].UUID < grouped[j].UUID
	})

	// Pre-load all prayers in batches
	fmt.Println("Pre-loading prayer texts...")
	err = preloadPrayers(grouped)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: error pre-loading prayers: %v\n", err)
	}

	// Generate consolidated report
	err = generateConsolidatedReport(grouped)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error generating report: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Consolidated report generated successfully")
}

func parseAllReviewFiles() ([]ReviewEntry, error) {
	var entries []ReviewEntry

	// Find all review files
	ambiguousFiles, err := filepath.Glob("review_ambiguous_*.txt")
	if err != nil {
		return nil, err
	}

	lowConfFiles, err := filepath.Glob("review_low_confidence_*.txt")
	if err != nil {
		return nil, err
	}

	allFiles := append(ambiguousFiles, lowConfFiles...)

	for _, filename := range allFiles {
		fileEntries, err := parseReviewFile(filename)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: error parsing %s: %v\n", filename, err)
			continue
		}
		entries = append(entries, fileEntries...)
	}

	return entries, nil
}

func parseReviewFile(filename string) ([]ReviewEntry, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var entries []ReviewEntry
	scanner := bufio.NewScanner(file)

	// Extract language from filename
	langMatch := regexp.MustCompile(`review_(?:ambiguous|low_confidence)_([a-z]{2,3})_`).FindStringSubmatch(filename)
	if len(langMatch) < 2 {
		return nil, fmt.Errorf("could not extract language from filename: %s", filename)
	}
	lang := langMatch[1]

	var currentEntry *ReviewEntry

	for scanner.Scan() {
		line := scanner.Text()

		if strings.Contains(line, "Target Prayer:") {
			if currentEntry != nil {
				entries = append(entries, *currentEntry)
			}
			currentEntry = &ReviewEntry{
				Language:   lang,
				SourceFile: filename,
			}
			idx := strings.Index(line, "Target Prayer:")
			if idx >= 0 {
				uuidPart := line[idx+len("Target Prayer:"):]
				currentEntry.TargetPrayerID = strings.TrimSpace(uuidPart)
			}
		} else if currentEntry != nil {
			if strings.HasPrefix(line, "Suggested Phelps:") {
				parts := strings.Split(line, ":")
				if len(parts) >= 2 {
					currentEntry.SuggestedPhelps = strings.TrimSpace(parts[1])
				}
			} else if strings.HasPrefix(line, "Match Type:") {
				parts := strings.Split(line, ":")
				if len(parts) >= 2 {
					currentEntry.MatchType = strings.TrimSpace(parts[1])
				}
			} else if strings.HasPrefix(line, "Confidence:") {
				parts := strings.Split(line, ":")
				if len(parts) >= 2 {
					currentEntry.Confidence = strings.TrimSpace(parts[1])
				}
			} else if strings.HasPrefix(line, "Reasons:") {
				parts := strings.Split(line, ":")
				if len(parts) >= 2 {
					currentEntry.Reasons = strings.TrimSpace(parts[1])
				}
			} else if strings.HasPrefix(line, "Ambiguity:") {
				parts := strings.Split(line, ":")
				if len(parts) >= 2 {
					currentEntry.Ambiguity = strings.TrimSpace(parts[1])
				}
			}
		}
	}

	if currentEntry != nil {
		entries = append(entries, *currentEntry)
	}

	return entries, scanner.Err()
}

func filterEntriesWithoutPhelps(entries []ReviewEntry) ([]ReviewEntry, error) {
	langPhelpsExists := make(map[string]bool)

	type LangPhelps struct {
		lang   string
		phelps string
	}
	checkSet := make(map[LangPhelps]bool)

	for _, entry := range entries {
		if entry.SuggestedPhelps != "" && entry.SuggestedPhelps != entry.TargetPrayerID {
			checkSet[LangPhelps{entry.Language, entry.SuggestedPhelps}] = true
		}
	}

	var checks []LangPhelps
	for lp := range checkSet {
		checks = append(checks, lp)
	}

	fmt.Printf("Checking %d language+Phelps combinations...\n", len(checks))

	batchSize := 100
	for i := 0; i < len(checks); i += batchSize {
		end := i + batchSize
		if end > len(checks) {
			end = len(checks)
		}
		batch := checks[i:end]

		var conditions []string
		for _, lp := range batch {
			conditions = append(conditions,
				fmt.Sprintf("(language = '%s' AND phelps = '%s')", lp.lang, lp.phelps))
		}
		whereClause := strings.Join(conditions, " OR ")

		query := fmt.Sprintf("SELECT language, phelps FROM writings WHERE %s", whereClause)

		cmd := exec.Command("dolt", "sql", "-r", "csv", "-q", query)

		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = os.Stderr

		err := cmd.Run()
		if err != nil {
			return nil, fmt.Errorf("error checking language+Phelps combinations: %v", err)
		}

		reader := csv.NewReader(&out)
		records, err := reader.ReadAll()
		if err != nil {
			return nil, err
		}

		for j := 1; j < len(records); j++ {
			if len(records[j]) >= 2 {
				key := records[j][0] + ":" + records[j][1]
				langPhelpsExists[key] = true
			}
		}

		if (i+len(batch))%500 == 0 || i+len(batch) == len(checks) {
			fmt.Printf("Checked %d/%d combinations...\n", i+len(batch), len(checks))
		}
	}

	var filtered []ReviewEntry
	phelpsPattern := regexp.MustCompile(`^[A-Z]{2,3}\d{5}`)

	for _, entry := range entries {
		if strings.ToUpper(entry.SuggestedPhelps) == strings.ToUpper(entry.TargetPrayerID) {
			continue
		}

		if !phelpsPattern.MatchString(entry.SuggestedPhelps) && entry.SuggestedPhelps != "" {
			if !strings.HasPrefix(entry.SuggestedPhelps, "BH") && !strings.HasPrefix(entry.SuggestedPhelps, "AB") {
				continue
			}
		}

		key := entry.Language + ":" + entry.SuggestedPhelps
		if langPhelpsExists[key] {
			continue
		}

		filtered = append(filtered, entry)
	}

	return filtered, nil
}

func groupByCandidates(entries []ReviewEntry) []PrayerWithCandidates {
	grouped := make(map[string]*PrayerWithCandidates)

	for _, entry := range entries {
		key := entry.Language + ":" + entry.TargetPrayerID

		if _, exists := grouped[key]; !exists {
			grouped[key] = &PrayerWithCandidates{
				UUID:       entry.TargetPrayerID,
				Language:   entry.Language,
				Candidates: []Candidate{},
			}
		}

		// Add this candidate if not already present
		candidate := Candidate{
			Phelps:     entry.SuggestedPhelps,
			Confidence: entry.Confidence,
			MatchType:  entry.MatchType,
		}

		// Check if we already have this Phelps code
		found := false
		for _, c := range grouped[key].Candidates {
			if c.Phelps == candidate.Phelps {
				found = true
				break
			}
		}

		if !found {
			grouped[key].Candidates = append(grouped[key].Candidates, candidate)
		}
	}

	// Convert to slice
	var result []PrayerWithCandidates
	for _, pwc := range grouped {
		result = append(result, *pwc)
	}

	return result
}

func preloadPrayers(grouped []PrayerWithCandidates) error {
	// Collect unique UUIDs and Phelps codes
	uuidSet := make(map[string]bool)
	phelpsSet := make(map[string]bool)

	for _, pwc := range grouped {
		uuidSet[pwc.UUID] = true
		for _, candidate := range pwc.Candidates {
			phelpsSet[candidate.Phelps] = true
		}
	}

	var uuids []string
	for uuid := range uuidSet {
		uuids = append(uuids, uuid)
	}

	var phelpsIDs []string
	for phelps := range phelpsSet {
		phelpsIDs = append(phelpsIDs, phelps)
	}

	fmt.Printf("Pre-loading %d unique prayers and %d unique references...\n", len(uuids), len(phelpsIDs))

	batchSize := 500
	for i := 0; i < len(uuids); i += batchSize {
		end := i + batchSize
		if end > len(uuids) {
			end = len(uuids)
		}
		batch := uuids[i:end]

		if err := loadPrayerBatch(batch); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: error loading prayer batch: %v\n", err)
		}
		fmt.Printf("Loaded prayers %d-%d/%d\n", i+1, end, len(uuids))
	}

	for i := 0; i < len(phelpsIDs); i += batchSize {
		end := i + batchSize
		if end > len(phelpsIDs) {
			end = len(phelpsIDs)
		}
		batch := phelpsIDs[i:end]

		if err := loadPhelpsReferencesBatch(batch); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: error loading Phelps batch: %v\n", err)
		}
		fmt.Printf("Loaded references %d-%d/%d\n", i+1, end, len(phelpsIDs))
	}

	return nil
}

func loadPrayerBatch(uuids []string) error {
	if len(uuids) == 0 {
		return nil
	}

	quotedUUIDs := make([]string, len(uuids))
	for i, uuid := range uuids {
		quotedUUIDs[i] = fmt.Sprintf("'%s'", uuid)
	}
	inClause := strings.Join(quotedUUIDs, ",")

	query := fmt.Sprintf("SELECT version, text, language, name, phelps FROM writings WHERE version IN (%s)", inClause)

	cmd := exec.Command("dolt", "sql", "-r", "csv", "-q", query)

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err != nil {
		return err
	}

	reader := csv.NewReader(&out)
	records, err := reader.ReadAll()
	if err != nil {
		return err
	}

	for i := 1; i < len(records); i++ {
		if len(records[i]) >= 5 {
			version := records[i][0]
			prayerCache[version] = &PrayerText{
				Text:     records[i][1],
				Language: records[i][2],
				Name:     records[i][3],
				Phelps:   records[i][4],
			}
		}
	}

	return nil
}

func loadPhelpsReferencesBatch(phelpsIDs []string) error {
	if len(phelpsIDs) == 0 {
		return nil
	}

	quotedPhelps := make([]string, len(phelpsIDs))
	for i, phelps := range phelpsIDs {
		quotedPhelps[i] = fmt.Sprintf("'%s'", phelps)
	}
	inClause := strings.Join(quotedPhelps, ",")

	query := fmt.Sprintf("SELECT phelps, text, language, name FROM writings WHERE phelps IN (%s) AND language = 'en'", inClause)

	cmd := exec.Command("dolt", "sql", "-r", "csv", "-q", query)

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err != nil {
		return err
	}

	reader := csv.NewReader(&out)
	records, err := reader.ReadAll()
	if err != nil {
		return err
	}

	for i := 1; i < len(records); i++ {
		if len(records[i]) >= 4 {
			phelps := records[i][0]
			phelpsCache[phelps] = &PrayerText{
				Text:     records[i][1],
				Language: records[i][2],
				Name:     records[i][3],
				Phelps:   phelps,
			}
		}
	}

	return nil
}

func getFirstLastLines(text string) (string, string) {
	if text == "" {
		return "", ""
	}

	lines := strings.Split(text, "\n")

	// Get first non-empty line
	var first string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			first = trimmed
			break
		}
	}

	// Get last non-empty line
	var last string
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			last = trimmed
			break
		}
	}

	return first, last
}

func generateConsolidatedReport(grouped []PrayerWithCandidates) error {
	filename := fmt.Sprintf("consolidated_review_%s.txt", time.Now().Format("20060102_150405"))
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	w := bufio.NewWriter(file)
	defer w.Flush()

	// Write header
	fmt.Fprintf(w, "CONSOLIDATED REVIEW - PRAYERS NEEDING PHELPS CODE ASSIGNMENT\n")
	fmt.Fprintf(w, "Generated: %s\n", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(w, "Total prayers: %d\n\n", len(grouped))

	fmt.Fprintf(w, "INSTRUCTIONS:\n")
	fmt.Fprintf(w, "- Each prayer shows the FULL text in the target language\n")
	fmt.Fprintf(w, "- Below each prayer is a list of candidate Phelps codes\n")
	fmt.Fprintf(w, "- For each candidate, you see the first and last line of the English reference\n")
	fmt.Fprintf(w, "- Pick the best matching Phelps code for each prayer\n")
	fmt.Fprintf(w, "- Write your selection in the 'Selected:' field\n\n")

	currentLang := ""
	itemNum := 1

	fmt.Println("Generating report...")
	for i, pwc := range grouped {
		if pwc.Language != currentLang {
			if currentLang != "" {
				fmt.Fprintf(w, "\n")
			}
			currentLang = pwc.Language
			fmt.Fprintf(w, "═══════════════════════════════════════════════════════════════\n")
			fmt.Fprintf(w, "LANGUAGE: %s\n", strings.ToUpper(currentLang))
			fmt.Fprintf(w, "═══════════════════════════════════════════════════════════════\n\n")
		}

		fmt.Fprintf(w, "───────────────────────────────────────────────────────────────\n")
		fmt.Fprintf(w, "[%d] Prayer UUID: %s\n", itemNum, pwc.UUID)

		// Get target prayer text
		targetPrayer, _ := prayerCache[pwc.UUID]
		if targetPrayer != nil && targetPrayer.Text != "" {
			if targetPrayer.Name != "" {
				fmt.Fprintf(w, "Name: %s\n", targetPrayer.Name)
			}
			fmt.Fprintf(w, "\nFULL TEXT (%s):\n", pwc.Language)
			fmt.Fprintf(w, "%s\n\n", targetPrayer.Text)
		} else {
			fmt.Fprintf(w, "\nFULL TEXT: [Not found in database]\n\n")
		}

		// List all candidate Phelps codes
		fmt.Fprintf(w, "CANDIDATE PHELPS CODES (%d options):\n", len(pwc.Candidates))
		for j, candidate := range pwc.Candidates {
			fmt.Fprintf(w, "\n  [%d] %s (Confidence: %s, Type: %s)\n", j+1, candidate.Phelps, candidate.Confidence, candidate.MatchType)

			refPrayer, _ := phelpsCache[candidate.Phelps]
			if refPrayer != nil && refPrayer.Text != "" {
				first, last := getFirstLastLines(refPrayer.Text)
				if refPrayer.Name != "" {
					fmt.Fprintf(w, "      Name: %s\n", refPrayer.Name)
				}
				if first != "" {
					fmt.Fprintf(w, "      First: %s\n", first)
				}
				if last != "" && last != first {
					fmt.Fprintf(w, "      Last:  %s\n", last)
				}
			} else {
				fmt.Fprintf(w, "      [English reference not found]\n")
			}
		}

		fmt.Fprintf(w, "\nSelected: ________________\n")
		fmt.Fprintf(w, "Notes: ___________________________________________________\n")
		fmt.Fprintf(w, "\n")

		itemNum++

		if (i+1)%100 == 0 {
			fmt.Printf("Generated %d/%d prayers...\n", i+1, len(grouped))
		}
	}

	fmt.Fprintf(w, "═══════════════════════════════════════════════════════════════\n")
	fmt.Fprintf(w, "END OF REPORT\n")

	return nil
}
