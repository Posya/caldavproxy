package caldav

import "bytes"

// maxContentOctets is the RFC 5545 limit for a content line, excluding CRLF.
const maxContentOctets = 75

// foldICS applies RFC 5545 line folding to an iCalendar document so that no
// content line exceeds 75 octets (excluding the CRLF). Continuation lines are
// introduced with a single SPACE. Splits never cut inside a UTF-8 sequence.
//
// The encoder from go-ical emits unfolded lines; many ICS subscribers are
// happier with folded output, so we normalize after Encode.
func foldICS(data []byte) []byte {
	if len(data) == 0 {
		return data
	}

	data = unfoldICS(data)

	var out bytes.Buffer
	out.Grow(len(data) + len(data)/20)

	for len(data) > 0 {
		var line []byte
		if i := bytes.Index(data, []byte("\r\n")); i >= 0 {
			line = data[:i]
			data = data[i+2:]
		} else if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line = data[:i]
			if i > 0 && data[i-1] == '\r' {
				line = data[:i-1]
			}
			data = data[i+1:]
		} else {
			line = data
			data = nil
		}

		writeFoldedLine(&out, line)
	}

	return out.Bytes()
}

// unfoldICS removes RFC 5545 folding (CRLF + SPACE/HTAB) so foldICS can
// re-apply it uniformly.
func unfoldICS(data []byte) []byte {
	var out bytes.Buffer
	out.Grow(len(data))

	for len(data) > 0 {
		if i := bytes.Index(data, []byte("\r\n")); i >= 0 {
			out.Write(data[:i])
			data = data[i+2:]
			if len(data) > 0 && (data[0] == ' ' || data[0] == '\t') {
				data = data[1:]
				continue
			}
			out.WriteString("\r\n")
			continue
		}
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			lineEnd := i
			if i > 0 && data[i-1] == '\r' {
				lineEnd = i - 1
			}
			out.Write(data[:lineEnd])
			data = data[i+1:]
			if len(data) > 0 && (data[0] == ' ' || data[0] == '\t') {
				data = data[1:]
				continue
			}
			out.WriteString("\r\n")
			continue
		}
		out.Write(data)
		break
	}

	return out.Bytes()
}

// writeFoldedLine writes line with RFC 5545 folding and a trailing CRLF.
func writeFoldedLine(out *bytes.Buffer, line []byte) {
	if len(line) == 0 {
		out.WriteString("\r\n")
		return
	}

	first := true
	for len(line) > 0 {
		limit := maxContentOctets
		if !first {
			// Leading SPACE on continuation lines counts toward the 75-octet budget.
			limit = maxContentOctets - 1
			out.WriteByte(' ')
		}

		n := limit
		if n > len(line) {
			n = len(line)
		}
		if n < len(line) {
			for n > 0 && line[n]&0xC0 == 0x80 {
				n--
			}
			if n == 0 {
				// Invalid UTF-8 or pathological input: advance one octet to avoid a loop.
				n = 1
			}
		}

		out.Write(line[:n])
		out.WriteString("\r\n")
		line = line[n:]
		first = false
	}
}
