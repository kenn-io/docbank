package document

import (
	"strings"
	"unicode/utf8"
)

type renditionBuilder struct {
	strings.Builder

	runes int
}

func (b *renditionBuilder) WriteString(value string) {
	b.Builder.WriteString(value)
	b.runes += utf8.RuneCountInString(value)
}

func serializeRenditionBlocks(blocks []renditionBlock, limit int) (string, bool) {
	var output renditionBuilder
	truncated := false
	previousList := false
	previousListOrdered := false
	previousListAlternate := false
	previousKind := renditionParagraph
	for _, block := range blocks {
		separator := ""
		listAlternate := false
		if output.Len() > 0 {
			separator = "\n"
			if previousKind == renditionParagraph && block.kind == renditionParagraph ||
				previousKind == renditionTable && block.kind == renditionTable ||
				previousKind == renditionListBlock && block.kind == renditionParagraph ||
				previousKind == renditionParagraph && block.kind == renditionListBlock && block.list != nil &&
					block.list.ordered && block.list.start != "1" {
				separator = "\n\n"
			}
			if previousList && block.kind == renditionListBlock && block.list != nil &&
				block.list.ordered == previousListOrdered {
				listAlternate = !previousListAlternate
			}
		}
		available := limit - output.runes - utf8.RuneCountInString(separator)
		if available < 0 {
			return finishRenditionMarkdown(output.String()), true
		}
		value, blockTruncated := serializeRenditionBlock(block, available, listAlternate)
		if value == "" && blockTruncated {
			return finishRenditionMarkdown(output.String()), true
		}
		if value != "" {
			output.WriteString(separator)
			output.WriteString(value)
			previousKind = block.kind
			previousList = block.kind == renditionListBlock && block.list != nil
			if previousList {
				previousListOrdered = block.list.ordered
				previousListAlternate = listAlternate
			}
		}
		if blockTruncated {
			truncated = true
			break
		}
	}
	return finishRenditionMarkdown(output.String()), truncated
}

func finishRenditionMarkdown(value string) string {
	return strings.TrimRight(value, "\n")
}

func canonicalNonnegativeDecimal(value string) (string, bool) {
	value = strings.TrimPrefix(value, "+")
	if value == "" {
		return "", false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return "", false
		}
	}
	value = strings.TrimLeft(value, "0")
	if value == "" {
		return "0", true
	}
	return value, true
}

func incrementNonnegativeDecimal(value string) string {
	digits := []byte(value)
	for index := len(digits) - 1; index >= 0; index-- {
		if digits[index] < '9' {
			digits[index]++
			return string(digits)
		}
		digits[index] = '0'
	}
	return "1" + string(digits)
}

func serializeRenditionBlock(block renditionBlock, available int, listAlternate bool) (string, bool) {
	switch block.kind {
	case renditionCodeBlock:
		value := serializeRenditionCodeBlock(block.language, block.code)
		if utf8.RuneCountInString(value) > available {
			return "", true
		}
		return value, false
	case renditionTable:
		value := serializeRenditionTable(block.rows)
		if utf8.RuneCountInString(value) > available {
			return "", true
		}
		return value, false
	case renditionListBlock:
		if block.list == nil {
			return "", false
		}
		return serializeRenditionList(*block.list, available, listAlternate)
	case renditionParagraph, renditionHeading:
		prefix := ""
		switch block.kind {
		case renditionParagraph:
			// Paragraphs have no generated block marker.
		case renditionHeading:
			prefix = strings.Repeat("#", block.level) + " "
		case renditionCodeBlock, renditionTable, renditionListBlock:
			return "", false
		}
		if utf8.RuneCountInString(prefix) > available {
			return "", true
		}
		value, truncated := serializeRenditionInlines(block.inlines, available-utf8.RuneCountInString(prefix), false)
		if value == "" && truncated {
			return "", true
		}
		return prefix + value, truncated
	default:
		return "", false
	}
}

func serializeRenditionList(list renditionList, available int, alternate bool) (string, bool) {
	var output renditionBuffer
	result := appendRenditionList(&output, list, available, 0, alternate)
	return output.String(), result.truncated
}

type renditionListResult struct {
	truncated        bool
	emitted          int
	contentIndent    int
	looseFallback    renditionBufferCheckpoint
	hasLooseFallback bool
}

type renditionSerializedItemRange struct {
	ordinal string
	start   int
	end     int
}

func appendRenditionList(
	output *renditionBuffer,
	list renditionList,
	limit int,
	indent int,
	alternate bool,
) renditionListResult {
	start := output.checkpoint()
	var result renditionListResult
	if list.ordered && len(list.start) <= maxOrderedListMarkerDigits {
		result = appendRepresentableOrderedListItems(output, list, limit, indent, alternate)
	} else {
		result = appendRenditionListItems(output, list, limit, indent, alternate, list.ordered)
	}
	if list.tight || result.emitted != 1 {
		return result
	}
	looseBoundary := renditionLooseBoundary(result.contentIndent)
	if output.runes+utf8.RuneCountInString(looseBoundary) > limit {
		if !result.hasLooseFallback {
			output.rollback(start.bytes, start.runes)
			return renditionListResult{truncated: true}
		}
		output.rollback(result.looseFallback.bytes, result.looseFallback.runes)
		result.truncated = true
	}
	output.WriteString(looseBoundary)
	return result
}

func renditionLooseBoundary(contentIndent int) string {
	return "\n\n" + strings.Repeat(" ", contentIndent) + "<!-- -->"
}

func appendRepresentableOrderedListItems(
	output *renditionBuffer,
	list renditionList,
	limit int,
	indent int,
	alternate bool,
) renditionListResult {
	listStart := output.checkpoint()
	entries := make([]renditionSerializedItemRange, 0, len(list.items))
	result := renditionListResult{}
	ordinal := list.start
	degraded := false
	var fallback []byte
	fallbackRunes := 0
	for _, item := range list.items {
		if !item.present {
			continue
		}
		if !degraded && len(ordinal) > maxOrderedListMarkerDigits {
			fallback = append([]byte(nil), output.bytes[listStart.bytes:]...)
			fallbackRunes = output.runes - listStart.runes
			var converted renditionBuffer
			for index, entry := range entries {
				if index > 0 {
					converted.WriteString(renditionListItemSeparator(list.tight))
				}
				converted.WriteString(degradeOrderedListItem(serializedOrderedListItem{
					ordinal: entry.ordinal,
					value:   string(output.bytes[entry.start:entry.end]),
				}, indent, alternate))
			}
			output.rollback(listStart.bytes, listStart.runes)
			output.WriteString(converted.String())
			if output.runes > limit {
				output.rollback(listStart.bytes, listStart.runes)
				output.WriteBytes(fallback, fallbackRunes)
				result.truncated = true
				return result
			}
			degraded = true
		}

		itemStart := output.checkpoint()
		if result.emitted > 0 {
			output.WriteString(renditionListItemSeparator(list.tight))
		}
		marker := ordinal + "."
		if alternate {
			marker = ordinal + ")"
		}
		labelPrefix := ""
		if degraded {
			marker = "-"
			if alternate {
				marker = "*"
			}
			labelPrefix = ordinal + "\\. "
		}
		itemFallback := newRenditionLooseItemFallback(list, limit, indent, marker, result.emitted, output.runes)
		valueStart := len(output.bytes)
		wrote, truncated := appendRenditionListItem(output, item, limit, indent, marker, labelPrefix, itemFallback)
		if !wrote {
			output.rollback(itemStart.bytes, itemStart.runes)
			if fallback != nil {
				output.rollback(listStart.bytes, listStart.runes)
				output.WriteBytes(fallback, fallbackRunes)
			}
			result.truncated = true
			return result
		}
		if !degraded {
			entries = append(entries, renditionSerializedItemRange{ordinal: ordinal, start: valueStart, end: len(output.bytes)})
		}
		fallback = nil
		result.emitted++
		if result.emitted == 1 {
			result.contentIndent = indent + utf8.RuneCountInString(marker) + 1
			result.looseFallback, result.hasLooseFallback = itemFallback.result()
		}
		if truncated {
			result.truncated = true
			return result
		}
		ordinal = incrementNonnegativeDecimal(ordinal)
	}
	return result
}

func appendRenditionListItems(
	output *renditionBuffer,
	list renditionList,
	limit int,
	indent int,
	alternate bool,
	degraded bool,
) renditionListResult {
	result := renditionListResult{}
	ordinal := list.start
	for _, item := range list.items {
		if !item.present {
			continue
		}
		itemStart := output.checkpoint()
		if result.emitted > 0 {
			output.WriteString(renditionListItemSeparator(list.tight))
		}
		marker := "-"
		if alternate {
			marker = "*"
		}
		labelPrefix := ""
		if list.ordered && !degraded {
			marker = ordinal + "."
			if alternate {
				marker = ordinal + ")"
			}
		} else if list.ordered {
			labelPrefix = ordinal + "\\. "
		}
		itemFallback := newRenditionLooseItemFallback(list, limit, indent, marker, result.emitted, output.runes)
		wrote, truncated := appendRenditionListItem(output, item, limit, indent, marker, labelPrefix, itemFallback)
		if !wrote {
			output.rollback(itemStart.bytes, itemStart.runes)
			result.truncated = true
			return result
		}
		result.emitted++
		if result.emitted == 1 {
			result.contentIndent = indent + utf8.RuneCountInString(marker) + 1
			result.looseFallback, result.hasLooseFallback = itemFallback.result()
		}
		if truncated {
			result.truncated = true
			return result
		}
		if list.ordered {
			ordinal = incrementNonnegativeDecimal(ordinal)
		}
	}
	return result
}

func newRenditionLooseItemFallback(
	list renditionList,
	limit int,
	indent int,
	marker string,
	emitted int,
	itemStartRunes int,
) *renditionBufferFallback {
	if list.tight || emitted != 0 {
		return nil
	}
	contentIndent := indent + utf8.RuneCountInString(marker) + 1
	return &renditionBufferFallback{
		limit:    limit - utf8.RuneCountInString(renditionLooseBoundary(contentIndent)),
		minRunes: itemStartRunes,
	}
}

func appendRenditionListItem(
	output *renditionBuffer,
	item renditionListItem,
	limit int,
	indent int,
	marker string,
	labelPrefix string,
	fallback *renditionBufferFallback,
) (bool, bool) {
	start := output.checkpoint()
	markerLine := strings.Repeat(" ", indent) + marker
	markerAnchor := markerLine
	if labelPrefix != "" {
		markerAnchor += " " + strings.TrimSuffix(labelPrefix, " ")
	}
	contentIndent := indent + utf8.RuneCountInString(marker) + 1
	if len(item.blocks) == 0 {
		if output.runes+utf8.RuneCountInString(markerAnchor) > limit {
			return false, true
		}
		output.WriteString(markerAnchor)
		fallback.mark(output)
		return true, false
	}
	previousList := false
	previousListOrdered := false
	previousListAlternate := false
	previousListIndent := contentIndent
	for _, block := range item.blocks {
		separator := ""
		listAlternate := false
		listIndent := contentIndent
		if output.runes > start.runes {
			if block.kind == renditionListBlock {
				separator = "\n"
				if previousList {
					listIndent = previousListIndent
				}
				if block.list != nil && previousList && block.list.ordered == previousListOrdered {
					listAlternate = !previousListAlternate
				} else if block.list != nil && !previousList && block.list.ordered && block.list.start != "1" {
					separator = "\n\n"
				}
			} else {
				separator = "\n\n"
			}
		}

		if block.kind == renditionListBlock {
			if block.list == nil {
				continue
			}
			if output.runes == start.runes {
				if block.list.ordered && block.list.start != "1" {
					listIndent += 2
				}
				if output.runes+utf8.RuneCountInString(markerAnchor) > limit {
					return false, true
				}
				output.WriteString(markerAnchor)
				if output.runes+1 > limit {
					return true, true
				}
				separator = "\n"
			}
			separatorStart := output.checkpoint()
			if output.runes+utf8.RuneCountInString(separator) > limit {
				return output.runes > start.runes, true
			}
			output.WriteString(separator)
			nestedStartRunes := output.runes
			result := appendRenditionList(output, *block.list, limit, listIndent, listAlternate)
			if output.runes == nestedStartRunes {
				output.rollback(separatorStart.bytes, separatorStart.runes)
				return output.runes > start.runes, result.truncated
			}
			previousList = true
			previousListOrdered = block.list.ordered
			previousListAlternate = listAlternate
			previousListIndent = listIndent
			fallback.mark(output)
			if result.truncated {
				return true, true
			}
			continue
		}

		separatorStart := output.checkpoint()
		if output.runes+utf8.RuneCountInString(separator) > limit {
			return output.runes > start.runes, true
		}
		output.WriteString(separator)
		firstPrefix := strings.Repeat(" ", contentIndent)
		if output.runes == start.runes {
			firstPrefix = markerLine + " " + labelPrefix
		}
		wrote, truncated := appendRenditionItemBlock(
			output, block, limit, firstPrefix, strings.Repeat(" ", contentIndent), fallback,
		)
		if !wrote {
			output.rollback(separatorStart.bytes, separatorStart.runes)
		}
		if !wrote && truncated {
			return output.runes > start.runes, true
		}
		if wrote {
			previousList = false
		}
		if truncated {
			return output.runes > start.runes, true
		}
	}
	return output.runes > start.runes, false
}

const maxOrderedListMarkerDigits = 9

type serializedOrderedListItem struct {
	ordinal string
	value   string
}

func renditionListItemSeparator(tight bool) string {
	if tight {
		return "\n"
	}
	return "\n\n"
}

func degradeOrderedListItem(item serializedOrderedListItem, indent int, alternate bool) string {
	normalMarker := item.ordinal + "."
	degradedMarker := "-"
	if alternate {
		normalMarker = item.ordinal + ")"
		degradedMarker = "*"
	}
	normalPrefix := strings.Repeat(" ", indent) + normalMarker
	degradedPrefix := strings.Repeat(" ", indent) + degradedMarker + " " + item.ordinal + "\\."
	lines := strings.Split(item.value, "\n")
	lines[0] = degradedPrefix + strings.TrimPrefix(lines[0], normalPrefix)
	normalIndent := strings.Repeat(" ", indent+utf8.RuneCountInString(normalMarker)+1)
	degradedIndent := strings.Repeat(" ", indent+2)
	for index := 1; index < len(lines); index++ {
		if after, ok := strings.CutPrefix(lines[index], normalIndent); ok {
			lines[index] = degradedIndent + after
		}
	}
	return strings.Join(lines, "\n")
}

func appendRenditionItemBlock(
	output *renditionBuffer,
	block renditionBlock,
	limit int,
	firstPrefix string,
	continuationPrefix string,
	fallback *renditionBufferFallback,
) (bool, bool) {
	start := output.checkpoint()
	if block.kind == renditionParagraph || block.kind == renditionHeading {
		if block.kind == renditionHeading {
			firstPrefix += strings.Repeat("#", block.level) + " "
		}
		prefixRunes := utf8.RuneCountInString(firstPrefix)
		if output.runes+prefixRunes > limit {
			return false, true
		}
		output.WriteString(firstPrefix)
		inlineStart := output.checkpoint()
		truncated := appendRenditionInlinesWithFallback(
			output, block.inlines, limit-output.runes, false, fallback,
		)
		if output.runes == inlineStart.runes && truncated {
			output.rollback(start.bytes, start.runes)
			return false, true
		}
		fallback.mark(output)
		return true, truncated
	}

	var value string
	switch block.kind {
	case renditionCodeBlock:
		value = serializeRenditionCodeBlock(block.language, block.code)
	case renditionTable:
		value = serializeRenditionTable(block.rows)
	default:
		return false, false
	}
	value = prefixRenditionLines(value, firstPrefix, continuationPrefix)
	if output.runes+utf8.RuneCountInString(value) > limit {
		return false, true
	}
	output.WriteString(value)
	fallback.mark(output)
	return true, false
}

func prefixRenditionLines(value, firstPrefix, continuationPrefix string) string {
	return firstPrefix + strings.ReplaceAll(value, "\n", "\n"+continuationPrefix)
}

func serializeRenditionInlines(inlines []renditionInline, available int, inTable bool) (string, bool) {
	var output renditionBuffer
	truncated := appendRenditionInlines(&output, inlines, available, inTable)
	return output.String(), truncated
}

type renditionBuffer struct {
	bytes []byte
	runes int
}

type renditionBufferCheckpoint struct {
	bytes int
	runes int
}

type renditionBufferFallback struct {
	limit      int
	minRunes   int
	checkpoint renditionBufferCheckpoint
	valid      bool
}

func (f *renditionBufferFallback) mark(output *renditionBuffer) {
	if f == nil || output.runes <= f.minRunes || output.runes > f.limit {
		return
	}
	f.checkpoint = output.checkpoint()
	f.valid = true
}

func (f *renditionBufferFallback) markEscapedText(
	start renditionBufferCheckpoint,
	value string,
) {
	if f == nil || start.runes >= f.limit {
		return
	}
	prefix := truncateEscapedRenditionText(value, f.limit-start.runes)
	if prefix == "" {
		return
	}
	f.checkpoint = renditionBufferCheckpoint{
		bytes: start.bytes + len(prefix),
		runes: start.runes + utf8.RuneCountInString(prefix),
	}
	f.valid = true
}

func (f *renditionBufferFallback) result() (renditionBufferCheckpoint, bool) {
	if f == nil {
		return renditionBufferCheckpoint{}, false
	}
	return f.checkpoint, f.valid
}

func (b *renditionBuffer) WriteString(value string) {
	b.bytes = append(b.bytes, value...)
	b.runes += utf8.RuneCountInString(value)
}

func (b *renditionBuffer) WriteBytes(value []byte, runes int) {
	b.bytes = append(b.bytes, value...)
	b.runes += runes
}

func (b *renditionBuffer) String() string {
	return string(b.bytes)
}

func (b *renditionBuffer) rollback(bytes, runes int) {
	b.bytes = b.bytes[:bytes]
	b.runes = runes
}

func (b *renditionBuffer) checkpoint() renditionBufferCheckpoint {
	return renditionBufferCheckpoint{bytes: len(b.bytes), runes: b.runes}
}

func appendRenditionInlines(
	output *renditionBuffer,
	inlines []renditionInline,
	available int,
	inTable bool,
) bool {
	return appendRenditionInlinesWithFallback(output, inlines, available, inTable, nil)
}

func appendRenditionInlinesWithFallback(
	output *renditionBuffer,
	inlines []renditionInline,
	available int,
	inTable bool,
	fallback *renditionBufferFallback,
) bool {
	startRunes := output.runes
	for _, inline := range inlines {
		if inline.kind == renditionLinkInline && inline.destination == "" {
			remaining := available
			if available >= 0 {
				remaining -= output.runes - startRunes
			}
			if appendRenditionInlinesWithFallback(output, inline.children, remaining, inTable, fallback) {
				return true
			}
			continue
		}
		remaining := available
		if available >= 0 {
			remaining -= output.runes - startRunes
		}
		switch inline.kind {
		case renditionText:
			textStart := output.checkpoint()
			value := escapeRenditionText(inline.text)
			if available >= 0 && utf8.RuneCountInString(value) > remaining {
				output.WriteString(truncateEscapedRenditionText(inline.text, max(0, remaining)))
				fallback.markEscapedText(textStart, inline.text)
				return true
			}
			output.WriteString(value)
			fallback.markEscapedText(textStart, inline.text)
		case renditionInlineCode:
			value := serializeRenditionInlineCode(inline.text, inTable)
			if available >= 0 && utf8.RuneCountInString(value) > remaining {
				return true
			}
			output.WriteString(value)
			fallback.mark(output)
		case renditionLinkInline:
			overhead := utf8.RuneCountInString(inline.destination) + 4
			if available >= 0 && overhead > remaining {
				appendRenditionPlainLabel(output, inline.children, remaining, fallback)
				return true
			}
			markBytes, markRunes := len(output.bytes), output.runes
			output.WriteString("[")
			labelStart := output.runes
			labelBudget := -1
			if available >= 0 {
				labelBudget = remaining - overhead
			}
			if appendRenditionInlinesWithFallback(output, inline.children, labelBudget, inTable, nil) {
				output.rollback(markBytes, markRunes)
				appendRenditionPlainLabel(output, inline.children, remaining, fallback)
				return true
			}
			if output.runes == labelStart {
				output.rollback(markBytes, markRunes)
				continue
			}
			output.WriteString("](")
			output.WriteString(inline.destination)
			output.WriteString(")")
			fallback.mark(output)
		}
	}
	return false
}

func appendRenditionPlainLabel(
	output *renditionBuffer,
	inlines []renditionInline,
	available int,
	fallback *renditionBufferFallback,
) bool {
	startRunes := output.runes
	for _, inline := range inlines {
		remaining := available
		if available >= 0 {
			remaining -= output.runes - startRunes
		}
		switch inline.kind {
		case renditionText, renditionInlineCode:
			textStart := output.checkpoint()
			value := escapeRenditionText(inline.text)
			if available >= 0 && utf8.RuneCountInString(value) > remaining {
				output.WriteString(truncateEscapedRenditionText(inline.text, max(0, remaining)))
				fallback.markEscapedText(textStart, inline.text)
				return true
			}
			output.WriteString(value)
			fallback.markEscapedText(textStart, inline.text)
		case renditionLinkInline:
			if appendRenditionPlainLabel(output, inline.children, remaining, fallback) {
				return true
			}
		}
	}
	return false
}

func escapeRenditionText(value string) string {
	var output strings.Builder
	for _, character := range value {
		if isMarkdownASCIIPunctuation(character) {
			output.WriteByte('\\')
		}
		output.WriteRune(character)
	}
	return output.String()
}

func truncateEscapedRenditionText(value string, limit int) string {
	var output strings.Builder
	used := 0
	for _, character := range value {
		cost := 1
		if isMarkdownASCIIPunctuation(character) {
			cost++
		}
		if used+cost > limit {
			break
		}
		if cost == 2 {
			output.WriteByte('\\')
		}
		output.WriteRune(character)
		used += cost
	}
	return output.String()
}

func isMarkdownASCIIPunctuation(character rune) bool {
	return character >= '!' && character <= '/' || character >= ':' && character <= '@' ||
		character >= '[' && character <= '`' || character >= '{' && character <= '~'
}

func serializeRenditionInlineCode(content string, inTable bool) string {
	content = strings.ReplaceAll(strings.ReplaceAll(content, "\r\n", " "), "\n", " ")
	if inTable {
		content = strings.ReplaceAll(content, "|", "\\|")
	}
	fence := strings.Repeat("`", maxBacktickRun(content)+1)
	if strings.HasPrefix(content, "`") || strings.HasSuffix(content, "`") {
		content = " " + content + " "
	}
	return fence + content + fence
}

func serializeRenditionCodeBlock(language, content string) string {
	fence := strings.Repeat("`", max(3, maxBacktickRun(content)+1))
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return fence + language + "\n" + content + fence
}

func serializeRenditionTable(rows [][][]renditionInline) string {
	if len(rows) == 0 {
		return ""
	}
	columns := 0
	for _, row := range rows {
		columns = max(columns, len(row))
	}
	if columns == 0 {
		return ""
	}
	format := func(row [][]renditionInline) string {
		cells := make([]string, columns)
		for index, cell := range row {
			cells[index], _ = serializeRenditionInlines(cell, -1, true)
		}
		return "| " + strings.Join(cells, " | ") + " |"
	}
	delimiter := make([]string, columns)
	for index := range delimiter {
		delimiter[index] = "---"
	}
	lines := []string{format(rows[0]), "| " + strings.Join(delimiter, " | ") + " |"}
	for _, row := range rows[1:] {
		lines = append(lines, format(row))
	}
	return strings.Join(lines, "\n")
}

func maxBacktickRun(value string) int {
	maximum := 0
	current := 0
	for _, character := range value {
		if character == '`' {
			current++
			maximum = max(maximum, current)
		} else {
			current = 0
		}
	}
	return maximum
}
