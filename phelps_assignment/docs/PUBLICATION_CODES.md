# Publication Code Reference

Quick reference for common publication abbreviations found in `inventory_publications`.

## Bahá'u'lláh's Writings

| Code | Full Title | Notes |
|------|------------|-------|
| **PMP** | Prayers and Meditations | 184 numbered prayers |
| **GHA** | Gleanings from the Writings of Bahá'u'lláh | Uses page numbers |
| **ATB** | Additional Tablets of Bahá'u'lláh | |
| **ESW** | Epistle to the Son of the Wolf | Also as BH00005 |
| **TB** / **BRL_TBUP** | Tablets of Bahá'u'lláh Revealed After the Kitáb-i-Aqdas | |
| **KA** | Kitáb-i-Aqdas | The Most Holy Book |
| **KI** | Kitáb-i-Íqán | Book of Certitude |
| **HW** | Hidden Words | |
| **SV** | Seven Valleys | |
| **FV** | Four Valleys | |

## 'Abdu'l-Bahá's Writings

| Code | Full Title | Notes |
|------|------------|-------|
| **SAQ** | Some Answered Questions | |
| **SDC** | Secret of Divine Civilization | |
| **TAB** | Tablets of 'Abdu'l-Bahá | |
| **WT** / **WTP** | Will and Testament | AB00001 |
| **TDP** | Tablets of the Divine Plan | |

## Compilations

| Code | Full Title | Notes |
|------|------------|-------|
| **BP** | Bahá'í Prayers | Various editions |
| **DOR** | Days of Remembrance | 2017 compilation |
| **CC** | Compilation of Compilations | |

## Format Notes

### Number Suffixes
- **#NNN** = Item number (e.g., `PMP#055` = Prayer #55)
- **.NNN** = Page number (e.g., `GHA.138` = Page 138)
- **x suffix** = Excerpt only (e.g., `PMP#002x` = excerpt from Prayer #2)

### Examples
```
PMP#017      → Full Prayer #17 from Prayers & Meditations
PMP#153x     → Excerpt from Prayer #153
GHA.138x     → Excerpt from Gleanings page 138
ATB.116      → Additional Tablets page 116
```

## Finding PINs

### SQL Queries

```sql
-- Find PIN for specific publication reference
SELECT i.PIN, i.Title 
FROM inventory i 
JOIN inventory_publications p ON i.PIN = p.PIN 
WHERE p.publication_ref = 'PMP#055';

-- Find all references for a PIN
SELECT p.publication_ref 
FROM inventory_publications p 
WHERE p.PIN = 'BH09704';

-- Search by title
SELECT PIN, Title 
FROM inventory 
WHERE Title LIKE '%Glad-Tidings%';
```

## Common PIN Ranges

| Prefix | Range | Content |
|--------|-------|---------|
| BH00001-BH11924 | Standard tablets/prayers by Bahá'u'lláh |
| BHU0001-BHU0073 | Utterances by Bahá'u'lláh |
| AB00001-AB12999 | Tablets/prayers by 'Abdu'l-Bahá |
| ABU0001-ABU3765 | Utterances by 'Abdu'l-Bahá |
| BB00001-BB00999 | Writings of the Báb |

## Last Updated
2025-11-29
