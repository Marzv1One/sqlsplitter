// Command sqlsplitter splits a very large SSMS-generated INSERT script into
// several smaller, independently runnable .sql files. The script may contain
// many tables, each wrapped in its own SET IDENTITY_INSERT ... ON/OFF section;
// parts never mix tables, and each part is prefixed with the USE statement and
// the SET IDENTITY_INSERT ... ON header for its own table, then suffixed with
// the matching OFF, so any part can be executed alone.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/unicode"
)

var (
	useRe      = regexp.MustCompile(`(?i)^\s*USE\s+(\S.*)$`)
	identityRe = regexp.MustCompile(`(?i)^\s*SET\s+IDENTITY_INSERT\s+(.+?)\s+(ON|OFF)\s*$`)
	goRe       = regexp.MustCompile(`(?i)^\s*GO\s*(?:--.*)?$`)
	insertRe   = regexp.MustCompile(`(?i)^\s*INSERT\s`)
)

// batch is one GO-delimited group of lines that all belong to the same
// identity-insert table context ("" means no SET IDENTITY_INSERT is active).
type batch struct {
	table   string
	body    []string // lines kept for output (USE and SET IDENTITY_INSERT lines removed)
	inserts int
	others  int // non-blank, non-INSERT statements passed through
}

type scriptInfo struct {
	useLine string
}

// detectDecode decodes raw input bytes to UTF-8, sniffing the BOM and
// falling back to Windows-1252 for SSMS ANSI exports.
func detectDecode(data []byte) (string, string, error) {
	switch {
	case len(data) >= 2 && data[0] == 0xFF && data[1] == 0xFE:
		return decodeWith(unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewDecoder(), data, "UTF-16 LE")
	case len(data) >= 2 && data[0] == 0xFE && data[1] == 0xFF:
		return decodeWith(unicode.UTF16(unicode.BigEndian, unicode.UseBOM).NewDecoder(), data, "UTF-16 BE")
	case len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF:
		return string(data[3:]), "UTF-8 (BOM)", nil
	}
	if utf8.Valid(data) {
		return string(data), "UTF-8", nil
	}
	return decodeWith(charmap.Windows1252.NewDecoder(), data, "Windows-1252")
}

func decodeWith(dec *encoding.Decoder, data []byte, name string) (string, string, error) {
	out, err := dec.Bytes(data)
	if err != nil {
		return "", "", fmt.Errorf("decoding %s input: %w", name, err)
	}
	return string(out), name, nil
}

func encodeOutput(s string, name string) ([]byte, error) {
	switch strings.ToLower(name) {
	case "utf8":
		return append([]byte{0xEF, 0xBB, 0xBF}, []byte(s)...), nil
	case "utf16le":
		return unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewEncoder().Bytes([]byte(s))
	default:
		return nil, fmt.Errorf("unsupported output encoding %q (use utf8 or utf16le)", name)
	}
}

// splitBatches breaks the script into GO-delimited batches, assigns each batch
// to the identity-insert table active at that point, and removes USE and
// SET IDENTITY_INSERT lines (they are re-emitted per part at render time).
func splitBatches(text string) ([]batch, scriptInfo) {
	var info scriptInfo
	var batches []batch
	var cur batch
	active := ""

	flush := func() {
		if len(cur.body) > 0 || cur.inserts > 0 || cur.others > 0 {
			batches = append(batches, cur)
		}
		cur = batch{}
	}

	for _, ln := range strings.Split(text, "\n") {
		ln = strings.TrimSuffix(ln, "\r")
		if goRe.MatchString(ln) {
			flush()
			continue
		}
		trimmed := strings.TrimSpace(ln)
		switch {
		case trimmed == "":
			if len(cur.body) > 0 {
				cur.body = append(cur.body, ln)
			}
		case useRe.MatchString(ln):
			if info.useLine == "" {
				info.useLine = trimmed
			}
		case identityRe.MatchString(ln):
			m := identityRe.FindStringSubmatch(ln)
			table := m[1]
			if strings.EqualFold(m[2], "ON") {
				if table != active {
					flush()
					active = table
				}
			} else {
				if active != "" {
					flush()
					active = ""
				}
			}
		case insertRe.MatchString(ln):
			if active != cur.table {
				flush()
			}
			cur.table = active
			cur.inserts++
			cur.body = append(cur.body, ln)
		default:
			if active != cur.table {
				flush()
			}
			cur.table = active
			cur.others++
			cur.body = append(cur.body, ln)
		}
	}
	flush()

	// trim trailing blank lines per batch
	for i := range batches {
		b := &batches[i]
		for len(b.body) > 0 && strings.TrimSpace(b.body[len(b.body)-1]) == "" {
			b.body = b.body[:len(b.body)-1]
		}
	}
	return batches, info
}

// chunk accumulates batches into parts. A part only ever contains batches for
// one identity table; the chunk is flushed when the table changes or the
// insert budget is reached.
type chunk struct {
	table   string
	batches []batch
	inserts int
	others  int
}

func chunkBatches(batches []batch, max int) []chunk {
	var chunks []chunk
	var cur chunk
	flush := func() {
		if len(cur.batches) > 0 {
			chunks = append(chunks, cur)
		}
		cur = chunk{}
	}
	for _, b := range batches {
		if len(cur.batches) > 0 && cur.table != b.table {
			flush()
		}
		cur.table = b.table
		cur.batches = append(cur.batches, b)
		cur.inserts += b.inserts
		cur.others += b.others
		if cur.inserts >= max {
			flush()
		}
	}
	flush()
	return chunks
}

func renderPart(c chunk, useLine string) string {
	var sb strings.Builder
	sb.WriteString(useLine + "\r\n")
	sb.WriteString("GO\r\n")
	if c.table != "" {
		sb.WriteString("SET IDENTITY_INSERT " + c.table + " ON\r\n")
		sb.WriteString("GO\r\n")
	}
	for _, b := range c.batches {
		for _, ln := range b.body {
			sb.WriteString(ln + "\r\n")
		}
		sb.WriteString("GO\r\n")
	}
	if c.table != "" {
		sb.WriteString("SET IDENTITY_INSERT " + c.table + " OFF\r\n")
		sb.WriteString("GO\r\n")
	}
	return sb.String()
}

type tableStat struct {
	inserts int
	parts   int
}

func main() {
	var (
		inPath    = flag.String("in", `C:\Users\SH_Develop\OneDrive\Documents\databases\scriptd.sql`, "input .sql file")
		outDirFl  = flag.String("out-dir", "", "output directory (default: <input dir>\\<input stem>_parts)")
		maxBatch  = flag.Int("batch", 100, "max INSERT statements per part")
		outEnc    = flag.String("encoding", "utf8", "output encoding: utf8 or utf16le")
		master    = flag.Bool("master", false, "also write master.sql with SQLCMD :r commands")
		overwrite = flag.Bool("overwrite", false, "allow overwriting existing part files")
	)
	flag.Parse()

	data, err := os.ReadFile(*inPath)
	if err != nil {
		fatal("reading input: %v", err)
	}
	text, encName, err := detectDecode(data)
	if err != nil {
		fatal("%v", err)
	}

	batches, info := splitBatches(text)
	if len(batches) == 0 {
		fatal("no batches found in %s", *inPath)
	}
	if info.useLine == "" {
		fatal("could not find a USE statement in the input")
	}
	text = "" // free memory before rendering parts

	totalInserts := 0
	totalOthers := 0
	for _, b := range batches {
		totalInserts += b.inserts
		totalOthers += b.others
	}
	chunks := chunkBatches(batches, *maxBatch)
	if totalInserts == 0 && totalOthers == 0 {
		fatal("nothing to write: no statements found")
	}

	stem := strings.TrimSuffix(filepath.Base(*inPath), filepath.Ext(*inPath))
	outDir := *outDirFl
	if outDir == "" {
		outDir = filepath.Join(filepath.Dir(*inPath), stem+"_parts")
	}
	digits := len(strconv.Itoa(len(chunks)))

	if entries, err := os.ReadDir(outDir); err == nil && len(entries) > 0 && !*overwrite {
		fatal("output directory %s is not empty; use -overwrite to replace its contents", outDir)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fatal("creating output directory: %v", err)
	}

	writtenInserts := 0
	stats := map[string]*tableStat{}
	var partNames []string
	for i, c := range chunks {
		name := fmt.Sprintf("%s_part_%0*d.sql", stem, digits, i+1)
		path := filepath.Join(outDir, name)
		content, err := encodeOutput(renderPart(c, info.useLine), *outEnc)
		if err != nil {
			fatal("%v", err)
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			fatal("writing %s: %v", path, err)
		}
		writtenInserts += c.inserts
		st := stats[c.table]
		if st == nil {
			st = &tableStat{}
			stats[c.table] = st
		}
		st.inserts += c.inserts
		st.parts++
		partNames = append(partNames, name)
		if (i+1)%5000 == 0 {
			fmt.Printf("progress: %d/%d parts written\n", i+1, len(chunks))
		}
	}

	if *master {
		var sb strings.Builder
		sb.WriteString("-- Run with SQLCMD mode enabled from this directory (SSMS: Query > SQLCMD Mode)\r\n")
		for _, name := range partNames {
			sb.WriteString(":r " + name + "\r\n")
		}
		masterPath := filepath.Join(outDir, "master.sql")
		content, err := encodeOutput(sb.String(), *outEnc)
		if err != nil {
			fatal("%v", err)
		}
		if err := os.WriteFile(masterPath, content, 0o644); err != nil {
			fatal("writing %s: %v", masterPath, err)
		}
		fmt.Printf("wrote %s\n", masterPath)
	}

	if writtenInserts != totalInserts {
		fatal("verification failed: %d inserts in input but %d written", totalInserts, writtenInserts)
	}

	fmt.Printf("\ninput encoding: %s | output encoding: %s\n", encName, strings.ToLower(*outEnc))
	fmt.Printf("database: %s\n", info.useLine)
	fmt.Printf("batches: %d | inserts: %d | other statements: %d | parts: %d\n", len(batches), totalInserts, totalOthers, len(chunks))
	fmt.Println("\nper-table breakdown:")
	tables := make([]string, 0, len(stats))
	for t := range stats {
		tables = append(tables, t)
	}
	sort.Strings(tables)
	for _, t := range tables {
		st := stats[t]
		if t == "" {
			t = "(no IDENTITY_INSERT active)"
		}
		fmt.Printf("  %-50s %10d inserts  %6d parts\n", t, st.inserts, st.parts)
	}
	fmt.Printf("\nall inserts accounted for; output in %s\n", outDir)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "sqlsplitter: "+format+"\n", args...)
	os.Exit(1)
}
