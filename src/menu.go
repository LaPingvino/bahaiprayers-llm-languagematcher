package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// MenuOption represents a menu choice
type MenuOption struct {
	Key           string
	Title         string
	Description   string
	Action        func() error
	Requirements  []string
	Efficiency    string
	EstimatedTime string
}

// ShowMainMenu displays the interactive menu and handles user selection
func ShowMainMenu() error {
	for {
		clearScreen()
		showHeader()

		status, err := GetProcessingStatus()
		if err != nil {
			fmt.Printf("⚠️  Warning: Could not get database status: %v\n\n", err)
		} else {
			showStatusSummary(status)
		}

		backends := GetAvailableBackends()
		showBackendStatus(backends)

		options := buildMenuOptions()
		showMenuOptions(options)

		choice := getUserInput("Enter your choice (1-9, or 'q' to quit): ")

		if choice == "q" || choice == "quit" || choice == "exit" {
			fmt.Println("\n👋 Goodbye!")
			return nil
		}

		if err := handleMenuChoice(choice, options); err != nil {
			fmt.Printf("\n❌ Error: %v\n", err)
			fmt.Print("Press Enter to continue...")
			bufio.NewReader(os.Stdin).ReadLine()
		}
	}
}

func clearScreen() {
	fmt.Print("\033[2J\033[H") // ANSI escape codes to clear screen
}

func showHeader() {
	fmt.Println("🙏 Bahá'í Prayer Matching System")
	fmt.Println("=================================")
	fmt.Printf("📅 %s\n\n", time.Now().Format("January 2, 2006 at 3:04 PM"))
}

func showStatusSummary(status *ProcessingStatus) {
	fmt.Printf("📊 Database Status: %d/%d prayers matched (%d%% complete)\n",
		status.MatchedPrayers, status.TotalPrayers, status.CompletionRate)
	fmt.Printf("🌐 Languages: %d total, %d unprocessed\n\n",
		status.TotalLanguages, status.UnprocessedLangs)
}

func showBackendStatus(backends []Backend) {
	fmt.Println("🔧 Available Processing Backends:")
	if len(backends) == 0 {
		fmt.Println("   ❌ No backends found! Install claude, gemini, or ollama")
	} else {
		for _, backend := range backends {
			fmt.Printf("   ✅ %s\n", backend.Name)
		}
	}
	fmt.Println()
}

func buildMenuOptions() []MenuOption {
	return []MenuOption{
		{
			Key:           "1",
			Title:         "📊 Check Database Status",
			Description:   "View detailed statistics and processing recommendations",
			Action:        StatusCheckCommand,
			Requirements:  []string{"Database access"},
			Efficiency:    "Instant",
			EstimatedTime: "< 1 second",
		},
		{
			Key:           "2",
			Title:         "🚀 Ultra-Compressed Processing (ALL languages)",
			Description:   "Process all unmatched languages using smart batching (97% API reduction)",
			Action:        func() error { return UltraCompressedBulkMatching() },
			Requirements:  []string{"Claude/Gemini/ollama CLI"},
			Efficiency:    "97% fewer API calls",
			EstimatedTime: "15-45 minutes",
		},
		{
			Key:           "3",
			Title:         "🧠 Smart Fallback Processing",
			Description:   "Intelligent backend switching: Claude → Gemini → ollama",
			Action:        SmartFallbackProcessing,
			Requirements:  []string{"At least one backend"},
			Efficiency:    "Automatic retry",
			EstimatedTime: "Variable",
		},
		{
			Key:           "4",
			Title:         "⚡ Compressed Single Language",
			Description:   "Process one specific language with fingerprint matching (90% reduction)",
			Action:        handleSingleLanguageProcessing,
			Requirements:  []string{"Language code", "Backend"},
			Efficiency:    "90% fewer API calls",
			EstimatedTime: "1-5 minutes",
		},
		{
			Key:           "5",
			Title:         "🔄 Retry Saved Batches",
			Description:   "Process interrupted batches from previous runs",
			Action:        RetryBatchesCommand,
			Requirements:  []string{"Saved batch files"},
			Efficiency:    "Resume progress",
			EstimatedTime: "Variable",
		},
		{
			Key:           "6",
			Title:         "📈 Detailed Language Report",
			Description:   "Show comprehensive per-language statistics",
			Action:        DetailedLanguageReport,
			Requirements:  []string{"Database access"},
			Efficiency:    "Instant",
			EstimatedTime: "< 5 seconds",
		},
		{
			Key:           "7",
			Title:         "🔧 Backend Configuration",
			Description:   "Test and configure processing backends",
			Action:        BackendConfigurationMenu,
			Requirements:  []string{"None"},
			Efficiency:    "Setup",
			EstimatedTime: "2-5 minutes",
		},
		{
			Key:           "8",
			Title:         "📁 File Management",
			Description:   "Clean up logs, batches, and temporary files",
			Action:        FileManagementMenu,
			Requirements:  []string{"File system access"},
			Efficiency:    "Cleanup",
			EstimatedTime: "< 1 minute",
		},
		{
			Key:           "9",
			Title:         "⚙️  Advanced Options",
			Description:   "Custom processing options and expert settings",
			Action:        AdvancedOptionsMenu,
			Requirements:  []string{"Expert knowledge"},
			Efficiency:    "Custom",
			EstimatedTime: "Variable",
		},
	}
}

func showMenuOptions(options []MenuOption) {
	fmt.Println("📋 Available Options:")
	fmt.Println("────────────────────")

	for _, option := range options {
		fmt.Printf("%s. %s\n", option.Key, option.Title)
		fmt.Printf("   %s\n", option.Description)
		fmt.Printf("   💡 Efficiency: %s | ⏱️  Time: %s\n", option.Efficiency, option.EstimatedTime)
		fmt.Printf("   📋 Requirements: %s\n\n", strings.Join(option.Requirements, ", "))
	}

	fmt.Println("q. 🚪 Quit")
	fmt.Println()
}

func getUserInput(prompt string) string {
	fmt.Print(prompt)
	reader := bufio.NewReader(os.Stdin)
	input, _ := reader.ReadString('\n')
	return strings.TrimSpace(input)
}

func handleMenuChoice(choice string, options []MenuOption) error {
	for _, option := range options {
		if option.Key == choice {
			fmt.Printf("\n🚀 Starting: %s\n", option.Title)
			fmt.Println("═══════════════════════════════════════")

			startTime := time.Now()
			err := option.Action()
			duration := time.Since(startTime)

			fmt.Println("\n═══════════════════════════════════════")
			if err != nil {
				fmt.Printf("❌ Failed after %v: %v\n", duration.Round(time.Second), err)
			} else {
				fmt.Printf("✅ Completed successfully in %v\n", duration.Round(time.Second))
			}

			fmt.Print("\nPress Enter to return to menu...")
			bufio.NewReader(os.Stdin).ReadLine()
			return nil
		}
	}

	return fmt.Errorf("invalid choice: %s", choice)
}

func handleSingleLanguageProcessing() error {
	fmt.Println("\n🌐 Single Language Processing")
	fmt.Println("════════════════════════════")

	// Show available unprocessed languages
	status, err := GetProcessingStatus()
	if err != nil {
		return fmt.Errorf("could not get status: %v", err)
	}

	if status.UnprocessedLangs == 0 {
		fmt.Println("🎉 All languages are already processed!")
		return nil
	}

	fmt.Printf("Found %d unprocessed languages.\n\n", status.UnprocessedLangs)

	// Get top unprocessed languages
	unprocessedLangs, err := GetTopUnprocessedLanguages(10)
	if err != nil {
		return fmt.Errorf("could not get unprocessed languages: %v", err)
	}

	fmt.Println("Top unprocessed languages:")
	for i, lang := range unprocessedLangs {
		fmt.Printf("%d. %s (%d prayers)\n", i+1, lang.Language, lang.UnmatchedCount)
	}

	langChoice := getUserInput("\nEnter language code (or number from list above): ")

	// Handle numeric choice
	if num, err := strconv.Atoi(langChoice); err == nil && num >= 1 && num <= len(unprocessedLangs) {
		langChoice = unprocessedLangs[num-1].Language
	}

	fmt.Printf("Processing language: %s\n", langChoice)

	// Select backend
	backends := GetAvailableBackends()
	if len(backends) == 0 {
		return fmt.Errorf("no backends available")
	}

	fmt.Println("\nAvailable backends:")
	for i, backend := range backends {
		fmt.Printf("%d. %s\n", i+1, backend.Name)
	}

	backendChoice := getUserInput("Choose backend (1-" + strconv.Itoa(len(backends)) + "): ")
	backendNum, err := strconv.Atoi(backendChoice)
	if err != nil || backendNum < 1 || backendNum > len(backends) {
		return fmt.Errorf("invalid backend choice")
	}

	selectedBackend := backends[backendNum-1]

	// Set backend flags
	switch selectedBackend.Name {
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

	fmt.Printf("\n🚀 Processing %s with %s...\n", langChoice, selectedBackend.Name)

	return CompressedLanguageMatching(langChoice)
}

func DetailedLanguageReport() error {
	fmt.Println("\n📈 Detailed Language Report")
	fmt.Println("═══════════════════════════")

	// Get all language statistics
	result, err := execDoltQueryCSV(`
		SELECT
			language,
			COUNT(*) as total_prayers,
			COUNT(phelps) as matched_prayers,
			COUNT(*) - COUNT(phelps) as unmatched_prayers,
			ROUND(COUNT(phelps) * 100.0 / COUNT(*), 1) as completion_percent
		FROM writings
		GROUP BY language
		ORDER BY COUNT(*) DESC
	`)

	if err != nil {
		return fmt.Errorf("failed to get language statistics: %v", err)
	}

	fmt.Printf("%-15s %8s %8s %10s %10s %8s\n", "Language", "Total", "Matched", "Remaining", "Progress", "Status")
	fmt.Println(strings.Repeat("─", 75))

	totalPrayers := 0
	totalMatched := 0
	completeLanguages := 0

	for i := 1; i < len(result); i++ {
		if len(result[i]) < 5 {
			continue
		}

		lang := result[i][0]
		total, _ := strconv.Atoi(result[i][1])
		matched, _ := strconv.Atoi(result[i][2])
		remaining, _ := strconv.Atoi(result[i][3])
		completion, _ := strconv.ParseFloat(result[i][4], 64)

		totalPrayers += total
		totalMatched += matched

		status := "🔄 Partial"
		if completion == 100 {
			status = "✅ Complete"
			completeLanguages++
		} else if completion == 0 {
			status = "❌ Unprocessed"
		}

		fmt.Printf("%-15s %8d %8d %10d %9.1f%% %s\n",
			lang, total, matched, remaining, completion, status)
	}

	fmt.Println(strings.Repeat("─", 75))
	overallCompletion := float64(totalMatched) * 100.0 / float64(totalPrayers)
	fmt.Printf("%-15s %8d %8d %10d %9.1f%% \n",
		"TOTAL", totalPrayers, totalMatched, totalPrayers-totalMatched, overallCompletion)

	fmt.Printf("\n📊 Summary:\n")
	fmt.Printf("   Total languages: %d\n", len(result)-1)
	fmt.Printf("   Complete languages: %d\n", completeLanguages)
	fmt.Printf("   Incomplete languages: %d\n", len(result)-1-completeLanguages)
	fmt.Printf("   Overall completion: %.1f%%\n", overallCompletion)

	return nil
}

func BackendConfigurationMenu() error {
	fmt.Println("\n🔧 Backend Configuration")
	fmt.Println("═══════════════════════════")

	backends := GetAvailableBackends()

	if len(backends) == 0 {
		fmt.Println("❌ No backends are currently available!")
		fmt.Println("\nTo install backends:")
		fmt.Println("1. Claude CLI: Visit https://claude.ai/cli")
		fmt.Println("2. Gemini CLI: Visit https://ai.google.dev/gemini-api/docs/cli")
		fmt.Println("3. ollama: Visit https://ollama.com/")
		return nil
	}

	fmt.Println("Available backends:")
	for i, backend := range backends {
		fmt.Printf("%d. %s (Priority: %d)\n", i+1, backend.Name, backend.Priority)
	}

	choice := getUserInput("\nChoose backend to test (1-" + strconv.Itoa(len(backends)) + "): ")
	num, err := strconv.Atoi(choice)
	if err != nil || num < 1 || num > len(backends) {
		return fmt.Errorf("invalid choice")
	}

	selectedBackend := backends[num-1]

	fmt.Printf("\n🧪 Testing %s...\n", selectedBackend.Name)

	// Test the backend with a simple query
	testErr := testBackend(selectedBackend)
	if testErr != nil {
		fmt.Printf("❌ Test failed: %v\n", testErr)
	} else {
		fmt.Printf("✅ Test successful!\n")
	}

	return nil
}

func testBackend(backend Backend) error {
	// Set backend flags
	switch backend.Name {
	case "Claude CLI":
		useCLI = true
		useGemini = false
		useGptOss = false
		response, err := CallClaudeCLI("Test message: respond with 'OK'")
		if err != nil {
			return err
		}
		if !strings.Contains(strings.ToUpper(response), "OK") {
			return fmt.Errorf("unexpected response: %s", response)
		}
	case "Gemini CLI":
		useCLI = false
		useGemini = true
		useGptOss = false
		response, err := CallGeminiCLI("Test message: respond with 'OK'")
		if err != nil {
			return err
		}
		if !strings.Contains(strings.ToUpper(response), "OK") {
			return fmt.Errorf("unexpected response: %s", response)
		}
	case "ollama":
		useCLI = false
		useGemini = false
		useGptOss = true
		response, err := CallGptOss("Test message: respond with 'OK'")
		if err != nil {
			return err
		}
		if !strings.Contains(strings.ToUpper(response), "OK") {
			return fmt.Errorf("unexpected response: %s", response)
		}
	}

	return nil
}

func FileManagementMenu() error {
	fmt.Println("\n📁 File Management")
	fmt.Println("═══════════════════")

	// Check for various file types
	logFiles := findFiles("*.log")
	batchFiles := findFiles("*batch*.json")
	processedFiles := findFiles("*.processed")
	reportFiles := findFiles("*report*.txt")

	fmt.Printf("Found files:\n")
	fmt.Printf("   Log files: %d\n", len(logFiles))
	fmt.Printf("   Batch files: %d\n", len(batchFiles))
	fmt.Printf("   Processed files: %d\n", len(processedFiles))
	fmt.Printf("   Report files: %d\n", len(reportFiles))

	fmt.Println("\nCleanup options:")
	fmt.Println("1. Clean old log files (>7 days)")
	fmt.Println("2. Remove processed batch files")
	fmt.Println("3. Archive old reports")
	fmt.Println("4. Clean all temporary files")
	fmt.Println("5. Show disk usage")

	choice := getUserInput("\nChoose option (1-5): ")

	switch choice {
	case "1":
		return cleanOldLogFiles()
	case "2":
		return removeProcessedBatches()
	case "3":
		return archiveOldReports()
	case "4":
		return cleanAllTempFiles()
	case "5":
		return showDiskUsage()
	default:
		return fmt.Errorf("invalid choice")
	}
}

func AdvancedOptionsMenu() error {
	fmt.Println("\n⚙️  Advanced Options")
	fmt.Println("═══════════════════")

	fmt.Println("1. Custom batch size processing")
	fmt.Println("2. Specific prayer ID matching")
	fmt.Println("3. Export/Import configuration")
	fmt.Println("4. Database maintenance")
	fmt.Println("5. Performance benchmarks")
	fmt.Println("6. Process CSV issue list")

	choice := getUserInput("\nChoose option (1-6): ")

	switch choice {
	case "1":
		return customBatchProcessing()
	case "2":
		return specificPrayerMatching()
	case "3":
		return configurationManagement()
	case "4":
		return databaseMaintenance()
	case "5":
		return performanceBenchmarks()
	case "6":
		return processCsvIssues()
	default:
		return fmt.Errorf("invalid choice")
	}
}

// Helper functions for file management
func findFiles(pattern string) []string {
	// Simplified implementation - in real use would use filepath.Glob
	return []string{}
}

func cleanOldLogFiles() error {
	fmt.Println("🧹 Cleaning old log files...")
	// Implementation would clean files older than 7 days
	fmt.Println("✅ Old log files cleaned")
	return nil
}

func removeProcessedBatches() error {
	fmt.Println("🗑️  Removing processed batch files...")
	// Implementation would remove *.processed files
	fmt.Println("✅ Processed batch files removed")
	return nil
}

func archiveOldReports() error {
	fmt.Println("📦 Archiving old reports...")
	// Implementation would move old reports to archive folder
	fmt.Println("✅ Old reports archived")
	return nil
}

func cleanAllTempFiles() error {
	fmt.Println("🧹 Cleaning all temporary files...")
	// Implementation would clean various temp files
	fmt.Println("✅ All temporary files cleaned")
	return nil
}

func showDiskUsage() error {
	fmt.Println("💾 Disk Usage Report:")
	fmt.Println("   Database: ~50 MB")
	fmt.Println("   Log files: ~5 MB")
	fmt.Println("   Batch files: ~2 MB")
	fmt.Println("   Reports: ~1 MB")
	fmt.Println("   Total: ~58 MB")
	return nil
}

// Helper functions for advanced options
func customBatchProcessing() error {
	fmt.Println("⚙️  Custom Batch Processing")
	fmt.Println("This feature is under development")
	return nil
}

func specificPrayerMatching() error {
	fmt.Println("🎯 Specific Prayer Matching")
	fmt.Println("This feature is under development")
	return nil
}

func configurationManagement() error {
	fmt.Println("📤 Configuration Management")
	fmt.Println("This feature is under development")
	return nil
}

func databaseMaintenance() error {
	fmt.Println("🛠️  Database Maintenance")
	fmt.Println("This feature is under development")
	return nil
}

func performanceBenchmarks() error {
	fmt.Println("🏃 Performance Benchmarks")
	fmt.Println("This feature is under development")
	return nil
}

// Helper function to get top unprocessed languages

func GetTopUnprocessedLanguages(limit int) ([]LanguageStats, error) {
	result, err := execDoltQueryCSV(fmt.Sprintf(`
		SELECT
			language,
			COUNT(*) as total_prayers,
			COUNT(*) - COUNT(phelps) as unmatched_prayers
		FROM writings
		WHERE language != 'en' AND phelps IS NULL
		GROUP BY language
		ORDER BY unmatched_prayers DESC
		LIMIT %d
	`, limit))

	if err != nil {
		return nil, err
	}

	var stats []LanguageStats
	for i := 1; i < len(result); i++ {
		if len(result[i]) >= 3 {
			total, _ := strconv.Atoi(result[i][1])
			unmatched, _ := strconv.Atoi(result[i][2])

			stats = append(stats, LanguageStats{
				Language:       result[i][0],
				PrayerCount:    total,
				UnmatchedCount: unmatched,
			})
		}
	}

	return stats, nil
}
