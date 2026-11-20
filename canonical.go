package synckosh

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Canonical writes JSON in one form only, so equal values are equal bytes in any language: RFC
// 8785's, which is JavaScript's JSON.stringify with object keys in order. Keys sort by their
// UTF-16 code units, numbers are written as JavaScript writes them, and strings escape only ",
// \ and the control characters. Insignificant space goes.
func Canonical(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("canonical: more than one JSON value")
	}
	var b bytes.Buffer
	if err := canonical(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func canonical(b *bytes.Buffer, v any) error {
	switch v := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(v))
	case json.Number:
		f, err := strconv.ParseFloat(string(v), 64)
		if err != nil || math.IsInf(f, 0) {
			return fmt.Errorf("canonical: number %s", v)
		}
		b.WriteString(esNumber(f))
	case string:
		esString(b, v)
	case []any:
		b.WriteByte('[')
		for i, e := range v {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := canonical(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		slices.SortFunc(keys, func(a, b string) int {
			return slices.Compare(utf16.Encode([]rune(a)), utf16.Encode([]rune(b)))
		})
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			esString(b, k)
			b.WriteByte(':')
			if err := canonical(b, v[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	}
	return nil
}

// esNumber writes a number as JavaScript's Number.prototype.toString does.
func esNumber(f float64) string {
	if f == 0 {
		return "0"
	}
	// The shortest digits that read back as f, and the exponent of the first: d.ddde±n.
	s := strconv.FormatFloat(f, 'e', -1, 64)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	mant, exp, _ := strings.Cut(s, "e")
	digits := strings.Replace(mant, ".", "", 1)
	e, _ := strconv.Atoi(exp)
	n := e + 1 // the decimal point comes after n digits
	k := len(digits)
	var out string
	switch {
	case k <= n && n <= 21:
		out = digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		out = digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		out = "0." + strings.Repeat("0", -n) + digits
	default:
		sign := "+"
		if n-1 < 0 {
			sign = "-"
		}
		out = digits[:1]
		if k > 1 {
			out += "." + digits[1:]
		}
		out += "e" + sign + strconv.Itoa(abs(n-1))
	}
	if neg {
		return "-" + out
	}
	return out
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// esString writes a string as JSON.stringify does: ", \ and control characters escaped, and
// everything else as it is.
func esString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20:
			fmt.Fprintf(b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}
