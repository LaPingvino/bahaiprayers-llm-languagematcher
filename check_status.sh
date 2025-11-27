#!/bin/bash

# Status check script for prayer matching progress
# Shows current state of matching across all languages

echo "🔍 Prayer Matching Database Status Check"
echo "========================================"
echo ""

# Check if we're in the right directory
if [ ! -d "bahaiwritings" ]; then
    echo "❌ Error: bahaiwritings directory not found"
    echo "Please run this script from the bahaiprayers-llm-languagematcher directory"
    exit 1
fi

cd bahaiwritings

echo "📊 Overall Statistics:"
echo "----------------------"

# Total prayers and languages
TOTAL_PRAYERS=$(dolt sql -q "SELECT COUNT(*) FROM writings" -r csv | tail -n +2)
TOTAL_LANGUAGES=$(dolt sql -q "SELECT COUNT(DISTINCT language) FROM writings" -r csv | tail -n +2)
MATCHED_PRAYERS=$(dolt sql -q "SELECT COUNT(*) FROM writings WHERE phelps IS NOT NULL" -r csv | tail -n +2)
UNMATCHED_PRAYERS=$(dolt sql -q "SELECT COUNT(*) FROM writings WHERE phelps IS NULL" -r csv | tail -n +2)

echo "  Total prayers: $TOTAL_PRAYERS"
echo "  Total languages: $TOTAL_LANGUAGES"
echo "  Matched prayers: $MATCHED_PRAYERS"
echo "  Unmatched prayers: $UNMATCHED_PRAYERS"
echo "  Overall completion: $(( MATCHED_PRAYERS * 100 / TOTAL_PRAYERS ))%"
echo ""

echo "🎯 English Reference Status:"
echo "-----------------------------"
EN_TOTAL=$(dolt sql -q "SELECT COUNT(*) FROM writings WHERE language = 'en'" -r csv | tail -n +2)
EN_MATCHED=$(dolt sql -q "SELECT COUNT(*) FROM writings WHERE language = 'en' AND phelps IS NOT NULL" -r csv | tail -n +2)
echo "  English prayers with Phelps codes: $EN_MATCHED / $EN_TOTAL"
echo "  English reference completion: $(( EN_MATCHED * 100 / EN_TOTAL ))%"
echo ""

echo "📈 Top 20 Languages by Prayer Count:"
echo "------------------------------------"
dolt sql -q "
    SELECT
        language,
        COUNT(*) as total_prayers,
        COUNT(phelps) as matched_prayers,
        ROUND(COUNT(phelps) * 100.0 / COUNT(*), 1) as match_percent,
        CASE
            WHEN COUNT(phelps) = COUNT(*) THEN '✅ COMPLETE'
            WHEN COUNT(phelps) = 0 THEN '❌ UNPROCESSED'
            ELSE '🔄 PARTIAL'
        END as status
    FROM writings
    GROUP BY language
    HAVING COUNT(*) > 0
    ORDER BY total_prayers DESC
    LIMIT 20
"
echo ""

echo "🚨 Unprocessed Languages (need matching):"
echo "-----------------------------------------"
UNPROCESSED=$(dolt sql -q "
    SELECT language, COUNT(*) as prayer_count
    FROM writings
    WHERE language != 'en' AND phelps IS NULL AND language NOT LIKE '%-translit'
    GROUP BY language
    HAVING COUNT(*) > 0
    ORDER BY prayer_count DESC
" -r csv | tail -n +2)

if [ -z "$UNPROCESSED" ]; then
    echo "🎉 ALL LANGUAGES HAVE BEEN PROCESSED!"
    echo "The prayer matching database is complete!"
else
    echo "$UNPROCESSED" | while IFS=',' read -r lang count; do
        echo "  $lang: $count prayers"
    done

    UNPROCESSED_COUNT=$(echo "$UNPROCESSED" | wc -l)
    echo ""
    echo "Total unprocessed languages: $UNPROCESSED_COUNT"
fi

echo ""
echo "🔤 Transliteration Languages (need special handling):"
echo "----------------------------------------------------"
TRANSLIT=$(dolt sql -q "
    SELECT language, COUNT(*) as prayer_count, COUNT(phelps) as matched_count
    FROM writings
    WHERE language LIKE '%-translit'
    GROUP BY language
    ORDER BY COUNT(*) DESC
" -r csv | tail -n +2)

if [ -z "$TRANSLIT" ]; then
    echo "  No transliteration languages found"
else
    echo "$TRANSLIT" | while IFS=',' read -r lang count matched; do
        if [ "$matched" = "0" ]; then
            echo "  $lang: $count prayers (unprocessed)"
        else
            echo "  $lang: $count prayers ($matched matched)"
        fi
    done
fi

echo ""
echo "🔧 Processing Recommendations:"
echo "------------------------------"

# Get count of unprocessed languages (excluding transliterations)
UNPROCESSED_LANGS=$(dolt sql -q "
    SELECT COUNT(DISTINCT language)
    FROM writings
    WHERE language != 'en' AND phelps IS NULL AND language NOT LIKE '%-translit'
" -r csv | tail -n +2)

# Get count of unprocessed transliteration languages
TRANSLIT_LANGS=$(dolt sql -q "
    SELECT COUNT(DISTINCT language)
    FROM writings
    WHERE language LIKE '%-translit' AND phelps IS NULL
" -r csv | tail -n +2)

if [ "$UNPROCESSED_LANGS" -eq "0" ]; then
    echo "✅ Database is fully matched - no action needed!"
elif [ "$UNPROCESSED_LANGS" -le "5" ]; then
    echo "🎯 Few languages remaining - process individually:"
    echo "   ./prayer-matcher -language=XX -compressed -cli"
elif [ "$UNPROCESSED_LANGS" -le "20" ]; then
    echo "⚡ Moderate number remaining - consider batch processing:"
    echo "   ./prayer-matcher -ultra -cli"
else
    echo "🚀 Many languages remaining - run ultra-compressed processing:"
    echo "   ./prayer-matcher -ultra -cli"
    echo "   Estimated API calls needed: $(( (UNPROCESSED_LANGS + 4) / 5 ))"
    echo "   Estimated time: $(( UNPROCESSED_LANGS / 3 )) minutes"
fi

if [ "$TRANSLIT_LANGS" -gt "0" ]; then
    echo ""
    echo "🔤 Transliteration languages: $TRANSLIT_LANGS need special handling"
    echo "   These will be processed automatically by copying from base languages"
fi

echo ""
echo "📊 Rate Limit Considerations:"
echo "-----------------------------"
if [ "$UNPROCESSED_LANGS" -gt "0" ]; then
    TRADITIONAL_CALLS=$(( EN_MATCHED * UNPROCESSED_LANGS / 30 ))
    ULTRA_CALLS=$(( (UNPROCESSED_LANGS + 4) / 5 ))
    echo "  Traditional approach would need: $TRADITIONAL_CALLS API calls"
    echo "  Ultra-compressed approach needs: $ULTRA_CALLS API calls"
    echo "  Efficiency improvement: $(( (TRADITIONAL_CALLS - ULTRA_CALLS) * 100 / TRADITIONAL_CALLS ))% fewer calls"
else
    echo "  ✅ No processing needed - database complete!"
fi

echo ""
echo "🔄 Last Updated: $(date)"
cd ..
