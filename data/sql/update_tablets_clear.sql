-- Clear tablet matches with mnemonics

-- Tablet 1: Excerpt from Súriy-i-Ahzán (BH00155)
UPDATE writings SET phelps = 'BH00155WOU' WHERE version = '13ca36d7-0362-4470-a6d2-af85742eeb3b';

-- Tablet 2: Epistle to the Son of the Wolf, p. 12 (BH00005)  
UPDATE writings SET phelps = 'BH00005ATT' WHERE version = '7fcfca50-7571-4a39-b01e-1ce9a2fb5a9c';

-- Tablet 5: Epistle to the Son of the Wolf, p. 70 (BH00005)
UPDATE writings SET phelps = 'BH00005PRA' WHERE version = 'fcc6b20b-edce-4e72-8510-864fd4f985d1';

