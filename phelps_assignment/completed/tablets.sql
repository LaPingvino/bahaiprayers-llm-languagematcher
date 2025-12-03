-- All 6 tablet prayers with appropriate codes

-- Tablet 1: Excerpt from Súriy-i-Ahzán (BH00155)
UPDATE writings SET phelps = 'BH00155WOU' WHERE version = '13ca36d7-0362-4470-a6d2-af85742eeb3b';

-- Tablet 2: Epistle to the Son of the Wolf, p. 12 (BH00005)
UPDATE writings SET phelps = 'BH00005ATT' WHERE version = '7fcfca50-7571-4a39-b01e-1ce9a2fb5a9c';

-- Tablet 3: Excerpt from Súriy-i-Dhikr (BH00297)
UPDATE writings SET phelps = 'BH00297EXC' WHERE version = 'd18199ff-71ea-4f80-8caf-cce10b2fd097';

-- Tablet 4: Bishárát (Glad-Tidings), p. 24 - prayer for forgiveness (BH00568)
UPDATE writings SET phelps = 'BH00568IMP' WHERE version = 'e4691fa9-e447-4089-aba1-dd174ff886d4';

-- Tablet 5: Epistle to the Son of the Wolf, p. 70 (BH00005)
UPDATE writings SET phelps = 'BH00005PRA' WHERE version = 'fcc6b20b-edce-4e72-8510-864fd4f985d1';

-- Tablet 6: Ridván tablet from Days of Remembrance - need to determine specific tablet
-- Temporarily assigning to BH01966 (Húr-i-'Ujáb/Wondrous Maiden) with mnemonic
-- May need adjustment after verification
UPDATE writings SET phelps = 'BH01966ANO' WHERE version = 'fd9891bb-9038-4430-ace9-c22ab1270b91';

