# TMP Code System - Temporary Phelps Codes

## Overview

The TMP code system creates a three-tier reference hierarchy that allows matching prayers even when there's no English reference available. This solves the problem of orphaned prayers that exist in Arabic or Persian but not in English.

## Architecture

### Three-Tier Fallback System

```
1. ENGLISH (Primary)    → Standard Phelps codes (e.g., BH00123)
2. ARABIC (Secondary)   → May have TMP codes or real Phelps codes
3. PERSIAN (Tertiary)   → May have TMP codes or real Phelps codes
4. NEW TMP (Fallback)   → Generate new TMP code if no match
```

### TMP Code Format

**Format**: `TMP#####` (e.g., `TMP00001`, `TMP00042`, `TMP00157`)

- **Prefix**: Always `TMP`
- **Number**: 5-digit sequential number (00001-99999)
- **Sequential**: Numbers are assigned in order, never reused
- **Cross-language**: Same TMP code = same prayer in different languages

## Key Principles

### 1. TMP Codes Are Always Overwritable

```
Priority: Real Phelps Codes > TMP Codes
```

When the system finds a match:
- If target has `TMP00042` but matches `BH00123` → Update to `BH00123`
- If target has `BH00123` and matches `TMP00042` → Keep `BH00123`
- If target has `TMP00042` and matches `TMP00042` → Keep (valid match)

### 2. Duplicate TMP Codes Are Valid

Unlike real Phelps codes, **duplicate TMP codes are intentional**:
- Same TMP code across languages = same prayer translation
- Error detection skips TMP codes in duplicate checking
- Example: `TMP00015` in Spanish + Turkish = same prayer in both languages

### 3. Sequential Assignment

TMP codes are assigned sequentially to avoid conflicts:
```go
Query: SELECT MAX(phelps) FROM writings WHERE phelps LIKE 'TMP%'
Last:  TMP00156
Next:  TMP00157
```

## Usage

### Step 1: Initialize TMP Codes

Before using TMP fallback, assign TMP codes to unmatched en/ar/fa prayers:

```bash
./prayer-matcher -init-tmp
```

**What it does:**
1. Finds all unmatched prayers in English, Arabic, and Persian
2. Assigns sequential TMP codes (TMP00001, TMP00002, etc.)
3. Updates database with TMP assignments

**Example output:**
```
🏷️  Assigning TMP codes to unmatched source prayers (en, ar, fa)...
   ✅ en: 23 TMP codes assigned
   ✅ ar: 67 TMP codes assigned  
   ✅ fa: 45 TMP codes assigned
✅ Total TMP codes assigned: 135
```

### Step 2: Use TMP Fallback Matching

Match target languages using the three-tier system:

```bash
./prayer-matcher -language=es -compressed -use-tmp-fallback -cli
```

**Matching Process:**
1. Try to match against English references first
2. If no English match, try Arabic references
3. If no Arabic match, try Persian references
4. If no match anywhere, generate NEW TMP code

### Step 3: Review Results

The system tracks which tier was used for each match:

```json
{
  "phelps": "BH00123",
  "target_version": "uuid-123",
  "match_type": "EXACT",
  "reference_tier": "english"  ← Matched against English
}
```

```json
{
  "phelps": "TMP00042",
  "target_version": "uuid-456", 
  "match_type": "LIKELY",
  "reference_tier": "arabic"  ← Matched against Arabic TMP
}
```

```json
{
  "phelps": "",
  "target_version": "uuid-789",
  "match_type": "NEW_TMP_CODE",
  "reference_tier": "none"  ← No match, needs new TMP
}
```

## Workflow Examples

### Example 1: Spanish Prayer Matching

**Scenario**: Matching Spanish prayers with TMP fallback

```bash
# Step 1: Initialize TMP codes (one-time setup)
./prayer-matcher -init-tmp

# Step 2: Match Spanish using three-tier fallback
./prayer-matcher -language=es -compressed -use-tmp-fallback -cli
```

**Results:**
- 150 prayers match English references → Get real Phelps codes
- 25 prayers match Arabic TMP codes → Get Arabic TMP codes
- 8 prayers match Persian TMP codes → Get Persian TMP codes
- 5 prayers have no match → Get new TMP codes (TMP00200-TMP00204)

### Example 2: Processing All Languages

Use ultra-compressed mode with TMP fallback:

```bash
# Step 1: Initialize TMP codes
./prayer-matcher -init-tmp

# Step 2: Process all languages with TMP fallback
# Note: Add TMP support to ultra mode in future iteration
./prayer-matcher -ultra -heuristic -cli
```

### Example 3: Finding Cross-Language Matches

After TMP assignment, prayers with the same TMP code are translations:

```sql
-- Find all languages that have TMP00042
SELECT language, version, name
FROM writings  
WHERE phelps = 'TMP00042'
ORDER BY language;

-- Result:
-- ar  | uuid-abc | صلاة الشفاء
-- es  | uuid-def | Oración de Curación
-- fa  | uuid-ghi | دعای شفا
-- tr  | uuid-jkl | Şifa Duası
```

## Implementation Details

### TMP Code Generation

```go
func getNextTMPNumber() (int, error) {
    // Query for highest existing TMP code
    query := `SELECT phelps FROM writings 
              WHERE phelps LIKE 'TMP%' 
              ORDER BY phelps DESC LIMIT 1`
    
    // Extract number and increment
    lastCode := "TMP00156"
    lastNum := 156
    return lastNum + 1  // Returns 157
}
```

### TMP Code Detection

```go
func isTMPCode(phelps string) bool {
    return strings.HasPrefix(phelps, "TMP")
}

// Usage in error detection:
if isTMPCode(phelps) {
    continue  // Skip - TMP duplicates are valid
}
```

### Match Priority

```
When matching a target prayer:

IF (matches English real Phelps):
    → Use English Phelps (highest priority)
ELSE IF (matches Arabic real Phelps):
    → Use Arabic Phelps
ELSE IF (matches Arabic TMP):
    → Use Arabic TMP
ELSE IF (matches Persian real Phelps):
    → Use Persian Phelps  
ELSE IF (matches Persian TMP):
    → Use Persian TMP
ELSE:
    → Generate NEW TMP code
```

## Benefits

### 1. Complete Coverage
- Every prayer can be matched, even without English reference
- No orphaned prayers left unmatched

### 2. Cross-Language Linking
- TMP codes link translations across languages
- Easy to find all versions of the same prayer

### 3. Future-Proof
- When English translation is added, TMP codes get upgraded
- No data loss - temporary codes preserve relationships

### 4. Error Correction Compatible
- TMP codes don't interfere with error detection
- Duplicate TMP codes are explicitly allowed
- Real Phelps codes always take priority

## Statistics After Implementation

Expected improvements:

**Before TMP System:**
- English-only matching: ~70-80% coverage
- Orphaned prayers: ~20-30% unmatched

**After TMP System:**
- Three-tier matching: ~95-98% coverage
- All prayers linked via TMP codes
- Cross-language matches enabled

## Command Reference

```bash
# Initialize TMP codes (one-time setup)
./prayer-matcher -init-tmp

# Single language with TMP fallback
./prayer-matcher -language=XX -compressed -use-tmp-fallback -cli

# With heuristic error correction
./prayer-matcher -language=XX -compressed -use-tmp-fallback -heuristic -cli

# Check database status
./prayer-matcher -status

# Build utility scripts
go build -tags consolidate -o consolidate-reviews consolidate_reviews.go
go build -tags generate -o generate-sql generate_sql_updates.go
```

## Database Queries

### Find all TMP codes
```sql
SELECT phelps, COUNT(*) as count
FROM writings
WHERE phelps LIKE 'TMP%'
GROUP BY phelps
ORDER BY count DESC, phelps;
```

### Find prayers needing upgrade from TMP to real Phelps
```sql
-- Prayers with TMP codes that now have English equivalents
SELECT w1.language, w1.phelps as tmp_code, w2.phelps as real_code
FROM writings w1
JOIN writings w2 ON w1.text_hash = w2.text_hash
WHERE w1.phelps LIKE 'TMP%'
  AND w2.language = 'en'
  AND w2.phelps NOT LIKE 'TMP%';
```

### Count TMP usage by language
```sql
SELECT language, COUNT(*) as tmp_count
FROM writings
WHERE phelps LIKE 'TMP%'
GROUP BY language
ORDER BY tmp_count DESC;
```

## Troubleshooting

### Issue: TMP numbers not sequential

**Symptom**: TMP codes jump (TMP00010 → TMP00150)

**Cause**: Normal - gaps occur when prayers get upgraded to real Phelps codes

**Solution**: No action needed - gaps are expected and harmless

### Issue: Same TMP code in same language

**Symptom**: Multiple prayers in Spanish have TMP00042

**Cause**: Actual duplicate/error - should not happen

**Solution**: Check prayer texts - they should be identical or need de-duplication

### Issue: TMP code not found in en/ar/fa

**Symptom**: German prayer has TMP00123 but no en/ar/fa prayer with that code

**Cause**: Source prayer was deleted or Phelps code changed

**Solution**: Re-run initialization: `./prayer-matcher -init-tmp`

## Future Enhancements

Potential improvements:

1. **Automatic Upgrades**: Detect when TMP-coded prayers get English translations
2. **TMP Code Cleanup**: Merge redundant TMP codes when duplicates discovered
3. **Cross-Tier Statistics**: Report on which tier most matches come from
4. **TMP Code History**: Track when codes were created and last used
5. **Smart TMP Generation**: Use content-based hashing for predictable codes
