#!/bin/bash

# Ultra-compressed prayer matching script
# Processes ALL languages using smart multi-language batching
# Achieves 97% reduction in API calls compared to traditional approach

set -e

LOG_FILE="ultra_compressed_$(date +%Y%m%d_%H%M%S).log"

echo "🚀 ULTRA-COMPRESSED PRAYER MATCHING" | tee -a "$LOG_FILE"
echo "====================================" | tee -a "$LOG_FILE"
echo "$(date): Starting ultra-compressed bulk processing" | tee -a "$LOG_FILE"
echo "" | tee -a "$LOG_FILE"

# Build the prayer-matcher with ultra-compressed support
echo "🔨 Building prayer-matcher with ultra-compressed support..." | tee -a "$LOG_FILE"
if go build -o prayer-matcher main.go compressed_matcher.go ultra_compressed_matcher.go; then
    echo "✅ Build successful" | tee -a "$LOG_FILE"
else
    echo "❌ Build failed" | tee -a "$LOG_FILE"
    exit 1
fi

echo "" | tee -a "$LOG_FILE"

# Check current status
echo "📊 Analyzing current database status..." | tee -a "$LOG_FILE"
cd bahaiwritings

TOTAL_LANGUAGES=$(dolt sql -q "SELECT COUNT(DISTINCT language) FROM writings WHERE language != 'en'" -r csv | tail -n +2)
UNPROCESSED_LANGUAGES=$(dolt sql -q "
    SELECT COUNT(DISTINCT language)
    FROM writings
    WHERE language != 'en' AND phelps IS NULL
" -r csv | tail -n +2)
TOTAL_UNMATCHED=$(dolt sql -q "
    SELECT COUNT(*)
    FROM writings
    WHERE language != 'en' AND phelps IS NULL
" -r csv | tail -n +2)

cd ..

echo "  Total non-English languages: $TOTAL_LANGUAGES" | tee -a "$LOG_FILE"
echo "  Unprocessed languages: $UNPROCESSED_LANGUAGES" | tee -a "$LOG_FILE"
echo "  Total unmatched prayers: $TOTAL_UNMATCHED" | tee -a "$LOG_FILE"
echo "" | tee -a "$LOG_FILE"

if [ "$UNPROCESSED_LANGUAGES" -eq "0" ]; then
    echo "🎉 DATABASE ALREADY COMPLETE!" | tee -a "$LOG_FILE"
    echo "All languages have been processed. No work needed." | tee -a "$LOG_FILE"
    exit 0
fi

# Show efficiency comparison
TRADITIONAL_CALLS=$(( (256 * UNPROCESSED_LANGUAGES + 29) / 30 ))
COMPRESSED_CALLS=$UNPROCESSED_LANGUAGES
ESTIMATED_ULTRA_CALLS=$(( (UNPROCESSED_LANGUAGES + 4) / 5 ))  # Rough estimate: 5 languages per batch

echo "🎯 EFFICIENCY COMPARISON:" | tee -a "$LOG_FILE"
echo "  Traditional chunked approach: ~$TRADITIONAL_CALLS API calls" | tee -a "$LOG_FILE"
echo "  Compressed approach: $COMPRESSED_CALLS API calls" | tee -a "$LOG_FILE"
echo "  Ultra-compressed approach: ~$ESTIMATED_ULTRA_CALLS API calls (estimated)" | tee -a "$LOG_FILE"
echo "  Improvement: $(( (TRADITIONAL_CALLS - ESTIMATED_ULTRA_CALLS) * 100 / TRADITIONAL_CALLS ))% fewer calls than traditional" | tee -a "$LOG_FILE"
echo "" | tee -a "$LOG_FILE"

# Confirm before proceeding
echo "⚡ READY TO PROCESS $UNPROCESSED_LANGUAGES LANGUAGES" | tee -a "$LOG_FILE"
echo "This will use smart batching to minimize API calls." | tee -a "$LOG_FILE"
echo "Estimated processing time: $(( ESTIMATED_ULTRA_CALLS * 2 )) minutes" | tee -a "$LOG_FILE"
echo "" | tee -a "$LOG_FILE"

# Run the ultra-compressed matcher
echo "🚀 Starting ultra-compressed processing..." | tee -a "$LOG_FILE"
echo "Command: ./prayer-matcher -ultra -cli" | tee -a "$LOG_FILE"
echo "" | tee -a "$LOG_FILE"

START_TIME=$(date +%s)

if ./prayer-matcher -ultra -cli 2>&1 | tee -a "$LOG_FILE"; then
    END_TIME=$(date +%s)
    DURATION=$((END_TIME - START_TIME))

    echo "" | tee -a "$LOG_FILE"
    echo "✅ ULTRA-COMPRESSED PROCESSING COMPLETED!" | tee -a "$LOG_FILE"
    echo "$(date): Processing finished successfully" | tee -a "$LOG_FILE"
    echo "Total processing time: $DURATION seconds" | tee -a "$LOG_FILE"

    # Generate final statistics
    echo "" | tee -a "$LOG_FILE"
    echo "📈 FINAL DATABASE STATISTICS:" | tee -a "$LOG_FILE"
    cd bahaiwritings

    FINAL_MATCHED=$(dolt sql -q "SELECT COUNT(*) FROM writings WHERE phelps IS NOT NULL AND language != 'en'" -r csv | tail -n +2)
    FINAL_UNMATCHED=$(dolt sql -q "SELECT COUNT(*) FROM writings WHERE phelps IS NULL AND language != 'en'" -r csv | tail -n +2)
    COMPLETION_RATE=$(( (FINAL_MATCHED * 100) / (FINAL_MATCHED + FINAL_UNMATCHED) ))

    echo "  Matched prayers: $FINAL_MATCHED" | tee -a "../$LOG_FILE"
    echo "  Unmatched prayers: $FINAL_UNMATCHED" | tee -a "../$LOG_FILE"
    echo "  Completion rate: $COMPLETION_RATE%" | tee -a "../$LOG_FILE"

    echo "" | tee -a "../$LOG_FILE"
    echo "🏆 TOP MATCHED LANGUAGES:" | tee -a "../$LOG_FILE"
    dolt sql -q "
        SELECT
            language,
            COUNT(*) as total_prayers,
            COUNT(phelps) as matched_prayers,
            ROUND(COUNT(phelps) * 100.0 / COUNT(*), 1) as match_percent
        FROM writings
        WHERE language != 'en' AND COUNT(*) > 5
        GROUP BY language
        HAVING COUNT(phelps) > 0
        ORDER BY matched_prayers DESC
        LIMIT 15
    " | tee -a "../$LOG_FILE"

    cd ..

    if [ "$FINAL_UNMATCHED" -eq "0" ]; then
        echo "" | tee -a "$LOG_FILE"
        echo "🎉 COMPLETE SUCCESS!" | tee -a "$LOG_FILE"
        echo "ALL LANGUAGES HAVE BEEN FULLY PROCESSED!" | tee -a "$LOG_FILE"
        echo "The Bahá'í prayer matching database is now complete!" | tee -a "$LOG_FILE"
    elif [ "$FINAL_UNMATCHED" -lt "100" ]; then
        echo "" | tee -a "$LOG_FILE"
        echo "🎯 NEARLY COMPLETE!" | tee -a "$LOG_FILE"
        echo "Only $FINAL_UNMATCHED prayers remain unmatched." | tee -a "$LOG_FILE"
        echo "Consider manual review for the remaining cases." | tee -a "$LOG_FILE"
    else
        echo "" | tee -a "$LOG_FILE"
        echo "📊 SIGNIFICANT PROGRESS!" | tee -a "$LOG_FILE"
        echo "Reduced unmatched prayers from $TOTAL_UNMATCHED to $FINAL_UNMATCHED" | tee -a "$LOG_FILE"
        echo "May need additional processing for remaining languages." | tee -a "$LOG_FILE"
    fi

else
    END_TIME=$(date +%s)
    DURATION=$((END_TIME - START_TIME))

    echo "" | tee -a "$LOG_FILE"
    echo "❌ PROCESSING FAILED OR INTERRUPTED" | tee -a "$LOG_FILE"
    echo "$(date): Processing failed after $DURATION seconds" | tee -a "$LOG_FILE"
    echo "" | tee -a "$LOG_FILE"
    echo "Possible causes:" | tee -a "$LOG_FILE"
    echo "  - Rate limit hit (wait until 11pm Lisbon time)" | tee -a "$LOG_FILE"
    echo "  - Network connectivity issues" | tee -a "$LOG_FILE"
    echo "  - API quota exceeded" | tee -a "$LOG_FILE"
    echo "  - Processing timeout" | tee -a "$LOG_FILE"
    echo "" | tee -a "$LOG_FILE"
    echo "You can retry with: ./run_ultra_compressed.sh" | tee -a "$LOG_FILE"
fi

echo "" | tee -a "$LOG_FILE"
echo "📄 Full log saved to: $LOG_FILE" | tee -a "$LOG_FILE"
echo "" | tee -a "$LOG_FILE"

# Final summary for user
echo "================================="
echo "Ultra-compressed matching completed!"
echo "Check $LOG_FILE for full details"
echo "================================="

if [ "$FINAL_UNMATCHED" -eq "0" ]; then
    echo ""
    echo "🏆 ACHIEVEMENT UNLOCKED: Complete Database!"
    echo "All Bahá'í prayers across all languages are now matched!"
    echo "The cross-language prayer discovery system is ready!"
else
    echo ""
    echo "📊 Progress Report:"
    echo "   Started with: $TOTAL_UNMATCHED unmatched prayers"
    echo "   Remaining: $FINAL_UNMATCHED unmatched prayers"
    echo "   Success rate: $(( (TOTAL_UNMATCHED - FINAL_UNMATCHED) * 100 / TOTAL_UNMATCHED ))%"
    echo ""
    echo "Run './check_status.sh' to see current database state"
fi
