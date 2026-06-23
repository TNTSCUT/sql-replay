// json_replay_route: route JSONL records by `dbname` field to per-dbname replay files.
//
// Reads every .json file in the input directory (sorted by filename), extracts the
// `dbname` field from each line using regex (no json.Unmarshal for speed),
// and appends the raw line to <prefix><dbname>.json in the output directory.
//
// Output files use JSONL format: one JSON object per line, written verbatim.

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var dbnameRe = regexp.MustCompile(`"dbname"\s*:\s*"((?:[^"\\]|\\.)*)"`)

func sanitize(name string) string {
	if name == "" {
		return "__empty_dbname__"
	}
	s := strings.NewReplacer("/", "_", "\\", "_", "\x00", "").Replace(name)
	if s == "" {
		return "__empty_dbname__"
	}
	return s
}

// unescapeJSONString reverses JSON string escaping (\" \\ \n \t \uXXXX etc).
func unescapeJSONString(s string) string {
	var v string
	if err := json.Unmarshal([]byte(`"`+s+`"`), &v); err == nil {
		return v
	}
	return s
}

func formatDuration(seconds float64) string {
	if seconds < 0 || seconds != seconds { // NaN check
		return "?"
	}
	s := int(seconds)
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	if s < 3600 {
		return fmt.Sprintf("%dm %ds", s/60, s%60)
	}
	h := s / 3600
	m := (s % 3600) / 60
	sec := s % 60
	return fmt.Sprintf("%dh %dm %ds", h, m, sec)
}

func formatBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	if n < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	}
	if n < 1024*1024*1024 {
		return fmt.Sprintf("%.1f MB", float64(n)/1024/1024)
	}
	return fmt.Sprintf("%.2f GB", float64(n)/1024/1024/1024)
}

// extractNumericPrefix returns the leading integer in the filename stem
// (the .json suffix stripped). If the filename has no numeric prefix,
// it returns math.MaxInt64 so such files sort after numeric ones.
// The second return value is the original name, used as a stable tiebreaker.
func extractNumericPrefix(name string) (int64, string) {
	stem := strings.TrimSuffix(name, ".json")
	i := 0
	for i < len(stem) && stem[i] >= '0' && stem[i] <= '9' {
		i++
	}
	if i == 0 {
		return math.MaxInt64, name
	}
	n, err := strconv.ParseInt(stem[:i], 10, 64)
	if err != nil {
		return math.MaxInt64, name
	}
	return n, name
}

// Progress is a lightweight single-line progress bar to stderr, throttled to 5 Hz.
type Progress struct {
	total, count int64
	start        time.Time
	lastPrint    time.Time
	enabled      bool
	prefix       string
	lastLen      int
}

func NewProgress(total int64, enabled bool, prefix string) *Progress {
	now := time.Now()
	return &Progress{
		total:     total,
		enabled:   enabled,
		prefix:    prefix,
		start:     now,
		lastPrint: now,
	}
}

func (p *Progress) Update(n int64) {
	p.count += n
	if !p.enabled {
		return
	}
	now := time.Now()
	if now.Sub(p.lastPrint) < 200*time.Millisecond {
		return
	}
	p.lastPrint = now
	p.render()
}

func (p *Progress) render() {
	width := 30
	elapsed := time.Since(p.start).Seconds()
	rate := 0.0
	if elapsed > 0 {
		rate = float64(p.count) / elapsed
	}

	var msg string
	if p.total > 0 {
		pct := float64(p.count) / float64(p.total)
		filled := int(float64(width) * pct)
		if filled > width {
			filled = width
		}
		if filled < 0 {
			filled = 0
		}
		bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
		eta := 0.0
		if rate > 0 {
			eta = float64(p.total-p.count) / rate
		}
		msg = fmt.Sprintf("\r%s |%s| %6.1f%% %d/%d [%7.0f/s ETA %s]",
			p.prefix, bar, pct*100, p.count, p.total, rate, formatDuration(eta))
	} else {
		// No total known (--no-line-count): show count + rate, no bar fill / ETA
		bar := strings.Repeat("░", width)
		msg = fmt.Sprintf("\r%s |%s| %d lines [%7.0f/s]",
			p.prefix, bar, p.count, rate)
	}
	fmt.Fprint(os.Stderr, msg+strings.Repeat(" ", max(0, p.lastLen-len(msg))))
	p.lastLen = len(msg)
}

func (p *Progress) Close() {
	if !p.enabled {
		return
	}
	p.render()
	fmt.Fprintln(os.Stderr)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// countLines counts lines in a file. bufio.Scanner splits on '\n' and counts
// a final non-terminated line at EOF, so no edge-case handling is needed.
func countLines(path string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	var n int64
	for sc.Scan() {
		n++
	}
	return n
}

// RouteJSONReplay routes JSONL records by `dbname` field to per-dbname replay files.
func RouteJSONReplay(inDir, outputDir, prefix string, dryRun, skipNoDBname, skipSelf, noProgress, noLineCount, quiet bool) {
	inDir, err := filepath.Abs(inDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: invalid input dir: %v\n", err)
		return
	}
	outDir := inDir
	if outputDir != "" {
		outDir, err = filepath.Abs(outputDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: invalid output dir: %v\n", err)
			return
		}
	}

	if info, err := os.Stat(inDir); err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "ERROR: input directory not found: %s\n", inDir)
		return
	}
	if err := os.MkdirAll(outDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: cannot create output dir %s: %v\n", outDir, err)
		return
	}

	// List .json files (excluding dirs)
	entries, err := os.ReadDir(inDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: read dir %s: %v\n", inDir, err)
		return
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			files = append(files, e.Name())
		}
	}
	if len(files) == 0 {
		fmt.Fprintf(os.Stderr, "ERROR: no .json files in %s\n", inDir)
		return
	}
	sort.Slice(files, func(i, j int) bool {
		ni, si := extractNumericPrefix(files[i])
		nj, sj := extractNumericPrefix(files[j])
		if ni != nj {
			return ni < nj
		}
		return si < sj // tie-break lexicographically for stability
	})

	// Skip self-write risk
	outputPattern := regexp.MustCompile("^" + regexp.QuoteMeta(prefix) + `.+\.json$`)
	var skippedSelf []string
	if skipSelf {
		filtered := files[:0]
		for _, f := range files {
			if outputPattern.MatchString(f) {
				skippedSelf = append(skippedSelf, f)
			} else {
				filtered = append(filtered, f)
			}
		}
		files = filtered
	}
	if len(files) == 0 {
		fmt.Fprintf(os.Stderr, "ERROR: no input files after self-skip. Skipped: %v\n", skippedSelf)
		return
	}

	if !quiet {
		fmt.Printf("[INFO] input_dir  = %s\n", inDir)
		fmt.Printf("[INFO] output_dir = %s\n", outDir)
		fmt.Printf("[INFO] input files (%d):\n", len(files))
		for i, f := range files {
			if i < 10 {
				fmt.Printf("           - %s\n", f)
			}
		}
		if len(files) > 10 {
			fmt.Printf("           ... and %d more\n", len(files)-10)
		}
		if len(skippedSelf) > 0 {
			fmt.Printf("[INFO] skipped (self-write risk): %v\n", skippedSelf)
		}
	}

	// Pre-pass: cache file sizes; count total lines unless --no-line-count
	progressEnabled := !(noProgress || dryRun)
	var totalLines int64
	fileSizes := make(map[string]int64, len(files))
	if progressEnabled {
		for _, fname := range files {
			path := filepath.Join(inDir, fname)
			info, err := os.Stat(path)
			if err != nil {
				continue
			}
			fileSizes[fname] = info.Size()
			if !noLineCount {
				totalLines += countLines(path)
			}
		}
	}

	bar := NewProgress(totalLines, progressEnabled, "Routing")
	defer bar.Close()

	// Output file handles (one per dbname), each with its own bufio.Writer
	type fileEntry struct {
		fh   *os.File
		buf  *bufio.Writer
		path string
	}
	fileHandles := make(map[string]*fileEntry)
	defer func() {
		for _, e := range fileHandles {
			if e != nil && e.buf != nil {
				e.buf.Flush()
			}
			if e != nil && e.fh != nil {
				e.fh.Close()
			}
		}
	}()

	lineCounts := make(map[string]int)
	var skippedLines int64
	var totalReadLines int64

	openOutput := func(safeName string) (*fileEntry, error) {
		outPath := filepath.Join(outDir, prefix+safeName+".json")
		fh, err := os.OpenFile(outPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return nil, err
		}
		// 1 MB buffer dramatically reduces syscall count for many small writes
		return &fileEntry{fh: fh, buf: bufio.NewWriterSize(fh, 1024*1024), path: outPath}, nil
	}

	getOrOpen := func(safeName string) (*fileEntry, error) {
		if e, ok := fileHandles[safeName]; ok {
			return e, nil
		}
		e, err := openOutput(safeName)
		if err != nil {
			return nil, err
		}
		fileHandles[safeName] = e
		return e, nil
	}

	// Write a line to the appropriate output, opening if needed.
	writeLine := func(safeName, line string) {
		e, err := getOrOpen(safeName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "WARN: cannot open output %s: %v\n", safeName, err)
			return
		}
		e.buf.WriteString(line)
		e.buf.WriteByte('\n')
		lineCounts[safeName]++
	}

	t0 := time.Now()

	for _, fname := range files {
		inPath := filepath.Join(inDir, fname)
		f, err := os.Open(inPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "WARN: cannot open %s: %v\n", inPath, err)
			continue
		}

		if !quiet && progressEnabled {
			fmt.Fprintf(os.Stderr, "\n>>> %s (%s)\n", fname, formatBytes(fileSizes[fname]))
		}

		sc := bufio.NewScanner(f)
		// Allow lines up to 16 MB
		sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)

		for sc.Scan() {
			bar.Update(1)
			totalReadLines++
			line := sc.Text()
			if strings.TrimSpace(line) == "" {
				continue
			}

			if dryRun {
				if !dbnameRe.MatchString(line) {
					skippedLines++
				}
				continue
			}

			m := dbnameRe.FindStringSubmatch(line)
			if m == nil {
				if skipNoDBname {
					skippedLines++
				} else {
					writeLine("__no_dbname__", line)
				}
				continue
			}

			rawName := m[1]
			dbname := unescapeJSONString(rawName)
			safe := sanitize(dbname)
			writeLine(safe, line)
		}
		if err := sc.Err(); err != nil {
			fmt.Fprintf(os.Stderr, "WARN: read error in %s: %v\n", fname, err)
		}
		f.Close()
	}

	// Close output handles (flush buffered writers first)
	for _, e := range fileHandles {
		if e != nil && e.buf != nil {
			e.buf.Flush()
		}
		if e != nil && e.fh != nil {
			e.fh.Close()
		}
	}
	fileHandles = nil

	elapsed := time.Since(t0).Seconds()
	rate := 0.0
	if elapsed > 0 {
		rate = float64(totalReadLines) / elapsed
	}

	fmt.Println()
	fmt.Println("[SUMMARY]")
	fmt.Printf("  input files       : %d\n", len(files))
	fmt.Printf("  total input lines : %d\n", totalReadLines)
	fmt.Printf("  skipped (no dbname): %d\n", skippedLines)
	fmt.Printf("  output files      : %d\n", len(lineCounts))
	fmt.Printf("  elapsed           : %.2fs (%.0f lines/s)\n", elapsed, rate)

	if len(lineCounts) > 0 {
		fmt.Println()
		fmt.Println("  per-file line counts (top 20):")
		type kv struct {
			k string
			v int
		}
		var sorted []kv
		for k, v := range lineCounts {
			sorted = append(sorted, kv{k, v})
		}
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].v > sorted[j].v
		})
		for i, item := range sorted {
			if i >= 20 {
				fmt.Printf("    ... and %d more\n", len(sorted)-20)
				break
			}
			fmt.Printf("    %s%s.json  -> %d lines\n", prefix, item.k, item.v)
		}
	}
}
