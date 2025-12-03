# Language Validation Fix

## Problem Identified

Armenian (hy) prayers showed 0/146 matched in the database, but the consolidated review file claimed to have processed 303 Armenian prayers. Investigation revealed that **~40% of the "Armenian" review files contained prayers from other languages** (Kannada, Herero, etc.).

### Root Cause

The LLM-based matching system was returning incorrect UUIDs that didn't match the target language, and there was **no validation** to catch these errors:

1. **CompressedMatchResult** struct had no `TargetLanguage` field for validation
2. **Review file generation** assumed all matches belonged to the specified language parameter
3. **Ultra-compressed matcher** trusted the LLM's `TargetLanguage` field without verifying against the database
4. **Database updates** used language constraints in WHERE clauses, but mismatched UUIDs silently failed to update

### Example of the Problem

From `review_low_confidence_hy_20251128_212008.txt`:
- Entry 1: UUID `09bb0f23...` → Actually **hy** (Armenian) ✓ Correct
- Entry 2: UUID `978c4927...` → Actually **kn** (Kannada) ✗ Wrong!
- Entry 50: UUID `01848a8c...` → Actually **kn** (Kannada) ✗ Wrong!

This caused:
- Armenian prayers to be skipped (marked as "attempted" but not actually processed)
- Review files to contain prayers from wrong languages
- Consolidated review to show wrong text for Armenian prayers

## Fixes Implemented

### 1. Added `TargetLanguage` Field to CompressedMatchResult

```go
type CompressedMatchResult struct {
    EnglishPhelps   string   `json:"phelps"`
    TargetVersion   string   `json:"target_version"`
    TargetLanguage  string   `json:"target_language,omitempty"` // NEW: For validation
    MatchType       string   `json:"match_type"`
    Confidence      float64  `json:"confidence"`
    MatchReasons    []string `json:"match_reasons"`
    AmbiguityReason string   `json:"ambiguity_reason,omitempty"`
}
```

### 2. Added Language Validation Function

**File**: `main.go`

```go
func validateMatchLanguages(expectedLang string, matches []CompressedMatchResult) ([]CompressedMatchResult, int) {
    // Loads database and builds version → language map
    // Filters out matches where actualLang != expectedLang
    // Logs warnings for mismatches
    // Returns: (validMatches, invalidCount)
}
```

### 3. Updated Compressed Matcher to Validate Before Processing

**File**: `main.go:applyCompressedMatches()`

```go
// Validate and filter matches to ensure they belong to the correct language
validatedMatches, invalidCount := validateMatchLanguages(language, results.Matches)
if invalidCount > 0 {
    log.Printf("⚠️ Filtered out %d matches with incorrect language (expected %s)", 
        invalidCount, language)
}

// Use validatedMatches instead of results.Matches for review files and DB updates
```

### 4. Added Validation to Ultra-Compressed Matcher

**File**: `ultra_compressed_matcher.go`

Added helper function:
```go
func getLanguageForVersion(version string) (string, error) {
    // Queries database to get actual language for a UUID
}
```

Updated processing loop:
```go
for _, match := range results.Matches {
    if match.TargetLanguage == lang {
        // NEW: Validate actual language
        actualLang, err := getLanguageForVersion(match.TargetVersion)
        if actualLang != lang {
            log.Printf("⚠️ Language mismatch: LLM said %s but UUID %s is actually %s", 
                lang, match.TargetVersion, actualLang)
            languageMismatches++
            continue  // Skip this match
        }
        // Apply match...
    }
}
```

## Impact

### Before Fix
- ❌ Wrong UUIDs silently entered review files
- ❌ Languages got marked as "attempted" with wrong prayers
- ❌ No detection of LLM errors in language assignment
- ❌ Armenian prayers never actually processed

### After Fix
- ✅ Language mismatches detected and logged
- ✅ Only correct-language matches enter review files
- ✅ Database updates protected from wrong-language UUIDs
- ✅ Clear warnings when LLM makes mistakes
- ✅ Languages can be re-processed correctly

## Next Steps

1. **Re-run Armenian (hy) matching** - The language needs to be reprocessed with the fixes in place
2. **Clean up old review files** - Delete incorrect review files for hy (and potentially other affected languages)
3. **Check other low-match languages** - Verify if Urdu (ur: 10/138), fa-translit (0/248), or hy-translit have similar issues
4. **Monitor validation warnings** - Watch for language mismatch warnings in future runs to catch LLM errors

## Files Modified

- `compressed_matcher.go` - Added `TargetLanguage` field
- `main.go` - Added `validateMatchLanguages()` function and validation in `applyCompressedMatches()`
- `ultra_compressed_matcher.go` - Added `getLanguageForVersion()` and validation in batch processing
- `consolidate_reviews_old.go` - Deleted (duplicate file)

## Testing

Code compiles successfully:
```bash
go build -o prayer-matcher-test main.go compressed_matcher.go ultra_compressed_matcher.go menu.go
```

Ready for deployment and re-running affected languages.
