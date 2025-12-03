# Phelps Code Assignment Workspace

This directory contains the organized workflow for assigning Phelps codes to prayers.

## Directory Structure

```
phelps_assignment/
├── README.md                    # This file
├── completed/                   # Successfully processed prayers
│   ├── tablets.sql             # 6 specific tablets ✓
│   └── prayers_meditations.sql # 18 Prayers & Meditations ✓
├── pending/                     # Prayers awaiting processing
│   ├── gleanings.json          # 5 Gleanings prayers
│   ├── kitab_aqdas.json        # 1 Kitáb-i-Aqdas prayer
│   ├── other_sources.json      # 28 other source prayers
│   └── no_citation.json        # 59 prayers without citations
├── working/                     # Current work in progress
└── docs/                        # Documentation and logs
    ├── SESSION_SUMMARY.md       # Overall progress
    ├── PROCESS_LOG.md           # Detailed steps and decisions
    └── PUBLICATION_CODES.md     # Reference for abbreviations
```

## Current Status

**Total English Prayers**: 117
- ✅ **Completed**: 24 (20.5%)
- ⏳ **Pending**: 93 (79.5%)

### Completed (24 prayers)
- [x] 6 Specific Tablets (Súriy-i-Ahzán, Epistle to Son of Wolf, etc.)
- [x] 18 Prayers and Meditations (II, XVII, LV, etc.)

### Pending by Category
- [ ] 5 Gleanings (VII, XXIII, CXV, CXXXVIII)
- [ ] 1 Kitáb-i-Aqdas (Kitáb-i-'Ahd)
- [ ] 28 Other Sources (various compilations)
- [ ] 59 No Citation (requires text matching)

## Workflow

### 1. Process by Category
Start with easiest categories first:
1. **Other Sources** (28) - likely have clear citations
2. **Kitáb-i-Aqdas** (1) - single prayer
3. **Gleanings** (5) - needs selection→PIN mapping
4. **No Citation** (59) - text matching to inventory

### 2. For Each Prayer
1. Extract citation/metadata
2. Search inventory for matching PIN
3. Determine if excerpt needs mnemonic
4. Generate SQL UPDATE statement
5. Test assignment
6. Document decision

### 3. Quality Checks
- Verify no duplicate mnemonics
- Check mnemonic is meaningful
- Confirm PIN exists in inventory
- Validate SQL syntax

## Key Reference

### Publication Code Abbreviations
- **PMP** = Prayers and Meditations of Bahá'u'lláh
- **GHA** = Gleanings from the Writings of Bahá'u'lláh  
- **ATB** = Additional Tablets of Bahá'u'lláh
- **BRL_TBUP** = Tablets of Bahá'u'lláh (compilation)

### Phelps Code Format
```
[PREFIX][NUMBER][MNEMONIC]
  BH    09704   ATT

BH/AB/BB = Author prefix
09704 = Document number
ATT = 3-letter mnemonic (for excerpts)
```

### When to Add Mnemonics
- Publication ref ends with 'x' → excerpt → needs mnemonic
- Multiple prayers from same document → each needs unique mnemonic
- Full tablet/prayer → no mnemonic needed

## Next Steps

1. Move completed SQL to `completed/` directory
2. Extract pending prayers to JSON files
3. Start processing "Other Sources" category
4. Document any ambiguous cases
5. Build up PROCESS_LOG as we go

## Files to Clean Up Later

After completion:
- Old review files (`review_*.txt`)
- Consolidated reviews (`consolidated_review_*.txt`)
- Temporary scripts in `/tmp/`
- Failed response files
