# Quick Start: Error Correction Mode

## Build
```bash
go build -o prayer-matcher
```

## Run Error Correction
```bash
./prayer-matcher -ultra -heuristic -cli
```

### Flags Explained
- `-ultra`: Use ultra-compressed multi-language batching
- `-heuristic`: **Enable error detection & correction mode**
- `-cli`: Use Claude CLI (requires `claude` command)

## What Happens in Error Correction Mode

### 1. Language Prioritization
Languages are processed in this order:
1. **Nearly complete (>95%)** - HIGHEST priority for error correction
2. **Very high completion (>90%)**
3. **High completion (>80%)**
4. **Medium completion (>50%)**
5. **Lower completion (<50%)**

### 2. Error Detection
For each language, the system checks for:

#### 🔴 **Critical Errors** (Score: 10)
- **Duplicate Phelps IDs**: Multiple prayers assigned the same code
- Example: Two different prayers both have code `BH00123`

#### 🟠 **High Priority** (Score: 8)
- **Missing English Reference**: Phelps code doesn't exist in English
- Example: Prayer has code `XY99999` but no English prayer with that code

#### 🟡 **Medium Priority** (Score: 6)
- **Similar Prayer Confusion**: Wrong prayer type assigned
- Example: Short Obligatory Prayer marked as Long Obligatory Prayer

#### 🟢 **Low Priority** (Score: 4)
- **Length Mismatches**: Text length differs significantly from English
- Example: Target prayer is 3x longer or 0.3x shorter than English reference

### 3. Sample Composition
For a language with 95% completion (e.g., 100 matched, 5 unmatched):
- **Error prayers**: All detected errors (up to 100)
- **Unmatched prayers**: All 5 unmatched
- **Random matched**: Fill to 40% of matched = 40 total
- **Total re-evaluation**: ~45 prayers

## Monitoring Output

Watch for these log messages:

```
🔍 Error Detection Results:
   - Duplicate Phelps IDs: 3
   - Length mismatches: 7
   - Similar prayer confusion: 2
   - Missing English reference: 1
   - Total error candidates: 13
```

```
📊 Error Correction: 40% sample rate (completion: 95.2%), 45 total prayers selected (13 errors + 32 random)
```

```
🔀 Potential confusion in group 'obligatory': Short Obligatory Prayer (uuid-123) -> long obligatory prayer
```

```
❌ Missing English reference: uuid-456 has Phelps XY99999 (no English prayer found)
```

## Expected Processing

### Example Session
```
Phase 1: Processing transliteration languages
✅ ar-translit: 45 prayers matched from ar base
✅ fa-translit: 67 prayers matched from fa base

Phase 2: Processing 45 main languages with ultra-compression
[1/8] Processing batch: [cy, mt, is]
   🔍 Error Detection Results:
      - Duplicate Phelps IDs: 2
      - Length mismatches: 5
      - Similar prayer confusion: 1
      - Missing English reference: 0
      - Total error candidates: 8
   📊 Error Correction: 35% sample rate (completion: 92.3%), 28 total prayers selected (8 errors + 20 random)
✅ Batch completed successfully

[2/8] Processing batch: [sk, hu, ro]
...
```

## Alternative Builds

### Build utility scripts separately:
```bash
# Build consolidate reviews utility
go build -tags consolidate -o consolidate-reviews consolidate_reviews.go

# Build SQL generator utility
go build -tags generate -o generate-sql generate_sql_updates.go
```

## Troubleshooting

### "Rate limit hit"
```
🚨 RATE LIMIT HIT for batch: [de, fr, es]
💾 Batch saved to: pending_batch_de_fr_es_1234567890.json
```
- Wait for rate limit reset (usually 11pm Lisbon time)
- Resume with individual languages: `./prayer-matcher -language=de -compressed -cli`

### "All backends failed"
```
❌ Failed with Claude CLI: command not found
❌ Failed with Gemini CLI: command not found
❌ Failed with ollama: connection refused
```
- Ensure at least one backend is available
- Install: `claude`, `gemini`, or `ollama`

## Results

### Review Files Generated
- `review_ambiguous_XX_TIMESTAMP.txt` - Cases needing manual review
- `review_low_confidence_XX_TIMESTAMP.txt` - Low confidence matches
- `review_summary_XX_TIMESTAMP.txt` - Overall statistics

### What to Check
1. Look for high counts in error detection logs
2. Review ambiguous match files for patterns
3. Check summary files for completion rates
4. Verify no duplicate Phelps IDs remain

## Next Run

After fixing errors manually, run again to catch any new issues:
```bash
./prayer-matcher -ultra -heuristic -cli
```

The system will re-evaluate the same languages, finding remaining errors.
