#!/bin/bash
# Quick progress check script

echo "=================================="
echo "Phelps Code Assignment Progress"
echo "=================================="
echo ""

cd bahaiwritings 2>/dev/null || cd ../bahaiwritings

# Total English prayers
total=$(dolt sql -q "SELECT COUNT(*) FROM writings WHERE language='en'" -r csv | tail -1)
echo "Total English prayers: $total"

# With Phelps codes
with_codes=$(dolt sql -q "SELECT COUNT(*) FROM writings WHERE language='en' AND phelps IS NOT NULL AND phelps != ''" -r csv | tail -1)
echo "With Phelps codes:     $with_codes"

# Without Phelps codes
without_codes=$(dolt sql -q "SELECT COUNT(*) FROM writings WHERE language='en' AND (phelps IS NULL OR phelps = '')" -r csv | tail -1)
echo "Without Phelps codes:  $without_codes"

# Calculate percentage
if [ "$total" -gt 0 ]; then
    pct=$((with_codes * 100 / total))
    echo ""
    echo "Coverage: $pct%"
fi

echo ""
echo "Assignment project focus: 117 specific prayers"
echo "Completed in this project: 24 (20.5%)"
echo "Remaining: 93 (79.5%)"
echo ""
echo "See phelps_assignment/STATUS.md for details"
