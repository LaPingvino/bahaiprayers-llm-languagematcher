# Quick Start Guide

Get up to speed quickly on the Phelps code assignment project.

## What's This About?

We're assigning unique Phelps codes (like BH09704 or AB00527) to 117 English prayers in the database that currently don't have codes.

## Current Status

**24 down, 93 to go!** (20.5% complete)

✅ Tablets (6) and Prayers & Meditations (18) are done.
⏳ Next up: Other Sources (28), Kitáb-i-Aqdas (1), Gleanings (5), No Citations (59)

## File Guide

| File | What It Is |
|------|------------|
| **STATUS.md** | 📊 Progress dashboard - start here |
| **README.md** | 📖 Detailed workflow and structure |
| **docs/PROCESS_LOG.md** | 📝 Detailed decisions and learnings |
| **docs/PUBLICATION_CODES.md** | 📚 Reference for PMP, GHA, etc. |
| **completed/*.sql** | ✅ Executed SQL updates |
| **pending/prayers_categorized.json** | 📦 All 117 prayers organized by type |

## How to Continue Work

### 1. Check Current Status
```bash
cd phelps_assignment
cat STATUS.md
```

### 2. Process Next Category
Example for "Other Sources" (28 prayers):

```bash
# Extract prayers from JSON
python3 << 'SCRIPT'
import json
with open('pending/prayers_categorized.json') as f:
    data = json.load(f)
    other_sources = data['8_Other_Sources']
    # Work with these prayers...
SCRIPT
```

### 3. Search Inventory for PINs
```bash
cd ../bahaiwritings
dolt sql -q "SELECT PIN, Title FROM inventory WHERE Title LIKE '%YourSearchTerm%'"
```

### 4. Generate SQL
Create file in `working/category_name.sql` with UPDATE statements

### 5. Execute Updates
```bash
dolt sql < working/category_name.sql
```

### 6. Move to Completed
```bash
mv working/category_name.sql completed/
```

### 7. Update STATUS.md
Mark category as complete and update progress bars

## Key Commands

### Check Database Status
```bash
cd bahaiwritings
dolt sql -q "SELECT COUNT(*) FROM writings WHERE language='en' AND (phelps IS NULL OR phelps='')"
```

### Search for Publication Codes
```bash
dolt sql -q "SELECT * FROM inventory_publications WHERE publication_ref LIKE 'PMP%' LIMIT 10"
```

### Find PIN by Title
```bash
dolt sql -q "SELECT PIN, Title FROM inventory WHERE Title LIKE '%Glad%'"
```

## Common Patterns

### Full Prayer (No Mnemonic)
```sql
UPDATE writings SET phelps = 'BH09704' WHERE version = 'uuid-here';
```

### Excerpt (Needs Mnemonic)
```sql
UPDATE writings SET phelps = 'BH00155WOU' WHERE version = 'uuid-here';
```

### Mnemonic Creation
1. Take first 2-3 meaningful words from prayer
2. Extract first letter of each: "Would Utter..." → WOU
3. Pad to 3 letters if needed

## Need Help?

- **Stuck on a citation?** → Try web search or check PUBLICATION_CODES.md
- **Database read-only?** → Wait a few seconds and retry
- **Not sure about mnemonic?** → Document it in PROCESS_LOG.md for review
- **Can't find PIN?** → May need to create new one (document in PROCESS_LOG.md)

## Quick Reference

**Phelps Format**: `[PREFIX][NUMBER][MNEMONIC]`
- PREFIX: BH (Bahá'u'lláh), AB ('Abdu'l-Bahá), BB (Báb)
- NUMBER: 5 digits (00001-99999)
- MNEMONIC: 3 letters (optional, for excerpts)

**Publication Codes**:
- PMP = Prayers and Meditations
- GHA = Gleanings
- ESW = Epistle to Son of Wolf
- (See PUBLICATION_CODES.md for full list)

---
**Ready to continue?** Check STATUS.md for next priority!
