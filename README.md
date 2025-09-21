# Bahá'í Prayers LLM Language Matcher

A sophisticated tool that uses Large Language Models (LLMs) to automatically match prayers in different languages to their corresponding Phelps codes in the Bahá'í writings database.

## Overview

This Go application integrates with both Gemini CLI and Ollama to provide intelligent prayer matching capabilities. It analyzes prayer texts and attempts to identify them using known Phelps codes, with configurable confidence thresholds for quality assurance.

## Features

- **Dual LLM Support**: Primary integration with Gemini CLI, with automatic fallback to Ollama
- **Configurable Models**: Support for different Ollama models (default: gpt-oss)
- **Smart Language Defaults**: Automatically selects language with fewest missing prayers for optimal processing
- **Reference Language Support**: Use prayer names and codes from one language to match prayers in another
- **Language-Specific Processing**: Target specific languages for processing
- **Confidence-Based Routing**: High confidence matches update the database directly, low confidence matches go to candidates table
- **Interactive Assignment**: Manual assignment interface for prayers that couldn't be automatically matched
- **Comprehensive Reporting**: Detailed reports with language statistics and processing summaries
- **Database Integration**: Full integration with Dolt version-controlled database
- **Automatic Commits**: Changes are automatically committed to Dolt with descriptive messages

## Prerequisites

- Go 1.25.1 or later
- [Dolt](https://github.com/dolthub/dolt) database
- [Gemini CLI](https://github.com/replit/gemini-cli) (optional, will fall back to Ollama)
- [Ollama](https://ollama.ai/) with desired models installed

## Installation

```bash
git clone https://github.com/LaPingvino/bahaiprayers-llm-languagematcher
cd bahaiprayers-llm-languagematcher
go build -o prayer-matcher .
```

## Usage

### Basic Usage

Process English prayers using default settings:
```bash
./prayer-matcher
```

### Advanced Usage

```bash
# Process Spanish prayers
./prayer-matcher -language=es

# Use French prayers with English reference codes
./prayer-matcher -language=fr -reference=en

# Use only Ollama (skip Gemini)
./prayer-matcher -language=fr -gemini=false

# Use a different Ollama model
./prayer-matcher -language=de -model=llama2

# Show full raw LLM responses at the end (helpful for debugging)
./prayer-matcher -language=es -show-raw

# Disable interactive mode for unmatched prayers
./prayer-matcher -language=es -interactive=false

# Custom report file location
./prayer-matcher -language=en -report=/path/to/custom_report.txt
```

### Command Line Options

| Flag | Default | Description |
|------|---------|-------------|
| `-language` | *auto-detect* | Target language code to process (auto-selects language with fewest missing prayers) |
| `-reference` | `en` | Reference language for Phelps codes and prayer names |
| `-gemini` | `true` | Use Gemini CLI (falls back to Ollama if failed) |
| `-model` | `gpt-oss` | Ollama model to use |
| `-interactive` | `true` | Enable interactive assignment for unmatched prayers |
| `-report` | `prayer_matching_report.txt` | Path for the detailed report file |
| `-show-raw` | `false` | Show full raw LLM responses at the end (normally truncated to avoid screen clutter) |
| `-help` | `false` | Show help message |

## How It Works

1. **Database Loading**: Loads the complete Bahá'í writings database from Dolt
2. **Smart Language Selection**: Auto-detects optimal target language (fewest missing Phelps codes) if not specified
3. **Header Preparation**: Creates an LLM prompt header with Phelps codes and names from reference language
4. **Prayer Processing**: For each prayer in the target language without a Phelps code:
   - Sends the prayer text to the LLM with matching instructions
   - Parses the response for Phelps code, confidence, and reasoning
5. **Confidence-Based Action**:
   - **High Confidence (≥70%)**: Updates the `writings` table directly with the Phelps code
   - **Low Confidence (<70%)**: Adds to `prayer_match_candidates` table for manual review
   - **Unknown/No Match**: Queued for interactive assignment
6. **Interactive Assignment**: Prompts user to manually assign Phelps codes to unmatched prayers
7. **Reporting**: Generates detailed reports of all activities including statistics by language
8. **Version Control**: Commits all changes to Dolt with descriptive messages

## Database Schema

### Core Tables

- **`writings`**: Main prayer texts with Phelps codes, language, and content
- **`prayer_match_candidates`**: Low-confidence matches awaiting manual validation
- **`prayer_heuristics`**: Pattern-based matching rules
- **`languages`**: Language code mappings

### Key Fields

- **Phelps Code**: Unique identifier for each prayer (e.g., `AB00001FIR`)
- **Confidence Score**: LLM-assigned confidence (0.0 to 1.0)
- **Validation Status**: Manual review status for candidates

## LLM Integration

### Response Format

The LLM receives a context-rich prompt containing all known Phelps codes with their names from the reference language, then responds in this format:
```
Phelps: [CODE or UNKNOWN]
Confidence: [0-100]
Reasoning: [Explanation]
```

### Example LLM Response

```
Phelps: AB00001FIR
Confidence: 85
Reasoning: This prayer contains the distinctive opening phrase "O Thou Who art the Lord of all names" and the characteristic supplicatory style of the Fire Tablet, making it a strong match.
```

### Confidence Thresholds

- **≥70%**: Direct database update (high confidence)
- **<70%**: Candidate table entry (requires review)
- **0%**: No match found or UNKNOWN response (queued for interactive assignment)

## Report Generation

Each run generates a comprehensive report including:

- Processing timestamp and configuration
- Auto-selected language information and missing prayer statistics
- Database loading statistics
- Per-prayer analysis results
- Summary statistics (processed, matched, candidates, unmatched)
- Interactive assignment session logs
- Error logs and debugging information
- Dolt commit information

### Sample Report Output

```
Prayer Matching Report
=====================
Started: 2025-09-21T18:30:00Z
Target Language: es
Reference Language: en
Using Gemini: true
Ollama Model: gpt-oss
Interactive Mode: true

Database loaded successfully
Database size: 9000 writings, 45 languages, 12 heuristics, 89 candidates

Auto-selected target language: es
Missing prayers by language:
  es: 89 <- SELECTED
  fr: 156
  de: 203
  pt: 267
  ... and 15 more languages

=== Processing prayers for language: es (reference: en) ===
Started at: 2025-09-21T18:30:05Z

Processing writing: Oración Matutina (Version: ES_MP_001)
  LLM Response - Phelps: AB00044PRO, Confidence: 87.0%, Reasoning: Clear match based on opening invocation
  MATCHED: Updated writings table with Phelps code AB00044PRO

Processing writing: Oración Desconocida (Version: ES_UK_002)
  LLM Response - Phelps: UNKNOWN, Confidence: 0.0%, Reasoning: No clear match found in database
  UNMATCHED: Will prompt for interactive assignment

Summary for es:
  Processed: 89 prayers
  High confidence matches: 67
  Low confidence candidates: 15
  Unmatched (for interactive): 7
Completed at: 2025-09-21T18:42:30Z
  SUCCESS: Changes committed to Dolt with message: LLM prayer matching for es: 67 matches, 15 candidates

=== Interactive Assignment Session ===
Started at: 2025-09-21T18:42:35Z
  ASSIGNED: AB00065KIN -> Oración Desconocida (Version: ES_UK_002)
  SKIPPED: Prayer without clear match (Version: ES_UK_005)
Interactive assignment completed. Assigned: 5, Skipped: 2
  SUCCESS: Interactive changes committed: Interactive prayer assignment: 5 prayers assigned
```

## Testing

Run the comprehensive test suite:

```bash
go test -v
```

### Test Coverage

- LLM response parsing
- Header preparation
- Confidence threshold logic
- SQL injection prevention
- Database structure validation
- Mock LLM integration

## Error Handling

- **Graceful Fallback**: Automatic fallback from Gemini to Ollama
- **SQL Injection Protection**: All user inputs are properly escaped
- **Comprehensive Logging**: All errors logged to report file
- **Rate Limiting**: Built-in delays to avoid overwhelming LLM services

## Performance Considerations

- **Batch Processing**: Processes prayers sequentially with rate limiting
- **Memory Efficient**: Loads database once, processes in memory
- **Resumable**: Can be run multiple times safely (skips already matched prayers)

## Contributing

1. Fork the repository
2. Create a feature branch
3. Add comprehensive tests for new functionality
4. Ensure all tests pass: `go test -v`
5. Submit a pull request

## License

This project is licensed under the same terms as specified in the LICENSE file.

## Troubleshooting

### Common Issues

**Gemini CLI not found:**
```bash
# Install Gemini CLI or use Ollama-only mode
./prayer-matcher -gemini=false
```

**Ollama model not available:**
```bash
# List available models
ollama list

# Pull required model
ollama pull gpt-oss
```

**Dolt database issues:**
```bash
# Initialize database if needed
dolt init
dolt sql < schema.sql
```

**Interactive mode issues:**
```bash
# Skip interactive mode if running in automated environment
./prayer-matcher -interactive=false
```

### Debug Mode

Enable verbose logging by examining the generated report file for detailed error information, processing steps, and language selection statistics.

## Support

For issues and questions, please create an issue in the GitHub repository with:
- Command used
- Error messages
- Relevant portions of the report file
- System information (OS, Go version, etc.)