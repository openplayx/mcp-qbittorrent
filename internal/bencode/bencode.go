// Package bencode is a small, strict bencode decoder. It keeps the raw byte
// span of every dictionary value so callers can hash the exact "info" bytes
// (BitTorrent v1 SHA-1 / v2 SHA-256 identities) without re-encoding.
package bencode

import (
	"bytes"
	"errors"
	"strconv"
)

// Value is one decoded node. Exactly one of Int, Str, List, Dict is set,
// according to Kind.
type Value struct {
	Kind byte // 'i', 's', 'l', 'd'
	Int  int64
	Str  []byte
	List []Value
	Dict map[string]Value
	Raw  []byte // exact encoded bytes of this node
}

var ErrInvalid = errors.New("invalid bencode")

const maxDepth = 64

// Decode parses data, which must contain exactly one value.
func Decode(data []byte) (Value, error) {
	d := decoder{data: data}
	v, err := d.value(0)
	if err != nil {
		return Value{}, err
	}
	if d.pos != len(data) {
		return Value{}, ErrInvalid
	}
	return v, nil
}

type decoder struct {
	data []byte
	pos  int
}

func (d *decoder) value(depth int) (Value, error) {
	if depth > maxDepth || d.pos >= len(d.data) {
		return Value{}, ErrInvalid
	}
	start := d.pos
	var v Value
	switch c := d.data[d.pos]; {
	case c == 'i':
		end := bytes.IndexByte(d.data[d.pos:], 'e')
		if end < 0 {
			return Value{}, ErrInvalid
		}
		s := string(d.data[d.pos+1 : d.pos+end])
		if s == "" || s == "-0" || (len(s) > 1 && s[0] == '0') || (len(s) > 2 && s[:2] == "-0") {
			return Value{}, ErrInvalid
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return Value{}, ErrInvalid
		}
		d.pos += end + 1
		v = Value{Kind: 'i', Int: n}
	case c >= '0' && c <= '9':
		s, err := d.str()
		if err != nil {
			return Value{}, err
		}
		v = Value{Kind: 's', Str: s}
	case c == 'l':
		d.pos++
		v = Value{Kind: 'l'}
		for {
			if d.pos >= len(d.data) {
				return Value{}, ErrInvalid
			}
			if d.data[d.pos] == 'e' {
				d.pos++
				break
			}
			item, err := d.value(depth + 1)
			if err != nil {
				return Value{}, err
			}
			v.List = append(v.List, item)
		}
	case c == 'd':
		d.pos++
		v = Value{Kind: 'd', Dict: map[string]Value{}}
		var last []byte
		for {
			if d.pos >= len(d.data) {
				return Value{}, ErrInvalid
			}
			if d.data[d.pos] == 'e' {
				d.pos++
				break
			}
			if c := d.data[d.pos]; c < '0' || c > '9' {
				return Value{}, ErrInvalid
			}
			key, err := d.str()
			if err != nil {
				return Value{}, err
			}
			// Keys must be unique and sorted; otherwise the info hash of
			// a re-encoding would differ from what peers compute.
			if last != nil && bytes.Compare(key, last) <= 0 {
				return Value{}, ErrInvalid
			}
			last = key
			item, err := d.value(depth + 1)
			if err != nil {
				return Value{}, err
			}
			v.Dict[string(key)] = item
		}
	default:
		return Value{}, ErrInvalid
	}
	v.Raw = d.data[start:d.pos]
	return v, nil
}

func (d *decoder) str() ([]byte, error) {
	colon := bytes.IndexByte(d.data[d.pos:], ':')
	if colon <= 0 || colon > 10 {
		return nil, ErrInvalid
	}
	digits := string(d.data[d.pos : d.pos+colon])
	if len(digits) > 1 && digits[0] == '0' {
		return nil, ErrInvalid
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n < 0 {
		return nil, ErrInvalid
	}
	begin := d.pos + colon + 1
	if begin+n > len(d.data) || begin+n < begin {
		return nil, ErrInvalid
	}
	d.pos = begin + n
	return d.data[begin : begin+n], nil
}

// Encode serialises simple Go values (used only for test fixtures):
// int/int64, string, []byte, []any, map[string]any.
func Encode(v any) []byte {
	var b bytes.Buffer
	encode(&b, v)
	return b.Bytes()
}

func encode(b *bytes.Buffer, v any) {
	switch x := v.(type) {
	case int:
		b.WriteString("i" + strconv.Itoa(x) + "e")
	case int64:
		b.WriteString("i" + strconv.FormatInt(x, 10) + "e")
	case string:
		b.WriteString(strconv.Itoa(len(x)) + ":" + x)
	case []byte:
		b.WriteString(strconv.Itoa(len(x)) + ":")
		b.Write(x)
	case []any:
		b.WriteByte('l')
		for _, item := range x {
			encode(b, item)
		}
		b.WriteByte('e')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sortStrings(keys)
		b.WriteByte('d')
		for _, k := range keys {
			encode(b, k)
			encode(b, x[k])
		}
		b.WriteByte('e')
	default:
		panic("bencode: unsupported type")
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
