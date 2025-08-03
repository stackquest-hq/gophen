package gophen

import (
	"embed"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/transform"
)

var dictionaries embed.FS
var hdcache = make(map[string]*HyphDict)

var (
	parseHex = regexp.MustCompile(`\^{2}([0-9a-fA-F]{2})`)
	parse    = regexp.MustCompile(`(\d?)(\D?)`)
)

var ignored = []string{"%", "#", "LEFTHYPHENMIN", "RIGHTHYPHENMIN", "COMPOUNDLEFTHYPHENMIN", "COMPOUNDRIGHTHYPHENMIN"}

var LANGUAGES = make(map[string]string)
var languagesLowercase = make(map[string]string)

func init() {
	if err := populateLanguages(); err != nil {
		panic(fmt.Sprintf("Failed to populate languages: %v", err))
	}
}

func populateLanguages() error {
	dirEntries, err := fs.ReadDir(dictionaries, "dictionaries")
	if err != nil {
		return err
	}
	sort.Slice(dirEntries, func(i, j int) bool {
		return dirEntries[i].Name() < dirEntries[j].Name()
	})
	for _, entry := range dirEntries {
		if strings.HasSuffix(entry.Name(), ".dic") {
			name := entry.Name()[5 : len(entry.Name())-4] // Remove "hyph_" prefix and ".dic" suffix
			path := fmt.Sprintf("dictionaries/%s.txt", name)
			shortName := strings.Split(name, "_")[0]
			if _, ok := LANGUAGES[shortName]; !ok {
				LANGUAGES[shortName] = path
			}
		}
	}
	for name := range LANGUAGES {
		languagesLowercase[strings.ToLower(name)] = name
	}
	return nil
}

// LanguageFallback gets a fallback language available in our dictionaries.
func LanguageFallback(language string) string {
	parts := strings.Split(strings.ReplaceAll(strings.ToLower(language), "-", "_"), "_")
	for len(parts) > 0 {
		lang := strings.Join(parts, "_")
		if name, ok := languagesLowercase[lang]; ok {
			return name
		}
		parts = parts[:len(parts)-1]
	}
	return ""
}

// hyphDataInt is an int with associated data, similar to Python's DataInt.
type hyphDataInt struct {
	value int
	data  []string
}

// AlternativeParser is a parser of nonstandard hyphen pattern alternatives.
type AlternativeParser struct {
	change string
	index  int
	cut    int
}

// newAlternativeParser creates a new AlternativeParser.
func newAlternativeParser(pattern, alternative string) *AlternativeParser {
	parts := strings.Split(alternative, ",")
	p := &AlternativeParser{
		change: parts[0],
		index:  atoi(parts[1]),
		cut:    atoi(parts[2]),
	}
	if strings.HasPrefix(pattern, ".") {
		p.index++
	}
	return p
}

// Parse returns an int or a hyphDataInt based on the value.
func (p *AlternativeParser) Parse(value int) interface{} {
	p.index--
	if value&1 == 1 {
		return hyphDataInt{
			value: value,
			data:  []string{p.change, strconv.Itoa(p.index), strconv.Itoa(p.cut)},
		}
	}
	return value
}

// HyphDict holds hyphenation patterns.
type HyphDict struct {
	patterns map[string]struct {
		start  int
		values []interface{}
	}
	cache  map[string][]hyphDataInt
	maxlen int
}

// NewHyphDict reads a hyph_*.dic file and parses its patterns.
func NewHyphDict(path string) (*HyphDict, error) {
	hd := &HyphDict{
		patterns: make(map[string]struct {
			start  int
			values []interface{}
		}),
		cache: make(map[string][]hyphDataInt),
	}

	file, err := dictionaries.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open dictionary file %s: %w", path, err)
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("failed to read dictionary file %s: %w", path, err)
	}

	lines := strings.Split(string(data), "\n")
	if len(lines) < 2 {
		return nil, fmt.Errorf("invalid dictionary file format: %s", path)
	}

	// First line is encoding, handle as in Python
	encoding := strings.ToLower(strings.TrimSpace(lines[0]))
	if encoding == "microsoft-cp1251" {
		// Convert CP1251 encoded data to UTF-8
		decoder := charmap.Windows1251.NewDecoder()
		var convertedLines []string
		convertedLines = append(convertedLines, lines[0]) // Keep the encoding line as-is

		for _, line := range lines[1:] {
			utf8Line, _, err := transform.String(decoder, line)
			if err != nil {
				// If conversion fails, use the original line
				convertedLines = append(convertedLines, line)
			} else {
				convertedLines = append(convertedLines, utf8Line)
			}
		}
		lines = convertedLines
	}

	var maxlen int
	for _, pattern := range lines[1:] {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}

		isIgnored := false
		for _, prefix := range ignored {
			if strings.HasPrefix(pattern, prefix) {
				isIgnored = true
				break
			}
		}
		if isIgnored {
			continue
		}

		// Replace ^^hh with the real character.
		pattern = parseHex.ReplaceAllStringFunc(pattern, func(s string) string {
			hexVal, _ := strconv.ParseInt(s[2:], 16, 64)
			return string(rune(hexVal))
		})

		var factory func(value int) interface{} = func(value int) interface{} { return value }
		if strings.Contains(pattern, "/") && strings.Contains(pattern, "=") {
			parts := strings.SplitN(pattern, "/", 2)
			pattern = parts[0]
			alternative := parts[1]
			ap := newAlternativeParser(pattern, alternative)
			factory = ap.Parse
		}

		matches := parse.FindAllStringSubmatch(pattern, -1)
		var tags []string
		var values []interface{}

		for _, match := range matches {
			i := match[1]
			s := match[2]
			tags = append(tags, s)
			val, _ := strconv.Atoi(i)
			values = append(values, factory(val))
		}

		hasNonZero := false
		for _, v := range values {
			if i, ok := v.(int); ok && i > 0 {
				hasNonZero = true
				break
			}
			if _, ok := v.(hyphDataInt); ok {
				hasNonZero = true
				break
			}
		}
		if !hasNonZero {
			continue
		}

		start, end := 0, len(values)
		for {
			val, ok := values[start].(int)
			if !ok || val != 0 {
				break
			}
			start++
		}
		for {
			val, ok := values[end-1].(int)
			if !ok || val != 0 {
				break
			}
			end--
		}

		if len(tags) > maxlen {
			maxlen = len(tags)
		}

		hd.patterns[strings.Join(tags, "")] = struct {
			start  int
			values []interface{}
		}{
			start:  start,
			values: values[start:end],
		}
	}
	hd.maxlen = maxlen
	return hd, nil
}

// Positions gets a list of positions where the word can be hyphenated.
func (hd *HyphDict) Positions(word string) []hyphDataInt {
	word = strings.ToLower(word)
	if points, ok := hd.cache[word]; ok {
		return points
	}

	pointedWord := "." + word + "."
	references := make([]interface{}, utf8.RuneCountInString(pointedWord)+1)

	for i := 0; i < utf8.RuneCountInString(pointedWord)-1; i++ {
		stop := min(i+hd.maxlen, utf8.RuneCountInString(pointedWord)) + 1
		for j := i + 1; j < stop; j++ {
			subWord := pointedWord[byteIndex(pointedWord, i):byteIndex(pointedWord, j)]
			pattern, ok := hd.patterns[subWord]
			if !ok {
				continue
			}

			offset, values := pattern.start, pattern.values
			for k, v := range values {
				idx := i + offset + k

				// Max logic
				var current int
				if currentRef, ok := references[idx].(int); ok {
					current = currentRef
				} else if currentRef, ok := references[idx].(hyphDataInt); ok {
					current = currentRef.value
				}

				var patternVal int
				var data []string
				if patternRef, ok := v.(int); ok {
					patternVal = patternRef
				} else if patternRef, ok := v.(hyphDataInt); ok {
					patternVal = patternRef.value
					data = patternRef.data
				}

				if patternVal > current {
					if data != nil {
						references[idx] = hyphDataInt{value: patternVal, data: data}
					} else {
						references[idx] = patternVal
					}
				}
			}
		}
	}

	var points []hyphDataInt
	for i, ref := range references {
		val, isInt := ref.(int)
		if isInt && val%2 != 0 {
			points = append(points, hyphDataInt{value: i - 1})
		}
		if hdi, isDataInt := ref.(hyphDataInt); isDataInt && hdi.value%2 != 0 {
			points = append(points, hyphDataInt{value: i - 1, data: hdi.data})
		}
	}

	hd.cache[word] = points
	return points
}

// Helper functions
func atoi(s string) int {
	i, _ := strconv.Atoi(s)
	return i
}

func byteIndex(s string, runeIndex int) int {
	return len([]byte(s)) - len([]byte(s[runeIndex:]))
}
