#!/bin/bash

# Bulk compressed matching script for all languages
# This massively reduces API calls by using semantic fingerprints

set -e

LOG_FILE="compressed_bulk_$(date +%Y%m%d_%H%M%S).log"

echo "🚀 Starting bulk compressed matching for ALL languages" | tee -a "$LOG_FILE"
echo "$(date): Starting compressed bulk processing" | tee -a "$LOG_FILE"

# Build the prayer-matcher if needed
if [ ! -f ./prayer-matcher ]; then
    echo "Building prayer-matcher..." | tee -a "$LOG_FILE"
    go build -o prayer-matcher main.go compressed_matcher.go
fi

# Get list of all languages with unmatched prayers
echo "📊 Analyzing database for languages to process..." | tee -a "$LOG_FILE"

cd bahaiwritings
LANGUAGES=$(dolt sql -q "
    SELECT DISTINCT language
    FROM writings
    WHERE language != 'en'
      AND phelps IS NULL
      AND language NOT IN ('', 'unknown')
    ORDER BY language
" -r csv | tail -n +2)

TOTAL_LANGUAGES=$(echo "$LANGUAGES" | wc -l)
cd ..

echo "Found $TOTAL_LANGUAGES languages to process:" | tee -a "$LOG_FILE"
echo "$LANGUAGES" | tee -a "$LOG_FILE"
echo "" | tee -a "$LOG_FILE"

# Process each language
PROCESSED=0
FAILED=0
SUCCESS_LANGS=""
FAILED_LANGS=""

for LANG in $LANGUAGES; do
    PROCESSED=$((PROCESSED + 1))

    echo "[$PROCESSED/$TOTAL_LANGUAGES] Processing language: $LANG" | tee -a "$LOG_FILE"
    echo "$(date): Starting $LANG" | tee -a "$LOG_FILE"

    # Run compressed matching
    if timeout 300 ./prayer-matcher -language="$LANG" -compressed -cli 2>&1 | tee -a "$LOG_FILE"; then
        echo "✅ SUCCESS: $LANG completed" | tee -a "$LOG_FILE"
        SUCCESS_LANGS="$SUCCESS_LANGS $LANG"
    else
        echo "❌ FAILED: $LANG failed or timed out" | tee -a "$LOG_FILE"
        FAILED=$((FAILED + 1))
        FAILED_LANGS="$FAILED_LANGS $LANG"
    fi

    echo "---" | tee -a "$LOG_FILE"

    # Small delay to be nice to the API
    sleep 2
done

# Final summary
echo "" | tee -a "$LOG_FILE"
echo "🏁 BULK COMPRESSED MATCHING COMPLETED!" | tee -a "$LOG_FILE"
echo "$(date): Bulk processing finished" | tee -a "$LOG_FILE"
echo "" | tee -a "$LOG_FILE"
echo "📈 SUMMARY:" | tee -a "$LOG_FILE"
echo "  Total languages: $TOTAL_LANGUAGES" | tee -a "$LOG_FILE"
echo "  Successful: $((TOTAL_LANGUAGES - FAILED))" | tee -a "$LOG_FILE"
echo "  Failed: $FAILED" | tee -a "$LOG_FILE"
echo "" | tee -a "$LOG_FILE"

if [ -n "$SUCCESS_LANGS" ]; then
    echo "✅ Successful languages:$SUCCESS_LANGS" | tee -a "$LOG_FILE"
fi

if [ -n "$FAILED_LANGS" ]; then
    echo "❌ Failed languages:$FAILED_LANGS" | tee -a "$LOG_FILE"
fi

# Generate database stats
echo "" | tee -a "$LOG_FILE"
echo "📊 Updated database stats:" | tee -a "$LOG_FILE"
cd bahaiwritings
dolt sql -q "
    SELECT
        language,
        COUNT(*) as total_prayers,
        COUNT(phelps) as matched_prayers,
        ROUND(COUNT(phelps) * 100.0 / COUNT(*), 1) as match_percentage
    FROM writings
    WHERE language != 'en'
    GROUP BY language
    HAVING COUNT(*) > 0
    ORDER BY match_percentage DESC, total_prayers DESC
" | tee -a "../$LOG_FILE"

echo "" | tee -a "$LOG_FILE"
echo "Log saved to: $LOG_FILE"
echo ""
echo "🎉 Compressed bulk matching completed!"
echo "    - Processed $TOTAL_LANGUAGES languages"
echo "    - Success rate: $(( (TOTAL_LANGUAGES - FAILED) * 100 / TOTAL_LANGUAGES ))%"
echo "    - Full log: $LOG_FILE"

if [ $FAILED -eq 0 ]; then
    echo ""
    echo "🚀 ALL LANGUAGES PROCESSED SUCCESSFULLY!"
    echo "The prayer matching database is now complete!"
else
    echo ""
    echo "⚠️  $FAILED languages had issues - check the log for details"
    echo "You may want to retry failed languages individually"
fi
