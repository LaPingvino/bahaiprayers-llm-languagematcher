# Final Usage Guide - Prayer Matcher with TMP System

## Quick Start

### Initial Setup (One Time)

```bash
# 1. Build the project
go build -o prayer-matcher

# 2. Initialize TMP codes for unmatched en/ar/fa prayers
./prayer-matcher -init-tmp
```

### Process All Languages with Error Correction

```bash
# Ultra-compressed mode with heuristic error correction
./prayer-matcher -ultra -heuristic -cli
```

### Process Single Language with TMP Fallback

```bash
# Match a specific language using three-tier fallback (en → ar → fa → new TMP)
./prayer-matcher -language=es -compressed -use-tmp-fallback -cli
```

## Complete Feature Set

### 1. Error Correction Mode (Recommended)

**Purpose**: Find and fix data quality issues in existing matches

```bash
./prayer-matcher -ultra -heuristic -cli
```

**What it does:**
- Processes languages with highest completion first (error correction focus)
- Detects 4 types of errors:
  - Duplicate Phelps IDs (same code on different prayers)
  - Length mismatches (>2.5x or <0.4x English length)
  - Similar prayer confusion (short vs long obligatory)
  - Missing English references (invalid codes)
- Re-evaluates 10-40% of matched prayers (up to 100 per language)
- Includes all languages with >10 matched prayers

**Sample rates by completion:**
- >95% complete: 40% re-evaluation
- >90% complete: 35% re-evaluation
- >80% complete: 25% re-evaluation
- >50% complete: 15% re-evaluation

### 2. TMP Code System

**Purpose**: Match prayers against Arabic/Persian when no English reference exists

```bash
# Step 1: Initialize TMP codes (one-time)
./prayer-matcher -init-tmp

# Step 2: Match with three-tier fallback
./prayer-matcher -language=tr -compressed -use-tmp-fallback -cli
```

**Fallback hierarchy:**
1. English references (standard Phelps codes)
2. Arabic references (may have TMP codes)
3. Persian references (may have TMP codes)
4. Generate new TMP code if no match

**TMP code format:** `TMP00001`, `TMP00002`, etc.

### 3. Standard Compressed Matching

**Purpose**: Fast fingerprint-based matching (without TMP fallback)

```bash
./prayer-matcher -language=fr -compressed -cli
```

### 4. Database Status Check

**Purpose**: See completion statistics and recommendations

```bash
./prayer-matcher -status
```

**Output:**
- Total prayers and languages
- Completion rates
- Unprocessed languages
- Processing recommendations

### 5. Retry Failed Batches

**Purpose**: Resume processing after rate limits

```bash
./prayer-matcher -retry
```

## Command-Line Flags Reference

### Core Flags
```bash
-language=XX        # Target language code (e.g., es, fr, de)
-cli                # Use Claude CLI (requires 'claude' command)
-gemini             # Use Gemini CLI fallback
-gpt-oss            # Use ollama (local, no rate limits)
```

### Processing Modes
```bash
-ultra              # Ultra-compressed multi-language batching
-compressed         # Compressed fingerprint matching  
-heuristic          # Enable error detection & correction
-use-tmp-fallback   # Enable three-tier matching (en→ar→fa→TMP)
```

### TMP System
```bash
-init-tmp           # Initialize TMP codes for en/ar/fa
```

### Other Options
```bash
-status             # Check database status
-retry              # Retry failed batches
-reverse            # Process smallest languages first
-skip-processed     # Skip languages with review files (default: true)
-dry-run            # Show what would happen without updating
```

## Common Workflows

### Workflow 1: Complete Database Processing

```bash
# 1. Build
go build -o prayer-matcher

# 2. Initialize TMP codes
./prayer-matcher -init-tmp

# 3. Process everything with error correction
./prayer-matcher -ultra -heuristic -cli

# 4. Check status
./prayer-matcher -status
```

### Workflow 2: Fix Errors in High-Completion Languages

```bash
# Focus on nearly-complete languages for error correction
./prayer-matcher -ultra -heuristic -cli

# Languages >95% complete get 40% re-evaluation
# Catches duplicates, length mismatches, confusion, invalid codes
```

### Workflow 3: Process Rare Language with TMP Fallback

```bash
# For languages where English references might be missing
./prayer-matcher -init-tmp
./prayer-matcher -language=tl -compressed -use-tmp-fallback -cli
```

### Workflow 4: Resume After Rate Limit

```bash
# If processing hit rate limit, retry saved batches
./prayer-matcher -retry
```

## Output Files

### Review Files (per language)
- `review_ambiguous_XX_TIMESTAMP.txt` - Cases needing manual review
- `review_low_confidence_XX_TIMESTAMP.txt` - Low confidence matches
- `review_summary_XX_TIMESTAMP.txt` - Statistics and completion rates

### Failed Processing
- `failed_response_XX_TIMESTAMP.txt` - LLM responses that couldn't be parsed
- `pending_batch_XX_TIMESTAMP.json` - Batches saved due to rate limits

### Consolidated Reports
- `consolidated_review_TIMESTAMP.txt` - Combined review file
- `review_summary.txt` - Overall statistics

## Monitoring

### Watch for These Log Messages

**TMP Code Initialization:**
```
🏷️  Assigning TMP codes to unmatched source prayers (en, ar, fa)...
   ✅ en: 23 TMP codes assigned
   ✅ ar: 67 TMP codes assigned
   ✅ fa: 45 TMP codes assigned
```

**Error Detection:**
```
🔍 Error Detection Results:
   - Duplicate Phelps IDs: 3
   - Length mismatches: 7
   - Similar prayer confusion: 2
   - Missing English reference: 1
   - Total error candidates: 13
```

**Processing Progress:**
```
[1/8] Processing batch: [cy, mt, is]
✅ Batch completed successfully
```

**Rate Limit Hit:**
```
🚨 RATE LIMIT HIT for batch: [de, fr, es]
💾 Batch saved to: pending_batch_de_fr_es_1234567890.json
```

## Expected Results

### After Error Correction Pass

Languages with >95% completion:
- 40% of matched prayers re-evaluated
- Errors identified and flagged
- Ambiguous cases saved for review

### After TMP System Implementation

Before:
- ~70-80% coverage (English-only matching)
- ~20-30% orphaned prayers

After:
- ~95-98% coverage (three-tier matching)
- All prayers linked via TMP or real Phelps codes
- Cross-language matches enabled

## Database Queries for Verification

### Check TMP code distribution
```sql
SELECT language, COUNT(*) as tmp_count
FROM writings
WHERE phelps LIKE 'TMP%'
GROUP BY language
ORDER BY tmp_count DESC;
```

### Find duplicate real Phelps codes (errors)
```sql
SELECT phelps, language, COUNT(*) as count
FROM writings
WHERE phelps NOT LIKE 'TMP%'
  AND phelps != ''
  AND language != 'en'
GROUP BY phelps, language
HAVING COUNT(*) > 1;
```

### Check completion rates
```sql
SELECT 
    language,
    COUNT(*) as total,
    SUM(CASE WHEN phelps IS NOT NULL AND phelps != '' THEN 1 ELSE 0 END) as matched,
    ROUND(100.0 * SUM(CASE WHEN phelps IS NOT NULL AND phelps != '' THEN 1 ELSE 0 END) / COUNT(*), 1) as completion_rate
FROM writings
WHERE language != 'en'
GROUP BY language
ORDER BY completion_rate DESC;
```

## Troubleshooting

### "Rate limit hit"
**Solution**: Wait for reset (11pm Lisbon time) or use `-gpt-oss` for local processing

### "TMP codes not sequential"
**Normal**: Gaps occur when codes are upgraded to real Phelps codes

### "Duplicate TMP codes in same language"
**Check**: Prayers should be identical - may need de-duplication

### "All backends failed"
**Solution**: Install at least one backend: `claude`, `gemini`, or `ollama`

### Build errors with other files
**Solution**: Utility scripts use build tags:
```bash
go build -tags consolidate -o consolidate-reviews consolidate_reviews.go
go build -tags generate -o generate-sql generate_sql_updates.go
```

## Performance Tips

1. **Use local ollama for testing**: `-gpt-oss` has no rate limits
2. **Process in batches**: Ultra mode automatically batches languages
3. **Start with error correction**: Fix existing data before adding more
4. **Initialize TMP codes once**: Only needed when starting or after major changes

## Next Steps After Processing

1. Review ambiguous match files for patterns
2. Check summary files for completion rates
3. Verify no duplicate real Phelps codes remain (TMP duplicates are OK)
4. Consider manual review of low-confidence matches
5. Re-run error correction periodically to catch new issues

## Documentation Files

- `ERROR_CORRECTION_IMPROVEMENTS.md` - Technical details on error system
- `TMP_CODE_SYSTEM.md` - Complete TMP code documentation
- `QUICK_START_ERROR_CORRECTION.md` - Quick reference guide
- `FINAL_USAGE_GUIDE.md` - This file

## Support

For issues or questions:
- Check log output for specific error messages
- Review generated review files for patterns
- Use `-status` to verify database state
- Test with small batches first: `-language=mt` (Maltese is small)
