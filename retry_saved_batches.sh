#!/bin/bash

# Retry Saved Batches Script
# Processes saved batch files using any available backend
# Simple fallback: Claude -> Gemini -> gpt-oss

set -e

echo "🔄 Retry Saved Batches"
echo "====================="
echo ""

# Check for saved batch files
REMAINING_FILES=$(ls remaining_batches_*.json 2>/dev/null || true)
PENDING_FILES=$(ls pending_batch_*.json 2>/dev/null || true)

if [ -z "$REMAINING_FILES" ] && [ -z "$PENDING_FILES" ]; then
    echo "📭 No saved batch files found"
    echo ""
    echo "Saved batches are created when processing is interrupted by rate limits."
    echo "Run one of these first:"
    echo "  ./prayer-matcher -ultra -cli"
    echo "  ./smart_fallback.sh"
    exit 0
fi

echo "📁 Found saved batch files:"
[ -n "$REMAINING_FILES" ] && echo "  Remaining batches: $(echo $REMAINING_FILES | wc -w)"
[ -n "$PENDING_FILES" ] && echo "  Pending batches: $(echo $PENDING_FILES | wc -w)"
echo ""

# Detect available backends
BACKENDS=""
if command -v claude &> /dev/null; then
    BACKENDS="$BACKENDS claude:-cli:Claude"
fi
if command -v gemini &> /dev/null && [ -n "$GEMINI_API_KEY" ]; then
    BACKENDS="$BACKENDS gemini:-gemini:Gemini"
fi
if command -v gpt-oss &> /dev/null; then
    BACKENDS="$BACKENDS gpt-oss:-gpt-oss:gpt-oss"
fi

if [ -z "$BACKENDS" ]; then
    echo "❌ No backends available!"
    echo "Install at least one: claude, gemini (with GEMINI_API_KEY), or gpt-oss"
    exit 1
fi

echo "🔧 Available backends: $(echo $BACKENDS | sed 's/.*://g' | tr ' ' ', ')"
echo ""

# Build prayer-matcher
if [ ! -f ./prayer-matcher ]; then
    echo "🔨 Building prayer-matcher..."
    go build -o prayer-matcher main.go compressed_matcher.go ultra_compressed_matcher.go
fi

# Function to extract languages from batch files
extract_languages() {
    local file="$1"
    # Extract language codes from JSON (simple approach)
    grep -o '"[a-z][a-z-]*"' "$file" | \
        grep -E '^"[a-z]{2,3}(-[a-z]+)?"$' | \
        tr -d '"' | \
        grep -v -E "(language|phelps|version|type|status|created|summary)" | \
        sort -u | \
        head -20  # Safety limit
}

# Function to try processing a language with available backends
process_language() {
    local lang="$1"

    for backend_info in $BACKENDS; do
        local cmd=$(echo "$backend_info" | cut -d: -f1)
        local flag=$(echo "$backend_info" | cut -d: -f2)
        local name=$(echo "$backend_info" | cut -d: -f3)

        echo "    🔄 Trying $name..."

        if timeout 300 ./prayer-matcher -language="$lang" -compressed $flag 2>/dev/null; then
            echo "    ✅ Success with $name"
            return 0
        else
            echo "    ❌ Failed with $name"
        fi

        sleep 2
    done

    echo "    💔 All backends failed for $lang"
    return 1
}

TOTAL_PROCESSED=0
TOTAL_FAILED=0
PROCESSED_FILES=0

# Process pending batch files (individual languages)
if [ -n "$PENDING_FILES" ]; then
    echo "🔄 Processing pending batches..."

    for file in $PENDING_FILES; do
        echo "📄 Processing: $file"

        # Extract language from filename
        lang=$(echo "$file" | sed 's/pending_batch_//; s/_[0-9]*\.json//')

        if [[ "$lang" =~ ^[a-z]{2,3}(-[a-z]+)?$ ]]; then
            echo "  Language: $lang"

            if process_language "$lang"; then
                TOTAL_PROCESSED=$((TOTAL_PROCESSED + 1))
                mv "$file" "${file}.processed"
                echo "  📁 Moved to ${file}.processed"
            else
                TOTAL_FAILED=$((TOTAL_FAILED + 1))
            fi
        else
            echo "  ⚠️ Invalid language code: $lang"
        fi

        PROCESSED_FILES=$((PROCESSED_FILES + 1))
        echo ""
    done
fi

# Process remaining batch files (multiple languages)
if [ -n "$REMAINING_FILES" ]; then
    echo "🔄 Processing remaining batches..."

    for file in $REMAINING_FILES; do
        echo "📄 Processing: $file"

        languages=$(extract_languages "$file")

        if [ -z "$languages" ]; then
            echo "  ⚠️ Could not extract languages from $file"
            continue
        fi

        echo "  Languages: $(echo "$languages" | head -5 | tr '\n' ' ')..."

        for lang in $languages; do
            if [[ "$lang" =~ ^[a-z]{2,3}(-[a-z]+)?$ ]] && [ ${#lang} -le 10 ]; then
                echo "  Processing: $lang"

                if process_language "$lang"; then
                    TOTAL_PROCESSED=$((TOTAL_PROCESSED + 1))
                else
                    TOTAL_FAILED=$((TOTAL_FAILED + 1))
                fi
            fi
        done

        mv "$file" "${file}.processed"
        echo "  📁 Moved to ${file}.processed"
        PROCESSED_FILES=$((PROCESSED_FILES + 1))
        echo ""
    done
fi

# Final summary
echo "🏁 Retry completed!"
echo "  Files processed: $PROCESSED_FILES"
echo "  Languages successful: $TOTAL_PROCESSED"
echo "  Languages failed: $TOTAL_FAILED"

if [ $TOTAL_PROCESSED -gt 0 ]; then
    echo ""
    echo "✅ Successfully processed $TOTAL_PROCESSED languages"
    echo "📊 Updated database status:"
    ./check_status.sh
fi

if [ $TOTAL_FAILED -gt 0 ]; then
    echo ""
    echo "⚠️ $TOTAL_FAILED languages failed with all backends"
    echo "These may need manual review or different approaches"
fi

echo ""
echo "🧹 Cleanup:"
echo "  - Processed files moved to *.processed"
echo "  - Run 'rm *.processed' when satisfied with results"
echo "  - Failed responses saved as failed_response_*.txt"
