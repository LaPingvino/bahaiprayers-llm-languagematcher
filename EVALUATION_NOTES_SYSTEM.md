# Evaluation Layer and Cross-Session Note System

## Overview

This document describes two major enhancements added to the Bahá'í Prayer Matching Tool:

1. **LLM Evaluation Layer**: A second LLM that reviews and validates the first LLM's responses
2. **Cross-Session Persistent Note System**: A database-backed system allowing LLMs to learn from previous sessions

## LLM Evaluation Layer

### Purpose

The evaluation layer addresses the limitation that the primary LLM works with smaller contexts and might make mistakes. A second LLM reviews the first LLM's work with broader context to:

- Validate match quality by comparing prayer text with matched Phelps code
- Improve low-confidence matches through additional analysis
- Catch potential errors in matching logic
- Adjust confidence levels based on thorough evaluation

### How It Works

1. **Primary LLM** processes the prayer and returns initial match (Phelps code + confidence)
2. **Evaluation LLM** receives:
   - Original prayer text
   - Primary LLM's response (code, confidence, reasoning)
   - Actual text of the matched prayer from database for comparison
   - Evaluation criteria and prompts
3. **Evaluation LLM** responds with:
   - `VALID: YES/NO` - Is the original match correct?
   - `IMPROVED_CODE: [code or NONE]` - Better match if available
   - `IMPROVED_CONFIDENCE: [0-100]` - Confidence for improved match
   - `EVALUATION: [explanation]` - Detailed assessment

### Evaluation Logic

```
If evaluation suggests better match with higher confidence:
  → Use improved match

Else if original match is validated:
  → Boost confidence by 10% (max 100%)

Else if original match is questioned:
  → Reduce confidence by 30%

Else:
  → Keep original response
```

### Configuration

- **Threshold**: Only prayers with >100 characters and confidence ≥30% are evaluated
- **Fallback**: If evaluation LLM fails, original response is used
- **Performance**: Evaluation adds ~1-2 seconds per prayer but significantly improves accuracy

### Code Structure

- `callLLMWithEvaluation()` - Main evaluation orchestrator
- `buildEvaluationPrompt()` - Creates evaluation prompt with context
- `parseEvaluationResponse()` - Parses structured evaluation response
- `applyEvaluation()` - Applies evaluation results to final response

## Cross-Session Persistent Note System

### Purpose

The original note system was session-specific and lost all accumulated experience when the program restarted. The new system provides:

- **Persistent Learning**: Notes survive across program restarts
- **Cross-Session Communication**: LLMs can learn from previous sessions
- **Knowledge Accumulation**: Build up expertise over time
- **Pattern Recognition**: Identify successful strategies across sessions

### Database Schema

```sql
CREATE TABLE session_notes (
    id INT AUTO_INCREMENT PRIMARY KEY,
    timestamp DATETIME NOT NULL,
    language VARCHAR(10) NOT NULL,
    note_type VARCHAR(20) NOT NULL,    -- SUCCESS, FAILURE, PATTERN, STRATEGY, TIP
    content TEXT NOT NULL,
    phelps_code VARCHAR(20),           -- Optional, for successful matches
    confidence FLOAT,                  -- Optional, confidence score
    session_id VARCHAR(100) NOT NULL,  -- Unique session identifier
    INDEX idx_language (language),
    INDEX idx_type (note_type),
    INDEX idx_timestamp (timestamp),
    INDEX idx_session (session_id)
);
```

### Note Types

- **SUCCESS**: Successful prayer matches with high confidence
- **FAILURE**: Failed matches or problematic patterns
- **PATTERN**: Observed patterns in prayer structure or content
- **STRATEGY**: Effective matching strategies and techniques
- **TIP**: General advice for improving matching accuracy

### Session Management

Each program run gets a unique session ID:
```
Format: YYYYMMDD_HHMMSS_PID_RANDOM
Example: 20251108_143022_12345_789
```

### Key Functions

- `addSessionNote(lang, type, content, phelps, confidence)` - Add persistent note
- `getRelevantNotes(language)` - Retrieve notes for LLM context (current + previous sessions)
- `searchSessionNotes(query, type, language)` - Search across all sessions
- `removeSessionNotes(type, language, olderThan)` - Clean up notes
- `formatNotesForPrompt(notes)` - Format for LLM consumption

### LLM Integration

Notes are automatically included in LLM prompts:

```
CROSS-SESSION EXPERIENCE NOTES:
Here are insights from all previous LLM sessions (current and past):

✅ SUCCESS (2h ago): Successfully matched using opening phrase [AB00123PRO, confidence: 90%] [CURRENT SESSION]
🔍 PATTERN (1d ago): Prayers with 'divine mercy' often relate to compassion themes [SESSION: 43022_789]
💡 STRATEGY (3d ago): Use combined keyword + length search for better accuracy [SESSION: 28015_234]

Use these cross-session insights to improve your analysis and learn from previous LLM experiences.
```

### Workflow Example

1. **First Session**: LLM discovers that Spanish prayers often start with "Oh Señor"
   ```
   addSessionNote("es", "PATTERN", "Spanish prayers commonly open with 'Oh Señor'", "", 0.0)
   ```

2. **Second Session**: Different LLM session can access this knowledge
   ```
   notes = getRelevantNotes("es")  // Returns pattern from previous session
   // LLM sees the pattern and applies it to new Spanish prayer
   ```

3. **Third Session**: Builds on accumulated knowledge
   ```
   // LLM has access to patterns and strategies from both previous sessions
   ```

## Function Changes for Cross-Session Support

### Enhanced Functions

- `SearchNotesFunction.Execute()` - Now searches across all sessions
- `ClearNotesFunction.Execute()` - Can clear notes by session, type, or age
- `AddNoteFunction.Execute()` - Automatically persists to database

### New LLM Functions Available

All existing note functions now work cross-session:

- `SEARCH_NOTES:query[,type,language]` - Search persistent notes
- `ADD_NOTE:type,content[,phelps,confidence]` - Add persistent note
- `CLEAR_NOTES:[type],[language],[days_old]` - Clean persistent notes

## Performance Considerations

### Database Operations

- **Async Writes**: Note persistence doesn't block main processing
- **Indexed Queries**: Fast retrieval using database indexes
- **Connection Reuse**: Efficient database connection management
- **Batch Operations**: Multiple notes can be processed efficiently

### Memory Management

- **Hybrid Storage**: Recent notes in memory, all notes in database
- **Size Limits**: Memory cache limited to 50 most recent notes
- **Query Limits**: Database queries limited to prevent excessive memory use

### Query Optimization

```sql
-- Optimized query for relevant notes
SELECT timestamp, language, note_type, content, phelps_code, confidence, session_id
FROM session_notes
WHERE language = ? OR language = '' OR note_type IN ('STRATEGY', 'PATTERN')
ORDER BY timestamp DESC
LIMIT 20
```

## Testing

### Unit Tests

- `TestLLMEvaluationLayer` - Tests evaluation logic and confidence adjustments
- `TestParseEvaluationResponse` - Tests parsing of evaluation LLM responses
- `TestCrossSessionNotes` - Tests persistent note storage and retrieval
- `TestSearchCrossSessionNotes` - Tests cross-session note searching
- `TestFormatNotesForPrompt` - Tests LLM prompt formatting

### Integration Tests

Tests verify:
- Database table creation
- Cross-session data persistence
- Session ID generation and tracking
- Note retrieval across multiple sessions
- Evaluation layer integration with main processing

## Benefits

### Improved Accuracy

- **Evaluation Layer**: Reduces false positives and improves match quality
- **Experience Learning**: LLMs get better over time with accumulated knowledge
- **Pattern Recognition**: Systematic identification of successful strategies

### Enhanced Debugging

- **Historical Context**: Debug issues by reviewing previous session notes
- **Strategy Tracking**: See which approaches work best for different languages
- **Performance Analysis**: Track success rates and confidence trends

### Knowledge Preservation

- **No Lost Learning**: Expertise accumulates rather than resets each run
- **Team Knowledge**: Multiple users can benefit from shared session experiences
- **Long-term Improvement**: System gets smarter with each prayer processed

## Configuration Options

### Evaluation Layer

```bash
# Enable/disable evaluation layer for prayers >100 characters
./prayer-matcher -language=es -evaluation=true

# Adjust evaluation threshold
./prayer-matcher -language=es -eval-threshold=0.4
```

### Note System

```bash
# Clear old notes (older than 30 days)
./prayer-matcher -clean-notes=30d

# Disable note persistence (testing only)
./prayer-matcher -notes=false
```

## Migration Notes

### Existing Installations

The system automatically:
1. Creates `session_notes` table on first run
2. Migrates existing in-memory notes (if any)
3. Maintains backward compatibility

### Database Management

```sql
-- Check note system health
SELECT COUNT(*) as total_notes, 
       COUNT(DISTINCT session_id) as sessions,
       MAX(timestamp) as latest_note 
FROM session_notes;

-- Clean old notes (optional maintenance)
DELETE FROM session_notes WHERE timestamp < DATE_SUB(NOW(), INTERVAL 90 DAY);
```

## Future Enhancements

### Planned Features

- **Note Classification**: ML-based categorization of note importance
- **Success Metrics**: Automatic tracking of strategy effectiveness
- **Note Summarization**: LLM-generated summaries of accumulated knowledge
- **Cross-Language Learning**: Share patterns between related languages

### API Extensions

- **REST API**: External access to accumulated knowledge
- **Export/Import**: Backup and share session knowledge
- **Analytics Dashboard**: Visual representation of learning progress