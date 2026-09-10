package emailmime

// This bounded parser follows Go's net/mail address grammar. See
// LICENSE.multipart.txt for the Go BSD license.

import (
	"errors"
	"fmt"
	"math"
	"net"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
)

type boundedAddrParser struct{ value string }

type addressTextSink struct {
	b      strings.Builder
	size   int64
	limit  int64
	retain bool
}

func (w *addressTextSink) writeString(value string) error {
	if int64(len(value)) > w.limit-w.size {
		return errDisplayBudget
	}
	w.size += int64(len(value))
	if w.retain {
		_, _ = w.b.WriteString(value)
	}
	return nil
}

func (w *addressTextSink) writeBytes(value []byte) error {
	if int64(len(value)) > w.limit-w.size {
		return errDisplayBudget
	}
	w.size += int64(len(value))
	if w.retain {
		_, _ = w.b.Write(value)
	}
	return nil
}

func (w *addressTextSink) writeByte(value byte) error {
	if 1 > w.limit-w.size {
		return errDisplayBudget
	}
	w.size++
	if w.retain {
		_ = w.b.WriteByte(value)
	}
	return nil
}

func (w *addressTextSink) writeRune(value rune) error {
	var encoded [utf8.UTFMax]byte
	size := utf8.EncodeRune(encoded[:], value)
	if int64(size) > w.limit-w.size {
		return errDisplayBudget
	}
	w.size += int64(size)
	if w.retain {
		_, _ = w.b.Write(encoded[:size])
	}
	return nil
}

func (w *addressTextSink) String() string { return w.b.String() }

func projectAddressesBounded(value string, limit int64) ([]document.EmailAddressV1, int64, error) {
	if limit < 0 {
		return nil, 0, errDisplayBudget
	}
	parser := boundedAddrParser{value: value}
	addresses := make([]document.EmailAddressV1, 0)
	var cost int64
	for {
		parser.skipSpace()
		if parser.consume(',') {
			continue
		}
		parsed, parsedCost, err := parser.parseAddress(true, limit-cost)
		if err != nil {
			return nil, 0, err
		}
		addresses = append(addresses, parsed...)
		cost += parsedCost
		if !parser.skipCFWS() {
			return nil, 0, errors.New("mail: misformatted parenthetical comment")
		}
		if parser.empty() {
			break
		}
		if parser.peek() != ',' {
			return nil, 0, errors.New("mail: expected comma")
		}
		for parser.consume(',') {
			parser.skipSpace()
		}
		if parser.empty() {
			break
		}
	}
	return addresses, cost, nil
}

func (p *boundedAddrParser) parseAddress(handleGroup bool, limit int64) ([]document.EmailAddressV1, int64, error) {
	p.skipSpace()
	if p.empty() {
		return nil, 0, errors.New("mail: no address")
	}
	original := *p
	specParser := original
	specCount := addressTextSink{limit: math.MaxInt64}
	if err := specParser.consumeAddrSpec(&specCount); err == nil {
		if specCount.size > limit {
			return nil, 0, errDisplayBudget
		}
		specSink := addressTextSink{limit: limit, retain: true}
		if err := p.consumeAddrSpec(&specSink); err != nil {
			return nil, 0, err
		}
		p.skipSpace()
		name := ""
		nameSize := int64(0)
		if !p.empty() && p.peek() == '(' {
			commentStart := *p
			commentCount := addressTextSink{limit: limit - specSink.size}
			if err := p.consumeDisplayNameComment(&commentCount); err != nil {
				return nil, 0, err
			}
			if commentCount.size > limit-specSink.size {
				return nil, 0, errDisplayBudget
			}
			*p = commentStart
			commentSink := addressTextSink{limit: limit - specSink.size, retain: true}
			if err := p.consumeDisplayNameComment(&commentSink); err != nil {
				return nil, 0, err
			}
			name, nameSize = commentSink.String(), commentSink.size
		}
		cost := specSink.size + nameSize
		return []document.EmailAddressV1{{Name: name, Address: specSink.String()}}, cost, nil
	}

	*p = original
	phraseStart := *p
	phraseCount := addressTextSink{limit: math.MaxInt64}
	if p.peek() != '<' {
		if err := p.consumePhrase(&phraseCount); err != nil {
			return nil, 0, err
		}
	}
	p.skipSpace()
	if handleGroup && p.consume(':') {
		return p.consumeGroupList(limit)
	}
	if !p.consume('<') {
		return nil, 0, errors.New("mail: no angle-addr")
	}
	if phraseCount.size > limit {
		return nil, 0, errDisplayBudget
	}
	*p = phraseStart
	nameSink := addressTextSink{limit: limit, retain: true}
	if p.peek() != '<' {
		if err := p.consumePhrase(&nameSink); err != nil {
			return nil, 0, err
		}
	}
	p.skipSpace()
	if !p.consume('<') {
		return nil, 0, errors.New("mail: no angle-addr")
	}
	addressCount := addressTextSink{limit: math.MaxInt64}
	addressStart := *p
	if err := p.consumeAddrSpec(&addressCount); err != nil {
		return nil, 0, err
	}
	if addressCount.size > limit-nameSink.size {
		return nil, 0, errDisplayBudget
	}
	*p = addressStart
	addressSink := addressTextSink{limit: limit - nameSink.size, retain: true}
	if err := p.consumeAddrSpec(&addressSink); err != nil {
		return nil, 0, err
	}
	if !p.consume('>') {
		return nil, 0, errors.New("mail: unclosed angle-addr")
	}
	return []document.EmailAddressV1{{Name: nameSink.String(), Address: addressSink.String()}}, nameSink.size + addressSink.size, nil
}

func (p *boundedAddrParser) consumeGroupList(limit int64) ([]document.EmailAddressV1, int64, error) {
	var group []document.EmailAddressV1
	var cost int64
	p.skipSpace()
	if p.consume(';') {
		if !p.skipCFWS() {
			return nil, 0, errors.New("mail: misformatted parenthetical comment")
		}
		return group, 0, nil
	}
	for {
		p.skipSpace()
		addresses, addressCost, err := p.parseAddress(false, limit-cost)
		if err != nil {
			return nil, 0, err
		}
		group = append(group, addresses...)
		cost += addressCost
		if !p.skipCFWS() {
			return nil, 0, errors.New("mail: misformatted parenthetical comment")
		}
		if p.consume(';') {
			if !p.skipCFWS() {
				return nil, 0, errors.New("mail: misformatted parenthetical comment")
			}
			break
		}
		if !p.consume(',') {
			return nil, 0, errors.New("mail: expected comma")
		}
	}
	return group, cost, nil
}

func (p *boundedAddrParser) consumeAddrSpec(out *addressTextSink) (resultErr error) {
	original := *p
	defer func() {
		if resultErr != nil {
			*p = original
		}
	}()
	p.skipSpace()
	if p.empty() {
		return errors.New("mail: no addr-spec")
	}
	beforeLocal := out.size
	var err error
	if p.peek() == '"' {
		err = p.consumeQuotedString(out)
		if err == nil && out.size == beforeLocal {
			err = errors.New("mail: empty quoted string in addr-spec")
		}
	} else {
		var local string
		local, err = p.consumeAtom(true, false)
		if err == nil {
			err = out.writeString(local)
		}
	}
	if err != nil {
		return err
	}
	if !p.consume('@') {
		return errors.New("mail: missing @ in addr-spec")
	}
	if err = out.writeByte('@'); err != nil {
		return err
	}
	p.skipSpace()
	if p.empty() {
		return errors.New("mail: no domain in addr-spec")
	}
	var domain string
	if p.peek() == '[' {
		domain, err = p.consumeDomainLiteral()
	} else {
		domain, err = p.consumeAtom(true, false)
	}
	if err != nil {
		return err
	}
	return out.writeString(domain)
}

func (p *boundedAddrParser) consumePhrase(out *addressTextSink) error {
	// Count completed grammar words separately from the current encoded run.
	// Empty encoded words add neither a word nor a separator; an empty quoted
	// string is still a word. Stream each nonempty run directly into the sink.
	words := 0
	var encodedRunBytes int64
	for {
		if words > 0 && !p.skipCFWS() {
			return errors.New("mail: misformatted parenthetical comment")
		}
		p.skipSpace()
		if p.empty() {
			break
		}
		wordStart := *p
		wordCount := addressTextSink{limit: math.MaxInt64}
		encoded, err := p.consumePhraseWord(&wordCount)
		if err != nil {
			if words == 0 && encodedRunBytes == 0 {
				return fmt.Errorf("mail: missing word in phrase: %w", err)
			}
			break
		}
		if encoded && wordCount.size == 0 {
			continue
		}
		if encoded {
			if encodedRunBytes == 0 && words > 0 {
				if err := out.writeByte(' '); err != nil {
					return err
				}
			}
			encodedRunBytes += wordCount.size
		} else {
			if encodedRunBytes > 0 {
				words++
				encodedRunBytes = 0
			}
			if words > 0 {
				if err := out.writeByte(' '); err != nil {
					return err
				}
			}
			words++
		}
		*p = wordStart
		if _, err := p.consumePhraseWord(out); err != nil {
			return err
		}
	}
	if words == 0 && encodedRunBytes == 0 {
		return errors.New("mail: missing word in phrase")
	}
	return nil
}

func (p *boundedAddrParser) consumePhraseWord(out *addressTextSink) (bool, error) {
	if p.peek() == '"' {
		return false, p.consumeQuotedString(out)
	}
	word, err := p.consumeAtom(true, true)
	if err != nil {
		return false, err
	}
	encoded, err := writeAddressEncodedWord(out, word)
	if err != nil || encoded {
		return encoded, err
	}
	return false, out.writeString(word)
}

func writeAddressEncodedWord(out *addressTextSink, word string) (bool, error) {
	return writeAddressEncodedCursor(out, addressWordCursor{raw: word})
}

func (p *boundedAddrParser) consumeQuotedString(out *addressTextSink) error {
	index := 1
	escaped := false
	for {
		value, size := utf8.DecodeRuneInString(p.value[index:])
		switch {
		case size == 0:
			return errors.New("mail: unclosed quoted-string")
		case size == 1 && value == utf8.RuneError:
			return errors.New("mail: invalid UTF-8 in quoted-string")
		case escaped:
			if !addressVChar(value) && !addressWSP(value) {
				return errors.New("mail: bad character in quoted-string")
			}
			if err := out.writeRune(value); err != nil {
				return err
			}
			escaped = false
		case addressQText(value), addressWSP(value):
			if err := out.writeRune(value); err != nil {
				return err
			}
		case value == '"':
			p.value = p.value[index+size:]
			return nil
		case value == '\\':
			escaped = true
		default:
			return errors.New("mail: bad character in quoted-string")
		}
		index += size
	}
}

func (p *boundedAddrParser) consumeAtom(dot, permissive bool) (string, error) {
	index := 0
	for {
		value, size := utf8.DecodeRuneInString(p.value[index:])
		if size == 1 && value == utf8.RuneError {
			return "", errors.New("mail: invalid UTF-8 in address")
		}
		if size == 0 || !addressAText(value, dot) {
			break
		}
		index += size
	}
	if index == 0 {
		return "", errors.New("mail: invalid string")
	}
	atom := p.value[:index]
	p.value = p.value[index:]
	if !permissive && (strings.HasPrefix(atom, ".") || strings.Contains(atom, "..") || strings.HasSuffix(atom, ".")) {
		return "", errors.New("mail: invalid dot in atom")
	}
	return atom, nil
}

func (p *boundedAddrParser) consumeDomainLiteral() (string, error) {
	start := p.value
	if !p.consume('[') {
		return "", errors.New("mail: missing domain-literal opener")
	}
	dtext := p.value
	size := 0
	for {
		if p.empty() {
			return "", errors.New("mail: unclosed domain-literal")
		}
		if p.peek() == ']' {
			break
		}
		value, width := utf8.DecodeRuneInString(p.value)
		if width == 1 && value == utf8.RuneError || !addressDText(value) {
			return "", errors.New("mail: invalid domain-literal")
		}
		size += width
		p.value = p.value[width:]
	}
	dtext = dtext[:size]
	if !p.consume(']') {
		return "", errors.New("mail: unclosed domain-literal")
	}
	if address, ok := strings.CutPrefix(dtext, "IPv6:"); ok {
		if len(net.ParseIP(address)) != net.IPv6len {
			return "", errors.New("mail: invalid IPv6 domain-literal")
		}
	} else if net.ParseIP(dtext).To4() == nil {
		return "", errors.New("mail: invalid IP domain-literal")
	}
	return start[:len(start)-len(p.value)], nil
}

func (p *boundedAddrParser) consumeDisplayNameComment(out *addressTextSink) error {
	if !p.consume('(') {
		return errors.New("mail: comment does not start with (")
	}
	source := p.value
	depth := 1
	wordStart := -1
	words := 0
	flushWord := func(end int) error {
		if wordStart < 0 {
			return nil
		}
		if words > 0 {
			if err := out.writeByte(' '); err != nil {
				return err
			}
		}
		if err := writeAddressCommentWord(out, source[wordStart:end]); err != nil {
			return err
		}
		wordStart = -1
		words++
		return nil
	}
	index := 0
	for index < len(source) && depth > 0 {
		value := source[index]
		width := 1
		if value == '\\' && index+1 < len(source) {
			value = source[index+1]
			width = 2
		} else if value == '(' {
			depth++
		} else if value == ')' {
			depth--
			if depth == 0 {
				if err := flushWord(index); err != nil {
					return err
				}
				index++
				break
			}
		}
		if value == ' ' || value == '\t' {
			if err := flushWord(index); err != nil {
				return err
			}
		} else if wordStart < 0 {
			wordStart = index
		}
		index += width
	}
	if depth != 0 {
		return errors.New("mail: misformatted parenthetical comment")
	}
	p.value = source[index:]
	return nil
}

func writeAddressCommentWord(out *addressTextSink, raw string) error {
	word := addressWordCursor{raw: raw, unescape: true}
	encoded, err := writeAddressEncodedCursor(out, word)
	if err != nil || encoded {
		return err
	}
	for value, ok := word.next(); ok; value, ok = word.next() {
		if err := out.writeByte(value); err != nil {
			return err
		}
	}
	return nil
}

func (p *boundedAddrParser) skipCFWS() bool {
	p.skipSpace()
	for p.consume('(') {
		if !p.discardComment() {
			return false
		}
		p.skipSpace()
	}
	return true
}

func (p *boundedAddrParser) discardComment() bool {
	depth := 1
	for !p.empty() && depth > 0 {
		if p.peek() == '\\' && len(p.value) > 1 {
			p.value = p.value[2:]
			continue
		}
		if p.peek() == '(' {
			depth++
		} else if p.peek() == ')' {
			depth--
		}
		p.value = p.value[1:]
	}
	return depth == 0
}

func (p *boundedAddrParser) skipSpace()  { p.value = strings.TrimLeft(p.value, " \t") }
func (p *boundedAddrParser) empty() bool { return len(p.value) == 0 }
func (p *boundedAddrParser) peek() byte  { return p.value[0] }

func (p *boundedAddrParser) consume(value byte) bool {
	if p.empty() || p.peek() != value {
		return false
	}
	p.value = p.value[1:]
	return true
}

func addressAText(value rune, dot bool) bool {
	if value == '.' {
		return dot
	}
	switch value {
	case '(', ')', '<', '>', '[', ']', ':', ';', '@', '\\', ',', '"':
		return false
	}
	return addressVChar(value)
}

func addressQText(value rune) bool { return value != '\\' && value != '"' && addressVChar(value) }
func addressVChar(value rune) bool { return '!' <= value && value <= '~' || value >= utf8.RuneSelf }
func addressWSP(value rune) bool   { return value == ' ' || value == '\t' }
func addressDText(value rune) bool {
	return value != '[' && value != ']' && value != '\\' && addressVChar(value)
}
