# Phelps Code Assignment - Process Log

Detailed log of decisions, challenges, and solutions during the assignment process.

## Session 1: 2025-11-28/29

### Initial Setup
- **Goal**: Assign Phelps codes to 117 English prayers without codes
- **Approach**: Categorize by citation type, process easiest first
- **Tools**: Dolt SQL, Python scripts, web search for verification

### Categorization Results

Prayers grouped by citation pattern:
```
6  Specific Tablets (Súriy-i-Ahzán, Epistle to Son of Wolf, etc.)
18 Prayers and Meditations (Roman numeral citations)
5  Gleanings (Roman numeral selections)
1  Kitáb-i-Aqdas
28 Other Sources (various compilations)
59 No Citations (will require text matching)
```

### Phase 1: Specific Tablets (6 prayers) ✓

**Challenges:**
1. Needed to identify source tablets from citations
2. Required web search for "Tablets of Bahá'u'lláh, p. 24"
3. Ridván tablet had no clear tablet name in citation

**Solutions:**
- Used web search to identify Bishárát (Glad-Tidings) as source for prayer about forgiveness
- Found Súriy-i-Dhikr in Days of Remembrance compilation
- Tentatively assigned Ridván tablet to BH01966 (may need verification)

**Mnemonics Created:**
- BH00155**WOU** - "Would that thou wert..." (Súriy-i-Ahzán)
- BH00005**ATT** - "Attire mine head..." (Epistle to Son of Wolf p.12)
- BH00297**EXC** - Excerpt from Súriy-i-Dhikr
- BH00568**IMP** - "I implore Thee by the blood..." (Bishárát)
- BH00005**PRA** - "We pray to God..." (Epistle to Son of Wolf p.70)
- BH01966**ANO** - "Another letter of thine..." (Ridván tablet)

**SQL File**: `completed/tablets.sql`

### Phase 2: Prayers and Meditations (18 prayers) ✓

**Process:**
1. Extract Roman numeral from citation (II, XVII, LV, etc.)
2. Convert to decimal (2, 17, 55, etc.)
3. Query for `PMP#NNN` in inventory_publications
4. Check if publication_ref has 'x' suffix → add mnemonic

**Technical Issue:**
- Initial subprocess parsing failed
- **Solution**: Switched to JSON output format (`--result-format=json`)

**Results:**
- 18/18 successfully mapped
- 3 were excerpts requiring mnemonics (PMP#002x, PMP#121x, PMP#153x)
- All others mapped directly to base PINs

**Mnemonic Generation:**
- Extract first 2-3 words from prayer
- Take first letter of each word
- Pad to 3 letters if needed

**SQL File**: `completed/prayers_meditations.sql`

### Database Issues Encountered

**Problem**: "database is read only" errors
**Cause**: Multiple dolt processes running
**Solution**: Retry after short delay, or use clean UPDATE statements without comments

### Key Learnings

1. **Publication ref patterns**:
   - `#` = item number (PMP#055 = Prayer 55)
   - `.` = page number (GHA.138 = page 138)
   - `x` = excerpt needing mnemonic

2. **Inventory structure**:
   - Inventory table = document registry
   - Publications table = links to source references
   - Not all prayers exist in inventory yet

3. **Web search valuable**:
   - Helped identify Bishárát source
   - Confirmed Days of Remembrance for Súriy-i-Dhikr
   - Could be used more for ambiguous cases

4. **Mnemonic strategy**:
   - Should be meaningful
   - Avoid collisions with existing codes
   - Check first words of prayer text

### Pending Issues to Resolve

1. **Gleanings prayers**: Need to map Roman numeral selections (VII, XXIII, etc.) to actual PINs
   - Problem: Gleanings uses page numbers (GHA.007) not selection numbers
   - Need to find mapping between selections and pages/PINs

2. **Ridván tablet (BH01966ANO)**: Verify this is correct PIN
   - May need to check Days of Remembrance compilation more thoroughly

3. **No citation prayers** (59): Need text matching strategy
   - Search inventory by first line (original/translated)
   - Use fuzzy matching if needed
   - May require creating new PINs for truly new prayers

## Next Session Tasks

### Immediate Priorities
1. **Other Sources category** (28 prayers)
   - Should be straightforward with clear citations
   - Likely mix of compilations and specific tablets

2. **Kitáb-i-Aqdas** (1 prayer)
   - Single prayer should be quick

3. **Gleanings** (5 prayers)
   - Research Gleanings structure
   - Find selection → PIN mapping
   - May need web search or reference materials

4. **No Citations** (59 prayers)
   - Develop text matching script
   - Search by first line in inventory
   - Document any new PIN creations needed

### Quality Assurance Steps
- [ ] Verify all mnemonics are unique
- [ ] Check no PIN collisions
- [ ] Test all SQL statements before execution
- [ ] Document any ambiguous decisions for user review

### Future Cleanup Tasks
- Transliteration standardization (ar-translit, fa-translit → Bahá'í standard)
- Final ambiguous prayer iteration
- Non-English prayer matching
- Directory cleanup (old review files, temp scripts)

## Notes for Future Reference

- Always use `--result-format=json` for subprocess queries
- Test SQL on small set before bulk execution
- Document reasoning for mnemonic choices
- Keep backup of original state before bulk updates
- Web search is your friend for identifying obscure sources

---
Last Updated: 2025-11-29 00:30
