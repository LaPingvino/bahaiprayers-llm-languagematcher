#!/bin/bash

# Smart Fallback Processing Script
# Tries Claude -> Gemini -> gpt-oss in order until successful
# Handles rate limits gracefully and provides manual pickup

set -e

LOG_FILE="smart_fallback_$(date +%Y%m%d_%H%M%S).log"

echo "🧠 Smart Fallback Prayer Matching" | tee -a "$LOG_FILE"
echo "=================================" | tee -a "$LOG_FILE"
echo "$(date): Starting intelligent multi-backend processing" | tee -a "$LOG_FILE"
echo "" | tee -a "$LOG_FILE"

# Check available backends
CLAUDE_AVAILABLE=false
GEMINI_AVAILABLE=false
GPTOSS_AVAILABLE=false

echo "🔍 Checking available backends..." | tee -a "$LOG_FILE"

# Check Claude CLI
if command -v claude &> /dev/null; then
    CLAUDE_AVAILABLE=true
    echo "  ✅ Claude CLI: Available" | tee -a "$LOG_FILE"
else
    echo "  ❌ Claude CLI: Not found" | tee -a "$LOG_FILE"
fi

# Check Gemini CLI and API key
if command -v gemini &> /dev/null && [ -n "$GEMINI_API_KEY" ]; then
    GEMINI_AVAILABLE=true
    echo "  ✅ Gemini CLI: Available with API key" | tee -a "$LOG_FILE"
elif command -v gemini &> /dev/null; then
    echo "  ⚠️  Gemini CLI: Available but no API key (GEMINI_API_KEY)" | tee -a "$LOG_FILE"
else
    echo "  ❌ Gemini CLI: Not found" | tee -a "$LOG_FILE"
fi

# Check gpt-oss
if command -v gpt-oss &> /dev/null; then
    GPTOSS_AVAILABLE=true
    echo "  ✅ gpt-oss: Available (local fallback)" | tee -a "$LOG_FILE"
else
    echo "  ❌ gpt-oss: Not found" | tee -a "$LOG_FILE"
fi

if ! $CLAUDE_AVAILABLE && ! $GEMINI_AVAILABLE && ! $GPTOSS_AVAILABLE; then
    echo "❌ No backends available! Please install at least one:" | tee -a "$LOG_FILE"
    echo "  - Claude CLI: https://claude.ai/cli" | tee -a "$LOG_FILE"
    echo "  - Gemini CLI: https://ai.google.dev/gemini-api/docs/cli" | tee -a "$LOG_FILE"
    echo "  - gpt-oss: https://github.com/your-repo/gpt-oss" | tee -a "$LOG_FILE"
    exit 1
fi

echo "" | tee -a "$LOG_FILE"

# Build prayer-matcher
if [ ! -f ./prayer-matcher ]; then
    echo "🔨 Building prayer-matcher..." | tee -a "$LOG_FILE"
    if ! go build -o prayer-matcher main.go compressed_matcher.go ultra_compressed_matcher.go; then
        echo "❌ Build failed" | tee -a "$LOG_FILE"
        exit 1
    fi
    echo "✅ Build successful" | tee -a "$LOG_FILE"
fi

# Function to try processing with a specific backend
try_backend() {
    local backend="$1"
    local backend_flag="$2"
    local backend_name="$3"

    echo "🔄 Attempting with $backend_name..." | tee -a "$LOG_FILE"

    if timeout 1800 ./prayer-matcher -ultra $backend_flag 2>&1 | tee -a "$LOG_FILE"; then
        echo "✅ SUCCESS with $backend_name!" | tee -a "$LOG_FILE"
        return 0
    else
        local exit_code=$?
        echo "❌ FAILED with $backend_name (exit code: $exit_code)" | tee -a "$LOG_FILE"
        return $exit_code
    fi
}

# Function to process saved batches with fallback
process_saved_batches() {
    local backend_flag="$1"
    local backend_name="$2"

    echo "🔄 Processing saved batches with $backend_name..." | tee -a "$LOG_FILE"

    local remaining_files=$(ls remaining_batches_*.json 2>/dev/null || true)
    local pending_files=$(ls pending_batch_*.json 2>/dev/null || true)

    if [ -z "$remaining_files" ] && [ -z "$pending_files" ]; then
        echo "  📭 No saved batches found" | tee -a "$LOG_FILE"
        return 0
    fi

    local processed=0
    local failed=0

    # Process individual pending batches
    for file in $pending_files; do
        if [ -f "$file" ]; then
            local lang=$(echo "$file" | sed 's/pending_batch_//; s/_[0-9]*\.json//')

            echo "  🔄 Processing $lang with $backend_name..." | tee -a "$LOG_FILE"

            if timeout 300 ./prayer-matcher -language="$lang" -compressed $backend_flag 2>&1 | tee -a "$LOG_FILE"; then
                echo "  ✅ $lang completed" | tee -a "$LOG_FILE"
                mv "$file" "${file}.processed"
                processed=$((processed + 1))
            else
                echo "  ❌ $lang failed" | tee -a "$LOG_FILE"
                failed=$((failed + 1))
            fi

            sleep 3
        fi
    done

    echo "  📊 Processed: $processed, Failed: $failed" | tee -a "$LOG_FILE"
    return $failed
}

# Main processing strategy
echo "🚀 Starting smart fallback processing..." | tee -a "$LOG_FILE"
echo "Strategy: Claude → Gemini → gpt-oss (local)" | tee -a "$LOG_FILE"
echo "" | tee -a "$LOG_FILE"

SUCCESS=false
FINAL_BACKEND=""

# Try Claude first (fastest when working)
if $CLAUDE_AVAILABLE && ! $SUCCESS; then
    echo "Phase 1: Trying Claude CLI" | tee -a "$LOG_FILE"
    if try_backend "claude" "-cli" "Claude CLI"; then
        SUCCESS=true
        FINAL_BACKEND="Claude CLI"
    else
        echo "  Claude failed (likely rate limit), trying next backend..." | tee -a "$LOG_FILE"
        sleep 5
    fi
fi

# Try Gemini if Claude failed
if $GEMINI_AVAILABLE && ! $SUCCESS; then
    echo "" | tee -a "$LOG_FILE"
    echo "Phase 2: Trying Gemini CLI" | tee -a "$LOG_FILE"
    if try_backend "gemini" "-gemini" "Gemini CLI"; then
        SUCCESS=true
        FINAL_BACKEND="Gemini CLI"
    else
        echo "  Gemini failed, trying next backend..." | tee -a "$LOG_FILE"

        # Try to process any saved batches from Claude with Gemini
        echo "  🔄 Attempting to process Claude's saved batches with Gemini..." | tee -a "$LOG_FILE"
        process_saved_batches "-gemini" "Gemini CLI"
        sleep 5
    fi
fi

# Try gpt-oss as last resort (local, always works but slow)
if $GPTOSS_AVAILABLE && ! $SUCCESS; then
    echo "" | tee -a "$LOG_FILE"
    echo "Phase 3: Trying gpt-oss (local fallback)" | tee -a "$LOG_FILE"
    echo "⏳ Note: gpt-oss is slower but has no rate limits" | tee -a "$LOG_FILE"

    if try_backend "gpt-oss" "-gpt-oss" "gpt-oss (local)"; then
        SUCCESS=true
        FINAL_BACKEND="gpt-oss (local)"
    else
        echo "  gpt-oss failed, trying to process saved batches..." | tee -a "$LOG_FILE"

        # Try to process any saved batches with gpt-oss
        echo "  🔄 Processing all saved batches with gpt-oss..." | tee -a "$LOG_FILE"
        process_saved_batches "-gpt-oss" "gpt-oss (local)"
    fi
fi

# Final summary
echo "" | tee -a "$LOG_FILE"
echo "🏁 SMART FALLBACK PROCESSING COMPLETED" | tee -a "$LOG_FILE"
echo "$(date): Processing finished" | tee -a "$LOG_FILE"
echo "" | tee -a "$LOG_FILE"

if $SUCCESS; then
    echo "🎉 SUCCESS with $FINAL_BACKEND!" | tee -a "$LOG_FILE"
    echo "✅ Prayer database processing completed" | tee -a "$LOG_FILE"

    echo "" | tee -a "$LOG_FILE"
    echo "📊 Final database status:" | tee -a "$LOG_FILE"
    ./check_status.sh | tee -a "$LOG_FILE"

else
    echo "❌ ALL BACKENDS FAILED" | tee -a "$LOG_FILE"
    echo "" | tee -a "$LOG_FILE"
    echo "🔧 Possible solutions:" | tee -a "$LOG_FILE"
    echo "  1. Wait for API rate limits to reset" | tee -a "$LOG_FILE"
    echo "  2. Check API keys (GEMINI_API_KEY, etc.)" | tee -a "$LOG_FILE"
    echo "  3. Install missing backends" | tee -a "$LOG_FILE"
    echo "  4. Process saved batches manually" | tee -a "$LOG_FILE"

    # Show any saved batches that need manual processing
    local saved_files=$(ls remaining_batches_*.json pending_batch_*.json 2>/dev/null || true)
    if [ -n "$saved_files" ]; then
        echo "" | tee -a "$LOG_FILE"
        echo "💾 Saved batch files for manual processing:" | tee -a "$LOG_FILE"
        for file in $saved_files; do
            echo "  - $file" | tee -a "$LOG_FILE"
        done
    fi
fi

echo "" | tee -a "$LOG_FILE"
echo "📄 Full log saved to: $LOG_FILE" | tee -a "$LOG_FILE"

# Backend availability summary
echo "" | tee -a "$LOG_FILE"
echo "🔧 Backend Availability Summary:" | tee -a "$LOG_FILE"
echo "  Claude CLI: $($CLAUDE_AVAILABLE && echo "✅ Available" || echo "❌ Missing")" | tee -a "$LOG_FILE"
echo "  Gemini CLI: $($GEMINI_AVAILABLE && echo "✅ Available" || echo "❌ Missing/No Key")" | tee -a "$LOG_FILE"
echo "  gpt-oss:    $($GPTOSS_AVAILABLE && echo "✅ Available" || echo "❌ Missing")" | tee -a "$LOG_FILE"

if ! $SUCCESS; then
    exit 1
fi
