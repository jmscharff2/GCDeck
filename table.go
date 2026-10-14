package main

import (
	"path"
	"regexp"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Command output tables (docker ps, kubectl get, gcloud list) are columns aligned under an upper-case header. They
// are collected as they stream and redrawn to fit the screen, instead of wrapping into an unreadable block.

var (
	tableHeaderLine = regexp.MustCompile(`^[A-Z][A-Z_]*(?: [A-Z_]+)*(?:\s{2,}[A-Z][A-Z_]*(?: [A-Z_]+)*){2,}\s*$`)
	headerColumn    = regexp.MustCompile(`[A-Z][A-Z_]*(?: [A-Z_]+)*`)
	longHex         = regexp.MustCompile(`[0-9a-f]{40,64}`)
)

// droppable columns, least useful first, are left out when a table is too wide for the screen.
var droppable = []string{"COMMAND", "PORTS", "CREATED", "SIZE", "MOUNTS", "LABELS", "NETWORKS"}

// Table lines are stored pre-drawn, marked so they are cut at the screen edge rather than wrapped.
const (
	markTable      = "\x02"
	tableHeaderRow = markTable + "H"
	tableGoodRow   = markTable + "G"
	tableBadRow    = markTable + "B"
	tablePlainRow  = markTable + "N"
)

type tableState struct {
	offsets []int
	header  []string
	rows    [][]string
}

// tablePush takes one display line and returns the lines to add: nothing while a table is being collected, the drawn
// table once a line that is not a row ends it.
func (v *commandView) tablePush(line string) []string {
	if t := v.table; t != nil {
		if row, ok := t.parse(line); ok {
			t.rows = append(t.rows, row)
			return nil
		}
		out := v.tableFlush()
		return append(out, v.tablePush(line)...)
	}
	if tableHeaderLine.MatchString(line) {
		t := &tableState{}
		for _, loc := range headerColumn.FindAllStringIndex(line, -1) {
			t.offsets = append(t.offsets, len([]rune(line[:loc[0]])))
			t.header = append(t.header, line[loc[0]:loc[1]])
		}
		v.table = t
		return nil
	}
	return []string{line}
}

// parse splits a row at the header's column starts. Each column must start right after a space, which is what tells a
// row from the next line of ordinary output.
func (t *tableState) parse(line string) ([]string, bool) {
	// Columns line up by characters on screen, so index runes, not bytes: "…" is three bytes wide.
	r := []rune(line)
	if strings.TrimSpace(line) == "" || r[0] == ' ' || len(r) <= t.offsets[len(t.offsets)-1] {
		return nil, false
	}
	cells := make([]string, len(t.offsets))
	for i, off := range t.offsets {
		if off > 0 && r[off-1] != ' ' {
			return nil, false
		}
		end := len(r)
		if i+1 < len(t.offsets) {
			end = t.offsets[i+1]
		}
		cells[i] = shortenCell(strings.TrimSpace(string(r[off:end])))
	}
	return cells, true
}

func shortenCell(s string) string {
	s = longHex.ReplaceAllStringFunc(s, func(h string) string { return h[:12] })
	// registry/project/repo/image:tag → image:tag
	if strings.Count(s, "/") >= 2 && !strings.ContainsAny(s, " \"") {
		s = path.Base(s)
	}
	return strings.Trim(s, `"`)
}

func (v *commandView) tableFlush() []string {
	t := v.table
	v.table = nil
	if t == nil {
		return nil
	}
	width := v.lastWidth
	if width <= 0 {
		width = 120
	}
	widths := make([]int, len(t.header))
	for i, h := range t.header {
		widths[i] = len([]rune(h))
	}
	for _, r := range t.rows {
		for i, c := range r {
			widths[i] = max(widths[i], len([]rune(c)))
		}
	}
	total := func() int {
		n := 2 * (len(widths) - 1)
		for _, w := range widths {
			n += w
		}
		return n
	}
	// Too wide: first leave out columns that matter least at a glance, then narrow the widest a cell at a time.
	for _, name := range droppable {
		if total() <= width {
			break
		}
		for i, h := range t.header {
			if h == name && len(t.header) > 2 {
				t.header = append(t.header[:i:i], t.header[i+1:]...)
				widths = append(widths[:i:i], widths[i+1:]...)
				for j, r := range t.rows {
					t.rows[j] = append(r[:i:i], r[i+1:]...)
				}
				break
			}
		}
	}
	for total() > width {
		widest := 0
		for i, w := range widths {
			if w > widths[widest] {
				widest = i
			}
		}
		if widths[widest] <= 8 {
			break
		}
		widths[widest]--
	}
	draw := func(cells []string) string {
		parts := make([]string, len(cells))
		for i, c := range cells {
			parts[i] = pad(truncate(c, widths[i]), widths[i])
		}
		return strings.TrimRight(strings.Join(parts, "  "), " ")
	}
	out := []string{tableHeaderRow + draw(t.header)}
	for _, r := range t.rows {
		mark := tablePlainRow
		line := strings.Join(r, " ")
		switch {
		case strings.Contains(line, "Exited") || strings.Contains(line, "unhealthy") ||
			strings.Contains(line, "CrashLoop") || strings.Contains(line, "Error"):
			mark = tableBadRow
		case strings.Contains(line, "Up ") || strings.Contains(line, "Running") || strings.Contains(line, "RUNNING"):
			mark = tableGoodRow
		}
		out = append(out, mark+draw(r))
	}
	return out
}

func formatTableLine(line string) (styled, bool) {
	if !strings.HasPrefix(line, markTable) || len(line) < 2 {
		return styled{}, false
	}
	style := map[byte]lipgloss.Style{'H': sBold, 'G': sGood, 'B': sBad, 'N': lipgloss.NewStyle()}[line[1]]
	return styled{text: line[2:], style: style}, true
}
