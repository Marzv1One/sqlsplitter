package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func headerFixture(table string) string {
	return "USE [ShuriDB_Prod]\r\n" +
		"GO\r\n" +
		"SET IDENTITY_INSERT [dbo].[" + table + "] ON\r\n" +
		"GO\r\n"
}

func insertLine(table string, id int) string {
	return "INSERT [dbo].[" + table + "] ([Id]) VALUES (" + strconv.Itoa(id) + ")"
}

func TestDetectDecodeUTF16LE(t *testing.T) {
	raw := []byte{0xFF, 0xFE, 'U', 0, 'S', 0, 'E', 0}
	text, name, err := detectDecode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if name != "UTF-16 LE" || text != "USE" {
		t.Fatalf("got %q (%s)", text, name)
	}
}

func TestDetectDecodeUTF8BOM(t *testing.T) {
	raw := []byte{0xEF, 0xBB, 0xBF, 'U', 'S', 'E'}
	text, name, err := detectDecode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if name != "UTF-8 (BOM)" || text != "USE" {
		t.Fatalf("got %q (%s)", text, name)
	}
}

func TestDetectDecodeWindows1252(t *testing.T) {
	// Ñ in Windows-1252 is 0xD1
	raw := []byte{'N', 0xD1, 0xD5} // N Ñ Õ
	text, name, err := detectDecode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if name != "Windows-1252" || text != "NÑÕ" {
		t.Fatalf("got %q (%s)", text, name)
	}
}

func TestSplitBatchesGoCaseInsensitive(t *testing.T) {
	text := "USE [Db]\nGO\nINSERT [t] ([Id]) VALUES (1)\n  go  \nINSERT [t] ([Id]) VALUES (2)\nGO\n"
	bs, _ := splitBatches(text)
	if len(bs) != 2 {
		t.Fatalf("expected 2 batches, got %d: %+v", len(bs), bs)
	}
	if bs[0].inserts != 1 || bs[1].inserts != 1 {
		t.Fatalf("insert counts wrong: %+v", bs)
	}
}

func TestSplitBatchesRemovesHeaderStatements(t *testing.T) {
	text := headerFixture("T1") +
		"INSERT [dbo].[T1] ([Id]) VALUES (1)\r\nGO\r\n"
	bs, _ := splitBatches(text)
	for _, b := range bs {
		for _, ln := range b.body {
			u := strings.ToUpper(ln)
			if strings.Contains(u, "IDENTITY_INSERT") || strings.HasPrefix(u, "USE ") {
				t.Fatalf("header statement leaked into body: %q", ln)
			}
		}
	}
}

func TestBatchTableAssignment(t *testing.T) {
	text := headerFixture("A") +
		insertLine("A", 1) + "\r\nGO\r\n" +
		"SET IDENTITY_INSERT [dbo].[B] ON\r\nGO\r\n" +
		insertLine("B", 2) + "\r\nGO\r\n" +
		"SET IDENTITY_INSERT [dbo].[B] OFF\r\nGO\r\n" +
		insertLine("C", 3) + "\r\nGO\r\n"
	bs, _ := splitBatches(text)
	wantTables := []string{"[dbo].[A]", "[dbo].[B]", ""}
	if len(bs) != len(wantTables) {
		t.Fatalf("expected %d batches, got %d", len(wantTables), len(bs))
	}
	for i, want := range wantTables {
		if bs[i].table != want {
			t.Fatalf("batch %d table: want %q got %q", i, want, bs[i].table)
		}
	}
	if bs[2].inserts != 1 || bs[2].others != 0 {
		t.Fatalf("insert without active identity block misclassified: %+v", bs[3])
	}
}

func TestChunkBatches(t *testing.T) {
	var bs []batch
	for i := 0; i < 5; i++ {
		bs = append(bs, batch{table: "[dbo].[A]", body: []string{insertLine("A", i)}, inserts: 1})
	}
	chunks := chunkBatches(bs, 2)
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(chunks))
	}
	want := []int{2, 2, 1}
	for i, c := range chunks {
		if c.inserts != want[i] {
			t.Fatalf("chunk %d: expected %d inserts, got %d", i, want[i], c.inserts)
		}
	}
}

func TestChunkBatchesSingleLargeBatch(t *testing.T) {
	b := batch{table: "[dbo].[A]", body: []string{insertLine("A", 1), insertLine("A", 2), insertLine("A", 3)}, inserts: 3}
	chunks := chunkBatches([]batch{b}, 2)
	if len(chunks) != 1 || chunks[0].inserts != 3 {
		t.Fatalf("batch larger than max must stay intact: %+v", chunks)
	}
}

func TestChunkNeverMixesTables(t *testing.T) {
	bs := []batch{
		{table: "[dbo].[A]", body: []string{insertLine("A", 1)}, inserts: 1},
		{table: "[dbo].[B]", body: []string{insertLine("B", 2)}, inserts: 1},
	}
	chunks := chunkBatches(bs, 100)
	if len(chunks) != 2 {
		t.Fatalf("table change must force a new part, got %d chunks", len(chunks))
	}
	if chunks[0].table != "[dbo].[A]" || chunks[1].table != "[dbo].[B]" {
		t.Fatalf("chunk tables wrong: %+v", chunks)
	}
}

func TestRenderPartHeaderFooter(t *testing.T) {
	c := chunk{table: "[dbo].[Bk_tbFormatos_Datos]", batches: []batch{{body: []string{insertLine("Bk_tbFormatos_Datos", 1)}, inserts: 1}}, inserts: 1}
	out := renderPart(c, "USE [ShuriDB_Prod]")
	if !strings.HasPrefix(out, "USE [ShuriDB_Prod]\r\nGO\r\nSET IDENTITY_INSERT [dbo].[Bk_tbFormatos_Datos] ON\r\nGO\r\n") {
		t.Fatalf("bad header:\n%s", out)
	}
	if !strings.HasSuffix(out, "SET IDENTITY_INSERT [dbo].[Bk_tbFormatos_Datos] OFF\r\nGO\r\n") {
		t.Fatalf("bad footer:\n%s", out)
	}
}

func TestRenderPartNoIdentityTable(t *testing.T) {
	c := chunk{table: "", batches: []batch{{body: []string{"PRINT 'hi'"}, others: 1}}, inserts: 0, others: 1}
	out := renderPart(c, "USE [ShuriDB_Prod]")
	if strings.Contains(out, "IDENTITY_INSERT") {
		t.Fatalf("no identity header expected:\n%s", out)
	}
	if !strings.HasPrefix(out, "USE [ShuriDB_Prod]\r\nGO\r\n") || !strings.HasSuffix(out, "GO\r\n") {
		t.Fatalf("bad body:\n%s", out)
	}
}

func TestEncodeOutputRoundTrip(t *testing.T) {
	for _, enc := range []string{"utf8", "utf16le"} {
		raw, err := encodeOutput("USE [X] Ñ", enc)
		if err != nil {
			t.Fatal(err)
		}
		text, name, err := detectDecode(raw)
		if err != nil {
			t.Fatal(err)
		}
		if text != "USE [X] Ñ" {
			t.Fatalf("%s: round trip mismatch %q", name, text)
		}
	}
}

func TestSplitBatchesPreservesVeryLongLine(t *testing.T) {
	long := strings.Repeat("X", 1_000_000)
	text := headerFixture("T1") + "INSERT [dbo].[T1] ([V]) VALUES ('" + long + "')\r\nGO\r\n"
	bs, _ := splitBatches(text)
	if len(bs) != 1 || len(bs[0].body) != 1 {
		t.Fatalf("unexpected batches: %+v", bs)
	}
	if !strings.Contains(bs[0].body[0], long) {
		t.Fatal("long line truncated")
	}
}

func TestEndToEnd(t *testing.T) {
	dir := t.TempDir()
	var sb strings.Builder
	sb.WriteString(headerFixture("T1"))
	for i := 1; i <= 250; i++ {
		sb.WriteString(insertLine("T1", i) + "\r\nGO\r\n")
	}
	sb.WriteString("SET IDENTITY_INSERT [dbo].[T1] OFF\r\nGO\r\n")
	sb.WriteString("SET IDENTITY_INSERT [dbo].[T2] ON\r\nGO\r\n")
	for i := 1; i <= 50; i++ {
		sb.WriteString(insertLine("T2", i) + "\r\nGO\r\n")
	}
	sb.WriteString("SET IDENTITY_INSERT [dbo].[T2] OFF\r\nGO\r\n")
	inPath := filepath.Join(dir, "script.sql")
	if err := os.WriteFile(inPath, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(inPath)
	if err != nil {
		t.Fatal(err)
	}
	text, _, err := detectDecode(raw)
	if err != nil {
		t.Fatal(err)
	}
	bs, info := splitBatches(text)
	if len(bs) != 300 {
		t.Fatalf("expected 300 batches, got %d", len(bs))
	}
	chunks := chunkBatches(bs, 100)
	if len(chunks) != 4 {
		t.Fatalf("expected 4 parts (3x T1 + 1x T2), got %d", len(chunks))
	}
	total := 0
	for i, c := range chunks {
		total += c.inserts
		out := renderPart(c, info.useLine)
		wantTable := "[dbo].[T1]"
		if i == 3 {
			wantTable = "[dbo].[T2]"
		}
		if c.table != wantTable {
			t.Fatalf("part %d table: want %s got %s", i+1, wantTable, c.table)
		}
		wantInsert := 100
		if i == 2 {
			wantInsert = 50
		}
		if i == 3 {
			wantInsert = 50
		}
		if n := strings.Count(out, "\r\nINSERT "+wantTable); n != wantInsert {
			t.Fatalf("part %d: rendered %d inserts, expected %d", i+1, n, wantInsert)
		}
		if !strings.Contains(out, "SET IDENTITY_INSERT "+wantTable+" ON") ||
			!strings.Contains(out, "SET IDENTITY_INSERT "+wantTable+" OFF") {
			t.Fatalf("part %d missing identity header/footer for %s", i+1, wantTable)
		}
	}
	if total != 300 {
		t.Fatalf("expected 300 total inserts, got %d", total)
	}
}
